"""Property assignment from JSON-friendly dicts onto FreeCAD document objects."""

# Annotations stay text, so importing this module (serialize does, for the
# quantity helpers) needs nothing of FreeCAD beyond its name.
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import FreeCAD

from rpc_server.agent_log import agent_error


@dataclass
class Object:
    name: str
    type: str | None = None
    analysis: str | None = None
    properties: dict[str, Any] = field(default_factory=dict)


def _to_shape_color(val: Any) -> tuple[float, float, float, float]:
    """Normalise a color to a 4-float RGBA tuple.

    Accepts RGB triples (alpha defaults to 1.0) and RGBA quads, matching what
    FreeCAD's ``ShapeColor`` accepts.
    """
    if not isinstance(val, (list, tuple)) or len(val) not in (3, 4):
        raise ValueError(
            f"ShapeColor must be an RGB or RGBA sequence, got {val!r}."
        )
    r, g, b = (float(val[0]), float(val[1]), float(val[2]))
    a = float(val[3]) if len(val) == 4 else 1.0
    return (r, g, b, a)


_REFERENCE_FORMS = (
    '{"object_name": "Box", "face": "Face1"}, '
    '{"object_name": "Box", "faces": ["Face1", "Face2"]}, '
    '["Box", "Face1"] or ["Box", ["Face1", "Face2"]]'
)


def _sub_elements(raw: Any) -> str | list[str] | None:
    """A sub-element name or a list of names, or None when ``raw`` is neither."""
    if isinstance(raw, str) and raw:
        return raw
    if (
        isinstance(raw, (list, tuple))
        and raw
        and all(isinstance(s, str) and s for s in raw)
    ):
        return list(raw)
    return None


def parse_reference_entry(entry: Any) -> tuple[str, str | list[str]]:
    """Normalise a single ``References`` entry to ``(object_name, sub_elements)``.

    ``sub_elements`` is one name or a list of names. Accepts
    ``{"object_name": "Box", "face": "Face1"}``,
    ``{"object_name": "Box", "faces": ["Face1", "Face2"]}``,
    ``["Box", "Face1"]`` and ``["Box", ["Face1", "Face2"]]``.
    """
    ref_name = None
    subs = None
    if isinstance(entry, dict):
        ref_name = entry.get("object_name", entry.get("Object"))
        subs = _sub_elements(
            entry.get("faces", entry.get("face", entry.get("Face")))
        )
    elif isinstance(entry, (list, tuple)) and len(entry) == 2:
        ref_name = entry[0]
        subs = _sub_elements(entry[1])
    if not isinstance(ref_name, str) or not ref_name or subs is None:
        raise ValueError(
            f"Invalid reference entry {entry!r}; expected {_REFERENCE_FORMS}."
        )
    return ref_name, subs


def resolve_references(doc: FreeCAD.Document, val: Any) -> list[tuple[Any, Any]]:
    """Resolve a ``References`` list into ``(DocumentObject, sub_elements)`` tuples."""
    refs = []
    for entry in val:
        ref_name, subs = parse_reference_entry(entry)
        ref_obj = doc.getObject(ref_name)
        if ref_obj is None:
            raise ValueError(f"Referenced object '{ref_name}' not found.")
        refs.append((ref_obj, subs))
    return refs


def _link_object(doc: FreeCAD.Document, name: Any) -> Any:
    """The document object called ``name``, or a ValueError."""
    if not isinstance(name, str):
        raise ValueError(f"expected an object name, got {name!r}.")
    ref_obj = doc.getObject(name)
    if ref_obj is None:
        raise ValueError(f"Referenced object '{name}' not found.")
    return ref_obj


def _type_id(obj: FreeCAD.DocumentObject, prop: str) -> str:
    """The FreeCAD type of a property, or "" when it cannot be read."""
    try:
        return obj.getTypeIdOfProperty(prop)
    except Exception:
        return ""


def _link_kind(obj: FreeCAD.DocumentObject, prop: str) -> str:
    """"single" for a link property that takes one object, "list" for one that
    takes several, else "".

    Every ``App::PropertyLink*`` and ``App::PropertyXLink*`` type counts,
    the sub-element ones (``PropertyLinkSub``, ``PropertyLinkSubList``)
    included: they all accept a bare object, which links it with no
    sub-element, and a list of objects for the list types.
    """
    try:
        type_id = obj.getTypeIdOfProperty(prop)
    except Exception:
        return ""
    if type_id.startswith(("App::PropertyLink", "App::PropertyXLink")):
        return "list" if "List" in type_id else "single"
    return ""


def quantity_type() -> Any:
    """FreeCAD's Quantity class, or None where FreeCAD has no Units module."""
    return getattr(getattr(FreeCAD, "Units", None), "Quantity", None)


def quantity_text(value: Any) -> str:
    """A quantity in the units FreeCAD prefers for it, for example "100.00 N",
    the way its property editor shows it."""
    try:
        return str(value.getUserPreferred()[0])
    except Exception:
        return str(value.UserString)


def quantity_values(obj: FreeCAD.DocumentObject, names: Any) -> dict[str, str]:
    """The quantity properties among ``names`` with their current value and
    unit in FreeCAD's preferred units, for example ``{"Force": "100.00 N"}``.
    """
    out = {}
    quantity = quantity_type()
    for name in names:
        if quantity is None or name not in obj.PropertiesList:
            continue
        try:
            value = getattr(obj, name)
        except Exception:
            continue
        if isinstance(value, quantity):
            out[name] = quantity_text(value)
    return out


def _map_text(value: Any) -> Any:
    """A number as the string a FreeCAD string map stores; anything else as is."""
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return str(value)
    return value


def _set_dotted(obj: FreeCAD.DocumentObject, path: str, val: Any) -> None:
    """Set one part of a compound property, such as ``Placement.Base.z``.

    A string starting with "=" binds an expression to the path ("=" alone
    removes it). A number is set by reading the property, changing the part
    and assigning the property back, since Placement, Vector and Rotation
    values are copies. ``Rotation.Angle`` is in degrees, as in a Placement
    given as a whole.
    """
    if isinstance(val, str) and val.startswith("="):
        obj.setExpression(path, val[1:].strip() or None)
        return
    names = path.split(".")
    values = [obj]
    for name in names[:-1]:
        values.append(getattr(values[-1], name))
    leaf = names[-1]
    new = val
    if isinstance(values[-1], FreeCAD.Rotation) and leaf == "Angle":
        # The attribute is in radians; replace the rotation itself instead.
        new = FreeCAD.Rotation(values[-1].Axis, float(val))
        values.pop()
        names.pop()
        leaf = names[-1]
    for i in range(len(values) - 1, -1, -1):
        setattr(values[i], leaf, new)
        if i == 0:
            break
        new = values[i]
        leaf = names[i - 1]


def set_object_property(
    doc: FreeCAD.Document, obj: FreeCAD.DocumentObject, properties: dict[str, Any]
):
    failures = []
    for prop, val in properties.items():
        try:
            if "." in prop and prop.split(".", 1)[0] in obj.PropertiesList:
                _set_dotted(obj, prop, val)
            elif prop in obj.PropertiesList:
                if prop == "Placement" and isinstance(val, dict):
                    if "Base" in val:
                        pos = val["Base"]
                    elif "Position" in val:
                        pos = val["Position"]
                    else:
                        pos = {}
                    rot = val.get("Rotation", {})
                    # Rotation(axis, angle) takes the angle in degrees, the unit
                    # serialize_value reports it in.
                    placement = FreeCAD.Placement(
                        FreeCAD.Vector(
                            pos.get("x", 0),
                            pos.get("y", 0),
                            pos.get("z", 0),
                        ),
                        FreeCAD.Rotation(
                            FreeCAD.Vector(
                                rot.get("Axis", {}).get("x", 0),
                                rot.get("Axis", {}).get("y", 0),
                                rot.get("Axis", {}).get("z", 1),
                            ),
                            rot.get("Angle", 0),
                        ),
                    )
                    setattr(obj, prop, placement)

                elif isinstance(getattr(obj, prop), FreeCAD.Vector) and isinstance(
                    val, dict
                ):
                    vector = FreeCAD.Vector(
                        val.get("x", 0), val.get("y", 0), val.get("z", 0)
                    )
                    setattr(obj, prop, vector)

                elif isinstance(val, dict) and _type_id(obj, prop) == "App::PropertyMap":
                    # A string map, such as a FEM material: numbers become the
                    # strings the map stores.
                    setattr(obj, prop, {str(k): _map_text(v) for k, v in val.items()})

                elif isinstance(val, str) and val.startswith("="):
                    # An expression, as in the spreadsheet tool; a bare "="
                    # removes the binding.
                    obj.setExpression(prop, val[1:].strip() or None)

                elif prop == "References":
                    # Always a list of entries: anything else is an error, never
                    # a link to the whole object.
                    if val is None:
                        # null clears the property, as before.
                        setattr(obj, prop, None)
                    elif isinstance(val, (list, tuple)):
                        setattr(obj, prop, resolve_references(doc, list(val)))
                    else:
                        raise ValueError(
                            f"References must be a list; each entry is {_REFERENCE_FORMS}."
                        )

                elif isinstance(val, str) and _link_kind(obj, prop) == "single":
                    setattr(obj, prop, _link_object(doc, val))

                elif _link_kind(obj, prop) == "list" and (
                    isinstance(val, str)
                    or (
                        isinstance(val, (list, tuple))
                        and all(isinstance(n, str) for n in val)
                    )
                ):
                    names = [val] if isinstance(val, str) else val
                    setattr(obj, prop, [_link_object(doc, n) for n in names])

                else:
                    setattr(obj, prop, val)
            # ShapeColor is a property of the ViewObject
            elif prop == "ShapeColor" and isinstance(val, (list, tuple)):
                setattr(obj.ViewObject, prop, _to_shape_color(val))

            elif prop == "ViewObject" and isinstance(val, dict):
                for k, v in val.items():
                    if k == "ShapeColor":
                        setattr(obj.ViewObject, k, _to_shape_color(v))
                    else:
                        setattr(obj.ViewObject, k, v)

            else:
                setattr(obj, prop, val)

        except Exception as e:
            agent_error(f"Property '{prop}' assignment error: {e}\n")
            failures.append(f"{prop}: {e}")

    if failures:
        raise ValueError(
            "Failed to set propert" + ("y" if len(failures) == 1 else "ies")
            + ": " + "; ".join(failures)
        )

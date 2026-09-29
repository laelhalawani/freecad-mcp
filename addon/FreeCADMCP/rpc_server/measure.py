"""Measure distances, angles, lengths, radii, areas and volumes.

Every ref resolves to a shape in global coordinates, by one of two routes
(see ``_resolve_ref``): a whole object or a plain element name goes through
``tessellation.shape_of`` plus ``getElement``; a dotted object path, the
form get_selection returns for anything below the top level, goes through
``Part.getShape`` with the path plus ``tessellation.apply_container_placement``.
Distances use distToShape. Part is imported inside the GUI task.
"""

import math
from typing import Any

from rpc_server.errors import INVALID_INPUT, NOT_FOUND, fail, tool_call
from rpc_server.gui_task import run_on_gui
from rpc_server.lookup import require_document, require_object
from rpc_server.serialize import finite_or_none


MEASURE_KINDS = ("distance", "angle", "length", "radius", "area", "volume")

# Kinds that take a fixed number of refs; everything else takes 1 or more.
_REF_COUNTS = {"distance": 2, "angle": 2, "radius": 1}

# Fixed budget: measure takes no timeout argument.
_TIMEOUT = 60.0

# Base::Precision::Angular() (Base/Precision.h:43-46), the tolerance FreeCAD's
# own Measure::MeasureAngle::execute() (Mod/Measure/App/MeasureAngle.cpp:236)
# uses to treat two direction vectors as (anti)parallel.
_ANGULAR_TOLERANCE = 1e-12


def _vector_list(vector: Any) -> list[float]:
    return [vector.x, vector.y, vector.z]


def _direction_of(shape: Any, Part: Any) -> Any:
    """Return the direction (a normalised Vector) of a straight edge or a
    planar face's normal axis, or None when ``shape`` is neither."""
    shape_type = getattr(shape, "ShapeType", "")
    if shape_type == "Edge":
        curve = shape.Curve
        if isinstance(curve, (Part.Line, Part.LineSegment)):
            return curve.Direction.normalize()
        return None
    if shape_type == "Face":
        surface = shape.Surface
        if isinstance(surface, Part.Plane):
            return surface.Axis.normalize()
        return None
    return None


def _radius_of(shape: Any, Part: Any) -> float | None:
    """Return the radius of a circular edge or a cylindrical/spherical face,
    or None when ``shape`` is none of those."""
    shape_type = getattr(shape, "ShapeType", "")
    if shape_type == "Edge":
        curve = shape.Curve
        if isinstance(curve, Part.Circle):
            return curve.Radius
        return None
    if shape_type == "Face":
        surface = shape.Surface
        if isinstance(surface, (Part.Cylinder, Part.Sphere)):
            return surface.Radius
        return None
    return None


def _not_found_hint(doc: Any, name: str) -> str:
    return ("Call " + tool_call("list_subelements", {"doc_name": doc.Name, "obj_name": name})
            + " to see its faces and edges with their names.")


def _path_owner(top_name: str, sub: str) -> str:
    """The object a dotted path's element name belongs to: the last real
    object name before it, the one get_object should report counts for.

    A path segment can be a mapped (TNP-mitigated) element name instead of
    an object name; FreeCAD marks those with a leading ``;``
    (``App/ElementNamingUtils.h`` ``ELEMENT_MAP_PREFIX``), for example
    ``Body.Pad.;#a:1;:G0;XTR;:Hfb5:8,F.Face3``, where the real owner is
    ``Pad``, not the mapped segment right before ``Face3``. Such segments are
    skipped; the ref's top-level object name is the fallback when none of
    the path's other segments is a plain object name.
    """
    for part in reversed(sub.split(".")[:-1]):
        if part and not part.startswith(";"):
            return part
    return top_name


def _resolve_ref(
    doc: Any, ref: dict[str, Any], Part: Any, shape_of: Any, apply_container_placement: Any
) -> tuple[Any, dict[str, Any] | None]:
    """Return (shape, None) for ``ref``, or (None, a fail() reply).

    ``sub`` is either a plain element name (``Face6``) on ``obj`` itself, or
    an object path down to one (``Pad.Face6``, ``Body.Pad.Face6``), the form
    get_selection returns for anything below the top level (no dot before
    the element name tells the two apart, matching how FreeCAD's own
    ``Data::findElementName`` splits a subname).

    A plain name is picked out of ``shape_of(obj)`` (the object's whole shape
    in global coordinates, the container's placement already applied) with
    ``getElement``, so nothing further is applied. A path is resolved with
    ``Part.getShape(obj, sub, needSubElement=True)``, which walks the path
    and accumulates the placement of ``obj`` and of every object named in
    it; that leaves only ``obj``'s own enclosing container unapplied, which
    ``apply_container_placement(shape, obj)`` (``tessellation``) supplies,
    the same correction ``shape_of`` makes for the plain-name case, through
    the one helper both routes share.
    """
    name = ref.get("object") if isinstance(ref, dict) else None
    if not name:
        return None, fail(INVALID_INPUT, f"each ref needs an \"object\" name, got {ref!r}")
    obj, error = require_object(doc, name)
    if error is not None:
        return None, error
    sub = ref.get("sub") or None
    if sub is not None and not isinstance(sub, str):
        return None, fail(INVALID_INPUT, f"sub must be a string, got {sub!r}")
    hint = "Call " + tool_call("get_object", {"doc_name": doc.Name, "obj_name": name}) + " to see its properties."

    if sub and "." in sub:
        try:
            element = Part.getShape(obj, sub, needSubElement=True)
        except Exception as e:
            return None, fail(
                INVALID_INPUT,
                f"could not resolve {name!r} sub-element {sub!r}: {type(e).__name__}: {e}",
                hint,
            )
        if element is None or element.isNull():
            return None, fail(
                NOT_FOUND,
                f"{name!r} has no sub-element {sub!r}",
                _not_found_hint(doc, _path_owner(name, sub)),
            )
        return apply_container_placement(element, obj), None

    try:
        shape = shape_of(obj)
    except Exception as e:
        return None, fail(INVALID_INPUT, f"could not resolve {name!r}: {type(e).__name__}: {e}", hint)
    if shape is None or shape.isNull():
        return None, fail(INVALID_INPUT, f"object {name!r} has no shape to measure", hint)
    if not sub:
        return shape, None
    try:
        # silent=True: a name outside the shape's face/edge/vertex range
        # comes back as None instead of raising (TopoShapePyImp.cpp
        # getElement).
        element = shape.getElement(sub, True)
    except Exception as e:
        return None, fail(
            INVALID_INPUT,
            f"could not resolve {name!r} sub-element {sub!r}: {type(e).__name__}: {e}",
            hint,
        )
    if element is None or element.isNull():
        return None, fail(NOT_FOUND, f"{name!r} has no sub-element {sub!r}", _not_found_hint(doc, name))
    return element, None


def _measure_gui(doc_name: str, kind: str, refs: list[dict[str, Any]]) -> dict[str, Any]:
    import Part

    from rpc_server.tessellation import apply_container_placement, shape_of

    doc, error = require_document(doc_name)
    if error is not None:
        return error

    shapes: list[Any] = []
    for ref in refs:
        shape, error = _resolve_ref(doc, ref, Part, shape_of, apply_container_placement)
        if error is not None:
            return error
        shapes.append(shape)

    result: dict[str, Any] = {"success": True, "document": doc.Name, "kind": kind, "refs": refs}

    if kind == "distance":
        dist, vectors, _infos = shapes[0].distToShape(shapes[1])
        result["value"] = finite_or_none(dist)
        result["unit"] = "mm"
        if vectors:
            p1, p2 = vectors[0]
            result["points"] = [_vector_list(p1), _vector_list(p2)]
        return result

    if kind == "angle":
        dir1 = _direction_of(shapes[0], Part)
        if dir1 is None:
            return fail(
                INVALID_INPUT,
                "the first ref is not a straight edge or a planar face",
                "Call " + tool_call("get_object", {"doc_name": doc.Name, "obj_name": refs[0].get("object")})
                + " to see its shape type.",
            )
        dir2 = _direction_of(shapes[1], Part)
        if dir2 is None:
            return fail(
                INVALID_INPUT,
                "the second ref is not a straight edge or a planar face",
                "Call " + tool_call("get_object", {"doc_name": doc.Name, "obj_name": refs[1].get("object")})
                + " to see its shape type.",
            )
        result["unit"] = "deg"
        # A direction vector's sign is arbitrary: an edge's Curve.Direction and
        # a face's Surface.Axis are properties of the underlying geometry, not
        # oriented towards anything, so the raw angle between two such vectors
        # can be the true angle or its 180-degree complement. This mirrors
        # Measure::MeasureAngle::execute() (Mod/Measure/App/MeasureAngle.cpp:
        # 236-257): two (anti)parallel directions report a 0-degree angle
        # (disambiguating them is ill-defined, since any point equidistant
        # from both elements lies on their shared line); otherwise each
        # direction is flipped, if needed, to point away from the midpoint of
        # the two elements' centres of mass (also FreeCAD's own convention,
        # Mod/Part/App/MeasureClient.cpp:428-467), which resolves the sign and
        # leaves a plain unsigned angle between the two flipped vectors.
        raw_angle = dir1.getAngle(dir2)
        if raw_angle <= _ANGULAR_TOLERANCE or math.pi - raw_angle <= _ANGULAR_TOLERANCE:
            result["value"] = 0.0
            return result
        loc1 = shapes[0].CenterOfMass
        loc2 = shapes[1].CenterOfMass
        origin = loc1.add(loc2).multiply(0.5)
        if loc1.sub(origin).dot(dir1) < 0:
            dir1 = dir1.negative()
        if loc2.sub(origin).dot(dir2) < 0:
            dir2 = dir2.negative()
        result["value"] = finite_or_none(math.degrees(dir1.getAngle(dir2)))
        return result

    if kind == "length":
        result["value"] = finite_or_none(sum(shape.Length for shape in shapes))
        result["unit"] = "mm"
        return result

    if kind == "radius":
        radius = _radius_of(shapes[0], Part)
        if radius is None:
            return fail(
                INVALID_INPUT,
                "the ref is not a circular edge or a cylindrical or spherical face",
                "Call " + tool_call("get_object", {"doc_name": doc.Name, "obj_name": refs[0].get("object")})
                + " to see its shape type.",
            )
        result["value"] = finite_or_none(radius)
        result["unit"] = "mm"
        return result

    if kind == "area":
        result["value"] = finite_or_none(sum(shape.Area for shape in shapes))
        result["unit"] = "mm^2"
        return result

    # kind == "volume", the only remaining member of MEASURE_KINDS.
    for ref, shape in zip(refs, shapes):
        if not shape.Solids:
            return fail(
                INVALID_INPUT,
                f"{ref.get('object')!r} has no solid to measure a volume",
                "Call " + tool_call("get_object", {"doc_name": doc.Name, "obj_name": ref.get("object")})
                + " to see its shape type.",
            )
    result["value"] = finite_or_none(sum(shape.Volume for shape in shapes))
    result["unit"] = "mm^3"
    return result


def measure(doc_name: str, kind: str, refs: list[dict[str, Any]]) -> dict[str, Any]:
    """Measure ``kind`` over ``refs`` ([{"object", "sub"?}]).

    Reply: ``{"success", "document", "kind", "value", "unit", "points"?,
    "refs"}``. GUI thread, 60 s. No transaction.
    """
    if kind not in MEASURE_KINDS:
        return fail(
            INVALID_INPUT,
            f"invalid kind: {kind!r}; kind must be one of {', '.join(MEASURE_KINDS)}",
        )
    if not isinstance(refs, list) or not refs:
        return fail(
            INVALID_INPUT,
            'refs must be a non-empty list of {"object": ..., "sub"?: ...}',
        )
    expected = _REF_COUNTS.get(kind)
    if expected is not None and len(refs) != expected:
        return fail(
            INVALID_INPUT,
            f"{kind} takes exactly {expected} ref{'s' if expected != 1 else ''}, got {len(refs)}",
        )
    for ref in refs:
        if not isinstance(ref, dict) or not ref.get("object"):
            return fail(INVALID_INPUT, f'each ref needs an "object" name, got {ref!r}')

    return run_on_gui(lambda: _measure_gui(doc_name, kind, refs), _TIMEOUT, "measure")

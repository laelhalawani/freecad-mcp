import FreeCAD as App
import json
import math
import xmlrpc.client

from rpc_server import tessellation
from rpc_server.object_validation import object_states, object_status, object_validity_error
from rpc_server.property_mapper import quantity_text, quantity_type


def _get_optional_app_type(name: str) -> type | tuple[type, ...] | None:
    value = getattr(App, name, None)
    if isinstance(value, type):
        return value
    if isinstance(value, tuple) and all(isinstance(item, type) for item in value):
        return value
    return None


_COLOR_TYPE = _get_optional_app_type("Color")
_DOCUMENT_OBJECT_TYPE = _get_optional_app_type("DocumentObject")
_QUANTITY_TYPE = quantity_type()

# Shape types whose CenterOfMass FreeCAD computes directly
# (Mod/Part/App/TopoShapeSolid.pyi, TopoShapeShell.pyi, TopoShapeFace.pyi,
# TopoShapeWire.pyi, TopoShapeEdge.pyi). Other shapes (Compound, CompSolid,
# Vertex, an empty Shape) fall back to a volume-weighted average over
# Shape.Solids, or omit the field when there are no solids.
_CENTER_OF_MASS_DIRECT_TYPES = {"Solid", "Shell", "Face", "Wire", "Edge"}


def finite_or_none(value: float) -> float | None:
    """None for NaN or infinite, since a reply can only carry finite floats.

    Degenerate OCC geometry (a self-intersecting or zero-volume shape) can
    produce either, and xmlrpc.client has no representation for them.
    """
    try:
        if math.isfinite(value):
            return value
    except TypeError:
        pass
    return None


def serialize_int(value: int) -> int | float | str:
    """Keep an int inside XML-RPC's 32-bit range so the reply can be marshalled.

    Larger values go out as a float when that is exact, otherwise as a
    decimal string.
    """
    if xmlrpc.client.MININT <= value <= xmlrpc.client.MAXINT:
        return value
    try:
        as_float = float(value)
    except OverflowError:
        return str(value)
    if as_float == value:
        return as_float
    return str(value)


def serialize_value(value):
    if value is None:
        return None
    elif isinstance(value, bool):
        return value
    elif isinstance(value, int):
        return serialize_int(value)
    elif isinstance(value, float):
        return finite_or_none(value)
    elif isinstance(value, str):
        return value
    elif isinstance(value, App.Vector):
        return {
            "x": finite_or_none(value.x),
            "y": finite_or_none(value.y),
            "z": finite_or_none(value.z),
        }
    elif isinstance(value, App.Rotation):
        # Rotation.Angle is in radians; the angle goes out in degrees, the unit
        # FreeCAD.Rotation(axis, angle) takes when property_mapper writes it back.
        return {
            "Axis": {
                "x": finite_or_none(value.Axis.x),
                "y": finite_or_none(value.Axis.y),
                "z": finite_or_none(value.Axis.z),
            },
            "Angle": finite_or_none(math.degrees(value.Angle)),
        }
    elif isinstance(value, App.Placement):
        return {
            "Base": serialize_value(value.Base),
            "Rotation": serialize_value(value.Rotation),
        }
    elif _DOCUMENT_OBJECT_TYPE is not None and isinstance(value, _DOCUMENT_OBJECT_TYPE):
        # A Link (or the object end of a LinkSub) is a DocumentObject.
        return value.Name
    elif _QUANTITY_TYPE is not None and isinstance(value, _QUANTITY_TYPE):
        # In the units FreeCAD prefers for it ("100.00 N"), the way the
        # create and update replies show it, not in base units.
        return quantity_text(value)
    elif isinstance(value, (list, tuple)):
        # A LinkSub is (DocumentObject, (sub names...)); a LinkSubList is a
        # list of those; a LinkList is a list of DocumentObject (handled by
        # the recursive call below through the DocumentObject case above).
        if (
            len(value) == 2
            and _DOCUMENT_OBJECT_TYPE is not None
            and isinstance(value[0], _DOCUMENT_OBJECT_TYPE)
            and isinstance(value[1], (list, tuple))
            and all(isinstance(item, str) for item in value[1])
        ):
            return [value[0].Name, [str(item) for item in value[1]]]
        return [serialize_value(v) for v in value]
    elif _COLOR_TYPE is not None and isinstance(value, _COLOR_TYPE):
        return tuple(value)
    else:
        return str(value)


def bound_box_list(bb) -> list[float | None]:
    """[xmin, ymin, zmin, xmax, ymax, zmax] (mm).

    A coordinate that is NaN or infinite (an unbounded or degenerate shape,
    for example from a bad import) goes out as None instead: xmlrpc.client
    has no representation for either.
    """
    return [finite_or_none(v) for v in (bb.XMin, bb.YMin, bb.ZMin, bb.XMax, bb.YMax, bb.ZMax)]


def _center_of_mass_dict(center) -> dict:
    return {
        "x": finite_or_none(center.x),
        "y": finite_or_none(center.y),
        "z": finite_or_none(center.z),
    }


def _center_of_mass(shape) -> dict | None:
    """CenterOfMass as {"x", "y", "z"}, or None when the shape has none.

    Solid, Shell, Face, Wire and Edge report it directly. Anything else
    (a compound from a boolean operation, a CompSolid, ...) gets a
    volume-weighted average over Shape.Solids; a shape with no solids
    (a Vertex, an empty compound) has none. Each coordinate goes through
    finite_or_none, as a degenerate solid's centroid can be NaN.
    """
    if getattr(shape, "ShapeType", "") in _CENTER_OF_MASS_DIRECT_TYPES:
        return _center_of_mass_dict(shape.CenterOfMass)

    try:
        solids = list(shape.Solids)
    except Exception:
        return None

    total_volume = 0.0
    sum_x = sum_y = sum_z = 0.0
    for solid in solids:
        volume = solid.Volume
        if volume <= 0:
            continue
        center = solid.CenterOfMass
        sum_x += center.x * volume
        sum_y += center.y * volume
        sum_z += center.z * volume
        total_volume += volume
    if total_volume <= 0:
        return None
    return {
        "x": finite_or_none(sum_x / total_volume),
        "y": finite_or_none(sum_y / total_volume),
        "z": finite_or_none(sum_z / total_volume),
    }


def serialize_shape(shape):
    if shape is None:
        return None
    try:
        result = {
            "Volume": finite_or_none(shape.Volume),
            "Area": finite_or_none(shape.Area),
            "VertexCount": len(shape.Vertexes),
            "EdgeCount": len(shape.Edges),
            "FaceCount": len(shape.Faces),
            # A fused result can be a Compound of several solids.
            "SolidCount": len(shape.Solids),
            "BoundBox": bound_box_list(shape.BoundBox),
        }
    except Exception as e:
        return {"error": f"invalid shape: {str(e)}"}
    try:
        center = _center_of_mass(shape)
    except Exception:
        center = None
    if center is not None:
        result["CenterOfMass"] = center
    return result


def serialize_view_object(view):
    if view is None:
        return None
    result = {}
    try:
        result["ShapeColor"] = serialize_value(view.ShapeColor)
    except AttributeError:
        pass
    try:
        result["Transparency"] = view.Transparency
    except AttributeError:
        pass
    try:
        result["Visibility"] = view.Visibility
    except AttributeError:
        pass
    return result


def _names(obj, attr: str) -> list[str]:
    """The Name of every object in obj.OutList / obj.InList."""
    try:
        items = getattr(obj, attr)
    except Exception:
        return []
    try:
        return [str(item.Name) for item in items]
    except Exception:
        return []


def _is_valid(obj) -> bool:
    """Whether ``obj`` is valid, for get_object's Valid and list_objects' compact "valid".

    Neither runs a recompute, so a bare Touched (not yet recomputed, not
    itself broken) is excluded, consistent with every other reply built from
    state before any recompute (open_document, undo/redo, a save with
    recompute false).
    """
    try:
        return object_validity_error(obj, exclude_touched=True) is None
    except Exception:
        return False


def visibility_of(obj, default: bool | None = None) -> bool | None:
    """``obj.ViewObject.Visibility``, or ``default`` when there is no
    ViewObject (a headless document) or it cannot be read.

    Shared by every module that needs an object's visibility: a compact
    object listing reports "unknown" (``default=None``) for a headless
    document, while export and printability treat an object with no
    ViewObject as visible (``default=True``), so it is not silently dropped
    from a document that never created one.
    """
    view = getattr(obj, "ViewObject", None)
    if view is None:
        return default
    try:
        return bool(view.Visibility)
    except Exception:
        return default


def list_objects_gui(doc_name: str) -> list[dict]:
    """Return the compact object list of ``doc_name`` for get_objects(compact=True).

    Runs on the GUI thread. Rows: ``{"name", "label", "type", "state",
    "valid", "parent", "visible"}``; ``[]`` for a document that is not open.
    ``parent`` comes from ``tessellation.parent_map``, the same claim logic
    ``tree_root_objects`` uses, so this list's top-level objects (an empty
    "parent") agree with what export_document and check_printability treat
    as top level by default.
    """
    try:
        doc = App.getDocument(doc_name)
    except Exception:
        return []
    if doc is None:
        return []
    parents = tessellation.parent_map(doc)
    return [
        {
            "name": obj.Name,
            "label": obj.Label,
            "type": obj.TypeId,
            "state": object_states(obj),
            "valid": _is_valid(obj),
            "parent": parents.get(str(getattr(obj, "Name", "")), ""),
            "visible": visibility_of(obj),
        }
        for obj in doc.Objects
    ]


def serialize_object(obj):
    if isinstance(obj, list):
        return [serialize_object(item) for item in obj]
    elif isinstance(obj, App.Document):
        return {
            "Name": obj.Name,
            "Label": obj.Label,
            "FileName": obj.FileName,
            "Objects": [serialize_object(child) for child in obj.Objects],
        }
    else:
        result = {
            "Name": obj.Name,
            "Label": obj.Label,
            "TypeId": obj.TypeId,
            "Valid": _is_valid(obj),
            "State": object_states(obj),
            "Status": object_status(obj),
            "OutList": _names(obj, "OutList"),
            "InList": _names(obj, "InList"),
            "Properties": {},
            "Placement": serialize_value(getattr(obj, "Placement", None)),
            "Shape": serialize_shape(getattr(obj, "Shape", None)),
            "ViewObject": {},
        }

        for prop in obj.PropertiesList:
            try:
                result["Properties"][prop] = serialize_value(getattr(obj, prop))
            except Exception as e:
                result["Properties"][prop] = f"<error: {str(e)}>"

        if hasattr(obj, "ViewObject") and obj.ViewObject is not None:
            view = obj.ViewObject
            result["ViewObject"] = serialize_view_object(view)

        return result

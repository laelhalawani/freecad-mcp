import FreeCAD as App
import json
import math
import xmlrpc.client


def _get_optional_app_type(name: str) -> type | tuple[type, ...] | None:
    value = getattr(App, name, None)
    if isinstance(value, type):
        return value
    if isinstance(value, tuple) and all(isinstance(item, type) for item in value):
        return value
    return None


_COLOR_TYPE = _get_optional_app_type("Color")


def _serialize_int(value: int) -> int | float | str:
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
        return _serialize_int(value)
    elif isinstance(value, (float, str)):
        return value
    elif isinstance(value, App.Vector):
        return {"x": value.x, "y": value.y, "z": value.z}
    elif isinstance(value, App.Rotation):
        # Rotation.Angle is in radians; the angle goes out in degrees, the unit
        # FreeCAD.Rotation(axis, angle) takes when property_mapper writes it back.
        return {
            "Axis": {"x": value.Axis.x, "y": value.Axis.y, "z": value.Axis.z},
            "Angle": math.degrees(value.Angle),
        }
    elif isinstance(value, App.Placement):
        return {
            "Base": serialize_value(value.Base),
            "Rotation": serialize_value(value.Rotation),
        }
    elif isinstance(value, (list, tuple)):
        return [serialize_value(v) for v in value]
    elif _COLOR_TYPE is not None and isinstance(value, _COLOR_TYPE):
        return tuple(value)
    else:
        return str(value)


def serialize_shape(shape):
    if shape is None:
        return None
    try:
        return {
            "Volume": shape.Volume,
            "Area": shape.Area,
            "VertexCount": len(shape.Vertexes),
            "EdgeCount": len(shape.Edges),
            "FaceCount": len(shape.Faces),
        }
    except Exception as e:
        return {"error": f"invalid shape: {str(e)}"}


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

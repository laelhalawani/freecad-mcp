"""List the faces and edges of an object with their names and geometry.

The names are the ``Face<N>`` and ``Edge<N>`` that measure, References and
attachments take. Shapes are read in global coordinates through
``tessellation.shape_of``, the same route measure uses. Part is imported
inside the GUI task.
"""

from typing import Any

from rpc_server.errors import INVALID_INPUT, fail, tool_call
from rpc_server.gui_task import run_on_gui
from rpc_server.lookup import require_document, require_object
from rpc_server.serialize import finite_or_none


SUBELEMENT_KINDS = ("faces", "edges", "all")

# Fixed budget: list_subelements takes no timeout argument.
_TIMEOUT = 60.0

# Decimals kept in reported lengths, areas and coordinates (millimetres).
_DECIMALS = 4


def _num(value: float) -> float | None:
    number = finite_or_none(value)
    # Adding 0.0 turns a negative zero into a plain zero.
    return None if number is None else round(number, _DECIMALS) + 0.0


def _vector(vector: Any) -> list[float | None]:
    return [_num(vector.x), _num(vector.y), _num(vector.z)]


def _surface_kind(surface: Any, Part: Any) -> str:
    for kind, name in (
        ("plane", "Plane"),
        ("cylinder", "Cylinder"),
        ("cone", "Cone"),
        ("sphere", "Sphere"),
        ("torus", "Toroid"),
    ):
        cls = getattr(Part, name, None)
        if cls is not None and isinstance(surface, cls):
            return kind
    return "other"


def _face_row(name: str, face: Any, Part: Any) -> dict[str, Any]:
    row: dict[str, Any] = {"name": name}
    try:
        surface = face.Surface
    except Exception:
        surface = None
    kind = _surface_kind(surface, Part)
    row["surface"] = kind
    row["area"] = _num(face.Area)
    row["center"] = _vector(face.CenterOfMass)
    if kind == "plane":
        u0, u1, v0, v1 = face.ParameterRange
        row["normal"] = _vector(face.normalAt((u0 + u1) / 2, (v0 + v1) / 2))
    elif kind in ("cylinder", "sphere"):
        row["radius"] = _num(surface.Radius)
        row["axis"] = _vector(surface.Axis)
    return row


def _edge_row(name: str, edge: Any, Part: Any) -> dict[str, Any]:
    row: dict[str, Any] = {"name": name}
    try:
        curve = edge.Curve
    except Exception:
        curve = None
    if isinstance(curve, (Part.Line, Part.LineSegment)):
        kind = "line"
    elif isinstance(curve, Part.Circle):
        kind = "circle"
    else:
        kind = "other"
    row["curve"] = kind
    row["length"] = _num(edge.Length)
    if kind == "circle":
        row["radius"] = _num(curve.Radius)
        row["center"] = _vector(curve.Center)
    return row


def _list_subelements_gui(doc_name: str, obj_name: str, kind: str) -> dict[str, Any]:
    import Part

    from rpc_server.tessellation import shape_of

    doc, error = require_document(doc_name)
    if error is not None:
        return error
    obj, error = require_object(doc, obj_name)
    if error is not None:
        return error
    shape = shape_of(obj)
    if shape is None:
        return fail(
            INVALID_INPUT,
            f"object {obj_name!r} has no shape, so it has no faces or edges",
            "Call " + tool_call("get_object", {"doc_name": doc.Name, "obj_name": obj_name})
            + " to see its type and properties.",
        )

    result: dict[str, Any] = {
        "success": True,
        "document": doc.Name,
        "object": obj_name,
        "kind": kind,
    }
    if kind in ("faces", "all"):
        result["faces"] = [
            _face_row(f"Face{i}", face, Part) for i, face in enumerate(shape.Faces, 1)
        ]
    if kind in ("edges", "all"):
        result["edges"] = [
            _edge_row(f"Edge{i}", edge, Part) for i, edge in enumerate(shape.Edges, 1)
        ]
    return result


def list_subelements(doc_name: str, obj_name: str, kind: str = "faces") -> dict[str, Any]:
    """List the faces and/or edges of ``obj_name``.

    ``kind`` is "faces", "edges" or "all". Reply: ``{"success", "document",
    "object", "kind", "faces"?: [{"name", "surface", "area", "center",
    "normal"? (planes), "radius"?, "axis"? (cylinders and spheres)}],
    "edges"?: [{"name", "curve", "length", "radius"?, "center"? (circles)}]}``.
    Units are millimetres and square millimetres. GUI thread, 60 s. No
    transaction.
    """
    if kind not in SUBELEMENT_KINDS:
        return fail(
            INVALID_INPUT,
            f"invalid kind: {kind!r}; kind must be one of {', '.join(SUBELEMENT_KINDS)}",
        )
    return run_on_gui(
        lambda: _list_subelements_gui(doc_name, obj_name, kind), _TIMEOUT, "list_subelements"
    )

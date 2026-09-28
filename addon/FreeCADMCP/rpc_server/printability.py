"""3D-printability checks for solids and meshes.

Shape validity and closedness come from Part (isValid, isClosed, check(True));
solidity, manifold and self-intersection checks come from a tessellated mesh
built with tessellation.py; bed fit comes from the bound box along the build
direction; overhang area comes from facet normals against the build direction,
excluding facets that rest on the model's lowest point (the print bed itself).
The cost of the expensive per-object steps (shape.check(True), which can run
a full BOP analysis; tessellation; the mesh self-intersection test; the
overhang scan) is bounded by the remaining share of the call's timeout, timed
from before the first object is even resolved.

GUI thread only. Part and MeshPart (through tessellation.py) are imported
inside the GUI-thread functions.
"""

import math
import time
from typing import Any

from rpc_server import tessellation
from rpc_server.errors import INVALID_INPUT, fail
from rpc_server.gui_task import resolve_timeout, run_on_gui
from rpc_server.lookup import require_document, require_object
from rpc_server.options import check_options
from rpc_server.serialize import bound_box_list, finite_or_none, serialize_int, visibility_of


PRINTABILITY_TIMEOUT = 300.0

# The MCP tool name, used for the run_on_gui timeout hint.
_TOOL_NAME = "check_printability"

# Keys accepted in ``options``.
PRINTABILITY_OPTIONS = (
    "bed", "build_direction", "overhang_angle_deg", "check_self_intersections",
    "quality", "linear_deflection", "angular_deflection_deg", "relative",
)
BUILD_DIRECTIONS = ("+Z", "-Z", "+X", "-X", "+Y", "-Y")
DEFAULT_OVERHANG_ANGLE_DEG = 45.0
OVERHANG_ANGLE_RANGE = (0.0, 89.0)

# Facets whose vertices all lie within this many mm of the model's lowest
# point along the build direction rest on the bed and are never an overhang.
_BASE_FACET_TOLERANCE_MM = 0.05

# Once the remaining share of the call's timeout drops under this fraction,
# the expensive per-object steps (shape.check(True), tessellation, the mesh
# self-intersection test, the overhang scan) are skipped for the rest of the
# call rather than risk running past the caller's budget.
_BUDGET_GUARD_FRACTION = 0.25

# Unit vectors for each accepted build direction.
DIRECTION_VECTORS: dict[str, tuple[float, float, float]] = {
    "+Z": (0.0, 0.0, 1.0), "-Z": (0.0, 0.0, -1.0),
    "+X": (1.0, 0.0, 0.0), "-X": (-1.0, 0.0, 0.0),
    "+Y": (0.0, 1.0, 0.0), "-Y": (0.0, -1.0, 0.0),
}


def check_printability(
    doc_name: str,
    object_names: list[str] | None = None,
    options: dict[str, Any] | None = None,
    timeout: Any = None,
) -> dict[str, Any]:
    """Check whether objects of ``doc_name`` are ready for 3D printing.

    Reply: ``{"success", "document", "printable", "settings", "objects"}``.
    GUI thread, default timeout ``PRINTABILITY_TIMEOUT``. No transaction.
    """
    timeout = resolve_timeout(timeout, PRINTABILITY_TIMEOUT)
    if isinstance(timeout, dict):
        return timeout

    options, error = check_options(options, PRINTABILITY_OPTIONS)
    if error is not None:
        return error

    bed = options.get("bed")
    if bed is not None:
        if (
            not isinstance(bed, (list, tuple))
            or len(bed) != 3
            or any(isinstance(v, bool) or not isinstance(v, (int, float)) or v <= 0 for v in bed)
        ):
            return fail(INVALID_INPUT, "bed must be [x, y, z] in mm, each greater than 0")
        bed = tuple(float(v) for v in bed)

    build_direction = options.get("build_direction")
    if build_direction is None or build_direction == "":
        build_direction = "+Z"
    if build_direction not in BUILD_DIRECTIONS:
        return fail(
            INVALID_INPUT,
            f"build_direction must be one of {', '.join(BUILD_DIRECTIONS)}, not {build_direction!r}",
        )

    overhang_angle_deg = options.get("overhang_angle_deg")
    if overhang_angle_deg is None:
        overhang_angle_deg = DEFAULT_OVERHANG_ANGLE_DEG
    else:
        if isinstance(overhang_angle_deg, bool) or not isinstance(overhang_angle_deg, (int, float)):
            return fail(
                INVALID_INPUT,
                f"overhang_angle_deg must be a number from {OVERHANG_ANGLE_RANGE[0]:g} to "
                f"{OVERHANG_ANGLE_RANGE[1]:g}, not {overhang_angle_deg!r}",
            )
        overhang_angle_deg = float(overhang_angle_deg)
        if not (OVERHANG_ANGLE_RANGE[0] <= overhang_angle_deg <= OVERHANG_ANGLE_RANGE[1]):
            return fail(
                INVALID_INPUT,
                f"overhang_angle_deg must be from {OVERHANG_ANGLE_RANGE[0]:g} to "
                f"{OVERHANG_ANGLE_RANGE[1]:g}, not {overhang_angle_deg!r}",
            )

    check_self_intersections = options.get("check_self_intersections")
    if check_self_intersections is None:
        check_self_intersections = True
    elif not isinstance(check_self_intersections, bool):
        return fail(INVALID_INPUT, f"check_self_intersections must be true or false, not {check_self_intersections!r}")

    settings = tessellation.resolve_settings(
        options.get("quality"),
        options.get("linear_deflection"),
        options.get("angular_deflection_deg"),
        options.get("relative"),
    )
    if isinstance(settings, dict):
        return settings

    def task() -> dict[str, Any]:
        return _check_printability_gui(
            doc_name, object_names, bed, build_direction, overhang_angle_deg,
            check_self_intersections, settings, timeout,
        )

    return run_on_gui(task, timeout, "check_printability", tool=_TOOL_NAME)


def _check_printability_gui(
    doc_name: str,
    object_names: list[str] | None,
    bed: tuple[float, float, float] | None,
    build_direction: str,
    overhang_angle_deg: float,
    check_self_intersections: bool,
    settings: "tessellation.Settings",
    timeout: float,
) -> dict[str, Any]:
    # Taken before the document is even resolved, so the budget guard also
    # covers the cost of _resolve_objects, which computes a shape (and so a
    # tessellation-sized amount of work) for every candidate object.
    start = time.monotonic()
    quarter = timeout * _BUDGET_GUARD_FRACTION

    def budget_ok() -> bool:
        return (timeout - (time.monotonic() - start)) >= quarter

    doc, error = require_document(doc_name)
    if error is not None:
        return error

    objects, error = _resolve_objects(doc, object_names)
    if error is not None:
        return error

    results = [
        _check_object(
            obj, settings, build_direction, overhang_angle_deg,
            check_self_intersections, bed, budget_ok,
        )
        for obj in objects
    ]

    # An empty result is never "printable": nothing was actually checked.
    printable = bool(results) and all(not item["issues"] for item in results)

    return {
        "success": True,
        "document": doc.Name,
        "printable": printable,
        "settings": {
            **settings.as_dict(),
            "bed": list(bed) if bed is not None else None,
            "build_direction": build_direction,
            "overhang_angle_deg": overhang_angle_deg,
        },
        "objects": results,
    }


def _resolve_objects(doc: Any, object_names: list[str] | None) -> tuple[list[Any], dict[str, Any] | None]:
    """Return the objects to check, or a failure reply.

    Given names must all resolve; without them, the visible top-level objects
    (as the FreeCAD tree shows them, tessellation.tree_root_objects) that
    have a solid or a mesh.
    """
    if object_names:
        objects = []
        for name in object_names:
            obj, error = require_object(doc, name)
            if error is not None:
                return [], error
            objects.append(obj)
        return objects, None

    objects = []
    for obj in tessellation.tree_root_objects(doc):
        if not visibility_of(obj, True):
            continue
        if tessellation.is_mesh_feature(obj):
            objects.append(obj)
            continue
        shape = tessellation.shape_of(obj)
        try:
            has_solid = shape is not None and len(shape.Solids) > 0
        except Exception:
            has_solid = False
        if has_solid:
            objects.append(obj)
    return objects, None


def _check_object(
    obj: Any,
    settings: "tessellation.Settings",
    build_direction: str,
    overhang_angle_deg: float,
    check_self_intersections: bool,
    bed: tuple[float, float, float] | None,
    budget_ok: Any,
) -> dict[str, Any]:
    issues: list[str] = []
    shape = tessellation.shape_of(obj)
    is_mesh = tessellation.is_mesh_feature(obj)

    valid = closed = None
    solids = 0
    shape_check = None

    if shape is not None:
        try:
            valid = bool(shape.isValid())
        except Exception as exc:
            valid = False
            issues.append(f"could not check shape validity: {type(exc).__name__}: {exc}")
        try:
            closed = bool(shape.isClosed())
        except Exception as exc:
            closed = False
            issues.append(f"could not check whether the shape is closed: {type(exc).__name__}: {exc}")
        try:
            solids = len(shape.Solids)
        except Exception:
            solids = 0

        if valid is False:
            issues.append("shape is not valid (Part isValid() reports False)")
        if closed is False:
            issues.append("shape is not closed; it has open boundaries")

        if not budget_ok():
            issues.append("shape check skipped: the remaining time budget is low")
        else:
            try:
                shape.check(True)
                shape_check = "ok"
            except Exception as exc:
                shape_check = str(exc)
                issues.append(f"shape check failed: {shape_check}")
    elif not is_mesh:
        issues.append("object has no solid or mesh geometry to check")

    facets = 0
    mesh_solid = non_manifold = self_intersections = None
    bound_box = size = None
    fits_bed = None
    overhang_area = overhang_fraction = None

    mesh = None
    if shape is not None or is_mesh:
        if not budget_ok():
            issues.append("mesh checks skipped: the remaining time budget is low")
        elif shape is not None:
            mesh = tessellation.mesh_shape(shape, settings)
        else:
            mesh = tessellation.mesh_object(obj, settings)

    if mesh is not None:
        facets = serialize_int(mesh.CountFacets)
        mesh_solid = bool(mesh.isSolid())
        non_manifold = bool(mesh.hasNonManifolds())
        if not mesh_solid:
            issues.append("tessellated mesh is not a closed solid")
        if non_manifold:
            issues.append("tessellated mesh has non-manifold geometry")
        if check_self_intersections:
            if not budget_ok():
                issues.append("self-intersection check skipped: the remaining time budget is low")
            else:
                self_intersections = bool(mesh.hasSelfIntersections())
                if self_intersections:
                    issues.append("tessellated mesh has self-intersecting facets")

        bb = shape.BoundBox if shape is not None else mesh.BoundBox
        bound_box = bound_box_list(bb)
        size = _axis_size(bb, build_direction)
        fits_bed = _fits_bed(size, bed)
        if fits_bed is False:
            issues.append("does not fit the bed in any orientation")

        if not budget_ok():
            issues.append("overhang check skipped: the remaining time budget is low")
        else:
            area, fraction = _overhang_area(mesh, bb, build_direction, overhang_angle_deg)
            overhang_area = finite_or_none(area)
            overhang_fraction = finite_or_none(fraction)

    return {
        "name": obj.Name,
        "label": obj.Label,
        "valid": valid,
        "closed": closed,
        "solids": solids,
        "shape_check": shape_check,
        "facets": facets,
        "mesh_solid": mesh_solid,
        "non_manifold": non_manifold,
        "self_intersections": self_intersections,
        "bound_box": bound_box,
        "size": size,
        "fits_bed": fits_bed,
        "overhang_area_mm2": overhang_area,
        "overhang_fraction": overhang_fraction,
        "issues": issues,
    }


def _axis_size(bb: Any, build_direction: str) -> list[float | None]:
    """Return [a, b, h]: the bound box's extent along the two axes across the
    build direction, then along the build direction itself."""
    axis = build_direction[1]
    lengths = {"X": bb.XLength, "Y": bb.YLength, "Z": bb.ZLength}
    h = lengths[axis]
    remaining = [name for name in ("X", "Y", "Z") if name != axis]
    a, b = lengths[remaining[0]], lengths[remaining[1]]
    return [finite_or_none(a), finite_or_none(b), finite_or_none(h)]


def _fits_bed(
    size: list[float | None], bed: tuple[float, float, float] | None
) -> bool | None:
    """Whether [a, b, h] fits a bed of [bed_x, bed_y, bed_z], rotating a/b as needed."""
    if bed is None:
        return None
    a, b, h = size
    if a is None or b is None or h is None:
        return None
    bed_x, bed_y, bed_z = bed
    if h > bed_z:
        return False
    return (a <= bed_x and b <= bed_y) or (a <= bed_y and b <= bed_x)


def _dot3(a: tuple[float, float, float], b: tuple[float, float, float]) -> float:
    return a[0] * b[0] + a[1] * b[1] + a[2] * b[2]


def _sub3(a: tuple[float, float, float], b: tuple[float, float, float]) -> tuple[float, float, float]:
    return (a[0] - b[0], a[1] - b[1], a[2] - b[2])


def _cross3(
    a: tuple[float, float, float], b: tuple[float, float, float]
) -> tuple[float, float, float]:
    return (
        a[1] * b[2] - a[2] * b[1],
        a[2] * b[0] - a[0] * b[2],
        a[0] * b[1] - a[1] * b[0],
    )


def _overhang_area(
    mesh: Any, bb: Any, build_direction: str, overhang_angle_deg: float
) -> tuple[float, float]:
    """Return (overhang area in mm^2, fraction of the mesh's total area).

    A facet is an overhang when its normal n satisfies n . d < -sin(angle),
    d being the unit build direction, unless every one of its vertices lies
    within _BASE_FACET_TOLERANCE_MM of the mesh's lowest point along d (it
    rests on the bed itself and needs no support). ``bb`` is the object's
    bound box, computed once by the caller and reused here.

    Reads mesh.Topology (points once, then facets as plain index triples)
    rather than iterating mesh.Facets, which would build a FacetPy, a
    VectorPy and a list of point tuples for every facet; the facet's area and
    normal are instead computed here from its three points.
    """
    d = DIRECTION_VECTORS[build_direction]
    corners = (
        (bb.XMin, bb.YMin, bb.ZMin), (bb.XMax, bb.YMin, bb.ZMin),
        (bb.XMin, bb.YMax, bb.ZMin), (bb.XMax, bb.YMax, bb.ZMin),
        (bb.XMin, bb.YMin, bb.ZMax), (bb.XMax, bb.YMin, bb.ZMax),
        (bb.XMin, bb.YMax, bb.ZMax), (bb.XMax, bb.YMax, bb.ZMax),
    )
    low_point = min(_dot3(corner, d) for corner in corners)
    threshold = -math.sin(math.radians(overhang_angle_deg))

    points, facets = mesh.Topology
    pts = [(p.x, p.y, p.z) for p in points]

    area = 0.0
    for i0, i1, i2 in facets:
        p0, p1, p2 = pts[i0], pts[i1], pts[i2]
        cross = _cross3(_sub3(p1, p0), _sub3(p2, p0))
        cross_len = math.sqrt(cross[0] * cross[0] + cross[1] * cross[1] + cross[2] * cross[2])
        if cross_len <= 0.0:
            continue  # degenerate facet: no area, no defined normal
        facet_area = 0.5 * cross_len
        normal = (cross[0] / cross_len, cross[1] / cross_len, cross[2] / cross_len)
        if _dot3(normal, d) >= threshold:
            continue
        if all(abs(_dot3(pt, d) - low_point) <= _BASE_FACET_TOLERANCE_MM for pt in (p0, p1, p2)):
            continue
        area += facet_area

    total_area = mesh.Area
    fraction = area / total_area if total_area > 0 else 0.0
    return area, fraction

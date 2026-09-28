"""Mesh analysis, repair and conversion.

analyze_mesh reads a Mesh::Feature; repair_mesh edits it; mesh_to_solid and
solid_to_mesh create new objects. The mutating tools run inside
transactions.transaction. Part is imported inside the GUI task where it is
needed (mesh_to_solid); MeshPart is used only through tessellation.py.
"""

import math
from typing import Any

from rpc_server import gui_task, tessellation, transactions
from rpc_server.errors import CONFLICT, INVALID_INPUT, fail, tool_call
from rpc_server.lookup import require_document, require_object
from rpc_server.options import check_options
from rpc_server.transactions import active_document
from rpc_server.serialize import bound_box_list, finite_or_none, serialize_int


ANALYZE_TIMEOUT = 120.0
REPAIR_TIMEOUT = 300.0
CONVERT_TIMEOUT = 300.0

REPAIR_STEPS = (
    "fix_indices",
    "remove_invalid_points",
    "remove_duplicated_points",
    "remove_duplicated_facets",
    "fix_degenerations",
    "fix_deformations",
    "remove_non_manifolds",
    "remove_non_manifold_points",
    "fix_self_intersections",
    "remove_folds",
    "harmonize_normals",
    "flip_normals",
    "fill_holes",
)
DEFAULT_REPAIR_STEPS = (
    "fix_indices",
    "remove_duplicated_points",
    "remove_duplicated_facets",
    "fix_degenerations",
    "remove_non_manifolds",
    "fix_self_intersections",
    "harmonize_normals",
    "fill_holes",
)
DEFAULT_FILL_HOLES_MAX_EDGES = 20

# mesh_to_solid makes one face per triangle; beyond this it needs force.
MAX_SOLID_FACETS = 200000

# Default tolerance (mm) used to sew mesh_to_solid's faces together.
DEFAULT_TOLERANCE = 0.1

# FreeCAD's own "Evaluate & Repair Mesh" dialog uses a 5 degree default for
# the maximum angle a deformed facet's normal may drift
# (Mod/Mesh/App/FeatureMeshDefects.cpp:211); fixDeformations has no default of
# its own, so fix_deformations uses the same value.
DEFAULT_DEFORMATION_ANGLE_DEG = 5.0

REPAIR_OPTIONS = ("fill_holes_max_edges",)
MESH_TO_SOLID_OPTIONS = ("result_name", "tolerance", "refine", "force")
SOLID_TO_MESH_OPTIONS = ("result_name", "quality", "linear_deflection", "angular_deflection_deg", "relative")


def _find_object(doc_name: str, obj_name: str):
    """Return ``(doc, obj, None)``, or ``(None, None, fail dict)``.

    Must run on the GUI thread, inside the dispatched task.
    """
    doc, error = require_document(doc_name)
    if error is not None:
        return None, None, error
    obj, error = require_object(doc, obj_name)
    if error is not None:
        return None, None, error
    return doc, obj, None


def _require_mesh_feature(obj: Any, doc_name: str, obj_name: str) -> dict[str, Any] | None:
    """Return a failure reply when ``obj`` is not a Mesh::Feature, else None."""
    if tessellation.is_mesh_feature(obj):
        return None
    return fail(
        INVALID_INPUT,
        f"'{obj_name}' is a {obj.TypeId}, not a mesh object (Mesh::Feature).",
        "Call " + tool_call("solid_to_mesh", {"doc_name": doc_name, "obj_name": obj_name}) + " to make one from it.",
    )


def _mesh_findings(mesh: Any) -> dict[str, Any]:
    """Return the raw defect flags and counts of ``mesh``."""
    return {
        "is_solid": bool(mesh.isSolid()),
        "non_manifolds": bool(mesh.hasNonManifolds()),
        "self_intersections": bool(mesh.hasSelfIntersections()),
        "non_uniform_oriented_facets": int(mesh.countNonUniformOrientedFacets()),
        "invalid_points": bool(mesh.hasInvalidPoints()),
        # Not mesh.hasCorruptedFacets(): in FreeCAD 1.1.3 that Python binding
        # calls hasFacetsOutOfRange() instead of the corrupted-facets check
        # (a copy-paste bug, Mod/Mesh/App/MeshPyImp.cpp:1157-1164), so it
        # would silently report the wrong defect. Compute the out-of-range
        # check directly on both facets and points instead.
        "corrupted_facets": bool(mesh.hasFacetsOutOfRange() or mesh.hasPointsOutOfRange()),
        "points_on_edge": bool(mesh.hasPointsOnEdge()),
        "components": int(mesh.countComponents()),
    }


def _issues_from_findings(findings: dict[str, Any]) -> list[str]:
    issues: list[str] = []
    if findings["invalid_points"]:
        issues.append("The mesh has points with invalid (NaN) coordinates.")
    if findings["corrupted_facets"]:
        issues.append("The mesh has facet or point indices out of range.")
    if findings["non_manifolds"]:
        issues.append("The mesh has non-manifold edges.")
    if findings["self_intersections"]:
        issues.append("The mesh has self-intersecting facets.")
    if findings["non_uniform_oriented_facets"]:
        issues.append(f"{findings['non_uniform_oriented_facets']} facet(s) have inconsistent orientation.")
    if findings["points_on_edge"]:
        issues.append("Some points lie exactly on the edge of another facet.")
    if not findings["is_solid"]:
        issues.append("The mesh is not a closed solid; it has open (border) edges.")
    if findings["components"] > 1:
        issues.append(f"The mesh has {findings['components']} separate components.")
    return issues


# Maps a defect flag to the repair step that addresses it, in REPAIR_STEPS
# order, so suggested_steps reads as a sensible repair sequence.
_STEP_FOR_FINDING = (
    ("corrupted_facets", "fix_indices"),
    ("invalid_points", "remove_invalid_points"),
    ("non_manifolds", "remove_non_manifolds"),
    ("self_intersections", "fix_self_intersections"),
    ("non_uniform_oriented_facets", "harmonize_normals"),
    ("not_solid", "fill_holes"),
)


def _suggested_steps(findings: dict[str, Any]) -> list[str]:
    flags = dict(findings)
    flags["not_solid"] = not findings["is_solid"]
    return [step for key, step in _STEP_FOR_FINDING if flags.get(key)]


def analyze_mesh(doc_name: str, obj_name: str, timeout: Any = None) -> dict[str, Any]:
    """Report the defects of a mesh object.

    GUI thread, default timeout ``ANALYZE_TIMEOUT``. No transaction.
    """
    resolved = gui_task.resolve_timeout(timeout, ANALYZE_TIMEOUT)
    if isinstance(resolved, dict):
        return resolved

    def task() -> dict[str, Any]:
        _doc, obj, error = _find_object(doc_name, obj_name)
        if error:
            return error
        type_error = _require_mesh_feature(obj, doc_name, obj_name)
        if type_error:
            return type_error

        mesh = obj.Mesh
        findings = _mesh_findings(mesh)
        return {
            "success": True,
            "document": doc_name,
            "object": obj.Name,
            "points": serialize_int(mesh.CountPoints),
            "facets": serialize_int(mesh.CountFacets),
            "is_solid": findings["is_solid"],
            "non_manifolds": findings["non_manifolds"],
            "self_intersections": findings["self_intersections"],
            "non_uniform_oriented_facets": serialize_int(findings["non_uniform_oriented_facets"]),
            "invalid_points": findings["invalid_points"],
            "corrupted_facets": findings["corrupted_facets"],
            "points_on_edge": findings["points_on_edge"],
            "components": serialize_int(findings["components"]),
            "volume": finite_or_none(mesh.Volume) if findings["is_solid"] else None,
            "area": finite_or_none(mesh.Area),
            "bound_box": bound_box_list(mesh.BoundBox),
            "issues": _issues_from_findings(findings),
            "suggested_steps": _suggested_steps(findings),
        }

    return gui_task.run_on_gui(task, resolved, "analyze_mesh", "analyze_mesh")


def _apply_repair_step(mesh: Any, step: str, fill_holes_max_edges: int) -> None:
    """Run one named repair step on ``mesh`` in place."""
    if step == "fix_indices":
        mesh.fixIndices()
    elif step == "remove_invalid_points":
        mesh.removeInvalidPoints()
    elif step == "remove_duplicated_points":
        mesh.removeDuplicatedPoints()
    elif step == "remove_duplicated_facets":
        mesh.removeDuplicatedFacets()
    elif step == "fix_degenerations":
        mesh.fixDegenerations()
    elif step == "fix_deformations":
        mesh.fixDeformations(math.radians(DEFAULT_DEFORMATION_ANGLE_DEG))
    elif step == "remove_non_manifolds":
        mesh.removeNonManifolds()
    elif step == "remove_non_manifold_points":
        mesh.removeNonManifoldPoints()
    elif step == "fix_self_intersections":
        mesh.fixSelfIntersections()
    elif step == "remove_folds":
        mesh.removeFoldsOnSurface()
    elif step == "harmonize_normals":
        mesh.harmonizeNormals()
    elif step == "flip_normals":
        mesh.flipNormals()
    elif step == "fill_holes":
        mesh.fillupHoles(fill_holes_max_edges)
    else:  # pragma: no cover - already validated by repair_mesh
        raise ValueError(f"unknown repair step: {step!r}")


def repair_mesh(
    doc_name: str,
    obj_name: str,
    steps: list[str] | None = None,
    options: dict[str, Any] | None = None,
    timeout: Any = None,
) -> dict[str, Any]:
    """Run named repair steps on a mesh object in place.

    GUI thread, default timeout ``REPAIR_TIMEOUT``, transaction
    ``MCP: repair_mesh``.
    """
    resolved = gui_task.resolve_timeout(timeout, REPAIR_TIMEOUT)
    if isinstance(resolved, dict):
        return resolved

    if steps is None:
        steps = list(DEFAULT_REPAIR_STEPS)
    else:
        if not isinstance(steps, list) or not all(isinstance(s, str) for s in steps):
            return fail(INVALID_INPUT, "steps must be a list of repair step names.")
        unknown = [s for s in steps if s not in REPAIR_STEPS]
        if unknown:
            return fail(
                INVALID_INPUT,
                f"Unknown repair step(s) {', '.join(unknown)}; accepted: {', '.join(REPAIR_STEPS)}.",
            )
        steps = list(steps)

    options, error = check_options(options, REPAIR_OPTIONS)
    if error:
        return error
    fill_holes_max_edges = options.get("fill_holes_max_edges", DEFAULT_FILL_HOLES_MAX_EDGES)
    if isinstance(fill_holes_max_edges, bool) or not isinstance(fill_holes_max_edges, (int, float)):
        return fail(INVALID_INPUT, "fill_holes_max_edges must be an integer from 3 to 10000.")
    fill_holes_max_edges = int(fill_holes_max_edges)
    if not (3 <= fill_holes_max_edges <= 10000):
        return fail(INVALID_INPUT, "fill_holes_max_edges must be an integer from 3 to 10000.")

    def task() -> dict[str, Any]:
        doc, obj, error = _find_object(doc_name, obj_name)
        if error:
            return error
        type_error = _require_mesh_feature(obj, doc_name, obj_name)
        if type_error:
            return type_error

        with active_document(doc), transactions.transaction("repair_mesh") as tx:
            mesh = obj.Mesh.copy()
            before = {
                "points": serialize_int(mesh.CountPoints),
                "facets": serialize_int(mesh.CountFacets),
                "is_solid": bool(mesh.isSolid()),
                "non_manifolds": bool(mesh.hasNonManifolds()),
            }
            step_reports = []
            for step in steps:
                facets_before = mesh.CountFacets
                _apply_repair_step(mesh, step, fill_holes_max_edges)
                step_reports.append({
                    "step": step,
                    "facets_before": serialize_int(facets_before),
                    "facets_after": serialize_int(mesh.CountFacets),
                })
            obj.Mesh = mesh
            doc.recompute()
            after = {
                "points": serialize_int(mesh.CountPoints),
                "facets": serialize_int(mesh.CountFacets),
                "is_solid": bool(mesh.isSolid()),
                "non_manifolds": bool(mesh.hasNonManifolds()),
            }
            reply = {
                "success": True,
                "document": doc_name,
                "object": obj.Name,
                "steps": step_reports,
                "before": before,
                "after": after,
                "issues": _issues_from_findings(_mesh_findings(mesh)),
            }
            reply.update(tx.reply_fields())
            return reply

    return gui_task.run_on_gui(task, resolved, "repair_mesh", "repair_mesh")


def mesh_to_solid(
    doc_name: str,
    obj_name: str,
    options: dict[str, Any] | None = None,
    timeout: Any = None,
) -> dict[str, Any]:
    """Create a Part solid from a mesh object.

    GUI thread, default timeout ``CONVERT_TIMEOUT``, transaction
    ``MCP: mesh_to_solid``.
    """
    resolved = gui_task.resolve_timeout(timeout, CONVERT_TIMEOUT)
    if isinstance(resolved, dict):
        return resolved

    options, error = check_options(options, MESH_TO_SOLID_OPTIONS)
    if error:
        return error

    result_name = options.get("result_name") or None
    if result_name is not None and not isinstance(result_name, str):
        return fail(INVALID_INPUT, "result_name must be a string.")

    tolerance = options.get("tolerance", DEFAULT_TOLERANCE)
    if isinstance(tolerance, bool) or not isinstance(tolerance, (int, float)) or not (0 < tolerance <= 10):
        return fail(INVALID_INPUT, "tolerance must be a number greater than 0 and at most 10 (mm).")
    tolerance = float(tolerance)

    refine = options.get("refine", False)
    if not isinstance(refine, bool):
        return fail(INVALID_INPUT, "refine must be true or false.")

    force = options.get("force", False)
    if not isinstance(force, bool):
        return fail(INVALID_INPUT, "force must be true or false.")

    def task() -> dict[str, Any]:
        doc, obj, error = _find_object(doc_name, obj_name)
        if error:
            return error
        type_error = _require_mesh_feature(obj, doc_name, obj_name)
        if type_error:
            return type_error

        mesh = obj.Mesh
        facet_count = mesh.CountFacets
        if facet_count > MAX_SOLID_FACETS and not force:
            return fail(
                CONFLICT,
                f"The mesh has {facet_count} facets, more than the {MAX_SOLID_FACETS} mesh_to_solid converts "
                "without force; each triangle becomes a face, so this can take minutes and much memory.",
                "Call " + tool_call("mesh_to_solid", {"doc_name": doc_name, "obj_name": obj_name, "force": True})
                + " to convert it anyway.",
            )

        with active_document(doc), transactions.transaction("mesh_to_solid") as tx:
            import Part

            shape = Part.Shape()
            # sewShape True: stitch the mesh's triangles into a connected
            # shell topology instead of a compound of unconnected faces
            # (Mod/Part/App/TopoShapePyImp.cpp:1886-1923).
            shape.makeShapeFromMesh(mesh.Topology, tolerance, True)

            not_closed_warning = (
                "The mesh is not a closed solid, so mesh_to_solid created a shell instead of a solid; repair it "
                "with " + tool_call("repair_mesh", {
                    "doc_name": doc_name, "obj_name": obj_name, "steps": ["remove_non_manifolds", "fill_holes"],
                }) + " and try again."
            )
            warnings: list[str] = []
            is_solid = False
            result_shape = shape
            try:
                shell = Part.Shell(shape.Faces)
                try:
                    solid = Part.Solid(shell)
                    if solid.isValid() and solid.isClosed():
                        result_shape = solid
                        is_solid = True
                    else:
                        result_shape = shell
                        warnings.append(not_closed_warning)
                except Exception:
                    result_shape = shell
                    warnings.append(not_closed_warning)
            except Exception:
                result_shape = shape
                warnings.append(
                    "The mesh's faces could not be sewn into a shell, so mesh_to_solid created a compound of "
                    "separate faces instead."
                )

            refined = False
            if refine:
                try:
                    result_shape = result_shape.removeSplitter()
                    refined = True
                except Exception:
                    warnings.append("refine was requested but merging coplanar faces failed; the faces were kept as is.")

            name = result_name or f"{obj_name}_solid"
            new_obj = doc.addObject("Part::Feature", name)
            new_obj.Shape = result_shape
            # mesh.Topology's points already carry the mesh object's own
            # Placement (MeshObject::getPoint, Mod/Mesh/App/Mesh.cpp:322-328,
            # which Mesh::Feature keeps in sync with its own Placement
            # property), so only the container's placement above that still
            # needs applying; a fresh Part::Feature already defaults to the
            # identity Placement, so nothing is set when there is no container.
            container = tessellation.container_placement(obj)
            if container is not None:
                new_obj.Placement = container
            doc.recompute()

            reply = {
                "success": True,
                "document": doc_name,
                "object": obj.Name,
                "created_object": {"name": new_obj.Name, "label": new_obj.Label, "type": new_obj.TypeId},
                "is_solid": is_solid,
                "faces": serialize_int(len(result_shape.Faces)),
                "volume": finite_or_none(result_shape.Volume) if is_solid else None,
                "refined": refined,
                "warnings": warnings,
            }
            reply.update(tx.reply_fields())
            return reply

    return gui_task.run_on_gui(task, resolved, "mesh_to_solid", "mesh_to_solid")


def solid_to_mesh(
    doc_name: str,
    obj_name: str,
    options: dict[str, Any] | None = None,
    timeout: Any = None,
) -> dict[str, Any]:
    """Create a mesh object from an object's shape with tessellation.py.

    GUI thread, default timeout ``CONVERT_TIMEOUT``, transaction
    ``MCP: solid_to_mesh``.
    """
    resolved = gui_task.resolve_timeout(timeout, CONVERT_TIMEOUT)
    if isinstance(resolved, dict):
        return resolved

    options, error = check_options(options, SOLID_TO_MESH_OPTIONS)
    if error:
        return error

    result_name = options.get("result_name") or None
    if result_name is not None and not isinstance(result_name, str):
        return fail(INVALID_INPUT, "result_name must be a string.")

    settings = tessellation.resolve_settings(
        options.get("quality"),
        options.get("linear_deflection"),
        options.get("angular_deflection_deg"),
        options.get("relative"),
    )
    if isinstance(settings, dict):
        return settings

    def task() -> dict[str, Any]:
        doc, obj, error = _find_object(doc_name, obj_name)
        if error:
            return error
        if tessellation.is_mesh_feature(obj):
            return fail(
                INVALID_INPUT,
                f"'{obj_name}' is already a mesh (Mesh::Feature); solid_to_mesh tessellates a Part shape "
                "instead. Use analyze_mesh or repair_mesh on it directly.",
                "Call " + tool_call("analyze_mesh", {"doc_name": doc_name, "obj_name": obj_name})
                + " to inspect it instead.",
            )
        mesh = tessellation.mesh_object(obj, settings)
        if mesh is None:
            return fail(
                INVALID_INPUT,
                f"'{obj_name}' has no shape geometry to tessellate.",
                "Call " + tool_call("get_object", {"doc_name": doc_name, "obj_name": obj_name})
                + " to see its properties.",
            )

        with active_document(doc), transactions.transaction("solid_to_mesh") as tx:
            name = result_name or f"{obj_name}_mesh"
            new_obj = doc.addObject("Mesh::Feature", name)
            new_obj.Mesh = mesh
            doc.recompute()

            reply = {
                "success": True,
                "document": doc_name,
                "object": obj.Name,
                "created_object": {"name": new_obj.Name, "label": new_obj.Label, "type": new_obj.TypeId},
                "points": serialize_int(mesh.CountPoints),
                "facets": serialize_int(mesh.CountFacets),
                "is_solid": bool(mesh.isSolid()),
                "non_manifolds": bool(mesh.hasNonManifolds()),
                "settings": settings.as_dict(),
            }
            reply.update(tx.reply_fields())
            return reply

    return gui_task.run_on_gui(task, resolved, "solid_to_mesh", "solid_to_mesh")

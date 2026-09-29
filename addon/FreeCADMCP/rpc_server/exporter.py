"""Export objects of a document to a file.

Mesh formats are tessellated with tessellation.py into a temporary hidden
document and written with Mesh.export; STEP/IGES and glTF go through
ImportGui.export; BREP through Part.makeCompound(...).exportBrep; FCStd
through doc.saveCopy; DXF and SVG through importDXF.export and
importSVG.export. Every export is verified on disk, because Mesh.export (and
doc.saveCopy onto the document's own file) swallow failures instead of
raising or returning a usable signal to Python. Process-global settings
changed for a call (Part.exportUnits, the STEP schema preference, the DXF
exporter choice) are restored afterwards.

Never ImportGui.exportOptions. FreeCAD is imported at module level (light);
Mesh, MeshPart, Part, ImportGui, importDXF and importSVG are imported inside
the functions that need them, on the GUI thread only.
"""

import math
import os
import tempfile
from typing import Any

import FreeCAD as App

from rpc_server import gui_task, tessellation
from rpc_server.errors import (
    CONFLICT,
    FREECAD_ERROR,
    INTERNAL_ERROR,
    INVALID_INPUT,
    NOT_FOUND,
    fail,
    tool_call,
)
from rpc_server.lookup import require_document
from rpc_server.object_validation import object_validity_error
from rpc_server.options import check_options
from rpc_server.paths import ensure_parent_directory, require_absolute_path
from rpc_server.serialize import bound_box_list, serialize_int, visibility_of


EXPORT_TIMEOUT = 300.0

# Lower-case extensions without the dot, matching the export format table
# below (one exporter name per extension).
EXPORT_FORMATS = (
    "stl", "ast", "3mf", "amf", "obj", "ply", "off",
    "step", "stp", "iges", "igs",
    "glb", "gltf",
    "brep", "brp",
    "fcstd",
    "dxf",
    "svg",
)

MESH_FORMATS = frozenset({"stl", "ast", "3mf", "amf", "obj", "ply", "off"})
STEP_FORMATS = frozenset({"step", "stp", "iges", "igs"})
GLTF_FORMATS = frozenset({"glb", "gltf"})
BREP_FORMATS = frozenset({"brep", "brp"})
SHAPE_ONLY_FORMATS = STEP_FORMATS | GLTF_FORMATS | BREP_FORMATS | frozenset({"dxf", "svg"})
# Formats that do not route through tessellation.shape_of, so an object
# nested in a moved App::Part or Body exports at its local position.
LOCAL_PLACEMENT_FORMATS = STEP_FORMATS | GLTF_FORMATS | frozenset({"dxf", "svg"})

# Keys accepted in ``options``.
EXPORT_OPTIONS = (
    "object_names", "overwrite", "include_hidden", "recompute",
    "quality", "linear_deflection", "angular_deflection_deg", "relative", "ascii",
    "step_unit", "step_schema",
)
STEP_UNITS = ("MM", "M", "INCH")
STEP_SCHEMAS = ("AP203", "AP214IS", "AP242DIS")
# The full set AppImportGuiPy.cpp's exporter itself accepts for the Scheme
# preference (Part::supportedSTEPSchemes, Mod/Part/App/ImportStep.h:42-51),
# wider than STEP_SCHEMAS (the step_schema option's contract-supported
# subset): a value being restored came from the user's own preference or
# FreeCAD's STEP export dialog, not from this tool's own input validation.
STEP_SUPPORTED_SCHEMES = frozenset({"AP203", "AP214CD", "AP214DIS", "AP214IS", "AP242DIS"})
# The schema AppImportGuiPy.cpp's exporter falls back to when the Scheme
# preference is absent is the current process-global static, not a fixed
# value (Mod/Import/Gui/AppImportGuiPy.cpp:580-583), and FreeCAD seeds that
# static at startup from the same, then-absent, preference
# (Mod/Part/App/OCAF/ImportExportSettings.cpp:150-151). Verified live on the
# installed FreeCAD 1.1.3 with no Scheme preference: Part.exportStep writes
# FILE_SCHEMA(('AUTOMOTIVE_DESIGN { 1 0 10303 214 1 1 1 1 }')), i.e. AP214IS.
STEP_DEFAULT_SCHEME = "AP214IS"

_EXPORTER_NAMES: dict[str, str] = {}
_EXPORTER_NAMES.update({ext: "Mesh.export" for ext in MESH_FORMATS})
_EXPORTER_NAMES.update({ext: "ImportGui.export" for ext in STEP_FORMATS | GLTF_FORMATS})
_EXPORTER_NAMES.update({ext: "Part.exportBrep" for ext in BREP_FORMATS})
_EXPORTER_NAMES["fcstd"] = "Document.saveCopy"
_EXPORTER_NAMES["dxf"] = "importDXF.export"
_EXPORTER_NAMES["svg"] = "importSVG.export"


def _is_visible(obj: Any) -> bool:
    # An object with no ViewObject (a headless document) is not hidden by
    # anything, so it counts as visible rather than being silently dropped
    # from the default export set.
    return bool(visibility_of(obj, True))


def _has_geometry(obj: Any) -> bool:
    """True when ``obj`` has a mesh or a non-null Part shape to export."""
    if tessellation.is_mesh_feature(obj):
        try:
            return obj.Mesh is not None and obj.Mesh.CountFacets > 0
        except Exception:
            return False
    return tessellation.shape_of(obj) is not None


def _mesh_reachable_via(obj: Any) -> bool:
    """True when ``obj`` has no shape or mesh of its own, but reaches a
    Mesh::Feature through a link target or a container's children.

    ``Part.getShape`` gives a null shape for a Link (or similar) whose target
    is a Mesh::Feature (Mod/Part/App/PartFeature.cpp:1015-1035), and an
    App::Part's compound shape does not include its Mesh::Feature children,
    so such an object is skipped from mesh exports; this only picks a
    clearer reason for it.
    """
    get_linked = getattr(obj, "getLinkedObject", None)
    if callable(get_linked):
        try:
            target = obj.getLinkedObject(True)
        except Exception:
            target = None
        if target is not None and target is not obj and tessellation.is_mesh_feature(target):
            return True
    for child in getattr(obj, "Group", None) or []:
        if tessellation.is_mesh_feature(child):
            return True
    return False


def _contained_meshes(obj: Any, seen: set[int] | None = None) -> list:
    """Mesh::Feature objects in ``obj``'s ``Group`` tree, recursively.

    A container's own Part shape (a compound of its Part children,
    Mod/Part/App/PartFeature.cpp:1015-1035) never includes a Mesh::Feature
    child, so a mixed container passes ``_has_geometry`` on its shape alone
    and its meshes would otherwise vanish from a mesh export with no sign of
    it (unlike a mesh-only container, which ``_mesh_reachable_via`` already
    catches because such a container has no shape of its own).
    """
    if seen is None:
        seen = set()
    found = []
    for child in getattr(obj, "Group", None) or []:
        child_id = id(child)
        if child_id in seen:
            continue
        seen.add(child_id)
        if tessellation.is_mesh_feature(child):
            found.append(child)
        else:
            found.extend(_contained_meshes(child, seen))
    return found


def _validate_options(options: Any) -> dict[str, Any]:
    """Return a normalised options dict, or a fail() reply.

    Runs on the RPC thread: only checks that need no FreeCAD state.
    """
    options, error = check_options(options, EXPORT_OPTIONS)
    if error is not None:
        return error

    out: dict[str, Any] = {}

    def _bool(name: str, default: bool) -> Any:
        value = options.get(name)
        if value is None:
            return default
        if not isinstance(value, bool):
            return fail(INVALID_INPUT, f"{name} must be true or false, not {value!r}")
        return value

    for name, default in (
        ("overwrite", False),
        ("include_hidden", False),
        ("recompute", True),
        ("ascii", False),
    ):
        value = _bool(name, default)
        if isinstance(value, dict):
            return value
        out[name] = value

    object_names = options.get("object_names")
    if object_names is not None:
        if not isinstance(object_names, list) or not all(isinstance(n, str) for n in object_names):
            return fail(INVALID_INPUT, "object_names must be a list of object names")
    out["object_names"] = object_names

    step_unit = options.get("step_unit")
    if step_unit is not None and step_unit not in STEP_UNITS:
        return fail(INVALID_INPUT, f"step_unit must be one of {', '.join(STEP_UNITS)}, not {step_unit!r}")
    out["step_unit"] = step_unit

    step_schema = options.get("step_schema")
    if step_schema is not None and step_schema not in STEP_SCHEMAS:
        return fail(INVALID_INPUT, f"step_schema must be one of {', '.join(STEP_SCHEMAS)}, not {step_schema!r}")
    out["step_schema"] = step_schema

    settings = tessellation.resolve_settings(
        options.get("quality"),
        options.get("linear_deflection"),
        options.get("angular_deflection_deg"),
        options.get("relative"),
    )
    if isinstance(settings, dict):
        return settings
    out["tessellation"] = settings

    return out


def _stat_snapshot(path: str) -> tuple[int, int, int] | None:
    """Return (size, mtime_ns, inode) for an existing file, or None."""
    try:
        st = os.stat(path)
    except OSError:
        return None
    return st.st_size, st.st_mtime_ns, getattr(st, "st_ino", 0)


def _verify_written(path: str, before: tuple[int, int, int] | None) -> tuple[bool, int]:
    """Check the file export actually wrote, since some exporters (Mesh.export,
    doc.saveCopy onto its own file) can silently write nothing.

    ``before`` is ``_stat_snapshot(path)`` taken right before the writer ran
    (None when the file did not exist yet), compared against the file's stat
    now rather than against the local clock, which a network share's clock
    skew or a coarse filesystem mtime can defeat either way.
    """
    try:
        st = os.stat(path)
    except OSError:
        return False, 0
    if st.st_size <= 0:
        return False, 0
    after = (st.st_size, st.st_mtime_ns, getattr(st, "st_ino", 0))
    if before is not None and after == before:
        return False, 0
    return True, st.st_size


def _units_for(ext: str, opts: dict[str, Any]) -> str:
    if ext in MESH_FORMATS or ext in BREP_FORMATS or ext == "fcstd":
        return "mm"
    if ext in GLTF_FORMATS:
        return "m"
    if ext in STEP_FORMATS:
        step_unit = opts.get("step_unit")
        if step_unit:
            return str(step_unit).lower()
        import Part

        current = Part.exportUnits()
        key = "write.iges.unit" if ext in ("iges", "igs") else "write.step.unit"
        return str(current.get(key, "mm")).lower()
    # dxf, svg: FreeCAD writes the document's own mm coordinates unscaled
    # unless the (separate, rarely used) Draft scaling preference changes it.
    return "mm"


def _export_mesh(
    objects: list, path: str, ext: str, opts: dict[str, Any], skipped: list[dict[str, str]]
) -> tuple[dict[str, Any] | None, list]:
    """Tessellate and write ``objects``; returns (mesh info, objects actually written).

    ``mesh_info`` is None, with an empty second element, when not one object
    tessellated to a usable mesh (every one is by then in ``skipped``); the
    caller turns that into ``invalid_input`` rather than writing nothing.

    An object that tessellates to zero facets (a sketch or a wire has a
    non-null Shape but no faces) is left out of the file and appended to
    ``skipped`` instead of being written as a degenerate, empty part.
    """
    import Mesh

    settings = opts["tessellation"]
    previous_active = App.ActiveDocument.Name if App.ActiveDocument else None
    temp_doc = App.newDocument("MCPExport", hidden=True, temp=True)
    try:
        meshes = []
        exported = []
        for obj in objects:
            mesh = tessellation.mesh_object(obj, settings)
            if mesh is None or mesh.CountFacets <= 0:
                skipped.append({"name": obj.Name, "reason": "tessellated to no facets (no faces to export)"})
                continue
            feature = temp_doc.addObject("Mesh::Feature", obj.Name)
            feature.Mesh = mesh
            feature.Label = obj.Label
            meshes.append((mesh, feature))
            exported.append(obj)
        if not meshes:
            return None, []

        mesh_features = [feature for _, feature in meshes]
        if ext == "stl" and opts["ascii"]:
            # Mesh.export picks binary vs ASCII STL from the extension
            # (MeshIO::GetFormat: .stl -> BSTL, .ast -> ASTL), so an ASCII
            # STL is written to a temp .ast file and moved onto path.
            # MergeExporter::write swallows a save error, so the temp file's
            # size is checked before it replaces a possibly-good target, and
            # its mode is normalised from mkstemp's owner-only 0600.
            directory = os.path.dirname(path) or "."
            fd, tmp_path = tempfile.mkstemp(suffix=".ast", dir=directory)
            os.close(fd)
            try:
                Mesh.export(mesh_features, tmp_path)
                if os.path.getsize(tmp_path) <= 0:
                    raise RuntimeError("the ASCII STL writer produced an empty file")
                try:
                    mask = os.umask(0)
                    os.umask(mask)
                    os.chmod(tmp_path, 0o666 & ~mask)
                except OSError:
                    pass
                os.replace(tmp_path, path)
            finally:
                if os.path.exists(tmp_path):
                    try:
                        os.remove(tmp_path)
                    except OSError:
                        pass
        else:
            Mesh.export(mesh_features, path)

        total_facets = 0
        all_closed = True
        bound_box = None
        for mesh, _ in meshes:
            total_facets += mesh.CountFacets
            if not mesh.isSolid():
                all_closed = False
            bb = mesh.BoundBox
            if bb.isValid():
                bound_box = bb if bound_box is None else bound_box.united(bb)
        mesh_info = {
            "facets": serialize_int(total_facets),
            "linear_deflection": settings.linear_deflection,
            "angular_deflection_deg": settings.angular_deflection_deg,
            "relative": settings.relative,
            "closed": all_closed,
            "bound_box": bound_box_list(bound_box) if bound_box is not None else [0.0] * 6,
        }
        return mesh_info, exported
    finally:
        App.closeDocument(temp_doc.Name)
        if previous_active and previous_active in App.listDocuments():
            App.setActiveDocument(previous_active)


def _export_step(objects: list, path: str, ext: str, opts: dict[str, Any]) -> None:
    import ImportGui
    import Part

    # write.step.unit and write.iges.unit are independent process-global
    # values; Part.exportUnits(unit) sets both together, so only the key for
    # the current format is touched (via setStaticValue) and restored.
    unit_key = "write.step.unit" if ext in ("step", "stp") else "write.iges.unit"
    step_unit = opts.get("step_unit")
    old_unit = None
    param = None
    scheme_existed = False
    old_scheme = None
    if step_unit:
        old_unit = Part.exportUnits().get(unit_key)
        Part.setStaticValue(unit_key, step_unit)
    if ext in ("step", "stp") and opts.get("step_schema"):
        param = App.ParamGet("User parameter:BaseApp/Preferences/Mod/Part/STEP")
        # GetString("Scheme", "") cannot tell an absent entry from one
        # explicitly set to "", so check membership first: SetString on an
        # absent key, restored the same way, would otherwise leave a
        # spurious empty entry in user.cfg.
        scheme_existed = "Scheme" in param.GetStrings()
        old_scheme = param.GetString("Scheme", "") if scheme_existed else None
        param.SetString("Scheme", opts["step_schema"])
    try:
        ImportGui.export(objects, path)
    finally:
        if step_unit and old_unit:
            Part.setStaticValue(unit_key, old_unit)
        if param is not None:
            if scheme_existed:
                # Always restore the parameter to its exact prior value
                # (SetString never validates and so never raises), even if
                # that value turns out not to be one setStaticValue accepts
                # below (an empty entry a past bug may have left, say).
                param.SetString("Scheme", old_scheme)
                restore_scheme = old_scheme
            else:
                # No prior preference: remove the entry instead of writing
                # an empty one.
                param.RemString("Scheme")
                restore_scheme = STEP_DEFAULT_SCHEME
            # ImportGui.export copies the parameter into the process-global
            # OCCT static write.step.schema (Mod/Import/Gui/AppImportGuiPy.cpp
            # exporter, :576-585); restoring only the parameter leaves later
            # exports in the session using the overridden schema, since with
            # no preference the exporter's own fallback is that same static,
            # not a fixed default (:580-583). setStaticValue raises for a
            # value OCCT does not accept (Mod/Part/App/AppPartPy.cpp:2447-2455,
            # for example the empty entry above), which must never turn an
            # already-written file into a reported failure.
            if restore_scheme in STEP_SUPPORTED_SCHEMES:
                try:
                    Part.setStaticValue("write.step.schema", restore_scheme)
                except Exception as e:
                    App.Console.PrintWarning(
                        "MCP RPC: could not restore the STEP schema preference to "
                        f"'{restore_scheme}': {type(e).__name__}: {e}\n"
                    )


def _export_gltf(objects: list, path: str, opts: dict[str, Any]) -> dict[str, Any]:
    import ImportGui
    import Part

    settings = opts["tessellation"]
    total_facets = 0
    all_closed = True
    bound_box = None
    for obj in objects:
        # Not tessellation.shape_of: it returns Part.getShape(obj).copy(),
        # an independent shape whose triangulation ImportGui.export never
        # sees. ExportOCAF2::exportObject (Mod/Import/App/ExportOCAF2.cpp,
        # exportObject) fetches each object's shape fresh through
        # Part::Feature::getTopoShape, the same call Part.getShape(obj)
        # makes (also resolving App::Link and App::Part containers), so
        # tessellating that shared shape here (without copying it, and
        # without ok=True, which would clean any existing triangulation)
        # is what the glTF writer later picks up.
        try:
            shape = Part.getShape(obj)
        except Exception:
            continue
        if shape is None or shape.isNull():
            continue
        _, facets = shape.tessellate(settings.linear_deflection)
        total_facets += len(facets)
        if not shape.isClosed():
            all_closed = False
        bb = shape.BoundBox
        if bb.isValid():
            bound_box = bb if bound_box is None else bound_box.united(bb)
    ImportGui.export(objects, path)
    # TopoShape.tessellate takes only a linear tolerance; the angular
    # deflection it derives internally is min(0.1, 5 * tolerance + 0.005)
    # radians (Mod/Part/App/TopoShape.cpp defaultAngularDeflection, used by
    # getFaces), reported here instead of the (unused) preset value. There is
    # no relative-deflection mode for this call. A face with an existing,
    # finer triangulation is left as it is (BRepMesh_IncrementalMesh only
    # refines, never coarsens, a mesh already fine enough).
    angular_rad = min(0.1, 5 * settings.linear_deflection + 0.005)
    return {
        "facets": serialize_int(total_facets),
        "linear_deflection": settings.linear_deflection,
        "angular_deflection_deg": math.degrees(angular_rad),
        "relative": False,
        "closed": all_closed,
        "bound_box": bound_box_list(bound_box) if bound_box is not None else [0.0] * 6,
    }


def _export_brep(objects: list, path: str) -> None:
    import Part

    shapes = [tessellation.shape_of(obj) for obj in objects]
    shapes = [s for s in shapes if s is not None]
    Part.makeCompound(shapes).exportBrep(path)


def _export_dxf(objects: list, path: str) -> None:
    import importDXF

    param = App.ParamGet("User parameter:BaseApp/Preferences/Mod/Draft")
    old_legacy = param.GetBool("dxfUseLegacyExporter", False)
    param.SetBool("dxfUseLegacyExporter", False)
    try:
        importDXF.export(objects, path)
    finally:
        param.SetBool("dxfUseLegacyExporter", old_legacy)


def _export_svg(objects: list, path: str) -> None:
    import importSVG

    importSVG.export(objects, path)


def _export_fcstd(doc: Any, path: str, opts: dict[str, Any]) -> dict[str, Any]:
    warnings: list[str] = []
    object_names = opts["object_names"]
    if object_names is not None:
        missing = [name for name in object_names if doc.getObject(name) is None]
        if missing:
            return fail(
                NOT_FOUND,
                f"no object(s) named {', '.join(missing)} in document {doc.Name!r}",
                "Call " + tool_call("list_objects", {"doc_name": doc.Name}) + " to see the document's objects.",
            )
        warnings.append(
            "object_names does not apply to a .FCStd export: the whole document is copied, "
            "not a subset of its objects."
        )
    if opts["include_hidden"]:
        warnings.append(
            "include_hidden does not apply to a .FCStd export: the whole document is copied."
        )

    target_real = os.path.normcase(os.path.realpath(path))
    current_file = doc.FileName or ""
    if current_file and os.path.normcase(os.path.realpath(current_file)) == target_real:
        return fail(
            CONFLICT,
            "Cannot export a copy onto the document's own file.",
            "Call " + tool_call("save_document", {"doc_name": doc.Name}) + " to save it normally instead.",
        )
    for other_name in App.listDocuments():
        if other_name == doc.Name:
            continue
        other_doc = App.getDocument(other_name)
        other_file = str(getattr(other_doc, "FileName", "") or "")
        if other_file and os.path.normcase(os.path.realpath(other_file)) == target_real:
            return fail(
                CONFLICT,
                f"'{path}' is already the file of open document '{other_name}'.",
                "Call " + tool_call("close_document", {"doc_name": other_name}) + " first, or choose another path.",
            )
    created_directory, dir_error = ensure_parent_directory(path)
    if dir_error is not None:
        return dir_error
    if os.path.exists(path) and not opts["overwrite"]:
        return fail(
            CONFLICT,
            f"{path} already exists",
            "Call " + tool_call("export_document", {"doc_name": doc.Name, "path": path, "overwrite": True})
            + " to replace it, or choose another path.",
        )
    before = _stat_snapshot(path)
    # doc.saveCopy always returns None to Python (the C++ bool result is
    # discarded); it silently no-ops onto the document's own file, which the
    # check above already refuses, and the write is verified below regardless.
    doc.saveCopy(path)
    ok, size = _verify_written(path, before)
    if not ok:
        return fail(FREECAD_ERROR, "The exporter wrote no file")
    reply: dict[str, Any] = {
        "success": True,
        "document": doc.Name,
        "file_name": os.path.abspath(path),
        "format": "fcstd",
        "exporter": _EXPORTER_NAMES["fcstd"],
        "bytes": serialize_int(size),
        "objects": [o.Name for o in doc.Objects],
        "skipped": [],
        "units": "mm",
        "warnings": warnings,
    }
    if created_directory:
        reply["created_directory"] = created_directory
    return reply


def _export(doc_name: str, path: str, ext: str, opts: dict[str, Any]) -> dict[str, Any]:
    doc, error = require_document(doc_name)
    if error is not None:
        return error

    if opts["recompute"]:
        doc.recompute()

    if ext == "fcstd":
        return _export_fcstd(doc, path, opts)

    warnings: list[str] = []
    object_names = opts["object_names"]
    if object_names is not None:
        objects = []
        missing = []
        for name in object_names:
            obj = doc.getObject(name)
            if obj is None:
                missing.append(name)
            else:
                objects.append(obj)
        if missing:
            return fail(
                NOT_FOUND,
                f"no object(s) named {', '.join(missing)} in document {doc_name!r}",
                "Call " + tool_call("list_objects", {"doc_name": doc_name}) + " to see the document's objects.",
            )
    else:
        # Tree-level objects, not doc.RootObjects: the latter is dependency
        # roots, which drops anything another object references (a Link's
        # target, an expression dependency, a TechDraw view's source).
        root_objects = tessellation.tree_root_objects(doc)
        objects = [obj for obj in root_objects if opts["include_hidden"] or _is_visible(obj)]
        if not opts["include_hidden"]:
            hidden_count = sum(1 for obj in root_objects if not _is_visible(obj))
            if hidden_count:
                warnings.append(
                    f"{hidden_count} hidden top-level object(s) were not exported; call "
                    + tool_call(
                        "export_document",
                        {"doc_name": doc_name, "path": path, "include_hidden": True, "overwrite": True},
                    )
                    + " to include them (the file this call wrote already exists at that path)."
                )

    if object_names is not None and ext in LOCAL_PLACEMENT_FORMATS:
        # Only these formats skip the container-placement fix (shape_of):
        # STEP/IGES/glTF resolve shapes themselves (ImportGui.export), and
        # DXF/SVG take the objects as they are.
        nested = [
            obj.Name for obj in objects
            if tessellation.parent_geo_feature_group(obj) is not None
        ]
        if nested:
            warnings.append(
                f"{', '.join(nested)}: this format does not apply an enclosing App::Part or Body's "
                "placement, unlike mesh formats, BREP and check_printability; export the top-level "
                "container instead if the moved position matters."
            )

    skipped: list[dict[str, str]] = []
    kept = []
    for obj in objects:
        if _has_geometry(obj):
            kept.append(obj)
        elif _mesh_reachable_via(obj):
            skipped.append({
                "name": obj.Name,
                "reason": "links to or contains a mesh; export or link to the underlying Mesh::Feature object directly",
            })
        else:
            skipped.append({"name": obj.Name, "reason": "no exportable geometry"})
    objects = kept

    if ext in MESH_FORMATS:
        exported_names = {obj.Name for obj in objects}
        reported_meshes: set[str] = set()
        for obj in objects:
            if tessellation.is_mesh_feature(obj):
                continue
            for mesh_child in _contained_meshes(obj):
                if mesh_child.Name in exported_names or mesh_child.Name in reported_meshes:
                    continue
                reported_meshes.add(mesh_child.Name)
                skipped.append({
                    "name": mesh_child.Name,
                    "reason": f"a mesh inside container '{obj.Name}'; add it to object_names "
                    "or link to it directly to include it",
                })

    if ext in SHAPE_ONLY_FORMATS:
        shape_only = []
        for obj in objects:
            if tessellation.shape_of(obj) is not None:
                shape_only.append(obj)
            else:
                skipped.append({"name": obj.Name, "reason": f"no shape geometry for .{ext} export"})
        objects = shape_only

    if not objects:
        return fail(
            INVALID_INPUT,
            "No object has exportable geometry to export.",
            "Call " + tool_call("list_objects", {"doc_name": doc_name}) + " to see the document's objects.",
            details={"skipped": skipped},
        )

    # An object that failed to recompute keeps its last good Shape, which
    # would otherwise be exported as if it were current, with no sign of the
    # problem. Check the exported objects and everything they depend on.
    # Without a recompute of our own, a bare Touched only means "not
    # recomputed yet", not broken (object_validation.object_validity_error).
    exclude_touched = not opts["recompute"]
    seen_invalid: set[str] = set()
    for obj in objects:
        candidates = [obj]
        try:
            candidates.extend(obj.OutListRecursive)
        except Exception:
            pass
        for candidate in candidates:
            name = getattr(candidate, "Name", "")
            if not name or name in seen_invalid:
                continue
            error = object_validity_error(candidate, exclude_touched=exclude_touched)
            if error:
                seen_invalid.add(name)
                warnings.append(error)
    if exclude_touched:
        try:
            needs_recompute = bool(doc.mustExecute())
        except Exception:
            needs_recompute = False
        if needs_recompute:
            warnings.append(
                "The document has objects that need a recompute; call "
                + tool_call("recompute_document", {"doc_name": doc_name})
                + " first, or pass recompute true, to export current geometry."
            )

    # Checked immediately before writing (not earlier, on the RPC thread),
    # since the object resolution above can itself take time.
    created_directory, dir_error = ensure_parent_directory(path)
    if dir_error is not None:
        return dir_error
    if os.path.exists(path) and not opts["overwrite"]:
        return fail(
            CONFLICT,
            f"{path} already exists",
            "Call " + tool_call("export_document", {"doc_name": doc_name, "path": path, "overwrite": True})
            + " to replace it, or choose another path.",
        )

    before = _stat_snapshot(path)
    mesh_info: dict[str, Any] | None = None
    try:
        if ext in MESH_FORMATS:
            mesh_info, exported = _export_mesh(objects, path, ext, opts, skipped)
            if mesh_info is None:
                return fail(
                    INVALID_INPUT,
                    "No object has exportable geometry to export.",
                    "Call " + tool_call("list_objects", {"doc_name": doc_name}) + " to see the document's objects.",
                    details={"skipped": skipped},
                )
            objects = exported
        elif ext in STEP_FORMATS:
            _export_step(objects, path, ext, opts)
        elif ext in GLTF_FORMATS:
            mesh_info = _export_gltf(objects, path, opts)
        elif ext in BREP_FORMATS:
            _export_brep(objects, path)
        elif ext == "dxf":
            _export_dxf(objects, path)
        elif ext == "svg":
            _export_svg(objects, path)
        else:
            return fail(INTERNAL_ERROR, f"unhandled export format {ext!r}")
    except Exception as e:
        return fail(FREECAD_ERROR, f"{type(e).__name__}: {e}")

    ok, size = _verify_written(path, before)
    if not ok:
        return fail(FREECAD_ERROR, "The exporter wrote no file")

    companion_file = None
    if ext == "gltf":
        # .gltf (unlike self-contained .glb) writes its buffer as a
        # separate .bin next to it; "bytes" above is the .gltf alone, so
        # the companion is reported on its own rather than silently folded
        # into a total that would no longer match the size of file_name on
        # disk. Reported only through this structured field, not also as a
        # warning: the Go tool renders one body line from it, so the agent
        # sees it once.
        bin_path = os.path.splitext(path)[0] + ".bin"
        try:
            bin_size = os.path.getsize(bin_path)
        except OSError:
            bin_size = 0
        if bin_size > 0:
            companion_file = {"path": os.path.abspath(bin_path), "bytes": serialize_int(bin_size)}

    reply: dict[str, Any] = {
        "success": True,
        "document": doc.Name,
        "file_name": os.path.abspath(path),
        "format": ext,
        "exporter": _EXPORTER_NAMES[ext],
        "bytes": serialize_int(size),
        "objects": [o.Name for o in objects],
        "skipped": skipped,
        "units": _units_for(ext, opts),
        "warnings": warnings,
    }
    if mesh_info is not None:
        reply["mesh"] = mesh_info
    if companion_file is not None:
        reply["companion_file"] = companion_file
    if created_directory:
        reply["created_directory"] = created_directory
    return reply


def export_document(
    doc_name: str,
    path: str,
    options: dict[str, Any] | None = None,
    timeout: Any = None,
) -> dict[str, Any]:
    """Write objects of ``doc_name`` to ``path`` in the format of its extension.

    Reply: ``{"success", "document", "file_name", "format", "exporter",
    "bytes", "objects", "skipped", "units", "warnings", "mesh"?,
    "companion_file"?, "created_directory"?}``. ``created_directory`` is the
    folder this call created because ``path``'s folder was missing.
    ``companion_file`` (``{"path", "bytes"}``) is the
    separate .bin buffer a .gltf export writes next to it; ``bytes`` above
    counts ``file_name`` alone. GUI thread, default timeout
    ``EXPORT_TIMEOUT``. No transaction.
    """
    run_timeout = gui_task.resolve_timeout(timeout, EXPORT_TIMEOUT)
    if isinstance(run_timeout, dict):
        return run_timeout

    opts = _validate_options(options)
    if opts.get("success") is False:
        return opts

    path, path_error = require_absolute_path(path)
    if path_error is not None:
        return path_error
    ext = os.path.splitext(path)[1].lower().lstrip(".")
    if ext not in EXPORT_FORMATS:
        suggested = os.path.splitext(path)[0] + ".stl"
        return fail(
            INVALID_INPUT,
            f"path must end with one of: {', '.join(EXPORT_FORMATS)}, not {ext or '(no extension)'}",
            "Call " + tool_call("export_document", {"doc_name": doc_name, "path": suggested})
            + " with one of those extensions instead.",
        )
    # The exists/overwrite conflict is checked on the GUI thread immediately
    # before writing (_export / _export_fcstd), not here: it must run after
    # object resolution and, for .FCStd, after the own-file check.

    def task() -> dict[str, Any]:
        return _export(doc_name, path, ext, opts)

    return gui_task.run_on_gui(task, run_timeout, "export_document", tool="export_document")

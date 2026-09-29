"""Import a CAD, mesh or 2D file into a document.

Importers are called directly with explicit options, never through
FreeCAD.loadFile or module_io, and never on a path that can open a dialog:
ImportGui.insert always gets mode=0, the DXF importer runs with the legacy
importer and its dialog switched off, and an SVG's DPI question is answered
automatically by a shim instead of ever being asked (see
_insert_svg_without_dialog). ImportGui, Part, Mesh, importDXF and importSVG
are imported inside the GUI task.
"""

import os
from typing import Any

import FreeCAD

from rpc_server import errors, gui_task
from rpc_server.agent_log import agent_warning
from rpc_server.errors import fail, tool_call
from rpc_server.lookup import require_document
from rpc_server.object_validation import invalid_objects_report
from rpc_server.options import check_options
from rpc_server.paths import require_absolute_path
from rpc_server.transactions import active_document, transaction


IMPORT_TIMEOUT = 300.0
TOOL_NAME = "import_file"

# Lower-case extensions without the dot, matching the import format table
# below (one importer per extension).
IMPORT_FORMATS = (
    "step", "stp", "iges", "igs", "gltf", "glb",
    "brep", "brp",
    "stl", "ast", "obj", "off", "ply", "3mf",
    "dxf",
    "svg",
)

# STEP, IGES and glTF go through ImportGui's OCAF importer and accept
# merge/use_link_group/import_hidden.
_OCAF_FORMATS = frozenset({"step", "stp", "iges", "igs", "gltf", "glb"})
_BREP_FORMATS = frozenset({"brep", "brp"})
_MESH_FORMATS = frozenset({"stl", "ast", "obj", "off", "ply", "3mf"})
_DXF_FORMATS = frozenset({"dxf"})
_SVG_FORMATS = frozenset({"svg"})

# Keys accepted in ``options`` (STEP, IGES and glTF only).
IMPORT_OPTIONS = ("merge", "use_link_group", "import_hidden")


def _extension(path: str) -> str:
    return os.path.splitext(path)[1].lower().lstrip(".")


class SvgDialogUnavailable(Exception):
    """importSVG has no ``QtWidgets`` attribute to intercept for
    ``_insert_svg_without_dialog``, so its DPI question cannot be answered
    without risking a real modal dialog on the GUI thread."""


def _insert_svg_without_dialog(path: str, doc_name: str, svg_module: Any) -> bool:
    """Insert an SVG file, answering its DPI question without ever showing it.

    importSVG asks a modal ``QMessageBox`` whenever its SAX handler sees an
    SVG root with no recognised Inkscape marker and no absolute width unit
    (Mod/Draft/importSVG.py:565-599), decided from its own re-serialised
    ``ElementTree`` copy of the file (importSVG.py:1236-1237), not the raw
    text. Rather than reproduce that decision from the raw text, the
    module's own ``QtWidgets`` binding is swapped for a shim for the
    duration of this one call: its ``QMessageBox`` is a real ``QMessageBox``
    subclass (every method except ``exec_`` behaves exactly as it would
    otherwise), and ``exec_`` answers ``Yes`` immediately without opening
    anything, giving 96 dpi, the same value importSVG assumes on its own
    when there is no GUI at all (importSVG.py:601-602). ``QtWidgets`` is
    used nowhere else in the module. The real binding is restored in
    ``finally``. Returns whether the question was actually asked, so the
    caller can warn about the assumption only when it was made.
    """
    if not hasattr(svg_module, "QtWidgets"):
        raise SvgDialogUnavailable(
            "This FreeCAD build's SVG importer (importSVG) has no QtWidgets attribute to "
            "intercept, so its DPI question cannot be answered without risking a real dialog. "
            "SVG import is not available on this FreeCAD build, regardless of the file."
        )
    real_qtwidgets = svg_module.QtWidgets
    asked = {"value": False}

    class _AutoYesMessageBox(real_qtwidgets.QMessageBox):
        def exec_(self):
            asked["value"] = True
            # On PySide's new-style enum (verified live on 6.8.3), Yes is
            # reachable on the class but not on an instance: self.Yes raises
            # AttributeError. The class attribute works on both styles.
            return real_qtwidgets.QMessageBox.Yes

    class _QtWidgetsShim:
        QMessageBox = _AutoYesMessageBox

    svg_module.QtWidgets = _QtWidgetsShim
    try:
        svg_module.insert(path, doc_name)
    finally:
        svg_module.QtWidgets = real_qtwidgets
    return asked["value"]


def _import_dxf(path: str, doc_name: str) -> None:
    """Insert a DXF file with the legacy importer and its dialog switched off.

    Both preferences are restored in ``finally`` regardless of outcome
    (Mod/Draft/importDXF.py: the legacy-library prompt at 156-178, the join
    box at 2392-2407, and the options dialog at 2804-2829 all depend on them).
    """
    import importDXF

    param = FreeCAD.ParamGet("User parameter:BaseApp/Preferences/Mod/Draft")
    old_legacy = param.GetBool("dxfUseLegacyImporter", False)
    old_dialog = param.GetBool("dxfShowDialog", True)
    param.SetBool("dxfUseLegacyImporter", False)
    param.SetBool("dxfShowDialog", False)
    try:
        importDXF.insert(path, doc_name)
    finally:
        param.SetBool("dxfUseLegacyImporter", old_legacy)
        param.SetBool("dxfShowDialog", old_dialog)


def _resolve_document(doc_name: str | None, path: str) -> tuple[Any, bool] | dict[str, Any]:
    """Return ``(document, created_document)``, or a failure reply.

    A given ``doc_name`` must already be open, because every importer here
    silently creates a new document for an unknown name. Without one, a new
    document is created and named after the file's stem.
    """
    if doc_name:
        doc, error = require_document(
            doc_name, hint_suffix="Or omit doc_name to import into a new document."
        )
        if error is not None:
            return error
        return doc, False
    stem = os.path.splitext(os.path.basename(path))[0]
    return FreeCAD.newDocument(stem or "Import"), True


def _run_importer(ext: str, path: str, doc: Any, opts: dict[str, Any]) -> tuple[str, str, list[str]]:
    """Run the importer for ``ext``, returning ``(importer_name, root_object,
    warnings)``.

    Any exception raised here (by the importer itself, or
    ``SvgDialogUnavailable``) is left to propagate: gui_dispatch formats it
    as a string and gui_task.run_on_gui turns that into a freecad_error
    reply. ``import_file`` catches ``SvgDialogUnavailable`` specifically and
    reports it as ``conflict`` instead.
    """
    if ext in _OCAF_FORMATS:
        import ImportGui

        # ImportGui.insert parses importHidden/merge/useLinkGroup as strict
        # bools ("O!" with &PyBool_Type, Mod/Import/Gui/AppImportGuiPy.cpp:
        # 201-218): passing one of them as None (unset) raises TypeError
        # instead of keeping FreeCAD's preference, so only the keywords the
        # caller actually gave are passed at all.
        kwargs = {
            name: value
            for name, value in (
                ("importHidden", opts.get("import_hidden")),
                ("merge", opts.get("merge")),
                ("useLinkGroup", opts.get("use_link_group")),
            )
            if value is not None
        }
        ret = ImportGui.insert(path, doc.Name, mode=0, **kwargs)
        root_name = getattr(ret, "Name", "") if ret is not None else ""
        return "ImportGui.insert", root_name, []

    if ext in _BREP_FORMATS:
        import Part

        shape = Part.read(path)
        stem = os.path.splitext(os.path.basename(path))[0] or "Shape"
        feature = doc.addObject("Part::Feature", stem)
        feature.Shape = shape
        return "Part.read", feature.Name, []

    if ext in _MESH_FORMATS:
        import Mesh

        Mesh.insert(path, doc.Name)
        return "Mesh.insert", "", []

    if ext in _DXF_FORMATS:
        _import_dxf(path, doc.Name)
        return "importDXF.insert", "", []

    if ext in _SVG_FORMATS:
        import importSVG

        dpi_assumed = _insert_svg_without_dialog(path, doc.Name, importSVG)
        warnings = (
            ["The SVG has no absolute units and no recognised Inkscape version marker; "
             "FreeCAD would ask for a DPI, so 96 dpi was assumed automatically."]
            if dpi_assumed else []
        )
        return "importSVG.insert", "", warnings

    # Unreachable: the extension was checked against IMPORT_FORMATS already.
    raise ValueError(f"no importer registered for '.{ext}'")


def import_file(
    path: str,
    doc_name: str | None = None,
    options: dict[str, Any] | None = None,
    timeout: Any = None,
) -> dict[str, Any]:
    """Import ``path`` into ``doc_name``, or into a new document named after it.

    Reply: ``{"success", "document", "created_document", "format",
    "importer", "created_objects", "object_count", "root_object",
    "invalid_objects", "invalid_count", "invalid_truncated", "warnings",
    "transaction", "transaction_merged"}``. GUI thread, default timeout
    ``IMPORT_TIMEOUT``, transaction ``MCP: import_file``.
    """
    budget = gui_task.resolve_timeout(timeout, IMPORT_TIMEOUT)
    if isinstance(budget, dict):
        return budget

    path, path_error = require_absolute_path(path)
    if path_error is not None:
        return path_error
    if not os.path.isfile(path):
        return fail(
            errors.NOT_FOUND,
            f"File not found: {path}",
            "Check the path; it must be an absolute path on the machine running FreeCAD.",
        )

    ext = _extension(path)
    if ext not in IMPORT_FORMATS:
        supported = ", ".join("." + e for e in IMPORT_FORMATS)
        return fail(
            errors.INVALID_INPUT,
            f"Unsupported import format '.{ext}'. Supported extensions: {supported}.",
            f"Call import_file with a file of one of these types ({supported}), "
            "or convert the file to one of them first.",
        )

    opts, opts_error = check_options(options, IMPORT_OPTIONS)
    if opts_error is not None:
        return opts_error
    given = {k: v for k, v in opts.items() if v is not None}
    if given and ext not in _OCAF_FORMATS:
        return fail(
            errors.INVALID_INPUT,
            "merge, use_link_group and import_hidden apply only to STEP, IGES and glTF imports "
            f"(got '.{ext}').",
        )
    for key, value in given.items():
        if not isinstance(value, bool):
            return fail(errors.INVALID_INPUT, f"'{key}' must be a boolean")

    # Whether doc_name names an open document is checked only later, on the
    # GUI thread inside the task (_resolve_document): FreeCAD's document map
    # is not safe to read concurrently with the GUI thread's own document
    # creation and closing.

    def task() -> dict[str, Any]:
        resolved = _resolve_document(doc_name, path)
        if isinstance(resolved, dict):
            return resolved
        doc, created_document = resolved

        failed = False
        try:
            # active_document holds doc active for the transaction's whole
            # life (transactions.active_document docstring): doc_name can
            # name an existing document that is not the one currently active,
            # and without this FreeCAD would open a second, empty linked
            # transaction in whichever document the GUI has focused.
            with active_document(doc), transaction(TOOL_NAME) as tx:
                before = {obj.Name for obj in doc.Objects}
                try:
                    importer_name, root_object, import_warnings = _run_importer(ext, path, doc, opts)
                except SvgDialogUnavailable as exc:
                    failed = True
                    svg_code = f"import importSVG; importSVG.insert({path!r}, {doc.Name!r})"
                    reply = fail(
                        errors.CONFLICT,
                        str(exc),
                        "Convert the file to DXF, STEP or another supported format outside FreeCAD "
                        "and call " + tool_call("import_file", {"path": "<converted file path>"})
                        + " with that file instead. Only if you already know this SVG has absolute "
                        "width/height units (mm, in or cm) or an Inkscape version marker, so FreeCAD "
                        "would not ask, you may run it yourself with "
                        + tool_call("execute_code", {"code": svg_code})
                        + "; without that assurance the same dialog would stall execute_code too.",
                    )
                    reply.update(tx.reply_fields())
                    return reply
                doc.recompute()
                after_names = [obj.Name for obj in doc.Objects if obj.Name not in before]

                if not after_names:
                    failed = True
                    reply = fail(
                        errors.FREECAD_ERROR,
                        "The importer created no objects; the file may be empty or unreadable.",
                    )
                    reply.update(tx.reply_fields())
                    return reply

                created_objects: list[dict[str, str]] = []
                created: list[Any] = []
                for name in after_names:
                    obj = doc.getObject(name)
                    if obj is None:
                        continue
                    created_objects.append({"name": obj.Name, "label": obj.Label, "type": obj.TypeId})
                    created.append(obj)

                if not root_object or root_object not in after_names:
                    root_object = ""
                    for name in after_names:
                        obj = doc.getObject(name)
                        if obj is not None and not obj.InList:
                            root_object = obj.Name
                            break

                warnings: list[str] = list(import_warnings)
                if ext in _MESH_FORMATS:
                    warnings.append(
                        "Mesh objects are triangle meshes; call "
                        + tool_call("mesh_to_solid", {"doc_name": doc.Name, "obj_name": after_names[0]})
                        + " to get a solid."
                    )

                reply = {
                    "success": True,
                    "document": doc.Name,
                    "created_document": created_document,
                    "format": ext,
                    "importer": importer_name,
                    "created_objects": created_objects,
                    "object_count": len(created_objects),
                    "root_object": root_object,
                    **invalid_objects_report(created),
                    "warnings": warnings,
                }
                reply.update(tx.reply_fields())
                return reply
        except Exception:
            failed = True
            raise
        finally:
            # A document created for this import that produced nothing is
            # not left behind: an unattended retry would otherwise pile up
            # "<stem>001", "<stem>002" empty documents. Closing after the
            # transaction and active-document blocks above have already
            # unwound keeps this from interfering with either's own cleanup.
            if failed and created_document:
                try:
                    FreeCAD.closeDocument(doc.Name)
                except Exception as exc:
                    agent_warning(
                        f"MCP RPC: could not close empty import document "
                        f"'{doc.Name}': {type(exc).__name__}: {exc}\n"
                    )

    return gui_task.run_on_gui(task, budget, "import_file", tool=TOOL_NAME)

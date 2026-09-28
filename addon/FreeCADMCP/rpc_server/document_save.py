"""Save, save as and close documents.

Only App-level ``doc.save()``, ``doc.saveAs()``, ``doc.saveCopy()`` and
``FreeCAD.closeDocument()`` are used: the Gui document's save paths open
dialogs (Gui/Document.cpp:1597-1752). After a save the Gui document's Modified
flag is cleared explicitly, since App save never touches it.

Validity comes from object_validation.object_validity_error, the check
create_object, update_object and recompute_document use.
"""

import os
from typing import Any

import FreeCAD
import FreeCADGui

from rpc_server.errors import CONFLICT, INVALID_INPUT, fail, tool_call
from rpc_server.gui_task import resolve_timeout, run_on_gui
from rpc_server.lookup import require_document
from rpc_server.object_validation import invalid_objects_report
from rpc_server.paths import home_example, require_absolute_path


SAVE_TIMEOUT = 120.0


def _same_path(a: str, b: str) -> bool:
    return os.path.normcase(os.path.realpath(a)) == os.path.normcase(os.path.realpath(b))


def _partial_conflict(doc_name: str, doc: Any) -> dict[str, Any] | None:
    """Refuse a partially loaded document: FreeCAD's own save() silently writes nothing for it.

    A document opened as an unresolved dependency of another one carries
    ``Partial`` true; saving or renaming it would report success while the
    file (and, for saveAs, the new name) never receives its data.
    """
    if not bool(getattr(doc, "Partial", False)):
        return None
    try:
        modified = bool(FreeCADGui.getDocument(doc_name).Modified)
    except Exception:
        modified = False
    if modified:
        # open_document reloading it fully closes and reopens the document
        # (App/Application.cpp openDocumentPrivate), discarding these changes
        # with no save able to catch them first; that has to be the agent's
        # explicit choice, so the hint does not lead straight into it.
        return fail(
            CONFLICT,
            f"Document '{doc_name}' is only partially loaded and has unsaved changes; FreeCAD cannot save "
            "a partial document, and loading it fully would discard those changes.",
            "Call "
            + tool_call("close_document", {"doc_name": doc_name, "discard_changes": True})
            + " to discard them, then "
            + tool_call("open_document", {"path": str(doc.FileName)})
            + " to load it fully.",
        )
    return fail(
        CONFLICT,
        f"Document '{doc_name}' is only partially loaded (opened as a dependency of another document); "
        "FreeCAD cannot save it like this.",
        "Call " + tool_call("open_document", {"path": str(doc.FileName)}) + " to load it fully, then save it.",
    )


def save_document(doc_name: str, recompute: bool = True, timeout: Any = None) -> dict[str, Any]:
    """Save a document to its own file.

    Reply: ``{"success", "document", "label", "file_name", "recomputed",
    "invalid_objects", "invalid_count", "invalid_truncated"}``. GUI thread,
    default timeout ``SAVE_TIMEOUT``. No transaction: saving is not undoable.
    """
    run_budget = resolve_timeout(timeout, SAVE_TIMEOUT)
    if isinstance(run_budget, dict):
        return run_budget

    def task() -> dict[str, Any]:
        doc, error = require_document(doc_name)
        if error is not None:
            return error
        partial = _partial_conflict(doc_name, doc)
        if partial is not None:
            return partial
        if not doc.FileName:
            return fail(
                CONFLICT,
                f"Document '{doc_name}' has never been saved, so it has no file yet.",
                "Call "
                + tool_call("save_document_as", {"doc_name": doc_name, "path": home_example(f"{doc_name}.FCStd")})
                + " instead.",
            )

        did_recompute = bool(recompute) and doc.mustExecute()
        if did_recompute:
            doc.recompute()

        doc.save()
        FreeCADGui.getDocument(doc_name).Modified = False

        return {
            "success": True,
            "document": doc_name,
            "label": str(doc.Label),
            "file_name": str(doc.FileName),
            "recomputed": did_recompute,
            # A Touched object left over from before this call (recompute
            # False, or nothing needed recomputing) is not a failure; one
            # still Touched after a recompute this call actually ran is.
            **invalid_objects_report(doc.Objects, exclude_touched=not did_recompute),
        }

    return run_on_gui(task, run_budget, "save_document", tool="save_document")


def save_document_as(
    doc_name: str,
    path: str,
    overwrite: bool = False,
    copy: bool = False,
    recompute: bool = True,
    timeout: Any = None,
) -> dict[str, Any]:
    """Save a document to a new .FCStd file, or write a copy of it.

    Reply: ``{"success", "document", "label", "file_name", "copy",
    "recomputed", "invalid_objects", "invalid_count", "invalid_truncated"}``.
    GUI thread, default timeout ``SAVE_TIMEOUT``. No transaction: saving is
    not undoable.
    """
    run_budget = resolve_timeout(timeout, SAVE_TIMEOUT)
    if isinstance(run_budget, dict):
        return run_budget

    expanded, path_error = require_absolute_path(path)
    if path_error is not None:
        return path_error

    root, ext = os.path.splitext(expanded)
    if ext == "":
        target = expanded + ".FCStd"
    elif ext.casefold() != ".fcstd":
        return fail(
            INVALID_INPUT,
            f"invalid path: {path!r}; save_document_as writes .FCStd files only.",
            "Call "
            + tool_call("save_document_as", {"doc_name": doc_name, "path": root + ".FCStd"})
            + " for a FreeCAD file, or use export_document for STEP, STL, 3MF and other formats.",
        )
    else:
        target = expanded

    def task() -> dict[str, Any]:
        doc, error = require_document(doc_name)
        if error is not None:
            return error
        partial = _partial_conflict(doc_name, doc)
        if partial is not None:
            return partial

        for other_name, other_doc in FreeCAD.listDocuments().items():
            if other_name == doc_name:
                continue
            other_file = str(getattr(other_doc, "FileName", "") or "")
            if other_file and _same_path(other_file, target):
                return fail(
                    CONFLICT,
                    f"'{target}' is already the file of open document '{other_name}'.",
                    "Call " + tool_call("close_document", {"doc_name": other_name}) + " first, or choose another path.",
                )

        own_file = str(doc.FileName or "")
        if copy and own_file and _same_path(own_file, target):
            return fail(
                CONFLICT,
                f"'{target}' is the current file of document '{doc_name}'; FreeCAD refuses to save a copy onto it.",
                "Call "
                + tool_call("save_document", {"doc_name": doc_name})
                + " to save it normally, or choose a different path for the copy.",
            )

        if os.path.exists(target) and not overwrite:
            return fail(
                CONFLICT,
                f"'{target}' already exists.",
                "Call "
                + tool_call("save_document_as", {"doc_name": doc_name, "path": target, "overwrite": True})
                + " to replace it, or choose another path.",
            )

        did_recompute = bool(recompute) and doc.mustExecute()
        if did_recompute:
            doc.recompute()

        if copy:
            # doc.saveCopy() returns None even when it silently declined (its
            # own path); the conflict check above is the only guard against that.
            doc.saveCopy(target)
            file_name = os.path.abspath(target)
        else:
            doc.saveAs(target)
            FreeCADGui.getDocument(doc_name).Modified = False
            file_name = str(doc.FileName)

        return {
            "success": True,
            "document": doc_name,
            "label": str(doc.Label),
            "file_name": file_name,
            "copy": bool(copy),
            "recomputed": did_recompute,
            **invalid_objects_report(doc.Objects, exclude_touched=not did_recompute),
        }

    return run_on_gui(task, run_budget, "save_document_as", tool="save_document_as")


def close_document(doc_name: str, discard_changes: bool = False) -> dict[str, Any]:
    """Close a document, refusing unsaved changes unless discarded.

    Reply: ``{"success", "closed", "discarded_changes", "active_document"}``.
    GUI thread, 60 s. No transaction. FreeCAD.closeDocument never prompts, so
    Modified and isClosable() are checked first.
    """

    def task() -> dict[str, Any]:
        doc, error = require_document(doc_name)
        if error is not None:
            return error
        gdoc = FreeCADGui.getDocument(doc_name)
        modified = bool(getattr(gdoc, "Modified", False))

        if modified and not discard_changes:
            return fail(
                CONFLICT,
                f"Document '{doc_name}' has unsaved changes.",
                "Call "
                + tool_call("save_document", {"doc_name": doc_name})
                + " to keep them, or "
                + tool_call("close_document", {"doc_name": doc_name, "discard_changes": True})
                + " to discard them.",
            )

        if not doc.isClosable():
            return fail(
                CONFLICT,
                f"Document '{doc_name}' cannot be closed right now (FreeCAD reports it is not closable).",
                "Finish or cancel whatever holds it open in FreeCAD (a task panel, or another document depending "
                "on it), then call " + tool_call("close_document", {"doc_name": doc_name}) + " again.",
            )

        FreeCAD.closeDocument(doc_name)
        active = FreeCAD.ActiveDocument

        return {
            "success": True,
            "closed": doc_name,
            "discarded_changes": bool(modified and discard_changes),
            "active_document": str(active.Name) if active is not None else "",
        }

    return run_on_gui(task, 60.0, "close_document")

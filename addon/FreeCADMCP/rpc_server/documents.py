"""List, open and activate documents.

Entry points run on the RPC thread and hand FreeCAD work to
gui_task.run_on_gui. Documents with Temporary set are never listed.
"""

import os
from typing import Any

import FreeCAD
import FreeCADGui

from rpc_server.errors import (
    CONFLICT,
    FREECAD_ERROR,
    INVALID_INPUT,
    NOT_FOUND,
    fail,
    tool_call,
)
from rpc_server.gui_task import resolve_timeout, run_on_gui
from rpc_server.lookup import require_document
from rpc_server.object_validation import invalid_objects_report
from rpc_server.paths import require_absolute_path


OPEN_TIMEOUT = 120.0

# get_documents and activate_document take no timeout argument: both are
# fixed at 60 s.
_FIXED_TIMEOUT = 60.0


def _active_window() -> Any:
    """Return FreeCAD's currently active MDI view (tab), or None."""
    try:
        return FreeCADGui.getMainWindow().getActiveWindow()
    except Exception:
        return None


def _restore_active(previous_doc: Any, previous_view: Any, had_view: bool) -> None:
    """Restore the document and view active before a call that reactivates a
    document as a side effect (FreeCAD.openDocument always makes the newly
    opened document active, App/Application.cpp openDocuments: "Set the
    active document using the first successfully restored main document").

    ``had_view`` is False for a document opened hidden: it gets no MDI view,
    so MainWindow.setActiveWindow(previous_view) does nothing (the active
    window itself never changed; Gui/MainWindow.cpp:1389-1416 returns early
    when "view == d->activeView", before the Application::viewActivated call
    that would otherwise resync it), while FreeCAD.ActiveDocument still
    points at the new, viewless document. The App-level document is restored
    directly instead: App::Application::setActiveDocument(name)
    (App/Application.cpp:1059-1071) sets FreeCAD.ActiveDocument itself and
    emits signalActiveDocument, which also switches the Gui tab when the
    restored document does have a view.

    Best-effort: when the document just opened was itself the previously
    active one and FreeCAD closed and reopened it (a stale partial reload),
    ``previous_view`` no longer exists; restoring is then simply skipped.
    """
    try:
        if had_view and previous_view is not None:
            FreeCADGui.getMainWindow().setActiveWindow(previous_view)
        elif previous_doc is not None:
            FreeCAD.setActiveDocument(previous_doc.Name)
    except Exception:
        pass


def _view_type_name(view: Any) -> str:
    """Return an MDI view's type name without PyCXX's trailing "Py".

    PyCXX names these Python classes with a trailing "Py" (View3DInventorPy,
    SheetViewPy, MDIViewPagePy; Gui/View3DPy.cpp:73 and siblings), which
    ``type(view).__name__`` reports as-is. DocInfo's views and
    activate_document's view_type drop that suffix (View3DInventor,
    SheetView, MDIViewPage).
    """
    name = type(view).__name__
    return name[:-2] if name.endswith("Py") else name


def _document_views(gdoc: Any, active_view: Any) -> list[dict[str, Any]]:
    """Return the ``views`` list of a DocInfo for one Gui document.

    Views are mapped to ``active_view`` by identity: FreeCAD caches one
    Python wrapper per MDIView (Gui/View3DInventor.cpp getPyObject), so the
    same view compares equal whether it came from ``mdiViewsOfType`` or from
    ``getActiveWindow()`` (Gui/MainWindowPy.cpp).
    """
    if gdoc is None:
        return []
    try:
        mdi_views = gdoc.mdiViewsOfType("Gui::MDIView")
    except Exception:
        return []
    return [
        {
            "index": index,
            "type": _view_type_name(view),
            "active": view is active_view,
        }
        for index, view in enumerate(mdi_views)
    ]


def _doc_info(doc: Any, active_name: str, active_view: Any) -> dict[str, Any]:
    """Build one DocInfo row (name, label, file_name, modified, needs_recompute,
    partial, object_count, active, views) for an open, non-temporary document."""
    try:
        gdoc = FreeCADGui.getDocument(doc.Name)
    except Exception:
        gdoc = None
    modified = bool(gdoc.Modified) if gdoc is not None else False
    return {
        "name": doc.Name,
        "label": doc.Label,
        "file_name": doc.FileName or "",
        "modified": modified,
        "needs_recompute": bool(doc.mustExecute()),
        "partial": bool(getattr(doc, "Partial", False)),
        "object_count": len(doc.Objects),
        "active": doc.Name == active_name,
        "views": _document_views(gdoc, active_view),
    }


def get_documents() -> dict[str, Any]:
    """Return every open document with its state and views.

    Reply: ``{"success", "active_document", "count", "documents": [DocInfo]}``.
    GUI thread, 60 s.
    """

    def task() -> dict[str, Any]:
        active_doc = FreeCAD.ActiveDocument
        active_name = active_doc.Name if active_doc is not None else ""
        active_view = _active_window()
        documents = [
            _doc_info(doc, active_name, active_view)
            for doc in FreeCAD.listDocuments().values()
            if not getattr(doc, "Temporary", False)
        ]
        return {
            "success": True,
            "active_document": active_name,
            "count": len(documents),
            "documents": documents,
        }

    return run_on_gui(task, _FIXED_TIMEOUT, "get_documents")


def open_document(
    path: str,
    hidden: bool = False,
    activate: bool = True,
    timeout: Any = None,
) -> dict[str, Any]:
    """Open an .FCStd file, or return the document that already has it open.

    Reply: ``{"success", "document", "label", "file_name", "already_open",
    "object_count", "needs_recompute", "invalid_objects", "invalid_count",
    "invalid_truncated", "active_document"}``. GUI thread, default timeout
    ``OPEN_TIMEOUT``. No transaction.
    """
    resolved_timeout = resolve_timeout(timeout, OPEN_TIMEOUT)
    if isinstance(resolved_timeout, dict):
        return resolved_timeout

    expanded, path_error = require_absolute_path(path)
    if path_error is not None:
        return path_error
    if not os.path.isfile(expanded):
        return fail(
            NOT_FOUND,
            f"'{expanded}' does not exist.",
            "Check the path; FreeCAD opens .FCStd files. Use "
            + tool_call("import_file", {"path": expanded})
            + " for STEP, STL, 3MF and other formats.",
        )
    if os.path.splitext(expanded)[1].lower() != ".fcstd":
        return fail(
            INVALID_INPUT,
            f"'{expanded}' is not a .FCStd file.",
            "For STEP, STL, 3MF and other formats, call " + tool_call("import_file", {"path": expanded}) + " instead.",
        )

    def task() -> dict[str, Any]:
        target = os.path.normcase(os.path.realpath(expanded))
        for name, doc in FreeCAD.listDocuments().items():
            if not doc.FileName or os.path.normcase(os.path.realpath(doc.FileName)) != target:
                continue

            if not getattr(doc, "Partial", False):
                if activate:
                    FreeCADGui.setActiveDocument(name)
                active_doc = FreeCAD.ActiveDocument
                # No recompute runs here, so a bare Touched only means the
                # document has pending changes, not that anything is broken.
                return {
                    "success": True,
                    "document": name,
                    "label": doc.Label,
                    "file_name": doc.FileName,
                    "already_open": True,
                    "object_count": len(doc.Objects),
                    "needs_recompute": bool(doc.mustExecute()),
                    **invalid_objects_report(doc.Objects, exclude_touched=True),
                    "active_document": active_doc.Name if active_doc is not None else "",
                }

            # A partially loaded document (a file another open document links
            # to) is not returned as-is: it is missing objects, and
            # FreeCAD.openDocument below reloads it fully as a main document
            # (App/Application.cpp openDocumentPrivate, PartialDoc +
            # isMainDoc) by closing and reopening it, with no check of its own
            # for unsaved edits. Refuse that silent loss; the agent must
            # discard them explicitly.
            try:
                partial_modified = bool(FreeCADGui.getDocument(name).Modified)
            except Exception:
                partial_modified = False
            if partial_modified:
                return fail(
                    CONFLICT,
                    f"Document '{name}' is only partially loaded and has unsaved changes; FreeCAD cannot "
                    "save a partial document, and loading it fully closes and reopens it, discarding "
                    "those changes.",
                    "Call "
                    + tool_call("close_document", {"doc_name": name, "discard_changes": True})
                    + " to discard them, then "
                    + tool_call("open_document", {"path": expanded})
                    + " to load it fully.",
                )

        previous_active_doc = FreeCAD.ActiveDocument
        previous_active_view = _active_window()

        try:
            doc = FreeCAD.openDocument(expanded, hidden)
        except Exception as exc:
            return fail(FREECAD_ERROR, f"{type(exc).__name__}: {exc}")

        if activate:
            # A document opened hidden has no view, and FreeCADGui's document
            # activation is a no-op without one; that is a known limitation of
            # this combination, not worked around here (Gui/ApplicationPy.cpp
            # sSetActiveDocument, Gui/MainWindow.cpp setActiveWindow).
            FreeCADGui.setActiveDocument(doc.Name)
        else:
            # FreeCAD.openDocument always makes the newly opened document
            # active regardless of this flag (App/Application.cpp
            # openDocuments), so the previous one is restored here. hidden
            # means the new document got no view (DocumentInitFlags.createView
            # = not hidden), which _restore_active must know to restore the
            # App-level active document directly.
            _restore_active(previous_active_doc, previous_active_view, had_view=not hidden)

        active_doc = FreeCAD.ActiveDocument
        # No recompute runs here, so a bare Touched only means the document
        # has pending changes, not that anything is broken.
        return {
            "success": True,
            "document": doc.Name,
            "label": doc.Label,
            "file_name": doc.FileName,
            "already_open": False,
            "object_count": len(doc.Objects),
            "needs_recompute": bool(doc.mustExecute()),
            **invalid_objects_report(doc.Objects, exclude_touched=True),
            "active_document": active_doc.Name if active_doc is not None else "",
        }

    return run_on_gui(task, resolved_timeout, "open_document", "open_document")


def activate_document(
    doc_name: str,
    view_index: int | None = None,
    create_view: bool = False,
) -> dict[str, Any]:
    """Make a document (and one of its views) the active one in FreeCAD.

    Reply: ``{"success", "document", "view_index", "view_type",
    "active_document"}``. GUI thread, 60 s. No transaction.
    """

    def task() -> dict[str, Any]:
        _doc, error = require_document(doc_name)
        if error is not None:
            return error
        try:
            gdoc = FreeCADGui.getDocument(doc_name)
        except Exception:
            gdoc = None
        if gdoc is None:
            return fail(FREECAD_ERROR, f"'{doc_name}' has no FreeCADGui document to activate.")

        try:
            mdi_views = list(gdoc.mdiViewsOfType("Gui::MDIView"))
        except Exception:
            mdi_views = []

        if view_index is not None:
            valid_index = (
                isinstance(view_index, int)
                and not isinstance(view_index, bool)
                and 0 <= view_index < len(mdi_views)
            )
            if not valid_index:
                if mdi_views:
                    hint = (
                        f"Valid view_index values for '{doc_name}' are 0 to {len(mdi_views) - 1}. Call "
                        + tool_call("list_documents", {})
                        + " to see its views."
                    )
                else:
                    hint = (
                        f"'{doc_name}' has no views yet. Call "
                        + tool_call("activate_document", {"doc_name": doc_name, "create_view": True})
                        + " to create one."
                    )
                return fail(NOT_FOUND, f"'{doc_name}' has no view at index {view_index!r}.", hint)
            view = mdi_views[view_index]
        elif mdi_views:
            # No index given: use the document's own last active view.
            active_attr = getattr(gdoc, "ActiveView", None)
            view = active_attr if active_attr is not None else mdi_views[0]
        else:
            if not create_view:
                return fail(
                    CONFLICT,
                    f"'{doc_name}' has no 3D view to activate (it may have been opened hidden).",
                    "Call " + tool_call("activate_document", {"doc_name": doc_name, "create_view": True})
                    + " to create one.",
                )
            view = gdoc.createView("Gui::View3DInventor")
            if view is None:
                return fail(FREECAD_ERROR, f"FreeCAD could not create a 3D view for '{doc_name}'.")
            mdi_views = list(gdoc.mdiViewsOfType("Gui::MDIView"))

        # MainWindow.setActiveWindow both raises the tab and activates its
        # document (Gui/MainWindow.cpp:1389, Application::Instance->viewActivated);
        # never a GUI command.
        mw = FreeCADGui.getMainWindow()
        mw.setActiveWindow(view)

        resolved_index = 0
        for index, candidate in enumerate(mdi_views):
            if candidate is view:
                resolved_index = index
                break

        active_doc = FreeCAD.ActiveDocument
        return {
            "success": True,
            "document": doc_name,
            "view_index": resolved_index,
            "view_type": _view_type_name(view),
            "active_document": active_doc.Name if active_doc is not None else "",
        }

    return run_on_gui(task, _FIXED_TIMEOUT, "activate_document")

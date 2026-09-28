"""close_freecad: close every document and quit FreeCAD.

On the GUI thread (gui_task.run_on_gui): refuse with conflict when a document
has unsaved changes and discard_changes is false, or while a task panel,
command or active transaction is open, or while the macro editor has unsaved
text; otherwise close each document with FreeCAD.closeDocument (App level,
never prompts) and schedule the actual quit a moment later
(QTimer.singleShot) so the reply leaves first. The session lock stays held
until that deferred step, which re-runs every check against whatever is open
by then and always releases it: the caller was already told closing: true
and will not come back, so holding its lock would only block other agents
until it times out. If that re-check finds something new, FreeCAD is left
open instead, with a Report View warning explaining why; only the human at
the FreeCAD computer can have caused that, since another agent's calls are
refused while the lock is still held. No code path may open a dialog.

This never calls MainWindow.closeAllDocuments, Application.tryClose or
Gui::Document.canClose: those are what put up FreeCAD's own save/discard
QMessageBox (Gui/MainWindow.cpp closeAllDocuments 928-1006, confirmSave
850-926; Gui/Document.cpp canClose 2499-2569) and, for the macro editor,
EditorView.canClose (Gui/EditorView.cpp:341-364, "Unsaved document... Save
all changes?"). Every check below is read-only and is done ourselves first so
none of that code ever runs. The final FreeCADGui.getMainWindow().close()
still walks MainWindow.closeEvent (Gui/MainWindow.cpp:1594-1651) into
Application.tryClose (Gui/Application.cpp:1699-1729), which asks every
passive view's canClose() (line 1707-1712); by the time that call is made,
every document is already closed (closeAllDocuments(false) finds nothing
modified) and the deferred re-check has already refused to proceed if
EditorView.canClose() would have prompted, so that walk always accepts
without a dialog.
"""

from typing import Any

import FreeCAD
import FreeCADGui
from PySide import QtCore, QtWidgets

from rpc_server import request_context, session_lock
from rpc_server.errors import CONFLICT, fail, tool_call
from rpc_server.gui_task import run_on_gui


CLOSE_TIMEOUT = 60.0
# How long the deferred quit waits after the reply, for it to leave first.
_QUIT_DELAY_MS = 500

# Every EditorView (the macro/script editor; Gui/EditorView.cpp is the only
# class that subclasses it) ends its window title this way
# (EditorView::setCurrentFileName, Gui/EditorView.cpp:546-554: "<name>[*] -
# Editor" or "untitled[*] - Editor"), and keeps windowModified bound to the
# text document's own modified flag for the life of the view
# (EditorView::EditorView, Gui/EditorView.cpp:143-144: connects
# QTextDocument::modificationChanged to setWindowModified).
_EDITOR_TITLE_SUFFIX = " - Editor"

_BUSY_MESSAGE = "A task panel or command is open in FreeCAD."

# The reply of a close_freecad call that scheduled a deferred quit still
# pending. GUI thread only: task() and _finish_close() both run there, one
# at a time, so a plain module global needs no lock.
_pending_close: dict[str, Any] | None = None


def _non_temporary_documents() -> dict[str, Any]:
    """Open documents, excluding Temporary ones (internal; never listed by
    get_documents either, and left for FreeCAD's own teardown to clear)."""
    return {
        name: doc
        for name, doc in FreeCAD.listDocuments().items()
        if not getattr(doc, "Temporary", False)
    }


def _modified_documents() -> list[tuple[str, str]]:
    """[(name, label), ...] for every open document with unsaved Gui changes.

    A document opened only as another one's dependency (Partial) is never
    asked to save (Gui/MainWindow.cpp:962-964, the same skip
    closeAllDocuments itself applies).
    """
    modified: list[tuple[str, str]] = []
    for name, doc in _non_temporary_documents().items():
        if getattr(doc, "Partial", False):
            continue
        try:
            gdoc = FreeCADGui.getDocument(name)
        except Exception:
            gdoc = None
        if gdoc is not None and bool(getattr(gdoc, "Modified", False)):
            modified.append((name, str(doc.Label)))
    return modified


def _busy_message() -> str | None:
    """Why FreeCAD cannot close right now: a view provider held in edit mode
    (Gui::Document::getInEdit, exposed as DocumentPy.getInEdit(),
    Gui/DocumentPyImp.cpp:164-176) or an application-wide transaction open
    (App::Document::hasPendingTransaction, exposed through
    FreeCAD.getActiveTransaction(), used the same way in transactions.py).
    Either means Document::canClose would ask to finish editing or would run
    into a transaction mid-flight (Gui/Document.cpp canClose, 2499-2569);
    closeDocument never prompts for either, so it has to be refused instead.
    """
    for name in _non_temporary_documents():
        try:
            gdoc = FreeCADGui.getDocument(name)
        except Exception:
            gdoc = None
        if gdoc is None:
            continue
        try:
            in_edit = gdoc.getInEdit()
        except Exception:
            in_edit = None
        if in_edit is not None:
            return _BUSY_MESSAGE
    try:
        active_transaction = FreeCAD.getActiveTransaction()
    except Exception:
        active_transaction = None
    if active_transaction:
        return _BUSY_MESSAGE
    return None


def _editor_widgets() -> list[Any]:
    """Every live macro-editor widget, docked or undocked.

    Docked, it is a QMdiSubWindow's widget like any other MDI view
    (MainWindow::addWindow, Gui/MainWindow.cpp:1246-1288:
    child->setWidget(view)). Undocking it (Std_ViewDockUndockFullscreen,
    MDIView::setCurrentViewMode, Gui/MDIView.cpp:435-448) removes it from the
    QMdiArea and reparents it as a top-level window; FreeCAD finds those the
    same way when re-docking them (MainWindow::switchToDockedMode,
    Gui/MainWindow.cpp:1874-1884: QApplication::topLevelWidgets()), so both
    places are scanned here.
    """
    widgets: list[Any] = []
    try:
        mw = FreeCADGui.getMainWindow()
    except Exception:
        mw = None
    if mw is not None:
        try:
            for subwindow in mw.findChildren(QtWidgets.QMdiSubWindow):
                widget = subwindow.widget()
                if widget is not None:
                    widgets.append(widget)
        except Exception:
            pass
    try:
        app = QtWidgets.QApplication.instance()
        if app is not None:
            widgets.extend(app.topLevelWidgets())
    except Exception:
        pass
    return widgets


def _macro_editor_message() -> str | None:
    """Why the macro editor cannot close right now, or None
    (EditorView::canClose, Gui/EditorView.cpp:341-364)."""
    for widget in _editor_widgets():
        try:
            title = widget.windowTitle()
            modified = bool(widget.isWindowModified())
        except Exception:
            continue
        if not modified or not title.endswith(_EDITOR_TITLE_SUFFIX):
            continue
        # windowTitle() returns the raw "<name>[*] - Editor" string; Qt only
        # substitutes the "[*]" placeholder for display, never in the
        # property itself (QWidget::windowModified docs).
        label = title[: -len(_EDITOR_TITLE_SUFFIX)].replace("[*]", "").strip() or "untitled"
        return f"The FreeCAD macro editor has unsaved changes in '{label}'."
    return None


def _finish_close(session: str | None) -> None:
    """The deferred half of close_freecad, run on the GUI thread
    _QUIT_DELAY_MS after the reply.

    Re-runs every refusal check against whatever is open by now: the lock
    was still held since the reply, so no other session's calls could have
    caused a new conflict in this gap (session_lock.guard refuses them);
    only the human at the FreeCAD computer could have. Either way the lock
    is freed here: the caller was already told closing: true and will not
    come back, so holding its lock would only block other agents until it
    times out. When a new conflict is found, FreeCAD stays open instead of
    closing, with a Report View warning explaining why.

    ``session`` is the calling session's identity (request_context.get(),
    read on the RPC thread before this was scheduled): request_context is
    thread-local and this callback runs on the GUI thread, where it would
    otherwise read empty and release nothing.
    """
    global _pending_close

    reason = None
    modified = _modified_documents()
    if modified:
        labels = ", ".join(label for _name, label in modified)
        reason = f"FreeCAD has unsaved changes in: {labels}."
    if reason is None:
        reason = _busy_message()
    if reason is None:
        reason = _macro_editor_message()

    session_lock.release_for_close(session)

    if reason is not None:
        _pending_close = None  # let a later close_freecad call try again
        FreeCAD.Console.PrintWarning(
            f"MCP RPC: close_freecad kept FreeCAD open: {reason}\n"
        )
        return

    closed = FreeCADGui.getMainWindow().close()
    if not closed:
        # MainWindow.closeEvent refused (Application.tryClose asked a passive
        # view's canClose, and it returned false, most often a Python view
        # whose canClose raised): FreeCAD stays open. The lock is already
        # released above, so nothing else needs undoing; without this,
        # _pending_close would stay set and every later close_freecad call
        # would return the stale cached reply forever.
        _pending_close = None
        FreeCAD.Console.PrintWarning(
            "MCP RPC: FreeCAD did not close: a view refused; close it by hand.\n"
        )


def close_freecad(discard_changes: bool = False) -> dict[str, Any]:
    """Close every document and quit FreeCAD.

    Reply: ``{"success": True, "closing": True, "closed": [names],
    "discarded": [names]}``.
    """
    discard_changes = bool(discard_changes)
    # Read on the RPC thread, before the GUI-thread task below is even
    # queued: request_context is thread-local, so reading it from the GUI
    # thread (where the deferred close later runs) would see nothing. A
    # request without a session header is the session "anonymous", same as
    # the lock itself treats it.
    session = request_context.get().session or session_lock.ANONYMOUS

    def task() -> dict[str, Any]:
        global _pending_close
        if _pending_close is not None:
            # A previous call already closed the documents and scheduled the
            # deferred quit; report the same outcome instead of closing
            # (nothing left to close) or scheduling a second _finish_close.
            return _pending_close

        if not discard_changes:
            modified = _modified_documents()
            if modified:
                first_name = modified[0][0]
                labels = ", ".join(label for _name, label in modified)
                return fail(
                    CONFLICT,
                    f"FreeCAD has unsaved changes in: {labels}.",
                    "Call "
                    + tool_call("save_document", {"doc_name": first_name})
                    + " for each, or "
                    + tool_call("close_freecad", {"discard_changes": True})
                    + " to discard them.",
                    details={"modified": [name for name, _label in modified]},
                )

        busy = _busy_message()
        if busy is not None:
            return fail(
                CONFLICT,
                busy,
                "Finish or cancel it in FreeCAD first, then " + tool_call("close_freecad", {}) + " again.",
            )

        macro = _macro_editor_message()
        if macro is not None:
            return fail(
                CONFLICT,
                macro,
                "Save or close it in FreeCAD first, then " + tool_call("close_freecad", {}) + " again.",
            )

        closed: list[str] = []
        discarded: list[str] = []
        for name in _non_temporary_documents():
            # A document already gone (closed as a side effect of closing a
            # document it was a partial dependency of, earlier in this loop).
            if name not in FreeCAD.listDocuments():
                continue
            try:
                gdoc = FreeCADGui.getDocument(name)
                was_modified = bool(getattr(gdoc, "Modified", False))
            except Exception:
                was_modified = False
            FreeCAD.closeDocument(name)
            closed.append(name)
            if discard_changes and was_modified:
                discarded.append(name)

        # The session lock is released in _finish_close, immediately before
        # the window actually closes, not here: releasing it now would let
        # another session claim FreeCAD and open or edit something in the
        # gap before the quit, walking straight into a save prompt.
        QtCore.QTimer.singleShot(_QUIT_DELAY_MS, lambda: _finish_close(session))

        _pending_close = {
            "success": True,
            "closing": True,
            "closed": closed,
            "discarded": discarded,
        }
        return _pending_close

    return run_on_gui(task, CLOSE_TIMEOUT, "close_freecad")

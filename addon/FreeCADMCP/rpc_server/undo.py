"""Undo and redo through the App document.

Only App ``doc.undo()`` and ``doc.redo()`` are used. Std_Undo and the Gui
document's undo check transaction IDs across documents and can show a modal
message box (Gui/Document.cpp:2866-2926).
"""

from typing import Any

import FreeCAD
import FreeCADGui

from rpc_server.errors import CONFLICT, INVALID_INPUT, fail, tool_call
from rpc_server.gui_task import run_on_gui
from rpc_server.lookup import require_document
from rpc_server.object_validation import invalid_objects_report


MAX_STEPS = 100
_TIMEOUT = 60.0


def _retry_hint(tool: str, doc_name: str, steps: Any) -> str:
    """A copy-pasteable retry of this call, for a conflict reply's hint."""
    return (
        "Finish or cancel the task panel in FreeCAD, then call "
        + tool_call(tool, {"doc_name": doc_name, "steps": steps})
        + " again."
    )


def _edit_mode_conflict(doc_name: str, steps: Any, tool: str) -> dict[str, Any] | None:
    """Return a conflict reply while ``doc_name`` has a task panel open, else None.

    A missing Gui document (a hidden document has none) means there is no task
    panel to conflict with.
    """
    try:
        gdoc = FreeCADGui.getDocument(doc_name)
    except Exception:
        return None
    try:
        in_edit = gdoc.getInEdit() is not None
    except Exception:
        in_edit = False
    if not in_edit:
        return None
    return fail(
        CONFLICT,
        f"Document '{doc_name}' has a task panel open (edit mode).",
        _retry_hint(tool, doc_name, steps),
    )


def _active_transaction_conflict(doc_name: str, steps: Any, tool: str) -> dict[str, Any] | None:
    """Return a conflict reply while a command or task panel holds an open
    application-wide transaction, else None."""
    try:
        active = FreeCAD.getActiveTransaction()
    except Exception:
        active = None
    if not active:
        return None
    name = active[0] if isinstance(active, (list, tuple)) and active else active
    return fail(
        CONFLICT,
        f"FreeCAD has an open transaction ({name!r}); a command or task panel is active.",
        _retry_hint(tool, doc_name, steps),
    )


def _run(doc_name: str, steps: Any, tool: str) -> dict[str, Any]:
    """Shared body of ``undo`` and ``redo``. ``tool`` is ``"undo"`` or ``"redo"``."""
    if isinstance(steps, bool) or not isinstance(steps, int) or not (1 <= steps <= MAX_STEPS):
        return fail(
            INVALID_INPUT,
            f"steps must be an integer from 1 to {MAX_STEPS}, got {steps!r}",
        )

    def task() -> dict[str, Any]:
        doc, error = require_document(doc_name)
        if error is not None:
            return error

        conflict = (
            _edit_mode_conflict(doc_name, steps, tool)
            or _active_transaction_conflict(doc_name, steps, tool)
        )
        if conflict is not None:
            return conflict

        count_attr = "UndoCount" if tool == "undo" else "RedoCount"
        names_attr = "UndoNames" if tool == "undo" else "RedoNames"
        action = doc.undo if tool == "undo" else doc.redo

        done: list[str] = []
        for _ in range(steps):
            if getattr(doc, count_attr) <= 0:
                break
            name = getattr(doc, names_attr)[0]
            action()
            done.append(name)

        did_recompute = bool(doc.mustExecute())
        if did_recompute:
            doc.recompute()

        key = "undone" if tool == "undo" else "redone"
        return {
            "success": True,
            "document": doc.Name,
            key: done,
            "undo_names": list(doc.UndoNames),
            "redo_names": list(doc.RedoNames),
            # A Touched object left over from before this call is not a
            # failure unless this call actually ran a recompute over it.
            **invalid_objects_report(doc.Objects, exclude_touched=not did_recompute),
        }

    # undo and redo take no timeout (fixed at _TIMEOUT), so run_on_gui's `tool`
    # argument is omitted: passing it would add a "call again with a larger
    # timeout" hint to a tool that has no timeout to raise (matches
    # documents.py, spreadsheet.py, measure.py and selection.py, whose fixed-
    # timeout methods likewise call run_on_gui without it).
    return run_on_gui(task, _TIMEOUT, tool)


def undo(doc_name: str, steps: int = 1) -> dict[str, Any]:
    """Undo up to ``steps`` transactions of ``doc_name``.

    Reply: ``{"success", "document", "undone", "undo_names", "redo_names",
    "invalid_objects", "invalid_count", "invalid_truncated"}``. GUI thread,
    60 s. Never opens a transaction.
    """
    return _run(doc_name, steps, "undo")


def redo(doc_name: str, steps: int = 1) -> dict[str, Any]:
    """Redo up to ``steps`` transactions of ``doc_name``.

    Reply: ``{"success", "document", "redone", "undo_names", "redo_names",
    "invalid_objects", "invalid_count", "invalid_truncated"}``. GUI thread,
    60 s. Never opens a transaction.
    """
    return _run(doc_name, steps, "redo")

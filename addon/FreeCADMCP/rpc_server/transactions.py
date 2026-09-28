"""Named, undoable transactions around the document edits of an MCP tool.

FreeCAD records property changes only while a transaction is open. Every
mutating handler therefore runs its edits inside ``transaction(tool)``, which
opens an application-wide transaction named ``MCP: <tool>`` so the user (or
the undo tool) can undo the whole call in one step.

When a transaction is already active, typically because a command or task
panel is open in FreeCAD, nothing is opened or closed: the edits join that
transaction and the reply says so. Opening a new one would commit the user's
pending edit and leave their task panel's Cancel undoing this call instead
(App/AutoTransaction.cpp:132-174).

By default a failure still commits: an object that was created but did not
compute stays in the document, as the tools report it, and undo removes it.
A caller that must instead undo a partly applied change it cannot fix by
restoring property values itself (some FreeCAD property setters have effects
beyond the property they set) calls the returned ``Transaction``'s
``abort()`` before the ``with`` block ends, which discards this call's own
transaction instead of committing it and leaves no undo step; see its
docstring.

Must be used on FreeCAD's GUI thread, inside the dispatched task.
"""

from collections.abc import Iterator
from contextlib import contextmanager
from typing import Any

import FreeCAD


PREFIX = "MCP: "


@contextmanager
def active_document(doc: Any):
    """Make ``doc`` the active document for the duration of the block,
    restoring the previous one (and its GUI tab) afterward.

    Two independent reasons to hold this open around a mutation of ``doc``:

    - Some factories (the Draft shapes, ``BasicShapes.Shapes.addTube``)
      create into ``FreeCAD.ActiveDocument`` rather than a document passed
      in, so without this an RPC call naming one document can drop geometry
      into whichever one the GUI happens to have focused.
    - ``transaction()`` opens its application-wide transaction by name only;
      the *document* it attaches to is decided the moment the target
      document's first property changes, in ``Document::_openTransaction``
      (App/Document.cpp:369-386). If the Application's active document is a
      different one at that moment (not ``doc``), FreeCAD also opens a
      linked ``"-> <name>"`` transaction in that unrelated document
      (App/Document.cpp:379-386), and ``_commitTransaction``
      (App/Document.cpp:478-509) commits it onto that document's undo stack
      even though nothing in it changed. Making ``doc`` active first, for
      the whole time its transaction can be open, avoids polluting an
      unrelated document's undo history with an empty step.

    ``FreeCAD.setActiveDocument`` fires ``signalActiveDocument``, which
    ``Gui::Application::slotActiveDocument`` (Gui/Application.cpp:1265-1289)
    uses to switch the GUI's active tab too (``getMainWindow()->setActiveWindow``),
    so restoring the previous document here also restores the tab the user had
    open. Used the same way by every mutating handler that names one
    document but may run while another is active in the GUI (create_object,
    edit_object, import_file, mesh_to_solid, solid_to_mesh, repair_mesh,
    update_spreadsheet_cells, run_fem_analysis).
    """
    previous = FreeCAD.ActiveDocument
    FreeCAD.setActiveDocument(doc.Name)
    try:
        yield
    finally:
        if previous is not None:
            try:
                FreeCAD.setActiveDocument(previous.Name)
            except Exception:
                pass


class Transaction:
    """What ``transaction`` did: opened its own, joined another, or neither."""

    def __init__(self, name: str) -> None:
        self.name = name
        self.opened = False  # True when this call opened (and would otherwise close) the transaction
        self.merged_into = ""  # name of the transaction that was already active
        self.id = 0
        self.aborted = False  # True once abort() has discarded this call's own transaction

    @property
    def merged(self) -> bool:
        return bool(self.merged_into)

    def abort(self) -> None:
        """Discard this call's own transaction instead of committing it, for
        a caller that must undo a partly applied change and cannot do so by
        restoring property values itself.

        A plain restore ("set every touched property back to what it was")
        is not always enough: some FreeCAD property setters have effects
        beyond the property itself; a spreadsheet alias removed or renamed,
        for one, rewrites every expression in the document that used it
        (Mod/Spreadsheet/App/PropertySheet.cpp:794-803), and re-adding the
        old alias does not reverse that rewrite. Aborting instead
        (``FreeCAD.closeActiveTransaction(True, id)``,
        App/AutoTransaction.cpp:188-225) reverts everything FreeCAD recorded
        since the transaction opened, that rewrite included, and leaves no
        undo step at all: there is nothing left to commit.

        A no-op when this call joined an already-open transaction
        (``merged``): aborting then would discard the user's own pending
        edit, not just this call's. The caller must keep restoring by hand
        for that case. Also a no-op if called twice.
        """
        if not self.opened or self.aborted:
            return
        try:
            FreeCAD.closeActiveTransaction(True, self.id)
        finally:
            self.aborted = True
            self.opened = False

    def reply_fields(self) -> dict[str, Any]:
        """Keys every mutating reply carries.

        ``transaction`` is the name of the transaction holding the changes
        (this call's own, or the one it joined), or "" when FreeCAD accepted
        none, or ``abort()`` discarded it. ``transaction_merged`` is true
        when it joined an open one; always false after ``abort()``, since
        nothing survives for the reply to name.
        """
        if self.aborted:
            return {"transaction": "", "transaction_merged": False}
        if self.opened:
            name = self.name
        else:
            name = self.merged_into
        return {"transaction": name, "transaction_merged": self.merged}


def _active_transaction() -> tuple[str, int] | None:
    """Return (name, id) of the application's active transaction, if any."""
    try:
        current = FreeCAD.getActiveTransaction()
    except Exception:
        return None
    if not current:
        return None
    try:
        name, tid = current
        return str(name), int(tid)
    except (TypeError, ValueError):
        return None


@contextmanager
def transaction(tool: str) -> Iterator[Transaction]:
    """Open ``MCP: <tool>`` for the edits in the ``with`` block.

    ``tool`` is the MCP tool name, for example ``import_file``. The
    transaction is committed on exit, also when the block raises, unless the
    block already called the yielded ``Transaction``'s ``abort()``, which
    discards it instead; exit then does nothing further.
    """
    tx = Transaction(PREFIX + tool)
    current = _active_transaction()
    if current is not None:
        tx.merged_into, tx.id = current
    else:
        tid = int(FreeCAD.setActiveTransaction(tx.name) or 0)
        if tid:
            tx.opened = True
            tx.id = tid
        else:
            # A guard or lock refused a new transaction; report the one that
            # holds the changes, if FreeCAD names one.
            current = _active_transaction()
            if current is not None:
                tx.merged_into, tx.id = current
    try:
        yield tx
    finally:
        if tx.opened:
            try:
                FreeCAD.closeActiveTransaction(False, tx.id)
            except Exception as e:
                FreeCAD.Console.PrintWarning(
                    f"MCP RPC: could not commit transaction '{tx.name}': {type(e).__name__}: {e}\n"
                )

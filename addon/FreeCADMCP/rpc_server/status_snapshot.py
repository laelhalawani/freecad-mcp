"""GUI-independent snapshot of the open documents for get_rpc_status.

A DocumentObserver keeps the list of open documents current as FreeCAD
creates, deletes, relabels, activates and saves them (and sets a newly
opened document's file name once it has one), so get_rpc_status can report
them without waiting for the GUI thread.

get_rpc_status merges this snapshot's fields into its reply without
overriding any existing key:
- ``install()`` runs on the GUI thread from start_rpc_server: registers one
  observer (removing a previous one first) and seeds the snapshot from
  ``FreeCAD.listDocuments()``. Temporary documents are skipped.
- ``remove()`` runs from stop_rpc_server and unregisters the observer.
- ``snapshot()`` runs on the RPC thread, reads under a lock and never touches
  FreeCAD documents. It returns ``freecad_version``, ``pid``,
  ``rpc_started_at``, ``documents`` ([{"name", "label", "file_name",
  "active"}]) and ``active_document``.
FreeCAD is imported inside install() and remove().
"""

import os
import threading
import time
from typing import Any

# Guards every module-level variable below. install()/remove() run on the GUI
# thread (from start_rpc_server/stop_rpc_server); the observer's slot methods
# also run on the GUI thread, as FreeCAD calls them directly; snapshot() runs
# on the RPC thread. The lock makes all three safe together.
_lock = threading.Lock()

_documents: dict[str, dict[str, Any]] = {}
_active_document: str = ""
_observer: Any = None
_freecad_version: str = ""
_rpc_started_at: float = 0.0


def _doc_entry(doc: Any) -> dict[str, Any]:
    return {"name": doc.Name, "label": doc.Label, "file_name": doc.FileName or ""}


class _Observer:
    """FreeCAD document observer. Every slot runs on the GUI thread."""

    def slotCreatedDocument(self, doc: Any) -> None:
        if doc.Temporary:
            return
        with _lock:
            _documents[doc.Name] = _doc_entry(doc)

    def slotDeletedDocument(self, doc: Any) -> None:
        if doc.Temporary:
            return
        global _active_document
        with _lock:
            _documents.pop(doc.Name, None)
            # FreeCAD clears the active document without a signal (only a new
            # active document fires slotActivateDocument), so the deleted
            # document is the only way to notice it stopped being active.
            if _active_document == doc.Name:
                _active_document = ""

    def slotRelabelDocument(self, doc: Any) -> None:
        if doc.Temporary:
            return
        with _lock:
            entry = _documents.get(doc.Name)
            if entry is not None:
                entry["label"] = doc.Label

    def slotActivateDocument(self, doc: Any) -> None:
        if doc.Temporary:
            return
        global _active_document
        with _lock:
            _active_document = doc.Name

    def slotFinishSaveDocument(self, doc: Any, file_name: str) -> None:
        # file_name is the path just written, which is doc.FileName for a
        # real save but the target of doc.saveCopy for a copy: a copy never
        # changes the document's own FileName, so falling back to file_name
        # here would report the copy's path as this document's file even
        # though it may still be unsaved. doc.FileName is the source of
        # truth; slotChangedDocument already keeps it current for a real save.
        if doc.Temporary:
            return
        with _lock:
            entry = _documents.get(doc.Name)
            if entry is not None:
                entry["file_name"] = doc.FileName or ""

    def slotChangedDocument(self, doc: Any, prop: str) -> None:
        # open_document creates the document (slotCreatedDocument, file_name
        # still "") before it sets FileName on it; this is the only signal for
        # that assignment, so without it every document opened after install()
        # keeps file_name "" until it is next saved.
        if prop != "FileName" or doc.Temporary:
            return
        with _lock:
            entry = _documents.get(doc.Name)
            if entry is not None:
                entry["file_name"] = doc.FileName or ""


def install() -> None:
    """Register the document observer and seed the snapshot. GUI thread."""
    import FreeCAD

    global _observer, _freecad_version, _rpc_started_at, _active_document

    remove()

    with _lock:
        _documents.clear()
        for doc in FreeCAD.listDocuments().values():
            if doc.Temporary:
                continue
            _documents[doc.Name] = _doc_entry(doc)
        active = FreeCAD.ActiveDocument
        _active_document = active.Name if active is not None and not active.Temporary else ""
        version = list(FreeCAD.Version())
        _freecad_version = ".".join(version[:3]) if len(version) >= 3 else ""
        _rpc_started_at = time.time()

    obs = _Observer()
    FreeCAD.addDocumentObserver(obs)
    _observer = obs


def remove() -> None:
    """Unregister the document observer. GUI thread."""
    import FreeCAD

    global _observer
    if _observer is not None:
        try:
            FreeCAD.removeDocumentObserver(_observer)
        except Exception:
            pass
        _observer = None


def snapshot() -> dict[str, Any]:
    """Return the document snapshot keys merged into get_rpc_status. RPC thread."""
    with _lock:
        documents = [dict(entry) for entry in _documents.values()]
        active = _active_document
        version = _freecad_version
        started_at = _rpc_started_at
    for entry in documents:
        entry["active"] = entry["name"] == active
    return {
        "freecad_version": version,
        "pid": os.getpid(),
        "rpc_started_at": started_at,
        "documents": documents,
        "active_document": active,
    }

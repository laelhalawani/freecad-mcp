"""Resolve a ``doc_name`` or ``obj_name`` argument, or fail with one message
and one hint per condition, for every handler that takes one."""

from typing import Any

import FreeCAD

from rpc_server.errors import NOT_FOUND, fail, tool_call


def require_document(
    doc_name: str, *, hint_suffix: str = ""
) -> tuple[Any, None] | tuple[None, dict[str, Any]]:
    """Return ``(doc, None)``, or ``(None, fail())`` when ``doc_name`` is not open.

    Must run on the GUI thread: FreeCAD's document map is not safe to read
    concurrently with the GUI thread's own document creation and closing.
    ``hint_suffix``, when given, is appended to the hint (a space between
    them), for a caller with extra advice of its own (import_file's "or omit
    doc_name to import into a new document").
    """
    doc = FreeCAD.listDocuments().get(doc_name)
    if doc is None:
        hint = "Call " + tool_call("list_documents") + " to see the open documents."
        if hint_suffix:
            hint += " " + hint_suffix
        return None, fail(NOT_FOUND, f"Document '{doc_name}' is not open in FreeCAD.", hint)
    return doc, None


def require_object(doc: Any, obj_name: str) -> tuple[Any, None] | tuple[None, dict[str, Any]]:
    """Return ``(obj, None)``, or ``(None, fail())`` when ``obj_name`` is not in ``doc``."""
    obj = doc.getObject(obj_name)
    if obj is None:
        return None, fail(
            NOT_FOUND,
            f"Object '{obj_name}' is not in document '{doc.Name}'.",
            "Call " + tool_call("list_objects", {"doc_name": doc.Name}) + " to see its objects.",
        )
    return obj, None

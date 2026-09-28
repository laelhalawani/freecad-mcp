"""Structured failure replies shared by the RPC handlers.

A failure is ``{"success": False, "code": ..., "error": ...}`` with an optional
``hint`` naming the next MCP tool call and optional ``details``. The MCP
server maps ``code`` to its error codes (reportedCode in reply.go).
"""

import json
from typing import Any


# Failures the addon classifies (TOOLS.md error codes).
NOT_FOUND = "not_found"
INVALID_INPUT = "invalid_input"
CONFLICT = "conflict"
UNAVAILABLE = "unavailable"
INTERNAL_ERROR = "internal_error"
# A failure FreeCAD itself reported: an exception from a FreeCAD API or a
# failed recompute.
FREECAD_ERROR = "freecad_error"

CODES = frozenset(
    {NOT_FOUND, INVALID_INPUT, CONFLICT, UNAVAILABLE, INTERNAL_ERROR, FREECAD_ERROR}
)


def fail(
    code: str,
    error: str,
    hint: str | None = None,
    details: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Return a failure reply.

    ``code`` is one of ``CODES``; any other value is reported as
    ``internal_error`` so the MCP server never receives an unknown code.
    ``hint`` should name a next tool call written with ``tool_call``.
    """
    if code not in CODES:
        code = INTERNAL_ERROR
    reply: dict[str, Any] = {"success": False, "code": code, "error": str(error)}
    if hint:
        reply["hint"] = hint
    if details:
        reply["details"] = details
    return reply


def tool_call(name: str, args: dict[str, Any] | None = None) -> str:
    """Render an MCP tool call for a hint: ``list_objects with {"doc_name": "Box"}``."""
    return f"{name} with {json.dumps(args or {}, ensure_ascii=False)}"

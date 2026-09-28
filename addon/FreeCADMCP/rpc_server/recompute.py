"""Recompute a document and report every object that failed.

Validity comes from object_validation.object_validity_error, the check
create_object and update_object use, with its default: still Touched after
this recompute counts as a failure, since that means the object was skipped
(an upstream dependency failed) rather than genuinely up to date.
"""

from typing import Any

from rpc_server.gui_task import resolve_timeout, run_on_gui
from rpc_server.lookup import require_document
from rpc_server.object_validation import (
    MAX_LISTED_OBJECTS,
    invalid_objects_report,
    object_states,
)


RECOMPUTE_TIMEOUT = 120.0

TOOL_NAME = "recompute_document"


def recompute_document(doc_name: str, timeout: Any = None) -> dict[str, Any]:
    """Recompute ``doc_name`` and list its invalid and touched objects.

    Reply: ``{"success", "document", "recomputed", "object_count",
    "invalid_objects", "invalid_count", "invalid_truncated",
    "touched_objects", "touched_count", "touched_truncated"}``. Both lists
    are capped at MAX_LISTED_OBJECTS rows, with the matching ``*_count``
    giving the true total and ``*_truncated`` set once the cap cuts the
    list short. GUI thread, default timeout ``RECOMPUTE_TIMEOUT``. No
    transaction.
    """
    run_budget = resolve_timeout(timeout, RECOMPUTE_TIMEOUT)
    if isinstance(run_budget, dict):
        return run_budget

    def task() -> dict[str, Any]:
        doc, error = require_document(doc_name)
        if error is not None:
            return error
        recomputed = doc.recompute()
        objects = doc.Objects
        touched_objects: list[str] = []
        touched_count = 0
        for obj in objects:
            states = object_states(obj)
            if any(state.strip().casefold() == "touched" for state in states):
                touched_count += 1
                if len(touched_objects) < MAX_LISTED_OBJECTS:
                    touched_objects.append(str(getattr(obj, "Name", "")))
        return {
            "success": True,
            "document": doc_name,
            "recomputed": int(recomputed),
            "object_count": len(objects),
            **invalid_objects_report(objects),
            "touched_objects": touched_objects,
            "touched_count": touched_count,
            "touched_truncated": touched_count > len(touched_objects),
        }

    return run_on_gui(task, run_budget, "recompute_document", tool=TOOL_NAME)

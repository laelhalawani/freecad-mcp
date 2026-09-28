"""Run budgets and GUI-thread execution for the feature handlers.

Feature entry points run on the RPC thread. They resolve their run budget with
``resolve_timeout`` and hand their FreeCAD work to ``run_on_gui``, which
dispatches it to the GUI thread and turns every dispatch outcome into a reply
dict (see errors.py).
"""

import math
from collections.abc import Callable
from typing import Any

from rpc_server.errors import (
    CODES,
    FREECAD_ERROR,
    INTERNAL_ERROR,
    INVALID_INPUT,
    UNAVAILABLE,
    fail,
    tool_call,
)
from rpc_server.gui_dispatch import dispatch_to_gui


# Ceiling for a caller-supplied timeout, as for execute_code. A GUI task cannot
# be cancelled once started, so an unbounded wait would hide a wedged GUI
# thread from the caller indefinitely.
MAX_TIMEOUT = 1800.0

_STUCK_CODE = "GUI_DISPATCH_STUCK"


def resolve_timeout(value: Any, default: float) -> float | dict[str, Any]:
    """Return the run budget in seconds, or a failure reply for a bad value.

    ``None`` gives ``default``. A finite number greater than 0 (not a bool) is
    capped at ``MAX_TIMEOUT``.
    """
    if value is None:
        return float(default)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return fail(INVALID_INPUT, f"invalid timeout: {value!r}; timeout must be a positive number of seconds")
    seconds = float(value)
    if not math.isfinite(seconds) or seconds <= 0:
        return fail(INVALID_INPUT, f"invalid timeout: {value!r}; timeout must be a positive number of seconds")
    return min(seconds, MAX_TIMEOUT)


def run_on_gui(
    task: Callable[[], dict[str, Any]],
    timeout: float,
    operation: str,
    tool: str | None = None,
) -> dict[str, Any]:
    """Run ``task`` on the GUI thread and return its reply dict.

    ``timeout`` is both the queue and the run budget. ``operation`` names the
    task in dispatch diagnostics (the addon method name). ``tool`` is the MCP
    tool name; when given, a timeout hint says to call it again with a larger
    ``timeout``. ``task`` returns a success dict or a ``fail()`` dict.
    """
    res = dispatch_to_gui(
        task, timeout=timeout, queue_timeout=timeout, operation_name=operation
    )
    if isinstance(res, dict):
        code = res.get("code")
        if res.get("success") is True or code in CODES:
            return res
        status = "Call " + tool_call("get_rpc_status") + (
            " to see which operation holds FreeCAD's GUI thread; it answers even while the thread is busy."
        )
        if code == _STUCK_CODE:
            return fail(
                UNAVAILABLE,
                str(res.get("error", "GUI dispatch is stuck")),
                status + " Restart FreeCAD if it stays stuck.",
                {"dispatch": res.get("dispatch", {})},
            )
        hint = status
        if tool:
            hint += (
                f" For slow work, call {tool} again with a larger timeout "
                f"(this call allowed {timeout:g} seconds, at most {MAX_TIMEOUT:g})."
            )
        return fail(UNAVAILABLE, str(res.get("error", f"{operation} did not finish")), hint)
    if isinstance(res, str):
        # gui_dispatch reports an exception raised by the task as "Type: message".
        return fail(FREECAD_ERROR, res)
    return fail(INTERNAL_ERROR, f"{operation} returned {type(res).__name__}, not a reply")

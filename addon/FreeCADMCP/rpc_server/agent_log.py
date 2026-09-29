"""Console messages about agent work, kept out of the Notification Area.

FreeCAD 1.x shows every console Error and Warning (PrintError, PrintWarning) as
a Notification Area popup in front of the whole window, which blocks clicks
while an agent works. The agent already gets each failure of its own call in
its reply; the person only needs to be able to read it in the Report View.
PrintDeveloperError and PrintDeveloperWarning are written to the Report View
(and the log file) like the others but are never shown as notifications.

Messages the person must act on (a port that is taken, unreadable settings, a
FreeCAD that would not close) keep PrintError and PrintWarning.

FreeCAD's own errors and warnings printed while agent code runs are kept out
of the Notification Area by quiet_notifications for the length of the call.
"""

import threading
from contextlib import contextmanager
from typing import Iterator

# The Console observer behind the Notification Area (FreeCAD.Console.GetObservers).
_NOTIFICATION_OBSERVER = "NotificationAreaObserver"
_QUIET_KINDS = ("Err", "Wrn")

# GUI tasks and async job workers overlap, so the mute is reference counted:
# the first to start turns the kinds off (remembering which it changed) and the
# last to end turns exactly those back on.
_quiet_lock = threading.Lock()
_quiet_users = 0
_quiet_changed: list[str] = []


def agent_error(message: str) -> None:
    """An error of agent work: in the Report View, no popup. FreeCAD before
    1.0 has no developer messages and shows it as an ordinary error."""
    import FreeCAD

    getattr(FreeCAD.Console, "PrintDeveloperError", FreeCAD.Console.PrintError)(message)


def agent_warning(message: str) -> None:
    """A warning about agent work: in the Report View, no popup (see agent_error)."""
    import FreeCAD

    getattr(FreeCAD.Console, "PrintDeveloperWarning", FreeCAD.Console.PrintWarning)(message)


@contextmanager
def quiet_notifications() -> Iterator[None]:
    """While an agent's call or job runs, stop the Notification Area receiving
    FreeCAD's error and warning messages (Console.SetStatus on its observer),
    and restore exactly what was changed once the last overlapping one ends.
    Only this process's console routing changes: the user's notification
    preferences are never written, and the messages still reach the Report
    View and the log file. Nothing happens where FreeCAD has no such observer.
    Safe to use from any thread."""
    import FreeCAD

    global _quiet_users
    with _quiet_lock:
        if _quiet_users == 0:
            try:
                for kind in _QUIET_KINDS:
                    if FreeCAD.Console.GetStatus(_NOTIFICATION_OBSERVER, kind):
                        FreeCAD.Console.SetStatus(_NOTIFICATION_OBSERVER, kind, False)
                        _quiet_changed.append(kind)
            except Exception:
                pass
        _quiet_users += 1
    try:
        yield
    finally:
        with _quiet_lock:
            _quiet_users -= 1
            if _quiet_users == 0:
                for kind in _quiet_changed:
                    try:
                        FreeCAD.Console.SetStatus(_NOTIFICATION_OBSERVER, kind, True)
                    except Exception as exc:
                        FreeCAD.Console.PrintLog(
                            f"MCP: could not restore the Notification Area's {kind} messages: {exc}\n"
                        )
                _quiet_changed.clear()

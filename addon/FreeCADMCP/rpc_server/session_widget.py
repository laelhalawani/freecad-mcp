"""The MCP status widget in FreeCAD's status bar.

A permanent status-bar widget, created on the GUI thread when the RPC server
starts and removed when it stops, or when FreeCAD's main window closes. It
shows whether a password is set and, with remote access on, who holds
FreeCAD, with a Force release button. A 2 s timer refreshes it from
session_lock.snapshot() and runs settings.poll(), so it follows changed
settings without requests.
"""

import FreeCAD
import FreeCADGui
from PySide import QtCore, QtWidgets

from rpc_server import session_lock
from rpc_server.settings import load_settings
from rpc_server.settings import poll as poll_settings

_container: "QtWidgets.QWidget | None" = None
_label: "QtWidgets.QLabel | None" = None
_button: "QtWidgets.QPushButton | None" = None
_timer: "QtCore.QTimer | None" = None

# Connected once for the life of the process: aboutToQuit fires whatever
# workbench is active and whether or not the RPC server is still running.
_quit_connected = False


def _format_idle(seconds: float) -> str:
    """"45 s" under a minute, else "N min"."""
    try:
        whole = max(0, int(seconds))
    except (TypeError, ValueError):
        whole = 0
    if whole < 60:
        return f"{whole} s"
    return f"{whole // 60} min"


def _label_text(snapshot: dict, settings: dict) -> str:
    password_part = "password set" if settings.get("auth_token") else "no password"
    if not snapshot.get("enabled"):
        return f"MCP: {password_part}"
    if not snapshot.get("held"):
        return f"MCP: free | {password_part}"
    holder = snapshot.get("holder") or ""
    if snapshot.get("busy"):
        activity = "working"
    else:
        activity = f"idle {_format_idle(snapshot.get('idle_seconds', 0))}"
    return f"MCP: in use by {holder} ({activity}) | {password_part}"


def _refresh() -> None:
    """Poll the settings file and redraw the widget from the current state."""
    if _label is None:
        return
    try:
        poll_settings()
    except Exception:
        pass
    try:
        settings = load_settings()
    except Exception:
        settings = {}
    try:
        snapshot = session_lock.snapshot()
    except Exception:
        snapshot = {"enabled": False, "held": False}
    _label.setText(_label_text(snapshot, settings))
    if _button is not None:
        _button.setVisible(bool(snapshot.get("held")))


def _force_release() -> None:
    """The widget's Force release button: never a dialog, one Report View line."""
    FreeCADGui.runCommand("Force_Release_Session")
    _refresh()


def _on_about_to_quit() -> None:
    remove()


def install() -> None:
    """Create the widget (idempotent). GUI thread only."""
    global _container, _label, _button, _timer, _quit_connected

    if _container is not None:
        _refresh()
        return

    try:
        status_bar = FreeCADGui.getMainWindow().statusBar()
    except Exception as e:
        FreeCAD.Console.PrintWarning(
            f"MCP: could not create the status widget: {type(e).__name__}: {e}\n"
        )
        return

    container = QtWidgets.QWidget()
    layout = QtWidgets.QHBoxLayout(container)
    layout.setContentsMargins(0, 0, 4, 0)
    layout.setSpacing(6)

    label = QtWidgets.QLabel()
    # The label text includes the holder's client label, which an agent
    # chooses (X-FreeCAD-MCP-Client). QLabel defaults to auto-detecting rich
    # text, so a label containing HTML-like markup could otherwise render as
    # formatting instead of literal text. Force plain text so it never does.
    label.setTextFormat(QtCore.Qt.PlainText)
    layout.addWidget(label)

    button = QtWidgets.QPushButton("Force release")
    button.setVisible(False)
    button.setToolTip(
        "Free FreeCAD from the agent that holds it, so another agent (or "
        "you) can use it."
    )
    button.clicked.connect(_force_release)
    layout.addWidget(button)

    status_bar.addPermanentWidget(container)

    _container = container
    _label = label
    _button = button

    timer = QtCore.QTimer(container)
    timer.setInterval(2000)
    timer.timeout.connect(_refresh)
    timer.start()
    _timer = timer

    if not _quit_connected:
        app = QtWidgets.QApplication.instance()
        if app is not None:
            app.aboutToQuit.connect(_on_about_to_quit)
            _quit_connected = True

    _refresh()


def remove() -> None:
    """Remove the widget and stop its timer (idempotent). GUI thread only."""
    global _container, _label, _button, _timer

    if _timer is not None:
        _timer.stop()
        _timer.deleteLater()
        _timer = None

    if _container is not None:
        try:
            FreeCADGui.getMainWindow().statusBar().removeWidget(_container)
        except Exception:
            pass
        _container.deleteLater()
        _container = None

    _label = None
    _button = None

"""Qt Command classes for the MCP Addon workbench menu.

Defines the six toolbar/menu entries (Start, Stop, Toggle Auto-Start,
Toggle Remote, Configure Allowed IPs, Force Release Session). The password
is set in freecad-mcp's "Share this PC", which writes it to the settings and
stores it for the agents on this computer.

``register_commands()`` is invoked from ``rpc_server.py`` at import time, so
importing it registers the commands.
"""

import json
import threading
import urllib.error
import urllib.request

import FreeCAD
import FreeCADGui
from PySide import QtCore, QtWidgets

from rpc_server.ip_filter import validate_allowed_ips
from rpc_server.settings import load_settings, load_settings_or_raise, save_settings

# The header every freecad-mcp listener response carries, whatever its status.
_LISTENER_MARKER_HEADER = "X-FreeCAD-MCP-Listener"

# System proxy environment variables (HTTP_PROXY etc.) must never apply to
# this loopback probe: urllib's default opener honours them, and a proxy
# configured for the outside world would otherwise receive (or simply
# break) a request meant for 127.0.0.1, making a working listener look like
# it does not answer.
_NO_PROXY_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def _listener_answers(port: int) -> bool:
    """Whether a freecad-mcp listener answers on 127.0.0.1:<port> (2 s).

    Runs the HTTP request; the caller is responsible for keeping this off the
    GUI thread, since urlopen blocks for up to the timeout.
    """
    request = urllib.request.Request(
        f"http://127.0.0.1:{port}/listener/status",
        data=json.dumps({}).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with _NO_PROXY_OPENER.open(request, timeout=2) as response:
            return response.headers.get(_LISTENER_MARKER_HEADER) is not None
    except urllib.error.HTTPError as e:
        return e.headers is not None and e.headers.get(_LISTENER_MARKER_HEADER) is not None
    except Exception:
        return False


class _ListenerProbeBridge(QtCore.QObject):
    """Carries a background probe's result back to the GUI thread.

    A plain callback or ``QTimer.singleShot`` invoked from the worker thread
    would run (or try to run) outside the Qt GUI thread, which is not safe.
    A queued Qt signal connection is: Qt redelivers the emission on the
    thread that owns the receiving object (here, the GUI thread that creates
    this bridge), the same technique ``gui_dispatch._WakeSignal`` uses to let
    the RPC thread wake the GUI thread.
    """

    _done = QtCore.Signal(bool, int)

    def __init__(self, port: int):
        super().__init__()
        self._port = port
        self._done.connect(self._deliver, QtCore.Qt.QueuedConnection)

    def report(self, answered: bool) -> None:
        """Called from the worker thread once the probe finishes."""
        self._done.emit(answered, self._port)

    def _deliver(self, answered: bool, port: int) -> None:
        # Runs on the GUI thread (QueuedConnection).
        try:
            _pending_listener_probes.remove(self)
        except ValueError:
            pass
        if not answered:
            FreeCAD.Console.PrintWarning(
                "Remote access is on, but the freecad-mcp listener "
                f"does not answer on port {port}. Set it up with "
                "`freecad-mcp` > Share this PC.\n"
            )


# Keeps each in-flight bridge referenced between its creation on the GUI
# thread and its queued signal delivery, also on the GUI thread once the
# worker thread finishes; with no other reference a bridge could be
# garbage-collected first, silently dropping the warning.
_pending_listener_probes: "list[_ListenerProbeBridge]" = []


def _probe_listener(port: int) -> None:
    """Check whether the listener answers on ``port`` without blocking the GUI thread."""
    bridge = _ListenerProbeBridge(port)
    _pending_listener_probes.append(bridge)
    threading.Thread(
        target=lambda: bridge.report(_listener_answers(port)), daemon=True
    ).start()


def _report_server_command(message: str, error: bool = False) -> None:
    """Show command feedback even when the Report View is closed."""
    printer = FreeCAD.Console.PrintError if error else FreeCAD.Console.PrintMessage
    printer(message + "\n")
    try:
        FreeCADGui.getMainWindow().statusBar().showMessage(message, 15000)
    except Exception:
        # Console feedback remains available if the main window is closing.
        pass


def _read_settings_or_report() -> dict | None:
    """Read the settings file for a toolbar command that is about to change
    it, or None (having already reported the failure) when the file exists
    but could not be read or parsed: the caller must not save over whatever
    it actually holds with the defaults.
    """
    try:
        return load_settings_or_raise()
    except Exception:
        _report_server_command(
            "FreeCAD MCP settings could not be read; save them again with "
            "freecad-mcp > Share this PC",
            error=True,
        )
        return None


class StartRPCServerCommand:
    def GetResources(self):
        return {"MenuText": "Start RPC Server", "ToolTip": "Start RPC Server"}

    def Activated(self, checked: int = 0) -> None:
        try:
            from . import rpc_server  # late import: avoids circular at module load
            msg = rpc_server.start_rpc_server()
        except Exception as exc:
            _report_server_command(f"RPC Server failed to start: {type(exc).__name__}: {exc}", error=True)
            return
        _report_server_command(msg)

    def IsActive(self):
        return True


class StopRPCServerCommand:
    def GetResources(self):
        return {"MenuText": "Stop RPC Server", "ToolTip": "Stop RPC Server"}

    def Activated(self, checked: int = 0) -> None:
        try:
            from . import rpc_server
            msg = rpc_server.stop_rpc_server()
        except Exception as exc:
            _report_server_command(f"RPC Server failed to stop: {type(exc).__name__}: {exc}", error=True)
            return
        _report_server_command(msg)

    def IsActive(self):
        return True


class _RemoteAccessActionSync(QtCore.QObject):
    """Keeps the "Remote Access" toolbar action's checked state following the
    settings file, so a change made in freecad-mcp (Share this PC, the
    install wizard) shows there too, not only what this action last set
    itself.

    A QObject living on the GUI thread with a queued signal, the same
    pattern as _ListenerProbeBridge above: settings.on_change's callback can
    run on any thread (it is invoked from poll(), which _dispatch calls on
    the RPC thread), so setChecked itself must not be called directly from
    it.
    """

    _apply = QtCore.Signal(bool)

    def __init__(self):
        super().__init__()
        self._apply.connect(self._set_checked, QtCore.Qt.QueuedConnection)

    def _set_checked(self, enabled: bool) -> None:
        try:
            command = FreeCADGui.Command.get("Toggle_Remote_Connections")
            actions = command.getAction() if command else None
        except Exception:
            return
        for action in actions or []:
            try:
                action.setChecked(enabled)
            except Exception:
                pass

    def notify(self, enabled: bool) -> None:
        self._apply.emit(bool(enabled))


_remote_access_action_sync = _RemoteAccessActionSync()


def _sync_remote_access_action(settings: dict) -> None:
    _remote_access_action_sync.notify(settings.get("remote_enabled", False))


def _restore_remote_access_checked(checked: bool) -> None:
    """Set the "Remote Access" action's checked state without retriggering
    Activated: a checkable action's checked state is flipped by Qt before
    Activated runs (the same behavior _sync_remote_access_action above
    corrects for on an external settings change), so Activated's own
    read-failure path calls this to put it back to what it actually was
    before the click, since nothing was saved. blockSignals suppresses the
    toggled signal a plain setChecked would otherwise re-emit, which would
    call Activated again for a change that never really happened.
    """
    try:
        command = FreeCADGui.Command.get("Toggle_Remote_Connections")
        actions = command.getAction() if command else None
    except Exception:
        return
    for action in actions or []:
        try:
            action.blockSignals(True)
            action.setChecked(checked)
        finally:
            try:
                action.blockSignals(False)
            except Exception:
                pass


class ToggleRemoteConnectionsCommand:
    def GetResources(self):
        settings = load_settings()
        return {
            "MenuText": "Remote Access",
            "ToolTip": (
                "Allow other devices through the freecad-mcp listener; set it "
                "up with `freecad-mcp` > Share this PC."
            ),
            "Checkable": bool(settings.get("remote_enabled", False)),
        }

    def Activated(self, checked=0):
        checked = bool(checked)
        settings = _read_settings_or_report()
        if settings is None:
            _restore_remote_access_checked(not checked)
            return
        if checked == bool(settings.get("remote_enabled", False)):
            # The action's own checked state was just set to match the
            # settings file (_sync_remote_access_action, in response to a
            # change made in freecad-mcp), which Qt reports as a toggle of
            # the action and so a click (Action::onToggled invokes the
            # command). The user did not click it: do nothing, rather than
            # re-writing the settings file freecad-mcp just wrote (a lost
            # update if both save around the same time) and re-printing the
            # enabled/disabled message and probe.
            return
        settings["remote_enabled"] = checked
        save_settings(settings)

        if settings["remote_enabled"]:
            allowed_ips = settings.get("allowed_ips", "127.0.0.1")
            FreeCAD.Console.PrintMessage(
                f"Remote access enabled. Allowed IPs: {allowed_ips}\n"
            )
            if not settings.get("auth_token", ""):
                FreeCAD.Console.PrintWarning(
                    "Remote access has no password: anyone on an allowed "
                    "IP address (or on this computer) can run code in "
                    "FreeCAD. Set one in freecad-mcp > Share this PC.\n"
                )
            _probe_listener(settings.get("listener_port", 9876))
        else:
            FreeCAD.Console.PrintMessage("Remote access disabled.\n")

    def IsActive(self):
        return True


class ConfigureAllowedIPsCommand:
    def GetResources(self):
        return {
            "MenuText": "Configure Allowed IPs",
            "ToolTip": "Set which IP addresses or subnets may connect through the freecad-mcp listener (remote access).",
        }

    def Activated(self):
        settings = _read_settings_or_report()
        if settings is None:
            return
        current_ips = settings.get("allowed_ips", "127.0.0.1")
        text, ok = QtWidgets.QInputDialog.getText(
            None,
            "Allowed IP Addresses",
            "Enter allowed IP addresses or subnets (comma-separated):\n"
            "Examples: 127.0.0.1, 192.168.1.0/24, 10.0.0.5",
            QtWidgets.QLineEdit.Normal,
            current_ips,
        )
        if ok and text.strip():
            valid, errors = validate_allowed_ips(text.strip())
            if errors:
                QtWidgets.QMessageBox.warning(
                    None,
                    "Invalid IP Configuration",
                    "The following errors were found:\n\n"
                    + "\n".join(f"• {e}" for e in errors)
                    + ("\n\nOnly valid entries will be saved."
                       if valid else "\n\nNo valid entries found. Settings not changed."),
                )
            if not valid:
                FreeCAD.Console.PrintWarning("Allowed IPs not changed: no valid entries.\n")
                return
            normalised = ", ".join(valid)
            # Re-read: the dialog above was modal and may have stayed open a
            # while, during which freecad-mcp (Share this PC, the install
            # wizard) or the Remote Access toggle could have changed the
            # file. Save allowed_ips on top of that current state, not the
            # copy read before the dialog opened, so this does not clobber
            # whatever else changed meanwhile (a lost update).
            settings = _read_settings_or_report()
            if settings is None:
                return
            settings["allowed_ips"] = normalised
            save_settings(settings)
            FreeCAD.Console.PrintMessage(
                f"Allowed IPs updated to: {normalised}\n"
            )
        else:
            FreeCAD.Console.PrintMessage("Allowed IPs not changed.\n")

    def IsActive(self):
        return True


class ForceReleaseSessionCommand:
    """Free FreeCAD from the agent session that holds it (never a dialog)."""

    def GetResources(self):
        return {
            "MenuText": "Force Release Session",
            "ToolTip": "Free FreeCAD from the agent that holds it, so another agent (or you) can use it.",
        }

    def Activated(self, checked: int = 0) -> None:
        from rpc_server import session_lock

        try:
            label = session_lock.force_release()
        except Exception as exc:
            _report_server_command(
                f"MCP: could not release the session: {type(exc).__name__}: {exc}", error=True
            )
            return
        if label:
            _report_server_command(f"MCP: released the session of {label}.")
        else:
            _report_server_command("MCP: no agent holds FreeCAD.")

    def IsActive(self):
        return True


class ToggleAutoStartCommand:
    def GetResources(self):
        settings = load_settings()
        return {
            "MenuText": "Auto-Start Server",
            "ToolTip": "Automatically start the RPC server when FreeCAD launches.",
            "Checkable": bool(settings.get("auto_start_rpc", False)),
        }

    def Activated(self, checked=0):
        settings = _read_settings_or_report()
        if settings is None:
            return
        settings["auto_start_rpc"] = bool(checked)
        save_settings(settings)

        if settings["auto_start_rpc"]:
            FreeCAD.Console.PrintMessage(
                "MCP RPC server will start automatically on next FreeCAD launch.\n"
            )
        else:
            FreeCAD.Console.PrintMessage(
                "MCP RPC server auto-start disabled.\n"
            )

    def IsActive(self):
        return True


def register_commands() -> None:
    FreeCADGui.addCommand("Start_RPC_Server", StartRPCServerCommand())
    FreeCADGui.addCommand("Stop_RPC_Server", StopRPCServerCommand())
    FreeCADGui.addCommand("Toggle_Auto_Start", ToggleAutoStartCommand())
    FreeCADGui.addCommand("Toggle_Remote_Connections", ToggleRemoteConnectionsCommand())
    FreeCADGui.addCommand("Configure_Allowed_IPs", ConfigureAllowedIPsCommand())
    FreeCADGui.addCommand("Force_Release_Session", ForceReleaseSessionCommand())

    from rpc_server.settings import on_change

    on_change(_sync_remote_access_action)

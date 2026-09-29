import sys as _sys
import os as _os
try:
    _addon_dir = _os.path.dirname(_os.path.abspath(__file__))
except NameError:
    import inspect as _inspect
    _addon_dir = _os.path.dirname(_os.path.abspath(_inspect.getfile(_inspect.currentframe())))
if _addon_dir not in _sys.path:
    _sys.path.insert(0, _addon_dir)


class FreeCADMCPAddonWorkbench(Workbench):
    MenuText = "MCP Addon"
    ToolTip = "Addon for MCP Communication"

    def Initialize(self):
        from rpc_server import rpc_server

        commands = [
            "Start_RPC_Server",
            "Stop_RPC_Server",
            "Toggle_Auto_Start",
            "Toggle_Remote_Connections",
            "Configure_Allowed_IPs",
            "Force_Release_Session",
        ]
        self.appendToolbar("FreeCAD MCP", commands)
        self.appendMenu("FreeCAD MCP", commands)

    def Activated(self):
        pass

    def Deactivated(self):
        pass

    def ContextMenu(self, recipient):
        pass

    def GetClassName(self):
        return "Gui::PythonWorkbench"


Gui.addWorkbench(FreeCADMCPAddonWorkbench())

# Every document that opens without GUI data (a file saved by freecadcmd) is
# restored, however it was opened. Registered here, when the GUI part loads, so
# the file FreeCAD opens from its command line is covered too.
try:
    from rpc_server import headless_files as _headless_files

    _headless_files.install()
except Exception as _headless_error:
    # The log only: no popup for a person who never asked for this.
    FreeCAD.Console.PrintLog(f"[MCP] Could not watch for headless-saved files: {_headless_error}\n")


def _auto_start_mcp():
    port = None
    try:
        # FreeCAD runs this file with exec() inside a function
        # (Gui/FreeCADGuiInit.py, RunInitGuiPy): a plain exec() with no
        # explicit namespace uses the caller's globals and locals, which are
        # different dicts inside a function, so a top-level "import os as _os"
        # lands in that call's locals and is gone by the time this callback
        # runs later from a QTimer. Every name a deferred callback needs (not
        # only "rpc_server", already imported like this) must be imported
        # again in its own body.
        import os

        from rpc_server import rpc_server

        settings = rpc_server.load_settings()
        if not settings.get("auto_start_rpc", False):
            return

        # start_freecad launches FreeCAD with FREECAD_MCP_PORT set to the port
        # its own connection settings expect, and starts the RPC server itself
        # through a startup macro once FreeCAD is up. Without this, auto-start
        # could bind the default port before that macro runs, so the macro
        # would then report "already running" on the wrong port.
        port_env = os.environ.get("FREECAD_MCP_PORT", "").strip()
        if port_env:
            try:
                parsed = int(port_env)
            except ValueError:
                parsed = None
            if parsed is not None and 1 <= parsed <= 65535:
                port = parsed
            else:
                FreeCAD.Console.PrintWarning(
                    f"[MCP] Auto-start: FREECAD_MCP_PORT={port_env!r} is not a valid TCP port; using the default.\n"
                )
        msg = rpc_server.start_rpc_server(port) if port is not None else rpc_server.start_rpc_server()
        FreeCAD.Console.PrintMessage(f"[MCP] Auto-start: {msg}\n")
    except Exception as e:
        FreeCAD.Console.PrintWarning(f"[MCP] Auto-start failed: {e}\n")
        try:
            from rpc_server import commands

            commands.report_port_taken(port if port is not None else 9875, e)
        except Exception:
            pass


from PySide import QtCore

QtCore.QTimer.singleShot(0, _auto_start_mcp)

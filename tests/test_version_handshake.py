"""The addon's half of the version handshake (#145).

The MCP server's half (warnings, budget adoption, the once-per-session notice)
is tested in Go: internal/freecad and internal/mcpserver. The shared protocol
number and the package.xml version are checked by
internal/addoninstall/install_test.go.
"""

import importlib.util
from pathlib import Path
import types
import xmlrpc.client

from test_rpc_concurrency import running_server
from test_rpc_handlers import rpc_module


ROOT = Path(__file__).resolve().parents[1]
ADDON_VERSION_PATH = ROOT / "addon" / "FreeCADMCP" / "rpc_server" / "version.py"


def load_addon_version() -> types.ModuleType:
    spec = importlib.util.spec_from_file_location("_addon_version_test", ADDON_VERSION_PATH)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_real_addon_reports_version_and_budgets(rpc_module: types.ModuleType) -> None:
    rpc = rpc_module.FreeCADRPC()
    rpc.EXECUTE_CODE_TIMEOUT = 42
    status = rpc.get_rpc_status()
    addon = load_addon_version()
    assert status["addon_version"] == addon.__version__
    assert status["protocol_version"] == addon.PROTOCOL_VERSION
    assert status["execute_code_timeout"] == 42
    assert status["max_execute_code_timeout"] == rpc.MAX_EXECUTE_CODE_TIMEOUT


def test_status_travels_over_xmlrpc(rpc_module: types.ModuleType) -> None:
    rpc = rpc_module.FreeCADRPC()
    with running_server(rpc) as (host, port):
        with xmlrpc.client.ServerProxy(f"http://{host}:{port}", allow_none=True) as proxy:
            status = proxy.get_rpc_status()
    assert isinstance(status["protocol_version"], int)
    assert not isinstance(status["protocol_version"], bool)
    assert status["addon_version"] == load_addon_version().__version__

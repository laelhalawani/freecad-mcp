"""Queued execute_code calls over TCP (the addon's queue and run budgets).

The MCP server's socket budgets are tested in Go: internal/freecad.
"""

from concurrent.futures import ThreadPoolExecutor
import threading
import types

from test_rpc_concurrency import client, running_server
from test_rpc_handlers import rpc_module


def test_concurrent_execute_calls_cover_queue_and_run_time(rpc_module: types.ModuleType) -> None:
    """Use a real TCP server and dispatcher with shortened budgets."""
    rpc = rpc_module.FreeCADRPC()
    rpc.EXECUTE_CODE_TIMEOUT = 1
    entered = threading.Event()
    rpc_module.FreeCAD.test_entered = entered
    with running_server(rpc) as (host, port):
        with ThreadPoolExecutor(max_workers=2) as workers:
            first = workers.submit(
                client(host, port, 4).execute_code,
                "import time\nFreeCAD.test_entered.set()\ntime.sleep(0.7)",
            )
            assert entered.wait(2)
            second = workers.submit(client(host, port, 4).execute_code, "import time\ntime.sleep(0.7)")
            assert first.result(timeout=6)["success"] is True
            assert second.result(timeout=6)["success"] is True
        assert client(host, port, 4).get_rpc_status()["gui_dispatch"]["state"] == "healthy"

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
    # Each run is well inside the budget, and the second call's queue wait plus
    # its run is over it. The run stays this far from the budget so a stalled
    # machine does not turn a pass into a timeout.
    budget, hold = 2, 1.2
    rpc.EXECUTE_CODE_TIMEOUT = budget
    entered = threading.Event()
    rpc_module.FreeCAD.test_entered = entered
    with running_server(rpc) as (host, port):
        with ThreadPoolExecutor(max_workers=2) as workers:
            first = workers.submit(
                client(host, port, 30).execute_code,
                f"import time\nFreeCAD.test_entered.set()\ntime.sleep({hold})",
            )
            assert entered.wait(10)
            second = workers.submit(client(host, port, 30).execute_code, f"import time\ntime.sleep({hold})")
            assert first.result(timeout=30)["success"] is True
            assert second.result(timeout=30)["success"] is True
        assert client(host, port, 30).get_rpc_status()["gui_dispatch"]["state"] == "healthy"

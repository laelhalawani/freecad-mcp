"""Reject unusable budgets before scheduling GUI work.

The MCP server rejects them before opening a socket; that side is tested in
Go: internal/freecad and internal/mcpserver.
"""

from concurrent.futures import ThreadPoolExecutor
import threading
import types
from unittest.mock import MagicMock

import pytest

from test_rpc_concurrency import client, running_server
from test_rpc_handlers import rpc_module


INVALID_TIMEOUTS = [
    0, -1, float("nan"), float("inf"), -float("inf"), True, False, "soon",
    pytest.param(10**400, id="overflowing-integer"),
]


@pytest.mark.parametrize("timeout", INVALID_TIMEOUTS)
def test_addon_rejects_invalid_timeout_before_dispatch(
    rpc_module: types.ModuleType, monkeypatch: pytest.MonkeyPatch, timeout: object,
) -> None:
    dispatch = MagicMock()
    monkeypatch.setattr(rpc_module, "dispatch_to_gui", dispatch)
    result = rpc_module.FreeCADRPC().execute_code("must_not_run = True", timeout)
    assert result["success"] is False
    assert "timeout" in result["error"]
    dispatch.assert_not_called()
    assert "must_not_run" not in rpc_module._EXEC_NAMESPACE


def test_custom_timeout_covers_queue_then_execution_over_tcp(rpc_module: types.ModuleType) -> None:
    rpc = rpc_module.FreeCADRPC()
    rpc.EXECUTE_CODE_TIMEOUT = 0.05
    entered = threading.Event()
    rpc_module.FreeCAD.test_entered = entered
    with running_server(rpc) as (host, port):
        with ThreadPoolExecutor(max_workers=2) as workers:
            first = workers.submit(
                client(host, port, 4).execute_code,
                "import time\nFreeCAD.test_entered.set()\ntime.sleep(0.35)\nprint('first')",
                0.6,
            )
            assert entered.wait(2)
            queued = workers.submit(
                client(host, port, 4).execute_code, "import time\ntime.sleep(0.35)\nprint('second')", 0.6,
            )
            first_result, second_result = first.result(timeout=4), queued.result(timeout=4)
        assert first_result["success"] is True
        assert first_result["message"].endswith("first\n")
        assert second_result["success"] is True
        assert second_result["message"].endswith("second\n")
        assert client(host, port, 4).get_rpc_status()["gui_dispatch"]["state"] == "healthy"

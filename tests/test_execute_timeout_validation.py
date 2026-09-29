"""Reject unusable budgets before scheduling GUI work.

The MCP server rejects them before opening a socket; that side is tested in
Go: internal/freecad and internal/mcpserver.
"""

import types
from unittest.mock import MagicMock

import pytest

from test_client_timeouts import check_budgets_reach_the_dispatch_and_outcomes_are_ordered, run_two_queued_calls
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


def test_custom_timeout_replaces_the_default_budget_of_queued_calls_over_tcp(rpc_module: types.ModuleType) -> None:
    rpc = rpc_module.FreeCADRPC()
    rpc.EXECUTE_CODE_TIMEOUT = 0.05  # the default, which the calls' own budget replaces
    budget = 30.0
    outcome = run_two_queued_calls(rpc_module, rpc, budget, call_timeout=budget)
    # The calls' own budget reaches the dispatch, not the default, and the
    # outcomes arrive in order (see run_two_queued_calls for what this cannot show).
    check_budgets_reach_the_dispatch_and_outcomes_are_ordered(outcome, budget)

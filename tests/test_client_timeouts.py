"""Queued execute_code calls over TCP (the addon's queue and run budgets).

The MCP server's socket budgets are tested in Go: internal/freecad.
"""

from concurrent.futures import ThreadPoolExecutor
import threading
import time
import types

from test_gui_dispatch import FakeClock
from test_rpc_concurrency import client, running_server
from test_rpc_handlers import rpc_module


def run_two_queued_calls(
    rpc_module: types.ModuleType, rpc: object, budget: float, call_timeout: float | None = None
) -> types.SimpleNamespace:
    """Run two execute_code calls over TCP, the second queued behind the first:
    the first holds the GUI thread until the second is known to be queued.
    The dispatch's clock is moved by hand (0.6 budgets while the second waits,
    0.6 while it runs), so nothing depends on how fast the machine is. Returns
    the replies, the budgets the dispatch was asked for and each script's
    start and end by that clock.

    This proves what reaches the dispatch and in what order. It cannot catch
    queue wait billed to the run budget, because the dispatch's own timeouts
    are real waits; that guard is test_gui_dispatch.py's
    test_run_budget_counts_from_task_start_not_from_enqueue."""
    clock = FakeClock()
    dispatch = rpc_module.test_dispatch
    dispatch.time = clock

    budgets: list[float] = []
    real = rpc_module.dispatch_to_gui

    def spy(task, timeout=60, operation_name=None):
        budgets.append(timeout)
        return real(task, timeout=timeout, operation_name=operation_name)

    rpc_module.dispatch_to_gui = spy

    log: list[tuple[str, float]] = []
    entered = {"first": threading.Event(), "second": threading.Event()}
    release = {"first": threading.Event(), "second": threading.Event()}
    rpc_module.FreeCAD.test_log = lambda label: log.append((label, clock.monotonic()))
    rpc_module.FreeCAD.test_entered = entered
    rpc_module.FreeCAD.test_release = release

    def script(name: str) -> str:
        return (
            f"FreeCAD.test_log('{name} start')\n"
            f"FreeCAD.test_entered['{name}'].set()\n"
            f"FreeCAD.test_release['{name}'].wait(60)\n"
            f"FreeCAD.test_log('{name} end')"
        )

    extra = () if call_timeout is None else (call_timeout,)
    with running_server(rpc) as (host, port):
        with ThreadPoolExecutor(max_workers=2) as workers:
            first = workers.submit(client(host, port, 120).execute_code, script("first"), *extra)
            assert entered["first"].wait(60)
            second = workers.submit(client(host, port, 120).execute_code, script("second"), *extra)
            # The second call is queued behind the first, which still holds the GUI thread.
            while dispatch._rpc_request_queue.qsize() < 1:
                time.sleep(0.005)
            queued_at = clock.monotonic()
            assert not entered["second"].is_set()
            clock.advance(0.6 * budget)
            release["first"].set()
            first_reply = first.result(timeout=60)
            assert entered["second"].wait(60)
            clock.advance(0.6 * budget)
            release["second"].set()
            second_reply = second.result(timeout=60)
        health = client(host, port, 120).get_rpc_status()["gui_dispatch"]["state"]
    at = dict(log)
    return types.SimpleNamespace(
        replies=[first_reply, second_reply], budgets=budgets, health=health, order=[label for label, _ in log],
        queue_wait=at["second start"] - queued_at, run=at["second end"] - at["second start"],
    )


def check_budgets_reach_the_dispatch_and_outcomes_are_ordered(outcome: types.SimpleNamespace, budget: float) -> None:
    assert [reply["success"] for reply in outcome.replies] == [True, True]
    assert outcome.health == "healthy"
    # The second call ran only after the first one ended.
    assert outcome.order == ["first start", "first end", "second start", "second end"]
    # Each phase, by the dispatch's clock, fits one budget while together they exceed it.
    assert outcome.queue_wait < budget and outcome.run < budget
    assert outcome.queue_wait + outcome.run > budget
    # Both calls reached the dispatch with the budget the addon computed for them.
    assert outcome.budgets == [budget, budget]


def test_queued_execute_calls_reach_the_dispatch_with_their_budget_in_order(rpc_module: types.ModuleType) -> None:
    budget = 30.0
    rpc = rpc_module.FreeCADRPC()
    rpc.EXECUTE_CODE_TIMEOUT = budget
    check_budgets_reach_the_dispatch_and_outcomes_are_ordered(run_two_queued_calls(rpc_module, rpc, budget), budget)

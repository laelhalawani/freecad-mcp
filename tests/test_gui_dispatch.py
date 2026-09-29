from contextlib import contextmanager
import importlib.util
from pathlib import Path
import sys
import threading
import time
import types
from typing import Iterator


ADDON_DIR = Path(__file__).resolve().parents[1] / "addon" / "FreeCADMCP"
GUI_DISPATCH_PATH = ADDON_DIR / "rpc_server" / "gui_dispatch.py"
if str(ADDON_DIR) not in sys.path:
    sys.path.insert(0, str(ADDON_DIR))


class FakeClock:
    """The ``time`` module of a dispatch module under test, with a clock the
    test moves by hand. The dispatch computes its budgets from ``monotonic``,
    so a test can spend a budget (or exceed it) by advancing the clock, while
    the real waits stay generous: nothing depends on how fast the machine is."""

    def __init__(self) -> None:
        self.offset = 0.0

    def monotonic(self) -> float:
        return time.monotonic() + self.offset

    def advance(self, seconds: float) -> None:
        self.offset += seconds

    def __getattr__(self, name: str):
        return getattr(time, name)


class FakeSignal:
    def __init__(self, *_types) -> None:
        # PySide's Signal(*types) declares its argument types at class
        # definition time (for example QtCore.Signal(bool, int)); this fake
        # only needs to accept and ignore them.
        self.callback = None

    def connect(self, callback, *_args) -> None:
        self.callback = callback

    def emit(self, *args) -> None:
        if self.callback is not None:
            self.callback(*args)


class FakeStatusBar:
    def showMessage(self, _message: str) -> None:
        pass

    def clearMessage(self) -> None:
        pass


def reset_transactions_import() -> None:
    """Drop the cached ``rpc_server.transactions`` from ``sys.modules``.

    It imports FreeCAD at module level and stays cached once imported, so
    without this, whichever test loads it first keeps it bound to that
    test's fake FreeCAD for the rest of the run: every loader that installs
    a fake FreeCAD calls this right after, forcing the next import of
    ``rpc_server.transactions`` (directly, or through a module that imports
    it, such as ``rpc_server.object_factory``) to bind to the fake just
    installed. ``"rpc_server.transactions"`` still belongs in the caller's
    own saved/restored module names, so the prior binding comes back when
    the caller's context exits.
    """
    sys.modules.pop("rpc_server.transactions", None)


class FakeApplication:
    @staticmethod
    def mouseButtons() -> int:
        return 0

    @staticmethod
    def activePopupWidget() -> None:
        return None

    @staticmethod
    def activeModalWidget() -> None:
        return None

    @staticmethod
    def instance() -> "FakeApplication":
        return FakeApplication()

    def setOverrideCursor(self, _cursor) -> None:
        pass

    def restoreOverrideCursor(self) -> None:
        pass


@contextmanager
def load_gui_dispatch() -> Iterator[types.ModuleType]:
    module_names = ["FreeCAD", "FreeCADGui", "PySide", "rpc_server.transactions"]
    missing = object()
    saved = {name: sys.modules.get(name, missing) for name in module_names}

    freecad = types.ModuleType("FreeCAD")
    freecad.Console = types.SimpleNamespace(PrintError=lambda _message: None)
    # rpc_server.transactions wraps every mutating handler in a FreeCAD
    # transaction; a real FreeCAD always has these, so the fake needs them for
    # execute_code and commit() to run under test. No transaction is ever
    # active by default, and setActiveTransaction always succeeds (a non-zero
    # id).
    freecad._active_transaction_id = 0

    def _set_active_transaction(_name: str, persist: bool = False) -> int:
        freecad._active_transaction_id += 1
        return freecad._active_transaction_id

    def _close_active_transaction(abort: bool = False, id: int = 0) -> None:
        return None

    freecad.getActiveTransaction = lambda: None
    freecad.setActiveTransaction = _set_active_transaction
    freecad.closeActiveTransaction = _close_active_transaction

    status_bar = FakeStatusBar()
    freecad_gui = types.ModuleType("FreeCADGui")
    freecad_gui.updateGui = lambda: None
    freecad_gui.getMainWindow = lambda: types.SimpleNamespace(
        statusBar=lambda: status_bar
    )

    qt_core = types.SimpleNamespace(
        QObject=object,
        Signal=FakeSignal,
        Qt=types.SimpleNamespace(
            QueuedConnection=0,
            NoButton=0,
            WaitCursor=0,
        ),
        QEventLoop=types.SimpleNamespace(
            ExcludeUserInputEvents=1,
            ExcludeSocketNotifiers=2,
        ),
        QThread=types.SimpleNamespace(msleep=lambda _delay: None),
        QTimer=types.SimpleNamespace(singleShot=lambda _delay, _callback: None),
    )
    qt_widgets = types.SimpleNamespace(QApplication=FakeApplication)
    pyside = types.ModuleType("PySide")
    pyside.QtCore = qt_core
    pyside.QtWidgets = qt_widgets

    sys.modules["FreeCAD"] = freecad
    sys.modules["FreeCADGui"] = freecad_gui
    sys.modules["PySide"] = pyside
    reset_transactions_import()

    module_name = f"_gui_dispatch_test_{time.monotonic_ns()}"
    try:
        spec = importlib.util.spec_from_file_location(module_name, GUI_DISPATCH_PATH)
        if spec is None or spec.loader is None:
            raise ImportError(f"Cannot load gui_dispatch from {GUI_DISPATCH_PATH}")
        module = importlib.util.module_from_spec(spec)
        sys.modules[module_name] = module
        spec.loader.exec_module(module)
        yield module
    finally:
        sys.modules.pop(module_name, None)
        for name, value in saved.items():
            if value is missing:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = value


class ThreadedWaker:
    def __init__(self, gui_dispatch: types.ModuleType):
        self.gui_dispatch = gui_dispatch
        self.threads: list[threading.Thread] = []

    def wake(self) -> None:
        thread = threading.Thread(
            target=lambda: self.gui_dispatch.process_gui_tasks(reschedule=False),
            daemon=True,
        )
        thread.start()
        self.threads.append(thread)

    def join(self) -> None:
        for thread in self.threads:
            thread.join(timeout=1.0)


def test_running_timeout_blocks_followups_until_task_finishes() -> None:
    with load_gui_dispatch() as gui_dispatch:
        waker = ThreadedWaker(gui_dispatch)
        gui_dispatch._waker = waker
        started = threading.Event()
        release = threading.Event()

        def blocked_task() -> bool:
            started.set()
            release.wait(timeout=30.0)
            return True

        # A short run budget over a task that never returns in time; the task
        # has all the time it needs to start.
        first = gui_dispatch.dispatch_to_gui(
            blocked_task,
            timeout=0.05,
            queue_timeout=30.0,
            operation_name="remove_broken_feature",
        )
        assert started.wait(timeout=30.0)
        assert first["code"] == "GUI_DISPATCH_STUCK"

        # Rejected at once: far under the budget it would otherwise wait for.
        before = time.monotonic()
        second = gui_dispatch.dispatch_to_gui(
            lambda: True,
            timeout=30.0,
            operation_name="list_documents",
        )
        elapsed = time.monotonic() - before

        assert second["code"] == "GUI_DISPATCH_STUCK"
        assert elapsed < 10.0
        assert gui_dispatch.get_dispatch_status()["state"] == "stuck"

        release.set()
        waker.join()
        assert gui_dispatch.get_dispatch_status()["state"] == "healthy"

        third = gui_dispatch.dispatch_to_gui(
            lambda: "recovered",
            timeout=30.0,
            operation_name="recovered_call",
        )
        waker.join()

        assert third == "recovered"


def test_queued_timeout_cancels_task_without_marking_dispatch_stuck() -> None:
    with load_gui_dispatch() as gui_dispatch:
        ran = threading.Event()

        result = gui_dispatch.dispatch_to_gui(
            lambda: ran.set(),
            timeout=0.01,
            operation_name="stale_call",
        )
        gui_dispatch.process_gui_tasks(reschedule=False)

        assert result["success"] is False
        assert "code" not in result
        assert "waiting for 'stale_call' to start" in result["error"]
        assert not ran.is_set()
        assert gui_dispatch.get_dispatch_status()["state"] == "healthy"


def test_run_budget_counts_from_task_start_not_from_enqueue() -> None:
    """A call queued behind a slow task must not lose its budget while waiting."""
    with load_gui_dispatch() as gui_dispatch:
        waker = ThreadedWaker(gui_dispatch)
        gui_dispatch._waker = waker
        first_started = threading.Event()
        release_first = threading.Event()
        results: dict[str, object] = {}

        def slow_task() -> str:
            first_started.set()
            release_first.wait(timeout=30.0)
            return "first"

        def run_first() -> None:
            results["first"] = gui_dispatch.dispatch_to_gui(
                slow_task, timeout=30.0, operation_name="slow_boolean"
            )

        first_thread = threading.Thread(target=run_first, daemon=True)
        first_thread.start()
        assert first_started.wait(timeout=10.0)

        # Second call: its own run takes ~0, but it waits in the queue for
        # longer than its whole run budget (by the dispatch's clock). It must
        # still succeed because the wait is not billed.
        clock = FakeClock()
        gui_dispatch.time = clock
        budget = 10.0

        def run_second() -> None:
            results["second"] = gui_dispatch.dispatch_to_gui(
                lambda: "second",
                timeout=budget,
                queue_timeout=1000.0,
                operation_name="quick_query",
            )

        second_thread = threading.Thread(target=run_second, daemon=True)
        second_thread.start()
        while gui_dispatch._rpc_request_queue.qsize() < 1:  # queued behind the slow task
            time.sleep(0.005)
        clock.advance(budget * 1.5)
        assert "second" not in results  # still queued behind the slow task
        release_first.set()
        first_thread.join(timeout=30.0)
        second_thread.join(timeout=30.0)
        waker.join()

        assert results["first"] == "first"
        assert results["second"] == "second"
        assert gui_dispatch.get_dispatch_status()["state"] == "healthy"


def test_already_queued_call_survives_stuck_predecessor() -> None:
    """Fail-fast applies to new calls only; a queued call runs once the wedge clears."""
    with load_gui_dispatch() as gui_dispatch:
        waker = ThreadedWaker(gui_dispatch)
        clock = FakeClock()
        gui_dispatch.time = clock
        first_started = threading.Event()
        release_first = threading.Event()
        results: dict[str, object] = {}
        budget = 30.0

        def stuck_task() -> bool:
            first_started.set()
            release_first.wait(timeout=30.0)
            return True

        def run_second() -> None:
            results["second"] = gui_dispatch.dispatch_to_gui(
                lambda: "queued", timeout=budget, queue_timeout=1000.0,
                operation_name="queued_behind_wedge",
            )

        second_thread = threading.Thread(target=run_second, daemon=True)
        woke = []

        def wake() -> None:
            waker.wake()
            if woke:
                return
            # The first call's wake: once its task holds the GUI thread and the
            # second call is queued behind it, the first call's run budget is
            # spent (by the dispatch's clock), so it goes stuck with the second
            # already waiting, however slowly the threads are scheduled.
            woke.append(True)
            assert first_started.wait(timeout=30.0)
            second_thread.start()
            while gui_dispatch._rpc_request_queue.qsize() < 1:
                time.sleep(0.005)
            clock.advance(budget)

        gui_dispatch._waker = types.SimpleNamespace(wake=wake)

        def run_first() -> None:
            results["first"] = gui_dispatch.dispatch_to_gui(
                stuck_task, timeout=budget, queue_timeout=1000.0, operation_name="wedged_op"
            )

        first_thread = threading.Thread(target=run_first, daemon=True)
        first_thread.start()
        first_thread.join(timeout=30.0)
        assert results["first"]["code"] == "GUI_DISPATCH_STUCK"
        assert gui_dispatch.get_dispatch_status()["state"] == "stuck"
        # A brand-new call is rejected immediately while the wedge persists.
        assert gui_dispatch.dispatch_to_gui(lambda: True, timeout=30.0)["code"] == "GUI_DISPATCH_STUCK"

        release_first.set()
        second_thread.join(timeout=30.0)
        waker.join()
        assert results["second"] == "queued"
        assert gui_dispatch.get_dispatch_status()["state"] == "healthy"


def test_run_deadline_does_not_restart_when_rpc_thread_resumes_late() -> None:
    with load_gui_dispatch() as dispatch:
        waker = ThreadedWaker(dispatch)
        entered, release = threading.Event(), threading.Event()
        clock = FakeClock()
        dispatch.time = clock
        budget = 30.0

        def delayed_wake() -> None:
            waker.wake()
            assert entered.wait(10)
            clock.advance(budget)  # The RPC thread resumes a whole budget later.

        def task() -> bool:
            entered.set()
            release.wait(30)
            return True

        dispatch._waker = types.SimpleNamespace(wake=delayed_wake)
        before = time.monotonic()
        try:
            result = dispatch.dispatch_to_gui(task, timeout=budget, queue_timeout=1000)
            assert result["code"] == "GUI_DISPATCH_STUCK"
            # The run deadline was spent while the RPC thread was away, and is
            # not reset to another budget when it resumes: a reset would wait a
            # real budget more (30 s), this returns at once.
            assert time.monotonic() - before < budget / 2
        finally:
            release.set()
            waker.join()


def test_queue_deadline_includes_time_spent_waking_gui() -> None:
    with load_gui_dispatch() as dispatch:
        clock = FakeClock()
        dispatch.time = clock
        queue_timeout = 30.0
        # Waking the GUI takes a whole queue budget.
        dispatch._waker = types.SimpleNamespace(wake=lambda: clock.advance(queue_timeout))
        ran = threading.Event()
        before = time.monotonic()
        result = dispatch.dispatch_to_gui(ran.set, timeout=1000, queue_timeout=queue_timeout)
        assert result["success"] is False
        # The wake counts against the queue deadline: waiting another
        # queue_timeout after it would take 30 real seconds more, this returns
        # at once.
        assert time.monotonic() - before < queue_timeout / 2
        dispatch.process_gui_tasks(reschedule=False)
        assert not ran.is_set()

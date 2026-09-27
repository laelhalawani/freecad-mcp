"""GUI-thread task dispatch for the RPC server.

The XML-RPC server runs in its own thread. FreeCAD APIs that touch the GUI
or the document tree must run in the main GUI thread. This module owns the
queue that ferries wrapped callables onto the GUI thread and the helper that
RPC handlers use to invoke them.

Robustness and performance guarantees:

1. Per-call response queues: each ``dispatch_to_gui`` call owns its own
   ``queue.Queue``. A timeout in one call can never corrupt the response for
   a subsequent call.
2. Immediate wake via Qt signal: ``dispatch_to_gui`` emits a signal from the
   RPC thread; the GUI thread processes the task immediately rather than
   waiting for the next 500 ms heartbeat tick. The 500 ms heartbeat is kept
   only as a fallback.
3. Mouse-button guard: a dispatch tick is skipped while
   mouse buttons are held so MCP tasks cannot interrupt 3D navigation drags,
   for at most ``MOUSE_DEFER_MAX_S`` so a lost mouse-release cannot wedge
   dispatch. A queued call that times out names the guard that held it.
4. One heartbeat chain: every 500 ms tick reschedules itself, including a
   tick that fires re-entrantly while a task pumps events, so guard-deferred
   work is always retried. Each start claims a new chain id and only the
   live chain reschedules, so a restart never leaves two chains running;
   ``request_shutdown`` ends the live chain and any start tick still pending.
5. Exception isolation: exceptions inside a task (including ``SystemExit``
   and ``KeyboardInterrupt``) are caught, logged, and returned as error
   strings; they never kill the dispatch loop.
6. Stuck-task fail-fast: once a task that already started times out, later GUI
   calls fail immediately until that task returns. Status remains available
   through a GUI-independent RPC method.
7. Timeout counts from task start: the GUI thread runs queued tasks one at a
   time, so a call that arrives while another task is running waits in the
   FIFO first. That wait is budgeted separately (``queue_timeout``) and does
   not consume the task's own ``timeout``. Two concurrent ``execute_code``
   calls therefore each get their full run budget instead of the second one
   being reported stuck because the first was slow.
"""

import itertools
import queue
import threading
import time
import traceback
from typing import Any, Callable

import FreeCAD
import FreeCADGui
from PySide import QtCore, QtWidgets

from rpc_server.dispatch_health import DispatchHealth, stuck_failure


_rpc_request_queue: "queue.Queue[Any]" = queue.Queue()
_processing = False  # re-entrancy guard: True while _drain_gui_tasks runs tasks
_processing_since: float = 0.0  # wall-clock time when _processing became True
_task_ids = itertools.count(1)
_dispatch_health = DispatchHealth()

# Qt can miss a mouse-release when the button is released outside the FreeCAD
# window (drag out of the viewport, release there). QApplication.mouseButtons()
# then reports the button as held indefinitely. An unbounded mouse guard turns
# that into a permanently wedged RPC server: every tick defers, no task ever
# runs, and every call fails with an unexplained timeout. Cap how long we are
# willing to believe a drag is still in progress.
MOUSE_DEFER_MAX_S = 5.0
# None = unarmed. Do not use 0.0 as the sentinel: time.monotonic() may legitimately
# return 0.0, which would re-arm the timer every tick and never reach the cap.
_mouse_defer_since: "float | None" = None
_stale_mouse_warned = False

# Orders guard deferrals against enqueues. A clock cannot: time.monotonic()
# advances only every ~16 ms on Windows, so a guard seen just before a call
# could share its timestamp and be blamed for holding it.
_event_seq = itertools.count()

# Why and when (in _event_seq order) the last tick declined to process,
# recorded on the GUI thread so the RPC thread can report it on timeout
# without touching Qt off-thread. One tuple assignment keeps reason and
# sequence consistent without a lock.
_last_defer: "tuple[str, int] | None" = None

# The 500 ms heartbeat is a chain of single-shot timers. Only the chain whose
# id matches _heartbeat_chain may reschedule, so a start claims a new id and
# any older chain ends at its next tick. _heartbeat_stopped keeps a start tick
# that was scheduled before request_shutdown from reviving the chain;
# init_waker clears it when the server starts again.
_heartbeat_lock = threading.Lock()
_heartbeat_chain = 0
_heartbeat_stopped = False


def _mouse_guard_should_defer(mouse_down: bool, now: float) -> bool:
    """Whether to skip this tick because mouse buttons are held.

    Defers while a genuine 3D-navigation drag is plausibly in progress, but
    gives up after ``MOUSE_DEFER_MAX_S``: buttons reported held that long
    across an MCP call are far more likely stale Qt state than a real drag,
    and deferring forever is strictly worse than interrupting a drag.
    """
    global _mouse_defer_since, _stale_mouse_warned
    if not mouse_down:
        _mouse_defer_since = None
        _stale_mouse_warned = False
        return False
    if _mouse_defer_since is None:
        _mouse_defer_since = now
        return True
    return (now - _mouse_defer_since) < MOUSE_DEFER_MAX_S


def _warn_stale_mouse_once() -> None:
    """Announce the override once per stuck-button episode."""
    global _stale_mouse_warned
    if _stale_mouse_warned:
        return
    _stale_mouse_warned = True
    FreeCAD.Console.PrintWarning(
        f"MCP RPC: mouse buttons reported held for over {MOUSE_DEFER_MAX_S:.0f}s. "
        "Treating as stale Qt input state and processing queued tasks anyway.\n"
    )


class _WakeSignal(QtCore.QObject):
    """Qt signal bridge for cross-thread GUI-task wakeup.

    Must be created on the GUI thread (``init_waker``). Emitting from the
    RPC thread is safe: Qt delivers the connection with ``QueuedConnection``,
    so the slot always fires in the GUI thread's event loop.
    """
    _sig = QtCore.Signal()

    def __init__(self):
        super().__init__()
        self._sig.connect(self._on_wake, QtCore.Qt.QueuedConnection)

    def wake(self) -> None:
        self._sig.emit()

    def _on_wake(self) -> None:
        process_gui_tasks(reschedule=False)


_waker: "_WakeSignal | None" = None


def init_waker() -> None:
    """Create the wake-signal bridge and re-arm the heartbeat.

    Call once from the GUI thread when the server starts, before scheduling
    the first ``process_gui_tasks`` tick.
    """
    global _waker, _heartbeat_stopped
    with _heartbeat_lock:
        _heartbeat_stopped = False
    _waker = _WakeSignal()


def cleanup_waker() -> None:
    """Release the wake-signal bridge on server stop."""
    global _waker
    _waker = None


def _flush_gui_events(delay_ms: int = 20) -> None:
    FreeCADGui.updateGui()
    app = QtWidgets.QApplication.instance()
    if app is None:
        return

    # ExcludeUserInputEvents: skip mouse/keyboard events to avoid re-entrancy
    # with ongoing navigation. ExcludeSocketNotifiers keeps network I/O out.
    flags = (
        QtCore.QEventLoop.ExcludeUserInputEvents
        | QtCore.QEventLoop.ExcludeSocketNotifiers
    )
    app.processEvents(flags, delay_ms)
    if delay_ms > 0:
        QtCore.QThread.msleep(delay_ms)
        app.processEvents(flags, delay_ms)


def process_gui_tasks(reschedule: bool = True) -> None:
    """Drain queued GUI-thread callables and optionally start the heartbeat.

    ``reschedule=True`` (the server's start tick) claims a new 500 ms
    heartbeat chain, which supersedes any older chain, then drains. It claims
    nothing after ``request_shutdown`` until ``init_waker`` re-arms it.

    ``reschedule=False`` is used by the immediate-wake path so it does not
    start a second heartbeat chain alongside the existing 500 ms one.
    """
    chain = _claim_heartbeat_chain() if reschedule else None
    try:
        _drain_gui_tasks()
    finally:
        if chain is not None:
            _schedule_heartbeat(chain)


def _claim_heartbeat_chain() -> "int | None":
    global _heartbeat_chain
    with _heartbeat_lock:
        if _heartbeat_stopped:
            return None
        _heartbeat_chain += 1
        return _heartbeat_chain


def _schedule_heartbeat(chain: int) -> None:
    """Queue the next tick of ``chain`` unless a restart or shutdown ended it."""
    with _heartbeat_lock:
        if chain != _heartbeat_chain:
            return
    QtCore.QTimer.singleShot(500, lambda: _heartbeat_tick(chain))


def _heartbeat_tick(chain: int) -> None:
    if chain != _heartbeat_chain:
        return  # superseded by a restart or ended by request_shutdown
    try:
        _drain_gui_tasks()
    finally:
        # Also after a re-entrant tick that skipped draining: the chain must
        # survive so ticks deferred by a guard are retried.
        _schedule_heartbeat(chain)


def _drain_gui_tasks() -> None:
    """Run queued GUI-thread callables unless a guard defers this tick.

    Skips the current tick when any mouse button is held (e.g., 3D navigation
    drag), a popup or modal dialog is open, or a task is already executing
    (re-entrancy guard). The guard prevents ``doc.recompute()`` or
    ``processEvents()`` inside a task from triggering a nested drain that
    corrupts FreeCAD state.
    """
    global _processing, _processing_since, _last_defer
    if _processing:
        return  # re-entrant call from processEvents inside a task; skip

    try:
        if _rpc_request_queue.empty():
            return  # nothing queued; skip cursor/status-bar churn on idle heartbeat ticks

        now = time.monotonic()
        mouse_down = QtWidgets.QApplication.mouseButtons() != QtCore.Qt.NoButton
        if _mouse_guard_should_defer(mouse_down, now):
            _last_defer = ("mouse buttons held (3D navigation drag)", next(_event_seq))
            return  # user is dragging; defer to next tick
        if mouse_down:
            _warn_stale_mouse_once()  # stale state: fall through and process anyway
        if QtWidgets.QApplication.activePopupWidget() is not None:
            _last_defer = ("a popup or context menu is open in FreeCAD", next(_event_seq))
            return  # context menu or popup open; defer to next tick
        if QtWidgets.QApplication.activeModalWidget() is not None:
            _last_defer = ("a modal dialog is open in FreeCAD", next(_event_seq))
            return  # modal dialog open; defer to next tick

        _last_defer = None
        _processing = True
        _processing_since = time.monotonic()
        app = QtWidgets.QApplication.instance()
        try:
            status_bar = FreeCADGui.getMainWindow().statusBar()
        except Exception:
            status_bar = None

        if app is not None:
            app.setOverrideCursor(QtCore.Qt.WaitCursor)
        if status_bar is not None:
            status_bar.showMessage("MCP: processing…")
        try:
            while not _rpc_request_queue.empty():
                task = _rpc_request_queue.get()
                try:
                    task()
                except BaseException as e:  # SystemExit/KeyboardInterrupt must not kill the drain
                    FreeCAD.Console.PrintError(
                        f"MCP RPC: unhandled exception in GUI task: {type(e).__name__}: {e}\n"
                        f"{traceback.format_exc()}"
                    )
        finally:
            if app is not None:
                app.restoreOverrideCursor()
            if status_bar is not None:
                status_bar.clearMessage()
    finally:
        _processing = False


def request_shutdown() -> None:
    """End the heartbeat chain, including a start tick that has not fired yet."""
    global _heartbeat_chain, _heartbeat_stopped
    with _heartbeat_lock:
        _heartbeat_stopped = True
        _heartbeat_chain += 1


def get_dispatch_status() -> dict[str, Any]:
    """Return GUI dispatch health without touching FreeCAD's GUI thread."""
    return _dispatch_health.snapshot()


def dispatch_to_gui(
    task: Callable[[], Any],
    timeout: float = 60,
    operation_name: str | None = None,
    queue_timeout: float | None = None,
) -> Any:
    """Run ``task`` on the GUI thread and return its result.

    Uses a per-call response queue so a timeout in one call never corrupts
    the response for a subsequent call. Wakes the GUI thread immediately via
    a Qt signal instead of waiting for the next 500 ms heartbeat.

    ``timeout`` is the run budget and starts counting when the task actually
    begins on the GUI thread. Time spent queued behind earlier tasks is
    budgeted separately by ``queue_timeout`` (defaults to ``timeout``); if the
    task has not started by then it is cancelled without marking dispatch
    stuck. A call that is already queued when an earlier task becomes stuck
    keeps waiting for its turn; only calls arriving after that point are
    rejected immediately.

    A task already running on the GUI thread cannot be interrupted; if it
    exceeds ``timeout`` it is marked as stuck and subsequent GUI calls fail
    immediately until it returns.

    Returns the task's return value on success, an error string if the task
    raises, or ``{"success": False, "error": ...}`` on timeout.
    """
    rejection = _dispatch_health.rejection()
    if rejection is not None:
        return rejection

    if queue_timeout is None:
        queue_timeout = timeout

    task_id = next(_task_ids)
    operation = operation_name or getattr(task, "__name__", "GUI operation")
    if operation == "<lambda>":
        operation = "GUI operation"

    response_queue: "queue.Queue[Any]" = queue.Queue(maxsize=1)
    state_lock = threading.Lock()
    started_event = threading.Event()
    started_at: float | None = None
    cancelled = False

    def _wrapped() -> None:
        nonlocal started_at
        with state_lock:
            if cancelled:
                return  # caller timed out and went away; don't run a stale task
            started_at = time.monotonic()
            _dispatch_health.start(task_id, operation)
            started_event.set()
        missing = object()
        res = missing
        try:
            try:
                res = task()
            except BaseException as e:  # sys.exit() in user code is a task error too
                FreeCAD.Console.PrintError(
                    f"MCP RPC: GUI task raised {type(e).__name__}: {e}\n"
                    f"{traceback.format_exc()}"
                )
                res = f"{type(e).__name__}: {e}"
        finally:
            # Publish completion atomically with clearing health, so a deadline
            # racing with completion cannot report a missing successful result.
            with state_lock:
                _dispatch_health.finish(task_id)
                if res is not missing:
                    response_queue.put_nowait(res)

    queued_at = time.monotonic()
    queued_seq = next(_event_seq)
    _rpc_request_queue.put(_wrapped)
    waker = _waker  # read once: cleanup_waker() may clear it concurrently
    if waker is not None:
        waker.wake()  # immediate wake via Qt signal (thread-safe)

    # Phase 1: wait for the task to start. Earlier queued tasks run first on
    # the GUI thread; that wait must not eat into this task's run budget.
    queue_deadline = queued_at + queue_timeout
    if not started_event.wait(max(0, queue_deadline - time.monotonic())):
        with state_lock:
            cancelled = started_at is None
    if cancelled:
        queued_for = time.monotonic() - queued_at
        last_defer = _last_defer
        if _processing:
            busy_for = time.monotonic() - _processing_since
            hint = (
                f" (GUI thread has been busy for {busy_for:.1f}s; for heavy OCCT"
                " geometry consider execute_code_async, which must apply document"
                " writes through its commit() helper)"
            )
        elif last_defer is not None and last_defer[1] > queued_seq:
            # Never silently time out on a guard: name it so the next wedge
            # diagnoses itself instead of looking like a dead server. A guard
            # that held only an earlier task says nothing about this one.
            hint = (
                f" (GUI thread is not processing tasks: {last_defer[0]}; "
                f"{_rpc_request_queue.qsize()} task(s) queued)"
            )
        else:
            hint = ""
        return {
            "success": False,
            "error": (
                f"GUI dispatch gave up after {queued_for:.1f}s waiting for "
                f"'{operation}' to start (queue_timeout={queue_timeout:g}s){hint}"
            ),
        }

    # Phase 2: count from actual GUI start, even if this RPC thread woke late.
    assert started_at is not None
    run_remaining = max(0, started_at + timeout - time.monotonic())
    try:
        return response_queue.get(timeout=run_remaining)
    except queue.Empty:
        with state_lock:
            # Completion may have won the race while we acquired the lock.
            try:
                return response_queue.get_nowait()
            except queue.Empty:
                stuck = _dispatch_health.mark_timed_out(task_id, timeout)
                if stuck is not None:
                    return stuck_failure(stuck, just_timed_out=True)
                return {"success": False, "error": f"GUI dispatch timed out after {timeout}s"}

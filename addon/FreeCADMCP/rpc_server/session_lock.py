"""The session lock: with remote access on, one agent session holds FreeCAD.

- Enabled only while remote_enabled is on (configure). Off means no checks,
  no refusals, and configure(False, ...) clears every holder, eviction and
  in-flight count, so turning it on again starts free.
- guard(method) wraps every RPC call in FreeCADRPC._dispatch. ping,
  get_rpc_status, release_session and get_async_status are exempt: they
  never claim and are never refused (get_async_status reads a job's own
  result, so the natural async-job-then-release-then-poll flow neither
  keeps the lock nor loses the result once release_session has freed it).
  Any other method claims a free or expired lock, refreshes
  the caller's own, or raises Fault(FAULT_IN_USE, "SESSION_IN_USE " + JSON)
  while another session holds it. A call without a session header is the
  session "anonymous".
- Idle time counts from the holder's last call start or end (time.monotonic);
  a holder with a call or an async job in flight (per-session counts) is
  never idle. The lock expires when idle reaches the timeout.
- force_release (the person at the FreeCAD computer) frees the lock and
  records the evicted session, whose next non-exempt call fails once with
  Fault(FAULT_RELEASED, "SESSION_RELEASED " + JSON). A call of an evicted
  session that ends afterwards only decrements its own in-flight count.
- The state is module-level and thread-safe; it outlives Stop/Start of the
  RPC server and ends with FreeCAD.
"""

import json
import math
import threading
import time
from collections.abc import Iterator
from contextlib import contextmanager
from typing import Any
from xmlrpc.client import Fault

from rpc_server import request_context


EXEMPT = frozenset({"ping", "get_rpc_status", "release_session", "get_async_status"})

FAULT_IN_USE = 4230
FAULT_RELEASED = 4231

ANONYMOUS = "anonymous"

DEFAULT_TIMEOUT_S = 30 * 60

_MIN_TIMEOUT_S = 60.0
_MAX_TIMEOUT_S = 86400.0

# All state below is guarded by this one lock, independent of
# rpc_server_instance: it survives Stop/Start of the RPC server and is only
# cleared by configure(False, ...) or the FreeCAD process ending.
_state_lock = threading.Lock()

_enabled = False
_timeout_s = float(DEFAULT_TIMEOUT_S)
_holder: str | None = None
_holder_label: str | None = None
_last_activity = 0.0
# Bumped by configure(False, ...), so a call that claimed the lock in an
# earlier generation can tell its own bookkeeping was already cleared and
# must not touch the fresh state of a later generation.
_generation = 0

# Per-session in-flight call/async-job counts, kept only for sessions with a
# non-zero count: a call of an evicted session must still decrement only its
# own count after a new session has claimed the lock.
_in_flight: dict[str, int] = {}
# Sessions force-released, each with the monotonic time of the eviction, kept
# until their next non-exempt call consumes the Fault(FAULT_RELEASED, ...).
_evicted: dict[str, float] = {}
# job_id -> the session that started it, for job_finished (called from the
# worker thread, which has no request_context of its own).
_job_sessions: dict[str, str] = {}
# The holder session, when release() was called while it still had work in
# flight (an async job, or a GUI dispatch task that outlived its timeout):
# the lock is not freed yet (that work still writes to FreeCAD through it),
# but job_finished frees it once that in-flight count reaches zero.
_release_pending: str | None = None


def _clamp_timeout(timeout_s: float) -> float:
    try:
        value = float(timeout_s)
    except (TypeError, ValueError):
        return float(DEFAULT_TIMEOUT_S)
    if not math.isfinite(value):
        return float(DEFAULT_TIMEOUT_S)
    return min(max(value, _MIN_TIMEOUT_S), _MAX_TIMEOUT_S)


def _touch(now: float) -> None:
    """Free an idle lock (lazy expiry). Caller holds _state_lock."""
    global _holder, _holder_label
    if _holder is not None and _in_flight.get(_holder, 0) <= 0 and (now - _last_activity) >= _timeout_s:
        _in_flight.pop(_holder, None)
        _holder = None
        _holder_label = None


def _view(now: float) -> dict[str, Any]:
    """The free/held state, not tied to a caller. Caller holds _state_lock, after _touch(now)."""
    timeout_i = int(_timeout_s)
    if _holder is None:
        return {
            "held": False,
            "holder": "",
            "busy": False,
            "idle_seconds": 0,
            "frees_in_seconds": 0,
            "timeout_seconds": timeout_i,
        }
    busy = _in_flight.get(_holder, 0) > 0
    if busy:
        idle_seconds = 0
        frees_in_seconds = timeout_i
    else:
        idle_seconds = max(0, int(now - _last_activity))
        frees_in_seconds = max(0, timeout_i - idle_seconds)
    return {
        "held": True,
        "holder": _holder_label or _holder,
        "busy": busy,
        "idle_seconds": idle_seconds,
        "frees_in_seconds": frees_in_seconds,
        "timeout_seconds": timeout_i,
    }


def configure(enabled: bool, timeout_s: float) -> None:
    """Turn the lock on or off and set the idle timeout (clamped to 1 to 1440 minutes)."""
    global _enabled, _timeout_s, _holder, _holder_label, _last_activity, _generation, _release_pending
    clamped = _clamp_timeout(timeout_s)
    with _state_lock:
        _enabled = bool(enabled)
        _timeout_s = clamped
        if not _enabled:
            # Turning remote off clears every holder, eviction and
            # in-flight/job count, so turning it on again starts free. The
            # new generation tells a call that claimed the lock earlier
            # (guard's finally, below) that its bookkeeping is gone.
            _holder = None
            _holder_label = None
            _last_activity = 0.0
            _in_flight.clear()
            _evicted.clear()
            _job_sessions.clear()
            _release_pending = None
            _generation += 1


@contextmanager
def guard(method: str) -> Iterator[None]:
    """Claim or check the lock for the calling session around one RPC call."""
    if method in EXEMPT:
        yield
        return

    global _holder, _holder_label, _last_activity, _release_pending
    claimed_session: str | None = None
    claimed_generation: int | None = None
    with _state_lock:
        if _enabled:
            now = time.monotonic()
            _touch(now)
            ctx = request_context.get()
            session = ctx.session or ANONYMOUS
            evicted_at = _evicted.pop(session, None)
            if evicted_at is not None:
                body = {"released_seconds_ago": max(0, int(now - evicted_at))}
                raise Fault(FAULT_RELEASED, "SESSION_RELEASED " + json.dumps(body, sort_keys=True))
            if _holder is None or _holder == session:
                _holder = session
                # Never the session id itself as a fallback: for an HTTP
                # client that is the random per-process/per-connection id,
                # which other agents should not see.
                _holder_label = ctx.client or "unnamed agent"
                _last_activity = now
                _in_flight[session] = _in_flight.get(session, 0) + 1
                claimed_session = session
                claimed_generation = _generation
                # A new call from a session release() had marked pending
                # withdraws that release: the agent has resumed work, the
                # same as the no-job path where release then call again
                # simply re-claims.
                if _release_pending == session:
                    _release_pending = None
            else:
                view = _view(now)
                body = {
                    "busy": view["busy"],
                    "frees_in_seconds": view["frees_in_seconds"],
                    "holder": view["holder"],
                    "idle_seconds": view["idle_seconds"],
                    "timeout_seconds": view["timeout_seconds"],
                }
                # The refusal touches no counter.
                raise Fault(FAULT_IN_USE, "SESSION_IN_USE " + json.dumps(body, sort_keys=True))
    try:
        yield
    finally:
        if claimed_session is not None:
            with _state_lock:
                if _generation == claimed_generation:
                    remaining = _in_flight.get(claimed_session, 0) - 1
                    if remaining <= 0:
                        _in_flight.pop(claimed_session, None)
                    else:
                        _in_flight[claimed_session] = remaining
                    # A call of a session evicted meanwhile only decrements
                    # its own count and never refreshes or re-claims.
                    if _holder == claimed_session:
                        _last_activity = time.monotonic()
                        # release_session marked this session pending while
                        # this call (or another job) was still in flight:
                        # this was the last of it, so free it now, the same
                        # as job_finished does.
                        if remaining <= 0 and _release_pending == claimed_session:
                            _holder = None
                            _holder_label = None
                            _release_pending = None
                # else: configure(False) ran since this call claimed the
                # lock; that bookkeeping is gone, nothing to undo.


def status() -> dict[str, Any]:
    """The session key of get_rpc_status, for the calling session.

    ``{"enabled": False}`` while the lock is off; else ``{"enabled", "held",
    "holder", "yours", "busy", "idle_seconds", "frees_in_seconds",
    "timeout_seconds"}``.
    """
    with _state_lock:
        if not _enabled:
            return {"enabled": False}
        now = time.monotonic()
        _touch(now)
        ctx = request_context.get()
        session = ctx.session or ANONYMOUS
        view = _view(now)
        return {
            "enabled": True,
            "held": view["held"],
            "holder": view["holder"],
            "yours": _holder == session,
            "busy": view["busy"],
            "idle_seconds": view["idle_seconds"],
            "frees_in_seconds": view["frees_in_seconds"],
            "timeout_seconds": view["timeout_seconds"],
        }


def release() -> dict[str, Any]:
    """release_session: free the lock if the calling session holds it.

    While the caller still has work in flight (an async job, or a GUI
    dispatch task that outlived its own timeout and is still actually
    running), the lock is not freed here: that work still writes to FreeCAD
    through it. Instead the session is marked (``pending``) and job_finished
    frees it once that in-flight count reaches zero; releaseOnExit (Go)
    calls this same method, so a server exiting while its own job runs
    marks it the same way rather than leaving the lock claimed by no one.
    """
    global _holder, _holder_label, _release_pending
    with _state_lock:
        if not _enabled:
            return {"success": True, "released": False, "enabled": False}
        now = time.monotonic()
        _touch(now)
        ctx = request_context.get()
        session = ctx.session or ANONYMOUS
        if _holder is None:
            return {"success": True, "released": False, "enabled": True}
        if _holder == session:
            if _in_flight.get(session, 0) > 0:
                _release_pending = session
                return {"success": True, "released": False, "enabled": True, "pending": True}
            _holder = None
            _holder_label = None
            if _release_pending == session:
                _release_pending = None
            return {"success": True, "released": True, "enabled": True}
        return {
            "success": True,
            "released": False,
            "enabled": True,
            "holder": _holder_label or _holder,
        }


def release_for_close(session: str | None = None) -> None:
    """Free the lock of the session that called close_freecad, once the documents closed.

    ``session`` is the session close_freecad captured on the RPC thread,
    before the GUI-thread work that closes the documents; pass it explicitly
    because that work no longer runs on the request thread, so the current
    thread's request_context is not necessarily still its caller's. Omitting
    it (None) falls back to the calling thread's own request_context.
    """
    global _holder, _holder_label, _release_pending
    with _state_lock:
        if not _enabled:
            return
        if session is None:
            ctx = request_context.get()
            session = ctx.session or ANONYMOUS
        if _holder == session:
            _holder = None
            _holder_label = None
            # A pending mark from before close_freecad started must not
            # later free a session that has since reclaimed the lock.
            if _release_pending == session:
                _release_pending = None


def force_release() -> str | None:
    """Free the lock for the person at the FreeCAD computer; return the evicted holder's label."""
    global _holder, _holder_label, _release_pending
    with _state_lock:
        if not _enabled or _holder is None:
            return None
        now = time.monotonic()
        _touch(now)
        if _holder is None:
            return None
        label = _holder_label or _holder
        _evicted[_holder] = now
        if _release_pending == _holder:
            _release_pending = None
        _holder = None
        _holder_label = None
        return label


def snapshot() -> dict[str, Any]:
    """The lock state for the status widget, not tied to a caller.

    ``{"enabled", "held", "holder", "busy", "idle_seconds",
    "frees_in_seconds", "timeout_seconds"}``.
    """
    with _state_lock:
        if not _enabled:
            return {"enabled": False, "held": False}
        now = time.monotonic()
        _touch(now)
        return {"enabled": True, **_view(now)}


def job_started(job_id: str) -> None:
    """An async job of the calling session started: it counts as in flight until job_finished."""
    with _state_lock:
        if not _enabled:
            return
        ctx = request_context.get()
        session = ctx.session or ANONYMOUS
        _job_sessions[job_id] = session
        _in_flight[session] = _in_flight.get(session, 0) + 1


def job_finished(job_id: str) -> None:
    """The async job (or timed-out GUI dispatch task) job_id ended.

    Frees the lock when this was the last in-flight count for a session
    release() had already marked pending: its work is done, so there is
    nothing left it could still write through the lock.
    """
    global _last_activity, _holder, _holder_label, _release_pending
    with _state_lock:
        session = _job_sessions.pop(job_id, None)
        if session is None:
            return
        remaining = _in_flight.get(session, 0) - 1
        if remaining <= 0:
            _in_flight.pop(session, None)
        else:
            _in_flight[session] = remaining
        if _holder == session:
            _last_activity = time.monotonic()
            if remaining <= 0 and _release_pending == session:
                _holder = None
                _holder_label = None
                _release_pending = None

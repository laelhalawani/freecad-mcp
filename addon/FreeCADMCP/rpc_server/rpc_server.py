import FreeCAD
import FreeCADGui

import contextlib
import io
import math
import os
import re
import threading
import time
import uuid
from collections.abc import Callable
from typing import Any
from xmlrpc.client import Fault
from xmlrpc.server import resolve_dotted_attribute

from PySide import QtCore

from rpc_server.commands import register_commands, schedule_toggle_sync
from rpc_server.errors import CONFLICT, INVALID_INPUT, NOT_FOUND, fail, tool_call
from rpc_server.fem_executor import run_fem_analysis as _run_fem_analysis
from rpc_server.gui_dispatch import (
    cleanup_waker,
    dispatch_to_gui,
    get_dispatch_status,
    init_waker,
    process_gui_tasks,
    request_shutdown,
)
from rpc_server.ip_filter import FilteredXMLRPCServer, validate_allowed_ips
from rpc_server.lookup import require_document
from rpc_server.object_factory import create_object_gui, edit_object_gui
from rpc_server.parts_library import get_parts_list, insert_part_from_library
from rpc_server.paths import home_example
from rpc_server.property_mapper import Object
from rpc_server.serialize import serialize_object
from rpc_server.settings import load_settings, save_settings
from rpc_server.version import PROTOCOL_VERSION, __version__ as ADDON_VERSION

# Feature handlers (documents, import, export, mesh tools, ...) live in their
# own modules. Each FreeCADRPC method below that serves one imports its module
# entry point on first call and returns its reply, so FreeCAD starts without
# loading them. The entry points run on this RPC thread and dispatch their own
# GUI-thread work themselves.

rpc_server_thread = None
rpc_server_instance = None
_stop_thread = None  # drains shutdown off the GUI thread; see stop_rpc_server

# Clients accepted while remote connections are off.
LOOPBACK_ALLOWED_IPS = "127.0.0.1,::1"

# Persistent namespace for execute_code / execute_code_async. A dedicated dict
# (instead of this module's globals()) keeps user code from shadowing server
# internals like dispatch_to_gui while preserving the documented pattern of
# sharing module-level variables between successive calls.
_EXEC_NAMESPACE: dict[str, Any] = {
    "FreeCAD": FreeCAD,
    "App": FreeCAD,
    "FreeCADGui": FreeCADGui,
    "Gui": FreeCADGui,
}
_async_execution = threading.local()

# Background jobs started by execute_code_async, newest last. The registry lets
# the client read errors raised off the GUI thread via get_async_status instead
# of only the Report View. Retain all running jobs and bound only completed
# history, in completion order.
_ASYNC_JOBS: dict[str, dict[str, Any]] = {}
_ASYNC_JOBS_LOCK = threading.Lock()
_ASYNC_JOBS_KEEP = 20


def _record_job(job_id: str, **fields: Any) -> None:
    with _ASYNC_JOBS_LOCK:
        job = _ASYNC_JOBS.pop(job_id, {"id": job_id})
        job.update(fields)
        _ASYNC_JOBS[job_id] = job
        finished = [
            key for key, value in _ASYNC_JOBS.items()
            if value.get("state") in {"done", "failed"}
        ]
        for key in finished[:max(0, len(finished) - _ASYNC_JOBS_KEEP)]:
            del _ASYNC_JOBS[key]


# XML 1.0 allows tab, line feed, carriage return and the characters from
# U+0020 up, except the surrogates, U+FFFE and U+FFFF. xmlrpc.client escapes
# only markup characters, so any other character (terminal colour codes in
# script output, a stray NUL in an object label) would make the whole reply
# unreadable to the client.
_XML_FORBIDDEN_RE = re.compile("[\x00-\x08\x0b\x0c\x0e-\x1f\ud800-\udfff￾￿]")


def _escape_forbidden(match: "re.Match[str]") -> str:
    return f"\\u{ord(match.group()):04x}"


def _xml_safe(value: Any) -> Any:
    """Return ``value`` with every string made valid XML 1.0 text.

    Forbidden characters are written as ``\\uXXXX`` escapes, so they stay
    visible in the reply instead of being dropped.
    """
    if isinstance(value, str):
        return _XML_FORBIDDEN_RE.sub(_escape_forbidden, value)
    if isinstance(value, dict):
        return {_xml_safe(key): _xml_safe(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_xml_safe(item) for item in value]
    return value


def _ok(res) -> bool:
    """True when a GUI-thread handler returned success."""
    return res is True


def _err(res) -> dict:
    """Convert any non-True result (error string or timeout dict) to a failure dict."""
    if isinstance(res, dict):
        return res
    return {"success": False, "error": str(res)}


def _commit_async(fn: Callable[[], Any], timeout: float = 120) -> Any:
    """Run an async script's document/view writes on the GUI thread."""
    if not getattr(_async_execution, "active", False):
        raise RuntimeError("commit() is only available inside execute_code_async workers")

    def task() -> tuple:
        from rpc_server.transactions import transaction

        # Same reasoning as execute_code's task(): commit() has no single
        # target document either (fn() is arbitrary script code), so no
        # active_document wrap is applied here.
        with transaction("execute_code_async"):
            return (fn(),)

    res = dispatch_to_gui(task, timeout=timeout, operation_name="async_commit")
    if isinstance(res, tuple):
        return res[0]
    error = _err(res)
    raise RuntimeError(f"commit() failed: {error['error']}")


# Keep one live namespace, including the helper: saved functions retain this
# dictionary as their globals. The thread-local guard prevents a GUI callback
# or synchronous script from waiting on its own GUI thread through commit().
_EXEC_NAMESPACE["commit"] = _commit_async


def _query_on_gui(task: Callable[[], Any], operation: str) -> Any:
    """Preserve query results while reporting dispatch failures as RPC faults."""
    # A tuple distinguishes valid results (including None and empty lists)
    # from the dispatcher's error strings and failure dictionaries.
    res = dispatch_to_gui(lambda: (task(),), operation_name=operation)
    if isinstance(res, tuple):
        return res[0]
    error = _err(res)
    code = error.get("code", "GUI_DISPATCH_FAILED")
    raise Fault(1, f"{code}: {error['error']}")


class FreeCADRPC:
    """RPC server for FreeCAD"""
    EXECUTE_CODE_TIMEOUT = 90  # GUI-thread execution; use execute_code_async for heavy OCCT ops
    # Ceiling for a caller-supplied execute_code timeout. A GUI task cannot be
    # cancelled once started, so an unbounded wait would hide a wedged GUI
    # thread from the caller indefinitely.
    MAX_EXECUTE_CODE_TIMEOUT = 1800

    def _dispatch(self, method: str, params: tuple) -> Any:
        """Call ``method`` and keep its reply and any fault valid XML.

        SimpleXMLRPCServer calls this for every method of the registered
        instance. Names starting with ``_`` are refused, as the default lookup
        refuses them, so helpers such as this one and ``_create_object_gui``
        cannot be called over RPC. Faults use the default wording, with
        characters XML 1.0 forbids escaped (see ``_xml_safe``).
        """
        try:
            func = resolve_dotted_attribute(self, method, False)
        except AttributeError:
            func = None
        if func is None or not callable(func):
            raise Exception(f'method "{method}" is not supported')
        try:
            result = func(*params)
        except Fault as fault:
            raise Fault(fault.faultCode, _xml_safe(str(fault.faultString))) from None
        except Exception as e:
            raise Fault(1, _xml_safe(f"{type(e)}:{e}")) from None
        return _xml_safe(result)

    def ping(self):
        return True

    def get_rpc_status(self) -> dict[str, Any]:
        """Report server and GUI-dispatch health without using the GUI thread."""
        with _ASYNC_JOBS_LOCK:
            running = [j["id"] for j in _ASYNC_JOBS.values() if j.get("state") == "running"]
        status = {
            "success": True,
            "rpc_server": "running",
            "gui_dispatch": get_dispatch_status(),
            "async_jobs_running": running,
            "addon_version": ADDON_VERSION,
            "protocol_version": PROTOCOL_VERSION,
            "execute_code_timeout": self.EXECUTE_CODE_TIMEOUT,
            "max_execute_code_timeout": self.MAX_EXECUTE_CODE_TIMEOUT,
        }
        # The document snapshot is kept by an observer, so reading it never
        # waits for the GUI thread. It adds keys but never replaces these.
        try:
            from rpc_server import status_snapshot

            for key, value in status_snapshot.snapshot().items():
                status.setdefault(key, value)
        except Exception as e:
            status["snapshot_error"] = f"{type(e).__name__}: {e}"
        return status

    def get_async_status(self, job_id: str = "") -> dict[str, Any]:
        """Report background jobs without using the GUI thread.

        With ``job_id`` returns that job (state ``running``/``done``/``failed``,
        error and traceback when failed). Without it returns all running jobs
        and up to 20 recently finished jobs. History resets when FreeCAD exits.
        """
        with _ASYNC_JOBS_LOCK:
            if job_id:
                job = _ASYNC_JOBS.get(job_id)
                if job is None:
                    return {"success": False, "error": f"unknown async job: {job_id}"}
                return {"success": True, "job": dict(job)}
            return {"success": True, "jobs": [dict(j) for j in _ASYNC_JOBS.values()]}

    def create_document(self, name="New_Document"):
        # The GUI handler reports the document's ACTUAL name: FreeCAD
        # sanitises requested names ("My Doc" -> "My_Doc") and de-duplicates
        # ("Doc" -> "Doc001"); reporting the requested name breaks every
        # follow-up call that uses it.
        res = dispatch_to_gui(
            lambda: self._create_document_gui(name),
            operation_name="create_document",
        )
        if isinstance(res, dict) and res.get("success"):
            return res
        return _err(res)

    def create_object(self, doc_name, obj_data: dict[str, Any]):
        obj = Object(
            name=obj_data.get("Name", "New_Object"),
            type=obj_data["Type"],
            analysis=obj_data.get("Analysis", None),
            properties=obj_data.get("Properties", {}),
        )
        # create_object_gui reports the created object's actual Name (see
        # its docstring), the same sanitise/de-duplicate concern as documents.
        res = dispatch_to_gui(
            lambda: self._create_object_gui(doc_name, obj),
            operation_name="create_object",
        )
        if isinstance(res, dict) and res.get("success"):
            return res
        return _err(res)

    def edit_object(self, doc_name: str, obj_name: str, properties: dict[str, Any]) -> dict[str, Any]:
        obj = Object(
            name=obj_name,
            properties=properties.get("Properties", {}),
        )
        res = dispatch_to_gui(
            lambda: self._edit_object_gui(doc_name, obj),
            operation_name="edit_object",
        )
        if isinstance(res, dict) and res.get("success"):
            return res
        return _err(res)

    def delete_object(self, doc_name: str, obj_name: str):
        res = dispatch_to_gui(
            lambda: self._delete_object_gui(doc_name, obj_name),
            operation_name="delete_object",
        )
        if isinstance(res, dict) and res.get("success"):
            return {"success": True, "object_name": obj_name, **{
                key: value for key, value in res.items() if key != "success"
            }}
        return _err(res)


    def reload_document(self, doc_name: str) -> dict[str, Any]:
        """Close and re-open a document by name to pick up external file
        changes (e.g. edits made by another process such as `freecadcmd`
        running headlessly). Returns success once the new document is
        loaded from disk, with ``document_name`` set to the name FreeCAD gave
        the reopened document, which can differ from ``doc_name``.
        """
        res = dispatch_to_gui(
            lambda: self._reload_document_gui(doc_name),
            operation_name="reload_document",
        )
        if isinstance(res, dict) and res.get("success"):
            return res
        return _err(res)

    def run_fem_analysis(self, doc_name: str, analysis_name: str, timeout: int = 600) -> dict[str, Any]:
        """Run the CalculiX solver on an existing Fem::FemAnalysis and return summary results."""
        try:
            timeout_s = int(timeout)
        except (TypeError, ValueError):
            return {"success": False, "error": f"invalid timeout: {timeout!r}"}
        res = dispatch_to_gui(
            lambda: self._run_fem_analysis_gui(doc_name, analysis_name),
            timeout=timeout_s,
            operation_name="run_fem_analysis",
        )
        if isinstance(res, dict):
            return res
        return {"success": False, "error": str(res)}

    def execute_code_async(self, code: str) -> dict[str, Any]:
        """Start code execution in a background thread and return immediately.

        Use for long-running OCCT *geometry* work (fuse/cut/loft on shapes) that
        would otherwise exceed the MCP timeout. The reply carries a ``job_id``;
        pass it to get_async_status to read the job's state (``running``,
        ``done`` or ``failed``) and, when it failed, its error and traceback.
        Starting a job never waits for the GUI thread.

        Thread-safety contract (read before using this method):

        FreeCAD documents and the Coin3D scenegraph are NOT thread-safe. Code run
        here executes off the GUI thread, so it must not touch them directly.
        Assigning ``obj.Shape``, calling ``doc.recompute()``, ``doc.addObject()``,
        ``doc.save()`` or any ``ViewObject`` from this thread races the GUI thread
        and can wedge FreeCAD's event loop, after which the RPC server stops
        answering entirely.

        Safe pattern: build shapes in the background, then hand the document write
        to the GUI thread via the injected ``commit`` helper::

            box = Part.makeBox(10, 10, 10)          # background: fine
            fused = base.fuse(box).removeSplitter()  # background: fine, this is the slow part

            def apply():                             # runs on the GUI thread
                obj.Shape = fused
                doc.recompute()

            commit(apply)                            # blocks until the GUI thread ran it

        ``commit(fn, timeout=...)`` returns ``fn``'s value, or raises RuntimeError
        if the GUI dispatch failed or timed out. The helper persists so saved
        functions can reuse it in later async calls. It may only be called from
        an async worker, not from a synchronous script or GUI callback.
        """
        # No status-bar message is shown for the job: setting one would wait on
        # the GUI thread, and process_gui_tasks clears the status bar as soon as
        # the task that set it finishes. Progress is read via get_async_status.
        job_id = f"job-{uuid.uuid4().hex}"
        code_preview = code if len(code) <= 200 else code[:200] + "…"

        def worker() -> None:
            # NOTE: we do NOT redirect sys.stdout here. contextlib.redirect_stdout
            # swaps stdout process-wide, not per-thread, so it would race with the
            # GUI thread and other concurrent work. Background code should report
            # via FreeCAD.Console (which is thread-safe) instead.
            # Execute against the live dictionary. Merging a snapshot on exit
            # would restore stale values and lose deletions/concurrent writes.
            _async_execution.active = True
            outcome: dict[str, Any] = {"state": "done"}
            try:
                exec(code, _EXEC_NAMESPACE)
            except BaseException as e:
                # SystemExit/KeyboardInterrupt raised by a worker script must
                # also finish its job record rather than leave it running.
                import traceback as _tb
                outcome = {
                    "state": "failed",
                    "error": f"{type(e).__name__}: {e}",
                    "traceback": _tb.format_exc().rstrip(),
                }
            finally:
                del _async_execution.active
                # Publish the result before best-effort logging, so a failing
                # log call cannot hide the script's outcome from the client.
                _record_job(job_id, finished=time.time(), **outcome)
                try:
                    if outcome["state"] == "done":
                        FreeCAD.Console.PrintMessage("Async code execution completed.\n")
                    else:
                        FreeCAD.Console.PrintError(
                            f"Async code error ({job_id}): {outcome['error']}\n{outcome['traceback']}\n"
                        )
                except Exception:
                    pass

        _record_job(job_id, state="running", started=time.time(), code=code_preview)
        try:
            threading.Thread(target=worker, daemon=True).start()
        except Exception as e:
            import traceback as _tb
            error = f"{type(e).__name__}: {e}"
            _record_job(
                job_id, state="failed", finished=time.time(), error=error,
                traceback=_tb.format_exc().rstrip(),
            )
            return {"success": False, "job_id": job_id, "error": error}
        return {
            "success": True,
            "job_id": job_id,
            "message": (
                "Code execution started in background. Document writes "
                "(obj.Shape = ..., recompute, addObject, save, ViewObject) must "
                "go through commit(fn); direct writes from this thread can wedge "
                "FreeCAD. Read the outcome with get_async_status(job_id)."
            ),
        }

    def execute_code(self, code: str, timeout: Any = None) -> dict[str, Any]:
        """Execute Python code on the GUI thread and wait for the result.

        Runs on the GUI thread so that FreeCAD document operations
        (addObject, recompute, save) are safe and correctly ordered.
        Use execute_code_async for heavy OCCT boolean ops (fuse/cut)
        that would block the GUI thread too long.

        ``timeout`` overrides EXECUTE_CODE_TIMEOUT for this call only, capped at
        MAX_EXECUTE_CODE_TIMEOUT. Raise it for genuinely slow GUI-thread work
        that cannot move off the GUI thread, such as importing or exporting a
        large STEP assembly. Without it such a call reports a timeout while the
        task keeps running, and its result is discarded even though the work
        completes.
        """
        timeout_s = self.EXECUTE_CODE_TIMEOUT
        if timeout is not None:
            try:
                timeout_s = float(timeout)
            except (TypeError, ValueError, OverflowError):
                return {"success": False, "error": f"invalid timeout: {timeout!r}"}
            if isinstance(timeout, bool) or not math.isfinite(timeout_s) or timeout_s <= 0:
                return {"success": False, "error": f"invalid timeout: {timeout!r}"}
            timeout_s = min(timeout_s, self.MAX_EXECUTE_CODE_TIMEOUT)

        output_buffer = io.StringIO()
        tx_fields: dict[str, Any] = {}

        def task():
            from rpc_server.transactions import transaction

            # No active_document wrap here: execute_code has no single target
            # document (the script decides, or touches none at all, or several
            # at once via FreeCAD.getDocument(...) by name), so there is
            # nothing we could make active on the script's behalf without
            # guessing. A script that reads/writes FreeCAD.ActiveDocument
            # gets whatever document is currently active, unchanged by this
            # call; a script that names another document explicitly and only
            # that document ends up transacted may still see FreeCAD open an
            # empty linked "-> execute_code" transaction in whatever is active
            # (App/Document.cpp:379-386), exactly as if the user had done the
            # same edit by hand from the Python console while that other
            # document was active in the GUI.
            with contextlib.redirect_stdout(output_buffer):
                with transaction("execute_code") as tx:
                    exec(code, _EXEC_NAMESPACE)
            tx_fields.update(tx.reply_fields())
            return True

        res = dispatch_to_gui(
            task,
            timeout=timeout_s,
            operation_name="execute_code",
        )
        if _ok(res):
            FreeCAD.Console.PrintMessage("Python code executed successfully.\n")
            return {
                "success": True,
                "message": "Python code executed successfully.\nOutput: " + output_buffer.getvalue(),
                **tx_fields,
            }
        # Log the offending code (truncated) to make errors traceable
        code_preview = code if len(code) <= 800 else code[:800] + "\n...(truncated)"
        FreeCAD.Console.PrintError(
            f"Error executing Python code: {res}\n"
            f"--- code ---\n{code_preview}\n--- end ---\n"
        )
        return _err(res)

    def get_objects(self, doc_name: str, compact: bool = False) -> list[dict[str, Any]]:
        """List a document's objects; ``compact`` gives one short row per object."""
        if compact:
            from rpc_server.serialize import list_objects_gui

            return _query_on_gui(lambda: list_objects_gui(doc_name), "get_objects")
        return _query_on_gui(lambda: self._get_objects_gui(doc_name), "get_objects")

    def _get_objects_gui(self, doc_name: str) -> list[dict[str, Any]]:
        # FreeCAD.getDocument raises (not returns None) for an unknown name.
        try:
            doc = FreeCAD.getDocument(doc_name)
        except Exception:
            return []
        return [serialize_object(obj) for obj in doc.Objects]

    def get_object(self, doc_name: str, obj_name: str) -> dict[str, Any] | None:
        return _query_on_gui(
            lambda: self._get_object_gui(doc_name, obj_name), "get_object"
        )

    def _get_object_gui(self, doc_name: str, obj_name: str) -> dict[str, Any] | None:
        # FreeCAD.getDocument raises (not returns None) for an unknown name.
        try:
            doc = FreeCAD.getDocument(doc_name)
        except Exception:
            return None
        obj = doc.getObject(obj_name)
        if obj:
            return serialize_object(obj)
        return None

    def insert_part_from_library(self, relative_path):
        res = dispatch_to_gui(
            lambda: self._insert_part_from_library(relative_path),
            operation_name="insert_part_from_library",
        )
        if isinstance(res, dict) and res.get("success"):
            return {"success": True, "message": "Part inserted from library.", **{
                key: value for key, value in res.items() if key != "success"
            }}
        return _err(res)

    def list_documents(self) -> list[str]:
        return _query_on_gui(
            lambda: list(FreeCAD.listDocuments().keys()), "list_documents"
        )

    def get_parts_list(self):
        return get_parts_list()

    def get_active_screenshot(
        self,
        view_name: str = "Isometric",
        width: int | None = None,
        height: int | None = None,
        focus_object: str | None = None,
        doc_name: str | None = None,
    ) -> dict[str, Any]:
        """Capture a 3D view as a base64-encoded PNG (view_manager).

        ``doc_name`` names the document whose 3D view is captured; without it
        the active view is. The reply carries the image, or a reason when
        there is no 3D view to capture; any other failure raises a Fault.
        """
        from rpc_server.view_manager import get_active_screenshot

        return get_active_screenshot(view_name, width, height, focus_object, doc_name)

    # Documents (documents.py, document_save.py)

    def get_documents(self) -> dict[str, Any]:
        from rpc_server.documents import get_documents

        return get_documents()

    def open_document(self, path, hidden=False, activate=True, timeout=None) -> dict[str, Any]:
        from rpc_server.documents import open_document

        return open_document(path, hidden, activate, timeout)

    def activate_document(self, doc_name, view_index=None, create_view=False) -> dict[str, Any]:
        from rpc_server.documents import activate_document

        return activate_document(doc_name, view_index, create_view)

    def save_document(self, doc_name, recompute=True, timeout=None) -> dict[str, Any]:
        from rpc_server.document_save import save_document

        return save_document(doc_name, recompute, timeout)

    def save_document_as(
        self, doc_name, path, overwrite=False, copy=False, recompute=True, timeout=None
    ) -> dict[str, Any]:
        from rpc_server.document_save import save_document_as

        return save_document_as(doc_name, path, overwrite, copy, recompute, timeout)

    def close_document(self, doc_name, discard_changes=False) -> dict[str, Any]:
        from rpc_server.document_save import close_document

        return close_document(doc_name, discard_changes)

    # Files (importer.py, exporter.py)

    def import_file(self, path, doc_name=None, options=None, timeout=None) -> dict[str, Any]:
        from rpc_server.importer import import_file

        return import_file(path, doc_name, options, timeout)

    def export_document(self, doc_name, path, options=None, timeout=None) -> dict[str, Any]:
        from rpc_server.exporter import export_document

        return export_document(doc_name, path, options, timeout)

    # Checks (recompute.py, printability.py)

    def recompute_document(self, doc_name, timeout=None) -> dict[str, Any]:
        from rpc_server.recompute import recompute_document

        return recompute_document(doc_name, timeout)

    def check_printability(self, doc_name, object_names=None, options=None, timeout=None) -> dict[str, Any]:
        from rpc_server.printability import check_printability

        return check_printability(doc_name, object_names, options, timeout)

    # Meshes (mesh_tools.py)

    def analyze_mesh(self, doc_name, obj_name, timeout=None) -> dict[str, Any]:
        from rpc_server.mesh_tools import analyze_mesh

        return analyze_mesh(doc_name, obj_name, timeout)

    def repair_mesh(self, doc_name, obj_name, steps=None, options=None, timeout=None) -> dict[str, Any]:
        from rpc_server.mesh_tools import repair_mesh

        return repair_mesh(doc_name, obj_name, steps, options, timeout)

    def mesh_to_solid(self, doc_name, obj_name, options=None, timeout=None) -> dict[str, Any]:
        from rpc_server.mesh_tools import mesh_to_solid

        return mesh_to_solid(doc_name, obj_name, options, timeout)

    def solid_to_mesh(self, doc_name, obj_name, options=None, timeout=None) -> dict[str, Any]:
        from rpc_server.mesh_tools import solid_to_mesh

        return solid_to_mesh(doc_name, obj_name, options, timeout)

    # Undo and redo (undo.py)

    def undo(self, doc_name, steps=1) -> dict[str, Any]:
        from rpc_server.undo import undo

        return undo(doc_name, steps)

    def redo(self, doc_name, steps=1) -> dict[str, Any]:
        from rpc_server.undo import redo

        return redo(doc_name, steps)

    # Spreadsheets (spreadsheet.py)

    def get_spreadsheet_cells(self, doc_name, sheet_name, cells=None) -> dict[str, Any]:
        from rpc_server.spreadsheet import get_spreadsheet_cells

        return get_spreadsheet_cells(doc_name, sheet_name, cells)

    def update_spreadsheet_cells(self, doc_name, sheet_name, cells, recompute=True) -> dict[str, Any]:
        from rpc_server.spreadsheet import update_spreadsheet_cells

        return update_spreadsheet_cells(doc_name, sheet_name, cells, recompute)

    # Inspection (measure.py, selection.py)

    def measure(self, doc_name, kind, refs) -> dict[str, Any]:
        from rpc_server.measure import measure

        return measure(doc_name, kind, refs)

    def get_selection(self, doc_name=None) -> dict[str, Any]:
        from rpc_server.selection import get_selection

        return get_selection(doc_name)

    def _create_document_gui(self, name):
        from rpc_server.transactions import transaction

        # No active_document wrap needed here (unlike create_object/
        # edit_object/delete_object/run_fem_analysis): FreeCAD.newDocument
        # makes the new document the Application's active document itself,
        # synchronously, before returning and before any property of it can
        # be changed (App/Application.cpp:505-517, the "temporary" restore
        # branch there does not apply since we do not pass temp=True). So by
        # the time doc.recompute() below can open the per-document
        # transaction, doc already IS the active document and no linked "->"
        # transaction can be opened elsewhere. Also, unlike those other
        # tools, create_document's whole point is to give the caller a new,
        # current document to work in, so leaving it active (rather than
        # restoring whatever was active before) is intentional.
        with transaction("create_document") as tx:
            doc = FreeCAD.newDocument(name)
            doc.recompute()
        FreeCAD.Console.PrintMessage(f"Document '{doc.Name}' created via RPC.\n")
        return {"success": True, "document_name": doc.Name, **tx.reply_fields()}

    def _create_object_gui(self, doc_name, obj: Object):
        return create_object_gui(doc_name, obj)

    def _edit_object_gui(self, doc_name: str, obj: Object):
        return edit_object_gui(doc_name, obj)

    def _run_fem_analysis_gui(self, doc_name: str, analysis_name: str):
        return _run_fem_analysis(doc_name, analysis_name)

    def _delete_object_gui(self, doc_name: str, obj_name: str):
        from rpc_server.transactions import active_document, transaction

        try:
            doc = FreeCAD.getDocument(doc_name)
        except Exception:
            FreeCAD.Console.PrintError(f"Document '{doc_name}' not found.\n")
            return f"Document '{doc_name}' not found.\n"

        try:
            # active_document (transactions.py) holds doc active for the
            # transaction's whole life, else FreeCAD can open an empty linked
            # "-> delete_object" transaction in whatever document the GUI has
            # focused (App/Document.cpp:379-386).
            with active_document(doc), transaction("delete_object") as tx:
                doc.removeObject(obj_name)
                doc.recompute()
            FreeCAD.Console.PrintMessage(f"Object '{obj_name}' deleted via RPC.\n")
            return {"success": True, **tx.reply_fields()}
        except Exception as e:
            return str(e)


    def _reload_document_gui(self, doc_name: str):
        doc, error = require_document(doc_name)
        if error is not None:
            return error
        file_path = doc.FileName
        if not file_path:
            suggested_path = home_example(f"{doc_name}.FCStd")
            return fail(
                CONFLICT,
                f"Document '{doc_name}' has no file on disk "
                "(unsaved scratch document); nothing to reload from.",
                "Call "
                + tool_call("save_document_as", {"doc_name": doc_name, "path": suggested_path})
                + " to save it first.",
            )
        if not os.path.exists(file_path):
            return fail(
                NOT_FOUND,
                f"File for '{doc_name}' not found at {file_path!r}.",
                "Call "
                + tool_call("save_document_as", {"doc_name": doc_name, "path": file_path, "overwrite": True})
                + " to write it there again, or check the path.",
            )
        # Close, then reopen from the same file. FreeCAD names the reopened
        # document after the file (de-duplicated against open documents), so
        # report the name it actually received.
        FreeCAD.closeDocument(doc_name)
        reopened = FreeCAD.openDocument(file_path)
        FreeCAD.Console.PrintMessage(
            f"Document '{doc_name}' reloaded from '{file_path}' as "
            f"'{reopened.Name}' via RPC.\n"
        )
        return {"success": True, "document_name": reopened.Name}

    def _insert_part_from_library(self, relative_path):
        try:
            tx_fields = insert_part_from_library(relative_path)
            return {"success": True, **(tx_fields or {})}
        except FileNotFoundError as e:
            return fail(
                NOT_FOUND,
                str(e),
                "Call " + tool_call("list_parts", {}) + " to see the available parts.",
            )
        except ValueError as e:
            return fail(
                INVALID_INPUT,
                str(e),
                "Call " + tool_call("list_parts", {}) + " to see the available parts.",
            )
        except Exception as e:
            return str(e)


def _status_snapshot_hook(action: str) -> None:
    """Install or remove the document observer behind get_rpc_status.

    A failure is reported on the Console and never stops the server from
    starting or stopping: get_rpc_status then reports the snapshot error.
    """
    try:
        from rpc_server import status_snapshot

        getattr(status_snapshot, action)()
    except Exception as e:
        FreeCAD.Console.PrintWarning(
            f"MCP RPC: document snapshot {action} failed: {type(e).__name__}: {e}\n"
        )


def start_rpc_server(port: int = 9875) -> str:
    global rpc_server_thread, rpc_server_instance

    if rpc_server_instance:
        host, bound_port = rpc_server_instance.server_address
        return f"RPC Server already running at {host}:{bound_port} (PID {os.getpid()})."

    # A previous stop may still be draining an in-flight request off-thread;
    # binding before its server_close() would hit the old socket.
    if _stop_thread is not None and _stop_thread.is_alive():
        _stop_thread.join(timeout=5.0)
        if _stop_thread.is_alive():
            return ("RPC Server is still stopping (a request is draining); "
                    "try again in a few seconds.")

    settings = load_settings()
    remote_enabled = settings.get("remote_enabled", False)
    auth_token = settings.get("auth_token", "")

    if remote_enabled:
        host = "0.0.0.0"
        allowed_ips = settings.get("allowed_ips", "127.0.0.1")
        if not auth_token:
            FreeCAD.Console.PrintWarning(
                "MCP RPC: remote connections are enabled WITHOUT an auth token. "
                "Anyone on an allowed IP can execute code in FreeCAD. "
                "Set a token via 'Set Auth Token' in the FreeCAD MCP menu.\n"
            )
    else:
        host = "127.0.0.1"
        # The stored allowlist applies to remote connections only. A LAN-only
        # list such as 192.168.1.0/24 would otherwise reject every client of
        # the loopback-bound server.
        allowed_ips = LOOPBACK_ALLOWED_IPS

    server = FilteredXMLRPCServer(
        (host, port),
        allowed_ips_str=allowed_ips,
        auth_token=auth_token,
        allow_none=True,
        logRequests=False,
    )
    try:
        server.register_instance(FreeCADRPC())
        init_waker()
        QtCore.QTimer.singleShot(500, process_gui_tasks)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
    except Exception:
        # A failed start must not retain a listening socket or appear running.
        # This allows the toolbar command to be retried without restarting FreeCAD.
        server.server_close()
        request_shutdown()
        cleanup_waker()
        raise

    rpc_server_instance = server
    rpc_server_thread = thread
    _status_snapshot_hook("install")
    bound_host, bound_port = server.server_address
    msg = f"RPC Server started at {bound_host}:{bound_port} (PID {os.getpid()})."
    if remote_enabled:
        msg += f" Allowed IPs: {allowed_ips}"
    if auth_token:
        msg += " Auth token required."
    return msg


def stop_rpc_server():
    global rpc_server_instance, rpc_server_thread, _stop_thread

    if not rpc_server_instance:
        return "RPC Server was not running."

    server = rpc_server_instance
    thread = rpc_server_thread
    rpc_server_instance = None
    rpc_server_thread = None

    request_shutdown()
    cleanup_waker()
    _status_snapshot_hook("remove")

    def _shutdown_and_close():
        # shutdown() only stops the accept loop; in-flight requests run in
        # their own daemon threads and are not waited for. Kept off the GUI
        # thread so a menu command cannot block the UI. server_close() must
        # always follow, or the listening socket stays bound and Stop -> Start
        # fails with EADDRINUSE.
        try:
            server.shutdown()
            if thread is not None:
                thread.join(timeout=10.0)
                if thread.is_alive():
                    FreeCAD.Console.PrintWarning(
                        "MCP RPC: server thread still draining a request; "
                        "socket closes when it finishes.\n"
                    )
        finally:
            server.server_close()
        FreeCAD.Console.PrintMessage("RPC Server stopped.\n")

    _stop_thread = threading.Thread(target=_shutdown_and_close, daemon=True)
    _stop_thread.start()
    return "RPC Server stopping…"


register_commands()
schedule_toggle_sync()

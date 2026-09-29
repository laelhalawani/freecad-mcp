# Code execution

[Back to README](../README.md) · [Tools](tools.md) · [Configuration](configuration.md)

## Choose an execution mode

| Tool | Use case |
| --- | --- |
| `execute_code` | Normal FreeCAD automation; the script runs on the GUI thread. |
| `execute_code_async` | Long background computations on independent geometry; hand document and view access to the GUI thread with `commit()`. |
| `execute_code_headless` | Heavy OCCT operations in a separate process, isolating native crashes from the GUI. |

A reply keeps at most the last 512 KiB of printed output or a traceback, and
says when it left the start out, so it stays within the 1 MiB reply limit.

## Shared script state

`execute_code` and `execute_code_async` share a persistent script namespace with
`FreeCAD`/`App` and `FreeCADGui`/`Gui` aliases. Script variables survive between
calls without overwriting the RPC server's own functions. This prevents accidental
name collisions; code execution still has FreeCAD's full privileges.

Concurrent scripts share live variables and must coordinate intentional writes
to the same data. Headless scripts run in a fresh process and do not share this
namespace.

## Background jobs

`execute_code_async` returns a `job_id` at once, without waiting for the GUI
thread. `get_async_status(job_id)` reports whether the job is `running`, `done`,
or `failed`, and includes the exception and traceback for failed jobs.

All running jobs and the 20 most recently completed jobs are retained in memory
until FreeCAD exits. `get_async_status()` lists this history; `get_rpc_status`
lists the IDs of jobs still running. Neither uses the GUI thread, so both answer
while a job or a GUI operation runs. Script success does not certify geometry
validity.

Job status needs the addon that ships with this server; `freecad-mcp
install-addon` installs it. An addon without job IDs reports only that the job
started; its outcome then shows in FreeCAD's Report View.

### Document and view access

Async code must keep document and view access on the GUI thread. Build
independent OCCT shapes in the worker, then use `commit(fn, timeout=120)` to apply
the result and recompute the document on the GUI thread. The helper returns
`fn`'s value or raises `RuntimeError` on failure.

`commit()` persists in the shared namespace so saved functions can reuse it in
later async calls. Calling it from `execute_code` or inside a GUI callback raises
immediately.

## Headless execution

A headless script that may take minutes runs in the background: pass
`background` true, or a `timeout` over 120 seconds. The call returns a `job_id`
at once, the output streams to a file, and `get_async_status` reports the job
(`running`, `finished` or `cancelled`, exit code, elapsed seconds, the last 200
lines of output); `cancel_job` stops it. Background jobs end when the MCP
server exits. On Windows every headless process runs in a job object, so a
cancel, a timeout, the end of the run and even a hard kill of the MCP server end
the whole process tree; on Linux and macOS the process group is ended by a
cancel, a timeout and a normal exit, and a job can outlive an MCP server that
was killed. A foreground run keeps the call open until the script ends. A
headless exit code that only a crash produces (an exception such as
`0xC0000005`, or `-1`) is reported as a crash of FreeCAD, with the output so
far, not as a bare exit code.

`execute_code_headless` writes the script to a file and runs it with
`freecadcmd -c` in a separate process. Use it for OpenCascade work that may
segfault or block the GUI for minutes: `makeHelix` + `makePipeShell` threads,
lofts and sweeps, or booleans with many B-spline tools. A native crash only ends
the helper process; the tool reports the signal (e.g. `SIGSEGV`) together with
everything the script printed, and the GUI keeps its documents.

The script must import the modules it needs and open and save documents itself
(`FreeCAD.openDocument`, `doc.save()`, `doc.saveAs()`, or `Shape.exportBrep` for
shape export). After saving an `.FCStd` file that is open in the GUI, use
`reload_document(doc_name)` to refresh the GUI copy. Get the document name with
`list_documents`.

The executable runs on the machine hosting the MCP server; `FREECAD_MCP_HOST`
only selects the GUI RPC host. Use file paths accessible on the MCP server machine. The
timeout is more than 0 and at most 604800 seconds (a week); the default is
600 seconds. A timeout returns
partial stdout/stderr, and temporary scripts are removed on success, failure,
and timeout.

The executable is auto-detected: `freecadcmd` or Snap's `freecad.cmd` on PATH,
then the standard installations (on Windows the FreeCAD installer's registry
entries and `Program Files`, on macOS `FreeCAD.app`), then the
`org.freecad.FreeCAD` Flatpak. `freecad-mcp doctor` shows what it found.
Override it with `FREECAD_MCP_FREECADCMD` in the client entry's `env` block,
for example `flatpak run --command=freecadcmd org.freecad.FreeCAD`. Words are
split like a shell does; quote a path with spaces. On Windows, backslashes are
path separators, not escapes.

On Windows a native crash ends the process with an exception code rather than a
signal; the tool reports it by name, for example `EXCEPTION_ACCESS_VIOLATION`.

## Agent banner

While an agent works, FreeCAD shows a banner along the top edge of the 3D view
area: the agent's name (the lock holder's, with remote access on), what it is
doing in plain words, how long it has run, and "An agent is changing this
model; please don't edit until it finishes." It never blocks the mouse or the
keyboard and takes no focus. It appears when a GUI-thread call has run about a
second, or at once, before the call starts, for the calls that may block the
GUI thread for long (`execute_code`, `recompute_document`, `run_fem_analysis`,
`import_file`, `export_document`, `repair_mesh`, `mesh_to_solid`,
`solid_to_mesh`, `analyze_mesh`, `check_printability`, `open_document` and
`reload_document`), which add "FreeCAD may not respond until this finishes".
Because these draw the banner before they know how long they will take, even a
short one shows it briefly and starts about 80 ms later. The banner takes its
background from the colour FreeCAD's main window is drawn in, with dark or light
text by that colour, so it follows a light or a dark theme. It also shows while a background job runs (an `execute_code_async`
job, or a headless job of the MCP server), with its timer ticking. It hides when
the work ends. Reads and `set_view` show nothing. Its text is plain text, and it
works whether or not remote access is on.

## GUI dispatch timeouts

GUI-thread operations run one at a time in FIFO order. Calls have separate queue
and execution budgets: execution time counts from the moment a call starts on
the GUI thread, and waiting for an earlier operation does not consume that
budget. The queue budget defaults to the execution budget. If it expires before
a call starts, the call is cancelled and will not run later; this does not mark
dispatch as stuck.

| Operation | Queue budget | Execution budget | MCP server's reply timeout |
| --- | --- | --- | --- |
| `execute_code` | 90 seconds (or `timeout`) | 90 seconds (or `timeout`) | At least `2 * timeout + 30` seconds; 210 seconds by default |
| `run_fem_analysis` | Requested `timeout` (1 to 604800 seconds, default 600) | Requested `timeout` | At least `2 * timeout + 30` seconds |
| `import_file`, `export_document`, `repair_mesh`, `mesh_to_solid`, `solid_to_mesh` | 300 seconds (or `timeout`) | 300 seconds (or `timeout`) | At least `2 * timeout + 30` seconds; 630 seconds by default |
| `check_printability` | 120 seconds (or `timeout`) | 120 seconds (or `timeout`) | At least `2 * timeout + 30` seconds; 270 seconds by default |
| `open_document`, `save_document`, `save_document_as`, `recompute_document`, `analyze_mesh` | 120 seconds (or `timeout`) | 120 seconds (or `timeout`) | At least `2 * timeout + 30` seconds; 270 seconds by default |

Every call is bounded by the MCP server's own deadline, the reply timeout above. When FreeCAD does not answer within it, the call fails with "Failed to <action>: FreeCAD did not answer within N s; it may be busy computing." and the hint "Call get_rpc_status with {}: it answers while FreeCAD computes and says whether FreeCAD is busy. Wait and poll it; do not repeat the call while FreeCAD is busy." This deadline hint replaces the larger-timeout hint, which stays only for the addon's own run-budget timeout. That tool tells a FreeCAD that is busy computing (its process is using CPU over a short window) from one that is stuck behind a dialog, and its last known documents show how old the reading is.

Every other tool, including `undo`, `redo`, `measure`, `get_selection`,
`get_spreadsheet_cells` and `update_spreadsheet_cells`, uses a fixed 60-second
budget and takes no `timeout` argument.

A GUI task cannot be cancelled once it has started, so a slower call reports a
timeout while the task keeps running, and its result is discarded even though
the work completes. Pass `timeout` (more than 0 and at most 1800 seconds) on a
tool that accepts it for work that genuinely has to run on the GUI thread and
takes longer, such as importing or exporting a large STEP assembly or checking
printability on a dense mesh; the MCP server widens its reply timeout to
match. Values outside that range are rejected before the MCP server contacts
FreeCAD. Without `timeout` the tool's own default above applies. For heavy
pure-geometry work that touches neither the document nor the GUI, prefer
`execute_code_async`.

The MCP server's timeout covers both budgets plus a 30-second margin. MCP hosts
must allow these response times in their own timeout settings.
Concurrent `execute_code` calls can be queued, but they still execute
sequentially, so total wall time includes each individual run.

### Recover from a stuck GUI operation

If a GUI-thread operation exceeds its execution budget after starting, the
bridge returns `GUI_DISPATCH_STUCK` and rejects later GUI operations immediately.
Calls that were already queued keep waiting, up to their queue timeout, and run
once the stuck operation returns.

Use `get_rpc_status` from a separate RPC client to identify the operation that
is still running. The RPC server handles connections concurrently, so
diagnostics do not wait for another request to finish. Document queries
(`get_object`, `list_objects`, and `list_documents`) run on the GUI thread
alongside modelling operations. `list_documents` reports an `unavailable`
error with a hint to check `get_rpc_status` if dispatch times out or is stuck;
`get_object` and `list_objects` still report an RPC fault in that case.

FreeCAD GUI work cannot be force-cancelled safely. If status does not return to
`healthy` after the operation finishes, restart FreeCAD.

After an `execute_code` exception on a FreeCAD development build, inspect any
new `FeaturePython` object before mutating or deleting it. In particular, do
not continue with an object whose required `Proxy` was never installed, as
touching that broken object can wedge FreeCAD's GUI thread.

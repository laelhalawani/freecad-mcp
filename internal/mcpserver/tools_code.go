package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/headless"
	"github.com/sairaph/mcp-wizard/render"
)

type executeCodeInput struct {
	Code    string   `json:"code" jsonschema:"the Python code to execute"`
	Timeout *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800, overriding the 90 s default for this call; raise it for other slow work that must run on the GUI thread"`
	screenshotOptions
}

type codeInput struct {
	Code string `json:"code" jsonschema:"background-safe Python code; use commit(fn) for every document and view write"`
}

type headlessInput struct {
	Code    string   `json:"code" jsonschema:"a complete Python script for freecadcmd"`
	Timeout *float64 `json:"timeout,omitempty" jsonschema:"seconds to wait before killing the process, more than 0 and at most 604800 (a week); default 600; partial output is kept on timeout"`
}

type asyncStatusInput struct {
	JobID *string `json:"job_id,omitempty" jsonschema:"the job_id execute_code_async returned; omit it to list all running jobs and up to 20 recently finished ones"`
}

type executeFront struct {
	Status      string `yaml:"status"`
	Transaction string `yaml:"transaction,omitempty"`
}

type asyncFront struct {
	JobID string `yaml:"job_id,omitempty"`
	State string `yaml:"state,omitempty"`
}

type jobsFront struct {
	Count int `yaml:"count"`
}

type headlessFront struct {
	Success  bool `yaml:"success"`
	ExitCode *int `yaml:"exit_code,omitempty"`
	Crashed  bool `yaml:"crashed,omitempty"`
	TimedOut bool `yaml:"timed_out,omitempty"`
}

const executeCodeDescription = `Execute Python code in FreeCAD on its GUI thread and wait for the result. This is the safe default for FreeCAD automation that the other tools do not cover: FreeCAD, FreeCADGui and the document are available, and whatever the code prints is returned.

The code runs with a 90 s budget for waiting its turn and another for running; pass timeout to raise both for other slow GUI-thread work that cannot move off the GUI thread. Without it such a call reports a timeout while the task keeps running, and its result is lost. Use import_file and export_document for STEP, STL, 3MF and other file exchange instead of scripting it here; prefer execute_code_async for heavy pure-geometry work that touches neither the document nor the GUI, and execute_code_headless for OCCT work that may crash FreeCAD.

Set include_screenshot to false when the code does not change the model's appearance, e.g. analytical scripts whose result is printed output.`

const executeCodeAsyncDescription = `Execute Python code in FreeCAD without waiting for completion. Use this ONLY for long-running background computations that do NOT touch the FreeCAD GUI or mutate the document tree directly.

The code runs in a background thread and the call returns at once with a job_id; poll get_async_status with it. Because the code does not run on FreeCAD's GUI thread, it must NOT call FreeCADGui APIs, manipulate the active view or selection, create or edit document objects, change object properties, call doc.recompute(), or save documents. FreeCAD documents and the Coin3D scenegraph are not thread-safe: writing to them from this thread races the GUI thread and can wedge FreeCAD's event loop, after which the RPC server stops responding and FreeCAD must be restarted.

Every document or view write must instead be handed to the GUI thread through the injected commit() helper:

    commit(fn, timeout=120) -> fn's return value

commit() queues fn on the GUI thread, waits for it, and raises RuntimeError if dispatch fails or times out. Scripts share a live namespace, so saved functions can use commit() in later async calls; calling it from execute_code or a GUI callback raises immediately. Coordinate concurrent scripts that intentionally modify the same variables. Example:

    fused = base.fuse(addition).removeSplitter()   # slow, safe in background

    def apply():                                   # runs on the GUI thread
        obj.Shape = fused
        doc.recompute()

    commit(apply)

Use this only when the heavy part is long-running OCCT geometry (fuse/cut/loft on already-fetched shapes) or other CPU-bound work that would exceed execute_code's 90 s budget. Typical pattern: 1. fetch shapes into module-level variables with execute_code; 2. run the heavy computation here; 3. apply the result inside commit(), or keep it in a module-level variable for a later execute_code call.

Performance note: booleans against shapes with many faces (e.g. a ribbed lid) are expensive. Fuse the additions together first, then apply a single boolean against the heavy shape, and avoid doc.recompute() unless the dependency graph needs it.`

const headlessDescription = `Run a FreeCAD Python script in a separate headless freecadcmd process.

Use this for OCCT work that can crash or block FreeCAD: helical threads (makeHelix + makePipeShell), lofts and sweeps, booleans with many or B-spline tools, long parametric rebuilds. A native OpenCascade crash here only kills the helper process; the GUI and its open documents survive, and the tool reports the crash and the script's output.

The script runs on the machine running this MCP server, independently of FREECAD_MCP_HOST, in a fresh process without GUI: import FreeCAD and Part yourself, open documents from disk (FreeCAD.openDocument(path)), and save results with doc.save()/saveAs() or Shape.exportBrep(). Nothing from the execute_code namespace is available. Print progress to stdout; it is returned when the process ends. After the script saved a .FCStd that is open in the GUI, call reload_document to show the result.`

func (s *Server) registerCodeTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "execute_code",
		Description: executeCodeDescription,
		InputSchema: withPositiveMax(inputSchema[executeCodeInput](screenshotDefaults), "timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.executeCode)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "execute_code_async",
		Description: executeCodeAsyncDescription,
		InputSchema: inputSchema[codeInput](nil),
	}, s.executeCodeAsync)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "get_async_status",
		Description: "Report the state of background jobs started by execute_code_async: running, done or " +
			"failed, with the error and traceback of a failed job. It does not use the FreeCAD GUI thread, so it " +
			"answers even while a job runs. History is held in memory until FreeCAD exits.",
		InputSchema: inputSchema[asyncStatusInput](nil),
	}, s.getAsyncStatus)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "execute_code_headless",
		Description: headlessDescription,
		InputSchema: withPositiveMax(inputSchema[headlessInput](map[string]string{"timeout": "600"}), "timeout", headless.MaxTimeout),
	}, s.executeCodeHeadless)
}

func (s *Server) executeCode(ctx context.Context, _ *mcp.CallToolRequest, in executeCodeInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure("execute code", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("execute code", err, ""), nil, nil
	}
	res, err := conn.ExecuteCode(ctx, in.Code, in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure("execute code", err, largerTimeout("execute_code"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reported("execute code", res,
			"Fix the code and retry; FreeCAD's Report View shows the traceback. Call get_rpc_status if the GUI thread seems stuck. "+
				largerTimeout("execute_code"))), nil, nil
	}
	txName, txMerged := transactionFields(res)
	out := render.SuccessResult(executeFront{Status: "ok", Transaction: txName},
		transactionNote("Code executed successfully: "+truncateOutput(str(res, "message")), txName, txMerged))
	// A screenshot is only taken after the code completed, so a failure never
	// queues a second GUI call behind a task that may still be running.
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), "")), nil, nil
}

func (s *Server) executeCodeAsync(ctx context.Context, _ *mcp.CallToolRequest, in codeInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("start async code execution", err, ""), nil, nil
	}
	res, err := conn.ExecuteCodeAsync(ctx, in.Code)
	if err != nil {
		return s.withNotice(failure("start async code execution", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reported("start async execution", res, "Fix the code and retry.")), nil, nil
	}
	jobID := str(res, "job_id")
	if jobID == "" {
		return s.withNotice(render.SuccessResult(asyncFront{State: "started"},
			"Code execution started in background.\n"+
				"This addon does not report job IDs; use get_object to poll a document status object "+
				"and inspect FreeCAD's Report View for errors.")), nil, nil
	}
	return s.withNotice(render.SuccessResult(asyncFront{JobID: jobID, State: "running"},
		fmt.Sprintf("Code execution started in background (job_id: %s).\n"+
			"Poll get_async_status with {\"job_id\": %q} for state, error and traceback. "+
			"FreeCAD's Report View shows printed output when done.", jobID, jobID))), nil, nil
}

func (s *Server) getAsyncStatus(ctx context.Context, _ *mcp.CallToolRequest, in asyncStatusInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("get async status", err, ""), nil, nil
	}
	jobID := ""
	if in.JobID != nil {
		jobID = *in.JobID
	}
	res, err := conn.GetAsyncStatus(ctx, jobID)
	if err != nil {
		return s.withNotice(failure("get async status", err, "")), nil, nil
	}
	if !succeeded(res) {
		code := codeFreeCAD
		if strings.HasPrefix(errorText(res), "unknown async job") {
			code = render.CodeNotFound
		}
		return s.withNotice(render.ErrorResult(render.Error{
			Code:    code,
			Message: "Failed to get async status: " + errorText(res),
			Hint:    "Call get_async_status with {} to list the known jobs.",
		})), nil, nil
	}
	job, ok := res["job"].(map[string]any)
	if !ok {
		jobs, _ := res["jobs"].([]any)
		if jobs == nil {
			jobs = []any{}
		}
		return s.withNotice(render.SuccessResult(jobsFront{Count: len(jobs)}, jsonBlock(jobs))), nil, nil
	}
	id, state := str(job, "id"), str(job, "state")
	if state == "" {
		state = "unknown"
	}
	text := fmt.Sprintf("Async job %s: %s", id, state)
	if e := str(job, "error"); e != "" {
		text += "\nError: " + shortMessage(e)
	}
	if tb := str(job, "traceback"); tb != "" {
		text += "\n" + truncateOutput(tb)
	}
	return s.withNotice(render.SuccessResult(asyncFront{JobID: id, State: state}, text)), nil, nil
}

func errorText(res map[string]any) string {
	if e := str(res, "error"); e != "" {
		return e
	}
	return "unknown"
}

func (s *Server) executeCodeHeadless(ctx context.Context, _ *mcp.CallToolRequest, in headlessInput) (*mcp.CallToolResult, any, error) {
	timeout := 600.0
	if in.Timeout != nil {
		timeout = *in.Timeout
	}
	r := headless.Run(ctx, in.Code, timeout, s.config.FreeCAD.FreecadCmd)
	front := headlessFront{Success: r.Success, ExitCode: r.ReturnCode, Crashed: r.Crashed, TimedOut: r.TimedOut}
	if !r.Success {
		code := codeFreeCAD
		hint := "Fix the script and retry; its output below shows where it stopped."
		switch {
		case strings.Contains(r.Error, "positive finite"):
			code, hint = render.CodeInvalidInput, invalidTimeoutHint
		case strings.Contains(r.Error, "freecadcmd not found"), strings.Contains(r.Error, "could not start"):
			code, hint = render.CodeUnavailable, "Install FreeCAD, or set "+
				domain.EnvFreecadCmd+" in the AI client's config to the command that starts freecadcmd."
		case strings.HasPrefix(r.Error, "could not prepare the script directory"), strings.HasPrefix(r.Error, "could not write the script"):
			code, hint = render.CodeInternal, "The script could not be written to freecad-mcp's cache directory "+
				"(.cache/freecad-mcp/headless in the home directory). Make sure it is writable and the disk has space, then retry."
		case r.TimedOut:
			code, hint = render.CodeUnavailable, fmt.Sprintf("Call execute_code_headless again with a larger timeout "+
				"(this run allowed %g s), or split the script into shorter steps; the output below shows how far it got.", timeout)
		case strings.Contains(r.Error, "cancelled"):
			code, hint = render.CodeUnavailable, cancelledHint
		}
		fields := map[string]any{"crashed": r.Crashed, "timed_out": r.TimedOut}
		if r.ReturnCode != nil {
			fields["exit_code"] = *r.ReturnCode
		}
		msg := r.Error
		if msg == "" {
			msg = "unknown error"
		}
		res := render.ErrorResult(render.Error{Code: code, Message: shortMessage("Headless FreeCAD script failed: " + msg), Hint: hint, Fields: fields})
		if output := strings.TrimRight(r.Output, " \t\r\n"); output != "" {
			res.Content = append(res.Content, &mcp.TextContent{Text: "Output:\n" + textBlock(output)})
		}
		return s.withNotice(res), nil, nil
	}
	r.Output = truncateOutput(r.Output)
	return s.withNotice(render.SuccessResult(front, headless.Format(r))), nil, nil
}

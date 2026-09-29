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
	Code    string   `json:"code"`
	Timeout *float64 `json:"timeout,omitempty"`
	screenshotOptions
}

type codeInput struct {
	Code string `json:"code"`
}

type headlessInput struct {
	Code    string   `json:"code"`
	Timeout *float64 `json:"timeout,omitempty"`
}

type asyncStatusInput struct {
	JobID *string `json:"job_id,omitempty"`
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

func (s *Server) registerCodeTools() {
	addTool(s.mcpServer, "execute_code",
		withPositiveMax(inputSchema[executeCodeInput](screenshotDefaults), "timeout", freecad.DefaultMaxExecuteCodeTime), s.executeCode)
	addTool(s.mcpServer, "execute_code_async", inputSchema[codeInput](nil), s.executeCodeAsync)
	addTool(s.mcpServer, "get_async_status", inputSchema[asyncStatusInput](nil), s.getAsyncStatus)
	addTool(s.mcpServer, "execute_code_headless",
		withPositiveMax(inputSchema[headlessInput](map[string]string{"timeout": "600"}), "timeout", headless.MaxTimeout), s.executeCodeHeadless)
}

func (s *Server) executeCode(ctx context.Context, _ *mcp.CallToolRequest, in executeCodeInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure(ctx, "execute code", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "execute code", err, ""), nil, nil
	}
	res, err := conn.ExecuteCode(ctx, in.Code, in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure(ctx, "execute code", err, largerTimeout("execute_code"))), nil, nil
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
		return failure(ctx, "start async code execution", err, ""), nil, nil
	}
	res, err := conn.ExecuteCodeAsync(ctx, in.Code)
	if err != nil {
		return s.withNotice(failure(ctx, "start async code execution", err, "")), nil, nil
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
		return failure(ctx, "get async status", err, ""), nil, nil
	}
	jobID := ""
	if in.JobID != nil {
		jobID = *in.JobID
	}
	res, err := conn.GetAsyncStatus(ctx, jobID)
	if err != nil {
		return s.withNotice(failure(ctx, "get async status", err, "")), nil, nil
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

package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/headless"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
	"github.com/sairaph/mcp-wizard/render"
)

// The three code tools take the script as code or as the path of a file; the
// handlers require exactly one (scriptSource).
type executeCodeInput struct {
	Code    string   `json:"code,omitempty"`
	Path    string   `json:"path,omitempty"`
	Timeout *float64 `json:"timeout,omitempty"`
	screenshotOptions
}

type codeInput struct {
	Code string `json:"code,omitempty"`
	Path string `json:"path,omitempty"`
}

type headlessInput struct {
	Code       string   `json:"code,omitempty"`
	Path       string   `json:"path,omitempty"`
	Timeout    *float64 `json:"timeout,omitempty"`
	Background *bool    `json:"background,omitempty"`
}

// scriptSource checks that exactly one of code and path is given. A non-nil
// result is the invalid_input reply to return.
func scriptSource(code, path string) *mcp.CallToolResult {
	var msg string
	switch {
	case code != "" && path != "":
		msg = "Pass code or path, not both."
	case code == "" && path == "":
		msg = "Pass the script as code or as path."
	default:
		return nil
	}
	return render.ErrorResult(render.Error{
		Code:    render.CodeInvalidInput,
		Message: msg,
		Hint:    "Send the Python as code, or write it to a .py file and send that file's absolute path as path.",
	})
}

// addonWithoutFiles is the reply for a run from a file (path is set) that an
// addon older than protocol 8 refused because it has no execute_file method;
// nil for any other outcome.
func addonWithoutFiles(path string, err error) *mcp.CallToolResult {
	var fault *xmlrpc.Fault
	if path == "" || !errors.As(err, &fault) || !fault.MissingMethod() {
		return nil
	}
	return render.ErrorResult(render.Error{
		Code:    render.CodeUnavailable,
		Message: "FreeCAD's addon is too old to run a script from a file.",
		Hint:    "Update the addon with `" + domain.BinaryName + " install-addon`, restart FreeCAD, or pass the script as code.",
	})
}

// checkScriptFile checks that path, for a run on this computer, is the
// absolute path of a regular file. A non-nil result is the reply to return.
func checkScriptFile(path string) *mcp.CallToolResult {
	if !filepath.IsAbs(path) {
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("path %q is not an absolute path.", path),
			Hint:    "Pass the full path of the .py file on the computer running this MCP server, such as " + filepath.Join(homeDirOrDot(), "script.py") + ".",
		})
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return render.ErrorResult(render.Error{
			Code:    render.CodeNotFound,
			Message: fmt.Sprintf("'%s' does not exist.", path),
			Hint:    "Write the script to that file on the computer running this MCP server, or check the path.",
		})
	case err != nil:
		return render.ErrorResult(render.Error{Code: render.CodeInvalidInput, Message: shortMessage(fmt.Sprintf("'%s' cannot be read: %v", path, err))})
	case !info.Mode().IsRegular():
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("'%s' is not a file.", path),
			Hint:    "Pass the path of the .py file to run.",
		})
	}
	return nil
}

func homeDirOrDot() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "."
}

type asyncStatusInput struct {
	JobID *string `json:"job_id,omitempty"`
}

// autoBackgroundAfter is the timeout, in seconds, above which a headless run
// goes to the background unless background is false.
const autoBackgroundAfter = 120.0

type executeFront struct {
	Status      string `yaml:"status"`
	Transaction string `yaml:"transaction,omitempty"`
}

type asyncFront struct {
	JobID          string `yaml:"job_id,omitempty"`
	State          string `yaml:"state,omitempty"`
	Tool           string `yaml:"tool,omitempty"`
	ExitCode       *int   `yaml:"exit_code,omitempty"`
	ElapsedSeconds *int   `yaml:"elapsed_seconds,omitempty"`
	OutputFile     string `yaml:"output_file,omitempty"`
	Crashed        bool   `yaml:"crashed,omitempty"`
	TimedOut       bool   `yaml:"timed_out,omitempty"`
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
	s.registerJobTools()
	addTool(s.mcpServer, "execute_code_headless",
		withPositiveMax(inputSchema[headlessInput](nil), "timeout", headless.MaxTimeout), s.executeCodeHeadless)
}

func (s *Server) executeCode(ctx context.Context, _ *mcp.CallToolRequest, in executeCodeInput) (*mcp.CallToolResult, any, error) {
	if bad := scriptSource(in.Code, in.Path); bad != nil {
		return bad, nil, nil
	}
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure(ctx, "execute code", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "execute code", err, ""), nil, nil
	}
	var res map[string]any
	if in.Path != "" {
		res, err = conn.ExecuteFile(ctx, in.Path, in.Timeout)
	} else {
		res, err = conn.ExecuteCode(ctx, in.Code, in.Timeout)
	}
	if err != nil {
		if old := addonWithoutFiles(in.Path, err); old != nil {
			return s.withNotice(old), nil, nil
		}
		return s.withNotice(timedFailure(ctx, "execute code", err, largerTimeout("execute_code"))), nil, nil
	}
	if !succeeded(res) {
		fix := "Fix the code and retry; FreeCAD's Report View shows the traceback. "
		if in.Path != "" {
			fix = "Fix the file and run it again with the same path; FreeCAD's Report View shows the traceback. "
		}
		return s.withNotice(reportedCode("execute code", res,
			fix+"Call get_rpc_status if the GUI thread seems stuck. "+largerTimeout("execute_code"), codeFreeCAD)), nil, nil
	}
	txName, txMerged := transactionFields(res)
	out := render.SuccessResult(executeFront{Status: "ok", Transaction: txName},
		transactionNote("Code executed successfully: "+truncateOutput(str(res, "message")), txName, txMerged))
	// A screenshot is only taken after the code completed, so a failure never
	// queues a second GUI call behind a task that may still be running.
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), "")), nil, nil
}

func (s *Server) executeCodeAsync(ctx context.Context, _ *mcp.CallToolRequest, in codeInput) (*mcp.CallToolResult, any, error) {
	if bad := scriptSource(in.Code, in.Path); bad != nil {
		return bad, nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "start async code execution", err, ""), nil, nil
	}
	var res map[string]any
	if in.Path != "" {
		res, err = conn.ExecuteFileAsync(ctx, in.Path)
	} else {
		res, err = conn.ExecuteCodeAsync(ctx, in.Code)
	}
	if err != nil {
		if old := addonWithoutFiles(in.Path, err); old != nil {
			return s.withNotice(old), nil, nil
		}
		return s.withNotice(failure(ctx, "start async code execution", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("start async execution", res, "Fix the code and retry.", codeFreeCAD)), nil, nil
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
	jobID := ""
	if in.JobID != nil {
		jobID = *in.JobID
	}
	if headless.IsJobID(jobID) {
		return s.headlessJobStatus(jobID, false), nil, nil
	}
	if isCallJobID(jobID) {
		return s.callJobStatus(jobID), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		if jobID == "" {
			// Headless and call jobs do not need FreeCAD: list them anyway.
			if entries := s.serverJobEntries(); len(entries) > 0 {
				return render.SuccessResult(jobsFront{Count: len(entries)}, jsonBlock(entries)), nil, nil
			}
		}
		return failure(ctx, "get async status", err, ""), nil, nil
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
		jobs = append(jobs, s.serverJobEntries()...)
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
	if bad := scriptSource(in.Code, in.Path); bad != nil {
		return bad, nil, nil
	}
	script := headless.Script{Code: in.Code, Path: in.Path}
	if in.Path != "" {
		if bad := checkScriptFile(in.Path); bad != nil {
			return bad, nil, nil
		}
	}
	timeout := 600.0
	if in.Timeout != nil {
		timeout = *in.Timeout
	}
	if runsInBackground(in) {
		return s.startHeadlessJob(ctx, script, timeout), nil, nil
	}
	stopKeepAlive := s.keepSessionAlive(ctx, nil, "headless-foreground-"+strconv.FormatInt(time.Now().UnixNano(), 36), "Running a script (headless)")
	r := headless.RunScript(ctx, script, timeout, s.config.FreeCAD.FreecadCmd)
	stopKeepAlive()
	front := headlessFront{Success: r.Success, ExitCode: r.ReturnCode, Crashed: r.Crashed, TimedOut: r.TimedOut}
	if !r.Success {
		return s.withNotice(headlessFailure(r, timeout)), nil, nil
	}
	r.Output = truncateOutput(r.Output)
	return s.withNotice(render.SuccessResult(front, headless.Format(r))), nil, nil
}

// headlessFailure renders a failed headless run as an error reply.
func headlessFailure(r headless.Result, timeout float64) *mcp.CallToolResult {
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
	return res
}

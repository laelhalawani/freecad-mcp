package mcpserver

// Long calls move to the background. A tool that waits on FreeCAD work can keep
// the agent waiting for up to its budget (a GUI call about an hour, FEM days).
// After the limit the user sets (background_after_minutes, 30 by default) the
// call becomes a job with an id starting with call-: the agent gets that id at
// once and polls get_async_status, which returns the call's own reply once it
// ends. The work itself is never interrupted: FreeCAD cannot stop a task
// running in its window.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/headless"
	"github.com/sairaph/mcp-wizard/render"
)

// notBackgrounded are the tools that never move to the background: the ones
// that answer without FreeCAD's GUI thread (get_rpc_status, get_async_status,
// cancel_job, release_session), the ones that start or quit FreeCAD, and
// execute_code_headless, which has its own background rule. Any other call
// can queue for the GUI thread and outlast the limit.
var notBackgrounded = map[string]bool{
	"get_rpc_status":        true,
	"get_async_status":      true,
	"cancel_job":            true,
	"release_session":       true,
	"start_freecad":         true,
	"close_freecad":         true,
	"execute_code_headless": true,
}

// callJobPrefix starts the id of a call that moved to the background.
const callJobPrefix = "call-"

// isCallJobID reports whether id looks like the id of a call job.
func isCallJobID(id string) bool { return strings.HasPrefix(id, callJobPrefix) }

func newCallJobID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return callJobPrefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return callJobPrefix + hex.EncodeToString(b)
}

// callJob is one tool call that runs in a goroutine of its own, detached from
// its request, and the reply it ends with.
type callJob struct {
	id      string
	tool    string
	started time.Time
	done    chan struct{}

	// Set once, before done closes.
	finished time.Time
	result   mcp.Result
	err      error
}

// callJobs holds the calls that moved to the background. They end with the
// server, like headless jobs.
type callJobs struct {
	mu   sync.Mutex
	jobs map[string]*callJob
}

func newCallJobs() *callJobs { return &callJobs{jobs: map[string]*callJob{}} }

// add keeps job, and forgets finished jobs older than headless.KeepFor.
func (c *callJobs) add(job *callJob) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	c.jobs[job.id] = job
}

// get returns the job called id, or nil.
func (c *callJobs) get(id string) *callJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	return c.jobs[id]
}

// list returns every job, newest first.
func (c *callJobs) list() []*callJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	out := make([]*callJob, 0, len(c.jobs))
	for _, j := range c.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].started.After(out[b].started) })
	return out
}

func (c *callJobs) sweepLocked() {
	cutoff := time.Now().Add(-headless.KeepFor)
	for id, j := range c.jobs {
		if finished, ok := j.finishedAt(); ok && finished.Before(cutoff) {
			delete(c.jobs, id)
		}
	}
}

// finishedAt returns when the job ended, and false while it runs.
func (j *callJob) finishedAt() (time.Time, bool) {
	select {
	case <-j.done:
		return j.finished, true
	default:
		return time.Time{}, false
	}
}

// run makes the call and records its reply.
func (j *callJob) run(ctx context.Context, next mcp.MethodHandler, method string, req mcp.Request) {
	defer close(j.done)
	defer func() {
		// A panic in a tool must not take the server down from a goroutine the
		// SDK does not guard.
		if p := recover(); p != nil {
			j.result, j.err = nil, fmt.Errorf("%s panicked: %v", j.tool, p)
		}
		j.finished = time.Now()
	}()
	j.result, j.err = next(ctx, method, req)
}

// callResult is the reply the call ended with, as the tool would have given
// it: its result, or its error rendered as one.
func (j *callJob) callResult() *mcp.CallToolResult {
	if res, ok := j.result.(*mcp.CallToolResult); ok && res != nil {
		return res
	}
	msg := "the call ended without a reply"
	if j.err != nil {
		msg = j.err.Error()
	}
	return render.ErrorResult(render.Error{
		Code:    render.CodeInternal,
		Message: shortMessage(fmt.Sprintf("Failed to run %s: %s", j.tool, msg)),
		Hint:    statusHint,
	})
}

// backgroundAfter is how long a call may keep the agent waiting: the addon's
// setting once known, else the default.
func (s *Server) backgroundAfter() time.Duration {
	if s.backgroundAfterHook > 0 {
		return s.backgroundAfterHook
	}
	s.fc.mu.Lock()
	conn := s.fc.conn
	s.fc.mu.Unlock()
	if conn == nil {
		return time.Duration(addoninstall.DefaultBackgroundAfterMinutes) * time.Minute
	}
	return conn.BackgroundAfter()
}

// backgroundLongCalls is the receiving middleware that runs every tool call
// except the notBackgrounded ones in a goroutine, with a context that keeps its values
// (the session identity) but not the request's cancellation, and waits for it
// or for backgroundAfter, whichever comes first. A call that ends in time
// answers as it always did; one still running becomes a call job.
func (s *Server) backgroundLongCalls(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		call, ok := req.(*mcp.CallToolRequest)
		if method != "tools/call" || !ok || call.Params == nil || notBackgrounded[call.Params.Name] {
			return next(ctx, method, req)
		}
		job := &callJob{id: newCallJobID(), tool: call.Params.Name, started: time.Now(), done: make(chan struct{})}
		go job.run(context.WithoutCancel(ctx), next, method, req)
		limit := s.backgroundAfter()
		timer := time.NewTimer(limit)
		defer timer.Stop()
		select {
		case <-job.done:
			return job.result, job.err
		case <-timer.C:
			s.calls.add(job)
			return s.handedOver(job, limit, req), nil
		case <-ctx.Done():
			// The caller gave up; the call itself runs on, unread.
			return nil, ctx.Err()
		}
	}
}

// handedOver is the reply of a call that moved to the background.
func (s *Server) handedOver(job *callJob, limit time.Duration, req mcp.Request) *mcp.CallToolResult {
	elapsed := seconds(time.Since(job.started))
	text := fmt.Sprintf("%s is still running after %s, so it continues in the background as job %s. FreeCAD keeps working on it. "+
		"Call get_async_status with this job_id every minute or two; when it finishes, get_async_status returns this call's reply.",
		job.tool, formatSessionDuration(int(limit.Seconds()), math.Floor), job.id)
	return markComplete(req, render.SuccessResult(asyncFront{JobID: job.id, State: "running", Tool: job.tool, ElapsedSeconds: &elapsed}, text))
}

// resultTypeVersion is the first protocol version whose clients require every
// result to say resultType.
const resultTypeVersion = "2026-07-28"

// markComplete gives res the resultType "complete" that clients of protocol
// 2026-07-28 and later require. The SDK sets it on the result a tool handler
// returns, but not on one a middleware makes itself, and the field cannot be
// set from outside the SDK, so the reply is rebuilt through its JSON form.
// Older clients get res unchanged.
func markComplete(req mcp.Request, res *mcp.CallToolResult) *mcp.CallToolResult {
	ss, ok := req.GetSession().(*mcp.ServerSession)
	if !ok || ss == nil {
		return res
	}
	if params := ss.InitializeParams(); params == nil || params.ProtocolVersion < resultTypeVersion {
		return res
	}
	data, err := json.Marshal(res)
	if err != nil {
		return res
	}
	var wire map[string]any
	if json.Unmarshal(data, &wire) != nil {
		return res
	}
	wire["resultType"] = "complete"
	if data, err = json.Marshal(wire); err != nil {
		return res
	}
	var out mcp.CallToolResult
	if json.Unmarshal(data, &out) != nil {
		return res
	}
	return &out
}

// callJobStatus reports one call job for get_async_status: while it runs, its
// tool and elapsed time; once it ended, the reply the call gave, with the job
// id and state added to its front matter.
func (s *Server) callJobStatus(id string) *mcp.CallToolResult {
	job := s.calls.get(id)
	if job == nil {
		return render.ErrorResult(render.Error{
			Code:    render.CodeNotFound,
			Message: fmt.Sprintf("Failed to get async status: unknown job %q", id),
			Hint:    "Call get_async_status with {} to list the known jobs. Jobs end with the MCP server and are forgotten a day after they finish.",
		})
	}
	if _, done := job.finishedAt(); done {
		return withJobFront(job.callResult(), id)
	}
	elapsed := seconds(time.Since(job.started))
	text := fmt.Sprintf("Job %s (%s) is still running after %s. FreeCAD keeps working on it. Call get_async_status with this job_id again in a minute or two.",
		id, job.tool, formatSessionDuration(elapsed, math.Floor))
	return render.SuccessResult(asyncFront{JobID: id, State: "running", Tool: job.tool, ElapsedSeconds: &elapsed}, text)
}

// withJobFront returns a copy of res whose front matter starts with the job id
// and the state finished. The stored reply is left as it was, so it can be read
// again.
func withJobFront(res *mcp.CallToolResult, id string) *mcp.CallToolResult {
	front := "job_id: " + id + "\nstate: finished\n"
	out := *res
	out.Content = append([]mcp.Content(nil), res.Content...)
	out.Meta = maps.Clone(res.Meta)
	if len(out.Content) > 0 {
		if text, ok := out.Content[0].(*mcp.TextContent); ok && strings.HasPrefix(text.Text, "---\n") {
			out.Content[0] = &mcp.TextContent{Text: "---\n" + front + strings.TrimPrefix(text.Text, "---\n")}
			return &out
		}
	}
	out.Content = append([]mcp.Content{&mcp.TextContent{Text: "---\n" + front + "---\n"}}, out.Content...)
	return &out
}

// serverJobEntries lists the jobs this server holds (headless and call jobs)
// for get_async_status.
func (s *Server) serverJobEntries() []any {
	out := s.headlessJobEntries()
	for _, job := range s.calls.list() {
		entry := map[string]any{"id": job.id, "kind": "call", "tool": job.tool, "state": "running", "elapsed_seconds": seconds(time.Since(job.started))}
		if finished, done := job.finishedAt(); done {
			entry["state"] = "finished"
			entry["elapsed_seconds"] = seconds(finished.Sub(job.started))
		}
		out = append(out, entry)
	}
	return out
}

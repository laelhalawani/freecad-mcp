package mcpserver

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/headless"
	"github.com/sairaph/mcp-wizard/render"
)

// keepAliveEvery is how often the server tells the addon that the session of a
// running headless job is still working.
var keepAliveEvery = 20 * time.Second

// keepSessionAlive keeps the session lock of ctx's session from expiring while
// a headless run goes on: at once and then every keepAliveEvery it makes the keep_alive call,
// which counts as activity of the lock's holder and claims nothing. It runs
// until the returned stop is called or done closes. Best effort: it does
// nothing while remote access is off or FreeCAD was never reached, and it never
// dials FreeCAD itself.
func (s *Server) keepSessionAlive(ctx context.Context, done <-chan struct{}, id, phrase string) (stop func()) {
	callCtx := context.WithoutCancel(ctx) // keeps the session headers
	started := time.Now()
	quit := make(chan struct{})
	go func() {
		s.sendKeepAlive(callCtx, id, phrase, started, false)
		t := time.NewTicker(keepAliveEvery)
		defer t.Stop()
		for {
			select {
			case <-quit:
				s.sendKeepAlive(callCtx, id, phrase, started, true)
				return
			case <-done:
				s.sendKeepAlive(callCtx, id, phrase, started, true)
				return
			case <-t.C:
				s.sendKeepAlive(callCtx, id, phrase, started, false)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(quit) }) }
}

// sendKeepAlive makes one keep_alive call on the connection FreeCAD already
// answered on. It also tells the banner over FreeCAD's 3D view which job runs
// (id, phrase) and for how long, or that it ended.
func (s *Server) sendKeepAlive(ctx context.Context, id, phrase string, started time.Time, ending bool) {
	s.fc.mu.Lock()
	conn := s.fc.conn
	s.fc.mu.Unlock()
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	_, _ = conn.KeepSessionAlive(ctx, id, phrase, time.Since(started).Seconds(), ending)
}

// startHeadlessJob starts code in the background and replies with its job id.
func (s *Server) startHeadlessJob(ctx context.Context, code string, timeout float64) *mcp.CallToolResult {
	job, msg := s.jobs.Start(ctx, code, timeout, s.config.FreeCAD.FreecadCmd)
	if msg != "" {
		return s.withNotice(headlessFailure(headless.Result{Error: msg}, timeout))
	}
	s.keepSessionAlive(ctx, job.Done(), job.ID, "Running a script in the background")
	text := fmt.Sprintf("Headless FreeCAD script started in the background (job_id: %s). It streams its output to '%s' "+
		"and is stopped after %g s. Poll get_async_status with {\"job_id\": %q} every 30 to 60 seconds for its state and "+
		"the last %d lines of its output; call cancel_job with {\"job_id\": %q} to stop it.",
		job.ID, job.OutputFile(), timeout, job.ID, headless.TailLines, job.ID)
	return s.withNotice(render.SuccessResult(asyncFront{JobID: job.ID, State: headless.StateRunning, OutputFile: job.OutputFile()}, text))
}

func seconds(d time.Duration) int { return int(math.Round(d.Seconds())) }

// headlessJobEntries lists the headless jobs for get_async_status.
func (s *Server) headlessJobEntries() []any {
	var out []any
	for _, snap := range s.jobs.Snapshots() {
		entry := map[string]any{"id": snap.ID, "kind": "headless", "state": snap.State, "elapsed_seconds": seconds(snap.Elapsed)}
		if snap.ExitCode != nil {
			entry["exit_code"] = *snap.ExitCode
		}
		out = append(out, entry)
	}
	return out
}

// runsInBackground says whether an execute_code_headless call runs in the
// background: when background is true, or when a timeout over
// autoBackgroundAfter is passed and background is not given. An omitted
// timeout runs in the foreground.
func runsInBackground(in headlessInput) bool {
	if in.Background != nil {
		return *in.Background
	}
	return in.Timeout != nil && *in.Timeout > autoBackgroundAfter
}

type cancelJobInput struct {
	JobID string `json:"job_id"`
}

func (s *Server) registerJobTools() {
	addTool(s.mcpServer, "cancel_job", inputSchema[cancelJobInput](nil), s.cancelJob)
}

func (s *Server) cancelJob(_ context.Context, _ *mcp.CallToolRequest, in cancelJobInput) (*mcp.CallToolResult, any, error) {
	if !headless.IsJobID(in.JobID) {
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("cancel_job stops a background headless job; %q is not one.", in.JobID),
			Hint:    "Call get_async_status with {} to list the jobs, then call cancel_job with the job_id of a headless job.",
		}), nil, nil
	}
	return s.headlessJobStatus(in.JobID, true), nil, nil
}

// headlessJobStatus reports one background headless job, stopping it first
// when cancel is set.
func (s *Server) headlessJobStatus(id string, cancel bool) *mcp.CallToolResult {
	job := s.jobs.Get(id)
	if job == nil {
		return render.ErrorResult(render.Error{
			Code:    render.CodeNotFound,
			Message: fmt.Sprintf("Failed to get async status: unknown headless job %q", id),
			Hint:    "Call get_async_status with {} to list the known jobs. Jobs end with the MCP server and are forgotten a day after they finish.",
		})
	}
	stopped := false
	if cancel {
		stopped = job.Cancel()
	}
	snap := job.Snapshot()
	elapsed := seconds(snap.Elapsed)
	front := asyncFront{JobID: snap.ID, State: snap.State, ExitCode: snap.ExitCode, ElapsedSeconds: &elapsed,
		Crashed: snap.Crashed, TimedOut: snap.TimedOut}
	var text strings.Builder
	switch {
	case snap.State == headless.StateRunning:
		front.OutputFile = snap.OutputFile
		fmt.Fprintf(&text, "Headless job %s: running for %d s (stopped after %g s at most).", snap.ID, elapsed, snap.Timeout)
	case snap.State == headless.StateCancelled:
		fmt.Fprintf(&text, "Headless job %s: cancelled after %d s.", snap.ID, elapsed)
	case snap.Success:
		fmt.Fprintf(&text, "Headless job %s: finished (exit 0) after %d s.", snap.ID, elapsed)
	default:
		fmt.Fprintf(&text, "Headless job %s: FAILED after %d s: %s", snap.ID, elapsed, snap.Error)
	}
	if cancel && !stopped && snap.State != headless.StateCancelled {
		text.WriteString(" It had already finished, so there was nothing to cancel.")
	}
	if snap.Output != "" {
		fmt.Fprintf(&text, "\n\nOutput (last %d lines):\n%s", headless.TailLines, textBlock(truncateOutput(snap.Output)))
	} else {
		text.WriteString("\n\nIt has printed nothing yet.")
	}
	if snap.State == headless.StateRunning {
		text.WriteString("\n\nPoll again in 30 to 60 seconds, or call cancel_job with {\"job_id\": \"" + snap.ID + "\"} to stop it.")
	} else if snap.Removed {
		text.WriteString("\n\nThe output file was removed after this read; later calls repeat the output above.")
	} else if snap.State == headless.StateFinished && snap.Success {
		text.WriteString("\n\nIf the script saved a document that is open in the GUI, call reload_document to see the result.")
	}
	return render.SuccessResult(front, text.String())
}

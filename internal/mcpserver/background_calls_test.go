package mcpserver

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/headless"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc/xmlrpctest"
)

var callJobID = regexp.MustCompile(`call-[0-9a-f]{8}`)

func allText(res *mcp.CallToolResult) string { return strings.Join(texts(res), "\n") }

// backgroundServer starts a server whose background limit is limit and whose
// fake addon runs execute_code: a script named slow or boom waits until
// release is closed, then succeeds or reports an error; any other returns at once.
func backgroundServer(t *testing.T, limit time.Duration, release chan struct{}) (*mcp.ClientSession, *xmlrpctest.Server) {
	t.Helper()
	return backgroundServerAt(t, limit, release, "")
}

// backgroundServerAt is backgroundServer for a client that asks for the given
// protocol version ("" keeps the SDK's default).
func backgroundServerAt(t *testing.T, limit time.Duration, release chan struct{}, protocol string) (*mcp.ClientSession, *xmlrpctest.Server) {
	t.Helper()
	fake := addon(t, map[string]xmlrpctest.Handler{
		"execute_code": func(p []any) (any, error) {
			code, _ := p[0].(string)
			switch code {
			case "slow":
				<-release
			case "boom":
				<-release
				return map[string]any{"success": false, "error": "NameError: name 'x' is not defined"}, nil
			}
			return map[string]any{"success": true, "message": "Python code executed successfully.\nOutput: " + code + "\n"}, nil
		},
		"get_async_status": func([]any) (any, error) { return map[string]any{"success": true, "jobs": []any{}}, nil },
		"get_documents": func([]any) (any, error) {
			<-release
			return []any{}, nil
		},
	})
	srv := New(Config{Version: "1.2.3", FreeCAD: settingsFor(fake)})
	srv.backgroundAfterHook = limit
	if protocol == "" {
		return sessionOf(t, srv), fake
	}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.MCPServer().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, fake
}

// waitForReply polls get_async_status for job until its reply is a finished one.
func waitForReply(t *testing.T, cs *mcp.ClientSession, job string) *mcp.CallToolResult {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := call(t, cs, "get_async_status", map[string]any{"job_id": job})
		if strings.Contains(allText(res), "state: finished") {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not finish; last reply: %s", job, allText(res))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLongCallMovesToTheBackgroundAndItsReplyIsKept(t *testing.T) {
	release := make(chan struct{})
	released := false
	releaseAll := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer releaseAll()
	cs, _ := backgroundServer(t, 200*time.Millisecond, release)

	// A call that outlasts the limit answers with a job id, not with an error.
	handover := call(t, cs, "execute_code", map[string]any{"code": "slow"})
	text := allText(handover)
	id := callJobID.FindString(text)
	if handover.IsError || id == "" || !strings.Contains(text, "state: running") || !strings.Contains(text, "tool: execute_code") ||
		!strings.Contains(text, "continues in the background as job "+id) {
		t.Fatalf("handover reply (error %v): %s", handover.IsError, text)
	}

	running := allText(call(t, cs, "get_async_status", map[string]any{"job_id": id}))
	if !strings.Contains(running, "job_id: "+id) || !strings.Contains(running, "state: running") || !strings.Contains(running, "still running") {
		t.Fatalf("running status: %s", running)
	}
	listed := allText(call(t, cs, "get_async_status", map[string]any{}))
	if !strings.Contains(listed, id) || !strings.Contains(listed, `"kind": "call"`) {
		t.Fatalf("job list does not show the call job: %s", listed)
	}
	cancel := call(t, cs, "cancel_job", map[string]any{"job_id": id})
	if !cancel.IsError || !strings.Contains(allText(cancel), "cannot stop work already running") {
		t.Fatalf("cancel_job on a call job: error %v: %s", cancel.IsError, allText(cancel))
	}

	// A second call that fails after the handover.
	boom := callJobID.FindString(allText(call(t, cs, "execute_code", map[string]any{"code": "boom", "include_screenshot": false})))
	if boom == "" || boom == id {
		t.Fatalf("second call got job id %q", boom)
	}

	releaseAll()
	finished := waitForReply(t, cs, id)
	if finished.IsError || !strings.Contains(allText(finished), "job_id: "+id) || !strings.Contains(allText(finished), "Output: slow") {
		t.Fatalf("finished reply (error %v): %s", finished.IsError, allText(finished))
	}
	if images(finished) != 1 {
		t.Fatalf("the finished reply holds %d screenshots, want the one the call took", images(finished))
	}
	// The reply stays readable, unchanged.
	if again := allText(call(t, cs, "get_async_status", map[string]any{"job_id": id})); again != allText(finished) {
		t.Fatalf("second read differs:\n%s\nfirst:\n%s", again, allText(finished))
	}

	failed := waitForReply(t, cs, boom)
	if !failed.IsError || !strings.Contains(allText(failed), "job_id: "+boom) || !strings.Contains(allText(failed), "NameError") {
		t.Fatalf("failed call's reply (error %v): %s", failed.IsError, allText(failed))
	}
}

func TestCallThatEndsInTimeAnswersDirectly(t *testing.T) {
	cs, _ := backgroundServer(t, 5*time.Second, make(chan struct{}))
	res := call(t, cs, "execute_code", map[string]any{"code": "fast", "include_screenshot": false})
	text := allText(res)
	if res.IsError || strings.Contains(text, "call-") || strings.Contains(text, "job_id") || !strings.Contains(text, "Output: fast") {
		t.Fatalf("reply (error %v): %s", res.IsError, text)
	}
}

func TestUnknownCallJobIsNotFound(t *testing.T) {
	cs, _ := backgroundServer(t, time.Second, make(chan struct{}))
	res := call(t, cs, "get_async_status", map[string]any{"job_id": "call-00000000"})
	if !res.IsError || !strings.Contains(allText(res), "not_found") {
		t.Fatalf("reply (error %v): %s", res.IsError, allText(res))
	}
}

func TestOnlyTheListedToolsStayOutOfTheBackground(t *testing.T) {
	want := []string{"cancel_job", "close_freecad", "execute_code_headless", "get_async_status", "get_rpc_status", "release_session", "start_freecad"}
	var got []string
	for name := range notBackgrounded {
		if _, ok := toolTexts[name]; !ok {
			t.Errorf("notBackgrounded lists %q, which is not a tool", name)
		}
		got = append(got, name)
	}
	if !reflect.DeepEqual(sortStrings(got), want) {
		t.Fatalf("tools kept out of the background: %v, want %v", sortStrings(got), want)
	}
}

func TestEveryOtherToolMovesToTheBackground(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	cs, _ := backgroundServer(t, 200*time.Millisecond, release)
	// list_documents is a quick tool that queues for the GUI thread like any other.
	res := call(t, cs, "list_documents", map[string]any{})
	if res.IsError || !callJobID.MatchString(allText(res)) || !strings.Contains(allText(res), "tool: list_documents") {
		t.Fatalf("list_documents (error %v): %s", res.IsError, allText(res))
	}
}

func TestReplyToAClientOfProtocol20260728KeepsResultType(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	cs, _ := backgroundServerAt(t, 200*time.Millisecond, release, "2026-07-28")
	handover := call(t, cs, "execute_code", map[string]any{"code": "slow", "include_screenshot": false})
	id := callJobID.FindString(allText(handover))
	raw, err := json.Marshal(handover)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || !strings.Contains(string(raw), `"resultType":"complete"`) {
		t.Fatalf("handover reply = %s", raw)
	}
	running, _ := json.Marshal(call(t, cs, "get_async_status", map[string]any{"job_id": id}))
	if !strings.Contains(string(running), `"resultType":"complete"`) {
		t.Fatalf("running status = %s", running)
	}
	released = true
	close(release)
	stored, _ := json.Marshal(waitForReply(t, cs, id))
	if !strings.Contains(string(stored), `"resultType":"complete"`) || !strings.Contains(string(stored), "Output: slow") {
		t.Fatalf("stored reply = %s", stored)
	}
}

func TestDetachedCallKeepsTheSessionIdentity(t *testing.T) {
	release := make(chan struct{})
	cs, fake := backgroundServer(t, 200*time.Millisecond, release)
	handover := call(t, cs, "execute_code", map[string]any{"code": "slow"})
	id := callJobID.FindString(allText(handover))
	close(release)
	waitForReply(t, cs, id)
	// The addon calls made for this tool call: the script, and the screenshot
	// that is only taken after the call has moved to the background.
	for _, method := range []string{"execute_code", "get_active_screenshot"} {
		calls := fake.CallsTo(method)
		if len(calls) != 1 {
			t.Fatalf("%s was called %d times", method, len(calls))
		}
		session, client := calls[0].Header.Get(domain.HeaderSession), calls[0].Header.Get(domain.HeaderClient)
		if !strings.HasPrefix(session, instanceID) || !strings.HasPrefix(client, "test on ") {
			t.Errorf("%s carried session %q, client %q; want this server's session and the client's label", method, session, client)
		}
	}
}

func TestRequestCancelledBeforeTheLimitLeavesNoJobAndDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	cs, _ := backgroundServer(t, 30*time.Second, release)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "execute_code", Arguments: map[string]any{"code": "slow"}})
		finished <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("the cancelled call returned a result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled call never returned")
	}
	time.Sleep(200 * time.Millisecond)
	if listed := allText(call(t, cs, "get_async_status", map[string]any{})); strings.Contains(listed, "call-") {
		t.Fatalf("a cancelled request left a job: %s", listed)
	}
	// The server still answers other calls.
	if res := call(t, cs, "execute_code", map[string]any{"code": "fast", "include_screenshot": false}); res.IsError || !strings.Contains(allText(res), "Output: fast") {
		t.Fatalf("call after the cancel (error %v): %s", res.IsError, allText(res))
	}
}

func TestCallThatPanicsIsStoredAsAnErrorReply(t *testing.T) {
	job := &callJob{id: "call-panic000", tool: "execute_code", started: time.Now(), done: make(chan struct{})}
	go job.run(context.Background(), func(context.Context, string, mcp.Request) (mcp.Result, error) {
		panic("tool bug")
	}, "tools/call", nil)
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("a panicking call never finished its job")
	}
	if _, done := job.finishedAt(); !done {
		t.Fatal("the job is not marked finished")
	}
	res := job.callResult()
	text := allText(res)
	if !res.IsError || !strings.Contains(text, "execute_code panicked: tool bug") || !strings.Contains(text, "internal_error") {
		t.Fatalf("stored reply (error %v): %s", res.IsError, text)
	}
}

func TestFinishedCallJobsAreForgottenAfterADay(t *testing.T) {
	jobs := newCallJobs()
	finished := func(id string, ago time.Duration) *callJob {
		j := &callJob{id: id, tool: "execute_code", started: time.Now().Add(-ago - time.Minute), done: make(chan struct{})}
		j.finished = time.Now().Add(-ago)
		close(j.done)
		return j
	}
	running := &callJob{id: "call-running0", tool: "execute_code", started: time.Now().Add(-48 * time.Hour), done: make(chan struct{})}
	jobs.add(finished("call-old00000", headless.KeepFor+time.Hour))
	jobs.add(finished("call-recent00", time.Hour))
	jobs.add(running)
	if jobs.get("call-old00000") != nil {
		t.Error("a job finished over a day ago is still kept")
	}
	if jobs.get("call-recent00") == nil || jobs.get("call-running0") == nil {
		t.Error("a recent or a running job was forgotten")
	}
}

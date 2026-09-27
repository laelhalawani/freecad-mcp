package xmlrpc_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/laelhalawani/freecad-mcp/internal/xmlrpc"
	"github.com/laelhalawani/freecad-mcp/internal/xmlrpc/xmlrpctest"
)

func TestClientSendsTheTokenWithoutExposingIt(t *testing.T) {
	srv := xmlrpctest.New(t, map[string]xmlrpctest.Handler{
		"ping": func([]any) (any, error) { return true, nil },
	})
	srv.Token = "s3cret-token"

	right := xmlrpc.NewClient(srv.Host, srv.Port, "s3cret-token")
	if v, err := right.Call(context.Background(), 5*time.Second, "ping"); err != nil || v != true {
		t.Fatalf("ping with the right token = %v, %v", v, err)
	}

	wrong := xmlrpc.NewClient(srv.Host, srv.Port, "wrong-token")
	_, err := wrong.Call(context.Background(), 5*time.Second, "ping")
	var perr *xmlrpc.ProtocolError
	if !errors.As(err, &perr) || perr.StatusCode != 401 {
		t.Fatalf("ping with a wrong token: %v, want HTTP 401", err)
	}
	// Tools return error text to the model, so it must not carry the token.
	if strings.Contains(err.Error(), "wrong-token") || strings.Contains(wrong.URL(), "wrong-token") {
		t.Fatalf("the error or URL exposes the token: %v / %s", err, wrong.URL())
	}
	if len(srv.CallsTo("ping")) != 1 {
		t.Fatalf("the rejected request reached the handler: %v", srv.Calls())
	}
}

func TestClientTimesOut(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := xmlrpctest.New(t, map[string]xmlrpctest.Handler{
		"slow": func([]any) (any, error) { <-release; return true, nil },
	})
	c := xmlrpc.NewClient(srv.Host, srv.Port, "")
	start := time.Now()
	if _, err := c.Call(context.Background(), 200*time.Millisecond, "slow"); err == nil {
		t.Fatal("a call past its timeout succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the timeout took %v", elapsed)
	}
}

func TestStatusCallIsNotQueuedBehindABlockedCall(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := xmlrpctest.New(t, map[string]xmlrpctest.Handler{
		"execute_code_async": func([]any) (any, error) {
			close(entered)
			<-release
			return map[string]any{"success": true, "job_id": "job-1"}, nil
		},
		"get_async_status": func(p []any) (any, error) {
			return map[string]any{"success": true, "job": map[string]any{"id": p[0], "state": "running"}}, nil
		},
	})
	c := xmlrpc.NewClient(srv.Host, srv.Port, "")
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), 5*time.Second, "execute_code_async", "pass")
		done <- err
	}()
	<-entered
	v, err := c.Call(context.Background(), 2*time.Second, "get_async_status", "job-1")
	if err != nil {
		t.Fatalf("status while blocked: %v", err)
	}
	if m, _ := v.(map[string]any); m["success"] != true {
		t.Fatalf("status = %#v", v)
	}
	select {
	case <-done:
		t.Fatal("the blocked call finished before it was released")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("blocked call: %v", err)
	}
}

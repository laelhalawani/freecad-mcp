package freecad_test

import (
	"context"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc/xmlrpctest"
)

func ptr[T any](v T) *T { return &v }

func matchingStatus(overrides map[string]any) map[string]any {
	s := map[string]any{
		"success":                  true,
		"addon_version":            "0.1.24",
		"protocol_version":         domain.ProtocolVersion,
		"execute_code_timeout":     90,
		"max_execute_code_timeout": 1800,
	}
	for k, v := range overrides {
		s[k] = v
	}
	return s
}

func statusServer(t *testing.T, status map[string]any) *xmlrpctest.Server {
	return xmlrpctest.New(t, map[string]xmlrpctest.Handler{
		"ping":           func([]any) (any, error) { return true, nil },
		"get_rpc_status": func([]any) (any, error) { return status, nil },
	})
}

func connect(srv *xmlrpctest.Server) *freecad.Connection {
	return freecad.NewConnection(srv.Host, srv.Port, "", 2*time.Second)
}

func TestExecuteCodeBudgets(t *testing.T) {
	c := freecad.NewConnection("localhost", 1, "", 150*time.Second)
	cases := []struct {
		timeout *float64
		run     float64
		wait    time.Duration
	}{
		// Queue budget plus run budget: 2 * 90 s plus the 30 s margin.
		{nil, 90, 210 * time.Second},
		// A 600 s GUI task must not be cut off by the 150 s default.
		{ptr(600.0), 600, 1230 * time.Second},
		{ptr(10.0), 10, 150 * time.Second},
		{ptr(1e9), 1800, 3630 * time.Second},
	}
	for _, tc := range cases {
		run, wait, err := c.ExecuteCodeBudget(tc.timeout)
		if err != nil || run != tc.run || wait != tc.wait {
			t.Errorf("budget(%v) = %v, %v, %v; want %v, %v", tc.timeout, run, wait, err, tc.run, tc.wait)
		}
	}
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, _, err := c.ExecuteCodeBudget(&bad); err != freecad.ErrInvalidTimeout {
			t.Errorf("budget(%v) err = %v", bad, err)
		}
	}
}

func TestExecuteCodeArguments(t *testing.T) {
	srv := xmlrpctest.New(t, map[string]xmlrpctest.Handler{
		"execute_code": func([]any) (any, error) { return map[string]any{"success": true, "message": "ok"}, nil },
	})
	c := connect(srv)
	if _, err := c.ExecuteCode(context.Background(), "x = 1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecuteCode(context.Background(), "x = 1", ptr(600.0)); err != nil {
		t.Fatal(err)
	}
	calls := srv.CallsTo("execute_code")
	// Without a timeout one argument goes out, so older addons keep working.
	if !reflect.DeepEqual(calls[0].Params, []any{"x = 1"}) {
		t.Errorf("without timeout sent %#v", calls[0].Params)
	}
	if !reflect.DeepEqual(calls[1].Params, []any{"x = 1", 600.0}) {
		t.Errorf("with timeout sent %#v", calls[1].Params)
	}
	if _, err := c.ExecuteCode(context.Background(), "raise AssertionError", ptr(0.0)); err != freecad.ErrInvalidTimeout {
		t.Fatalf("invalid timeout err = %v", err)
	}
	if n := len(srv.CallsTo("execute_code")); n != 2 {
		t.Fatalf("an invalid timeout reached the addon (%d calls)", n)
	}
}

func TestAddonWithoutGetRPCStatusIsReportedAsOld(t *testing.T) {
	srv := xmlrpctest.New(t, map[string]xmlrpctest.Handler{"ping": func([]any) (any, error) { return true, nil }})
	w := connect(srv).CheckAddonVersion(context.Background(), "1.0.0")
	if !strings.Contains(w, "no get_rpc_status") || !strings.Contains(w, "Update the addon") {
		t.Fatalf("warning = %q", w)
	}
}

func TestAddonWithoutVersionFieldsIsReportedAsOld(t *testing.T) {
	srv := statusServer(t, map[string]any{"success": true, "rpc_server": "running"})
	w := connect(srv).CheckAddonVersion(context.Background(), "1.0.0")
	if !strings.Contains(w, "does not report a version") || !strings.Contains(w, "Update the addon") {
		t.Fatalf("warning = %q", w)
	}
}

func TestFailingGetRPCStatusIsNotAnOldAddon(t *testing.T) {
	srv := xmlrpctest.New(t, map[string]xmlrpctest.Handler{
		"get_rpc_status": func([]any) (any, error) {
			return nil, &xmlrpc.Fault{Code: 1, String: "<class 'RuntimeError'>:status exploded"}
		},
	})
	c := connect(srv)
	if w := c.CheckAddonVersion(context.Background(), "1.0.0"); w != "" {
		t.Fatalf("warning = %q", w)
	}
	if c.ExecuteCodeTimeout != 90 {
		t.Fatalf("budget changed to %v", c.ExecuteCodeTimeout)
	}
}

func TestProtocolMismatchNamesTheSideToUpdate(t *testing.T) {
	for protocol, fix := range map[int]string{domain.ProtocolVersion - 1: "Update the addon", domain.ProtocolVersion + 1: "Update the MCP server"} {
		srv := statusServer(t, matchingStatus(map[string]any{"addon_version": "9.9.9", "protocol_version": protocol}))
		w := connect(srv).CheckAddonVersion(context.Background(), "1.0.0")
		want := "FreeCAD addon 9.9.9 (protocol " + strconv.Itoa(protocol) + ") does not match"
		if !strings.Contains(w, want) || !strings.Contains(w, fix) {
			t.Errorf("protocol %d: warning = %q", protocol, w)
		}
	}
}

func TestBooleanProtocolIsNotAVersion(t *testing.T) {
	w := freecad.AddonVersionWarning(matchingStatus(map[string]any{"protocol_version": true}), "1.0.0")
	if !strings.Contains(w, "does not report a version") {
		t.Fatalf("warning = %q", w)
	}
}

func TestMatchingAddonGivesNoWarning(t *testing.T) {
	status := matchingStatus(nil)
	status["protocol_version"] = int64(domain.ProtocolVersion)
	if w := freecad.AddonVersionWarning(status, "1.0.0"); w != "" {
		t.Fatalf("warning = %q", w)
	}
}

func TestClientAdoptsTheAddonsBudgets(t *testing.T) {
	srv := statusServer(t, matchingStatus(map[string]any{"execute_code_timeout": 45, "max_execute_code_timeout": 600}))
	c := connect(srv)
	if w := c.CheckAddonVersion(context.Background(), "1.0.0"); w != "" {
		t.Fatalf("warning = %q", w)
	}
	if c.ExecuteCodeTimeout != 45 || c.MaxExecuteCodeTimeout != 600 {
		t.Fatalf("budgets = %v, %v", c.ExecuteCodeTimeout, c.MaxExecuteCodeTimeout)
	}
	if other := freecad.NewConnection("localhost", 1, "", 0); other.ExecuteCodeTimeout != 90 {
		t.Fatalf("defaults changed for other connections: %v", other.ExecuteCodeTimeout)
	}
}

func TestClientIgnoresInvalidBudgets(t *testing.T) {
	for _, bad := range []any{0, -5, true, "90", nil, 1801, 1e9, 1e308} {
		srv := statusServer(t, matchingStatus(map[string]any{"execute_code_timeout": bad, "max_execute_code_timeout": bad}))
		c := connect(srv)
		c.CheckAddonVersion(context.Background(), "1.0.0")
		if c.ExecuteCodeTimeout != 90 || c.MaxExecuteCodeTimeout != 1800 {
			t.Errorf("budget %#v adopted: %v, %v", bad, c.ExecuteCodeTimeout, c.MaxExecuteCodeTimeout)
		}
	}
}

func TestClientAdoptsABudgetUpToItsOwnCeiling(t *testing.T) {
	c := connect(statusServer(t, matchingStatus(map[string]any{"execute_code_timeout": 1800})))
	c.CheckAddonVersion(context.Background(), "1.0.0")
	if c.ExecuteCodeTimeout != 1800 {
		t.Fatalf("budget = %v", c.ExecuteCodeTimeout)
	}
}

func TestUnreachableAddonDoesNotBlockTheCheck(t *testing.T) {
	c := freecad.NewConnection("127.0.0.1", 9, "", 500*time.Millisecond)
	if w := c.CheckAddonVersion(context.Background(), "1.0.0"); w != "" {
		t.Fatalf("warning = %q", w)
	}
}

func TestHungAddonCannotHoldUpTheCheck(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := xmlrpctest.New(t, map[string]xmlrpctest.Handler{
		"get_rpc_status": func([]any) (any, error) { <-release; return matchingStatus(nil), nil },
	})
	// The connection timeout stays long; only the check is short.
	c := freecad.NewConnection(srv.Host, srv.Port, "", 150*time.Second)
	c.VersionCheckTimeout = 200 * time.Millisecond
	start := time.Now()
	if w := c.CheckAddonVersion(context.Background(), "1.0.0"); w != "" {
		t.Fatalf("warning = %q", w)
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("the check took %v", elapsed)
	}
}

func TestScreenshotSendsNilForOmittedArguments(t *testing.T) {
	srv := xmlrpctest.New(t, map[string]xmlrpctest.Handler{
		"get_active_screenshot": func([]any) (any, error) { return nil, nil },
	})
	c := connect(srv)
	shot, err := c.GetActiveScreenshot(context.Background(), "Top", nil, ptr(200), nil, "")
	if err != nil || shot.Image != "" {
		t.Fatalf("screenshot = %+v, %v", shot, err)
	}
	if got := srv.CallsTo("get_active_screenshot")[0].Params; !reflect.DeepEqual(got, []any{"Top", nil, int64(200), nil}) {
		t.Fatalf("sent %#v", got)
	}
}

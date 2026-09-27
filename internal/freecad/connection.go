// Package freecad talks to the FreeCAD addon's XML-RPC server.
package freecad

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/laelhalawani/freecad-mcp/internal/xmlrpc"
)

// Budget defaults. The addon reports its own run budgets through
// get_rpc_status; CheckAddonVersion adopts them.
const (
	DefaultTimeout             = 150 * time.Second
	DefaultExecuteCodeTimeout  = 90.0   // seconds
	DefaultMaxExecuteCodeTime  = 1800.0 // seconds
	DefaultRPCTimeoutMargin    = 30.0   // seconds
	DefaultVersionCheckTimeout = 5 * time.Second
)

// Connection is a client for one FreeCAD addon.
type Connection struct {
	rpc     *xmlrpc.Client
	timeout time.Duration

	// Run budgets in seconds. get_rpc_status never waits for the GUI thread,
	// so a healthy addon answers the version check at once; a hung one must
	// not hold up the session for the full connection timeout.
	ExecuteCodeTimeout    float64
	MaxExecuteCodeTimeout float64
	RPCTimeoutMargin      float64
	VersionCheckTimeout   time.Duration
}

// NewConnection returns a connection to the addon at host:port. timeout is
// the default wait for a reply; long calls widen it to cover their budgets.
func NewConnection(host string, port int, token string, timeout time.Duration) *Connection {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Connection{
		rpc:                   xmlrpc.NewClient(host, port, token),
		timeout:               timeout,
		ExecuteCodeTimeout:    DefaultExecuteCodeTimeout,
		MaxExecuteCodeTimeout: DefaultMaxExecuteCodeTime,
		RPCTimeoutMargin:      DefaultRPCTimeoutMargin,
		VersionCheckTimeout:   DefaultVersionCheckTimeout,
	}
}

// URL returns the addon endpoint without credentials.
func (c *Connection) URL() string { return c.rpc.URL() }

// Close releases pooled connections.
func (c *Connection) Close() { c.rpc.CloseIdle() }

func (c *Connection) call(ctx context.Context, timeout time.Duration, method string, params ...any) (any, error) {
	return c.rpc.Call(ctx, timeout, method, params...)
}

func (c *Connection) callMap(ctx context.Context, timeout time.Duration, method string, params ...any) (map[string]any, error) {
	v, err := c.call(ctx, timeout, method, params...)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected reply of type %T", method, v)
	}
	return m, nil
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Ping reports whether the addon answers.
func (c *Connection) Ping(ctx context.Context) (bool, error) {
	v, err := c.call(ctx, c.timeout, "ping")
	if err != nil {
		return false, err
	}
	ok, _ := v.(bool)
	return ok, nil
}

// GetRPCStatus returns the addon's GUI-independent health report.
func (c *Connection) GetRPCStatus(ctx context.Context) (any, error) {
	return c.call(ctx, c.timeout, "get_rpc_status")
}

// IsBudget reports whether v is a number of seconds in (0, ceiling].
func IsBudget(v any, ceiling float64) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case int64:
		f = float64(x)
	case float64:
		f = x
	default:
		return 0, false
	}
	if math.IsNaN(f) || f <= 0 || f > ceiling {
		return 0, false
	}
	return f, true
}

// CheckAddonVersion compares the addon's version with this server's and
// adopts the run budgets the addon reports. It returns a warning for an old
// or mismatched addon, and "" otherwise, including when the addon cannot be
// asked.
func (c *Connection) CheckAddonVersion(ctx context.Context, serverVersion string) string {
	v, err := c.call(ctx, c.VersionCheckTimeout, "get_rpc_status")
	if err != nil {
		var fault *xmlrpc.Fault
		if errors.As(err, &fault) && fault.MissingMethod() {
			return AddonVersionWarning(nil, serverVersion)
		}
		// Stdout carries the MCP protocol, so diagnostics go to stderr.
		fmt.Fprintf(os.Stderr, "freecad-mcp: could not check the FreeCAD addon version: %v\n", err)
		return ""
	}
	status, ok := v.(map[string]any)
	if !ok {
		return AddonVersionWarning(map[string]any{}, serverVersion)
	}
	// Socket timeouts derive from these budgets, so this client's own ceiling
	// bounds how long an addon's report can make it wait.
	ceiling := DefaultMaxExecuteCodeTime
	if f, ok := IsBudget(status["execute_code_timeout"], ceiling); ok {
		c.ExecuteCodeTimeout = f
	}
	if f, ok := IsBudget(status["max_execute_code_timeout"], ceiling); ok {
		c.MaxExecuteCodeTimeout = f
	}
	return AddonVersionWarning(status, serverVersion)
}

// ErrInvalidTimeout rejects a timeout that is not a positive finite number.
var ErrInvalidTimeout = errors.New("timeout must be a positive finite number")

// ExecuteCodeBudget returns the run budget sent to the addon and the time to
// wait for the reply. The addon permits a full queue budget followed by a
// full run budget, so the wait must outlast both.
func (c *Connection) ExecuteCodeBudget(timeout *float64) (run float64, wait time.Duration, err error) {
	run = c.ExecuteCodeTimeout
	if timeout != nil {
		t := *timeout
		if math.IsNaN(t) || math.IsInf(t, 0) || t <= 0 {
			return 0, 0, ErrInvalidTimeout
		}
		run = math.Min(t, c.MaxExecuteCodeTimeout)
	}
	wait = max(c.timeout, seconds(2*run+c.RPCTimeoutMargin))
	return run, wait, nil
}

// ExecuteCode runs Python code on FreeCAD's GUI thread. Without a timeout it
// sends one argument, so it keeps working with addons that predate the
// timeout parameter.
func (c *Connection) ExecuteCode(ctx context.Context, code string, timeout *float64) (map[string]any, error) {
	run, wait, err := c.ExecuteCodeBudget(timeout)
	if err != nil {
		return nil, err
	}
	if timeout == nil {
		return c.callMap(ctx, wait, "execute_code", code)
	}
	return c.callMap(ctx, wait, "execute_code", code, run)
}

// ExecuteCodeAsync starts code in the addon's background worker.
func (c *Connection) ExecuteCodeAsync(ctx context.Context, code string) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "execute_code_async", code)
}

// GetAsyncStatus reports one background job, or all of them for "".
func (c *Connection) GetAsyncStatus(ctx context.Context, jobID string) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "get_async_status", jobID)
}

// CreateDocument creates a document.
func (c *Connection) CreateDocument(ctx context.Context, name string) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "create_document", name)
}

// CreateObject creates an object from obj_data (Name, Type, Properties, Analysis).
func (c *Connection) CreateObject(ctx context.Context, doc string, objData map[string]any) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "create_object", doc, objData)
}

// EditObject sets properties on an object.
func (c *Connection) EditObject(ctx context.Context, doc, obj string, objData map[string]any) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "edit_object", doc, obj, objData)
}

// DeleteObject removes an object.
func (c *Connection) DeleteObject(ctx context.Context, doc, obj string) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "delete_object", doc, obj)
}

// ReloadDocument closes and reopens a document from disk.
func (c *Connection) ReloadDocument(ctx context.Context, doc string) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "reload_document", doc)
}

// InsertPartFromLibrary inserts a part from the parts library addon.
func (c *Connection) InsertPartFromLibrary(ctx context.Context, relativePath string) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "insert_part_from_library", relativePath)
}

// GetActiveScreenshot returns the active view as base64 PNG, or "" when the
// active view cannot be captured (a TechDraw page or a spreadsheet).
func (c *Connection) GetActiveScreenshot(ctx context.Context, view string, width, height *int, focus *string) (string, error) {
	var w, h, f any
	if width != nil {
		w = *width
	}
	if height != nil {
		h = *height
	}
	if focus != nil {
		f = *focus
	}
	v, err := c.call(ctx, c.timeout, "get_active_screenshot", view, w, h, f)
	if err != nil {
		return "", err
	}
	s, _ := v.(string)
	return s, nil
}

// GetObjects lists the objects of a document.
func (c *Connection) GetObjects(ctx context.Context, doc string) (any, error) {
	return c.call(ctx, c.timeout, "get_objects", doc)
}

// GetObject returns one object.
func (c *Connection) GetObject(ctx context.Context, doc, obj string) (any, error) {
	return c.call(ctx, c.timeout, "get_object", doc, obj)
}

// GetPartsList lists the parts library.
func (c *Connection) GetPartsList(ctx context.Context) (any, error) {
	return c.call(ctx, c.timeout, "get_parts_list")
}

// ListDocuments lists the open documents.
func (c *Connection) ListDocuments(ctx context.Context) (any, error) {
	return c.call(ctx, c.timeout, "list_documents")
}

// RunFEMAnalysis solves an analysis with CalculiX. Queueing and solving can
// each take timeout seconds.
func (c *Connection) RunFEMAnalysis(ctx context.Context, doc, analysis string, timeout int) (map[string]any, error) {
	wait := max(c.timeout, seconds(2*float64(timeout)+c.RPCTimeoutMargin))
	return c.callMap(ctx, wait, "run_fem_analysis", doc, analysis, timeout)
}

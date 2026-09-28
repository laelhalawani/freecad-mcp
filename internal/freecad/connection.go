// Package freecad talks to the FreeCAD addon's XML-RPC server.
package freecad

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
)

// Budget defaults. The addon reports its own run budgets through
// get_rpc_status; CheckAddonVersion adopts them.
const (
	DefaultTimeout            = 150 * time.Second
	DefaultExecuteCodeTimeout = 90.0   // seconds
	DefaultMaxExecuteCodeTime = 1800.0 // seconds
	DefaultRPCTimeoutMargin   = 30.0   // seconds
	// DefaultVersionCheckTimeout matches pingTimeout (internal/mcpserver):
	// right after FreeCAD starts, its GUI thread is still running startup
	// Python (workbench and Start page loading), and every XML-RPC request
	// runs on a ThreadingMixIn thread that needs the GIL, so even a call that
	// never waits for the GUI thread can take several seconds. A shorter
	// bound here was seen to time out about 10 s after the RPC server came
	// up, when the same ping would have succeeded.
	DefaultVersionCheckTimeout = 10 * time.Second
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

// opt returns *p, or nil when p is nil. The addon reads XML-RPC nil as "not
// given" for every optional argument.
func opt[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// optString returns s, or nil for "" (not given).
func optString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// optList returns list, or nil when it is empty (not given).
func optList[T any](list []T) any {
	if len(list) == 0 {
		return nil
	}
	return list
}

// optMap returns m, or nil when it is empty (not given).
func optMap(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

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

// CheckTimeout returns ErrInvalidTimeout for a timeout that is given but not
// a positive finite number, so a caller can reject it before connecting.
func CheckTimeout(timeout *float64) error {
	if timeout == nil {
		return nil
	}
	if t := *timeout; math.IsNaN(t) || math.IsInf(t, 0) || t <= 0 {
		return ErrInvalidTimeout
	}
	return nil
}

// budget returns the run budget sent to the addon for a call whose default
// run budget is def seconds, and the time to wait for the reply. A given
// timeout replaces def, capped at MaxExecuteCodeTimeout (1800 s unless the
// addon reports less). The addon permits a full queue budget followed by a
// full run budget, so the wait must outlast both.
func (c *Connection) budget(timeout *float64, def float64) (run float64, wait time.Duration, err error) {
	run = def
	if err := CheckTimeout(timeout); err != nil {
		return 0, 0, err
	}
	if timeout != nil {
		run = math.Min(*timeout, c.MaxExecuteCodeTimeout)
	}
	wait = max(c.timeout, seconds(2*run+c.RPCTimeoutMargin))
	return run, wait, nil
}

// callBudget calls a method that takes a run budget as its last argument. It
// sends params followed by the run budget resolved from timeout and def, and
// waits long enough for the queue and run budgets.
func (c *Connection) callBudget(ctx context.Context, method string, timeout *float64, def float64, params ...any) (map[string]any, error) {
	run, wait, err := c.budget(timeout, def)
	if err != nil {
		return nil, err
	}
	return c.callMap(ctx, wait, method, append(params, run)...)
}

// ExecuteCodeBudget returns the run budget sent to the addon and the time to
// wait for the reply. The addon permits a full queue budget followed by a
// full run budget, so the wait must outlast both.
func (c *Connection) ExecuteCodeBudget(timeout *float64) (run float64, wait time.Duration, err error) {
	return c.budget(timeout, c.ExecuteCodeTimeout)
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

// Screenshot is a capture of a document's 3D view, or why none was made.
type Screenshot struct {
	Image    string // base64-encoded PNG; empty when nothing was captured
	Document string // the document whose view was captured
	// When nothing was captured, Code is not_found or unavailable and Reason
	// one of no_document, document_not_found, no_3d_view, not_3d_view. An
	// addon older than protocol 3 gives no reason.
	Code    string
	Reason  string
	Message string
	Hint    string
}

// GetActiveScreenshot captures the 3D view of docName, or the active view
// when docName is "". A view that cannot be captured gives a Screenshot
// without Image that says why; any other failure to capture it is an
// *xmlrpc.Fault. docName is sent only when set, so automatic screenshots keep
// working with addons that predate it.
func (c *Connection) GetActiveScreenshot(ctx context.Context, view string, width, height *int, focus *string, docName string) (Screenshot, error) {
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
	params := []any{view, w, h, f}
	if docName != "" {
		params = append(params, docName)
	}
	v, err := c.call(ctx, c.timeout, "get_active_screenshot", params...)
	if err != nil {
		return Screenshot{}, err
	}
	switch r := v.(type) {
	case string:
		// Addons before protocol 3 reply with the bare image.
		return Screenshot{Image: r}, nil
	case map[string]any:
		get := func(key string) string { s, _ := r[key].(string); return s }
		shot := Screenshot{Document: get("document")}
		if ok, _ := r["success"].(bool); ok {
			shot.Image = get("image")
			return shot, nil
		}
		shot.Code, shot.Reason, shot.Message, shot.Hint = get("code"), get("reason"), get("error"), get("hint")
		return shot, nil
	}
	return Screenshot{}, nil
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

package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/laelhalawani/freecad-mcp/internal/domain"
	"github.com/laelhalawani/freecad-mcp/internal/freecad"
	"github.com/laelhalawani/freecad-mcp/internal/xmlrpc"
	"github.com/sairaph/mcp-wizard/render"
)

// connector hands out the FreeCAD connection. It connects on first use and
// keeps the connection only once FreeCAD has answered, so a server started
// before FreeCAD (the usual order, since AI clients launch it) checks the
// addon version on the first call that reaches FreeCAD.
type connector struct {
	settings domain.Settings
	version  string
	dial     func() *freecad.Connection
	launcher *freecad.Launcher // the FreeCAD start_freecad launched, for state-aware messages

	// connectMu serializes get's own connect attempts (dial, ping, version
	// check), which can take seconds against a stalled host: while one get
	// call is connecting, another blocks here rather than dialing again, then
	// reuses the connection the first one cached. It is never taken by probe,
	// so get_rpc_status stays responsive while a get call is mid-connect.
	connectMu sync.Mutex

	// mu guards only the fields below, which every read or write of finishes
	// at once: the cached connection and pending notice, and the last known
	// status. Nothing that can block on the network runs while it is held.
	mu     sync.Mutex
	conn   *freecad.Connection
	notice string // addon version warning not yet shown in a tool reply

	// lastDocuments and lastActiveDocument are the documents get_rpc_status
	// last saw while FreeCAD was reachable, kept so it can still report them
	// while FreeCAD is down; statusAt is when recordStatus took that reading,
	// and haveLastStatus is false until the first one. lastKnownDocuments
	// compares statusAt against the current launch's StartedAt itself,
	// instead of reset clearing these on a new launch: reset runs some time
	// after Launch returns (the whole forwardWindow, for a genuine start),
	// and not at all on the Reused path, so clearing on reset either shows a
	// previous process's reading as this one's for that window, or throws
	// away a current one that a probe recorded in the meantime.
	lastDocuments      []map[string]any
	lastActiveDocument string
	haveLastStatus     bool
	statusAt           time.Time

	// answeredAt is when a connection was last established (zero: never).
	// everAnswered compares it against the current launch's StartedAt for
	// the same reason statusAt is compared rather than cleared on reset.
	answeredAt time.Time
}

func newConnector(settings domain.Settings, version string, launcher *freecad.Launcher) *connector {
	c := &connector{settings: settings, version: version, launcher: launcher}
	c.dial = func() *freecad.Connection {
		return freecad.NewConnection(settings.Host, settings.Port, settings.Token, freecad.DefaultTimeout)
	}
	return c
}

// get returns the connection, connecting and checking the addon version the
// first time FreeCAD answers. Concurrent calls serialize on connectMu while
// one of them connects, so only one dial happens; a quick, separately-locked
// read of the cache lets an already-connected call return at once without
// waiting for that lock at all.
func (c *connector) get(ctx context.Context) (*freecad.Connection, error) {
	c.mu.Lock()
	cached := c.conn
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	c.connectMu.Lock()
	defer c.connectMu.Unlock()

	c.mu.Lock()
	cached = c.conn
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	conn := c.dial()
	// ping never waits for FreeCAD's GUI thread, so a healthy addon answers at
	// once; a short bound keeps a stalled host from holding connectMu (and
	// every other get call) for the full reply timeout.
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	ok, err := conn.Ping(pingCtx)
	cancel()
	if err != nil {
		conn.Close()
		// Any HTTP-level rejection (a bad or missing auth token, or the
		// addon's browser/host guard) means FreeCAD is up and answering, just
		// refusing this request: report it exactly as probe and get_rpc_status
		// do, not as FreeCAD being down. The addon's IP allowlist is not one
		// of these: it closes the connection with no HTTP response at all
		// (ip_filter.py verify_request), which reaches here as some other
		// error and falls through to unreachableError below.
		var perr *xmlrpc.ProtocolError
		if errors.As(err, &perr) {
			return nil, &toolError{protocolRejectedError(perr)}
		}
		return nil, &toolError{c.unreachableError(
			fmt.Sprintf("Failed to connect to FreeCAD at %s (%v). Make sure the FreeCAD addon is running.", conn.URL(), err))}
	}
	if !ok {
		conn.Close()
		return nil, &toolError{c.unreachableError(
			"Failed to connect to FreeCAD: the addon did not answer ping. Make sure the FreeCAD addon is running.")}
	}
	notice := conn.CheckAddonVersion(ctx, c.version)
	// A concurrent probe never takes connectMu, so it can have connected and
	// cached its own connection while this one was dialing; commitConnection
	// keeps whichever was cached first and closes the other, so this one's
	// connection is never leaked as an orphaned, never-closed socket.
	return c.commitConnection(conn, notice), nil
}

// takeNotice returns the pending version warning once.
func (c *connector) takeNotice() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.notice
	c.notice = ""
	return n
}

func (c *connector) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// reset drops the cached connection, so the next call pings FreeCAD and
// checks the addon version again (after start_freecad launched a new one).
// It leaves answeredAt and the last known documents alone (see the connector
// struct's doc comment on statusAt for why).
func (c *connector) reset() {
	c.close()
}

// everAnswered reports whether a connection has been established for ls: see
// the connector struct's doc comment on statusAt for why this compares
// answeredAt against ls.StartedAt instead of reset clearing it.
func (c *connector) everAnswered(ls freecad.LaunchState) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.answeredAt.IsZero() && (ls.State == "" || !c.answeredAt.Before(ls.StartedAt))
}

// resetIfCurrent drops the cached connection only if it is still exactly
// conn, then closes conn. A probe that found conn dead must not blindly
// reset: a concurrent call may already have replaced it with a healthy one
// (for example start_freecad's reset followed by another poll connecting),
// and dropping that one instead would undo the reconnect.
func (c *connector) resetIfCurrent(conn *freecad.Connection) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
	conn.Close()
}

// unreachableError builds the error connector.get returns when FreeCAD did
// not answer, state-aware and using the same classification fillDownState
// does (tools_status.go), so this message and get_rpc_status never disagree:
//   - launched (started or forwarded), RPC has not answered since the launch,
//     and under 120 s old: "FreeCAD is starting", pointing at get_rpc_status,
//     never start_freecad again.
//   - the launched process itself exited: say so.
//   - a forwarded launch that either never answered past 120 s, or did answer
//     and has since stopped: its own process already exited on purpose after
//     handing the request on, so there is nothing of this server's launch
//     left to check; point at the other FreeCAD's Report View.
//   - a started launch past 120 s, or one that already answered and has since
//     stopped: still alive (get_rpc_status calls this unresponsive) but not
//     answering; check there before launching another FreeCAD next to it.
//   - never launched, and this server is configured for FreeCAD on another
//     host: start_freecad cannot start it here, so say where to start it
//     instead of suggesting a tool call that only refuses.
//   - never launched, local host: the original base message and the
//     start_freecad / get_rpc_status advice.
func (c *connector) unreachableError(base string) render.Error {
	ls := c.launcher.State()
	everAnswered := c.everAnswered(ls)
	starting := !everAnswered && (ls.State == freecad.LaunchStarted || ls.State == freecad.LaunchForwarded) &&
		time.Since(ls.StartedAt) < freecad.StartingWindow
	if starting {
		elapsed := int(time.Since(ls.StartedAt).Round(time.Second).Seconds())
		if elapsed < 0 {
			elapsed = 0
		}
		return render.Error{
			Code:    render.CodeUnavailable,
			Message: fmt.Sprintf("FreeCAD is starting (%d s since start_freecad).", elapsed),
			Hint:    "Call get_rpc_status with {} until it reports rpc: reachable, then retry.",
		}
	}
	if ls.State == freecad.LaunchExited {
		return render.Error{
			Code:    render.CodeUnavailable,
			Message: "The FreeCAD process this server launched has exited.",
			Hint:    "Call get_rpc_status with {} for its exit code and log, then start_freecad with {} to launch it again.",
		}
	}
	if ls.State == freecad.LaunchForwarded {
		hint := "start_freecad forwarded this to a FreeCAD window that was already open, but its RPC server " +
			"never answered. Check that window's Report View, then call start_freecad with {} again."
		if everAnswered {
			hint = "start_freecad forwarded this to a FreeCAD window that was already open; its RPC server did " +
				"answer for a while but has since stopped, most likely because that window was closed. Call " +
				"start_freecad with {} to launch a fresh one."
		}
		return render.Error{Code: render.CodeUnavailable, Message: base, Hint: hint}
	}
	if ls.State == freecad.LaunchStarted {
		return render.Error{
			Code:    render.CodeUnavailable,
			Message: base,
			Hint: "Call get_rpc_status with {} to check whether FreeCAD is unresponsive (a blocking dialog) or " +
				"has exited, before calling start_freecad again.",
		}
	}
	if refuseNonLoopbackHost(c.settings.Host) != nil {
		return render.Error{
			Code:    render.CodeUnavailable,
			Message: base,
			Hint: fmt.Sprintf("This server is configured for FreeCAD on %s, not this machine: start FreeCAD "+
				"there, then call get_rpc_status with {}.", c.settings.Host),
		}
	}
	return render.Error{
		Code:    render.CodeUnavailable,
		Message: base,
		Hint:    "Call start_freecad with {} to launch FreeCAD, then get_rpc_status with {}. " + startHint,
	}
}

// commitConnection caches conn as the connector's connection, unless another
// call already cached one first (a probe and a get can race to connect), in
// which case conn is closed instead and the winner already cached is
// returned. Every path that just connected must go through this (never a
// bare c.conn = conn), or the loser's connection leaks: nothing else closes it.
func (c *connector) commitConnection(conn *freecad.Connection, notice string) *freecad.Connection {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		c.conn = conn
		c.notice = notice
		c.answeredAt = time.Now()
		return conn
	}
	conn.Close()
	return c.conn
}

// recordStatus keeps the documents and active document of a successful
// get_rpc_status, so they can still be reported once FreeCAD stops answering.
func (c *connector) recordStatus(status map[string]any) {
	if status == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if docs, ok := status["documents"].([]any); ok {
		list := make([]map[string]any, 0, len(docs))
		for _, d := range docs {
			if m, ok := d.(map[string]any); ok {
				list = append(list, m)
			}
		}
		c.lastDocuments = list
		c.haveLastStatus = true
		c.statusAt = time.Now()
	}
	if active, ok := status["active_document"].(string); ok {
		c.lastActiveDocument = active
	}
}

// lastKnownDocuments returns the documents from the last successful
// get_rpc_status, and whether there is a reading to show for ls: one has
// been taken and, when ls describes a launch this server started, it was
// taken at or after that launch started (see the connector struct's doc
// comment on statusAt for why this compares timestamps instead of reset
// clearing the reading on a new launch).
func (c *connector) lastKnownDocuments(ls freecad.LaunchState) (documents []map[string]any, activeDocument string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.haveLastStatus || (ls.State != "" && c.statusAt.Before(ls.StartedAt)) {
		return nil, "", false
	}
	return c.lastDocuments, c.lastActiveDocument, true
}

// probeResult is what probe found out about FreeCAD. Exactly one of status,
// oldAddon, fault or rejected describes a reachable outcome; none of them set
// means unreachable, classified further by timedOut.
type probeResult struct {
	status map[string]any // the addon's get_rpc_status reply, only when it fully succeeded

	reachable bool // FreeCAD answered at the HTTP/RPC level at all
	timedOut  bool // unreachable only: a definite timeout, not a refusal

	oldAddon bool // reachable, but the addon has no get_rpc_status (protocol < 3)

	// fault is set when the addon answered but get_rpc_status itself raised
	// (an addon-side bug): reachable is true, status is nil, and the
	// connection is left exactly as it was, since the failure was not the
	// connection's fault.
	fault string

	// rejected is set when FreeCAD answered at the HTTP level but refused the
	// request (see protocolRejectedError): FreeCAD is up, this is not
	// "unreachable", and getRPCStatus reports it as this error instead of a
	// state.
	rejected *render.Error
}

// probe attempts to reach FreeCAD for get_rpc_status, which must answer even
// while FreeCAD is down: unlike get, a connection failure never becomes an
// error here, only an unreachable result. It never takes connectMu, only the
// quick cache lock, so it stays responsive while a get call is busy dialing.
func (c *connector) probe(ctx context.Context) probeResult {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	fresh := conn == nil
	if fresh {
		conn = c.dial()
		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		ok, err := conn.Ping(pingCtx)
		cancel()
		if err != nil || !ok {
			var perr *xmlrpc.ProtocolError
			if err != nil && errors.As(err, &perr) {
				conn.Close()
				e := protocolRejectedError(perr)
				return probeResult{rejected: &e}
			}
			timedOut := err != nil && isTimeout(err)
			conn.Close()
			return probeResult{timedOut: timedOut}
		}
	}

	statusCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	v, err := conn.GetRPCStatus(statusCtx)
	cancel()
	if err != nil {
		var perr *xmlrpc.ProtocolError
		if errors.As(err, &perr) {
			// FreeCAD answered but refused this request (see
			// protocolRejectedError for which statuses and why): it is up,
			// and the connection itself is not at fault, only this request
			// was, but a rejection is not going to start succeeding on a
			// retry with the same connection either, so drop it like any
			// other dead one.
			if fresh {
				conn.Close()
			} else {
				c.resetIfCurrent(conn)
			}
			e := protocolRejectedError(perr)
			return probeResult{rejected: &e}
		}
		var fault *xmlrpc.Fault
		if errors.As(err, &fault) {
			if fault.MissingMethod() {
				if fresh {
					c.commitConnection(conn, freecad.AddonVersionWarning(nil, c.version))
				}
				return probeResult{reachable: true, oldAddon: true}
			}
			// The addon answered (or a cached connection is still working)
			// but get_rpc_status itself raised: an addon-side bug, not
			// FreeCAD being down or this connection being dead. A cached
			// connection is left exactly as it was. A fresh one is closed
			// rather than cached uninspected: caching it here would skip the
			// version check and the addon's run budgets (never adopted from
			// this failed call), and an addon whose get_rpc_status raises is
			// exactly the one most likely to need that warning shown; the
			// next get or probe dials again and does both properly.
			if fresh {
				conn.Close()
			}
			return probeResult{reachable: true, fault: fault.String}
		}
		if fresh {
			conn.Close()
		} else {
			// This was the cached connection, found dead: drop it, but only
			// if it is still the one cached (a concurrent call may already
			// have replaced it with a healthy one, which must not be undone).
			c.resetIfCurrent(conn)
		}
		return probeResult{timedOut: isTimeout(err)}
	}

	m, _ := v.(map[string]any)
	if fresh {
		// Adopt the run budgets and version notice from the status this call
		// already fetched, instead of a second get_rpc_status round trip
		// (what CheckAddonVersion would otherwise make).
		ceiling := freecad.DefaultMaxExecuteCodeTime
		if f, ok := freecad.IsBudget(m["execute_code_timeout"], ceiling); ok {
			conn.ExecuteCodeTimeout = f
		}
		if f, ok := freecad.IsBudget(m["max_execute_code_timeout"], ceiling); ok {
			conn.MaxExecuteCodeTimeout = f
		}
		c.commitConnection(conn, freecad.AddonVersionWarning(m, c.version))
	}
	c.recordStatus(m)
	return probeResult{status: m, reachable: true}
}

// protocolRejectedError builds the error get, probe (and so get_rpc_status)
// and start_freecad all report the same way when FreeCAD answered at the
// HTTP level but refused the request. FreeCAD is up either way, so this is
// never treated as it being down: a 401 means a bad or missing auth token;
// any other status means the addon's browser guard refused it (a request
// carrying an Origin header, an unexpected Content-Type, or, for a
// loopback-only addon, a Host header that is not localhost). The addon's IP
// allowlist is a separate case that never reaches here, since it closes the
// connection with no HTTP response at all (ip_filter.py verify_request), so
// the caller sees some other error instead of a status.
func protocolRejectedError(perr *xmlrpc.ProtocolError) render.Error {
	if perr.StatusCode == 401 {
		return render.Error{
			Code: render.CodeAuth,
			Message: "FreeCAD rejected the connection: the addon requires an auth token, " +
				"and none or a different one was given.",
			Hint: authHint,
		}
	}
	return render.Error{
		Code:    render.CodeForbidden,
		Message: fmt.Sprintf("FreeCAD's RPC server answered but refused the request (HTTP %s).", perr.Status),
		Hint: fmt.Sprintf("FREECAD_MCP_HOST must be localhost or 127.0.0.1 for a loopback-only addon, and the "+
			"request must not look like it came from a web page. Run `%s doctor` to check the setup.", domain.BinaryName),
	}
}

const pingTimeout = 10 * time.Second

const startHint = "Start FreeCAD, select the MCP Addon workbench and click Start RPC Server " +
	"(or turn on its auto-start), then retry. Run `" + domain.BinaryName + " doctor` to check the setup."

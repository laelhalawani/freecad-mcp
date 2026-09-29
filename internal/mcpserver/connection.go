package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/freecad-mcp/internal/remote"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
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

	// onLock is told the addon's X-FreeCAD-MCP-Lock header of every reply
	// (on while remote access is on), and onStatus every get_rpc_status reply
	// the connector sees (with its session key). The Server sets both before
	// serving; they show and hide the remote-only tools (visibility.go).
	onLock   func(on bool)
	onStatus func(status map[string]any)

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

	// identityMismatch is set alongside conn, once, when the identity check
	// (identityMismatchWarning) run at connect time (get's or probe's fresh
	// dial) finds that the computer answering a loopback host is not this
	// one: cleared whenever conn is (close, resetIfCurrent), so the next
	// connection is checked again fresh. Every call refuses while it is
	// set, not only get_rpc_status, since acting on it would silently reach
	// the wrong FreeCAD (live check L11).
	identityMismatch string

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
	lastPID            int // FreeCAD's process id at the last reading, for busy detection
	haveLastStatus     bool
	statusAt           time.Time

	// lastRemoteEnabled is session.enabled of the last successful
	// get_rpc_status, nil until the first one: for the direct (non-listener)
	// path, once FreeCAD stops answering there is no listener to ask
	// instead, so this is the only way to tell "remote access was on last
	// we knew" from "never known at all" (live-fixes review, remaining
	// nits: session_lock should read "unknown", not "off", in the first
	// case, and be left out of the reply entirely in the second, rather
	// than guessing "off" either way).
	lastRemoteEnabled *bool

	// answeredAt is when a connection was last established (zero: never).
	// everAnswered compares it against the current launch's StartedAt for
	// the same reason statusAt is compared rather than cleared on reset.
	answeredAt time.Time

	// listenerMu guards the cached classification of the configured endpoint:
	// listenerKnown is false until the first successful probe, so isListener
	// probes again on every call until one actually answers (a network
	// failure is never cached); once known, listenerIsOne holds for the rest
	// of the process.
	listenerMu    sync.Mutex
	listenerKnown bool
	listenerIsOne bool
}

func newConnector(settings domain.Settings, version string, launcher *freecad.Launcher) *connector {
	c := &connector{settings: settings, version: version, launcher: launcher}
	c.dial = func() *freecad.Connection {
		conn := freecad.NewConnection(settings.Host, settings.Port, settings.Token, freecad.DefaultTimeout)
		conn.OnLock = c.lockSeen
		conn.OnHeaders = func(h http.Header) { c.headersSeen(conn, h) }
		conn.OnDocumentsChanged = func(ctx context.Context) { c.refreshStatus(ctx, conn) }
		return conn
	}
	return c
}

// endpoint is the configured listener address, for internal/remote calls.
func (c *connector) endpoint() remote.Endpoint {
	return remote.Endpoint{Host: c.settings.Host, Port: c.settings.Port, Token: c.settings.Token}
}

// lockSeen passes the addon's lock header on to onLock.
func (c *connector) lockSeen(on bool) {
	if c.onLock != nil {
		c.onLock(on)
	}
}

// headersSeen passes conn's reply headers on, and drops conn from the cache
// when they say FreeCAD is not running behind a listener: the next call
// reconnects and checks the addon version again once FreeCAD answers.
// conn is the specific connection this reply came from, so a reply from a
// connection already replaced by a fresher one never resets that one
// instead (resetIfCurrent).
func (c *connector) headersSeen(conn *freecad.Connection, h http.Header) {
	if h.Get(domain.HeaderListener) == domain.ListenerFreeCADDown {
		c.resetIfCurrent(conn)
	}
}

// statusSeen passes a get_rpc_status reply on to onStatus.
func (c *connector) statusSeen(status map[string]any) {
	if c.onStatus != nil && status != nil {
		c.onStatus(status)
	}
}

// get returns the connection, connecting and checking the addon version the
// first time FreeCAD answers. Concurrent calls serialize on connectMu while
// one of them connects, so only one dial happens; a quick, separately-locked
// read of the cache lets an already-connected call return at once without
// waiting for that lock at all.
func (c *connector) get(ctx context.Context) (*freecad.Connection, error) {
	c.mu.Lock()
	cached := c.conn
	mismatch := c.identityMismatch
	c.mu.Unlock()
	if cached != nil {
		if mismatch != "" {
			return nil, &toolError{identityMismatchError(mismatch)}
		}
		return cached, nil
	}

	c.connectMu.Lock()
	defer c.connectMu.Unlock()

	c.mu.Lock()
	cached = c.conn
	mismatch = c.identityMismatch
	c.mu.Unlock()
	if cached != nil {
		if mismatch != "" {
			return nil, &toolError{identityMismatchError(mismatch)}
		}
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
			if perr.Listener == domain.ListenerFreeCADDown {
				// The listener answered but FreeCAD is not running there:
				// that is unreachable, never a rejection.
				return nil, &toolError{listenerDownError(hostOf(perr.URL))}
			}
			return nil, &toolError{protocolRejectedError(c.settings.Host, perr)}
		}
		var fault *xmlrpc.Fault
		if errors.As(err, &fault) {
			// ping itself only ever faults this way (it never raises on its
			// own): FreeCAD answered, so this is reachable, not unreachable;
			// the fault's own text already carries the advice (live check
			// L4), matching what a call-specific fault gets through failure().
			// The specific hint is given only when the fault actually is the
			// settings-unreadable one (matched exactly, not any fault from
			// ping): something else answering on this port could raise a
			// different one, for which that advice would not fit (live-fixes
			// review N1).
			hint := "FreeCAD reported this error while handling the call. Check the arguments and retry. " + statusHint
			if fault.SettingsUnreadable() {
				hint = settingsUnreadableHint
			}
			return nil, &toolError{render.Error{Code: codeFreeCAD, Message: fault.String, Hint: hint}}
		}
		base := fmt.Sprintf("Failed to connect to FreeCAD at %s (%v). Make sure the FreeCAD addon is running.", conn.URL(), err)
		if looksLikeListener(c.settings.Host, c.settings.Port) {
			// This endpoint is configured as a listener's, not the local
			// addon's: name it, instead of talking about "the FreeCAD
			// addon" as if this were a local connection (live check L7).
			base = fmt.Sprintf("The freecad-mcp listener on %s could not be reached (%v).", c.settings.Host, err)
		}
		return nil, &toolError{c.unreachableError(base)}
	}
	if !ok {
		conn.Close()
		return nil, &toolError{c.unreachableError(
			"Failed to connect to FreeCAD: the addon did not answer ping. Make sure the FreeCAD addon is running.")}
	}
	notice, status := conn.CheckAddonVersion(ctx, c.version)
	c.statusSeen(status)
	// A concurrent probe never takes connectMu, so it can have connected and
	// cached its own connection while this one was dialing; commitConnection
	// keeps whichever was cached first and closes the other, so this one's
	// connection is never leaked as an orphaned, never-closed socket.
	committed := c.commitConnection(conn, notice, identityMismatchWarning(c.settings.Host, status))
	if mismatch := c.identityMismatchWarningCached(); mismatch != "" {
		// Read back whichever connection actually won the race above (this
		// one's finding, or a concurrent probe's): the identity check runs
		// once, at connect time, and every call refuses while it stands, not
		// only get_rpc_status (live check L11).
		return nil, &toolError{identityMismatchError(mismatch)}
	}
	return committed, nil
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
	c.identityMismatch = ""
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
		c.identityMismatch = ""
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
	if looksLikeListener(c.settings.Host, c.settings.Port) {
		// A non-loopback host, or a loopback host on a port other than the
		// addon's own, is only ever a freecad-mcp listener now (the addon
		// binds 127.0.0.1:9875 always): name it and give the same advice
		// get_rpc_status already gives for the same endpoint (live check
		// L7), instead of the plain local advice below, which would wrongly
		// tell the caller to go start FreeCAD there in person. base already
		// names the listener here (see get, which builds it this way
		// whenever looksLikeListener agrees).
		return render.Error{
			Code:    render.CodeUnavailable,
			Message: base,
			// No start_freecad advice here: the listener itself, not only
			// FreeCAD, could not be reached, so start_freecad would fail
			// the exact same way (live-fixes review, remaining nits).
			Hint: fmt.Sprintf("Check that \"Share this PC\" is set up and its listener runs on %s, and that this "+
				"computer's IP address is in its allowed list.", c.settings.Host),
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
func (c *connector) commitConnection(conn *freecad.Connection, notice, identityMismatch string) *freecad.Connection {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		c.conn = conn
		c.notice = notice
		c.identityMismatch = identityMismatch
		c.answeredAt = time.Now()
		return conn
	}
	conn.Close()
	return c.conn
}

// identityMismatchWarningCached returns the identity check's own finding for
// the connection currently cached ("" when there is none, or none was
// found): whichever connection actually won commitConnection's race, not
// necessarily this call's own.
func (c *connector) identityMismatchWarningCached() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.identityMismatch
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
	// The process id is only usable when FreeCAD runs on this computer: the
	// host is loopback and the reply's hostname and platform are this one's.
	if domain.IsLoopbackHost(c.settings.Host) && identityMismatchWarning(c.settings.Host, status) == "" {
		switch pid := status["pid"].(type) {
		case int64:
			c.lastPID = int(pid)
		case float64:
			c.lastPID = int(pid)
		}
	} else {
		c.lastPID = 0
	}
	if session, ok := status["session"].(map[string]any); ok {
		if enabled, ok := session["enabled"].(bool); ok {
			c.lastRemoteEnabled = &enabled
		}
	}
}

// refreshStatus re-reads the documents after a call that changed them, so the
// last known state get_rpc_status shows while FreeCAD is down is not older
// than the last change. It is best effort and bounded: the status call never
// waits for the GUI thread.
func (c *connector) refreshStatus(ctx context.Context, conn *freecad.Connection) {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if status, err := conn.GetRPCStatus(ctx); err == nil {
		if m, ok := status.(map[string]any); ok {
			c.recordStatus(m)
		}
	}
}

// knownPID returns FreeCAD's process id: the one its last status reading
// reported, when that reading belongs to the current launch, else the id of
// the process this server started. Zero when neither is known, or when the
// configured host is not this computer.
func (c *connector) knownPID(ls freecad.LaunchState) int {
	if !domain.IsLoopbackHost(c.settings.Host) {
		return 0
	}
	c.mu.Lock()
	pid, at := c.lastPID, c.statusAt
	c.mu.Unlock()
	if pid > 0 && !(ls.State != "" && at.Before(ls.StartedAt)) {
		return pid
	}
	if ls.State == freecad.LaunchStarted {
		return ls.PID
	}
	return 0
}

// lastStatusAge is how long ago the last status reading was taken.
func (c *connector) lastStatusAge() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.statusAt)
}

// lastKnownRemoteEnabled returns session.enabled of the last successful
// get_rpc_status, and whether there has ever been one: false, false when
// FreeCAD has never answered since this process started (not knowable at
// all), as opposed to false, true (last known: off) or true, true (last
// known: on).
func (c *connector) lastKnownRemoteEnabled() (enabled, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastRemoteEnabled == nil {
		return false, false
	}
	return *c.lastRemoteEnabled, true
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

	// identityMismatch is the connector's own identityMismatchWarningCached
	// (live check L11): non-"" means the computer answering a loopback host
	// is not this one, so this reply is refused the same way get's callers
	// are, rather than shown as a footnote in an otherwise successful reply.
	identityMismatch string
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
				if perr.Listener == domain.ListenerFreeCADDown {
					// The listener answered but FreeCAD is not running
					// there: that is unreachable, so get_rpc_status
					// classifies it from the listener's own status instead
					// of this being reported as a rejection.
					return probeResult{}
				}
				e := protocolRejectedError(c.settings.Host, perr)
				return probeResult{rejected: &e}
			}
			var fault *xmlrpc.Fault
			if err != nil && errors.As(err, &fault) {
				// ping itself only ever faults this way (it never raises on
				// its own): FreeCAD answered, so this is reachable, not
				// unreachable (live check L4); get_rpc_status reports it the
				// same way a fault from get_rpc_status itself already does.
				conn.Close()
				return probeResult{reachable: true, fault: fault.String}
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
			if perr.Listener == domain.ListenerFreeCADDown {
				// The listener answered but FreeCAD is not running there:
				// unreachable, not a rejection (see above).
				return probeResult{}
			}
			e := protocolRejectedError(c.settings.Host, perr)
			return probeResult{rejected: &e}
		}
		var fault *xmlrpc.Fault
		if errors.As(err, &fault) {
			if fault.MissingMethod() {
				if fresh {
					c.commitConnection(conn, freecad.AddonVersionWarning(nil, c.version), "")
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
		conn.AdoptBudgets(m)
		c.commitConnection(conn, freecad.AddonVersionWarning(m, c.version), identityMismatchWarning(c.settings.Host, m))
	}
	c.recordStatus(m)
	c.statusSeen(m)
	return probeResult{status: m, reachable: true, identityMismatch: c.identityMismatchWarningCached()}
}

// identityMismatchWarning warns when configuredHost is loopback (this
// server was told to reach FreeCAD at "localhost", which domain.SettingsFromEnv
// resolves to 127.0.0.1 itself) but the computer that actually answered
// reports a different hostname, or a different platform, than this one:
// something else is capturing that address, most likely another computer's
// own port forwarding onto it, for example WSL's own localhost forwarding
// when freecad-mcp is shared from inside WSL and on Windows on the same
// machine at once. get and probe run this once, when a connection is first
// established, and cache its result on the connector (identityMismatch) for
// as long as that connection lives, rather than on every call: every call
// refuses while it is set, not only get_rpc_status, since acting on it
// would silently reach the wrong FreeCAD (live check L11). status nil, or
// carrying no hostname (an older addon, or the hostname call failed there),
// never warns, since there is then nothing to compare.
func identityMismatchWarning(configuredHost string, status map[string]any) string {
	if !domain.IsLoopbackHost(configuredHost) || status == nil {
		return ""
	}
	remoteHostname, _ := status["hostname"].(string)
	remotePlatform, _ := status["platform"].(string)
	remoteWSL, _ := status["wsl"].(bool)

	hostnameDiffers := false
	if remoteHostname != "" {
		if here, err := os.Hostname(); err == nil && here != "" {
			hostnameDiffers = !strings.EqualFold(here, remoteHostname)
		}
	}
	// Platform/WSL is a second, independent signal, not only a fallback for
	// when the hostname check is silent: by default WSL takes the Windows
	// computer's own name, so two sides of exactly the boundary this warns
	// about can report the same hostname and still need catching (live-fixes
	// review M1).
	platformDiffers := remotePlatform != "" &&
		(normalizeSysPlatform(remotePlatform) != runtime.GOOS || remoteWSL != domain.IsWSL())
	if !hostnameDiffers && !platformDiffers {
		return ""
	}

	// Names the answering side as "<platform> (WSL) on <hostname>" (or
	// without "(WSL)" when it is not one), never just the hostname, so it
	// reads clearly as another computer, not a stale reading of this one
	// under a changed name (live re-check L11).
	who := "another platform"
	if remoteHostname != "" || remotePlatform != "" {
		platform := remotePlatform
		if platform == "" {
			platform = "an unknown platform"
		} else {
			platform = normalizeSysPlatform(platform)
			if remoteWSL {
				platform += " (WSL)"
			}
		}
		host := remoteHostname
		if host == "" {
			host = "an unknown host"
		}
		who = fmt.Sprintf("%s on %s", platform, host)
	}
	return fmt.Sprintf(
		"localhost answers as %s, not this computer: most likely another computer's own port forwarding onto it, "+
			"for example WSL's own localhost forwarding while freecad-mcp also shares FreeCAD inside WSL. Every "+
			"call is refused until %s (or the FreeCAD connection this AI client is configured with) names the "+
			"right computer. See "+
			"https://github.com/sairaph/freecad-mcp/blob/main/docs/remote-access.md#wsl-and-windows-on-the-same-computer.",
		who, domain.EnvHost)
}

// normalizeSysPlatform maps a Python sys.platform value (the addon's own
// report) to the runtime.GOOS value it corresponds to, so the two can be
// compared directly: "win32" (or "cygwin") is "windows"; "linux" and
// "darwin" already match.
func normalizeSysPlatform(sysPlatform string) string {
	switch sysPlatform {
	case "win32", "cygwin":
		return "windows"
	default:
		return sysPlatform
	}
}

// identityMismatchError is the refusal every call meets while the
// connector's identity check (identityMismatchWarning) found a mismatch:
// warning is its own text, naming the answering side and why.
func identityMismatchError(warning string) render.Error {
	return render.Error{
		Code:    codeIdentityMismatch,
		Message: warning,
		Hint: "Point " + domain.EnvHost + " at the right computer, or turn off whichever port forwarding is " +
			"capturing localhost, then restart the AI client: this finding is cached for as long as the current " +
			"connection lives, so it does not clear on its own until then.",
	}
}

// looksLikeListener reports whether host:port is configured the way a
// freecad-mcp listener is, never the local addon (contract 3's probe rule):
// not a loopback host, or a loopback host on a port other than the addon's
// own default (domain.DefaultRPCPort). It is a syntactic check only, made
// without a network call, unlike isListener's own confirmed classification;
// callers use it to word an unreachable-endpoint message before any call
// has told them for certain (live check L7).
func looksLikeListener(host string, port int) bool {
	return !(domain.IsLoopbackHost(host) && port == domain.DefaultRPCPort)
}

// isListener reports whether the configured endpoint is a freecad-mcp
// listener (detected by its marker, cached for the process and probed again
// after a connection failure). A loopback host on the addon's own RPC port
// is never probed: an addon would log a "Rejected request" warning for
// every probe, and it can never be a listener.
func (c *connector) isListener(ctx context.Context) bool {
	if !looksLikeListener(c.settings.Host, c.settings.Port) {
		return false
	}
	c.listenerMu.Lock()
	known, isOne := c.listenerKnown, c.listenerIsOne
	c.listenerMu.Unlock()
	if known {
		return isOne
	}
	found, err := remote.Probe(ctx, c.endpoint())
	switch {
	case err == nil:
		// The marker was present and the status was 200: definitely a
		// listener.
		c.setListener(true)
		return true
	case errors.Is(err, remote.ErrNotListener):
		// No marker at all: the addon, or something else, never a
		// listener, whatever the host or port.
		c.setListener(false)
		return false
	case found:
		// The marker was present but the call was refused (a wrong or
		// missing password, or the listener's own guard order): still a
		// listener, whatever the status.
		c.setListener(true)
		return true
	default:
		// Unreachable: nothing is cached, so the next call probes again.
		return false
	}
}

// setListener caches isListener's classification of the configured endpoint
// for the rest of the process.
func (c *connector) setListener(isOne bool) {
	c.listenerMu.Lock()
	c.listenerKnown, c.listenerIsOne = true, isOne
	c.listenerMu.Unlock()
}

// listenerStatus calls the listener's /listener/status with the password and
// the session headers in ctx.
func (c *connector) listenerStatus(ctx context.Context) (listenerapi.Status, error) {
	return remote.Status(ctx, c.endpoint())
}

// listenerStart calls the listener's /listener/start to start FreeCAD on its
// computer, opening file (a path there) when it is not "".
func (c *connector) listenerStart(ctx context.Context, file string) (listenerapi.StartResult, error) {
	return remote.Start(ctx, c.endpoint(), file)
}

// listenerDownFailure is failure's case for a listener's /RPC2 503 while
// FreeCAD is not running there (xmlrpc.ProtocolError.Listener ==
// domain.ListenerFreeCADDown): unavailable, "FreeCAD is not running on
// <host>", with the start_freecad hint. It returns nil for any other error.
func listenerDownFailure(err error) *mcp.CallToolResult {
	var perr *xmlrpc.ProtocolError
	if !errors.As(err, &perr) || perr.Listener != domain.ListenerFreeCADDown {
		return nil
	}
	return render.ErrorResult(listenerDownError(hostOf(perr.URL)))
}

// listenerDownError is the render.Error a listener's /RPC2 503 becomes,
// shared by listenerDownFailure (a call already routed through a
// cached connection, reaching reply.go's failure unwrapped) and get (the
// same 503 met while dialing, before anything is cached, which pre-renders
// its own error instead of leaving that to failure).
func listenerDownError(host string) render.Error {
	return render.Error{
		Code:    render.CodeUnavailable,
		Message: fmt.Sprintf("FreeCAD is not running on %s.", host),
		Hint: "Call start_freecad with {} to start it there, then get_rpc_status with {} until it reports " +
			"rpc: reachable.",
	}
}

// hostOf returns the host of a URL built by internal/xmlrpc.NewClient
// (http://host:port/RPC2), without the port or credentials, or the whole
// URL when it cannot be parsed, so a message built from it stays readable
// either way.
func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return rawURL
}

// hostPortOf returns the host and port of a URL built by
// internal/xmlrpc.NewClient (http://host:port/RPC2), without the
// credentials, or the whole URL when it cannot be parsed.
func hostPortOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}

// protocolRejectedError builds the error get, probe (and so get_rpc_status)
// and start_freecad all report the same way when FreeCAD, or a listener in
// front of it, answered at the HTTP level but refused the request. Either
// way something is up and reachable, so this is never treated as FreeCAD
// being down. host is the configured host this rejection came from, used
// only to word the 401 hint; "" when a caller has none to give (reply.go's
// generic failure path), which just omits that extra sentence.
//
// perr.Listener tells the two cases apart: "" is the addon answering
// directly, non-empty is a listener's own refusal, rendered by
// listenerRejectedError (see listenerErrorFor, its counterpart for
// internal/remote.Status and .Start).
func protocolRejectedError(host string, perr *xmlrpc.ProtocolError) render.Error {
	if perr.Listener != "" {
		return listenerRejectedError(perr)
	}
	// The addon itself refused the request: a 401 means a bad or missing
	// password; any other status means its browser guard refused it (a
	// request carrying an Origin header, an unexpected Content-Type, or,
	// for a loopback-only addon, a Host header that is not localhost). The
	// addon's IP allowlist is a separate case that never reaches here,
	// since it closes the connection with no HTTP response at all
	// (ip_filter.py verify_request), so the caller sees some other error
	// instead of a status.
	if perr.StatusCode == 401 {
		return render.Error{
			Code:    render.CodeAuth,
			Message: "FreeCAD asks for a password, and none or a different one was given.",
			Hint:    authHint + wslLoopback401Note(host),
		}
	}
	return render.Error{
		Code:    render.CodeForbidden,
		Message: fmt.Sprintf("FreeCAD's RPC server answered but refused the request (HTTP %s).", perr.Status),
		Hint: fmt.Sprintf("FREECAD_MCP_HOST must be localhost or 127.0.0.1 for a loopback-only addon, and the "+
			"request must not look like it came from a web page. Run `%s doctor` to check the setup.", domain.BinaryName),
	}
}

// wslLoopback401Note extends a 401 hint on Windows, for a loopback host,
// with a pointer at WSL: FreeCAD on Windows cannot even open its own RPC
// port while WSL's own localhost forwarding holds it
// (docs/remote-access.md's WSL section, final live check), so whatever
// answered 401 there is at least as likely to be the WSL side, with its
// own separate password, as this computer's own addon or listener (live
// re-check L11; final live check extends this to the listener path too).
// "" everywhere else, so a caller can always append it plainly.
func wslLoopback401Note(host string) string {
	if runtime.GOOS != "windows" || !domain.IsLoopbackHost(host) {
		return ""
	}
	return " If freecad-mcp shares FreeCAD inside WSL too, WSL port forwarding may be answering localhost; see " +
		"the remote access guide, " +
		"https://github.com/sairaph/freecad-mcp/blob/main/docs/remote-access.md#wsl-and-windows-on-the-same-computer."
}

// listenerErrorBody decodes perr.Body (the listener's JSON error, set only
// when the marker is present) into listenerapi.Error. An empty or
// undecodable body yields the zero value, so callers still have a usable
// (empty) Error, Hint and Reason to fall back from.
func listenerErrorBody(perr *xmlrpc.ProtocolError) listenerapi.Error {
	var e listenerapi.Error
	if perr.Body != "" {
		_ = json.Unmarshal([]byte(perr.Body), &e)
	}
	return e
}

// attributedText marks text as coming from the listener rather than from
// this client, so it is never mistaken for this client's own words; "" when
// text is empty, so a caller with its own fallback text can tell whether the
// listener actually sent one.
func attributedText(host, text string) string {
	if text == "" {
		return ""
	}
	return fmt.Sprintf("The listener on %s says: %s", host, text)
}

// attributedHint prefixes a hint written by the listener (over an
// unencrypted connection, by whoever runs the computer at the other end),
// so the model never follows it as if this client wrote it: a hint such as
// "call execute_code_headless with ..." would otherwise run as this
// client's own suggestion.
func attributedHint(hint string) string {
	if hint == "" {
		return ""
	}
	return "The listener suggests: " + hint
}

// listenerRejectedError is protocolRejectedError's case for perr.Listener
// != "": a listener's own refusal of a proxied /RPC2 call, rendered by
// listenerRefusalError. Kept as its own name (rather than inlined into
// protocolRejectedError) since tools_launch.go's start_freecad also calls it
// directly, packing a *remote.Error into a synthetic *xmlrpc.ProtocolError
// to reuse this rendering for a refusal met on a direct listener call
// (listenerStatus, listenerStart) instead of a proxied one.
func listenerRejectedError(perr *xmlrpc.ProtocolError) render.Error {
	return listenerRefusalError(hostPortOf(perr.URL), perr.StatusCode, perr.RetryAfter, listenerErrorBody(perr))
}

// listenerRefusalError builds the render.Error for any refusal a listener
// gives this client, from a proxied /RPC2 call (protocolRejectedError) or a
// direct call to /listener/status or /listener/start (listenerCallError):
// mapped by the listener's own Reason when this client has specific wording
// for it, or the listener's own decoded code,
// text and hint, attributed to it, for anything else (a missing or unknown
// reason). hostPort is the listener's address as host:port; statusCode and
// retryAfter (seconds, 0 when absent) are the HTTP reply's; body is the
// decoded JSON error, the zero value when there was none or it failed to
// decode.
//
// It never needs to single out FreeCAD being down behind a listener (503,
// ReasonFreeCADDown): that is recognised and treated as unreachable, not a
// refusal, by every caller before this runs (get, probe, listenerDownFailure).
func listenerRefusalError(hostPort string, statusCode, retryAfter int, body listenerapi.Error) render.Error {
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	reason := body.Reason
	if reason == "" && statusCode == http.StatusUnauthorized {
		// An undecoded or reason-less body still needs the password text: a
		// 401 only ever means one thing, whatever the body says.
		reason = listenerapi.ReasonPassword
	}
	switch reason {
	case listenerapi.ReasonPassword:
		return render.Error{
			Code: render.CodeAuth,
			Message: fmt.Sprintf("The FreeCAD computer %s asks for a password, and none or a different one was given.",
				host),
			Hint: fmt.Sprintf("Enter the password set in \"Share this PC\" on that computer: use freecad-mcp > "+
				"Connect to FreeCAD on another computer, or run `%s connect --host %s --password-stdin` and "+
				"restart the AI client, or set %s in its config for this server.",
				domain.BinaryName, host, domain.EnvToken) + wslLoopback401Note(host),
		}
	case listenerapi.ReasonRemoteOff:
		return render.Error{
			Code:    render.CodeUnavailable,
			Message: fmt.Sprintf("Remote access is off on %s.", host),
			Hint: fmt.Sprintf("Turn on \"Share this PC\" in %s on that computer, then call get_rpc_status with {}.",
				domain.BinaryName),
		}
	case listenerapi.ReasonFreeCADStarting:
		// The listener has a launch of its own in flight (live check L7):
		// unlike ReasonFreeCADDown, this never suggests start_freecad again,
		// only waiting it out.
		return render.Error{
			Code:    render.CodeUnavailable,
			Message: fmt.Sprintf("FreeCAD is still starting on %s.", host),
			Hint:    "Call get_rpc_status with {} in a few seconds.",
		}
	case listenerapi.ReasonOrigin:
		// A request that looks like it came from a web page: this client
		// never sends one, so this is unexpected; the listener's own text
		// and hint, attributed to it.
		msg := attributedText(host, body.Error)
		if msg == "" {
			msg = fmt.Sprintf("The listener on %s refused this request (HTTP %d).", host, statusCode)
		}
		return render.Error{Code: render.CodeForbidden, Message: msg, Hint: attributedHint(body.Hint)}
	case listenerapi.ReasonRateLimited:
		n := retryAfter
		if n <= 0 {
			n = 60
		}
		return render.Error{
			Code: render.CodeUnavailable,
			Message: fmt.Sprintf("The listener on %s refuses this computer for %d s after too many wrong passwords.",
				host, n),
			Hint: fmt.Sprintf("Check the stored password (freecad-mcp > Connect to FreeCAD on another computer, "+
				"or `%s connect --host %s --password-stdin`), then call get_rpc_status with {} after that time.",
				domain.BinaryName, host),
		}
	case listenerapi.ReasonTestMode:
		return render.Error{
			Code: render.CodeUnavailable,
			Message: fmt.Sprintf("%s is a test listener (freecad-mcp's Share this PC page is testing it); it does "+
				"not forward to FreeCAD.", hostPort),
			Hint: "Call get_rpc_status with {} again once the test on that computer has finished.",
		}
	case listenerapi.ReasonMethod, listenerapi.ReasonContentType, listenerapi.ReasonTooLarge, listenerapi.ReasonBadRequest:
		// A bug in this client (it always sends POST, the right
		// Content-Type and a body within the limit): the listener's own
		// text, attributed to it.
		msg := attributedText(host, body.Error)
		if msg == "" {
			msg = fmt.Sprintf("The listener on %s refused this request (HTTP %d).", host, statusCode)
		}
		return render.Error{
			Code:    render.CodeInternal,
			Message: msg,
			Hint:    fmt.Sprintf("Run `%s doctor` and report the problem.", domain.BinaryName),
		}
	case listenerapi.ReasonProxyFailed:
		// The listener's reverse proxy could not complete the request
		// against the addon (any failure other than the addon actively
		// refusing the connection, which is ReasonFreeCADDown, handled
		// before this function is even reached).
		msg := attributedText(host, body.Error)
		if msg == "" {
			msg = fmt.Sprintf("FreeCAD's RPC server on %s closed the connection.", host)
		}
		return render.Error{
			Code:    render.CodeUnavailable,
			Message: msg,
			Hint:    fmt.Sprintf("Call get_rpc_status with {} to check FreeCAD on %s.", host),
		}
	default:
		// A missing or unknown reason (an undecoded body, a reason this
		// client does not have bespoke wording for, such as not_found or one
		// of /listener/start's own launch failures, which already carry a
		// good message and hint of their own, or a future listener version
		// using a reason this client does not know yet): the listener's own
		// code, text and hint, as they are, the text attributed to it.
		msg := attributedText(host, body.Error)
		if msg == "" {
			msg = fmt.Sprintf("The listener on %s answered but refused the request (HTTP %d).", host, statusCode)
		}
		hint := attributedHint(body.Hint)
		if hint == "" {
			hint = fmt.Sprintf("Run `%s doctor` to check the setup.", domain.BinaryName)
		}
		return render.Error{Code: listenerCode(body.Code), Message: msg, Hint: hint}
	}
}

// listenerCode maps a listener's own JSON error code to the matching
// render.Code* constant to use for it. The listener's codes are always one
// of the render codes already (not_found, invalid_input, authentication,
// forbidden, rate_limited, conflict, unavailable, internal_error), so this
// only needs to fall back to internal_error when the code is empty or not
// one of them (an undecoded body, or a future listener version using a code
// this client does not know yet).
func listenerCode(code string) string {
	switch code {
	case render.CodeNotFound, render.CodeInvalidInput, render.CodeAuth, render.CodeForbidden,
		render.CodeRateLimited, render.CodeConflict, render.CodeUnavailable, render.CodeInternal:
		return code
	default:
		return render.CodeInternal
	}
}

const pingTimeout = 10 * time.Second

const startHint = "Start FreeCAD, select the MCP Addon workbench and click Start RPC Server " +
	"(or turn on its auto-start), then retry. Run `" + domain.BinaryName + " doctor` to check the setup."

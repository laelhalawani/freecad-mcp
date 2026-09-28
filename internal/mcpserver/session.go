package mcpserver

// Session identity and the session lock's errors. Every tools/call carries
// this server's session id and label to
// FreeCAD in the X-FreeCAD-MCP-Session and X-FreeCAD-MCP-Client headers, put
// into the call's context by the sessionIdentity middleware, so no tool
// handler or Connection method takes them as arguments.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
	"github.com/sairaph/mcp-wizard/render"
)

// instanceID identifies this server process: 16 lowercase hex characters
// minted from crypto/rand at start, so a restart is always a new session.
var instanceID = newInstanceID()

func newInstanceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// maxLabelRunes caps the label sent as X-FreeCAD-MCP-Client.
const maxLabelRunes = 80

// releaseOnExitTimeout bounds the best-effort release_session call at
// process shutdown.
const releaseOnExitTimeout = 2 * time.Second

// identity is the session id and label sessionIdentity computed for one
// *mcp.ServerSession, and when that was last seen.
type identity struct {
	id       string
	label    string
	lastSeen time.Time
}

// maxSessionIdentityAge is how long a sessionIdentities entry is kept since
// it was last seen: the longest possible session_timeout_minutes (1440),
// past which any lock that session could still be holding has already
// expired addon-side, so releasing it on this server's exit would be
// pointless.
const maxSessionIdentityAge = 24 * time.Hour

// sessionIdentitiesMu guards sessionIdentities, the identity sessionIdentity
// last computed for each *mcp.ServerSession that has made a tools/call
// (including an exempt one such as get_rpc_status): releaseOnExit reuses it
// to release every session's lock with that session's own headers, because
// by then the run context is already cancelled, no tools/call is in flight
// to compute one, and (for stdio, mcp.Server.Run closes and unregisters the
// session before returning) the SDK's own session list can already be
// empty. Keyed by the session itself rather than a single last-seen pair,
// so a second session's calls (including exempt ones) never overwrite the
// first's identity and cause the wrong session to be released.
//
// An entry for a streamable HTTP session is also removed as soon as that
// session ends (releaseOnSessionEnd, via ServerSession.Wait(), started the
// first time sessionIdentity sees it), which also releases the lock it may
// hold right then instead of leaving it for the full idle timeout;
// not for stdio, where Wait() would only return when the process itself is
// exiting, already handled by releaseOnExit. rememberIdentity additionally
// drops any entry not seen within maxSessionIdentityAge, so a server with
// many short-lived HTTP sessions over a long uptime does not grow this map
// without bound even if a Wait() goroutine is somehow lost (a panic
// recovered elsewhere, a test double).
var (
	sessionIdentitiesMu sync.Mutex
	sessionIdentities   = map[*mcp.ServerSession]identity{}
)

// sessionID returns the session id a tools/call on ss carries: instanceID
// alone for stdio, where ServerSession.ID() is "", else instanceID plus "-"
// plus the SDK's own session id for streamable HTTP.
func sessionID(ss *mcp.ServerSession) string {
	if id := ss.ID(); id != "" {
		return instanceID + "-" + id
	}
	return instanceID
}

// sessionLabel returns "<clientInfo.name> on <hostname>": "agent" when the
// client name is missing, or the session has not sent InitializeParams yet
// (InitializeParams can be nil before initialize); "unknown host" when the
// hostname cannot be read. Every rune that is not printable (control,
// format and separator characters, which would break the header or a
// Report View line, or could be used to make one label imitate another) is
// removed, matching the addon's own filter (ip_filter.py's isprintable()
// check on the client header), and the result is capped at maxLabelRunes
// runes.
func sessionLabel(ss *mcp.ServerSession) string {
	name := "agent"
	if params := ss.InitializeParams(); params != nil && params.ClientInfo != nil && params.ClientInfo.Name != "" {
		name = params.ClientInfo.Name
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown host"
	}
	return capRunes(stripUnprintable(name+" on "+host), maxLabelRunes)
}

// stripUnprintable removes every rune that is not printable (unicode.IsPrint)
// from s, so a label can never split a header or a Report View line, and
// never contains a format or separator character (a no-break space, a
// zero-width character) that the addon's own isprintable() check would
// refuse, which would otherwise fall back to the session id (session_lock.py).
func stripUnprintable(s string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
}

// capRunes truncates s to at most n runes.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// rememberIdentity keeps the identity sessionIdentity computed for ss, so
// releaseOnExit can release ss's lock with its own headers, and drops any
// entry (including a stale one for the same ss) last seen more than
// maxSessionIdentityAge ago.
// rememberIdentity reports whether ss was not already recorded (the first
// tools/call this process has seen from it), so the caller knows whether to
// start its releaseOnSessionEnd watcher.
func rememberIdentity(ss *mcp.ServerSession, id, label string) bool {
	now := time.Now()
	sessionIdentitiesMu.Lock()
	defer sessionIdentitiesMu.Unlock()
	_, existed := sessionIdentities[ss]
	sessionIdentities[ss] = identity{id: id, label: label, lastSeen: now}
	for key, ident := range sessionIdentities {
		if now.Sub(ident.lastSeen) > maxSessionIdentityAge {
			delete(sessionIdentities, key)
		}
	}
	return !existed
}

// recordedIdentities returns every identity rememberIdentity has stored, in
// no particular order.
func recordedIdentities() []identity {
	sessionIdentitiesMu.Lock()
	defer sessionIdentitiesMu.Unlock()
	out := make([]identity, 0, len(sessionIdentities))
	for _, ident := range sessionIdentities {
		out = append(out, ident)
	}
	return out
}

// sessionIdentity is the receiving middleware that puts the session headers
// (freecad.WithSession) into the context of every tools/call: the id is this
// process's instance id, plus "-" and the SDK session id for streamable
// HTTP; the label is "<client name> on <hostname>". A method (not a plain
// function) so it can start releaseOnSessionEnd, which needs s to release
// through the current connection.
func (s *Server) sessionIdentity(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		id, label := instanceID, "agent"
		if ss, ok := req.GetSession().(*mcp.ServerSession); ok && ss != nil {
			id, label = sessionID(ss), sessionLabel(ss)
			if rememberIdentity(ss, id, label) && ss.ID() != "" {
				// A new streamable HTTP session (ss.ID() == "" is stdio,
				// excluded): release its lock as soon as it ends, rather
				// than leaving it held for the full idle timeout after the
				// agent is already gone.
				go s.releaseOnSessionEnd(ss, id, label)
			}
		}
		return next(freecad.WithSession(ctx, id, label), method, req)
	}
}

// releaseOnSessionEnd waits for a streamable HTTP session to end (a client
// DELETE, or its connection closing; whatever ServerSession reports, success
// or error, Wait() returning at all means the connection is gone) and then
// releases the lock it may hold, with its own identity, the same
// release_session call and budget releaseOnExit uses. A client that never
// sends DELETE and just vanishes (a crash, a lost network) is not caught
// here: its lock frees itself at the idle timeout, same as before this
// existed. A no-op when remote access is off or no connection was ever
// made, the same guard releaseOnExit applies.
func (s *Server) releaseOnSessionEnd(ss *mcp.ServerSession, id, label string) {
	_ = ss.Wait()

	sessionIdentitiesMu.Lock()
	delete(sessionIdentities, ss)
	sessionIdentitiesMu.Unlock()

	s.remote.mu.Lock()
	on := s.remote.on
	s.remote.mu.Unlock()
	if !on {
		return
	}
	s.fc.mu.Lock()
	conn := s.fc.conn
	s.fc.mu.Unlock()
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseOnExitTimeout)
	defer cancel()
	_, _ = conn.ReleaseSession(freecad.WithSession(ctx, id, label))
}

// formatSessionDuration renders a duration in whole seconds the way it is
// shown to the user: under a minute in seconds ("45 s"), else rounded
// minutes ("4 min"), matching the FreeCAD status widget's wording.
// formatIdleDuration renders how long the holder has been idle, rounded
// down: an idle time just under a minute boundary is not shown as if it had
// already crossed it.
func formatIdleDuration(seconds int) string {
	return formatSessionDuration(seconds, math.Floor)
}

// formatFreesDuration renders when the lock frees, rounded up: a caller
// told "1 min" should never find it still held a moment later because the
// real value was a few seconds short.
func formatFreesDuration(seconds int) string {
	return formatSessionDuration(seconds, math.Ceil)
}

func formatSessionDuration(seconds int, round func(float64) float64) string {
	if seconds < 0 {
		seconds = 0
	}
	if seconds < 60 {
		return fmt.Sprintf("%d s", seconds)
	}
	minutes := int(round(float64(seconds) / 60))
	return fmt.Sprintf("%d min", minutes)
}

// sessionInUseHint is the fixed hint of a SessionInUseError, whatever hint
// the caller passed.
const sessionInUseHint = "Call get_rpc_status with {} to see who holds FreeCAD and when it frees, then retry " +
	"after that. release_session only frees your own session."

// sessionReleasedHint is the fixed hint of a SessionReleasedError.
const sessionReleasedHint = "Call get_rpc_status with {} to see who holds FreeCAD, then list_documents with {} " +
	"to check the documents before you continue."

// sessionInUseMessage renders the SESSION_IN_USE conflict text: the busy
// wording while the holder has a call or job in flight (idle 0 s,
// frees_in_seconds == timeout_seconds), else the idle wording. ownLabel is
// this call's own label (freecad.WithSession, read from ctx by the caller);
// when it equals the holder's, the fault still means a different session
// (the addon only ever raises SESSION_IN_USE for one, since a call from the
// session that already holds it just refreshes it instead), but an equal
// label is not necessarily this same agent from before a restart: the label
// is "<client name> on <hostname>" (sessionLabel), so two windows of the
// same AI client on one computer, or two agents the same client started,
// share it too, and the label is sender-chosen besides (live-fixes review
// L2). Naming it as "an earlier session of this agent" would tell agent 2,
// wrongly, that it is reading its own stale hold while agent 1 is working
// right now; say only that the name is shared, with both plausible reasons.
func sessionInUseMessage(e *freecad.SessionInUseError, ownLabel string) string {
	if ownLabel != "" && e.Holder == ownLabel {
		who := fmt.Sprintf("another session with the same name (%s), for example this agent before a restart, "+
			"or another window of the same app", e.Holder)
		if e.Busy {
			return fmt.Sprintf("FreeCAD is in use by %s, working now. It frees %s after that agent's last call, "+
				"or sooner if it releases it. The person at the FreeCAD computer can also free it with Force "+
				"release.", who, formatFreesDuration(e.FreesInSeconds))
		}
		return fmt.Sprintf("FreeCAD is in use by %s, idle %s. It frees in %s if that agent stays idle, or sooner "+
			"if it releases it.", who, formatIdleDuration(e.IdleSeconds), formatFreesDuration(e.FreesInSeconds))
	}
	if e.Busy {
		return fmt.Sprintf("FreeCAD is in use by another agent (%s, working now). It frees %s after that agent's "+
			"last call, or sooner if it releases it. The person at the FreeCAD computer can also free it with "+
			"Force release.", e.Holder, formatFreesDuration(e.FreesInSeconds))
	}
	return fmt.Sprintf("FreeCAD is in use by another agent (%s, idle %s). It frees in %s if that agent stays "+
		"idle, or sooner if it releases it.", e.Holder, formatIdleDuration(e.IdleSeconds), formatFreesDuration(e.FreesInSeconds))
}

// callerLabel returns the label this call's own context carries
// (freecad.WithSession put it into the X-FreeCAD-MCP-Client header, read
// back the same way xmlrpc.Client.Call sends it), or "" when ctx carries
// none (a call made before any session identity was attached).
func callerLabel(ctx context.Context) string {
	return xmlrpc.HeadersFrom(ctx)[domain.HeaderClient]
}

// sessionFailure renders *freecad.SessionInUseError and
// *freecad.SessionReleasedError as conflict errors with their fixed texts,
// whatever hint the caller passed; nil for any other error. ctx is read only
// for the caller's own label, to word a SESSION_IN_USE conflict against the
// agent's own earlier session (sessionInUseMessage).
func sessionFailure(ctx context.Context, err error) *mcp.CallToolResult {
	var inUse *freecad.SessionInUseError
	if errors.As(err, &inUse) {
		return render.ErrorResult(render.Error{
			Code:    render.CodeConflict,
			Message: sessionInUseMessage(inUse, callerLabel(ctx)),
			Hint:    sessionInUseHint,
			Fields: map[string]any{
				"holder":           inUse.Holder,
				"idle_seconds":     inUse.IdleSeconds,
				"frees_in_seconds": inUse.FreesInSeconds,
				"busy":             inUse.Busy,
			},
		})
	}
	var released *freecad.SessionReleasedError
	if errors.As(err, &released) {
		return render.ErrorResult(render.Error{
			Code: render.CodeConflict,
			Message: "The person at the FreeCAD computer released your session; other agents may have changed " +
				"FreeCAD since. Your next call takes the session again if it is free.",
			Hint: sessionReleasedHint,
		})
	}
	return nil
}

// releaseOnExit frees every session lock this server's own tools/calls may
// hold (release_session), when the server stops, best effort within a
// shared 2 s budget, with a fresh context that carries each session's own
// headers (the run context is cancelled by then). It does nothing while the
// lock is off or FreeCAD was never reached. A streamable HTTP server can
// have served several client sessions; each is released with its own id and
// label, since any of them (not only the most recent) may hold the lock,
// and releasing one this server never held is a harmless no-op.
func (s *Server) releaseOnExit() {
	s.remote.mu.Lock()
	on := s.remote.on
	s.remote.mu.Unlock()
	if !on {
		return
	}
	s.fc.mu.Lock()
	conn := s.fc.conn
	s.fc.mu.Unlock()
	if conn == nil {
		return
	}
	identities := recordedIdentities()
	if len(identities) == 0 {
		// No tools/call was ever attributed to a session (should not happen
		// once conn is cached, since every path that connects runs through
		// sessionIdentity first): fall back to the process's own identity.
		identities = []identity{{id: instanceID, label: "agent"}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseOnExitTimeout)
	defer cancel()
	// Released concurrently, all under the one deadline: only one identity
	// can actually hold the lock, and serially trying every remembered one
	// (most long gone) could exhaust the budget before reaching it.
	var wg sync.WaitGroup
	for _, ident := range identities {
		wg.Add(1)
		go func(ident identity) {
			defer wg.Done()
			_, _ = conn.ReleaseSession(freecad.WithSession(ctx, ident.id, ident.label))
		}(ident)
	}
	wg.Wait()
}

package listener

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/mcp-wizard/render"
)

// handler is the listener's HTTP handler: the guard chain (allowed_ips is
// enforced earlier, by allowedListener, and re-checked here for a
// keep-alive connection that outlived a settings change), then the three
// paths it serves.
type handler struct {
	settings *settingsCache
	rpcPort  int
	launcher *freecad.Launcher
	proxy    *httputil.ReverseProxy
	version  string
	once     bool
	log      *log.Logger
	rlog     *rateLimitedLog
	backoff  *backoff
}

// ServeHTTP runs the guard chain in order, each check returning at the
// first one that applies; only a request that passes every one reaches a
// path handler (or the final unknown-path 404), so an unauthenticated
// caller cannot use the response to tell a real path from a made-up one.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	addr, ok := parseRemoteIP(r.RemoteAddr)
	snap := h.settings.snapshot()
	if !ok || !allowedIn(snap, addr) {
		// The address passed the allow-list check when this connection was
		// accepted (allowedListener.Accept), but the list has since
		// changed: a keep-alive connection can outlive that change. Abort
		// with no response at all, the same as a rejected connection at
		// Accept, rather than answer anything on it.
		panic(http.ErrAbortHandler)
	}

	// Every response from here on carries the marker; the /RPC2 down-state
	// overwrites it to domain.ListenerFreeCADDown (proxy.go).
	w.Header().Set(domain.HeaderListener, domain.ListenerMarker)

	if !snap.Settings.RemoteEnabled && !h.once {
		h.reject(w, r, http.StatusForbidden, render.CodeForbidden, listenerapi.ReasonRemoteOff,
			"Remote access is turned off on this computer.",
			`Turn it on in freecad-mcp > Share this PC on that computer.`)
		return
	}

	if r.Header.Get("Origin") != "" {
		h.reject(w, r, http.StatusForbidden, render.CodeForbidden, listenerapi.ReasonOrigin,
			"Requests from web pages are not accepted.", "")
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.reject(w, r, http.StatusMethodNotAllowed, render.CodeInvalidInput, listenerapi.ReasonMethod,
			"Only POST is accepted.", "")
		return
	}

	path := r.URL.Path
	switch path {
	case listenerapi.PathRPC:
		if !hasContentType(r.Header.Get("Content-Type"), "text/xml", "application/xml") {
			h.reject(w, r, http.StatusUnsupportedMediaType, render.CodeInvalidInput, listenerapi.ReasonContentType,
				"Content-Type must be text/xml or application/xml.", "")
			return
		}
	case listenerapi.PathStatus, listenerapi.PathStart:
		if !hasContentType(r.Header.Get("Content-Type"), listenerapi.ContentTypeJSON) {
			h.reject(w, r, http.StatusUnsupportedMediaType, render.CodeInvalidInput, listenerapi.ReasonContentType,
				"Content-Type must be application/json.", "")
			return
		}
	}
	// A path this switch does not know keeps no content-type requirement:
	// it falls through to backoff and password below, same as a known one,
	// so only the final unknown-path 404 tells it apart from a real one.

	// The password is compared first (constant time, no side effects), then
	// the block state is checked and any failure recorded in one locked
	// step, so two concurrent requests from the same address cannot both
	// slip past the limit between checking it and recording their own
	// failure. Only a request that carried an Authorization header at all
	// counts toward the limit, so a caller that never attempted a password
	// cannot be blocked by it.
	hasAuth := r.Header.Get("Authorization") != ""
	passwordOK := snap.Settings.AuthToken == "" || checkPassword(r, snap.Settings.AuthToken)
	if h.backoff.checkAndRecord(addr, passwordOK, hasAuth) {
		w.Header().Set("Retry-After", "60")
		h.reject(w, r, http.StatusTooManyRequests, render.CodeRateLimited, listenerapi.ReasonRateLimited,
			"Too many failed passwords from this address; try again in a minute.", "")
		return
	}
	if !passwordOK {
		w.Header().Set("WWW-Authenticate", `Bearer realm="freecad-mcp"`)
		// hasAuth tells a wrong password (live check N1: "The password was
		// not accepted.") apart from none sent at all ("A password is
		// required."), the same distinction connect and share already make
		// in their own wording for the same two cases.
		msg := "A password is required."
		if hasAuth {
			msg = "The password was not accepted."
		}
		h.reject(w, r, http.StatusUnauthorized, render.CodeAuth, listenerapi.ReasonPassword, msg, "")
		return
	}

	switch path {
	case listenerapi.PathStatus, listenerapi.PathStart:
		r.Body = http.MaxBytesReader(w, r.Body, listenerapi.MaxJSONBodyBytes)
	case listenerapi.PathRPC:
		if r.ContentLength > listenerapi.MaxBodyBytes {
			// Answered up front from the declared length, before even
			// trying to proxy a request that can only fail once read.
			h.reject(w, r, http.StatusRequestEntityTooLarge, render.CodeInvalidInput, listenerapi.ReasonTooLarge,
				"The request body is too large.", "")
			return
		}
		if h.once {
			writeListenerError(w, http.StatusServiceUnavailable, render.CodeUnavailable, listenerapi.ReasonTestMode,
				"This is a test listener; it does not forward to FreeCAD.", "")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, listenerapi.MaxBodyBytes)
	}

	switch path {
	case listenerapi.PathStatus:
		h.handleStatus(w, r, snap)
	case listenerapi.PathStart:
		h.handleStart(w, r, snap)
	case listenerapi.PathRPC:
		h.proxy.ServeHTTP(w, r)
	default:
		h.reject(w, r, http.StatusNotFound, render.CodeNotFound, listenerapi.ReasonNotFound, "Not found.", "")
	}
}

// reject writes a listenerapi.Error reply (Code and Reason together tell a
// client the exact refusal apart from any other with the same status) and
// logs it with the caller's IP, the exact request path and the reason,
// never the password, at most once per minute per address bucket and
// reason (so a caller retrying two different refusals is not silenced by
// the throttle on the first one).
func (h *handler) reject(w http.ResponseWriter, r *http.Request, status int, code, reason, message, hint string) {
	writeListenerError(w, status, code, reason, message, hint)
	ip := clientIP(r)
	key := reason
	if addr, ok := parseRemoteIP(r.RemoteAddr); ok {
		key = backoffKey(addr) + "|" + reason
	}
	h.rlog.Printf(key, "listener: rejected %s %s %q: %s", ip, r.Method, r.URL.EscapedPath(), reason)
}

func writeListenerError(w http.ResponseWriter, status int, code, reason, message, hint string) {
	w.Header().Set("Content-Type", listenerapi.ContentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(listenerapi.Error{Code: code, Error: message, Hint: hint, Reason: reason})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", listenerapi.ContentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// hasContentType reports whether header names one of want, ignoring
// parameters (charset and the like) and case.
func hasContentType(header string, want ...string) bool {
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	for _, w := range want {
		if strings.EqualFold(mt, w) {
			return true
		}
	}
	return false
}

// checkPassword compares the request's Authorization header against token:
// Bearer, or Basic with token as the password (any username).
func checkPassword(r *http.Request, token string) bool {
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return constantTimeEqual(v, token)
	}
	if _, pass, ok := r.BasicAuth(); ok {
		return constantTimeEqual(pass, token)
	}
	return false
}

// constantTimeEqual compares a and b without leaking either one's length
// through timing: both are hashed to a fixed-size digest first, so the
// comparison itself always runs over the same number of bytes regardless of
// how long the password or the guess is.
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// clientIP returns the request's remote address without its port, for
// logging and the rate-limited rejection log.
func clientIP(r *http.Request) string {
	if addr, ok := parseRemoteIP(r.RemoteAddr); ok {
		return addr.String()
	}
	return r.RemoteAddr
}

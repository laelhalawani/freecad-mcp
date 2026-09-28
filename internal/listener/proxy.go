package listener

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/mcp-wizard/render"
)

// newRPCProxy forwards /RPC2 to the addon's XML-RPC server on
// 127.0.0.1:rpcPort byte for byte. Rewrite only calls SetURL, so Host
// becomes 127.0.0.1:<rpc_port> (the addon's loopback guard accepts that)
// and every other header, including Authorization and the session headers,
// passes through unchanged; it never calls SetXForwarded, so
// X-Forwarded-For is neither set nor trusted.
func newRPCProxy(rpcPort int, h *handler) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(rpcPort))}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
		},
		ErrorHandler: h.proxyError,
		ErrorLog:     h.log,
	}
}

// proxyError classifies a failure to reach or read from the addon: a
// request body over the limit is 413; a refused or timed-out dial to the
// addon's port (nothing listening, meaning FreeCAD is not running) is the
// 503 down-state carrying domain.ListenerFreeCADDown; any other failure,
// including one reading the client's own request body, is a generic 502
// rather than the down-state marker, since it says nothing about whether
// FreeCAD is actually running.
func (h *handler) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		h.reject(w, r, http.StatusRequestEntityTooLarge, render.CodeInvalidInput, listenerapi.ReasonTooLarge,
			"The request body is too large.", "")
		return
	}
	if addonUnreachable(err) {
		if h.launchInFlight() {
			// A launch this listener started (/listener/start) has not
			// reached the addon's RPC port yet: say so, rather than
			// "FreeCAD is not running", which would read as nothing having
			// been done about it and could prompt another start_freecad
			// (live check L7). The marker stays the plain "1": this is a
			// refusal with a reason, not the down-state header, which
			// callers treat as license to drop a cached connection and
			// retry the version check once FreeCAD is back.
			writeListenerError(w, http.StatusServiceUnavailable, render.CodeUnavailable, listenerapi.ReasonFreeCADStarting,
				"FreeCAD is still starting on this computer.",
				"Call get_rpc_status with {} in a few seconds.")
			h.logProxyError(r, listenerapi.ReasonFreeCADStarting, "listener: RPC2 from %s: FreeCAD is still starting: %v", clientIP(r), err)
			return
		}
		w.Header().Set(domain.HeaderListener, domain.ListenerFreeCADDown)
		writeListenerError(w, http.StatusServiceUnavailable, render.CodeUnavailable, listenerapi.ReasonFreeCADDown,
			"FreeCAD is not running on this computer.",
			"Call start_freecad with {} to start it, then get_rpc_status with {}.")
		// Rate limited like a rejection (live check N5): an agent polling
		// get_rpc_status every few seconds while FreeCAD is down would
		// otherwise fill the log with this one line, once per poll.
		h.logProxyError(r, listenerapi.ReasonFreeCADDown, "listener: RPC2 from %s: FreeCAD is not running: %v", clientIP(r), err)
		return
	}
	writeListenerError(w, http.StatusBadGateway, render.CodeUnavailable, listenerapi.ReasonProxyFailed,
		"FreeCAD's RPC server closed the connection.", "")
	h.logProxyError(r, listenerapi.ReasonProxyFailed, "listener: RPC2 from %s: proxy error: %v", clientIP(r), err)
}

// logProxyError logs a /RPC2 proxy failure at most once per client bucket
// and reason per minute, the same throttling h.reject already gives every
// other refusal (live check N5): unlike those, /RPC2 has no session-level
// backoff of its own to lean on, so a client that keeps polling while
// FreeCAD is down or starting would otherwise write one line per poll.
func (h *handler) logProxyError(r *http.Request, reason, format string, args ...any) {
	key := reason
	if addr, ok := parseRemoteIP(r.RemoteAddr); ok {
		key = backoffKey(addr) + "|" + reason
	}
	h.rlog.Printf(key, format, args...)
}

// launchInFlight reports whether this listener started a FreeCAD launch
// that has not exited and is still within its starting window (the same
// test connection.go's unreachableError, in the MCP server, makes of its own
// local launcher for the same purpose): the addon's RPC port not yet
// answering is then expected, not a sign FreeCAD failed to start.
func (h *handler) launchInFlight() bool {
	ls := h.launcher.State()
	return (ls.State == freecad.LaunchStarted || ls.State == freecad.LaunchForwarded) &&
		time.Since(ls.StartedAt) < freecad.StartingWindow
}

// addonUnreachable reports whether err means nothing answers on the addon's
// RPC port at all: a dial that was refused or itself timed out (this also
// covers a refusal on Windows). It is deliberately narrower than "any
// timeout", so a slow or failed read of the client's own request, which can
// also surface as a timeout, is never misreported as FreeCAD being down.
// context.Canceled is excluded too: a dial cancelled because the client
// itself left before it finished says nothing about whether FreeCAD is
// actually running.
func addonUnreachable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

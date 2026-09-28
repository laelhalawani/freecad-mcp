package listener

import (
	"net"
	"net/netip"
)

// allowedListener enforces the allowed IP list plus loopback before any byte
// is read from a connection: a rejected address is closed at once and never
// reaches the HTTP server, so it gets no response at all, not even a
// malformed one. A change to the allow list after a connection was accepted
// (a keep-alive connection can outlive it) is caught again per request by
// handler.ServeHTTP.
type allowedListener struct {
	net.Listener
	settings *settingsCache
	rlog     *rateLimitedLog
}

// Accept only returns connections from an allowed address. It never returns
// an error for a rejected one (http.Server.Serve stops entirely on an Accept
// error), so it loops internally until an allowed connection arrives or the
// underlying listener itself fails or is closed.
func (l *allowedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		remote := conn.RemoteAddr().String()
		addr, ok := parseRemoteIP(remote)
		if !ok || !l.settings.allowed(addr) {
			host, key := remote, remote
			if ok {
				host = addr.String()
				key = backoffKey(addr) + "|not_allowed"
			}
			l.rlog.Printf(key, "listener: rejected %s: not in the allowed IP list", host)
			conn.Close()
			continue
		}
		return conn, nil
	}
}

// parseRemoteIP parses the host part of a "host:port" remote address, as
// net.Conn.RemoteAddr and http.Request.RemoteAddr format it.
func parseRemoteIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	return addr, err == nil
}

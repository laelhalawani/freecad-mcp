package listener

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

// bindListeners opens the listener's socket(s) from the allowed list: every
// interface unless the list is loopback only, in which case it always binds
// 127.0.0.1 (loopback is implicitly allowed anyway, settingsCache.allowedIn,
// so every local check that probes 127.0.0.1, such as the doctor, the
// FreeCAD addon's own warning, Share's Test, and the listener's own
// single-instance tie-break, keeps working whatever family the allowed list
// happens to name), plus [::1] (a second, independent net.Listen) when the
// list also names an IPv6 loopback address (for example allowed_ips =
// "::1"): a socket bound to one loopback address does not accept
// connections to the other, so naming only ::1 would otherwise leave
// 127.0.0.1 unreachable.
func bindListeners(port int, allowed []netip.Prefix) ([]net.Listener, error) {
	if !domain.LoopbackOnly(allowed) {
		addr := fmt.Sprintf(":%d", port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("bind %s: %w", addr, err)
		}
		return []net.Listener{ln}, nil
	}

	addrs := []string{fmt.Sprintf("127.0.0.1:%d", port)}
	if loopbackIncludesIPv6(allowed) {
		addrs = append(addrs, fmt.Sprintf("[::1]:%d", port))
	}

	lns := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, opened := range lns {
				opened.Close()
			}
			return nil, fmt.Errorf("bind %s: %w", addr, err)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// loopbackIncludesIPv6 reports whether a loopback-only allowed list
// (domain.LoopbackOnly already true for it) names an IPv6 loopback address.
func loopbackIncludesIPv6(allowed []netip.Prefix) bool {
	for _, p := range allowed {
		if a := p.Addr().Unmap(); a.Is6() {
			return true
		}
	}
	return false
}

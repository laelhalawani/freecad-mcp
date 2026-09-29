package listener

import (
	"net/netip"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
)

// TestLoopbackIsAdmittedWhateverTheAllowedList covers start_freecad from a
// Windows session without a desktop, which asks the listener on 127.0.0.1 to
// start FreeCAD: allowed_ips can never refuse that caller.
func TestLoopbackIsAdmittedWhateverTheAllowedList(t *testing.T) {
	snap := snapshot{
		Settings:        addoninstall.DefaultRemoteSettings(),
		AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
	}
	for addr, want := range map[string]bool{
		"127.0.0.1":        true,
		"::1":              true,
		"::ffff:127.0.0.1": true,
		"192.0.2.7":        true,
		"198.51.100.7":     false,
	} {
		if got := allowedIn(snap, netip.MustParseAddr(addr)); got != want {
			t.Errorf("allowedIn(%s) = %v, want %v", addr, got, want)
		}
	}
	// A list that could not be read leaves loopback alone allowed.
	if !allowedIn(snapshot{}, netip.MustParseAddr("127.0.0.1")) {
		t.Error("loopback refused while the allowed list is unusable")
	}
}

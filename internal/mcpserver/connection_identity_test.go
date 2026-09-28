package mcpserver

import (
	"os"
	"runtime"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

// TestIdentityMismatchWarning covers the final live check's L11 rule: for a
// loopback endpoint, the answering side is flagged as a mismatch when its
// reported hostname differs from this computer's own, or its platform/WSL
// state does (a second, independent signal, since WSL takes the Windows
// computer's own name by default), and never otherwise. Derives its
// expectations from this machine's own os.Hostname/runtime.GOOS/IsWSL,
// rather than hardcoded values, so it holds on any computer it runs on.
func TestIdentityMismatchWarning(t *testing.T) {
	here, err := os.Hostname()
	if err != nil || here == "" {
		t.Skip("os.Hostname unavailable in this environment")
	}
	wsl := domain.IsWSL()

	if got := identityMismatchWarning("127.0.0.1", map[string]any{
		"hostname": here, "platform": runtime.GOOS, "wsl": wsl,
	}); got != "" {
		t.Fatalf("matching hostname and platform/WSL warned: %q", got)
	}

	if got := identityMismatchWarning("127.0.0.1", map[string]any{
		"hostname": here + "-other", "platform": runtime.GOOS, "wsl": wsl,
	}); got == "" {
		t.Fatal("a different hostname on a loopback host did not warn")
	}

	// Same hostname (as WSL reports by default) but platform/WSL differs:
	// still a mismatch, the independent second signal (live-fixes review
	// M1).
	if got := identityMismatchWarning("127.0.0.1", map[string]any{
		"hostname": here, "platform": runtime.GOOS, "wsl": !wsl,
	}); got == "" {
		t.Fatal("a different WSL state with a matching hostname did not warn")
	}

	// A non-loopback configured host is never checked: the caller
	// deliberately named another computer, so there is no "not this PC"
	// ambiguity to catch.
	if got := identityMismatchWarning("192.168.1.50", map[string]any{
		"hostname": here + "-other", "platform": runtime.GOOS, "wsl": wsl,
	}); got != "" {
		t.Fatalf("a non-loopback host warned: %q", got)
	}

	// No status at all (an older addon, or the hostname call failed there):
	// nothing to compare, never warns.
	if got := identityMismatchWarning("127.0.0.1", nil); got != "" {
		t.Fatalf("nil status warned: %q", got)
	}
}

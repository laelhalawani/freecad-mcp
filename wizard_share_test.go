package main

import (
	"strings"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
)

// initShareChoice runs shareChoiceStep.Init against a target primed with
// the given settings, returning the resulting share state.
func initShareChoice(t *testing.T, settings addoninstall.RemoteSettings) shareState {
	t.Helper()
	dataDir := t.TempDir()
	target := addoninstall.Target{UserDataDir: dataDir}
	if err := addoninstall.UpdateSettings(target, map[string]any{
		addoninstall.KeyRemoteEnabled: settings.RemoteEnabled,
		addoninstall.KeyAllowedIPs:    settings.AllowedIPs,
		addoninstall.KeyAuthToken:     settings.AuthToken,
	}); err != nil {
		t.Fatal(err)
	}
	state := &AppState{Addon: addonState{Phase: addonChoosing, Targets: []addoninstall.Target{target}}}
	step := &shareChoiceStep{}
	step.Init(state)
	return state.Share
}

// TestShareChoiceKeepsCustomAllowedIPsAfterOff covers live check L8: a
// saved list other than the plain default is kept exactly, whether or not
// remote access is on, so turning sharing off and back on never drops it.
func TestShareChoiceKeepsCustomAllowedIPsAfterOff(t *testing.T) {
	const custom = "192.168.1.0/24, 172.22.208.0/20"
	got := initShareChoice(t, addoninstall.RemoteSettings{RemoteEnabled: false, AllowedIPs: custom})
	if got.AllowedIPs != custom {
		t.Fatalf("AllowedIPs = %q, want %q", got.AllowedIPs, custom)
	}
}

// TestShareChoiceKeepsLoopbackWhileOn covers live check L8, build 2: while
// remote access is already on, a saved plain default (127.0.0.1) is the
// loopback-only SSH-tunnel setup, chosen on purpose, so it is kept rather
// than replaced with a LAN subnet suggestion.
func TestShareChoiceKeepsLoopbackWhileOn(t *testing.T) {
	got := initShareChoice(t, addoninstall.RemoteSettings{
		RemoteEnabled: true, AllowedIPs: addoninstall.DefaultAllowedIPs,
	})
	if got.AllowedIPs != addoninstall.DefaultAllowedIPs {
		t.Fatalf("AllowedIPs = %q, want the loopback default kept as is", got.AllowedIPs)
	}
}

// TestShareChoiceSuggestsWhenOffAndNeverCustomized covers live check L8:
// remote access off and nothing but the plain default ever saved is not a
// deliberate choice, so this screen suggests a LAN subnet instead of
// keeping "127.0.0.1" (which would otherwise silently limit sharing to
// loopback only once turned on).
func TestShareChoiceSuggestsWhenOffAndNeverCustomized(t *testing.T) {
	got := initShareChoice(t, addoninstall.RemoteSettings{
		RemoteEnabled: false, AllowedIPs: addoninstall.DefaultAllowedIPs,
	})
	want := strings.Join(suggestedAllowedIPs(), ", ")
	if want == "" {
		want = addoninstall.DefaultAllowedIPs
	}
	if got.AllowedIPs != want {
		t.Fatalf("AllowedIPs = %q, want the suggested list %q (not the kept default)", got.AllowedIPs, want)
	}
}

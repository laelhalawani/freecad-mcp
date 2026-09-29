package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
)

func TestASecondInstallOfTheSameAddonWritesNothing(t *testing.T) {
	target := addoninstall.Target{UserDataDir: t.TempDir()}
	if r := installAddon(target, true); r.Err != nil || r.Current {
		t.Fatalf("first install = %+v", r)
	}
	// What FreeCAD adds to the folder once it has loaded the addon.
	cache := filepath.Join(target.AddonDir(), "rpc_server", "__pycache__")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "rpc_server.cpython-311.pyc"), []byte("bytecode"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An old time on a file the install would rewrite shows whether it did.
	version := filepath.Join(target.AddonDir(), "rpc_server", "version.py")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(version, old, old); err != nil {
		t.Fatal(err)
	}

	r := installAddon(target, true)
	if r.Err != nil || !r.Current || r.Replaced || r.SettingChanged {
		t.Fatalf("second install = %+v, want the current addon left alone", r)
	}
	if info, err := os.Stat(version); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("version.py was rewritten: %v, %v", info, err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("the bytecode folder FreeCAD wrote was removed: %v", err)
	}
	var out bytes.Buffer
	reportAddonResults(&out, []addonResult{r})
	if !strings.Contains(out.String(), "already current") || strings.Contains(out.String(), "updated") ||
		strings.Contains(out.String(), "Restart FreeCAD") {
		t.Fatalf("report = %q", out.String())
	}

	// A setting the person asks for still applies to a current addon.
	r = installAddon(target, false)
	if r.Err != nil || !r.Current || !r.SettingChanged || r.AutoStart {
		t.Fatalf("install turning auto-start off = %+v", r)
	}
}

func TestADryRunOnACurrentAddonSaysItIsCurrent(t *testing.T) {
	target := addoninstall.Target{UserDataDir: t.TempDir()}
	var out bytes.Buffer
	reportAddonDryRun(&out, target)
	if !strings.Contains(out.String(), "would install the FreeCAD addon into "+target.AddonDir()) {
		t.Fatalf("dry run without an addon = %q", out.String())
	}
	if r := installAddon(target, true); r.Err != nil {
		t.Fatal(r.Err)
	}
	out.Reset()
	reportAddonDryRun(&out, target)
	if !strings.Contains(out.String(), "already current: addon") || strings.Contains(out.String(), "would") {
		t.Fatalf("dry run on a current addon = %q", out.String())
	}
	if code := installAddonReport(context.Background(), &out, []addoninstall.Target{target}, true); code != 0 ||
		strings.Contains(out.String(), "would install") {
		t.Fatalf("install --dry-run = %d, %q", code, out.String())
	}
}

func TestAnEditedAddonIsRewrittenByInstall(t *testing.T) {
	target := addoninstall.Target{UserDataDir: t.TempDir()}
	if r := installAddon(target, true); r.Err != nil {
		t.Fatal(r.Err)
	}
	version := filepath.Join(target.AddonDir(), "rpc_server", "version.py")
	if err := os.WriteFile(version, []byte("# edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := installAddon(target, true); r.Err != nil || r.Current || !r.Replaced {
		t.Fatalf("install over an edited addon = %+v", r)
	}
	if !addoninstall.SameContent(target) {
		t.Fatal("the addon was not restored")
	}
}

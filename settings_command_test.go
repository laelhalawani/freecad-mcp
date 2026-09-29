package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
)

func runSettingsCommand(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code = settingsCommand(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestSettingsCommandPrintsWritesAndKeepsTheOtherKeys(t *testing.T) {
	dir := t.TempDir()
	target := addoninstall.Target{UserDataDir: dir}
	file := addoninstall.SettingsPath(target)
	before := `{"session_timeout_minutes": 12, "auth_token": "secret", "allowed_ips": "10.0.0.0/24", "auto_start_rpc": true}`
	if err := os.WriteFile(file, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runSettingsCommand(t, "--user-data-dir", dir)
	if code != 0 || !strings.Contains(out, "background_after_minutes: 30") || errOut != "" {
		t.Fatalf("print: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if data, _ := os.ReadFile(file); string(data) != before {
		t.Fatalf("printing changed the file: %s", data)
	}

	for _, bad := range []string{"0", "1441", "-5"} {
		code, _, errOut := runSettingsCommand(t, "--user-data-dir", dir, "--background-after", bad)
		if code != 2 || !strings.Contains(errOut, "must be 1 to 1440") {
			t.Errorf("--background-after %s: exit %d, stderr %q", bad, code, errOut)
		}
	}
	if data, _ := os.ReadFile(file); string(data) != before {
		t.Fatalf("a rejected value changed the file: %s", data)
	}

	code, out, errOut = runSettingsCommand(t, "--user-data-dir", dir, "--background-after", "45")
	if code != 0 || !strings.Contains(out, "background_after_minutes: 45") || errOut != "" {
		t.Fatalf("write: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	var saved map[string]any
	data, _ := os.ReadFile(file)
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"session_timeout_minutes": 12.0, "auth_token": "secret", "allowed_ips": "10.0.0.0/24", "auto_start_rpc": true, "background_after_minutes": 45.0}
	if len(saved) != len(want) {
		t.Fatalf("saved %v, want %v", saved, want)
	}
	for k, v := range want {
		if saved[k] != v {
			t.Errorf("key %s = %v, want %v", k, saved[k], v)
		}
	}
	if code, out, _ := runSettingsCommand(t, "--user-data-dir", dir); code != 0 || !strings.Contains(out, "background_after_minutes: 45") {
		t.Fatalf("print after write: exit %d, stdout %q", code, out)
	}
}

func TestSettingsCommandRefusesExtraArguments(t *testing.T) {
	code, _, errOut := runSettingsCommand(t, "--user-data-dir", t.TempDir(), "60")
	if code != 2 || !strings.Contains(errOut, `unexpected argument "60"`) {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

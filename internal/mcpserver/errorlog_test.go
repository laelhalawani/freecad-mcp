package mcpserver

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/freecad-mcp/internal/xmlrpc/xmlrpctest"
)

func TestFailedToolCallIsLogged(t *testing.T) {
	fc := addon(t, map[string]xmlrpctest.Handler{
		"edit_object": func([]any) (any, error) {
			return map[string]any{"success": false, "error": "Object 'Nope' not found"}, nil
		},
	})
	cs := session(t, settingsFor(fc))
	long := strings.Repeat("x", 500)
	call(t, cs, "update_object", map[string]any{"doc_name": "D", "obj_name": long, "obj_properties": map[string]any{"Length": 5}})

	lines, err := RecentErrors(10)
	if err != nil || len(lines) == 0 {
		t.Fatalf("error log = %v, %v", lines, err)
	}
	line := lines[len(lines)-1]
	for _, want := range []string{
		"tool=update_object", "code=freecad_error", `message="Failed to update object: Object 'Nope' not found"`, "hint=", `client="test on `, "session=",
		`"doc_name":"D"`, `"obj_properties":"{\"Length\":5}"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %s:\n%s", want, line)
		}
	}
	// The hint the addon built repeats the name in full; the argument summary cuts it.
	args := line[strings.LastIndex(line, " args="):]
	if strings.Contains(args, long) || !strings.Contains(args, `"`+strings.Repeat("x", errorLogArgChars)+`"`) {
		t.Errorf("an argument value was not cut to %d characters:\n%s", errorLogArgChars, args)
	}
	if strings.Count(line, "\n") != 0 {
		t.Errorf("log entry spans lines: %q", line)
	}
	if _, err := time.Parse("2006-01-02T15:04:05Z", strings.Fields(line)[0]); err != nil {
		t.Errorf("log line does not start with a UTC time: %v", err)
	}
}

func TestSuccessfulToolCallIsNotLogged(t *testing.T) {
	before, _ := RecentErrors(1000)
	fc := addon(t, nil)
	cs := session(t, settingsFor(fc))
	call(t, cs, "get_rpc_status", nil)
	after, _ := RecentErrors(1000)
	if len(after) != len(before) {
		t.Fatalf("a call that succeeded was logged: %v", after[len(before):])
	}
}

func TestRefusedArgumentsAreLogged(t *testing.T) {
	fc := addon(t, nil)
	cs := session(t, settingsFor(fc))
	call(t, cs, "get_view", map[string]any{"view_name": "Sideways"})
	lines, _ := RecentErrors(1)
	if len(lines) != 1 || !strings.Contains(lines[0], "tool=get_view") || !strings.Contains(lines[0], "code=invalid_input") ||
		!strings.Contains(lines[0], `"view_name":"Sideways"`) {
		t.Fatalf("log = %v", lines)
	}
}

func TestSecretArgumentsAreNeverLogged(t *testing.T) {
	got := argumentSummary([]byte(`{"auth_token": "hunter2", "Password": "hunter3", "code": "print(1)"}`))
	if strings.Contains(got, "hunter") || !strings.Contains(got, `"code":"print(1)"`) {
		t.Fatalf("summary = %s", got)
	}
}

func TestErrorLogRotatesPastFiveMegabytes(t *testing.T) {
	path, err := ErrorLogPath()
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	os.Remove(path + ".1")
	appendErrorLog("first")
	if err := os.WriteFile(path, []byte(strings.Repeat("y", errorLogMaxBytes+1)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appendErrorLog("second")
	appendErrorLog("third")

	old, err := os.Stat(path + ".1")
	if err != nil || old.Size() != errorLogMaxBytes+2 {
		t.Fatalf("rotated copy = %v, %v", old, err)
	}
	lines, err := RecentErrors(2)
	if err != nil || strings.Join(lines, "|") != "second|third" {
		t.Fatalf("recent = %v, %v", lines, err)
	}
	// The rotated copy is read first, oldest entries first, when the current file is short.
	lines, _ = RecentErrors(3)
	if len(lines) != 3 || lines[1] != "second" {
		t.Fatalf("recent across the rotation = %d lines", len(lines))
	}
}

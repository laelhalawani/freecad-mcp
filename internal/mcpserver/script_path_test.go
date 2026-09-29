package mcpserver

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/headless"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc/xmlrpctest"
)

var regexpHeadlessID = regexp.MustCompile(`headless-[0-9a-f]{8}`)

func TestScriptIsPassedAsCodeOrAsPathNotBoth(t *testing.T) {
	// The rule is checked before FreeCAD is contacted: the host below answers nothing.
	cs := session(t, domain.Settings{Host: "127.0.0.1", Port: 9})
	for _, tool := range []string{"execute_code", "execute_code_async", "execute_code_headless"} {
		both := call(t, cs, tool, map[string]any{"code": "pass", "path": "/tmp/s.py"})
		if !both.IsError || !strings.Contains(allText(both), "invalid_input") || !strings.Contains(allText(both), "Pass code or path, not both") {
			t.Errorf("%s with code and path (error %v): %s", tool, both.IsError, allText(both))
		}
		neither := call(t, cs, tool, map[string]any{})
		if !neither.IsError || !strings.Contains(allText(neither), "invalid_input") || !strings.Contains(allText(neither), "Pass the script as code or as path") {
			t.Errorf("%s with neither (error %v): %s", tool, neither.IsError, allText(neither))
		}
	}
}

func TestPathRunsThroughTheAddonsFileMethods(t *testing.T) {
	fake := addon(t, map[string]xmlrpctest.Handler{
		"execute_file": func(p []any) (any, error) {
			if p[0] == "/missing.py" {
				return map[string]any{"success": false, "code": "not_found", "error": "'/missing.py' does not exist or is not a file.", "hint": "Write the script first."}, nil
			}
			return map[string]any{"success": true, "message": "Python code executed successfully.\nOutput: ran\n"}, nil
		},
		"execute_file_async": func([]any) (any, error) { return map[string]any{"success": true, "job_id": "job-abc"}, nil },
	})
	cs := session(t, settingsFor(fake))

	res := call(t, cs, "execute_code", map[string]any{"path": "/scripts/box.py", "timeout": 100, "include_screenshot": false})
	if res.IsError || !strings.Contains(allText(res), "Code executed successfully: ") {
		t.Fatalf("execute_code with path (error %v): %s", res.IsError, allText(res))
	}
	calls := fake.CallsTo("execute_file")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].Params, []any{"/scripts/box.py", float64(100)}) {
		t.Fatalf("execute_file calls = %+v; the timeout budget must travel with the path", calls)
	}
	if n := len(fake.CallsTo("execute_code")); n != 0 {
		t.Fatalf("%d execute_code calls for a path", n)
	}

	missing := call(t, cs, "execute_code", map[string]any{"path": "/missing.py", "include_screenshot": false})
	if !missing.IsError || !strings.Contains(allText(missing), "not_found") || !strings.Contains(allText(missing), "Write the script first.") {
		t.Fatalf("the addon's not_found was not kept (error %v): %s", missing.IsError, allText(missing))
	}

	started := call(t, cs, "execute_code_async", map[string]any{"path": "/scripts/fuse.py"})
	if started.IsError || !strings.Contains(allText(started), "job-abc") {
		t.Fatalf("execute_code_async with path (error %v): %s", started.IsError, allText(started))
	}
	if n := len(fake.CallsTo("execute_file_async")); n != 1 || len(fake.CallsTo("execute_code_async")) != 0 {
		t.Fatalf("execute_file_async called %d times, execute_code_async %d times", n, len(fake.CallsTo("execute_code_async")))
	}
}

func TestPathOnAnAddonWithoutFileMethodsSaysTheAddonIsTooOld(t *testing.T) {
	// The fake addon has no execute_file methods, as an addon of protocol 7 has not.
	cs := session(t, settingsFor(addon(t, nil)))
	for _, tool := range []string{"execute_code", "execute_code_async"} {
		res := call(t, cs, tool, map[string]any{"path": "/scripts/box.py"})
		text := allText(res)
		if !res.IsError || !strings.Contains(text, "FreeCAD's addon is too old to run a script from a file") ||
			!strings.Contains(text, "install-addon") || !strings.Contains(text, "pass the script as code") {
			t.Errorf("%s with path (error %v): %s", tool, res.IsError, text)
		}
	}
}

func TestHeadlessPathMustBeAnAbsoluteExistingFile(t *testing.T) {
	cs := session(t, domain.Settings{Host: "127.0.0.1", Port: 9})
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.py")
	for _, tc := range []struct{ path, code, want string }{
		{"script.py", "invalid_input", "not an absolute path"},
		{missing, "not_found", missing},
		{dir, "invalid_input", "is not a file"},
	} {
		res := call(t, cs, "execute_code_headless", map[string]any{"path": tc.path})
		text := allText(res)
		if !res.IsError || !strings.Contains(text, tc.code) || !strings.Contains(text, tc.want) {
			t.Errorf("path %q (error %v), want %s naming %q: %s", tc.path, res.IsError, tc.code, tc.want, text)
		}
	}
}

// pythonCommand finds an interpreter to stand in for freecadcmd, which accepts
// -c <code> the same way; FREECAD_MCP_TEST_PYTHON overrides the search.
func pythonCommand(t *testing.T) []string {
	t.Helper()
	for _, c := range []string{os.Getenv("FREECAD_MCP_TEST_PYTHON"), "python3", "python"} {
		if c == "" {
			continue
		}
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		// The Windows Store alias exists on PATH but is not an interpreter.
		if exec.Command(path, "-c", "import sys; sys.exit(0)").Run() == nil {
			return []string{path}
		}
	}
	t.Skip("no Python interpreter; set FREECAD_MCP_TEST_PYTHON")
	return nil
}

func TestHeadlessRunsAFileInPlaceInTheForegroundAndInTheBackground(t *testing.T) {
	py := pythonCommand(t)
	scriptDir := t.TempDir()
	old := headless.ScriptDir
	headless.ScriptDir = func() (string, error) { return scriptDir, nil }
	t.Cleanup(func() { headless.ScriptDir = old })

	file := filepath.Join(t.TempDir(), "run me.py")
	if err := os.WriteFile(file, []byte("print('file', __file__)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cs := session(t, domain.Settings{Host: "127.0.0.1", Port: 9, FreecadCmd: py})

	fg := call(t, cs, "execute_code_headless", map[string]any{"path": file})
	if fg.IsError || !strings.Contains(allText(fg), "file "+file) {
		t.Fatalf("foreground run (error %v): %s", fg.IsError, allText(fg))
	}

	bg := call(t, cs, "execute_code_headless", map[string]any{"path": file, "background": true})
	id := regexpHeadlessID.FindString(allText(bg))
	if bg.IsError || id == "" {
		t.Fatalf("background start (error %v): %s", bg.IsError, allText(bg))
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		st := allText(call(t, cs, "get_async_status", map[string]any{"job_id": id}))
		if strings.Contains(st, "finished") {
			if !strings.Contains(st, "file "+file) {
				t.Fatalf("background output lacks the file path: %s", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background job did not finish: %s", st)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the script file is gone: %v", err)
	}
	if entries, _ := os.ReadDir(scriptDir); len(entries) != 0 {
		t.Fatalf("the script directory holds %v: a file that was passed as path must not be copied", entries)
	}
}

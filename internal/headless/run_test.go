package headless

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// python finds an interpreter to stand in for freecadcmd: both accept
// `-c <code>`. FREECAD_MCP_TEST_PYTHON overrides the search, for example with
// the python.exe FreeCAD ships.
func python(t *testing.T) []string {
	t.Helper()
	candidates := []string{os.Getenv("FREECAD_MCP_TEST_PYTHON"), "python3", "python"}
	for _, c := range candidates {
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

// scripts points ScriptDir at a temporary directory and returns it.
func scripts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := ScriptDir
	ScriptDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { ScriptDir = old })
	return dir
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("scripts left behind: %v", entries)
	}
}

func TestSnapName(t *testing.T) {
	for command, want := range map[string]string{
		"/snap/bin/freecad.cmd":                    "freecad",
		"/snap/bin/freecad":                        "freecad",
		"/snap/bin/freecad-dev.cmd":                "freecad-dev",
		"freecad.cmd":                              "freecad",
		"/snap/freecad/current/usr/bin/freecadcmd": "",
		"/usr/bin/freecadcmd":                      "",
		"/opt/freecad/bin/freecadcmd":              "",
		`C:\FreeCAD\bin\freecadcmd.exe`:            "",
	} {
		if got := SnapName(command); got != want {
			t.Errorf("SnapName(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestSnapScriptsGoWhereTheSnapCanRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	plain := scripts(t)

	dir, err := scriptDirFor("/snap/bin/freecad.cmd")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "snap", "freecad", "common", "freecad-mcp", "headless")
	if dir != want {
		t.Fatalf("snap script dir = %s, want %s (the snap cannot read hidden directories)", dir, want)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("snap script dir not created: %v", err)
	}
	if dir, _ := scriptDirFor("/usr/bin/freecadcmd"); dir != plain {
		t.Fatalf("a non-snap command uses %s, want ScriptDir %s", dir, plain)
	}
}

func TestSuccessReturnsOutput(t *testing.T) {
	py := python(t)
	dir := scripts(t)
	res := Run(context.Background(), "print('built'); print('  (50 %)')", 30, py)
	if !res.Success || res.ReturnCode == nil || *res.ReturnCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if res.Output != "built" {
		t.Fatalf("output = %q (progress noise must be filtered)", res.Output)
	}
	text := Format(res)
	if !strings.HasPrefix(text, "Headless FreeCAD script finished") || !strings.Contains(text, "reload_document") {
		t.Fatalf("text = %q", text)
	}
	assertEmpty(t, dir)
}

func TestExceptionReportsExitCodeAndTraceback(t *testing.T) {
	py := python(t)
	scripts(t)
	res := Run(context.Background(), "print('step 1')\nraise ValueError('Null shape')", 30, py)
	if res.Success || res.ReturnCode == nil || *res.ReturnCode != 1 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Output, "step 1") || !strings.Contains(res.Output, "ValueError: Null shape") {
		t.Fatalf("output = %q", res.Output)
	}
	if !strings.HasPrefix(Format(res), "Headless FreeCAD script FAILED: script failed (exit code 1)") {
		t.Fatalf("text = %q", Format(res))
	}
}

// writeScriptFile writes a script into a folder of its own, with a space in its
// name, and returns its path.
func writeScriptFile(t *testing.T, source string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "my script.py")
	if err := os.WriteFile(file, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestScriptFileRunsInPlaceAsMain(t *testing.T) {
	py := python(t)
	dir := scripts(t)
	file := writeScriptFile(t, "\xef\xbb\xbfimport sys\nprint('file', __file__)\nprint('name', __name__)\n")
	res := RunScript(context.Background(), Script{Path: file}, 30, py)
	if !res.Success || res.Output != "file "+file+"\nname __main__" {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the script file is gone after the run: %v", err)
	}
	assertEmpty(t, dir) // nothing was copied into the script directory
}

func TestScriptFileTracebackCitesTheFileAndLine(t *testing.T) {
	py := python(t)
	scripts(t)
	file := writeScriptFile(t, "import sys\nprint('start')\n\nraise ValueError('line four')\n")
	res := RunScript(context.Background(), Script{Path: file}, 30, py)
	if res.Success || res.ReturnCode == nil || *res.ReturnCode != 1 {
		t.Fatalf("result = %+v", res)
	}
	want := `File "` + file + `", line 4`
	if !strings.Contains(res.Output, want) || !strings.Contains(res.Output, "ValueError: line four") || !strings.Contains(res.Output, "start") {
		t.Fatalf("output = %q, want it to hold %q", res.Output, want)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the script file is gone after a failed run: %v", err)
	}
}

func TestNativeCrashIsReportedNotPropagated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal exit status")
	}
	py := python(t)
	scripts(t)
	res := Run(context.Background(), "import os, resource, signal; resource.setrlimit(resource.RLIMIT_CORE, (0, 0)); print('before', flush=True); os.kill(os.getpid(), signal.SIGSEGV)", 30, py)
	if res.Success || !res.Crashed {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Error, "SIGSEGV") || !strings.Contains(res.Error, "GUI is unaffected") || res.Output != "before" {
		t.Fatalf("result = %+v", res)
	}
}

func TestTimeoutKillsTheProcess(t *testing.T) {
	py := python(t)
	scripts(t)
	res := Run(context.Background(), "import time; time.sleep(30)", 1, py)
	if res.Success || !res.TimedOut || !strings.Contains(res.Error, "did not finish within 1 s") {
		t.Fatalf("result = %+v", res)
	}
}

func TestCancellationIsNotACrashOrATimeout(t *testing.T) {
	py := python(t)
	dir := scripts(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(700 * time.Millisecond); cancel() }()
	res := Run(ctx, "import time\nprint('started', flush=True)\ntime.sleep(30)", 60, py)
	if res.Success || res.Crashed || res.TimedOut || !strings.Contains(res.Error, "cancelled") {
		t.Fatalf("result = %+v", res)
	}
	assertEmpty(t, dir)
}

func TestTimeoutAboveTheCapIsRejected(t *testing.T) {
	dir := scripts(t)
	res := Run(context.Background(), "pass", MaxTimeout+1, []string{"must-not-run"})
	if res.Success || !strings.Contains(res.Error, "at most") {
		t.Fatalf("result = %+v", res)
	}
	assertEmpty(t, dir)
}

func TestFreeCADSignalHandlerReportRequiresFailedExit(t *testing.T) {
	py := python(t)
	scripts(t)
	for _, code := range []string{"0", "1"} {
		res := Run(context.Background(),
			"import sys\nprint('Program received signal SIGSEGV, Segmentation fault.', file=sys.stderr)\nsys.exit("+code+")", 10, py)
		if res.Success != (code == "0") {
			t.Fatalf("exit %s: result = %+v", code, res)
		}
		if code == "1" && (!res.Crashed || !strings.Contains(res.Error, "reported a crash with SIGSEGV")) {
			t.Fatalf("exit 1: result = %+v", res)
		}
		if code == "0" && res.Crashed {
			t.Fatalf("exit 0 reported as a crash: %+v", res)
		}
	}
}

func TestTimeoutPreservesPartialOutputAndRemovesScript(t *testing.T) {
	py := python(t)
	cases := []struct{ code, want string }{
		{"print('started', flush=True)", "started"},
		{"print('warning', file=sys.stderr, flush=True)", "warning"},
		{"print('started', flush=True); print('warning', file=sys.stderr, flush=True)", "started\nwarning"},
		{"sys.stdout.buffer.write(b'progress \\xe3\\x81'); sys.stdout.flush()", "progress \uFFFD"},
	}
	for _, tc := range cases {
		dir := scripts(t)
		res := Run(context.Background(), "import sys, time\n"+tc.code+"\ntime.sleep(30)", 1.5, py)
		if res.Success || !res.TimedOut || res.Output != tc.want || !strings.Contains(res.Error, "within 1.5 s") {
			t.Errorf("%s: result = %+v", tc.code, res)
		}
		assertEmpty(t, dir)
	}
}

func TestInvalidTimeoutNeverLaunchesCode(t *testing.T) {
	dir := scripts(t)
	for _, timeout := range []float64{0, -1} {
		res := Run(context.Background(), "raise AssertionError('must not run')", timeout, []string{"must-not-run"})
		if res.Success || !strings.Contains(res.Error, "positive finite") {
			t.Fatalf("timeout %v: %+v", timeout, res)
		}
	}
	assertEmpty(t, dir)
}

func TestStartFailureIsReportedAndRemovesScript(t *testing.T) {
	dir := scripts(t)
	res := Run(context.Background(), "pass", 5, []string{filepath.Join(dir, "missing-freecadcmd")})
	if res.Success || !strings.Contains(res.Error, "could not start") {
		t.Fatalf("result = %+v", res)
	}
	assertEmpty(t, dir)
}

func TestPyString(t *testing.T) {
	cases := map[string]string{
		`C:\Users\me\script.py`: `'C:\\Users\\me\\script.py'`,
		`/home/o'brien/s.py`:    `'/home/o\'brien/s.py'`,
	}
	for in, want := range cases {
		if got := pyString(in); got != want {
			t.Errorf("pyString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestFormatFailure(t *testing.T) {
	code := 3
	text := Format(Result{ReturnCode: &code, Error: "script failed (exit code 3)", Output: "partial\n"})
	if text != "Headless FreeCAD script FAILED: script failed (exit code 3)\nOutput:\npartial" {
		t.Fatalf("text = %q", text)
	}
}

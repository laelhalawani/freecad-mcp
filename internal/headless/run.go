package headless

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Result is the outcome of one headless run.
type Result struct {
	Success    bool
	ReturnCode *int
	Output     string // stdout and stderr, FreeCAD's banner and progress noise removed
	Crashed    bool
	TimedOut   bool
	Error      string
}

// MaxTimeout bounds a run: a week, far past any real script and well inside
// what a time.Duration can hold.
const MaxTimeout = 7 * 24 * 3600.0

var noise = []string{"%)", "Importing project files", "Postprocessing", "FreeCAD 1.", "(C) 2001", "LGPL"}

// crashReport matches the line FreeCAD's own signal handler prints before it
// exits with status 1, which hides the signal from the OS exit status.
var crashReport = regexp.MustCompile(`(?m)^Program received signal (SIG[A-Z0-9]+),`)

// ScriptDir is where scripts are written before they run. Flatpak sandboxes
// usually see $HOME but not /tmp, so it lives under the home directory.
var ScriptDir = func() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "headless")
	return dir, os.MkdirAll(dir, 0o755)
}

// CacheDir is freecad-mcp's cache directory (not created here).
func CacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "freecad-mcp"), nil
}

// SnapName returns the name of the snap a command runs, or "" when it is
// not one: the launcher /snap/bin/<snap>.<app> or /snap/bin/<snap>, or the
// Snap FreeCAD's freecad.cmd named without a path.
func SnapName(command string) string {
	base := filepath.Base(command)
	switch {
	case strings.HasPrefix(filepath.ToSlash(command), "/snap/bin/"):
		return strings.SplitN(base, ".", 2)[0]
	case base == "freecad.cmd":
		return "freecad"
	}
	return ""
}

// SnapDir is where freecad-mcp keeps files for the named snap (not created
// here). A Snap-confined FreeCAD cannot read ~/.cache: the snap's home
// interface leaves out hidden directories. It can read its own
// ~/snap/<name>/common.
func SnapDir(snap string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "snap", snap, "common", "freecad-mcp"), nil
}

// scriptDirFor returns the directory for scripts run by command.
func scriptDirFor(command string) (string, error) {
	snap := SnapName(command)
	if snap == "" {
		return ScriptDir()
	}
	dir, err := SnapDir(snap)
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "headless")
	return dir, os.MkdirAll(dir, 0o755)
}

func clean(output []byte) string {
	text := strings.ToValidUTF8(string(output), "�")
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" || isNoise(line) {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func isNoise(line string) bool {
	for _, n := range noise {
		if strings.Contains(line, n) {
			return true
		}
	}
	return false
}

// pyString renders s as a Python single-quoted string literal.
func pyString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`).Replace(s) + "'"
}

func joinOutput(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

// bootstrapCode runs the script file as __main__ in its own namespace, with
// __file__ set to its path, as a normal script runs. freecadcmd
// drops what a script printed when the script raises, so a failure flushes the
// output first, then prints the traceback (without this wrapper's own frame)
// and exits 1.
const bootstrapCode = `import sys as _sys, traceback as _traceback
try:
    _sys.stdout.reconfigure(line_buffering=True)
except Exception:
    pass
_ns = {'__name__': '__main__', '__file__': SCRIPT, '__builtins__': __builtins__}
try:
    with open(SCRIPT, encoding='utf-8-sig') as _f:
        _source = _f.read()
    exec(compile(_source, SCRIPT, 'exec'), _ns)
except SystemExit:
    _sys.stdout.flush()
    raise
except BaseException as _e:
    _sys.stdout.flush()
    print(''.join(_traceback.format_exception(type(_e), _e, _e.__traceback__.tb_next)), file=_sys.stderr, end='')
    _sys.stderr.flush()
    _sys.exit(1)
_sys.stdout.flush()
`

// bootstrap returns the -c program that runs the script file at path.
func bootstrap(script string) string {
	return "SCRIPT = " + pyString(script) + "\n" + bootstrapCode
}

// prepare checks timeout, finds the freecadcmd command (command, or detected)
// and its script directory. msg is the error text, "" when all is well.
func prepare(ctx context.Context, timeout float64, command []string) (cmd []string, dir string, msg string) {
	if math.IsNaN(timeout) || math.IsInf(timeout, 0) || timeout <= 0 {
		return nil, "", "timeout must be a positive finite number"
	}
	if timeout > MaxTimeout {
		return nil, "", fmt.Sprintf("timeout must be a positive finite number of at most %g s", MaxTimeout)
	}
	if len(command) == 0 {
		command = Detect(ctx)
	}
	if len(command) == 0 {
		return nil, "", "freecadcmd not found: install FreeCAD, or set FREECAD_MCP_FREECADCMD to the command that starts it"
	}
	dir, err := scriptDirFor(command[0])
	if err != nil {
		return nil, "", fmt.Sprintf("could not prepare the script directory: %v", err)
	}
	return command, dir, ""
}

// Script is what a headless run executes: Code, which is written to a
// temporary file for the run, or the file at Path, which runs where it is and
// is never copied or deleted. Exactly one is set.
type Script struct {
	Code string
	Path string
}

// prepare gives the path of the file to run. temp says the file was written
// here, in dir, and is the caller's to remove.
func (s Script) prepare(dir string) (path string, temp bool, msg string) {
	if s.Path != "" {
		return s.Path, false, ""
	}
	path, msg = writeScript(dir, s.Code)
	return path, msg == "", msg
}

// writeScript writes code to a new script file in dir.
func writeScript(dir, code string) (path, msg string) {
	f, err := os.CreateTemp(dir, "script-*.py")
	if err != nil {
		return "", fmt.Sprintf("could not write the script: %v", err)
	}
	_, werr := f.WriteString(code)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(f.Name())
		return "", fmt.Sprintf("could not write the script: %v", errors.Join(werr, cerr))
	}
	return f.Name(), ""
}

// crashMessage words a native crash. name is the signal or exception name, ""
// when the exit code alone shows the crash.
func crashMessage(name string, code int) string {
	if name == "" {
		return fmt.Sprintf("FreeCAD crashed (exit code %d; the GUI is unaffected)", code)
	}
	return fmt.Sprintf("headless FreeCAD crashed with %s (OCCT native crash; the GUI is unaffected)", name)
}

// Run executes code as a script file through freecadcmd -c with bootstrap,
// waiting at most timeout seconds. A nil command is auto-detected.
func Run(ctx context.Context, code string, timeout float64, command []string) Result {
	return RunScript(ctx, Script{Code: code}, timeout, command)
}

// RunScript is Run for a Script: inline code or a file that already exists.
func RunScript(ctx context.Context, s Script, timeout float64, command []string) Result {
	command, dir, msg := prepare(ctx, timeout, command)
	if msg != "" {
		return Result{Error: msg}
	}
	script, temp, msg := s.prepare(dir)
	if msg != "" {
		return Result{Error: msg}
	}
	if temp {
		defer os.Remove(script)
	}

	limit := time.Duration(timeout * float64(time.Second))
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	args := append(append([]string{}, command[1:]...), "-c", bootstrap(script))
	cmd := exec.CommandContext(runCtx, command[0], args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A helper that inherited the pipes must not keep Wait from returning.
	cmd.WaitDelay = 2 * time.Second

	kill, runErr := startTree(cmd)
	if runErr != nil {
		return Result{Error: fmt.Sprintf("could not start headless FreeCAD: %v", runErr)}
	}
	runErr = cmd.Wait()
	kill() // whatever the script left running ends with it
	partial := func() string { return joinOutput(clean(stdout.Bytes()), clean(stderr.Bytes())) }
	if runErr != nil && ctx.Err() != nil {
		// The caller gave up (the MCP request was cancelled): a kill we
		// caused is not a crash.
		return Result{Error: "headless FreeCAD was stopped because the request was cancelled", Output: partial()}
	}
	if runErr != nil && runCtx.Err() == context.DeadlineExceeded {
		return Result{
			TimedOut: true,
			Error:    fmt.Sprintf("headless FreeCAD did not finish within %s s", strconv.FormatFloat(timeout, 'g', -1, 64)),
			Output:   partial(),
		}
	}

	output := clean(append(append(stdout.Bytes(), '\n'), stderr.Bytes()...))
	code0 := cmd.ProcessState.ExitCode()
	res := Result{Success: code0 == 0 && runErr == nil, ReturnCode: &code0, Output: output}
	if signal, number, ok := crashSignal(cmd.ProcessState); ok {
		res.Success = false
		res.Crashed = true
		if number != 0 {
			// As Python reports it: minus the signal number.
			res.ReturnCode = &number
		}
		res.Error = crashMessage(signal, code0)
		return res
	}
	if !res.Success {
		if m := crashReport.FindStringSubmatch(output); m != nil {
			res.Crashed = true
			res.Error = fmt.Sprintf("headless FreeCAD reported a crash with %s (exit code %d; the GUI is unaffected)", m[1], code0)
		} else {
			res.Error = fmt.Sprintf("script failed (exit code %d)", code0)
		}
	}
	return res
}

// Format renders a result as the tool's text reply.
func Format(r Result) string {
	var text string
	if r.Success {
		text = "Headless FreeCAD script finished (exit 0)."
	} else {
		msg := r.Error
		if msg == "" {
			msg = "unknown error"
		}
		text = "Headless FreeCAD script FAILED: " + msg
	}
	if r.Output != "" {
		text += "\nOutput:\n" + strings.TrimRight(r.Output, " \t\r\n")
	}
	if r.Success {
		text += "\nIf the script saved a document that is open in the GUI, call reload_document to see the result."
	}
	return text
}

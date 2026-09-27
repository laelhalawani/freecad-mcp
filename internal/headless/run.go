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

// Run executes code with `command -c "exec(open(script).read())"`, waiting at
// most timeout seconds. A nil command is auto-detected.
func Run(ctx context.Context, code string, timeout float64, command []string) Result {
	if math.IsNaN(timeout) || math.IsInf(timeout, 0) || timeout <= 0 {
		return Result{Error: "timeout must be a positive finite number"}
	}
	if timeout > MaxTimeout {
		return Result{Error: fmt.Sprintf("timeout must be a positive finite number of at most %g s", MaxTimeout)}
	}
	if len(command) == 0 {
		command = Detect(ctx)
	}
	if len(command) == 0 {
		return Result{Error: "freecadcmd not found: install FreeCAD, or set FREECAD_MCP_FREECADCMD to the command that starts it"}
	}
	dir, err := scriptDirFor(command[0])
	if err != nil {
		return Result{Error: fmt.Sprintf("could not prepare the script directory: %v", err)}
	}
	f, err := os.CreateTemp(dir, "script-*.py")
	if err != nil {
		return Result{Error: fmt.Sprintf("could not write the script: %v", err)}
	}
	script := f.Name()
	defer os.Remove(script)
	_, werr := f.WriteString(code)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return Result{Error: fmt.Sprintf("could not write the script: %v", errors.Join(werr, cerr))}
	}

	limit := time.Duration(timeout * float64(time.Second))
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	args := append(append([]string{}, command[1:]...), "-c", "exec(open("+pyString(script)+", encoding='utf-8').read())")
	cmd := exec.CommandContext(runCtx, command[0], args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A helper that inherited the pipes must not keep Wait from returning.
	cmd.WaitDelay = 2 * time.Second
	// A wrapper script's own children are ended too where the OS allows it.
	killTree(cmd)

	runErr := cmd.Run()
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) && cmd.ProcessState == nil {
		return Result{Error: fmt.Sprintf("could not start headless FreeCAD: %v", runErr)}
	}
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
		res.Error = fmt.Sprintf("headless FreeCAD crashed with %s (OCCT native crash; the GUI is unaffected)", signal)
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

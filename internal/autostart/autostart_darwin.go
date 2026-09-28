package autostart

// macOS: a LaunchAgent (~/Library/LaunchAgents/<MacLabel>.plist) in the
// user's gui/<uid> domain.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
)

// noAquaSessionExitCode is what launchctl exits with when the calling user
// has no graphical (Aqua) session to talk to, for example over SSH or before
// first login.
const noAquaSessionExitCode = 125

// entryArgs is the listener's argument list after the executable itself:
// "listen --user-data-dir <dir> [--rpc-port <port>] [--freecad <path>]".
// RPCPort 0 or the addon's default port omits the flag; FreeCADPath empty
// omits --freecad. Each element becomes its own ProgramArguments <string>,
// XML-escaped on its own by plistContent, so none of them need quoting here.
func entryArgs(e Entry) []string {
	args := []string{"listen", "--user-data-dir", e.UserDataDir}
	if e.RPCPort != 0 && e.RPCPort != domain.DefaultRPCPort {
		args = append(args, "--rpc-port", strconv.Itoa(e.RPCPort))
	}
	if e.FreeCADPath != "" {
		args = append(args, "--freecad", e.FreeCADPath)
	}
	return args
}

// logPath is e.LogFile, defaulting to domain.ListenerStdoutPath when the
// caller left it empty.
func logPath(e Entry) string {
	if e.LogFile != "" {
		return e.LogFile
	}
	return domain.ListenerStdoutPath()
}

// plistPath is where the LaunchAgent is written.
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", MacLabel+".plist"), nil
}

// domainTarget is this user's launchd GUI domain, gui/<uid>.
func domainTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// serviceTarget is the job inside domainTarget, as launchctl's newer
// subcommands (bootstrap, bootout, kickstart, print) address it.
func serviceTarget() string {
	return domainTarget() + "/" + MacLabel
}

func xmlText(s string) string {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		// EscapeText only fails on a broken Writer; bytes.Buffer never is.
		return s
	}
	return b.String()
}

// plistContent renders the LaunchAgent property list: RunAtLoad,
// KeepAlive.SuccessfulExit false, ThrottleInterval 10, AbandonProcessGroup
// true (belt and braces alongside the listener's own Setsid, so a FreeCAD
// it launched is never signalled along with the job), and the listener's
// own log file as both stdout and stderr.
func plistContent(e Entry) string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("\t<key>Label</key>\n\t<string>" + xmlText(MacLabel) + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	args := append([]string{e.Executable}, entryArgs(e)...)
	for _, a := range args {
		b.WriteString("\t\t<string>" + xmlText(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	b.WriteString("\t<key>ThrottleInterval</key>\n\t<integer>10</integer>\n")
	b.WriteString("\t<key>AbandonProcessGroup</key>\n\t<true/>\n")
	log := xmlText(logPath(e))
	b.WriteString("\t<key>StandardOutPath</key>\n\t<string>" + log + "</string>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + log + "</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// runLaunchctl runs launchctl with args, returning its combined output
// (trimmed) for the caller's error, and a clear error when the user has no
// Aqua session to run it in.
func runLaunchctl(args ...string) (string, error) {
	cmd := exec.Command("launchctl", args...)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err == nil {
		return output, nil
	}
	if exitCode(err) == noAquaSessionExitCode {
		return output, fmt.Errorf("no graphical (Aqua) login session is available for this user, so launchctl cannot manage the listener; log in to the desktop and try again (launchctl %s: %s)",
			strings.Join(args, " "), output)
	}
	return output, fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, output)
}

// register writes the LaunchAgent and loads it. RunAtLoad starts the
// listener immediately, so a separate start() is not needed after this.
// It is idempotent: a copy already loaded is unloaded first so bootstrap
// replaces it, matching a plist that may have changed (a new executable
// path, a different user data directory).
func register(e Entry) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if dir := filepath.Dir(logPath(e)); dir != "." {
		// 0700: this is ordinarily domain.DataDir() (~/.freecad-mcp) itself,
		// which holds the credential store elsewhere and is kept private
		// everywhere else it is created.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(plistContent(e)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// Best effort: a job not currently loaded fails here, which is expected
	// and not reported.
	runLaunchctl("bootout", serviceTarget())
	if _, err := runLaunchctl("bootstrap", domainTarget(), path); err != nil {
		return err
	}
	return nil
}

// unregister unloads the job (when loaded) and removes the plist. Nothing
// registered is not an error, only a real launchctl failure (no Aqua
// session) is.
func unregister() error {
	if _, err := runLaunchctl("bootout", serviceTarget()); err != nil && exitCode(err) == noAquaSessionExitCode {
		return err
	}
	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// start (also used for restart) kills any running instance of the job and
// starts a fresh one from the registered plist; AbandonProcessGroup keeps a
// FreeCAD it launched from being signalled along with it.
func start() error {
	_, err := runLaunchctl("kickstart", "-k", serviceTarget())
	return err
}

// stop unloads the job. Not loaded is treated as already stopped, not as a
// failure.
func stop() error {
	_, err := runLaunchctl("bootout", serviceTarget())
	if err == nil || exitCode(err) != noAquaSessionExitCode {
		return nil
	}
	return err
}

// restart uses the same kickstart -k as start: launchd has no separate
// "restart a stopped job" primitive, and kickstart -k both starts it when
// stopped and replaces the running instance when not.
func restart() error {
	return start()
}

// status reports whether the job is loaded (launchctl print exits 0) and
// whether a listener actually holds the lock file, independently: the
// second can be true even when this OS mechanism is not what started it.
func status() (State, error) {
	_, err := runLaunchctl("print", serviceTarget())
	if err != nil && exitCode(err) == noAquaSessionExitCode {
		return State{}, err
	}
	registered := err == nil
	running := listenerapi.Running()
	detail := "not registered"
	switch {
	case registered && running:
		detail = "registered, running"
	case registered:
		detail = "registered, not running"
	}
	return State{Registered: registered, Running: running, Detail: detail}, nil
}

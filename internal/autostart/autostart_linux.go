package autostart

// Linux: a systemd user unit (~/.config/systemd/user/<LinuxUnit>) plus an
// XDG autostart entry (~/.config/autostart/<LinuxDesktop>) that imports the
// graphical session's environment and restarts the unit; without systemd the
// XDG entry runs the listener directly.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
)

// systemdMarker is checked to tell whether this session has a systemd user
// manager available at all.
const systemdMarker = "/run/systemd/system"

// desktopContentSystemd is the fixed XDG autostart entry used when systemd
// is available: it imports the graphical session's environment (including
// XAUTHORITY, so an X11 or Xwayland cookie outside ~/.Xauthority still
// reaches FreeCAD) into the user's systemd instance, then restarts the unit.
const desktopContentSystemd = `[Desktop Entry]
Type=Application
Name=freecad-mcp listener
NoDisplay=true
Exec=sh -c "systemctl --user import-environment DISPLAY WAYLAND_DISPLAY XAUTHORITY XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS; systemctl --user restart freecad-mcp-listener"
`

// graphicalEnvNames are the variables imported into the user's systemd
// instance before enabling or restarting the unit (also done by the desktop
// entry above, which only runs at a graphical login): only names actually
// set in the registering process are passed, since systemctl errors on one
// that is not.
var graphicalEnvNames = []string{"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY"}

// importGraphicalEnvironment runs "systemctl --user import-environment" for
// whichever of graphicalEnvNames are set here, so a FreeCAD the unit starts
// can open a display without waiting for the next graphical login. Best
// effort: nothing being set is not an error, and neither is the systemd
// user manager not having anywhere to put it yet.
//
// Skipped entirely in an SSH session (SSH_CONNECTION or SSH_TTY set):
// import-environment writes into the user manager's own environment block,
// which every later user unit gets, not only this one, so running
// "freecad-mcp share --on" over "ssh -X" would otherwise copy the SSH
// client's forwarded DISPLAY (and its XAUTHORITY cookie) into every user
// service until the next graphical login overwrites it. The desktop entry
// still provides the real display at that next login.
func importGraphicalEnvironment() {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return
	}
	var names []string
	for _, n := range graphicalEnvNames {
		if os.Getenv(n) != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return
	}
	runSystemctl(append([]string{"import-environment"}, names...)...)
}

// resetFailedUnit clears the unit's start rate counter before enabling or
// restarting it, so a start limit an earlier crash loop hit (or that a run
// of quick Share saves would otherwise approach, systemd.unit(5)
// StartLimitIntervalSec/StartLimitBurst) does not carry over into this
// attempt. Best effort: nothing failed is not an error.
func resetFailedUnit() {
	runSystemctl("reset-failed", LinuxUnit)
}

func hasSystemd() bool {
	_, err := os.Stat(systemdMarker)
	return err == nil
}

func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	return home, nil
}

func unitPath(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", LinuxUnit)
}

func desktopPath(home string) string {
	return filepath.Join(home, ".config", "autostart", LinuxDesktop)
}

// entryArgs is the listener's argument list after the executable itself:
// "listen --user-data-dir <dir> [--rpc-port <port>] [--freecad <path>]".
// RPCPort 0 or the addon's default port omits the flag; FreeCADPath empty
// omits --freecad.
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

// systemdReplacer prepares an argument for a systemd unit's ExecStart= line
// (systemd.service(5) / systemd.syntax(7)): '\' and '"' are backslash
// escaped, and '$' and '%' are doubled so they pass through literally
// instead of starting a variable or specifier expansion. This applies
// whether or not the argument ends up quoted.
var systemdReplacer = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	`$`, `$$`,
	`%`, `%%`,
)

// systemdQuote escapes an argument with systemdReplacer, then wraps it in
// double quotes whenever it is empty or held whitespace or any of the
// escaped characters (an unquoted token is split on whitespace, so a
// literal space needs quoting, and an unquoted lone escape sequence such as
// $$ would otherwise sit next to unrelated characters).
func systemdQuote(s string) string {
	escaped := systemdReplacer.Replace(s)
	if s == "" || strings.ContainsAny(s, " \t\"\\$%") {
		return `"` + escaped + `"`
	}
	return escaped
}

// desktopReserved is the Desktop Entry Specification's reserved-character
// list for an Exec= argument, plus whitespace: any argument holding one of
// these must be quoted (the four characters desktopQuote backslash-escapes,
// '"', '`', '$' and '\', are also reserved, so they are covered here too).
const desktopReserved = " \t\n\"'\\><~|&;$*?#()`"

// desktopQuote quotes an argument for a Desktop Entry Exec= line (the
// freedesktop.org Desktop Entry Specification): every '%' is doubled to
// "%%" first, independent of quoting, since a lone '%' starts a field code
// there; the result is then double-quoted when the original argument held
// whitespace or a reserved character, escaping '"', '`', '$' and '\' inside
// the quotes (the only four characters the spec requires escaping there).
// execArgvFromDesktop reverses both steps when reading the entry back.
func desktopQuote(s string) string {
	if s == "" {
		return `""`
	}
	doubled := strings.ReplaceAll(s, "%", "%%")
	if !strings.ContainsAny(s, desktopReserved) {
		return doubled
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range doubled {
		if r == '"' || r == '`' || r == '$' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func execArgv(e Entry) []string {
	return append([]string{e.Executable}, entryArgs(e)...)
}

// unitContent renders the systemd user unit: StartLimit* in [Unit] (so a
// crash loop is capped rather than restarted forever), Restart=on-failure,
// and KillMode=process, which leaves a FreeCAD the listener launched
// running through a stop, crash restart or session restart, since only the
// listener's own main process is signalled.
func unitContent(e Entry) string {
	args := execArgv(e)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = systemdQuote(a)
	}
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=freecad-mcp listener\n")
	b.WriteString("StartLimitIntervalSec=60\n")
	b.WriteString("StartLimitBurst=5\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("ExecStart=" + strings.Join(quoted, " ") + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=3\n")
	b.WriteString("KillMode=process\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// desktopContentDirect is the XDG autostart entry used without systemd: it
// runs the listener itself, since there is no user service manager to hand
// it to.
func desktopContentDirect(e Entry) string {
	args := execArgv(e)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = desktopQuote(a)
	}
	return "[Desktop Entry]\nType=Application\nName=freecad-mcp listener\nNoDisplay=true\nExec=" +
		strings.Join(quoted, " ") + "\n"
}

// splitExecLine tokenises a Desktop Entry Exec= value written by
// desktopQuote: double-quoted segments may contain backslash-escaped
// characters, unquoted segments split on plain spaces.
func splitExecLine(s string) []string {
	var args []string
	var cur strings.Builder
	inQuotes, has := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuotes = !inQuotes
			has = true
		case c == '\\' && inQuotes && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			has = true
		case c == ' ' && !inQuotes:
			if has {
				args = append(args, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	if has {
		args = append(args, cur.String())
	}
	return args
}

// execArgvFromDesktop reads back the argv that register() wrote into the
// XDG autostart entry, for start() to run directly when there is no
// systemd user manager to ask instead. splitExecLine undoes desktopQuote's
// quoting and backslash escaping; the "%%" -> "%" pass afterwards undoes
// its unconditional field-code doubling, so a path holding a literal '%'
// comes back unchanged.
func execArgvFromDesktop(content string) ([]string, error) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		rest, ok := strings.CutPrefix(line, "Exec=")
		if !ok {
			continue
		}
		argv := splitExecLine(rest)
		if len(argv) == 0 {
			return nil, errors.New("the freecad-mcp autostart entry's Exec line is empty")
		}
		for i, a := range argv {
			argv[i] = strings.ReplaceAll(a, "%%", "%")
		}
		return argv, nil
	}
	return nil, errors.New("the freecad-mcp autostart entry has no Exec line; register it again")
}

// runSystemctl runs "systemctl --user" with args, returning its combined
// output (trimmed) for the caller's error.
func runSystemctl(args ...string) (string, error) {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		return output, fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, output)
	}
	return output, nil
}

// register writes the unit (when systemd is available) or the direct XDG
// entry, and always writes the XDG entry (systemd needs it to restart the
// unit at graphical login, since a systemd user manager on its own starts
// before there is a DISPLAY to import). It is idempotent: files are
// overwritten and enable --now (re)applies to whatever they now say.
func register(e Entry) error {
	home, err := homeDir()
	if err != nil {
		return err
	}
	autostartDir := filepath.Dir(desktopPath(home))
	if err := os.MkdirAll(autostartDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", autostartDir, err)
	}
	if !hasSystemd() {
		if err := os.WriteFile(desktopPath(home), []byte(desktopContentDirect(e)), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", desktopPath(home), err)
		}
		return nil
	}
	unitDir := filepath.Dir(unitPath(home))
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", unitDir, err)
	}
	if err := os.WriteFile(unitPath(home), []byte(unitContent(e)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath(home), err)
	}
	if err := os.WriteFile(desktopPath(home), []byte(desktopContentSystemd), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", desktopPath(home), err)
	}
	if _, err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	resetFailedUnit()
	importGraphicalEnvironment()
	if _, err := runSystemctl("enable", "--now", LinuxUnit); err != nil {
		return err
	}
	return nil
}

// unregister stops and disables the unit (when systemd is available) and
// removes every file register wrote. Nothing registered is not an error.
func unregister() error {
	home, err := homeDir()
	if err != nil {
		return err
	}
	if hasSystemd() {
		// Best effort: nothing enabled fails here, which is expected.
		runSystemctl("disable", "--now", LinuxUnit)
		if err := os.Remove(unitPath(home)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", unitPath(home), err)
		}
		runSystemctl("daemon-reload")
	}
	if err := os.Remove(desktopPath(home)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", desktopPath(home), err)
	}
	return nil
}

// restartUnit resets the unit's start limit counter, imports the graphical
// session's environment, and restarts it: this OS's single primitive for
// both start and restart (systemctl restart starts a stopped unit just as
// well as it restarts a running one).
func restartUnit() error {
	resetFailedUnit()
	importGraphicalEnvironment()
	_, err := runSystemctl("restart", LinuxUnit)
	return err
}

// start, with systemd, (re)starts the unit through restartUnit. Without
// systemd it runs the listener directly, detached from this process with no
// inherited stdio, since there is no user service manager to hand it to.
func start() error {
	if hasSystemd() {
		return restartUnit()
	}
	home, err := homeDir()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(desktopPath(home))
	if err != nil {
		return fmt.Errorf("reading %s: %w (register the listener first)", desktopPath(home), err)
	}
	argv, err := execArgvFromDesktop(string(data))
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", argv[0], err)
	}
	return nil
}

// errNoUserSession is returned by stop and restart without systemd: there is
// no user service manager to ask, and killing the listener directly here
// would defeat the point of KillMode=process elsewhere, so the user is
// asked to log out and back in (which reruns the XDG autostart entry) or to
// stop it manually.
var errNoUserSession = errors.New("no systemd user session is available on this system; log out and back in to restart the listener, or stop it yourself")

func stop() error {
	if hasSystemd() {
		_, err := runSystemctl("stop", LinuxUnit)
		return err
	}
	return errNoUserSession
}

func restart() error {
	if hasSystemd() {
		return restartUnit()
	}
	return errNoUserSession
}

// status reports whether the unit is enabled (with systemd) or the XDG
// entry exists (without), and whether a listener actually holds the lock
// file, independently of what registered it.
func status() (State, error) {
	running := listenerapi.Running()
	if hasSystemd() {
		out, err := runSystemctl("is-enabled", LinuxUnit)
		registered := err == nil
		detail := "not registered"
		if registered {
			detail = fmt.Sprintf("registered (%s), not running", out)
			if running {
				detail = fmt.Sprintf("registered (%s), running", out)
			}
		}
		return State{Registered: registered, Running: running, Detail: detail}, nil
	}
	home, err := homeDir()
	if err != nil {
		return State{}, err
	}
	_, statErr := os.Stat(desktopPath(home))
	registered := statErr == nil
	detail := "not registered"
	if registered {
		detail = "registered without systemd (starts at graphical login)"
	}
	return State{Registered: registered, Running: running, Detail: detail}, nil
}

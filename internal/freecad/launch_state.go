package freecad

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/headless"
)

// Launch states. An empty State means this server has not launched FreeCAD.
const (
	LaunchStarted   = "started"   // the launched process is running
	LaunchForwarded = "forwarded" // a running FreeCAD took over the request and the launched process exited
	LaunchExited    = "exited"    // the launched process exited on its own
)

// forwardWindow is how long Launch waits for the launched process to exit on
// its own before treating it as a genuine start. FreeCAD's single-instance
// check (Gui/Application.cpp onlySingleInstance) forwards the request to an
// already running window and returns at once, well under this.
const forwardWindow = 5 * time.Second

// StartingWindow is how long a started launch is given to reach the point
// where a repeat start_freecad would either see it answer (already_running)
// or forward to it, matching the "starting" state get_rpc_status reports for
// a launch this recent. A second Launch call inside this window returns the
// existing launch instead of starting another FreeCAD process next to one
// still initialising. Exported so internal/mcpserver classifies the same
// launch the same way, without a second definition that could drift from
// this one.
const StartingWindow = 120 * time.Second

// macroPattern matches the startup macros this launcher writes, one per
// launch (see Launch): a fixed name would race between two freecad-mcp
// processes on the same machine configured for different ports, each
// rewriting the file the other is about to have FreeCAD run.
const macroPattern = "freecad-mcp-start-*.FCMacro"

// keepLogs is how many of this launcher's own FreeCAD logs (and, separately,
// startup macros) are kept.
const keepLogs = 5

// LaunchState is what this server knows about the FreeCAD it started with
// start_freecad.
type LaunchState struct {
	State      string // "", LaunchStarted, LaunchForwarded or LaunchExited
	PID        int
	Executable string
	StartedAt  time.Time
	ExitedAt   time.Time // zero while the process runs
	ExitCode   *int      // nil while the process runs
	LogPath    string    // FreeCAD's --log-file for this launch
	Port       int       // RPC port the startup macro starts the server on
	// Reused is true when this Launch call did not start anything: an
	// existing launch was still inside StartingWindow, so it was returned as
	// is. A file this call was asked to open was not passed to FreeCAD, since
	// the earlier launch already ran without it.
	Reused bool
}

// Launcher starts FreeCAD's GUI and tracks the process it started. It is
// safe for concurrent use.
type Launcher struct {
	settings domain.Settings

	// launchMu serialises Launch calls within this process: a second
	// start_freecad while one is still in its forwardWindow wait must not
	// race the first over log pruning and this launcher's state. It does not
	// protect against another freecad-mcp process launching at the same
	// time; each launch's own macro and log file names are unique to it.
	launchMu sync.Mutex

	mu    sync.Mutex
	state LaunchState
	// generation counts Launch calls that started a process (not ones that
	// returned an existing, still-starting launch). Each such call's exit
	// watcher goroutine only writes state while generation still matches the
	// value it captured, so an older launch's watcher cannot overwrite the
	// state of a newer one it does not describe.
	generation int
}

// NewLauncher returns a launcher for the FreeCAD the settings describe.
func NewLauncher(settings domain.Settings) *Launcher {
	return &Launcher{settings: settings}
}

// ErrLaunchUnavailable is returned by Launch when no FreeCAD GUI executable
// could be found to start.
var ErrLaunchUnavailable = errors.New("no FreeCAD GUI executable was found on PATH or in the standard install locations")

// signedExitCode reinterprets an exit code as signed 32 bits. A process
// Windows terminated abruptly (TerminateProcess, which is how a killed
// process ends) exits with a DWORD such as 0xFFFFFFFF; Go's ExitCode widens
// that to an int without sign-extending it first, so on windows/amd64 (a
// 64-bit int) it comes back as 4294967295 rather than the -1 every other
// platform reports for the same kind of termination.
func signedExitCode(code int) int {
	return int(int32(code))
}

// Launch starts FreeCAD's GUI with a startup macro that starts the RPC server,
// opening file (an .FCStd path) when it is not "".
func (l *Launcher) Launch(ctx context.Context, file string) (LaunchState, error) {
	l.launchMu.Lock()
	defer l.launchMu.Unlock()

	l.mu.Lock()
	cur := l.state
	l.mu.Unlock()
	if cur.State == LaunchStarted && cur.ExitCode == nil && time.Since(cur.StartedAt) < StartingWindow {
		// A launch from a moment ago may not have reached FreeCAD's
		// single-instance check yet (that comes after loading every module's
		// Init.py, which the tool itself says can take 15 s or more on a
		// first start): a second Launch here could start an independent
		// second FreeCAD window instead of forwarding to the first. Report
		// the existing launch instead of starting another; file (if any) was
		// not passed to it, since it already started without it.
		cur.Reused = true
		return cur, nil
	}

	guiCmd, err := l.guiCommand(ctx)
	if err != nil {
		return LaunchState{}, err
	}

	cacheDir, err := macroDirFor(guiCmd[0])
	if err != nil {
		return LaunchState{}, fmt.Errorf("prepare the freecad-mcp cache directory: %w", err)
	}

	port := l.settings.Port
	if port <= 0 {
		port = domain.DefaultRPCPort
	}

	// Make room for this launch's own macro and log among the ones already on
	// disk before creating them, so at most keepLogs of each exist afterwards.
	pruneGlob(cacheDir, macroPattern, keepLogs-1)
	pruneGlob(cacheDir, "freecad-gui-*.log", keepLogs-1)

	// A unique name per launch: a fixed one would race between two
	// freecad-mcp processes on this machine (see launchMu's doc comment).
	stamp := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	macroPath := filepath.Join(cacheDir, fmt.Sprintf("freecad-mcp-start-%s.FCMacro", stamp))
	if err := os.WriteFile(macroPath, []byte(startupMacro(port)), 0o644); err != nil {
		return LaunchState{}, fmt.Errorf("write the startup macro: %w", err)
	}
	logPath := filepath.Join(cacheDir, fmt.Sprintf("freecad-gui-%s.log", stamp))

	args := append(append([]string{}, guiCmd[1:]...), "--single-instance", "--log-file", logPath, macroPath)
	if file != "" {
		args = append(args, file)
	}
	env := overrideEnv(os.Environ(), domain.EnvPort, fmt.Sprintf("%d", port))

	cmd, err := startDetached(guiCmd[0], args, env)
	if err != nil {
		return LaunchState{}, fmt.Errorf("start FreeCAD: %w", err)
	}

	state := LaunchState{
		State:      LaunchStarted,
		PID:        cmd.Process.Pid,
		Executable: strings.Join(guiCmd, " "),
		StartedAt:  time.Now(),
		LogPath:    logPath,
		Port:       port,
	}
	l.mu.Lock()
	l.generation++
	gen := l.generation
	l.state = state
	l.mu.Unlock()

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	// watch records the process's eventual exit in the background, once the
	// synchronous wait below has already returned started (still running past
	// forwardWindow, or the caller's context was cancelled first). It writes
	// only while gen is still the current generation, so a later Launch call
	// that has since started a new process is never overwritten with this
	// one's exit.
	watch := func() {
		go func() {
			<-exited
			code := signedExitCode(cmd.ProcessState.ExitCode())
			l.mu.Lock()
			if l.generation == gen {
				l.state.State = LaunchExited
				l.state.ExitCode = &code
				l.state.ExitedAt = time.Now()
			}
			l.mu.Unlock()
		}()
	}

	select {
	case <-exited:
		// FreeCAD's single-instance check forwards to an already running
		// window and exits 0 almost at once; anything else exiting this
		// quickly is a launch failure. launchMu has been held for this whole
		// call, so gen is still current: no watch/generation guard is needed
		// for this synchronous branch.
		code := signedExitCode(cmd.ProcessState.ExitCode())
		l.mu.Lock()
		if code == 0 {
			l.state.State = LaunchForwarded
		} else {
			l.state.State = LaunchExited
			l.state.ExitCode = &code
		}
		l.state.ExitedAt = time.Now()
		result := l.state
		l.mu.Unlock()
		return result, nil
	case <-time.After(forwardWindow):
		// Still running past the forwarding window: a genuine start. Keep
		// watching in the background so State() and get_rpc_status learn
		// about a later crash.
		watch()
		return state, nil
	case <-ctx.Done():
		// The caller gave up waiting; the process itself is unaffected and
		// already started, so report it as started and keep watching for its
		// eventual exit in the background, same as the timeout case.
		watch()
		return state, nil
	}
}

// State returns a snapshot of the launch state.
func (l *Launcher) State() LaunchState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// LogTail returns up to the last maxBytes of the launch's FreeCAD log, or ""
// when there is none.
func (l *Launcher) LogTail(maxBytes int) string {
	l.mu.Lock()
	path := l.state.LogPath
	l.mu.Unlock()
	if path == "" || maxBytes <= 0 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	var offset int64
	if info.Size() > int64(maxBytes) {
		offset = info.Size() - int64(maxBytes)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(data)
}

// guiCommand resolves the FreeCAD GUI command: the configured override
// (FREECAD_MCP_FREECAD), or auto-detection on PATH or in the standard install
// locations.
func (l *Launcher) guiCommand(ctx context.Context) ([]string, error) {
	if len(l.settings.FreecadGUI) > 0 {
		return l.settings.FreecadGUI, nil
	}
	if cmd := headless.DetectGUI(ctx); cmd != nil {
		return cmd, nil
	}
	return nil, ErrLaunchUnavailable
}

// macroDirFor returns the directory the startup macro and this launch's log
// are written to: the snap-confined FreeCAD's own common directory when
// command is a snap (its home interface hides ~/.cache from the confined
// process), else freecad-mcp's cache directory (also reachable from a
// Flatpak sandbox, which usually sees $HOME).
func macroDirFor(command string) (string, error) {
	if snap := headless.SnapName(command); snap != "" {
		dir, err := headless.SnapDir(snap)
		if err != nil {
			return "", err
		}
		return dir, os.MkdirAll(dir, 0o755)
	}
	dir, err := headless.CacheDir()
	if err != nil {
		return "", err
	}
	return dir, os.MkdirAll(dir, 0o755)
}

// pruneGlob removes all but the newest keep files matching pattern in dir.
// Their names sort chronologically (a fixed-width UnixNano prefix or infix
// for a very long time to come), so a lexicographic sort orders them too. A
// failure to clean up is not fatal to the launch that called it.
func pruneGlob(dir, pattern string, keep int) {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil || len(matches) <= keep {
		return
	}
	sort.Strings(matches)
	for _, m := range matches[:len(matches)-keep] {
		os.Remove(m)
	}
}

// overrideEnv returns env with every existing entry for key removed and
// key=value appended, so the new value is the one the child process and
// Python's os.environ see.
func overrideEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+value)
}

// startupMacro is the .FCMacro written to the cache directory. It starts the
// MCP addon's RPC server on port, independent of the addon's own auto-start
// setting and its default port, and reports the result (or why it failed) to
// the Console, which the launch's --log-file captures. FreeCAD runs it as one
// of its command line files (App/Application.cpp processFiles) whether this
// launch started FreeCAD fresh or an already running FreeCAD received it by
// single-instance forwarding (Gui/Application.cpp onlySingleInstance,
// Gui/MainWindow.cpp processMessages).
func startupMacro(port int) string {
	return fmt.Sprintf(`# Written by freecad-mcp's start_freecad tool. Do not edit; it is
# regenerated on every launch.
import FreeCAD

try:
    from rpc_server import rpc_server
    _mcp_msg = rpc_server.start_rpc_server(%d)
except Exception as _mcp_exc:
    FreeCAD.Console.PrintError(f"[MCP] start_freecad macro failed: {type(_mcp_exc).__name__}: {_mcp_exc}\n")
else:
    FreeCAD.Console.PrintMessage(f"[MCP] start_freecad: {_mcp_msg}\n")
`, port)
}

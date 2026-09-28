package main

// `freecad-mcp listen`: the listener process the OS starts with the user's
// session when "Share this PC" is on. It holds a file lock so only one
// instance serves at a time, logs to its own rotated log file, detaches from
// a console it owns alone on Windows, and runs the listener HTTP server
// until it is asked to stop.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sairaph/mcp-wizard/daemon/lock"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/listener"
	"github.com/sairaph/freecad-mcp/internal/remote"
)

// Timing for the single-instance check: on a lock collision, retry
// acquiring the lock for a while before asking whether the process that
// holds it is actually serving. A collision can be a passing race (something
// briefly held the lock to check whether a listener is running), not
// necessarily a second listener, so a short retry avoids treating that race
// as "already running" and exiting when nothing is actually up.
const (
	lockRetryInterval = 250 * time.Millisecond
	lockRetryTimeout  = 3 * time.Second
	tieBreakProbeWait = 2 * time.Second
)

// startupRetryInterval is how long a failed attempt to start serving waits
// before trying again (live check L3): Task Scheduler's RestartOnFailure
// only restarts a task that fails to launch, not one whose program exits
// non-zero, so a listener that exited on a start error that could clear by
// itself (the port briefly in use, the settings file unusable or missing, a
// bind failure, or its lock held by something that turns out not to be a
// live listener) would otherwise stay down until the next logon or the next
// Share save. Retrying here instead means it recovers on its own once the
// port frees up, the settings file is fixed, or whatever held the lock is
// gone.
const startupRetryInterval = 30 * time.Second

// listenerLogMaxBytes is where the log file is rotated to one old copy.
const listenerLogMaxBytes = 1 << 20 // 1 MiB

// runListen is the `listen` command: flags --user-data-dir, --rpc-port and
// --freecad. It exits only for invalid flags (2) or once a genuine second
// instance is confirmed already serving (0); every other start error retries
// every startupRetryInterval instead of exiting (live check L3), since
// nothing outside this process is guaranteed to restart it on a non-zero
// exit (Windows Task Scheduler's RestartOnFailure only covers a task that
// fails to launch, not one whose program runs and then exits 1).
func runListen(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	dir := fs.String("user-data-dir", "", "FreeCAD user data directory holding the addon settings (default: the first FreeCAD installation found)")
	rpcPort := fs.Int("rpc-port", domain.DefaultRPCPort, "the addon's XML-RPC port on 127.0.0.1")
	freecadCmd := fs.String("freecad", "", "the command that starts FreeCAD's GUI, recorded by Share this PC; wins over discovery and FREECAD_MCP_FREECAD")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Detach from a console this process owns alone (a logon start) before
	// doing anything else, so at most a brief flash is visible; a no-op
	// outside Windows.
	hideConsole()

	rl, err := openRotatingLog(domain.ListenerLogPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "  listen: could not open the log file: %v\n", err)
		return 1
	}
	defer rl.Close()
	logger := log.New(rl, "", log.LstdFlags)

	launcher, err := freecadLauncher(*freecadCmd, *rpcPort)
	if err != nil {
		logger.Printf("--freecad %q: %v; falling back to discovery and %s", *freecadCmd, err, domain.EnvFreecadGUI)
	} else if *freecadCmd != "" {
		logger.Printf("starting FreeCAD with --freecad %s", *freecadCmd)
	}
	logger.Printf("starting: addon RPC port %d", *rpcPort)

	passwordLogged := false
	var lastRetryMsg string
	for {
		// Located fresh every attempt (rather than once, before the loop),
		// so a FreeCAD installed, or an addon target that appears, after
		// this process started is picked up without a restart.
		userDataDir := *dir
		if userDataDir == "" {
			if targets := addoninstall.Locate(ctx, freecadCommand()); len(targets) > 0 {
				userDataDir = targets[0].UserDataDir
			}
		}
		listenerPort := addoninstall.DefaultListenerPort
		if settings, err := addoninstall.ReadRemoteSettings(addoninstall.Target{UserDataDir: userDataDir}); err == nil {
			listenerPort = settings.ListenerPort
			if !passwordLogged && settings.AuthToken == "" {
				logger.Printf("No password is set: anyone on an allowed IP address (or on this computer) can run code in FreeCAD.")
				passwordLogged = true
			}
		}

		code, done, retryMsg := attemptServe(ctx, logger, userDataDir, listenerPort, *rpcPort, launcher, rl)
		if done {
			return code
		}
		if retryMsg != lastRetryMsg {
			logger.Printf("%s; retrying every %s until it serves", retryMsg, startupRetryInterval)
			lastRetryMsg = retryMsg
		}
		select {
		case <-ctx.Done():
			logger.Printf("stopped while waiting to retry")
			return 0
		case <-time.After(startupRetryInterval):
		}
	}
}

// attemptServe tries once to become the listener: acquire the single
// instance lock, then run the server until it stops or fails to start.
// done is true when the caller should return code now: ctx ended (a stop
// signal, code 0), or a genuine second instance already answers (code 0).
// done is false for a start error that can clear by itself; retryMsg then
// names it, for the caller to log (once per distinct message) and retry.
func attemptServe(ctx context.Context, logger *log.Logger, userDataDir string, listenerPort, rpcPort int, launcher *freecad.Launcher, rl *rotatingLog) (code int, done bool, retryMsg string) {
	inst, err := acquireLockWithRetry(ctx)
	switch {
	case err == nil:
		defer inst.Close()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		logger.Printf("stopped while waiting for the listener lock")
		return 0, true, ""
	case errors.Is(err, lock.ErrAlreadyRunning):
		// Settles a lock collision the retries inside acquireLockWithRetry
		// could not: the lock file can be held by something other than a
		// running listener only for a passing moment (listenerapi.Running
		// takes it briefly to check), so a marker that still does not answer
		// after those retries means the lock is stale, not a live second
		// instance, and so is itself just another start error to retry.
		probeCtx, cancel := context.WithTimeout(ctx, tieBreakProbeWait)
		marker, _ := remote.Probe(probeCtx, remote.Endpoint{Host: "127.0.0.1", Port: listenerPort})
		cancel()
		if marker {
			logger.Printf("another listener already answers on port %d; exiting", listenerPort)
			return 0, true, ""
		}
		return 0, false, fmt.Sprintf("the listener lock is held but nothing answers on port %d yet", listenerPort)
	default:
		return 0, false, fmt.Sprintf("could not open the listener lock: %v", err)
	}

	runErr := listener.Run(ctx, listener.Options{
		UserDataDir: userDataDir,
		RPCPort:     rpcPort,
		Version:     version,
		Launcher:    launcher,
		Log:         rl,
	})
	if runErr == nil || errors.Is(runErr, context.Canceled) {
		logger.Printf("stopped")
		return 0, true, ""
	}
	return 0, false, runErr.Error()
}

// freecadLauncher builds the launcher /listener/start uses from freecadCmd
// (the --freecad flag, as domain.JoinCommand wrote it into
// autostart.Entry.FreeCADPath when Share this PC registered this listener):
// it wins over discovery and FREECAD_MCP_FREECAD, since an AI client's
// FREECAD_MCP_FREECAD reaches the MCP server, not this process. freecadCmd
// "" (not registered through Share this PC, or run by hand) leaves the
// choice to listener.Run's own default instead; so does a recorded command
// whose executable no longer exists (the FreeCAD it named was upgraded or
// removed since this entry was registered: reregisterListener records a
// fresh one on the next update, but until then this falls back rather than
// failing every remote start_freecad).
func freecadLauncher(freecadCmd string, rpcPort int) (*freecad.Launcher, error) {
	if freecadCmd == "" {
		return nil, nil
	}
	cmd, err := domain.SplitCommand(freecadCmd)
	if err != nil {
		return nil, err
	}
	if !executableExists(cmd[0]) {
		return nil, fmt.Errorf("%s: no such file or not on PATH", cmd[0])
	}
	return freecad.NewLauncher(domain.Settings{Port: rpcPort, FreecadGUI: cmd}), nil
}

// executableExists reports whether path is runnable the way exec.Command
// would run it: a file that exists, for a path with a directory component
// (absolute, or relative to this process, which for a listener started by
// the OS is not meaningful, so a recorded path is always absolute), or a
// bare name findable on PATH.
func executableExists(path string) bool {
	if strings.ContainsAny(path, `/\`) {
		_, err := os.Stat(path)
		return err == nil
	}
	_, err := exec.LookPath(path)
	return err == nil
}

// acquireLockWithRetry opens the listener's single-instance lock, retrying
// on a collision for lockRetryTimeout before giving up. It returns
// lock.ErrAlreadyRunning when the lock is still held after the retry
// window, and ctx.Err() when ctx ends first.
func acquireLockWithRetry(ctx context.Context) (*lock.Instance, error) {
	opts := lock.Options{LockFile: domain.ListenerLockPath()}
	inst, err := lock.Open(opts)
	if !errors.Is(err, lock.ErrAlreadyRunning) {
		return inst, err
	}
	deadline := time.Now().Add(lockRetryTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockRetryInterval):
		}
		inst, err = lock.Open(opts)
		if !errors.Is(err, lock.ErrAlreadyRunning) {
			return inst, err
		}
	}
	return nil, err
}

// rotatingLog is an io.Writer that rotates domain.ListenerLogPath to one old
// copy (".1") when it would grow past listenerLogMaxBytes, so a long-running
// listener never fills the disk with its own log.
type rotatingLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openRotatingLog(path string) (*rotatingLog, error) {
	if err := os.MkdirAll(domain.DataDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	size := int64(0)
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	return &rotatingLog{path: path, f: f, size: size}, nil
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size >= listenerLogMaxBytes {
		if err := r.rotate(); err != nil {
			// Keep writing to the current file; losing rotation is better
			// than losing the line.
			fmt.Fprintf(os.Stderr, "  listen: could not rotate the log file: %v\n", err)
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingLog) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	old := r.path + ".1"
	os.Remove(old)
	renameErr := os.Rename(r.path, old)
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	r.f = f
	r.size = 0
	return renameErr
}

func (r *rotatingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

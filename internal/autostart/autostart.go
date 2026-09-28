// Package autostart registers the freecad-mcp listener to start with the
// user's session, per user and without admin rights: a Task Scheduler logon
// task on Windows, a LaunchAgent on macOS, a systemd user unit (plus an XDG
// autostart entry) on Linux.
//
// Starting, stopping and restarting always go through the OS mechanism, so
// the listener runs detached from the calling process and is restarted after
// a crash. Stopping or restarting it never ends a FreeCAD it launched.
package autostart

import "errors"

// Names of the registered entry.
const (
	WindowsTaskName = "freecad-mcp listener"
	MacLabel        = "io.github.sairaph.freecad-mcp.listener"
	LinuxUnit       = "freecad-mcp-listener.service"
	LinuxDesktop    = "freecad-mcp-listener.desktop"
)

// Entry is what the registered listener runs:
// "<Executable> listen --user-data-dir <UserDataDir> [--rpc-port <RPCPort>]
// [--freecad <FreeCADPath>]".
type Entry struct {
	Executable  string // absolute path of the freecad-mcp binary
	UserDataDir string // FreeCAD user data directory holding the addon settings
	RPCPort     int    // the addon's RPC port; 0 means domain.DefaultRPCPort (flag omitted)
	// LogFile receives launchd's stdout and stderr of the listener on macOS:
	// domain.ListenerStdoutPath, never domain.ListenerLogPath, which the
	// listener rotates itself (launchd would keep writing the rotated copy).
	LogFile string
	// FreeCADPath is the FreeCAD GUI command found when this entry was
	// registered (domain.JoinCommand of what FREECAD_MCP_FREECAD or
	// headless.DetectGUI resolved to), written as --freecad so the listener
	// starts that FreeCAD without depending on its own environment or PATH.
	// Empty leaves discovery to the listener process itself.
	FreeCADPath string
}

// State is the registration as the OS reports it.
type State struct {
	Registered bool
	Running    bool
	// Detail is a one-line description for the doctor, such as the task
	// status or the unit's active state; never localised prose parsed by
	// this package.
	Detail string
}

// ErrUnsupported is returned on an OS without an autostart mechanism here.
var ErrUnsupported = errors.New("starting the freecad-mcp listener with the session is not supported on this system")

// Register creates or replaces the entry. It is idempotent.
func Register(e Entry) error { return register(e) }

// Unregister stops the listener and removes the entry; nothing registered is
// not an error.
func Unregister() error { return unregister() }

// Start starts the registered listener now.
func Start() error { return start() }

// Stop stops the registered listener (on Windows through the task, never by
// killing the process).
func Stop() error { return stop() }

// Restart stops and starts the registered listener, so it runs the binary
// the entry names.
func Restart() error { return restart() }

// Status reports whether the entry is registered and the listener running.
func Status() (State, error) { return status() }

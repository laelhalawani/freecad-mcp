// Package listenerapi holds the HTTP API shared by the freecad-mcp listener
// (`freecad-mcp listen`, internal/listener) and its clients (internal/remote,
// the TUI and the doctor): paths, headers, request and reply shapes.
//
// Every reply of a listener carries domain.HeaderListener. Times cross
// machines as durations in seconds, never as clock times, so clock skew
// between two computers cannot change what they mean.
package listenerapi

import (
	"time"

	"github.com/sairaph/mcp-wizard/daemon/lock"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
)

// Service is the value of Status.Service, which identifies a listener.
const Service = "freecad-mcp-listener"

// Paths. Every path takes POST only.
const (
	PathStatus = "/listener/status" // Status (request body {} or empty JSON)
	PathStart  = "/listener/start"  // StartRequest -> StartResult
	PathRPC    = "/RPC2"            // XML-RPC, forwarded byte for byte to the addon
)

// Content types the listener requires (else 415).
const (
	ContentTypeJSON = "application/json" // /listener/*
	ContentTypeXML  = "text/xml"         // /RPC2; application/xml is accepted too
)

// MaxBodyBytes bounds an /RPC2 request body (else 413; a larger
// Content-Length is refused before the body is read).
const MaxBodyBytes = 64 << 20

// Client-side waits for listener calls.
const (
	StatusTimeout = 10 * time.Second
	StartTimeout  = 30 * time.Second
	ProbeTimeout  = 10 * time.Second
)

// Values of Status.RPC.
const (
	RPCReachable   = "reachable"
	RPCUnreachable = "unreachable"
)

// Values of StartResult.State besides the freecad.Launch* states.
const StateAlreadyRunning = "already_running"

// LaunchInfo is a freecad.LaunchState as it crosses the network.
type LaunchInfo struct {
	// State is "" (nothing launched), freecad.LaunchStarted,
	// freecad.LaunchForwarded or freecad.LaunchExited.
	State string `json:"state"`
	PID   int    `json:"pid,omitempty"`
	// PIDIsLauncher is true while PID is the process the launcher itself
	// started, not confirmed to be FreeCAD's own: a wrapper script or an
	// AppImage's AppRun can exec a child rather than replace itself, so the
	// two pids can differ (live-fixes review N8). FromLaunchState always
	// sets it when PID is filled, since it only ever has the launcher's own
	// tracking to go on; handleStatus clears it once the addon's own pid
	// (get_rpc_status's, status_snapshot.py's os.getpid()) replaces PID.
	PIDIsLauncher bool   `json:"pid_is_launcher,omitempty"`
	Executable    string `json:"executable,omitempty"` // a command on the FreeCAD computer
	// ElapsedSeconds is the time since the launch started; nil when nothing
	// was launched.
	ElapsedSeconds *float64 `json:"elapsed_seconds,omitempty"`
	// ExitedSecondsAgo is the time since the launched process exited; nil
	// while it runs.
	ExitedSecondsAgo *float64 `json:"exited_seconds_ago,omitempty"`
	ExitCode         *int     `json:"exit_code,omitempty"`
	LogFile          string   `json:"log_file,omitempty"` // a path on the FreeCAD computer
	LogTail          string   `json:"log_tail,omitempty"` // the last 4 KiB of that log
	Port             int      `json:"port,omitempty"`     // the addon's RPC port
	Reused           bool     `json:"reused,omitempty"`   // see freecad.LaunchState.Reused
}

// Status is the reply of PathStatus.
type Status struct {
	Service       string     `json:"service"`        // always Service
	Version       string     `json:"version"`        // freecad-mcp version of the listener
	Protocol      int        `json:"protocol"`       // domain.ProtocolVersion of the listener
	RemoteEnabled bool       `json:"remote_enabled"` // "Share this PC" is on
	RPC           string     `json:"rpc"`            // RPCReachable when the addon answers ping
	Display       bool       `json:"display"`        // a graphical session is available to start FreeCAD in
	FreeCAD       LaunchInfo `json:"freecad"`        // what the listener launched
	// Hostname is os.Hostname() of the computer running the listener, "" on
	// error. A client reached through a loopback host name (contract 3, "localhost")
	// compares it against its own to tell this computer apart from another
	// one also answering that name, such as WSL's own localhost port
	// forwarding (live check L11).
	Hostname string `json:"hostname,omitempty"`
	// Platform is runtime.GOOS of the computer running the listener, and
	// WSL is domain.IsWSL() there: a hostname can be shared across a
	// loopback boundary (WSL takes the Windows computer's own name by
	// default), so a client compares these too, as a second, independent
	// signal (live-fixes review M1).
	Platform string `json:"platform,omitempty"`
	WSL      bool   `json:"wsl,omitempty"`
}

// StartRequest is the body of PathStart.
type StartRequest struct {
	// File is an .FCStd path on the FreeCAD computer to open, or "".
	File string `json:"file,omitempty"`
}

// StartResult is the reply of PathStart.
type StartResult struct {
	// State is StateAlreadyRunning (the addon answered; nothing launched),
	// freecad.LaunchStarted, freecad.LaunchForwarded or freecad.LaunchExited.
	State   string     `json:"state"`
	FreeCAD LaunchInfo `json:"freecad"`
}

// Error is the JSON body of every listener reply that is not 200. Code is
// one of the render error codes (not_found, invalid_input, authentication,
// forbidden, rate_limited, conflict, unavailable, internal_error); Reason
// names the exact refusal (the Reason* constants), so a client tells refusals
// with the same status apart without reading the text.
type Error struct {
	Code   string `json:"code"`
	Error  string `json:"error"`
	Hint   string `json:"hint,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Values of Error.Reason, one per refusal the listener writes.
const (
	ReasonRemoteOff       = "remote_off"       // 403: remote access is turned off on the listener's computer
	ReasonOrigin          = "origin"           // 403: the request carried an Origin header (a web page)
	ReasonMethod          = "method"           // 405: not POST
	ReasonContentType     = "content_type"     // 415: the wrong Content-Type for the path
	ReasonRateLimited     = "rate_limited"     // 429: too many wrong passwords from this IP (Retry-After)
	ReasonPassword        = "password"         // 401: the password is missing or wrong
	ReasonNotFound        = "not_found"        // 404: unknown path
	ReasonTooLarge        = "too_large"        // 413: the body exceeds the path's limit
	ReasonFreeCADDown     = "freecad_down"     // 503 on /RPC2: the addon refused the connection (FreeCAD not running)
	ReasonFreeCADStarting = "freecad_starting" // 503 on /RPC2: a launch this listener started is still in flight
	ReasonProxyFailed     = "proxy_failed"     // 502 on /RPC2: any other failure forwarding to the addon
	ReasonTestMode        = "test_mode"        // 503: a test listener (Options.Once) starts nothing and forwards nothing
	ReasonBadRequest      = "bad_request"      // 400: invalid JSON or an invalid file on /listener/start
	ReasonNoDisplay       = "no_display"       // 503 on /listener/start: no graphical session to start FreeCAD in
	ReasonNoFreeCAD       = "no_freecad"       // 503 on /listener/start: no FreeCAD GUI executable was found
	ReasonLaunchFailed    = "launch_failed"    // 500 on /listener/start: FreeCAD could not be started
)

// MaxJSONBodyBytes bounds a /listener/* request body (else 413).
const MaxJSONBodyBytes = 64 << 10

// FromLaunchState converts ls into its network form as of now.
func FromLaunchState(ls freecad.LaunchState, now time.Time) LaunchInfo {
	info := LaunchInfo{
		State:         ls.State,
		PID:           ls.PID,
		PIDIsLauncher: ls.PID != 0,
		Executable:    ls.Executable,
		ExitCode:      ls.ExitCode,
		LogFile:       ls.LogPath,
		Port:          ls.Port,
		Reused:        ls.Reused,
	}
	if ls.State != "" && !ls.StartedAt.IsZero() {
		elapsed := max(0, now.Sub(ls.StartedAt).Seconds())
		info.ElapsedSeconds = &elapsed
	}
	if !ls.ExitedAt.IsZero() {
		ago := max(0, now.Sub(ls.ExitedAt).Seconds())
		info.ExitedSecondsAgo = &ago
	}
	return info
}

// LaunchState rebuilds the launch state on this machine's clock: StartedAt is
// now minus ElapsedSeconds and ExitedAt now minus ExitedSecondsAgo.
func (i LaunchInfo) LaunchState(now time.Time) freecad.LaunchState {
	ls := freecad.LaunchState{
		State:      i.State,
		PID:        i.PID,
		Executable: i.Executable,
		ExitCode:   i.ExitCode,
		LogPath:    i.LogFile,
		Port:       i.Port,
		Reused:     i.Reused,
	}
	if i.ElapsedSeconds != nil {
		ls.StartedAt = now.Add(-time.Duration(*i.ElapsedSeconds * float64(time.Second)))
	}
	if i.ExitedSecondsAgo != nil {
		ls.ExitedAt = now.Add(-time.Duration(*i.ExitedSecondsAgo * float64(time.Second)))
	}
	return ls
}

// Running reports whether a listener holds domain.ListenerLockPath. It takes
// the lock for a moment itself when it is free; the listener retries its own
// lock for that reason.
func Running() bool {
	return lock.IsRunning(domain.ListenerLockPath())
}

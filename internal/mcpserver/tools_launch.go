package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/freecad-mcp/internal/remote"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
	"github.com/sairaph/mcp-wizard/render"
)

type startFreeCADInput struct {
	File *string `json:"file,omitempty"`
}

// expectedWaitSeconds is how long a fresh FreeCAD GUI start takes before its
// RPC server answers, given in the reply so a client knows how long to wait
// before its first get_rpc_status poll.
const expectedWaitSeconds = 15

// pollAdvice is the body of a start_freecad reply that launched or forwarded
// to FreeCAD: the exact next steps, since the reply itself returns at once.
const pollAdvice = "Call get_rpc_status with {} in about 15 seconds and every 3 to 5 seconds after that " +
	"until it reports rpc: reachable; give up after 120 seconds."

type startFreeCADFront struct {
	State      string `yaml:"state"`
	Executable string `yaml:"executable,omitempty"`
	PID        int    `yaml:"pid,omitempty"`
	// PIDIsLauncher is true while PID is the process the launcher itself
	// started, not confirmed to be FreeCAD's own: a wrapper script or an
	// AppImage's AppRun can exec a child rather than replace itself, so the
	// two pids can differ until the addon answers (live-fixes review N8).
	// get_rpc_status reports FreeCAD's own pid once it is reachable
	// (status_snapshot.py's os.getpid()).
	PIDIsLauncher       bool   `yaml:"pid_is_launcher,omitempty"`
	Port                int    `yaml:"port,omitempty"`
	LogFile             string `yaml:"log_file,omitempty"`
	ExpectedWaitSeconds int    `yaml:"expected_wait_seconds,omitempty"`
}

func (s *Server) registerLaunchTools() {
	addTool(s.mcpServer, "start_freecad", inputSchema[startFreeCADInput](nil), s.startFreeCAD)
}

func (s *Server) startFreeCAD(ctx context.Context, _ *mcp.CallToolRequest, in startFreeCADInput) (*mcp.CallToolResult, any, error) {
	// Classify the endpoint before touching file: a listener endpoint only
	// gets a syntax check (the path names a file on the FreeCAD computer, not
	// this one), while a local endpoint still needs the full
	// freecad.ValidateStartFile check. Bad input is invalid_input in every
	// branch, including already_running.
	listener := s.fc.isListener(ctx)

	file := ""
	if in.File != nil {
		if listener {
			if ferr := freecad.CheckStartFileSyntax(*in.File); ferr != nil {
				return startFileFailure(ferr), nil, nil
			}
			file = *in.File
		} else {
			f, ferr := freecad.ValidateStartFile(*in.File)
			if ferr != nil {
				return startFileFailure(ferr), nil, nil
			}
			file = f
		}
	}

	if listener {
		return s.startFreeCADListener(ctx, file, in.File)
	}

	// Local endpoint: unchanged behaviour.
	//
	// Ping first: an addon that already answers means nothing needs launching,
	// whatever this server's launcher last saw (another process, or a FreeCAD
	// started outside this tool entirely). get alone is not enough: it returns
	// a cached connection at once with no network call, so a FreeCAD that
	// crashed or was closed after an earlier tool call would otherwise still
	// read as already_running here. A fresh Ping confirms it is actually still
	// there; resetIfCurrent drops a stale cache so the launch path below (and
	// every later tool call) reconnects instead of reusing it.
	conn, err := s.fc.get(ctx)
	if err == nil {
		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		ok, perr := conn.Ping(pingCtx)
		cancel()
		if perr == nil && ok {
			// The lock applies to every agent, including one on the FreeCAD
			// computer itself (requirements: "applies to all agents including
			// local ones"), so this path checks the same as the listener path
			// (startFreeCADListener) rather than only reporting already_running.
			if inUse := s.sessionHeldByAnother(ctx); inUse != nil {
				return s.withNotice(failure(ctx, "start FreeCAD", inUse, "")), nil, nil
			}
			return s.withNotice(alreadyRunningResult(s.config.FreeCAD.Port, "", in.File)), nil, nil
		}
		s.fc.resetIfCurrent(conn)
		// The cached connection answered ping but refused it (the auth token
		// changed while it was cached, or the addon's browser/host guard now
		// refuses it, for example FREECAD_MCP_HOST no longer matching):
		// FreeCAD is running, so launching or forwarding to it would only
		// repeat the same rejection through get_rpc_status. Report it
		// directly instead, the same way a rejection on a fresh connect
		// already does a few lines below.
		var perrType *xmlrpc.ProtocolError
		if errors.As(perr, &perrType) {
			return render.ErrorResult(protocolRejectedError(s.config.FreeCAD.Host, perrType)), nil, nil
		}
		// The cached connection's ping faulted (the settings file became
		// unreadable since it was cached): FreeCAD is running and reachable,
		// so this is the same case as a fresh connect's ping fault, reported
		// the same way (live check L4), never a reason to launch a second
		// FreeCAD. The specific hint applies only when the fault matches it
		// exactly (live-fixes review N1).
		var faultType *xmlrpc.Fault
		if errors.As(perr, &faultType) {
			hint := "FreeCAD reported this error while handling the call. Check the arguments and retry. " + statusHint
			if faultType.SettingsUnreadable() {
				hint = settingsUnreadableHint
			}
			return render.ErrorResult(render.Error{Code: codeFreeCAD, Message: faultType.String, Hint: hint}), nil, nil
		}
	}
	// The addon answered but rejected the request, faulted on every call
	// because its settings file cannot be read (get's own ping hit the same
	// classification get() detects), or answered as another computer through
	// a loopback host (live check L11, identityMismatchError): FreeCAD is
	// running (or something is, standing in for it), but launching or
	// forwarding to it would only tell the caller to poll get_rpc_status for
	// up to 120 s, which can never succeed while this stands (live check L4:
	// a second FreeCAD must never be launched for it). Report it directly
	// instead, for any such rejection, not only a bad password.
	var te *toolError
	if errors.As(err, &te) && (te.e.Code == render.CodeAuth || te.e.Code == render.CodeForbidden ||
		te.e.Code == codeFreeCAD || te.e.Code == codeIdentityMismatch) {
		return render.ErrorResult(te.e), nil, nil
	}

	if rerr := refuseNonLoopbackHost(s.config.FreeCAD.Host); rerr != nil {
		return render.ErrorResult(*rerr), nil, nil
	}

	state, err := s.launcher.Launch(ctx, file)
	if err != nil {
		if errors.Is(err, freecad.ErrLaunchUnavailable) {
			return render.ErrorResult(render.Error{
				Code:    render.CodeUnavailable,
				Message: "No FreeCAD GUI executable was found on PATH or in the standard install locations, and " + domain.EnvFreecadGUI + " is not set.",
				Hint:    "Install FreeCAD, or set " + domain.EnvFreecadGUI + " to the command that starts it, then call start_freecad with {} again.",
			}), nil, nil
		}
		return render.ErrorResult(render.Error{
			Code:    render.CodeInternal,
			Message: shortMessage(fmt.Sprintf("Could not start FreeCAD: %v", err)),
			Hint:    "Check that FreeCAD is installed correctly and its directory is writable, then call start_freecad with {} again.",
		}), nil, nil
	}
	s.fc.reset()

	return s.withNotice(launchResult(state, func() string { return s.launcher.LogTail(1500) }, file, "")), nil, nil
}

// alreadyRunningResult is start_freecad's already_running reply. port is
// where the caller reached it: the addon's own RPC port for a local
// endpoint (host ""), or the listener's port for a remote one (host names
// that computer), which forwards to the addon's own port there, not port
// itself (live-fixes review, remaining nits: "FreeCAD's RPC server already
// answers on port 9876" wrongly named the listener's port as if it were
// the RPC server's own).
func alreadyRunningResult(port int, host string, file *string) *mcp.CallToolResult {
	body := fmt.Sprintf("FreeCAD's RPC server already answers on port %d; nothing was launched.", port)
	if host != "" {
		body = fmt.Sprintf("FreeCAD on %s already answers, through the listener on port %d; nothing was launched.",
			host, port)
	}
	if file != nil {
		body += fmt.Sprintf("\n\nCall open_document with {\"path\": %q} to open it in the running FreeCAD.", *file)
	}
	return render.SuccessResult(startFreeCADFront{State: "already_running", Port: port}, body)
}

// launchResult builds start_freecad's reply for a freshly started, forwarded
// or exited FreeCAD, whether launched by the local launcher or by a listener
// on another computer: host names that computer for its wording ("" for the
// local launcher, whose hint and body then omit the host). logTail is called
// only for an exited launch, so a local log read never happens for any other
// state.
func launchResult(state freecad.LaunchState, logTail func() string, file, host string) *mcp.CallToolResult {
	if state.State == freecad.LaunchExited {
		code := 0
		if state.ExitCode != nil {
			code = *state.ExitCode
		}
		msg := fmt.Sprintf("FreeCAD exited immediately (exit code %d) instead of starting.", code)
		// shortMessage below keeps only the first maxMessageBytes of msg; a
		// larger tail here would have its most useful part (the last lines,
		// closest to the exit) cut off instead of the tail itself already
		// being the log's end.
		if tail := logTail(); tail != "" {
			msg += "\n\nLast lines of its log:\n" + tail
		}
		hint := fmt.Sprintf("Check the log at %s, then call start_freecad with {} again.", state.LogPath)
		if host != "" {
			hint = fmt.Sprintf("Check the log at %s on %s, then call start_freecad with {} again.", state.LogPath, host)
		}
		return render.ErrorResult(render.Error{
			Code:    render.CodeInternal,
			Message: shortMessage(msg),
			Hint:    hint,
		})
	}

	front := startFreeCADFront{
		State:               state.State,
		Executable:          state.Executable,
		Port:                state.Port,
		LogFile:             state.LogPath,
		ExpectedWaitSeconds: expectedWaitSeconds,
	}
	if state.State == freecad.LaunchStarted {
		front.PID = state.PID
		front.PIDIsLauncher = true
	}
	body := pollAdvice
	if host != "" {
		body = fmt.Sprintf("FreeCAD is starting from %s on %s; its log is %s on %s.\n\n",
			state.Executable, host, state.LogPath, host) + body
	}
	if state.Reused {
		// Launch returned a launch already starting from a moment ago instead
		// of starting FreeCAD again, so this call otherwise reads exactly like
		// a fresh one (same state, same pid): say so, and if a file was given,
		// that it was never passed to FreeCAD, since the earlier launch already
		// started without it.
		elapsed := int(time.Since(state.StartedAt).Round(time.Second).Seconds())
		if elapsed < 0 {
			elapsed = 0
		}
		reused := fmt.Sprintf("Reused the launcher's own process already starting (pid %d, started %d s ago); "+
			"nothing new was launched.", state.PID, elapsed)
		if file != "" {
			reused += fmt.Sprintf(" %q was not opened: file only takes effect for a fresh launch, not a reused one. "+
				"Once get_rpc_status reports rpc: reachable, call open_document with {\"path\": %q} to open it.", file, file)
		}
		body = reused + "\n\n" + body
	}
	if front.PIDIsLauncher {
		body += "\n\npid is the launcher's own process, not confirmed as FreeCAD's own yet (they can differ for " +
			"a wrapper script or an AppImage): call get_rpc_status once it reports rpc: reachable for FreeCAD's " +
			"own pid."
	}
	return render.SuccessResult(front, body)
}

// startFreeCADListener is start_freecad's remote path, once the configured
// endpoint has classified as a listener: file was already checked with
// freecad.CheckStartFileSyntax only, since it names a path on the FreeCAD
// computer, not this one.
func (s *Server) startFreeCADListener(ctx context.Context, file string, rawFile *string) (*mcp.CallToolResult, any, error) {
	result, err := s.fc.listenerStart(ctx, file)
	if err != nil {
		if lerr := listenerCallError(s.config.FreeCAD.Host, s.config.FreeCAD.Port, err); lerr != nil {
			return s.withNotice(render.ErrorResult(*lerr)), nil, nil
		}
		return s.withNotice(failure(ctx, "start FreeCAD", err, "")), nil, nil
	}

	if result.State == listenerapi.StateAlreadyRunning {
		// The listener's own check is a plain ping: it says nothing about who
		// holds the session lock. Ask get_rpc_status before handing the reply
		// back, so an agent never reads "already running" for a FreeCAD
		// another agent currently holds.
		if inUse := s.sessionHeldByAnother(ctx); inUse != nil {
			return s.withNotice(failure(ctx, "start FreeCAD", inUse, "")), nil, nil
		}
		return s.withNotice(alreadyRunningResult(s.config.FreeCAD.Port, s.config.FreeCAD.Host, rawFile)), nil, nil
	}

	ls := result.FreeCAD.LaunchState(time.Now())
	return s.withNotice(launchResult(ls, func() string { return result.FreeCAD.LogTail }, file, s.config.FreeCAD.Host)), nil, nil
}

// listenerCallError adapts a refusal from a direct listener call
// (remote.Status, remote.Start, met through listenerStatus/listenerStart) to
// the render.Error connection.go's listenerRefusalError already builds for
// the same refusal met on the proxied /RPC2 connection, so the two ways of
// reaching a listener report a refusal identically. It returns nil for a
// plain network error (unreachable, left to failure's own classification)
// and for remote.ErrNotListener (not expected here: isListener already
// classified the endpoint as one).
func listenerCallError(host string, port int, err error) *render.Error {
	hostPort := net.JoinHostPort(host, strconv.Itoa(port))
	if errors.Is(err, remote.ErrPasswordRequired) {
		e := listenerRefusalError(hostPort, http.StatusUnauthorized, 0, listenerapi.Error{Reason: listenerapi.ReasonPassword})
		return &e
	}
	var rerr *remote.Error
	if errors.As(err, &rerr) {
		e := listenerRefusalError(hostPort, rerr.StatusCode, rerr.RetryAfter, rerr.Body)
		return &e
	}
	return nil
}

// sessionHeldByAnother reads get_rpc_status's session key to find out
// whether another agent currently holds FreeCAD, returning a
// *freecad.SessionInUseError for failure to report the same way a refused
// call would (nil when the lock is free, held by the caller, or its state
// could not be read). A rejection met while reading it (a wrong password, or
// a guard refusing the request) is itself returned, so the caller reports
// that instead of silently treating a rejection as an unheld lock.
func (s *Server) sessionHeldByAnother(ctx context.Context) error {
	pr := s.fc.probe(ctx)
	if pr.rejected != nil {
		return &toolError{*pr.rejected}
	}
	if pr.identityMismatch != "" {
		// Another computer answers this loopback host (live check L11):
		// never read its session lock as this one's, or as free.
		return &toolError{identityMismatchError(pr.identityMismatch)}
	}
	if !pr.reachable || pr.status == nil {
		return nil
	}
	session, _ := pr.status["session"].(map[string]any)
	if !boolField(session, "held") || boolField(session, "yours") {
		return nil
	}
	idle, _ := intFromStatus(session, "idle_seconds")
	frees, _ := intFromStatus(session, "frees_in_seconds")
	timeoutSeconds, _ := intFromStatus(session, "timeout_seconds")
	return &freecad.SessionInUseError{
		Holder:         str(session, "holder"),
		IdleSeconds:    idle,
		FreesInSeconds: frees,
		TimeoutSeconds: timeoutSeconds,
		Busy:           boolField(session, "busy"),
	}
}

// refuseNonLoopbackHost rejects starting FreeCAD locally unless this server is
// configured for the address the addon actually listens on: rpc_server.py
// always binds loopback (127.0.0.1); only a freecad-mcp listener binds a
// non-loopback address, and start_freecad reaches one through its own path
// (isListener), not this one. Another loopback form (::1, another
// 127.0.0.0/8 address) would never be reachable either.
func refuseNonLoopbackHost(host string) *render.Error {
	if strings.EqualFold(host, "localhost") || host == "127.0.0.1" {
		return nil
	}
	return &render.Error{
		Code: render.CodeInvalidInput,
		Message: fmt.Sprintf("This server is configured for FreeCAD at %s, which does not answer as a "+
			"freecad-mcp listener, so start_freecad cannot start FreeCAD there.", host),
		Hint: fmt.Sprintf("Run `%s connect --clear` (or set FREECAD_MCP_HOST to 127.0.0.1) to use FreeCAD on this "+
			"machine, or turn on \"Share this PC\" in `%s` on that computer and connect to its port 9876.",
			domain.BinaryName, domain.BinaryName),
	}
}

// startFileFailure renders a refused file argument (freecad.ValidateStartFile
// or freecad.CheckStartFileSyntax) as invalid_input.
func startFileFailure(err error) *mcp.CallToolResult {
	var sfe *freecad.StartFileError
	if errors.As(err, &sfe) {
		return render.ErrorResult(render.Error{Code: render.CodeInvalidInput, Message: sfe.Message, Hint: sfe.Hint})
	}
	return render.ErrorResult(render.Error{
		Code:    render.CodeInvalidInput,
		Message: shortMessage(err.Error()),
		Hint:    "Pass the absolute path of a .FCStd document, or omit file.",
	})
}

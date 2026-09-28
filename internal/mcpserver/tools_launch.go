package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
	"github.com/sairaph/mcp-wizard/render"
)

type startFreeCADInput struct {
	File *string `json:"file,omitempty" jsonschema:"absolute path of an .FCStd file to open once FreeCAD has started (optional)"`
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
	State               string `yaml:"state"`
	Executable          string `yaml:"executable,omitempty"`
	PID                 int    `yaml:"pid,omitempty"`
	Port                int    `yaml:"port,omitempty"`
	LogFile             string `yaml:"log_file,omitempty"`
	ExpectedWaitSeconds int    `yaml:"expected_wait_seconds,omitempty"`
}

const startFreeCADDescription = `Start FreeCAD's GUI with the MCP addon's RPC server on this machine, when it is not running yet.

The tool first checks whether FreeCAD already answers; then it reports state already_running and launches nothing. Otherwise it starts FreeCAD detached from this server with a startup macro that starts the RPC server on the configured port, so the addon's auto-start setting does not matter. When another FreeCAD window is already open without the RPC server, that FreeCAD receives the request instead (state forwarded). Pass file to open an .FCStd document once FreeCAD is up; for a FreeCAD that already runs, use open_document.

FreeCAD takes about 15 seconds to start, longer on its first start. The reply returns at once: call get_rpc_status every few seconds until it reports rpc: reachable, then continue with list_documents. Set FREECAD_MCP_FREECAD in the AI client's config when FreeCAD is installed somewhere the server does not find. Only a local FreeCAD can be started; with FREECAD_MCP_HOST set to another machine, start FreeCAD there.`

func (s *Server) registerLaunchTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "start_freecad",
		Description: startFreeCADDescription,
		InputSchema: inputSchema[startFreeCADInput](nil),
	}, s.startFreeCAD)
}

func (s *Server) startFreeCAD(ctx context.Context, _ *mcp.CallToolRequest, in startFreeCADInput) (*mcp.CallToolResult, any, error) {
	// Validate what needs no FreeCAD first, so bad input is invalid_input in
	// every branch, including already_running.
	file := ""
	if in.File != nil {
		f, rerr := validateStartFile(*in.File)
		if rerr != nil {
			return render.ErrorResult(*rerr), nil, nil
		}
		file = f
	}

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
			body := fmt.Sprintf("FreeCAD's RPC server already answers on port %d; nothing was launched.", s.config.FreeCAD.Port)
			if in.File != nil {
				body += fmt.Sprintf("\n\nCall open_document with {\"path\": %q} to open it in the running FreeCAD.", *in.File)
			}
			return s.withNotice(render.SuccessResult(startFreeCADFront{State: "already_running", Port: s.config.FreeCAD.Port}, body)), nil, nil
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
			return render.ErrorResult(protocolRejectedError(perrType)), nil, nil
		}
	}
	// The addon answered but rejected the request (get's own ping or status
	// call hit the same HTTP-level rejection get() detects): FreeCAD is
	// running, but launching or forwarding to it would only tell the caller
	// to poll get_rpc_status for up to 120 s, which can never succeed while
	// the rejection stands. Report it directly instead, for any such
	// rejection, not only a bad auth token.
	var te *toolError
	if errors.As(err, &te) && (te.e.Code == render.CodeAuth || te.e.Code == render.CodeForbidden) {
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
		if tail := s.launcher.LogTail(1500); tail != "" {
			msg += "\n\nLast lines of its log:\n" + tail
		}
		return render.ErrorResult(render.Error{
			Code:    render.CodeInternal,
			Message: shortMessage(msg),
			Hint:    fmt.Sprintf("Check the log at %s, then call start_freecad with {} again.", state.LogPath),
		}), nil, nil
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
	}
	body := pollAdvice
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
		reused := fmt.Sprintf("Reused FreeCAD's launch already starting (pid %d, started %d s ago); nothing new was launched.",
			state.PID, elapsed)
		if file != "" {
			reused += fmt.Sprintf(" %q was not opened: file only takes effect for a fresh launch, not a reused one. "+
				"Once get_rpc_status reports rpc: reachable, call open_document with {\"path\": %q} to open it.", file, file)
		}
		body = reused + "\n\n" + body
	}
	return s.withNotice(render.SuccessResult(front, body)), nil, nil
}

// refuseNonLoopbackHost rejects starting FreeCAD locally unless this server is
// configured for the address the addon actually listens on: rpc_server.py
// binds IPv4 127.0.0.1 unless remote connections are enabled, in which case it
// binds every interface (0.0.0.0), still reachable from this machine at
// 127.0.0.1. Another loopback form (::1, another 127.0.0.0/8 address) would
// never be reachable, and a genuinely remote host can only be started there,
// not by this tool.
func refuseNonLoopbackHost(host string) *render.Error {
	if strings.EqualFold(host, "localhost") || host == "127.0.0.1" {
		return nil
	}
	return &render.Error{
		Code: render.CodeInvalidInput,
		Message: fmt.Sprintf("This server is configured for FreeCAD at %s, which the addon's RPC server does not "+
			"bind (it listens on 127.0.0.1 unless remote connections are turned on), so start_freecad cannot start it there.", host),
		Hint: "Set FREECAD_MCP_HOST to 127.0.0.1 or localhost to start FreeCAD on this machine, or start FreeCAD " +
			"on the configured host yourself, then call get_rpc_status with {}.",
	}
}

// validateStartFile checks the file argument: an absolute path to an
// existing .FCStd file.
func validateStartFile(path string) (string, *render.Error) {
	if !filepath.IsAbs(path) {
		return "", &render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("file must be an absolute path; got %q.", path),
			Hint:    "Pass an absolute path, or omit file to start FreeCAD without opening a document.",
		}
	}
	if !strings.EqualFold(filepath.Ext(path), ".FCStd") {
		return "", &render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("file must be a .FCStd file; got %q.", path),
			Hint:    "Pass the path of a .FCStd document, or omit file. Use import_file after FreeCAD has started for other formats.",
		}
	}
	if _, err := os.Stat(path); err != nil {
		return "", &render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("file does not exist: %s", path),
			Hint:    "Check the path, or omit file to start FreeCAD without opening a document.",
		}
	}
	return path, nil
}

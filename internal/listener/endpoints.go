package listener

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
	"github.com/sairaph/mcp-wizard/render"
)

// handleStatus answers /listener/status.
func (h *handler) handleStatus(w http.ResponseWriter, r *http.Request, snap snapshot) {
	ctx, cancel := context.WithTimeout(r.Context(), listenerapi.StatusTimeout)
	defer cancel()

	rpc := listenerapi.RPCUnreachable
	reachable := h.pingAddon(ctx, snap.Settings.AuthToken)
	if reachable {
		rpc = listenerapi.RPCReachable
	}

	info := listenerapi.FromLaunchState(h.launcher.State(), time.Now())
	info.LogTail = h.launcher.LogTail(logTailBytes)
	if reachable && info.PID != 0 {
		// A launcher wrapper or an AppImage's AppRun (live check N8) can
		// exec a child rather than replace itself, so the process the
		// launcher tracked is not always FreeCAD's own: once the addon
		// answers, ask it directly instead, the way get_rpc_status already
		// does (status_snapshot.py os.getpid()).
		if pid, ok := h.addonPID(ctx, snap.Settings.AuthToken); ok {
			info.PID = pid
			info.PIDIsLauncher = false
		}
	}

	hostname, _ := os.Hostname()

	writeJSON(w, http.StatusOK, listenerapi.Status{
		Service:       listenerapi.Service,
		Version:       h.version,
		Protocol:      domain.ProtocolVersion,
		RemoteEnabled: snap.Settings.RemoteEnabled,
		RPC:           rpc,
		Display:       displayAvailable(),
		FreeCAD:       info,
		Hostname:      hostname,
		Platform:      runtime.GOOS,
		WSL:           domain.IsWSL(),
	})
}

// handleStart answers /listener/start, in order: invalid JSON, the
// test-listener refusal, already_running, no display, the file argument,
// then Launch.
func (h *handler) handleStart(w http.ResponseWriter, r *http.Request, snap snapshot) {
	ctx, cancel := context.WithTimeout(r.Context(), listenerapi.StartTimeout)
	defer cancel()

	var req listenerapi.StartRequest
	if err := decodeJSONBody(r, &req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			h.reject(w, r, http.StatusRequestEntityTooLarge, render.CodeInvalidInput, listenerapi.ReasonTooLarge,
				"The request body is too large.", "")
			return
		}
		h.reject(w, r, http.StatusBadRequest, render.CodeInvalidInput, listenerapi.ReasonBadRequest,
			"The request body is not valid JSON.", "")
		return
	}

	if h.once {
		writeListenerError(w, http.StatusServiceUnavailable, render.CodeUnavailable, listenerapi.ReasonTestMode,
			"This is a test listener; it does not start FreeCAD.", "")
		return
	}

	if h.pingAddon(ctx, snap.Settings.AuthToken) {
		writeJSON(w, http.StatusOK, listenerapi.StartResult{
			State:   listenerapi.StateAlreadyRunning,
			FreeCAD: listenerapi.FromLaunchState(h.launcher.State(), time.Now()),
		})
		return
	}

	if !displayAvailable() {
		writeListenerError(w, http.StatusServiceUnavailable, render.CodeUnavailable, listenerapi.ReasonNoDisplay,
			"No graphical session is available on this computer to start FreeCAD in.",
			"Log in to the desktop of that computer, then call start_freecad with {} again.")
		return
	}

	file := ""
	if req.File != "" {
		f, ferr := freecad.ValidateStartFile(req.File)
		if ferr != nil {
			var sfe *freecad.StartFileError
			if errors.As(ferr, &sfe) {
				writeListenerError(w, http.StatusBadRequest, render.CodeInvalidInput, listenerapi.ReasonBadRequest, sfe.Message, sfe.Hint)
			} else {
				writeListenerError(w, http.StatusBadRequest, render.CodeInvalidInput, listenerapi.ReasonBadRequest, ferr.Error(), "")
			}
			return
		}
		file = f
	}

	state, err := h.launcher.Launch(ctx, file)
	if err != nil {
		if errors.Is(err, freecad.ErrLaunchUnavailable) {
			writeListenerError(w, http.StatusServiceUnavailable, render.CodeUnavailable, listenerapi.ReasonNoFreeCAD,
				"No FreeCAD GUI executable was found on PATH or in the standard install locations on that computer, "+
					"and "+domain.EnvFreecadGUI+" is not set there.",
				"Install FreeCAD on that computer, or set "+domain.EnvFreecadGUI+
					" to the command that starts it there, then call start_freecad with {} again.")
			return
		}
		writeListenerError(w, http.StatusInternalServerError, render.CodeInternal, listenerapi.ReasonLaunchFailed, err.Error(), "")
		h.log.Printf("listener: start_freecad from %s: %v", clientIP(r), err)
		return
	}

	info := listenerapi.FromLaunchState(state, time.Now())
	if state.State == freecad.LaunchExited {
		info.LogTail = h.launcher.LogTail(logTailBytes)
	}
	h.log.Printf("listener: start_freecad from %s: %s (pid %d)", clientIP(r), state.State, state.PID)
	writeJSON(w, http.StatusOK, listenerapi.StartResult{State: state.State, FreeCAD: info})
}

// pingAddon reports whether the addon answers ping within pingAddonTimeout,
// using the stored password: FreeCAD is running whenever it does, even when
// ping itself faults, since that only ever means its settings file cannot
// be read right now (rpc_server.py _dispatch refuses every call, ping
// included, the same way; live check L4). Callers must never launch a
// second FreeCAD, or report FreeCAD as down, because of that fault alone.
func (h *handler) pingAddon(ctx context.Context, token string) bool {
	pingCtx, cancel := context.WithTimeout(ctx, pingAddonTimeout)
	defer cancel()
	conn := freecad.NewConnection("127.0.0.1", h.rpcPort, token, pingAddonTimeout)
	defer conn.Close()
	ok, err := conn.Ping(pingCtx)
	if err == nil {
		return ok
	}
	var fault *xmlrpc.Fault
	return errors.As(err, &fault)
}

// addonPID asks the addon for its own process id (get_rpc_status's "pid",
// status_snapshot.py's os.getpid()), so a launch state can report the pid of
// the process FreeCAD actually is, rather than a launcher wrapper's or an
// AppImage's own child pid, which can differ from FreeCAD's real one (live
// check N8). ok is false when the addon is not reachable, or its reply
// carries no usable pid.
func (h *handler) addonPID(ctx context.Context, token string) (pid int, ok bool) {
	conn := freecad.NewConnection("127.0.0.1", h.rpcPort, token, pingAddonTimeout)
	defer conn.Close()
	statusCtx, cancel := context.WithTimeout(ctx, pingAddonTimeout)
	defer cancel()
	v, err := conn.GetRPCStatus(statusCtx)
	if err != nil {
		return 0, false
	}
	m, ok2 := v.(map[string]any)
	if !ok2 {
		return 0, false
	}
	switch p := m["pid"].(type) {
	case int64:
		return int(p), true
	case float64:
		return int(p), true
	default:
		return 0, false
	}
}

// displayAvailable reports whether a graphical session is available to start
// FreeCAD in: on Linux, DISPLAY or WAYLAND_DISPLAY in the listener's own
// environment (set there by the autostart desktop entry's
// import-environment step, or applyWSLgDefaults below); Windows and macOS
// always report true.
func displayAvailable() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// wslgRuntimeDir is where WSLg (WSL's own X11/Wayland compositor) exposes
// itself whenever it is running, whether or not anyone opened a WSL
// terminal to capture it into the environment first.
const wslgRuntimeDir = "/mnt/wslg"

// applyWSLgDefaults sets DISPLAY, WAYLAND_DISPLAY and XDG_RUNTIME_DIR to
// WSLg's own defaults, for whichever of them this process does not already
// have, when running in WSL with WSLg available (wslgRuntimeDir exists).
// The systemd user unit's own import-environment step (contract 8) only
// runs when the listener is registered or restarted from a shell that
// already has these, never at the unit's own start, so the listener still
// has none of them after WSL restarts and no terminal has been opened since
// (live check L9); setting them here, in this process's own environment,
// reaches both displayAvailable above and the FreeCAD this listener
// launches, which inherits os.Environ() (internal/freecad/launch_state.go).
// A no-op outside Linux, or when WSLg is not present.
func applyWSLgDefaults() {
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := os.Stat(wslgRuntimeDir); err != nil {
		return
	}
	setEnvIfEmpty("DISPLAY", ":0")
	setEnvIfEmpty("WAYLAND_DISPLAY", "wayland-0")
	setEnvIfEmpty("XDG_RUNTIME_DIR", wslgRuntimeDir+"/runtime-dir")
}

func setEnvIfEmpty(key, value string) {
	if os.Getenv(key) == "" {
		os.Setenv(key, value)
	}
}

// decodeJSONBody reads and decodes r's body into v; an empty body (the
// PathStatus request, or a PathStart request with no fields) leaves v
// unchanged.
func decodeJSONBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

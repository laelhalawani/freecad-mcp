package main

// Doctor checks of remote access: "Remote access (this
// PC)" (the listener's registration, process, port, and whether
// /listener/status answers on loopback) and "FreeCAD on another computer"
// (the stored host and its listener). Both report OK "not used" when remote
// access is unused.

import (
	"context"
	"errors"
	"fmt"

	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/autostart"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/freecad-mcp/internal/remote"
)

func remoteChecks() []doctor.Check {
	return []doctor.Check{remoteAccessCheck{}, remoteFreeCADCheck{}}
}

// --- Remote access (this PC) ---

type remoteAccessCheck struct{}

func (remoteAccessCheck) Name() string { return "Remote access (this PC)" }

// Run reports whether this computer's listener, when remote access is used
// at all, is registered to start with the session, actually running, and
// answering on its own port.
func (c remoteAccessCheck) Run(ctx context.Context) doctor.Result {
	located := addoninstall.LocateAll(ctx, freecadCommand())
	enabledAnywhere := false
	port := addoninstall.DefaultListenerPort
	havePort := false
	for _, t := range located.Installed {
		s, err := addoninstall.ReadRemoteSettings(t)
		if err != nil {
			// FreeCAD (and the listener, which answers regardless) may well
			// be fine; only this file could not be used to tell (live check
			// L4): reported as its own failure, ahead of the registration
			// and probe below, rather than silently skipped and read as
			// remote access being off.
			return doctor.Result{Name: c.Name(), Status: doctor.Fail,
				Detail: fmt.Sprintf("%s: %v; save it again with freecad-mcp > Share this PC", addoninstall.SettingsPath(t), err)}
		}
		if !havePort {
			port = s.ListenerPort
			havePort = true
		}
		if s.RemoteEnabled {
			enabledAnywhere = true
			port = s.ListenerPort
		}
	}
	// The probe below always uses the credential store's own password
	// (mirrored there whenever Share this PC last saved successfully) rather
	// than a value freshly read from the settings file above: that file can
	// be the very thing unreadable when the probe most needs the password
	// (live check L4), and the store is the same one the local agents
	// already use to reach this computer's own addon.
	token, _ := loadCredential(ctx, domain.TokenKey)

	state, err := autostart.Status()
	if err != nil {
		// On macOS, checking the registration itself needs an Aqua session
		// and fails outright without one (for example over SSH), whether or
		// not anything was ever registered. Without remote access enabled
		// anywhere, that is not a problem to report; with it enabled, not
		// being able to tell is.
		if !enabledAnywhere {
			return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: "not used"}
		}
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: "checking the listener registration: " + err.Error()}
	}
	switch {
	case !enabledAnywhere && !state.Registered:
		return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: "not used"}
	case enabledAnywhere && !state.Registered:
		return doctor.Result{Name: c.Name(), Status: doctor.Fail,
			Detail: "remote access is on but no listener is registered; open `freecad-mcp` > Share this PC and save it again"}
	}

	// Registered (with or without remote access currently on somewhere): a
	// leftover registration still runs something on the port, so it is
	// checked the same way.
	reachable, probeErr := remote.Probe(ctx, remote.Endpoint{Host: "127.0.0.1", Port: port, Token: token})
	detail := "registered"
	if state.Detail != "" {
		detail = state.Detail
	}
	if state.Running && reachable {
		detail += fmt.Sprintf("; answers on 127.0.0.1:%d", port)
		return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: detail}
	}
	switch {
	case !state.Running:
		detail += "; not running"
	case !reachable && probeErr != nil:
		detail += fmt.Sprintf("; does not answer on 127.0.0.1:%d: %v", port, probeErr)
	default:
		detail += fmt.Sprintf("; does not answer on 127.0.0.1:%d", port)
	}
	return doctor.Result{Name: c.Name(), Status: doctor.Warn, Detail: detail}
}

// --- FreeCAD on another computer ---

type remoteFreeCADCheck struct{}

func (remoteFreeCADCheck) Name() string { return "FreeCAD on another computer" }

// Run reports whether the FreeCAD this MCP server would use, when it is
// FreeCAD on another computer at all, is reachable through its listener.
func (c remoteFreeCADCheck) Run(ctx context.Context) doctor.Result {
	token, storedHost, storedPort, err := loadCredentials(ctx)
	if err != nil {
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: err.Error()}
	}
	settings, err := domain.SettingsFromEnv(token, storedHost, storedPort)
	if err != nil {
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: err.Error()}
	}
	// The probe rule: a loopback host on the addon's default port is always
	// the local addon, never probed.
	if (storedHost == "" && domain.IsLoopbackHost(settings.Host)) ||
		(domain.IsLoopbackHost(settings.Host) && settings.Port == domain.DefaultRPCPort) {
		return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: "not used"}
	}

	status, err := remote.Status(ctx, remote.Endpoint{Host: settings.Host, Port: settings.Port, Token: settings.Token})
	where := hostPort(settings.Host, settings.Port)
	switch {
	case err == nil:
		if status.RPC == listenerapi.RPCReachable {
			return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: where + ": FreeCAD is running"}
		}
		return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: where + ": FreeCAD is not running; agents start it with start_freecad"}
	case errors.Is(err, remote.ErrNotListener):
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: where + " is not a freecad-mcp listener; update freecad-mcp there and turn on \"Share this PC\""}
	case errors.Is(err, remote.ErrPasswordRequired):
		return doctor.Result{Name: c.Name(), Status: doctor.Fail,
			Detail: where + " asks for a password: use `freecad-mcp` > Connect to FreeCAD on another computer, " +
				"or run `freecad-mcp connect --host " + settings.Host + " --password-stdin`"}
	}
	var rerr *remote.Error
	if errors.As(err, &rerr) {
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: fmt.Sprintf("%s: %v", where, rerr)}
	}
	return doctor.Result{Name: c.Name(), Status: doctor.Warn, Detail: fmt.Sprintf("could not reach %s: %v", where, err)}
}

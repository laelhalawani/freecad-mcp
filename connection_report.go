package main

// The connection check of the app and `check-connection`, and the doctor's
// RPC server check: both ask the FreeCAD the MCP server would use, on this
// machine or through a listener on another. Through a listener, FreeCAD not
// running there is reported the same way as locally not started: a warning
// with the advice to start it, never a failure.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
)

// connectionReport checks the RPC server the MCP server would use.
func connectionReport(ctx context.Context, w io.Writer) int {
	settings, err := serverSettings(ctx)
	if err != nil {
		fmt.Fprintf(w, "  [fail] %v\n", err)
		return 1
	}
	// 10 s, not less: right after FreeCAD starts its GUI thread is still busy
	// with startup Python, and a shorter bound was seen to time out here
	// while the same ping succeeded a moment later.
	conn := freecad.NewConnection(settings.Host, settings.Port, settings.Token, 10*time.Second)
	defer conn.Close()
	ok, err := conn.Ping(ctx)
	if err != nil || !ok {
		if down := listenerFreeCADDown(err); down {
			fmt.Fprintf(w, "  [warn] FreeCAD is not running on %s; agents start it with start_freecad.\n", settings.Host)
			return 0
		}
		if fault, unreadable := settingsUnreadableFault(err); unreadable {
			// FreeCAD is running (it answered with a fault, not silence or a
			// refused connection); the fault's own text already carries the
			// "save them again" advice (live check L4).
			fmt.Fprintf(w, "  [fail] %s\n", fault)
			return 1
		}
		fmt.Fprintf(w, "  [fail] FreeCAD RPC server at %s: %v\n", conn.URL(), rpcProblem(err))
		return 1
	}
	fmt.Fprintf(w, "  [ok]   FreeCAD RPC server at %s answers\n", conn.URL())
	if warning, _ := conn.CheckAddonVersion(ctx, version); warning != "" {
		fmt.Fprintf(w, "  [warn] %s\n", warning)
		return 0
	}
	fmt.Fprintln(w, "  [ok]   the addon matches this server")
	return 0
}

func rpcProblem(err error) string {
	if err == nil {
		return "it did not answer ping"
	}
	msg := err.Error()
	if strings.Contains(msg, "401") {
		return "FreeCAD asks for a password; it is set in `" + domain.BinaryName + "` > Share this PC on the computer running FreeCAD"
	}
	return msg + " (is FreeCAD running with the RPC server started?)"
}

// listenerFreeCADDown reports whether err is a listener's 503 saying FreeCAD
// is not running there (domain.ListenerFreeCADDown on the /RPC2 reply):
// unreachable in the ordinary sense, but expected whenever nothing has
// started FreeCAD there yet, so it is never a failure.
func listenerFreeCADDown(err error) bool {
	var perr *xmlrpc.ProtocolError
	return errors.As(err, &perr) && perr.Listener == domain.ListenerFreeCADDown
}

// settingsUnreadableFault reports whether err is the addon's settings file
// being unreadable, and its own message when it is: FreeCAD answered (a
// fault, not silence or a refused connection), so this is never "not
// running" (live check L4).
func settingsUnreadableFault(err error) (string, bool) {
	var fault *xmlrpc.Fault
	if errors.As(err, &fault) && fault.SettingsUnreadable() {
		return fault.String, true
	}
	return "", false
}

type rpcCheck struct{}

func (rpcCheck) Name() string { return "FreeCAD RPC server" }

func (c rpcCheck) Run(ctx context.Context) doctor.Result {
	settings, err := serverSettings(ctx)
	if err != nil {
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: err.Error()}
	}
	// 10 s: see connectionReport's comment on the same bound.
	conn := freecad.NewConnection(settings.Host, settings.Port, settings.Token, 10*time.Second)
	defer conn.Close()
	conn.VersionCheckTimeout = 10 * time.Second
	ok, err := conn.Ping(ctx)
	if err != nil || !ok {
		if listenerFreeCADDown(err) {
			return doctor.Result{Name: c.Name(), Status: doctor.Warn,
				Detail: fmt.Sprintf("FreeCAD is not running on %s; agents start it with start_freecad", settings.Host)}
		}
		if fault, unreadable := settingsUnreadableFault(err); unreadable {
			return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: fault}
		}
		// FreeCAD not running is normal when nothing is being modelled.
		return doctor.Result{Name: c.Name(), Status: doctor.Warn, Detail: fmt.Sprintf("%s: %s", conn.URL(), rpcProblem(err))}
	}
	if warning, _ := conn.CheckAddonVersion(ctx, version); warning != "" {
		return doctor.Result{Name: c.Name(), Status: doctor.Warn, Detail: warning}
	}
	return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: conn.URL() + " answers; versions match"}
}

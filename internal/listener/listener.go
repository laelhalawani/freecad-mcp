// Package listener is the freecad-mcp listener's HTTP server: the only
// network-facing endpoint of a computer that shares FreeCAD ("Share this
// PC"). It enforces the allowed IPs, the browser guard and the password,
// forwards /RPC2 to the addon on 127.0.0.1, and starts FreeCAD on request.
package listener

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
)

// Options configures Run.
type Options struct {
	// UserDataDir is the FreeCAD user data directory whose addon settings
	// file (freecad_mcp_settings.json) holds remote_enabled, allowed_ips,
	// auth_token and listener_port. It is re-read when it changes.
	UserDataDir string
	// RPCPort is the addon's RPC port on 127.0.0.1 (default
	// domain.DefaultRPCPort).
	RPCPort int
	// Version is the freecad-mcp version reported by /listener/status.
	Version string
	// Launcher starts FreeCAD for /listener/start; nil means one built from
	// the environment (FREECAD_MCP_FREECAD) with RPCPort.
	Launcher *freecad.Launcher
	// Log receives one line per start, stop and rejected request (never a
	// password); nil discards them.
	Log io.Writer
	// Override, when set, replaces the settings file: the TUI's Test
	// connection serves the values on screen before they are saved.
	Override *addoninstall.RemoteSettings
	// Once is the TUI test mode: serve with Override until ctx ends, and
	// refuse /listener/start and /RPC2 (nothing is launched or forwarded by
	// a test).
	Once bool
	// Ready, when set, is called once with the address the listener bound.
	Ready func(addr string)
}

// Timeouts and limits of the listener's HTTP surface.
const (
	// settingsPollInterval bounds how often the settings file's modification
	// time and size are restated: "checked at most every 2 s on a request".
	settingsPollInterval = 2 * time.Second
	// pingAddonTimeout is how long /listener/status and /listener/start wait
	// for the addon to answer ping before reporting it unreachable.
	pingAddonTimeout = 3 * time.Second
	// logTailBytes is how much of the launch log a reply carries.
	logTailBytes = 4096
	// shutdownGrace is how long Run waits for in-flight requests to finish
	// once ctx is cancelled, before closing every connection outright.
	shutdownGrace = 5 * time.Second
)

// Run serves until ctx is cancelled, then shuts down gracefully. It returns
// an error when there is nowhere to read settings from, when it cannot bind
// its port, or when the settings' allowed_ips does not parse (an empty or
// invalid list refuses to start). The caller (listen.go) logs that error
// once and retries every 30 s until it serves, rather than exiting (live
// check L3: an OS-level restart hook is not guaranteed to retry a program
// that exits non-zero, only one that fails to launch at all), so Run itself
// does not also log these startup refusals: logging them here too would
// only duplicate that one line.
func Run(ctx context.Context, opts Options) error {
	logger := log.New(orDiscard(opts.Log), "", log.LstdFlags)

	// A no-op outside Linux or without WSLg; see applyWSLgDefaults (live
	// check L9). Done before anything else touches DISPLAY or launches
	// FreeCAD, so both see it.
	applyWSLgDefaults()

	if opts.UserDataDir == "" && opts.Override == nil {
		return errors.New("no FreeCAD user data directory was given, and there is no settings file to read remote access settings from")
	}

	target := addoninstall.Target{UserDataDir: opts.UserDataDir}
	settings := newSettingsCache(target, opts.Override, logger)

	snap := settings.snapshot()
	if snap.AllowedErr != nil {
		// AllowedErr covers the whole settings file, not only allowed_ips
		// (settingsCache.allowListFailed): the file may be invalid JSON, or
		// hold a value of the wrong type (for example a non-string
		// auth_token) elsewhere in it, not only an unparseable allowed_ips.
		return fmt.Errorf("the settings file cannot be used: %w", snap.AllowedErr)
	}

	port := snap.Settings.ListenerPort
	if port <= 0 {
		port = addoninstall.DefaultListenerPort
	}
	// The bind address (or addresses) is decided once, from the settings at
	// start: a later change of port or of loopback-only needs a restart, so
	// there is no need to rebind while serving. The allow list itself is
	// still re-read live by allowedListener and handler.
	lns, err := bindListeners(port, snap.AllowedPrefixes)
	if err != nil {
		return err
	}

	rpcPort := opts.RPCPort
	if rpcPort <= 0 {
		rpcPort = domain.DefaultRPCPort
	}
	launcher := opts.Launcher
	if launcher == nil {
		launcher = defaultLauncher(rpcPort)
	}

	rlog := newRateLimitedLog(logger)

	h := &handler{
		settings: settings,
		rpcPort:  rpcPort,
		launcher: launcher,
		version:  opts.Version,
		once:     opts.Once,
		log:      logger,
		rlog:     rlog,
		backoff:  newBackoff(),
	}
	h.proxy = newRPCProxy(rpcPort, h)

	srv := &http.Server{
		Handler: h,
		// Read bounds only; no WriteTimeout, so a long execute_code proxied
		// over /RPC2 is bounded only by the client's own wait. ReadTimeout
		// bounds reading the request line, headers and body; net/http clears
		// the read deadline once the body has been read to its end, so it
		// never cuts off a handler that is still waiting on a slow addon
		// reply after that.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          logger,
	}

	wrapped := make([]net.Listener, len(lns))
	for i, ln := range lns {
		wrapped[i] = &allowedListener{Listener: ln, settings: settings, rlog: rlog}
	}

	if opts.Ready != nil {
		opts.Ready(lns[0].Addr().String())
	}
	boundAddrs := lns[0].Addr().String()
	for _, ln := range lns[1:] {
		boundAddrs += " and " + ln.Addr().String()
	}
	logger.Printf("listener: serving on %s, forwarding /RPC2 to 127.0.0.1:%d", boundAddrs, rpcPort)

	return serveAll(ctx, srv, wrapped, logger)
}

// serveAll runs srv.Serve on every listener in lns (more than one only when
// a loopback-only allowed list names both address families, bindListeners)
// until ctx is cancelled, then shuts srv down gracefully. A single
// *http.Server can Serve more than one listener at once: Shutdown and
// Close each affect every listener currently registered with it, so one
// graceful shutdown stops all of them together.
func serveAll(ctx context.Context, srv *http.Server, lns []net.Listener, logger *log.Logger) error {
	errCh := make(chan error, len(lns))
	for _, ln := range lns {
		go func(ln net.Listener) { errCh <- srv.Serve(ln) }(ln)
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Printf("listener: graceful shutdown did not finish in time, closing now: %v", err)
			srv.Close()
		}
		// Wait for every Serve call to actually return, so no goroutine
		// outlives Run.
		for range lns {
			<-errCh
		}
		return nil
	case err := <-errCh:
		// One listener's Serve returned on its own (not through the
		// shutdown above): stop the rest of the server with it rather than
		// leaving them running without whichever one just failed.
		srv.Close()
		for i := 1; i < len(lns); i++ {
			<-errCh
		}
		// Not logged here: the caller (listen.go) logs any non-nil error
		// Run returns exactly once, so logging it again here would only
		// duplicate that line.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// orDiscard returns w, or io.Discard when it is nil.
func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// defaultLauncher builds a Launcher from the environment
// (domain.EnvFreecadGUI), for a listener run without an explicit
// Options.Launcher, as the process shell (listen.go) does.
func defaultLauncher(rpcPort int) *freecad.Launcher {
	settings := domain.Settings{Port: rpcPort}
	if v := strings.TrimSpace(os.Getenv(domain.EnvFreecadGUI)); v != "" {
		if cmd, err := domain.SplitCommand(v); err == nil {
			settings.FreecadGUI = cmd
		}
	}
	return freecad.NewLauncher(settings)
}

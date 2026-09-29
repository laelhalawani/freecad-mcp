// Package mcpserver provides the MCP server setup and tool registration.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/headless"
	"github.com/sairaph/freecad-mcp/internal/remote"
	"github.com/sairaph/freecad-mcp/internal/winsession"
)

// Server wraps the MCP server and the FreeCAD connection.
type Server struct {
	mcpServer *mcp.Server
	config    Config
	launcher  *freecad.Launcher
	fc        *connector
	remote    remoteToolsState // whether release_session and close_freecad are listed (visibility.go)
	jobs      *headless.Jobs   // background execute_code_headless jobs
	calls     *callJobs        // calls that moved to the background (tools_call_jobs.go)

	// sessionOf returns the Windows session of a process (winsession.Of);
	// tests replace it.
	sessionOf func(pid int) (uint32, error)
	// desktopListener finds the freecad-mcp listener on this computer that
	// start_freecad goes through when this process has no desktop
	// (localListenerEndpoint); tests replace it.
	desktopListener func(ctx context.Context, token string) remote.Endpoint

	// backgroundAfterHook, when above zero, replaces the addon's
	// background_after_minutes. Tests set it; it is not a setting.
	backgroundAfterHook time.Duration
}

// New creates a new MCP server with the given config and registers its tools.
func New(config Config) *Server {
	launcher := freecad.NewLauncher(config.FreeCAD)
	srv := &Server{
		config:          config,
		launcher:        launcher,
		fc:              newConnector(config.FreeCAD, config.Version, launcher),
		jobs:            headless.NewJobs(),
		calls:           newCallJobs(),
		sessionOf:       winsession.Of,
		desktopListener: localListenerEndpoint,
		mcpServer: mcp.NewServer(
			&mcp.Implementation{
				Name:    "freecad",
				Title:   "FreeCAD",
				Version: config.Version,
			},
			&mcp.ServerOptions{
				Instructions: serverInstructions,
			},
		),
	}
	srv.fc.onLock = srv.setRemoteTools
	srv.fc.onStatus = srv.observeStatus

	srv.registerLaunchTools()
	srv.registerDocumentTools()
	srv.registerDocumentSaveTools()
	srv.registerImportTools()
	srv.registerExportTools()
	srv.registerObjectTools()
	srv.registerRecomputeTools()
	srv.registerPrintabilityTools()
	srv.registerMeshTools()
	srv.registerUndoTools()
	srv.registerSpreadsheetTools()
	srv.registerInspectTools()
	srv.registerCodeTools()
	srv.registerViewTools()
	srv.registerStatusTools()
	srv.registerPrompts()
	srv.setRemoteTools(initialRemoteTools(config.FreeCAD))
	// The first is outermost: the background wrapper runs inside the session
	// identity, so the call it detaches keeps it, and the error log is
	// innermost, so it also sees the result of a call that moved to the
	// background.
	srv.mcpServer.AddReceivingMiddleware(invalidArguments, srv.sessionIdentity, srv.backgroundLongCalls, logToolErrors)

	return srv
}

// MCPServer exposes the underlying server, for tests.
func (s *Server) MCPServer() *mcp.Server { return s.mcpServer }

// Run starts the server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	defer s.fc.close()
	// Background headless jobs end with the server.
	defer s.jobs.StopAll()
	s.jobs.SweepAt(s.config.FreeCAD.FreecadCmd)
	// Deferred after close, so it runs first: the release needs the connection.
	defer s.releaseOnExit()
	switch s.config.Transport {
	case "http":
		return s.runHTTP(ctx)
	default:
		return s.runStdio(ctx)
	}
}

func (s *Server) runStdio(ctx context.Context) error {
	return s.mcpServer.Run(ctx, &mcp.StdioTransport{})
}

func (s *Server) runHTTP(ctx context.Context) error {
	addr := s.config.HTTPAddr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			return s.mcpServer
		},
		&mcp.StreamableHTTPOptions{},
	)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "error shutting down HTTP server: %v\n", err)
		}
	}()

	fmt.Fprintf(os.Stderr, "freecad-mcp listening on %s (Streamable HTTP)\n", addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}
	return nil
}

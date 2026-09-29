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
)

// Server wraps the MCP server and the FreeCAD connection.
type Server struct {
	mcpServer *mcp.Server
	config    Config
	launcher  *freecad.Launcher
	fc        *connector
	remote    remoteToolsState // whether release_session and close_freecad are listed (visibility.go)
}

// New creates a new MCP server with the given config and registers its tools.
func New(config Config) *Server {
	launcher := freecad.NewLauncher(config.FreeCAD)
	srv := &Server{
		config:   config,
		launcher: launcher,
		fc:       newConnector(config.FreeCAD, config.Version, launcher),
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
	srv.mcpServer.AddReceivingMiddleware(invalidArguments, srv.sessionIdentity)

	return srv
}

// MCPServer exposes the underlying server, for tests.
func (s *Server) MCPServer() *mcp.Server { return s.mcpServer }

// Run starts the server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	defer s.fc.close()
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

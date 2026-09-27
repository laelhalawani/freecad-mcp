package mcpserver

import "github.com/laelhalawani/freecad-mcp/internal/domain"

// Config holds the MCP server configuration.
type Config struct {
	Version   string
	Transport string // "stdio" or "http"
	HTTPAddr  string // listen address for HTTP mode
	FreeCAD   domain.Settings
}

package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sairaph/freecad-mcp/internal/guide"
)

// registerPrompts registers asset_creation_strategy, which returns the
// freecad-mcp-guide skill: its home text followed by every guide file, so the
// prompt stands alone where no skill folder was written.
func (s *Server) registerPrompts() {
	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "asset_creation_strategy",
		Description: "The recommended workflow for building models in FreeCAD with these tools.",
	}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: guide.Full()},
			}},
		}, nil
	})
}

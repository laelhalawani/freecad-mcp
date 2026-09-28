package mcpserver

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/mcp-wizard/render"
)

// imageBudget returns the largest base64-decoded image res can still carry
// without taking the reply past render.MaxBytes, given the text content
// already in res. Shared by every caller that appends an image to a result
// that may already have text, so the budget and the size actually checked
// against never drift apart.
func imageBudget(res *mcp.CallToolResult) int {
	text := 0
	for _, c := range res.Content {
		if tc, isText := c.(*mcp.TextContent); isText {
			text += len(tc.Text)
		}
	}
	return (render.MaxBytes - replyMargin - text) / 4 * 3
}

// screenshot attaches a screenshot of docName's 3D view (the active view when
// docName is "") from view to res unless screenshots are off. A failed
// capture is logged and left out, as it is optional feedback; get_view
// reports the failure as an error. So is a screenshot that would take the
// reply past render.MaxBytes.
func (s *Server) screenshot(ctx context.Context, conn *freecad.Connection, res *mcp.CallToolResult, include *bool, view, docName string) *mcp.CallToolResult {
	if s.config.FreeCAD.OnlyTextFeedback || (include != nil && !*include) {
		return res
	}
	shot, err := conn.GetActiveScreenshot(ctx, viewOrDefault(view), nil, nil, nil, docName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freecad-mcp: screenshot failed: %v\n", err)
		return res
	}
	img, ok := imageContent(shot.Image)
	if !ok {
		if shot.Message != "" {
			fmt.Fprintf(os.Stderr, "freecad-mcp: no screenshot: %s\n", shot.Message)
		}
		return res
	}
	if size := len(img.(*mcp.ImageContent).Data); size > imageBudget(res) {
		fmt.Fprintf(os.Stderr, "freecad-mcp: screenshot of %d KiB left out: the reply would exceed 1 MiB\n", size>>10)
		return res
	}
	res.Content = append(res.Content, img)
	return res
}

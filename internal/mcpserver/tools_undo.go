package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

// maxUndoSteps bounds the transactions one undo or redo call walks.
const maxUndoSteps = 100

type undoInput struct {
	DocName string `json:"doc_name"`
	Steps   *int   `json:"steps,omitempty"`
	screenshotOptions
}

func (s *Server) registerUndoTools() {
	stepped := func() *jsonschema.Schema {
		return withRange(inputSchema[undoInput](mergeDefaults(screenshotDefaults, map[string]string{"steps": "1"})),
			1, maxUndoSteps, "steps")
	}
	addTool(s.mcpServer, "undo", stepped(), s.undo)
	addTool(s.mcpServer, "redo", stepped(), s.redo)
}

// undoFront is the front matter shared by undo and redo; only the field for
// the operation that ran is set (the other stays nil and omitted), as a
// pointer so a genuine 0 (nothing was undone or redone) still renders.
type undoFront struct {
	Document  string `yaml:"document"`
	Undone    *int   `yaml:"undone,omitempty"`
	Redone    *int   `yaml:"redone,omitempty"`
	UndoCount int    `yaml:"undo_count"`
	RedoCount int    `yaml:"redo_count"`
}

func (s *Server) undo(ctx context.Context, _ *mcp.CallToolRequest, in undoInput) (*mcp.CallToolResult, any, error) {
	return s.undoOrRedo(ctx, in, "undo")
}

func (s *Server) redo(ctx context.Context, _ *mcp.CallToolRequest, in undoInput) (*mcp.CallToolResult, any, error) {
	return s.undoOrRedo(ctx, in, "redo")
}

// undoOrRedo is the shared body of undo and redo; which is "undo" or "redo"
// and names the MCP tool, the addon method and the reply key ("undone" or
// "redone") alike.
func (s *Server) undoOrRedo(ctx context.Context, in undoInput, which string) (*mcp.CallToolResult, any, error) {
	steps := 1
	if in.Steps != nil {
		steps = *in.Steps
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, which, err, ""), nil, nil
	}
	var res map[string]any
	if which == "undo" {
		res, err = conn.Undo(ctx, in.DocName, steps)
	} else {
		res, err = conn.Redo(ctx, in.DocName, steps)
	}
	if err != nil {
		return s.withNotice(failure(ctx, which, err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode(which, res, "")), nil, nil
	}

	walkedKey := "undone"
	if which == "redo" {
		walkedKey = "redone"
	}
	walked := stringItems(res[walkedKey])
	undoNames := stringItems(res["undo_names"])
	redoNames := stringItems(res["redo_names"])

	front := undoFront{Document: in.DocName, UndoCount: len(undoNames), RedoCount: len(redoNames)}
	walkedCount := len(walked)
	if which == "undo" {
		front.Undone = &walkedCount
	} else {
		front.Redone = &walkedCount
	}

	var body strings.Builder
	if len(walked) == 0 {
		fmt.Fprintf(&body, "Nothing to %s in '%s'.", which, in.DocName)
	} else {
		verb := "Undone"
		if which == "redo" {
			verb = "Redone"
		}
		fmt.Fprintf(&body, "%s %d transaction(s) in '%s': %s.", verb, len(walked), in.DocName, strings.Join(walked, ", "))
	}
	fmt.Fprintf(&body, "\n%s; %s.", remainingClause("undo", undoNames), remainingClause("redo", redoNames))
	body.WriteString(invalidObjectsBody(res, in.DocName))

	out := render.SuccessResult(front, body.String())
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), in.DocName)), nil, nil
}

// remainingClause names the transactions left to undo or redo, up to a
// handful, matching what the tool descriptions promise ("those left to undo
// or redo"): the addon reports the full FreeCAD.getDocument(...).UndoNames /
// RedoNames stack, not just its length.
func remainingClause(which string, names []string) string {
	const shown = 8
	if len(names) == 0 {
		return fmt.Sprintf("0 more to %s", which)
	}
	if len(names) <= shown {
		return fmt.Sprintf("%d more to %s: %s", len(names), which, strings.Join(names, ", "))
	}
	return fmt.Sprintf("%d more to %s: %s, and %d more", len(names), which,
		strings.Join(names[:shown], ", "), len(names)-shown)
}

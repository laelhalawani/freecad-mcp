package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/mcp-wizard/render"
)

type saveDocumentInput struct {
	DocName   string   `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	Recompute *bool    `json:"recompute,omitempty" jsonschema:"recompute the document before saving when it needs it (default true)"`
	Timeout   *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 120"`
}

type saveDocumentAsInput struct {
	DocName   string   `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	Path      string   `json:"path" jsonschema:"absolute path of the .FCStd file to write on the machine running FreeCAD; .FCStd is appended when the name has no extension"`
	Overwrite *bool    `json:"overwrite,omitempty" jsonschema:"replace an existing file at path (default false)"`
	Copy      *bool    `json:"copy,omitempty" jsonschema:"write a copy and keep the document on its current file and name (default false)"`
	Recompute *bool    `json:"recompute,omitempty" jsonschema:"recompute the document before saving when it needs it (default true)"`
	Timeout   *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 120"`
}

type closeDocumentInput struct {
	DocName        string `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	DiscardChanges *bool  `json:"discard_changes,omitempty" jsonschema:"close even when the document has unsaved changes, losing them (default false)"`
}

type saveDocumentFront struct {
	Document   string `yaml:"document"`
	File       string `yaml:"file"`
	Recomputed bool   `yaml:"recomputed"`
}

type saveDocumentAsFront struct {
	Document string `yaml:"document"`
	Label    string `yaml:"label"`
	File     string `yaml:"file"`
	Copy     bool   `yaml:"copy"`
}

type closeDocumentFront struct {
	Closed           string `yaml:"closed"`
	ActiveDocument   string `yaml:"active_document"`
	DiscardedChanges bool   `yaml:"discarded_changes"`
}

func (s *Server) registerDocumentSaveTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "save_document",
		Description: "Save an open FreeCAD document to its own .FCStd file, recomputing it first when it needs it, " +
			"without any dialog in FreeCAD. A document that was never saved has no file yet: use save_document_as " +
			"with a path instead. The reply names the file and lists objects that are invalid after the recompute; " +
			"list_documents shows which documents have unsaved changes.",
		InputSchema: withPositiveMax(inputSchema[saveDocumentInput](map[string]string{"recompute": "true", "timeout": "120"}),
			"timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.saveDocument)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "save_document_as",
		Description: "Save an open FreeCAD document to a new .FCStd file at an absolute path on the machine running " +
			"FreeCAD. The document then uses that file and its label becomes the file name; with copy true a copy is " +
			"written and the document keeps its file. An existing file is only replaced with overwrite true, and a " +
			"file another open document uses is refused. Use export_document for STEP, STL, 3MF and other formats. " +
			filePathsNote,
		InputSchema: withPositiveMax(inputSchema[saveDocumentAsInput](map[string]string{
			"overwrite": "false", "copy": "false", "recompute": "true", "timeout": "120"}),
			"timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.saveDocumentAs)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "close_document",
		Description: "Close an open FreeCAD document and its tabs. A document with unsaved changes is refused unless " +
			"discard_changes is true; call save_document or save_document_as first to keep them. The reply names the " +
			"document that is active afterwards. FreeCAD asks no questions, so nothing waits for a person.",
		InputSchema: inputSchema[closeDocumentInput](map[string]string{"discard_changes": "false"}),
	}, s.closeDocument)
}

func (s *Server) saveDocument(ctx context.Context, _ *mcp.CallToolRequest, in saveDocumentInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure(ctx, "save document", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "save document", err, ""), nil, nil
	}
	res, err := conn.SaveDocument(ctx, in.DocName, boolOr(in.Recompute, true), in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure(ctx, "save document", err, largerTimeout("save_document"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("save document", res,
			"Call list_documents with {} to see the open documents.")), nil, nil
	}

	doc := str(res, "document")
	file := str(res, "file_name")
	recomputed, _ := res["recomputed"].(bool)
	front := saveDocumentFront{Document: doc, File: file, Recomputed: recomputed}

	var body strings.Builder
	fmt.Fprintf(&body, "Document '%s' saved to '%s'.", doc, file)
	if recomputed {
		body.WriteString(" It was recomputed first.")
	}
	body.WriteString(invalidObjectsBody(res, doc))

	return s.withNotice(render.SuccessResult(front, body.String())), nil, nil
}

func (s *Server) saveDocumentAs(ctx context.Context, _ *mcp.CallToolRequest, in saveDocumentAsInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure(ctx, "save document as", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "save document as", err, ""), nil, nil
	}
	res, err := conn.SaveDocumentAs(ctx, in.DocName, in.Path,
		boolOr(in.Overwrite, false), boolOr(in.Copy, false), boolOr(in.Recompute, true), in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure(ctx, "save document as", err, largerTimeout("save_document_as"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("save document as", res, "")), nil, nil
	}

	doc := str(res, "document")
	label := str(res, "label")
	file := str(res, "file_name")
	copyMade, _ := res["copy"].(bool)
	recomputed, _ := res["recomputed"].(bool)
	front := saveDocumentAsFront{Document: doc, Label: label, File: file, Copy: copyMade}

	var body strings.Builder
	if copyMade {
		fmt.Fprintf(&body, "Wrote a copy of document '%s' to '%s'; the document keeps its own file and label.", doc, file)
	} else {
		fmt.Fprintf(&body, "Document '%s' saved to '%s' (label is now '%s').", doc, file, label)
	}
	if recomputed {
		body.WriteString(" It was recomputed first.")
	}
	body.WriteString(invalidObjectsBody(res, doc))

	return s.withNotice(render.SuccessResult(front, body.String())), nil, nil
}

func (s *Server) closeDocument(ctx context.Context, _ *mcp.CallToolRequest, in closeDocumentInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "close document", err, ""), nil, nil
	}
	res, err := conn.CloseDocument(ctx, in.DocName, boolOr(in.DiscardChanges, false))
	if err != nil {
		return s.withNotice(failure(ctx, "close document", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("close document", res,
			"Call list_documents with {} to see the open documents.")), nil, nil
	}

	closed := str(res, "closed")
	active := str(res, "active_document")
	discarded, _ := res["discarded_changes"].(bool)
	front := closeDocumentFront{Closed: closed, ActiveDocument: active, DiscardedChanges: discarded}

	body := fmt.Sprintf("Document '%s' closed.", closed)
	if discarded {
		body += " Its unsaved changes were discarded."
	}
	if active != "" {
		body += fmt.Sprintf(" Document '%s' is now active.", active)
	} else {
		body += " No document is active now."
	}

	return s.withNotice(render.SuccessResult(front, body)), nil, nil
}

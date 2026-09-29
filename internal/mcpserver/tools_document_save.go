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
	DocName   string   `json:"doc_name"`
	Recompute *bool    `json:"recompute,omitempty"`
	Timeout   *float64 `json:"timeout,omitempty"`
}

type saveDocumentAsInput struct {
	DocName   string   `json:"doc_name"`
	Path      string   `json:"path"`
	Overwrite *bool    `json:"overwrite,omitempty"`
	Copy      *bool    `json:"copy,omitempty"`
	Recompute *bool    `json:"recompute,omitempty"`
	Timeout   *float64 `json:"timeout,omitempty"`
}

type closeDocumentInput struct {
	DocName        string `json:"doc_name"`
	DiscardChanges *bool  `json:"discard_changes,omitempty"`
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
	addTool(s.mcpServer, "save_document",
		withPositiveMax(inputSchema[saveDocumentInput](map[string]string{"recompute": "true", "timeout": "120"}),
			"timeout", freecad.DefaultMaxExecuteCodeTime), s.saveDocument)
	addTool(s.mcpServer, "save_document_as",
		withPositiveMax(inputSchema[saveDocumentAsInput](map[string]string{
			"overwrite": "false", "copy": "false", "recompute": "true", "timeout": "120"}),
			"timeout", freecad.DefaultMaxExecuteCodeTime), s.saveDocumentAs)
	addTool(s.mcpServer, "close_document",
		inputSchema[closeDocumentInput](map[string]string{"discard_changes": "false"}), s.closeDocument)
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
	body.WriteString(createdFolderNote(res))
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

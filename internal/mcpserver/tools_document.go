package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/mcp-wizard/render"
)

type createDocumentInput struct {
	Name string `json:"name" jsonschema:"the name of the document to create"`
}

type documentFront struct {
	Document    string `yaml:"document"`
	Transaction string `yaml:"transaction,omitempty"`
}

type docNameInput struct {
	DocName string `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
}

type documentsFront struct {
	Count          int    `yaml:"count"`
	ActiveDocument string `yaml:"active_document"`
}

type openDocumentInput struct {
	Path     string   `json:"path" jsonschema:"absolute path of the .FCStd file on the machine running FreeCAD"`
	Hidden   *bool    `json:"hidden,omitempty" jsonschema:"open without a 3D view tab (default false); activate_document with create_view true adds one later"`
	Activate *bool    `json:"activate,omitempty" jsonschema:"make it the active document (default true)"`
	Timeout  *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 120; raise it for large assemblies"`
}

type activateDocumentInput struct {
	DocName    string `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	ViewIndex  *int   `json:"view_index,omitempty" jsonschema:"which of the document's views (tabs) to activate, as list_documents numbers them (default: its active view)"`
	CreateView *bool  `json:"create_view,omitempty" jsonschema:"open a 3D view when the document has none, for example after opening it hidden (default false)"`
}

type openDocumentFront struct {
	Document       string `yaml:"document"`
	Label          string `yaml:"label"`
	File           string `yaml:"file"`
	AlreadyOpen    bool   `yaml:"already_open"`
	ObjectCount    int    `yaml:"object_count"`
	NeedsRecompute bool   `yaml:"needs_recompute"`
}

type activateDocumentFront struct {
	Document  string `yaml:"document"`
	ViewIndex int    `yaml:"view_index"`
	ViewType  string `yaml:"view_type"`
}

func (s *Server) registerDocumentTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "create_document",
		Description: "Create a new, empty document in FreeCAD and make it the active document. " +
			"Use it before create_object when no document exists yet; list_documents shows the open ones. " +
			"The reply carries the name FreeCAD gave the document, which later calls take as doc_name. " +
			"Example: {\"name\": \"MyDocument\"}.",
		InputSchema: inputSchema[createDocumentInput](nil),
	}, s.createDocument)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "list_documents",
		Description: "List every document open in FreeCAD with its label, file, whether it has unsaved changes " +
			"or needs a recompute, whether it is the active document, and its views (tabs) with their index. Use " +
			"it first to find the doc_name the other tools take and the view_index activate_document takes; call " +
			"create_document or open_document when the list is empty.",
		InputSchema: inputSchema[struct{}](nil),
	}, s.listDocuments)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "reload_document",
		Description: "Close and re-open a document to pick up changes made to its .FCStd file outside the " +
			"FreeCAD GUI, for example by a script run with execute_code_headless that edited and saved it. " +
			"The open GUI document is otherwise unaware of on-disk changes; this closes the stale in-memory " +
			"copy and reopens the file so the GUI shows current geometry. Fails when the document is not open " +
			"or was never saved to a file. Example: {\"doc_name\": \"chassis\"}.",
		InputSchema: inputSchema[docNameInput](nil),
	}, s.reloadDocument)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "open_document",
		Description: "Open a FreeCAD document (.FCStd file) from an absolute path on the machine running FreeCAD, " +
			"without any dialog, and make it the active document. A file that is already open is not reloaded; " +
			"the reply says so and gives the document name that later calls take as doc_name, its object count, " +
			"and whether it needs a recompute. Use import_file for STEP, STL, 3MF and other formats, and " +
			"reload_document to pick up changes made to an open file on disk. " + filePathsNote,
		InputSchema: withPositiveMax(inputSchema[openDocumentInput](map[string]string{"hidden": "false", "activate": "true", "timeout": "120"}),
			"timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.openDocument)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "activate_document",
		Description: "Make an open FreeCAD document the active one and bring its tab to the front, optionally a " +
			"specific view by the index list_documents shows. Tools without doc_name, such as execute_code and " +
			"insert_part_from_library, act on the active document. With create_view true a document opened hidden " +
			"gets a 3D view, which get_view needs for a screenshot.",
		InputSchema: withRange(inputSchema[activateDocumentInput](map[string]string{"create_view": "false"}), 0, 1000, "view_index"),
	}, s.activateDocument)
}

func (s *Server) openDocument(ctx context.Context, _ *mcp.CallToolRequest, in openDocumentInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure(ctx, "open document", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "open document", err, ""), nil, nil
	}
	res, err := conn.OpenDocument(ctx, in.Path, boolOr(in.Hidden, false), boolOr(in.Activate, true), in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure(ctx, "open document", err, largerTimeout("open_document"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("open document", res, "")), nil, nil
	}

	front := openDocumentFront{
		Document:       str(res, "document"),
		Label:          str(res, "label"),
		File:           str(res, "file_name"),
		AlreadyOpen:    boolField(res, "already_open"),
		ObjectCount:    intField(res, "object_count"),
		NeedsRecompute: boolField(res, "needs_recompute"),
	}

	var body strings.Builder
	if front.AlreadyOpen {
		fmt.Fprintf(&body, "Document '%s' was already open (%s); it was not reloaded. Call reload_document with {\"doc_name\": %q} to pick up on-disk changes.",
			front.Document, front.File, front.Document)
	} else {
		fmt.Fprintf(&body, "Opened '%s' from %s (%d object(s)).", front.Document, front.File, front.ObjectCount)
	}
	body.WriteString(invalidObjectsBody(res, front.Document))
	if front.NeedsRecompute {
		fmt.Fprintf(&body, "\n\nCall recompute_document with {\"doc_name\": %q} to bring it up to date.", front.Document)
	}
	if boolOr(in.Hidden, false) && boolOr(in.Activate, true) && str(res, "active_document") != front.Document {
		fmt.Fprintf(&body, "\n\n'%s' was opened hidden, so it has no view to activate; call activate_document "+
			"with {\"doc_name\": %q, \"create_view\": true} to give it one and make it active.",
			front.Document, front.Document)
	}

	return s.withNotice(render.SuccessResult(front, body.String())), nil, nil
}

func (s *Server) activateDocument(ctx context.Context, _ *mcp.CallToolRequest, in activateDocumentInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "activate document", err, ""), nil, nil
	}
	res, err := conn.ActivateDocument(ctx, in.DocName, in.ViewIndex, boolOr(in.CreateView, false))
	if err != nil {
		return s.withNotice(failure(ctx, "activate document", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("activate document", res, "")), nil, nil
	}

	front := activateDocumentFront{
		Document:  str(res, "document"),
		ViewIndex: intField(res, "view_index"),
		ViewType:  str(res, "view_type"),
	}
	body := fmt.Sprintf("Document '%s' is now active (view %d, %s).", front.Document, front.ViewIndex, front.ViewType)
	return s.withNotice(render.SuccessResult(front, body)), nil, nil
}

func (s *Server) createDocument(ctx context.Context, _ *mcp.CallToolRequest, in createDocumentInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "create document", err, ""), nil, nil
	}
	res, err := conn.CreateDocument(ctx, in.Name)
	if err != nil {
		return s.withNotice(failure(ctx, "create document", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reported("create document", res, "Call list_documents to see the open documents.")), nil, nil
	}
	name := str(res, "document_name")
	txName, txMerged := transactionFields(res)
	return s.withNotice(render.SuccessResult(documentFront{Document: name, Transaction: txName},
		transactionNote(fmt.Sprintf("Document '%s' created successfully.", name), txName, txMerged))), nil, nil
}

func (s *Server) listDocuments(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "list documents", err, ""), nil, nil
	}
	res, err := conn.GetDocuments(ctx)
	if err != nil {
		return s.withNotice(failure(ctx, "list documents", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("list documents", res, "")), nil, nil
	}

	docs, _ := res["documents"].([]any)
	front := documentsFront{Count: len(docs), ActiveDocument: str(res, "active_document")}

	var body strings.Builder
	if len(docs) == 0 {
		body.WriteString("No document is open. Call create_document with {\"name\": \"MyDocument\"} or " +
			"open_document with {\"path\": \"...\"}.")
	} else {
		body.WriteString("| Name | Label | File | Modified | Active | Views |\n")
		body.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		for _, item := range docs {
			d, ok := item.(map[string]any)
			if !ok {
				continue
			}
			fmt.Fprintf(&body, "| %s | %s | %s | %t | %t | %s |\n",
				str(d, "name"), str(d, "label"), str(d, "file_name"),
				boolField(d, "modified"), boolField(d, "active"), viewsSummary(d))
		}
		body.WriteString("\nCall list_objects or get_object with a doc_name to inspect one, activate_document " +
			"to switch the active document, or open_document / create_document to add another.")
	}

	return s.withNotice(render.SuccessResult(front, body.String())), nil, nil
}

// viewsSummary renders one DocInfo's views as a compact "index:type" list for
// the list_documents table, marking the active one with "*".
func viewsSummary(doc map[string]any) string {
	views, _ := doc["views"].([]any)
	if len(views) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(views))
	for _, item := range views {
		v, ok := item.(map[string]any)
		if !ok {
			continue
		}
		mark := ""
		if boolField(v, "active") {
			mark = "*"
		}
		parts = append(parts, fmt.Sprintf("%d:%s%s", intField(v, "index"), str(v, "type"), mark))
	}
	return strings.Join(parts, ", ")
}

func (s *Server) reloadDocument(ctx context.Context, _ *mcp.CallToolRequest, in docNameInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "reload document", err, ""), nil, nil
	}
	res, err := conn.ReloadDocument(ctx, in.DocName)
	if err != nil {
		return s.withNotice(failure(ctx, "reload document", err, "")), nil, nil
	}
	if !succeeded(res) {
		// _reload_document_gui's every failure path (not_found, conflict for
		// an unsaved document) goes through fail() with its own hint, which
		// always wins here regardless of hintCodes; unlike run_fem_analysis
		// and insert_part_from_library it has no bare, uncoded failure shape
		// of its own, only the generic dispatch-level one every tool shares,
		// for which the default codeFreeCAD/unavailable hints already fit at
		// least as well as this one, so no hintCodes are named here.
		return s.withNotice(reportedCode("reload document", res, "Call list_documents to see the open documents.")), nil, nil
	}
	name := str(res, "document_name")
	return s.withNotice(render.SuccessResult(documentFront{Document: name},
		fmt.Sprintf("Document '%s' reloaded from disk.", name))), nil, nil
}

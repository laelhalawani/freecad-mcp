package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

type createDocumentInput struct {
	Name string `json:"name" jsonschema:"the name of the document to create"`
}

type documentFront struct {
	Document string `yaml:"document"`
}

type docNameInput struct {
	DocName string `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
}

type documentsFront struct {
	Count int `yaml:"count"`
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
		Description: "List the names of the documents open in FreeCAD. Use it first to find the doc_name " +
			"the other tools take; call create_document when the list is empty.",
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
}

func (s *Server) createDocument(ctx context.Context, _ *mcp.CallToolRequest, in createDocumentInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("create document", err, ""), nil, nil
	}
	res, err := conn.CreateDocument(ctx, in.Name)
	if err != nil {
		return s.withNotice(failure("create document", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reported("create document", res, "Call list_documents to see the open documents.")), nil, nil
	}
	name := str(res, "document_name")
	return s.withNotice(render.SuccessResult(documentFront{Document: name},
		fmt.Sprintf("Document '%s' created successfully.", name))), nil, nil
}

func (s *Server) listDocuments(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("list documents", err, ""), nil, nil
	}
	docs, err := conn.ListDocuments(ctx)
	if err != nil {
		return s.withNotice(failure("list documents", err, "")), nil, nil
	}
	count := 0
	if list, ok := docs.([]any); ok {
		count = len(list)
	}
	body := jsonBlock(docs)
	if count == 0 {
		body += "\nNo document is open. Call create_document to start one."
	}
	return s.withNotice(render.SuccessResult(documentsFront{Count: count}, body)), nil, nil
}

func (s *Server) reloadDocument(ctx context.Context, _ *mcp.CallToolRequest, in docNameInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("reload document", err, ""), nil, nil
	}
	res, err := conn.ReloadDocument(ctx, in.DocName)
	if err != nil {
		return s.withNotice(failure("reload document", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reported("reload document", res, "Call list_documents to see the open documents.")), nil, nil
	}
	name := str(res, "document_name")
	return s.withNotice(render.SuccessResult(documentFront{Document: name},
		fmt.Sprintf("Document '%s' reloaded from disk.", name))), nil, nil
}

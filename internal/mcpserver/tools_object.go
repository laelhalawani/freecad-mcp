package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

// screenshotOptions is embedded by every tool that can attach a screenshot.
type screenshotOptions struct {
	IncludeScreenshot *bool     `json:"include_screenshot,omitempty"`
	ViewName          *ViewName `json:"view_name,omitempty"`
}

var screenshotDefaults = map[string]string{"include_screenshot": "true", "view_name": `"Isometric"`}

const propertiesHint = `Pass obj_properties as a JSON object of property names and values, e.g. {"Length": 20}.`

type createObjectInput struct {
	DocName       string      `json:"doc_name"`
	ObjType       string      `json:"obj_type"`
	ObjName       string      `json:"obj_name"`
	AnalysisName  *string     `json:"analysis_name,omitempty"`
	ObjProperties *Properties `json:"obj_properties,omitempty"`
	screenshotOptions
}

type updateObjectInput struct {
	DocName       string     `json:"doc_name"`
	ObjName       string     `json:"obj_name"`
	ObjProperties Properties `json:"obj_properties"`
	screenshotOptions
}

type objectInput struct {
	DocName string `json:"doc_name"`
	ObjName string `json:"obj_name"`
	screenshotOptions
}

type documentInput struct {
	DocName string `json:"doc_name"`
	Compact *bool  `json:"compact,omitempty"`
	screenshotOptions
}

// noScreenshotDefaults is for the read-only inspect tools, whose screenshot is
// off unless include_screenshot is passed as true.
var noScreenshotDefaults = map[string]string{"include_screenshot": "false", "view_name": `"Isometric"`}

// listObjectsDefaults adds list_objects' own compact default.
var listObjectsDefaults = mergeDefaults(noScreenshotDefaults, map[string]string{"compact": "false"})

// screenshotOptIn is include for a tool whose screenshot is off unless asked
// for: an absent argument means off.
func screenshotOptIn(include *bool) *bool {
	if include == nil {
		off := false
		return &off
	}
	return include
}

type objectFront struct {
	Document    string `yaml:"document"`
	Object      string `yaml:"object_name"`
	Type        string `yaml:"object_type,omitempty"`
	Transaction string `yaml:"transaction,omitempty"`
}

type objectsFront struct {
	Document string `yaml:"document"`
	Count    int    `yaml:"count"`
}

func (s *Server) registerObjectTools() {
	addTool(s.mcpServer, "create_object", inputSchema[createObjectInput](screenshotDefaults), s.createObject)
	addTool(s.mcpServer, "update_object", inputSchema[updateObjectInput](screenshotDefaults), s.updateObject)
	addTool(s.mcpServer, "delete_object", inputSchema[objectInput](screenshotDefaults), s.deleteObject)
	addTool(s.mcpServer, "list_objects", inputSchema[documentInput](listObjectsDefaults), s.listObjects)
	addTool(s.mcpServer, "get_object", inputSchema[objectInput](noScreenshotDefaults), s.getObject)
}

func (s *Server) createObject(ctx context.Context, req *mcp.CallToolRequest, in createObjectInput) (*mcp.CallToolResult, any, error) {
	props, err := rawProperties(req)
	if err != nil {
		return failure(ctx, "create object", &toolError{render.Error{Code: render.CodeInvalidInput, Message: err.Error(), Hint: propertiesHint}}, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "create object", err, ""), nil, nil
	}
	var analysis any
	if in.AnalysisName != nil {
		analysis = *in.AnalysisName
	}
	objData := map[string]any{
		"Name":       in.ObjName,
		"Type":       in.ObjType,
		"Properties": props,
		"Analysis":   analysis,
	}
	res, err := conn.CreateObject(ctx, in.DocName, objData)
	if err != nil {
		return s.withNotice(failure(ctx, "create object", err, "")), nil, nil
	}
	if !succeeded(res) {
		if name := str(res, "object_name"); name != "" {
			// The object was created but did not compute; it stays in the
			// document under this name.
			return s.withNotice(render.ErrorResult(render.Error{
				Code:    codeFreeCAD,
				Message: shortMessage(fmt.Sprintf("Object '%s' was created in '%s' but is not valid: %s", name, in.DocName, errorText(res))),
				Hint:    fixOrRemoveHint(in.DocName, name),
				Fields:  map[string]any{"object_name": name},
			})), nil, nil
		}
		return s.withNotice(reported("create object", res,
			fmt.Sprintf("Check obj_type and the property names; call list_objects with {\"doc_name\": %q} to see the document.", in.DocName))), nil, nil
	}
	name := str(res, "object_name")
	txName, txMerged := transactionFields(res)
	out := render.SuccessResult(objectFront{Document: in.DocName, Object: name, Type: in.ObjType, Transaction: txName},
		loadNote(quantityNote(transactionNote(fmt.Sprintf("Object '%s' created successfully.", name), txName, txMerged), res), res))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), in.DocName)), nil, nil
}

func (s *Server) updateObject(ctx context.Context, req *mcp.CallToolRequest, in updateObjectInput) (*mcp.CallToolResult, any, error) {
	props, err := rawProperties(req)
	if err != nil {
		return failure(ctx, "update object", &toolError{render.Error{Code: render.CodeInvalidInput, Message: err.Error(), Hint: propertiesHint}}, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "update object", err, ""), nil, nil
	}
	res, err := conn.EditObject(ctx, in.DocName, in.ObjName, map[string]any{"Properties": props})
	if err != nil {
		return s.withNotice(failure(ctx, "update object", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reported("update object", res,
			fmt.Sprintf("Call get_object with {\"doc_name\": %q, \"obj_name\": %q} to see its properties.", in.DocName, in.ObjName))), nil, nil
	}
	name := str(res, "object_name")
	txName, txMerged := transactionFields(res)
	out := render.SuccessResult(objectFront{Document: in.DocName, Object: name, Transaction: txName},
		loadNote(quantityNote(transactionNote(fmt.Sprintf("Object '%s' updated successfully.", name), txName, txMerged), res), res))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), in.DocName)), nil, nil
}

func (s *Server) deleteObject(ctx context.Context, _ *mcp.CallToolRequest, in objectInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "delete object", err, ""), nil, nil
	}
	res, err := conn.DeleteObject(ctx, in.DocName, in.ObjName)
	if err != nil {
		return s.withNotice(failure(ctx, "delete object", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reported("delete object", res,
			fmt.Sprintf("Call list_objects with {\"doc_name\": %q} to see the object names.", in.DocName))), nil, nil
	}
	name := str(res, "object_name")
	txName, txMerged := transactionFields(res)
	out := render.SuccessResult(objectFront{Document: in.DocName, Object: name, Transaction: txName},
		transactionNote(fmt.Sprintf("Object '%s' deleted successfully.", name), txName, txMerged))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), in.DocName)), nil, nil
}

func (s *Server) listObjects(ctx context.Context, _ *mcp.CallToolRequest, in documentInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "list objects", err, ""), nil, nil
	}
	compact := boolOr(in.Compact, false)
	objects, err := conn.GetObjects(ctx, in.DocName, compact)
	if err != nil {
		return s.withNotice(failure(ctx, "list objects", err, "")), nil, nil
	}
	count := 0
	if list, ok := objects.([]any); ok {
		count = len(list)
	}
	body := jsonBlock(objects)
	if count == 0 {
		body += "\nThe document is empty or not open. Call list_documents to see the open documents."
	}
	out := render.SuccessResult(objectsFront{Document: in.DocName, Count: count}, body)
	return s.withNotice(s.screenshot(ctx, conn, out, screenshotOptIn(in.IncludeScreenshot), viewString(in.ViewName), in.DocName)), nil, nil
}

func (s *Server) getObject(ctx context.Context, _ *mcp.CallToolRequest, in objectInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "get object", err, ""), nil, nil
	}
	object, err := conn.GetObject(ctx, in.DocName, in.ObjName)
	if err != nil {
		return s.withNotice(failure(ctx, "get object", err, "")), nil, nil
	}
	if object == nil {
		return s.withNotice(render.ErrorResult(render.Error{
			Code:    render.CodeNotFound,
			Message: fmt.Sprintf("object %q not found in document %q", in.ObjName, in.DocName),
			Hint:    fmt.Sprintf("Call list_objects with {\"doc_name\": %q} to see the object names, or list_documents for the documents.", in.DocName),
		})), nil, nil
	}
	out := render.SuccessResult(objectFront{Document: in.DocName, Object: in.ObjName}, jsonBlock(object))
	return s.withNotice(s.screenshot(ctx, conn, out, screenshotOptIn(in.IncludeScreenshot), viewString(in.ViewName), in.DocName)), nil, nil
}

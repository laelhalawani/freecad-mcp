package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
	"github.com/sairaph/mcp-wizard/render"
)

type getViewInput struct {
	DocName     *string   `json:"doc_name,omitempty" jsonschema:"the document whose 3D view to capture (default: the active document's active view)"`
	ViewName    *ViewName `json:"view_name,omitempty" jsonschema:"the view orientation of the screenshot (default Isometric)"`
	Width       *int      `json:"width,omitempty" jsonschema:"the width of the screenshot in pixels, 1 to 2048 (default: see the tool description)"`
	Height      *int      `json:"height,omitempty" jsonschema:"the height of the screenshot in pixels, 1 to 2048 (default: see the tool description)"`
	FocusObject *string   `json:"focus_object,omitempty" jsonschema:"the name of an object to focus on (default: fit all objects in the view)"`
}

type partInput struct {
	RelativePath string `json:"relative_path" jsonschema:"the path of the part inside the parts library, as list_parts shows it"`
	screenshotOptions
}

type getViewFront struct {
	Document string `yaml:"document,omitempty"`
	View     string `yaml:"view"`
}

type partFront struct {
	Part        string `yaml:"part"`
	Transaction string `yaml:"transaction,omitempty"`
}

type partsFront struct {
	Count int `yaml:"count"`
}

func (s *Server) registerViewTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "get_view",
		Description: "Get a screenshot of a FreeCAD document's 3D view from the given orientation (default Isometric). " +
			"With doc_name, that document's 3D view is captured, switching to it and back if it is not already " +
			"the active window; without it, the active document's active view is used. Either way FreeCAD's " +
			"camera and selection are left exactly as found afterwards. Use it to inspect the model after changes " +
			"made with include_screenshot false, choosing the most informative angle, or with focus_object to " +
			"frame one object from list_objects. When width and height are both omitted, the image has the " +
			"viewport's size, scaled down to keep its aspect ratio when the longest edge exceeds 1024 pixels; " +
			"when only one is given, the other is the viewport's size in that direction. width and height go up " +
			"to 2048, and an image too large for one reply (about 740 KiB of PNG) is refused with a hint to ask " +
			"for a smaller one. Fails when no document is open, doc_name is not an open document, that document " +
			"has no 3D view (opened hidden, or all its 3D views closed), or (without doc_name) the active window " +
			"is not a 3D view, such as a TechDraw page or a spreadsheet.",
		InputSchema: withRange(inputSchema[getViewInput](map[string]string{"view_name": `"Isometric"`}),
			1, maxViewSize, "width", "height"),
	}, s.getView)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "insert_part_from_library",
		Description: "Insert a part from the FreeCAD parts library addon into the active document. Call " +
			"list_parts first to find the part's relative path, then position it with update_object and check " +
			"it with get_object.",
		InputSchema: inputSchema[partInput](screenshotDefaults),
	}, s.insertPartFromLibrary)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "list_parts",
		Description: "List the parts available in the FreeCAD parts library addon, as relative paths for " +
			"insert_part_from_library. The list is empty when the parts_library addon is not installed in FreeCAD; " +
			"then build the shape with create_object instead.",
		InputSchema: inputSchema[struct{}](nil),
	}, s.listParts)
}

func (s *Server) getView(ctx context.Context, _ *mcp.CallToolRequest, in getViewInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("get view", err, ""), nil, nil
	}
	docName := ""
	if in.DocName != nil {
		docName = *in.DocName
	}
	shot, err := conn.GetActiveScreenshot(ctx, viewOrDefault(viewString(in.ViewName)), in.Width, in.Height, in.FocusObject, docName)
	if err != nil {
		// A fault is FreeCAD failing to capture the view.
		hint := ""
		var fault *xmlrpc.Fault
		if errors.As(err, &fault) && !fault.MissingMethod() {
			hint = "Make sure a 3D view of a document is active in FreeCAD, then call get_view again; " +
				"call get_rpc_status if FreeCAD seems stuck."
		}
		return s.withNotice(failure("get view", err, hint)), nil, nil
	}
	img, ok := imageContent(shot.Image)
	if !ok && shot.Reason != "" {
		// The addon says why there is nothing to capture.
		return s.withNotice(reportedCode("get view", map[string]any{
			"code": shot.Code, "error": shot.Message, "hint": shot.Hint,
		}, "")), nil, nil
	}
	if !ok {
		// An addon before protocol 3 answers the same way when no document is
		// open and when the active view is not a 3D view; the open documents
		// tell them apart.
		if docs, err := conn.ListDocuments(ctx); err == nil {
			if list, isList := docs.([]any); isList && len(list) == 0 {
				return s.withNotice(render.ErrorResult(render.Error{
					Code:    render.CodeUnavailable,
					Message: "No document is open in FreeCAD, so there is no 3D view to capture.",
					Hint:    "Call create_document, or open a document in FreeCAD, then call get_view again.",
				})), nil, nil
			}
		}
		return s.withNotice(render.ErrorResult(render.Error{
			Code: render.CodeUnavailable,
			Message: "FreeCAD's active window is not a 3D view that can be captured " +
				"(for example a TechDraw page or a spreadsheet is active, or no document window is).",
			Hint: "Switch FreeCAD to a 3D view of a document, then call get_view again; list_documents shows the open documents.",
		})), nil, nil
	}
	viewName := viewOrDefault(viewString(in.ViewName))
	body := "Screenshot of the " + viewName + " view"
	if shot.Document != "" {
		body += " of " + shot.Document
	}
	body += "."
	out := render.SuccessResult(getViewFront{Document: shot.Document, View: viewName}, body)
	budget := imageBudget(out)
	if size := len(img.(*mcp.ImageContent).Data); size > budget {
		return s.withNotice(render.ErrorResult(render.Error{
			Code: render.CodeInvalidInput,
			Message: fmt.Sprintf("The screenshot is %d KiB, more than the %d KiB a reply can carry.",
				size>>10, budget>>10),
			Hint: "Call get_view again with a smaller width and height, e.g. {\"width\": 1024, \"height\": 768}, " +
				"or omit both for a viewport-sized image of at most 1024 pixels on its longest edge.",
		})), nil, nil
	}
	out.Content = append(out.Content, img)
	return s.withNotice(out), nil, nil
}

func (s *Server) insertPartFromLibrary(ctx context.Context, _ *mcp.CallToolRequest, in partInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("insert part from library", err, ""), nil, nil
	}
	res, err := conn.InsertPartFromLibrary(ctx, in.RelativePath)
	if err != nil {
		return s.withNotice(failure("insert part from library", err, "")), nil, nil
	}
	if !succeeded(res) {
		// _insert_part_from_library's own except FileNotFoundError/ValueError
		// branches carry a code and hint, which win here regardless of
		// hintCodes, and this hint fits that not_found case (the default).
		// Its bare `except Exception as e: return str(e)` carries no code
		// either, landing on codeFreeCAD, but that is an unexpected failure
		// during the insert itself, not a bad path, so listing the available
		// paths does not help there: codeFreeCAD is deliberately not named,
		// leaving that case to the generic default hint.
		return s.withNotice(reportedCode("insert part from library", res,
			"Call list_parts to see the available paths.")), nil, nil
	}
	txName, txMerged := transactionFields(res)
	out := render.SuccessResult(partFront{Part: in.RelativePath, Transaction: txName},
		transactionNote("Part inserted from library: "+str(res, "message"), txName, txMerged))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), "")), nil, nil
}

func (s *Server) listParts(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("list parts", err, ""), nil, nil
	}
	parts, err := conn.GetPartsList(ctx)
	if err != nil {
		return s.withNotice(failure("list parts", err, "")), nil, nil
	}
	list, _ := parts.([]any)
	if len(list) == 0 {
		return s.withNotice(render.SuccessResult(partsFront{Count: 0},
			"No parts found in the parts library. Install the parts_library addon in FreeCAD's Addon Manager to use it.")), nil, nil
	}
	return s.withNotice(render.SuccessResult(partsFront{Count: len(list)}, jsonBlock(list))), nil, nil
}

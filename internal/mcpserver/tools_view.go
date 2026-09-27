package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

type getViewInput struct {
	ViewName    ViewName `json:"view_name" jsonschema:"the view orientation of the screenshot"`
	Width       *int     `json:"width,omitempty" jsonschema:"the width of the screenshot in pixels (default: the viewport width)"`
	Height      *int     `json:"height,omitempty" jsonschema:"the height of the screenshot in pixels (default: the viewport height)"`
	FocusObject *string  `json:"focus_object,omitempty" jsonschema:"the name of an object to focus on (default: fit all objects in the view)"`
}

type partInput struct {
	RelativePath string `json:"relative_path" jsonschema:"the path of the part inside the parts library, as get_parts_list shows it"`
	screenshotOptions
}

type partFront struct {
	Part string `yaml:"part"`
}

type partsFront struct {
	Count int `yaml:"count"`
}

func (s *Server) registerViewTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "get_view",
		Description: "Get a screenshot of FreeCAD's active 3D view from the given orientation. Use it to inspect " +
			"the model after changes made with include_screenshot false, choosing the most informative angle. " +
			"Fails when the active view cannot be captured, such as a TechDraw page or a spreadsheet.",
		InputSchema: inputSchema[getViewInput](nil),
	}, s.getView)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "insert_part_from_library",
		Description: "Insert a part from the FreeCAD parts library addon into the active document. Call " +
			"get_parts_list first to find the part's relative path, then position it with edit_object.",
		InputSchema: inputSchema[partInput](screenshotDefaults),
	}, s.insertPartFromLibrary)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "get_parts_list",
		Description: "List the parts available in the FreeCAD parts library addon, as relative paths for " +
			"insert_part_from_library. The list is empty when the parts_library addon is not installed in FreeCAD.",
		InputSchema: inputSchema[struct{}](nil),
	}, s.getPartsList)
}

func (s *Server) getView(ctx context.Context, _ *mcp.CallToolRequest, in getViewInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("get view", err, ""), nil, nil
	}
	b64, err := conn.GetActiveScreenshot(ctx, viewOrDefault(string(in.ViewName)), in.Width, in.Height, in.FocusObject)
	if err != nil {
		return s.withNotice(failure("get view", err, "")), nil, nil
	}
	img, ok := imageContent(b64)
	if !ok {
		return s.withNotice(render.ErrorResult(render.Error{
			Code:    render.CodeUnavailable,
			Message: "Cannot get a screenshot in the current view type (such as TechDraw or Spreadsheet).",
			Hint:    "Switch FreeCAD to a 3D view of the document, then call get_view again.",
		})), nil, nil
	}
	return s.withNotice(&mcp.CallToolResult{Content: []mcp.Content{img}}), nil, nil
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
		return s.withNotice(reported("insert part from library", res, "Call get_parts_list to see the available paths.")), nil, nil
	}
	out := render.SuccessResult(partFront{Part: in.RelativePath}, "Part inserted from library: "+str(res, "message"))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, string(in.ViewName))), nil, nil
}

func (s *Server) getPartsList(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("get parts list", err, ""), nil, nil
	}
	parts, err := conn.GetPartsList(ctx)
	if err != nil {
		return s.withNotice(failure("get parts list", err, "")), nil, nil
	}
	list, _ := parts.([]any)
	if len(list) == 0 {
		return s.withNotice(render.SuccessResult(partsFront{Count: 0},
			"No parts found in the parts library. Install the parts_library addon in FreeCAD's Addon Manager to use it.")), nil, nil
	}
	return s.withNotice(render.SuccessResult(partsFront{Count: len(list)}, jsonBlock(list))), nil, nil
}

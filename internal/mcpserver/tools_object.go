package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

// screenshotOptions is embedded by every tool that can attach a screenshot.
type screenshotOptions struct {
	IncludeScreenshot *bool     `json:"include_screenshot,omitempty" jsonschema:"whether to return a screenshot of the model (default true); set false to save tokens when visual feedback is not needed, e.g. for intermediate steps in a longer sequence of changes"`
	ViewName          *ViewName `json:"view_name,omitempty" jsonschema:"the view orientation of the returned screenshot (default Isometric); pick the view that best shows the change"`
}

var screenshotDefaults = map[string]string{"include_screenshot": "true", "view_name": `"Isometric"`}

const propertiesHint = `Pass obj_properties as a JSON object of property names and values, e.g. {"Length": 20}.`

type createObjectInput struct {
	DocName       string      `json:"doc_name" jsonschema:"the name of the document to create the object in"`
	ObjType       string      `json:"obj_type" jsonschema:"the FreeCAD type of the object, e.g. Part::Box, Part::Cylinder, Part::Cut, PartDesign::Body, Fem::ConstraintFixed"`
	ObjName       string      `json:"obj_name" jsonschema:"the name of the object to create"`
	AnalysisName  *string     `json:"analysis_name,omitempty" jsonschema:"for FEM objects: the name of the FEM analysis to add the object to"`
	ObjProperties *Properties `json:"obj_properties,omitempty" jsonschema:"the properties of the object, e.g. {\"Height\": 30, \"Radius\": 10}; placement as {\"Placement\": {\"Base\": {\"x\": 0, \"y\": 0, \"z\": 0}, \"Rotation\": {\"Axis\": {\"x\": 0, \"y\": 0, \"z\": 1}, \"Angle\": 45}}} with Angle in degrees; view colors as {\"ViewObject\": {\"ShapeColor\": [0.5, 0.5, 0.5, 1.0]}}"`
	screenshotOptions
}

type updateObjectInput struct {
	DocName       string     `json:"doc_name" jsonschema:"the name of the document holding the object"`
	ObjName       string     `json:"obj_name" jsonschema:"the name of the object to update"`
	ObjProperties Properties `json:"obj_properties" jsonschema:"the properties to set, in the same form create_object takes"`
	screenshotOptions
}

type objectInput struct {
	DocName string `json:"doc_name" jsonschema:"the name of the document holding the object"`
	ObjName string `json:"obj_name" jsonschema:"the name of the object"`
	screenshotOptions
}

type documentInput struct {
	DocName string `json:"doc_name" jsonschema:"the name of the document"`
	screenshotOptions
}

type objectFront struct {
	Document string `yaml:"document"`
	Object   string `yaml:"object_name"`
	Type     string `yaml:"object_type,omitempty"`
}

type objectsFront struct {
	Document string `yaml:"document"`
	Count    int    `yaml:"count"`
}

const createObjectDescription = `Create a new object in a FreeCAD document.

obj_type must name a type registered in FreeCAD's C++ type system, such as "Part::", "PartDesign::" or "Fem::" types. A number of FreeCAD features are implemented in Python rather than C++ and so are not registered types; of those, these are supported here through dedicated factories, and each requires the properties listed beside it:

    Part::Tube        InnerRadius, OuterRadius, Height
    Draft::Circle     Radius
    Draft::Rectangle  Length, Height
    Draft::Polygon    FacesNumber, Radius
    Draft::Wire       Points (list of {x, y, z}), optional Closed

Any other Python-implemented type (Draft::Point, Draft::Ellipse, ...) must be built with execute_code instead.

The Draft factories name objects themselves, so for those the returned object_name differs from the requested obj_name, which is applied to the object's Label instead. Always use the returned name in later get_object and update_object calls.

A Placement's Rotation Angle is in degrees, the unit get_object and list_objects report it in.

Examples:
- A cylinder: {"doc_name": "MyDoc", "obj_name": "Cylinder", "obj_type": "Part::Cylinder", "obj_properties": {"Height": 30, "Radius": 10, "Placement": {"Base": {"x": 10, "y": 10, "z": 0}, "Rotation": {"Axis": {"x": 0, "y": 0, "z": 1}, "Angle": 45}}, "ViewObject": {"ShapeColor": [0.5, 0.5, 0.5, 1.0]}}}
- A cut: create both solids first, then reference them by name: {"doc_name": "MyPart", "obj_name": "Cut", "obj_type": "Part::Cut", "obj_properties": {"Base": "Box", "Tool": "Cylinder"}}
- A pipe with outer diameter 250, a 10 wall and length 500: {"doc_name": "MyPipe", "obj_name": "Pipe", "obj_type": "Part::Tube", "obj_properties": {"OuterRadius": 125, "InnerRadius": 115, "Height": 500}}
- A FEM analysis: {"doc_name": "MyFEM", "obj_name": "FemAnalysis", "obj_type": "Fem::AnalysisPython"}
- A FEM constraint: {"doc_name": "MyFEM", "obj_name": "FemConstraint", "obj_type": "Fem::ConstraintFixed", "analysis_name": "FemAnalysis", "obj_properties": {"References": [{"object_name": "MyObject", "face": "Face1"}]}}
- A FEM material: {"doc_name": "MyFEM", "obj_name": "FemMechanicalMaterial", "obj_type": "Fem::MaterialCommon", "analysis_name": "FemAnalysis", "obj_properties": {"Material": {"Name": "MyMaterial", "Density": "7900 kg/m^3", "YoungsModulus": "210 GPa", "PoissonRatio": 0.3}}}
- A FEM mesh (Shape is required; legacy Part is also accepted; on FreeCAD 1.x the size limits are CharacteristicLengthMax/Min, and the legacy ElementSizeMax/Min are also accepted): {"doc_name": "MyFEM", "obj_name": "FemMesh", "obj_type": "Fem::FemMeshGmsh", "analysis_name": "FemAnalysis", "obj_properties": {"Shape": "MyObject", "CharacteristicLengthMax": 10, "CharacteristicLengthMin": 0.1}}

The reply names the created object and, unless include_screenshot is false, shows a screenshot. Run run_fem_analysis once an analysis has its geometry, material, mesh and constraints.`

func (s *Server) registerObjectTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "create_object",
		Description: createObjectDescription,
		InputSchema: inputSchema[createObjectInput](screenshotDefaults),
	}, s.createObject)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "update_object",
		Description: "Set properties of an existing object in a FreeCAD document, such as dimensions, " +
			"Placement or ViewObject colors, in the same form create_object takes. Use it when create_object " +
			"cannot set a property at creation time, and verify the result with get_object; list_objects shows " +
			"the object names. Example: {\"doc_name\": \"MyDoc\", \"obj_name\": \"Box\", \"obj_properties\": {\"Length\": 20}}.",
		InputSchema: inputSchema[updateObjectInput](screenshotDefaults),
	}, s.updateObject)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "delete_object",
		Description: "Delete an object from a FreeCAD document. Objects that depend on it (for example a " +
			"Part::Cut using it as Base or Tool) may become invalid; call list_objects afterwards to check the document.",
		InputSchema: inputSchema[objectInput](screenshotDefaults),
	}, s.deleteObject)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "list_objects",
		Description: "List every object in a document with its type and properties. Use it before changing a " +
			"document to see what exists and which names to pass to get_object, update_object and delete_object. " +
			"An unknown document gives an empty list; list_documents shows the open ones.",
		InputSchema: inputSchema[documentInput](screenshotDefaults),
	}, s.listObjects)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "get_object",
		Description: "Get one object of a document with its type and all its properties. Use it to check the " +
			"values update_object or create_object set, or to see which properties an object has before updating it.",
		InputSchema: inputSchema[objectInput](screenshotDefaults),
	}, s.getObject)
}

func (s *Server) createObject(ctx context.Context, req *mcp.CallToolRequest, in createObjectInput) (*mcp.CallToolResult, any, error) {
	props, err := rawProperties(req)
	if err != nil {
		return failure("create object", &toolError{render.Error{Code: render.CodeInvalidInput, Message: err.Error(), Hint: propertiesHint}}, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("create object", err, ""), nil, nil
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
		return s.withNotice(failure("create object", err, "")), nil, nil
	}
	if !succeeded(res) {
		if name := str(res, "object_name"); name != "" {
			// The object was created but did not compute; it stays in the
			// document under this name.
			return s.withNotice(render.ErrorResult(render.Error{
				Code:    codeFreeCAD,
				Message: shortMessage(fmt.Sprintf("Object '%s' was created in '%s' but is not valid: %s", name, in.DocName, errorText(res))),
				Hint: fmt.Sprintf("Fix it with update_object {\"doc_name\": %q, \"obj_name\": %q, \"obj_properties\": {...}}, "+
					"or remove it with delete_object {\"doc_name\": %q, \"obj_name\": %q}.", in.DocName, name, in.DocName, name),
				Fields: map[string]any{"object_name": name},
			})), nil, nil
		}
		return s.withNotice(reported("create object", res,
			fmt.Sprintf("Check obj_type and the property names; call list_objects with {\"doc_name\": %q} to see the document.", in.DocName))), nil, nil
	}
	name := str(res, "object_name")
	out := render.SuccessResult(objectFront{Document: in.DocName, Object: name, Type: in.ObjType},
		fmt.Sprintf("Object '%s' created successfully.", name))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName))), nil, nil
}

func (s *Server) updateObject(ctx context.Context, req *mcp.CallToolRequest, in updateObjectInput) (*mcp.CallToolResult, any, error) {
	props, err := rawProperties(req)
	if err != nil {
		return failure("update object", &toolError{render.Error{Code: render.CodeInvalidInput, Message: err.Error(), Hint: propertiesHint}}, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("update object", err, ""), nil, nil
	}
	res, err := conn.EditObject(ctx, in.DocName, in.ObjName, map[string]any{"Properties": props})
	if err != nil {
		return s.withNotice(failure("update object", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reported("update object", res,
			fmt.Sprintf("Call get_object with {\"doc_name\": %q, \"obj_name\": %q} to see its properties.", in.DocName, in.ObjName))), nil, nil
	}
	name := str(res, "object_name")
	out := render.SuccessResult(objectFront{Document: in.DocName, Object: name},
		fmt.Sprintf("Object '%s' updated successfully.", name))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName))), nil, nil
}

func (s *Server) deleteObject(ctx context.Context, _ *mcp.CallToolRequest, in objectInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("delete object", err, ""), nil, nil
	}
	res, err := conn.DeleteObject(ctx, in.DocName, in.ObjName)
	if err != nil {
		return s.withNotice(failure("delete object", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reported("delete object", res,
			fmt.Sprintf("Call list_objects with {\"doc_name\": %q} to see the object names.", in.DocName))), nil, nil
	}
	name := str(res, "object_name")
	out := render.SuccessResult(objectFront{Document: in.DocName, Object: name},
		fmt.Sprintf("Object '%s' deleted successfully.", name))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName))), nil, nil
}

func (s *Server) listObjects(ctx context.Context, _ *mcp.CallToolRequest, in documentInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("list objects", err, ""), nil, nil
	}
	objects, err := conn.GetObjects(ctx, in.DocName)
	if err != nil {
		return s.withNotice(failure("list objects", err, "")), nil, nil
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
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName))), nil, nil
}

func (s *Server) getObject(ctx context.Context, _ *mcp.CallToolRequest, in objectInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("get object", err, ""), nil, nil
	}
	object, err := conn.GetObject(ctx, in.DocName, in.ObjName)
	if err != nil {
		return s.withNotice(failure("get object", err, "")), nil, nil
	}
	if object == nil {
		return s.withNotice(render.ErrorResult(render.Error{
			Code:    render.CodeNotFound,
			Message: fmt.Sprintf("object %q not found in document %q", in.ObjName, in.DocName),
			Hint:    fmt.Sprintf("Call list_objects with {\"doc_name\": %q} to see the object names, or list_documents for the documents.", in.DocName),
		})), nil, nil
	}
	out := render.SuccessResult(objectFront{Document: in.DocName, Object: in.ObjName}, jsonBlock(object))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName))), nil, nil
}

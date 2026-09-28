package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/laelhalawani/freecad-mcp/internal/freecad"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

// maxBedSize bounds the printer bed dimensions in mm.
const maxBedSize = 10000

type checkPrintabilityInput struct {
	DocName                string   `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	ObjectNames            []string `json:"object_names,omitempty" jsonschema:"names of the objects to check, as list_objects shows them (default: the visible top-level solids and meshes)"`
	BedX                   *float64 `json:"bed_x,omitempty" jsonschema:"printer bed width in mm; give bed_x, bed_y and bed_z together to check the fit"`
	BedY                   *float64 `json:"bed_y,omitempty" jsonschema:"printer bed depth in mm"`
	BedZ                   *float64 `json:"bed_z,omitempty" jsonschema:"printer build height in mm"`
	BuildDirection         *string  `json:"build_direction,omitempty" jsonschema:"the model axis that points up on the printer (default +Z)"`
	OverhangAngleDeg       *float64 `json:"overhang_angle_deg,omitempty" jsonschema:"overhangs steeper than this angle from vertical, in degrees from 0 to 89, count as needing support (default 45)"`
	CheckSelfIntersections *bool    `json:"check_self_intersections,omitempty" jsonschema:"also look for self-intersecting triangles, which is slower on large meshes (default true)"`
	Quality                *string  `json:"quality,omitempty" jsonschema:"tessellation preset for the mesh checks (default standard)"`
	LinearDeflection       *float64 `json:"linear_deflection,omitempty" jsonschema:"largest distance in mm between the surface and its triangles, 0.001 to 100; overrides the preset"`
	AngularDeflectionDeg   *float64 `json:"angular_deflection_deg,omitempty" jsonschema:"largest angle in degrees between neighbouring triangles, 0.5 to 90; overrides the preset"`
	Timeout                *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 300"`
}

const checkPrintabilityDescription = `Check whether objects of a FreeCAD document are ready for 3D printing, before export_document writes them to STL or 3MF.

For each object it reports: whether the shape is valid and closed and how many solids it has, FreeCAD's full shape check, whether its tessellated mesh is a closed solid without non-manifold edges or self-intersections, its size along the build axes and, with bed_x, bed_y and bed_z, whether it fits the printer (turned by 90 degrees if needed), and the area of overhangs steeper than overhang_angle_deg that need support. printable is true only when at least one object was checked and none of them has an issue; it is false, not vacuously true, when object_names names nothing or no visible top-level solid or mesh exists.

Mesh objects are checked as they are; fix them with repair_mesh. Invalid shapes usually come from a failed feature: recompute_document shows which one.`

type printabilityFront struct {
	Document    string `yaml:"document"`
	Printable   bool   `yaml:"printable"`
	ObjectCount int    `yaml:"object_count"`
	// ObjectsWithIssues counts objects that have at least one issue, unlike
	// analyze_mesh's issue_count, which counts issue strings for one object;
	// the two tools check different things, so the key names differ too.
	ObjectsWithIssues int `yaml:"objects_with_issues"`
}

func (s *Server) registerPrintabilityTools() {
	schema := inputSchema[checkPrintabilityInput](map[string]string{
		"build_direction": `"+Z"`, "overhang_angle_deg": "45", "check_self_intersections": "true",
		"quality": `"standard"`, "timeout": "300"})
	schema = withEnum(schema, "build_direction", "+Z", "-Z", "+X", "-X", "+Y", "-Y")
	schema = withEnum(schema, "quality", "coarse", "standard", "fine")
	schema = withRange(schema, 0, 89, "overhang_angle_deg")
	schema = withRange(schema, 0.001, 100, "linear_deflection")
	schema = withRange(schema, 0.5, 90, "angular_deflection_deg")
	for _, name := range []string{"bed_x", "bed_y", "bed_z"} {
		schema = withPositiveMax(schema, name, maxBedSize)
	}
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "check_printability",
		Description: checkPrintabilityDescription,
		InputSchema: withPositiveMax(schema, "timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.checkPrintability)
}

func (s *Server) checkPrintability(ctx context.Context, _ *mcp.CallToolRequest, in checkPrintabilityInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure("check printability", err, ""), nil, nil
	}
	bedGiven := in.BedX != nil || in.BedY != nil || in.BedZ != nil
	if bedGiven && (in.BedX == nil || in.BedY == nil || in.BedZ == nil) {
		return failure("check printability", &toolError{render.Error{
			Code:    render.CodeInvalidInput,
			Message: "bed_x, bed_y and bed_z must be given together, or not at all",
			Hint:    "Pass all three, e.g. {\"bed_x\": 220, \"bed_y\": 220, \"bed_z\": 250}, or omit all three to skip the bed fit check.",
		}}, ""), nil, nil
	}

	options := map[string]any{}
	if bedGiven {
		options["bed"] = []float64{*in.BedX, *in.BedY, *in.BedZ}
	}
	if in.BuildDirection != nil {
		options["build_direction"] = *in.BuildDirection
	}
	if in.OverhangAngleDeg != nil {
		options["overhang_angle_deg"] = *in.OverhangAngleDeg
	}
	if in.CheckSelfIntersections != nil {
		options["check_self_intersections"] = *in.CheckSelfIntersections
	}
	if in.Quality != nil {
		options["quality"] = *in.Quality
	}
	if in.LinearDeflection != nil {
		options["linear_deflection"] = *in.LinearDeflection
	}
	if in.AngularDeflectionDeg != nil {
		options["angular_deflection_deg"] = *in.AngularDeflectionDeg
	}

	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("check printability", err, ""), nil, nil
	}
	res, err := conn.CheckPrintability(ctx, in.DocName, in.ObjectNames, options, in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure("check printability", err, largerTimeout("check_printability"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("check printability", res,
			fmt.Sprintf("Call list_objects with {\"doc_name\": %q} to see the object names.", in.DocName))), nil, nil
	}

	objects, _ := res["objects"].([]any)
	printable, _ := res["printable"].(bool)
	objectsWithIssues := 0

	var body strings.Builder
	if len(objects) == 0 {
		fmt.Fprintf(&body, "No solid or mesh objects were found to check in '%s'. Pass object_names, or make an object "+
			"visible; list_objects with {\"doc_name\": %q} shows the document.", in.DocName, in.DocName)
	} else if printable {
		fmt.Fprintf(&body, "Document '%s': all %d object(s) checked are ready to print.\n", in.DocName, len(objects))
	} else {
		fmt.Fprintf(&body, "Document '%s': %d object(s) checked, some have issues.\n", in.DocName, len(objects))
	}

	for _, item := range objects {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := str(obj, "name")
		label := str(obj, "label")
		texts := stringItems(obj["issues"])
		if len(texts) > 0 {
			objectsWithIssues++
			fmt.Fprintf(&body, "\n- %s (%s): %s", name, label, strings.Join(texts, "; "))
		} else {
			fmt.Fprintf(&body, "\n- %s (%s): ready to print", name, label)
		}
		if area, ok := number(obj["overhang_area_mm2"]); ok && area > 0 {
			fraction, _ := number(obj["overhang_fraction"])
			fmt.Fprintf(&body, " (overhang area %.1f mm^2, %.0f%% of surface area; plan supports)", area, fraction*100)
		}
	}

	front := printabilityFront{
		Document:          in.DocName,
		Printable:         printable,
		ObjectCount:       len(objects),
		ObjectsWithIssues: objectsWithIssues,
	}
	return s.withNotice(render.SuccessResult(front, body.String())), nil, nil
}

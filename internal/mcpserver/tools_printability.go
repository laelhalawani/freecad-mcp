package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/mcp-wizard/render"
)

// maxBedSize bounds the printer bed dimensions in mm.
const maxBedSize = 10000

type checkPrintabilityInput struct {
	DocName                string   `json:"doc_name"`
	ObjectNames            []string `json:"object_names,omitempty"`
	BedX                   *float64 `json:"bed_x,omitempty"`
	BedY                   *float64 `json:"bed_y,omitempty"`
	BedZ                   *float64 `json:"bed_z,omitempty"`
	BuildDirection         *string  `json:"build_direction,omitempty"`
	OverhangAngleDeg       *float64 `json:"overhang_angle_deg,omitempty"`
	CheckSelfIntersections *bool    `json:"check_self_intersections,omitempty"`
	Quality                *string  `json:"quality,omitempty"`
	LinearDeflection       *float64 `json:"linear_deflection,omitempty"`
	AngularDeflectionDeg   *float64 `json:"angular_deflection_deg,omitempty"`
	Timeout                *float64 `json:"timeout,omitempty"`
}

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
	addTool(s.mcpServer, "check_printability", withPositiveMax(schema, "timeout", freecad.DefaultMaxExecuteCodeTime), s.checkPrintability)
}

// printabilityFacts lists what a check found for one object whatever its
// issues: the bounding box size, whether it fits the bed when one was given,
// and the overhang area.
func printabilityFacts(obj map[string]any, in checkPrintabilityInput) []string {
	var facts []string
	if size, ok := obj["size"].([]any); ok && len(size) == 3 {
		facts = append(facts, fmt.Sprintf("size %s x %s x %s mm (the last along the build direction)",
			formatNumber(size[0]), formatNumber(size[1]), formatNumber(size[2])))
	}
	if in.BedX != nil && in.BedY != nil && in.BedZ != nil {
		if fits, ok := obj["fits_bed"].(bool); ok {
			bed := fmt.Sprintf("%g x %g x %g mm bed", *in.BedX, *in.BedY, *in.BedZ)
			if fits {
				facts = append(facts, "fits the "+bed)
			} else {
				facts = append(facts, "does not fit the "+bed+", even turned about the build axis")
			}
		}
	}
	if area, ok := number(obj["overhang_area_mm2"]); ok {
		if area > 0 {
			fraction, _ := number(obj["overhang_fraction"])
			facts = append(facts, fmt.Sprintf("overhang area %.1f mm^2, %.0f%% of surface area; plan supports", area, fraction*100))
		} else {
			facts = append(facts, "no overhang area needing support")
		}
	}
	return facts
}

func (s *Server) checkPrintability(ctx context.Context, _ *mcp.CallToolRequest, in checkPrintabilityInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure(ctx, "check printability", err, ""), nil, nil
	}
	bedGiven := in.BedX != nil || in.BedY != nil || in.BedZ != nil
	if bedGiven && (in.BedX == nil || in.BedY == nil || in.BedZ == nil) {
		return failure(ctx, "check printability", &toolError{render.Error{
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
		return failure(ctx, "check printability", err, ""), nil, nil
	}
	res, err := conn.CheckPrintability(ctx, in.DocName, in.ObjectNames, options, in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure(ctx, "check printability", err, largerTimeout("check_printability"))), nil, nil
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
		if facts := printabilityFacts(obj, in); len(facts) > 0 {
			fmt.Fprintf(&body, "\n  %s.", strings.Join(facts, "; "))
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

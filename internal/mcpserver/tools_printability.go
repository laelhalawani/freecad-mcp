package mcpserver

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/mcp-wizard/render"
)

// maxBedSize bounds the plate dimensions in mm.
const maxBedSize = 10000

type checkPrintabilityInput struct {
	DocName     string   `json:"doc_name"`
	ObjectNames []string `json:"object_names,omitempty"`
	BedX        float64  `json:"bed_x"`
	BedY        float64  `json:"bed_y"`
	BedZ        *float64 `json:"bed_z,omitempty"`
	BedOriginX  *float64 `json:"bed_origin_x,omitempty"`
	BedOriginY  *float64 `json:"bed_origin_y,omitempty"`
	Timeout     *float64 `json:"timeout,omitempty"`
}

type printabilityFront struct {
	Document    string `yaml:"document"`
	Printable   bool   `yaml:"printable"`
	ObjectCount int    `yaml:"object_count"`
	// ObjectsWithIssues counts parts with at least one issue: outside the
	// plate, overlapping another part, or without a solid.
	ObjectsWithIssues int `yaml:"objects_with_issues"`
	OverlapCount      int `yaml:"overlap_count"`
}

func (s *Server) registerPrintabilityTools() {
	schema := inputSchema[checkPrintabilityInput](map[string]string{"timeout": "120"})
	for _, name := range []string{"bed_x", "bed_y", "bed_z"} {
		schema = withPositiveMax(schema, name, maxBedSize)
	}
	addTool(s.mcpServer, "check_printability", withPositiveMax(schema, "timeout", freecad.DefaultMaxExecuteCodeTime), s.checkPrintability)
}

// partFacts lists what the check found for one part whatever its issues: its
// size and how much room it has to the plate edges.
func partFacts(obj map[string]any) []string {
	var facts []string
	if size, ok := obj["size"].([]any); ok && len(size) == 3 {
		facts = append(facts, fmt.Sprintf("size %s x %s x %s mm",
			formatNumber(size[0]), formatNumber(size[1]), formatNumber(size[2])))
	}
	if margin, ok := number(obj["free_margin_mm"]); ok {
		facts = append(facts, fmt.Sprintf("free margin to the plate edges %.4g mm", margin))
	}
	return facts
}

func (s *Server) checkPrintability(ctx context.Context, _ *mcp.CallToolRequest, in checkPrintabilityInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure(ctx, "check printability", err, ""), nil, nil
	}

	bed := []float64{in.BedX, in.BedY}
	if in.BedZ != nil {
		bed = append(bed, *in.BedZ)
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "check printability", err, ""), nil, nil
	}
	options := map[string]any{"bed": bed}
	originX, originY := 0.0, 0.0
	if in.BedOriginX != nil {
		originX = *in.BedOriginX
	}
	if in.BedOriginY != nil {
		originY = *in.BedOriginY
	}
	for _, v := range []float64{originX, originY} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return failure(ctx, "check printability", fmt.Errorf("bed_origin_x and bed_origin_y must be finite numbers"), ""), nil, nil
		}
	}
	if originX != 0 || originY != 0 {
		options["origin"] = []float64{originX, originY}
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
	overlaps, _ := res["overlaps"].([]any)
	notChecked, _ := res["not_checked"].([]any)
	printable, _ := res["printable"].(bool)
	objectsWithIssues := 0

	plate := fmt.Sprintf("%g x %g mm plate", in.BedX, in.BedY)
	if in.BedZ != nil {
		plate = fmt.Sprintf("%g x %g x %g mm plate", in.BedX, in.BedY, *in.BedZ)
	}
	if originX != 0 || originY != 0 {
		plate += fmt.Sprintf(" with its corner at x %g, y %g", originX, originY)
	}
	var body strings.Builder
	switch {
	case len(objects) == 0:
		fmt.Fprintf(&body, "No solid parts were found to check in '%s'. Pass object_names, or make a part visible; "+
			"list_objects with {\"doc_name\": %q} shows the document.", in.DocName, in.DocName)
	case printable:
		fmt.Fprintf(&body, "Document '%s': all %d part(s) lie inside the %s and none overlap.\n", in.DocName, len(objects), plate)
	default:
		fmt.Fprintf(&body, "Document '%s': %d part(s) checked against the %s; some need to move.\n", in.DocName, len(objects), plate)
	}

	for _, item := range objects {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// The object Name is what other tools take; the Label is added only
		// when it differs, so two parts are never confused.
		who := str(obj, "name")
		if label := str(obj, "label"); label != "" && label != who {
			who = fmt.Sprintf("%s (label %q)", who, label)
		}
		texts := stringItems(obj["issues"])
		if len(texts) > 0 {
			objectsWithIssues++
			fmt.Fprintf(&body, "\n- %s: %s", who, strings.Join(texts, "; "))
		} else {
			fmt.Fprintf(&body, "\n- %s: inside the plate, no overlap", who)
		}
		if facts := partFacts(obj); len(facts) > 0 {
			fmt.Fprintf(&body, "\n  %s.", strings.Join(facts, "; "))
		}
	}
	for _, item := range notChecked {
		if pair, ok := item.(map[string]any); ok {
			fmt.Fprintf(&body, "\n\nNot checked: %s and %s (%s).", str(pair, "a"), str(pair, "b"), str(pair, "reason"))
		}
	}

	front := printabilityFront{
		Document:          in.DocName,
		Printable:         printable,
		ObjectCount:       len(objects),
		ObjectsWithIssues: objectsWithIssues,
		OverlapCount:      len(overlaps),
	}
	return s.withNotice(render.SuccessResult(front, body.String())), nil, nil
}

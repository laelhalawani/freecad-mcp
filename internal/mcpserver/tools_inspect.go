package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

type measureRef struct {
	Object string  `json:"object" jsonschema:"the name of an object, as list_objects shows it"`
	Sub    *string `json:"sub,omitempty" jsonschema:"a sub-element name (Face3, Edge1, Vertex2) or the full object path get_selection returns for anything below the top level (Body.Pad.Face3); pass get_selection's sub_elements unchanged rather than shortening them to the element name, which would pick that element on the top object instead (default: the whole object)"`
}

type measureInput struct {
	DocName string       `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	Kind    string       `json:"kind" jsonschema:"what to measure: distance and angle take two refs, radius one, length, area and volume one or more"`
	Refs    []measureRef `json:"refs" jsonschema:"the objects or sub-elements to measure"`
}

type getSelectionInput struct {
	DocName *string `json:"doc_name,omitempty" jsonschema:"the document whose selection to read (default: every open document)"`
}

type measureFront struct {
	Document string   `yaml:"document"`
	Kind     string   `yaml:"kind"`
	Value    *float64 `yaml:"value,omitempty"`
	Unit     string   `yaml:"unit"`
}

type selectionFront struct {
	Count int `yaml:"count"`
}

func (s *Server) registerInspectTools() {
	schema := withMinItems(withEnum(inputSchema[measureInput](nil), "kind", "distance", "angle", "length", "radius", "area", "volume"), "refs", 1)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "measure",
		Description: "Measure FreeCAD geometry in global coordinates: the shortest distance between two objects or " +
			"sub-elements (with the closest points), the angle between two straight edges or planar faces, the " +
			"length of edges, the radius of a circular edge or a cylindrical or spherical face, and the area or " +
			"volume of faces and objects. get_selection returns the sub-element names of what the user selected, " +
			"and get_object gives an object's bounding box.",
		InputSchema: schema,
	}, s.measure)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "get_selection",
		Description: "Read what the user has selected in FreeCAD: each selected object with its document, label and " +
			"type, the selected sub-elements such as Face6 or Edge12, and the picked points. Use it when the user " +
			"refers to \"this face\" or \"the selected edge\", then pass the names to measure or update_object. An " +
			"empty result means nothing is selected.",
		InputSchema: inputSchema[getSelectionInput](nil),
	}, s.getSelection)
}

// refLabel names a ref the way a person would type it back: the object name,
// or "object.sub" when a sub-element was given.
func refLabel(r measureRef) string {
	if r.Sub != nil && *r.Sub != "" {
		return r.Object + "." + *r.Sub
	}
	return r.Object
}

// refsDescription describes what a measurement was taken of, for the two-ref
// kinds as "a and b", otherwise as a comma-separated list.
func refsDescription(refs []measureRef) string {
	labels := make([]string, len(refs))
	for i, r := range refs {
		labels[i] = refLabel(r)
	}
	if len(labels) == 2 {
		return labels[0] + " and " + labels[1]
	}
	return strings.Join(labels, ", ")
}

func (s *Server) measure(ctx context.Context, _ *mcp.CallToolRequest, in measureInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "measure", err, ""), nil, nil
	}
	refs := make([]map[string]any, len(in.Refs))
	for i, r := range in.Refs {
		ref := map[string]any{"object": r.Object}
		if r.Sub != nil {
			ref["sub"] = *r.Sub
		}
		refs[i] = ref
	}
	res, err := conn.Measure(ctx, in.DocName, in.Kind, refs)
	if err != nil {
		return s.withNotice(failure(ctx, "measure", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("measure", res,
			fmt.Sprintf("Call list_objects with {\"doc_name\": %q} to see the object and sub-element names.", in.DocName))), nil, nil
	}
	unit := str(res, "unit")
	front := measureFront{Document: in.DocName, Kind: in.Kind, Unit: unit}
	if v, ok := number(res["value"]); ok {
		front.Value = &v
	}

	var body strings.Builder
	fmt.Fprintf(&body, "%s of %s: %s.", in.Kind, refsDescription(in.Refs), formatMeasure(res["value"], unit))
	if points, ok := res["points"].([]any); ok && len(points) == 2 {
		fmt.Fprintf(&body, "\n\nClosest points: %v to %v.", points[0], points[1])
	}
	body.WriteString("\n\n" + jsonBlock(res))

	out := render.SuccessResult(front, body.String())
	return s.withNotice(out), nil, nil
}

func (s *Server) getSelection(ctx context.Context, _ *mcp.CallToolRequest, in getSelectionInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "get selection", err, ""), nil, nil
	}
	doc := ""
	if in.DocName != nil {
		doc = *in.DocName
	}
	res, err := conn.GetSelection(ctx, doc)
	if err != nil {
		return s.withNotice(failure(ctx, "get selection", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("get selection", res, "")), nil, nil
	}
	rows, _ := res["selection"].([]any)
	front := selectionFront{Count: len(rows)}

	var body strings.Builder
	if len(rows) == 0 {
		body.WriteString("Nothing is selected. Select an object or sub-element in FreeCAD, then call get_selection again.")
	} else {
		body.WriteString("| document | object | label | type | sub-elements |\n|---|---|---|---|---|\n")
		for _, item := range rows {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			subText := strings.Join(stringItems(row["sub_elements"]), ", ")
			if subText == "" {
				subText = "(whole object)"
			}
			fmt.Fprintf(&body, "| %s | %s | %s | %s | %s |\n", str(row, "document"), str(row, "object"), str(row, "label"), str(row, "type"), subText)
		}
		// The table leaves out the picked points (a click location per row);
		// they follow here in full.
		body.WriteString("\n" + jsonBlock(res))
	}

	out := render.SuccessResult(front, body.String())
	return s.withNotice(out), nil, nil
}

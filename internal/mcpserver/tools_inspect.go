package mcpserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

type measureRef struct {
	Object string  `json:"object"`
	Sub    *string `json:"sub,omitempty"`
}

type measureInput struct {
	DocName string       `json:"doc_name"`
	Kind    string       `json:"kind"`
	Refs    []measureRef `json:"refs"`
}

type getSelectionInput struct {
	DocName *string `json:"doc_name,omitempty"`
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

type listSubelementsInput struct {
	DocName string `json:"doc_name"`
	ObjName string `json:"obj_name"`
	Kind    string `json:"kind,omitempty"`
}

type subelementsFront struct {
	Document string `yaml:"document"`
	Object   string `yaml:"object_name"`
	Kind     string `yaml:"kind"`
	Faces    *int   `yaml:"faces,omitempty"`
	Edges    *int   `yaml:"edges,omitempty"`
}

func (s *Server) registerInspectTools() {
	schema := withMinItems(withEnum(inputSchema[measureInput](nil), "kind", "distance", "angle", "length", "radius", "area", "volume"), "refs", 1)
	addTool(s.mcpServer, "measure", schema, s.measure)
	addTool(s.mcpServer, "get_selection", inputSchema[getSelectionInput](nil), s.getSelection)
	addTool(s.mcpServer, "list_subelements",
		withEnum(inputSchema[listSubelementsInput](map[string]string{"kind": `"faces"`}), "kind", "faces", "edges", "all"), s.listSubelements)
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

func (s *Server) listSubelements(ctx context.Context, _ *mcp.CallToolRequest, in listSubelementsInput) (*mcp.CallToolResult, any, error) {
	kind := in.Kind
	if kind == "" {
		kind = "faces"
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "list subelements", err, ""), nil, nil
	}
	res, err := conn.ListSubelements(ctx, in.DocName, in.ObjName, kind)
	if err != nil {
		return s.withNotice(failure(ctx, "list subelements", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("list subelements", res,
			fmt.Sprintf("Call list_objects with {\"doc_name\": %q} to see the object names.", in.DocName))), nil, nil
	}
	front := subelementsFront{Document: in.DocName, Object: in.ObjName, Kind: kind}
	var body strings.Builder
	if faces, ok := res["faces"].([]any); ok {
		n := len(faces)
		front.Faces = &n
		body.WriteString(subelementTable("faces", "surface", "area mm^2", faces, faceDetails))
	}
	if edges, ok := res["edges"].([]any); ok {
		n := len(edges)
		front.Edges = &n
		if body.Len() > 0 {
			body.WriteString("\n")
		}
		body.WriteString(subelementTable("edges", "curve", "length mm", edges, edgeDetails))
	}
	body.WriteString("\nPass a name as a measure ref's sub, or in a References entry such as {\"object_name\": " +
		fmt.Sprintf("%q", in.ObjName) + ", \"face\": \"Face1\"}.")
	return s.withNotice(render.SuccessResult(front, body.String())), nil, nil
}

// subelementTable renders rows (the addon's face or edge dicts) as a markdown
// table with name, the type column, the size column and a details column.
func subelementTable(what, typeHeader, sizeHeader string, rows []any, details func(map[string]any) string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "| %s | %s | %s | details |\n|---|---|---|---|\n", strings.TrimSuffix(what, "s"), typeHeader, sizeHeader)
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		size := row["area"]
		if what == "edges" {
			size = row["length"]
		}
		typeKey := "surface"
		if what == "edges" {
			typeKey = "curve"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", str(row, "name"), str(row, typeKey), formatNumber(size), details(row))
	}
	if len(rows) == 0 {
		fmt.Fprintf(&b, "\nThe shape has no %s.\n", what)
	}
	return b.String()
}

func faceDetails(row map[string]any) string {
	parts := []string{"center " + formatVector(row["center"])}
	if v, ok := row["normal"]; ok {
		parts = append(parts, "normal "+formatVector(v))
	}
	if v, ok := row["radius"]; ok {
		parts = append(parts, "radius "+formatNumber(v))
	}
	if v, ok := row["axis"]; ok {
		parts = append(parts, "axis "+formatVector(v))
	}
	return strings.Join(parts, ", ")
}

func edgeDetails(row map[string]any) string {
	var parts []string
	if v, ok := row["radius"]; ok {
		parts = append(parts, "radius "+formatNumber(v))
	}
	if v, ok := row["center"]; ok {
		parts = append(parts, "center "+formatVector(v))
	}
	return strings.Join(parts, ", ")
}

// formatNumber renders a numeric reply field, or "?" when it is missing (the
// addon sends null for a value that was not finite).
func formatNumber(v any) string {
	f, ok := number(v)
	if !ok {
		return "?"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// formatVector renders a three-number reply field as "(x, y, z)".
func formatVector(v any) string {
	items, ok := v.([]any)
	if !ok {
		return "?"
	}
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = formatNumber(item)
	}
	return "(" + strings.Join(parts, ", ") + ")"
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

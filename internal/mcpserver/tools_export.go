package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/mcp-wizard/render"
)

type exportDocumentInput struct {
	DocName              string   `json:"doc_name"`
	Path                 string   `json:"path"`
	ObjectNames          []string `json:"object_names,omitempty"`
	Overwrite            *bool    `json:"overwrite,omitempty"`
	IncludeHidden        *bool    `json:"include_hidden,omitempty"`
	Recompute            *bool    `json:"recompute,omitempty"`
	Quality              *string  `json:"quality,omitempty"`
	LinearDeflection     *float64 `json:"linear_deflection,omitempty"`
	AngularDeflectionDeg *float64 `json:"angular_deflection_deg,omitempty"`
	Relative             *bool    `json:"relative,omitempty"`
	ASCII                *bool    `json:"ascii,omitempty"`
	StepUnit             *string  `json:"step_unit,omitempty"`
	StepSchema           *string  `json:"step_schema,omitempty"`
	Timeout              *float64 `json:"timeout,omitempty"`
}

func (s *Server) registerExportTools() {
	schema := inputSchema[exportDocumentInput](map[string]string{
		"overwrite": "false", "include_hidden": "false", "recompute": "true", "quality": `"standard"`,
		"relative": "false", "ascii": "false", "timeout": "300"})
	schema = withEnum(schema, "quality", "coarse", "standard", "fine")
	schema = withEnum(schema, "step_unit", "MM", "M", "INCH")
	schema = withEnum(schema, "step_schema", "AP203", "AP214IS", "AP242DIS")
	schema = withRange(schema, 0.001, 100, "linear_deflection")
	schema = withRange(schema, 0.5, 90, "angular_deflection_deg")
	addTool(s.mcpServer, "export_document", withPositiveMax(schema, "timeout", freecad.DefaultMaxExecuteCodeTime), s.exportDocument)
}

type exportDocumentFront struct {
	Document       string `yaml:"document"`
	File           string `yaml:"file"`
	Format         string `yaml:"format"`
	Exporter       string `yaml:"exporter"`
	Bytes          int64  `yaml:"bytes"`
	ObjectCount    int    `yaml:"object_count"`
	Units          string `yaml:"units"`
	Facets         *int64 `yaml:"facets,omitempty"`
	ClosedMesh     *bool  `yaml:"closed_mesh,omitempty"`
	CompanionFile  string `yaml:"companion_file,omitempty"`
	CompanionBytes *int64 `yaml:"companion_bytes,omitempty"`
}

// exportOptionsMap builds the addon's “options“ struct from the given
// arguments, nil when none were given.
func exportOptionsMap(in exportDocumentInput) map[string]any {
	opts := map[string]any{}
	if len(in.ObjectNames) > 0 {
		opts["object_names"] = in.ObjectNames
	}
	if in.Overwrite != nil {
		opts["overwrite"] = *in.Overwrite
	}
	if in.IncludeHidden != nil {
		opts["include_hidden"] = *in.IncludeHidden
	}
	if in.Recompute != nil {
		opts["recompute"] = *in.Recompute
	}
	if in.Quality != nil {
		opts["quality"] = *in.Quality
	}
	if in.LinearDeflection != nil {
		opts["linear_deflection"] = *in.LinearDeflection
	}
	if in.AngularDeflectionDeg != nil {
		opts["angular_deflection_deg"] = *in.AngularDeflectionDeg
	}
	if in.Relative != nil {
		opts["relative"] = *in.Relative
	}
	if in.ASCII != nil {
		opts["ascii"] = *in.ASCII
	}
	if in.StepUnit != nil {
		opts["step_unit"] = *in.StepUnit
	}
	if in.StepSchema != nil {
		opts["step_schema"] = *in.StepSchema
	}
	if len(opts) == 0 {
		return nil
	}
	return opts
}

func (s *Server) exportDocument(ctx context.Context, _ *mcp.CallToolRequest, in exportDocumentInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure(ctx, "export document", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "export document", err, ""), nil, nil
	}
	res, err := conn.ExportDocument(ctx, in.DocName, in.Path, exportOptionsMap(in), in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure(ctx, "export document", err, largerTimeout("export_document"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("export document", res,
			fmt.Sprintf("Call list_objects with {\"doc_name\": %q} to see the document's objects.", in.DocName))), nil, nil
	}

	objects, _ := res["objects"].([]any)
	skipped, _ := res["skipped"].([]any)
	warnings, _ := res["warnings"].([]any)
	front := exportDocumentFront{
		Document:    str(res, "document"),
		File:        str(res, "file_name"),
		Format:      str(res, "format"),
		Exporter:    str(res, "exporter"),
		Bytes:       bigIntField(res, "bytes"),
		ObjectCount: len(objects),
		Units:       str(res, "units"),
	}

	var body strings.Builder
	fmt.Fprintf(&body, "Exported %d object(s) from '%s' to '%s' (%s, via %s).",
		len(objects), in.DocName, front.File, front.Format, front.Exporter)
	body.WriteString(createdFolderNote(res))

	if names := stringItems(objects); len(names) > 0 {
		fmt.Fprintf(&body, "\n\nExported: %s.", strings.Join(names, ", "))
	}

	if mesh, ok := res["mesh"].(map[string]any); ok {
		facets := bigIntField(mesh, "facets")
		front.Facets = &facets
		closed, _ := mesh["closed"].(bool)
		front.ClosedMesh = &closed
		linear, _ := number(mesh["linear_deflection"])
		angular, _ := number(mesh["angular_deflection_deg"])
		relative, _ := mesh["relative"].(bool)
		closedWord := "open"
		if closed {
			closedWord = "closed"
		}
		fmt.Fprintf(&body, "\n\nMesh: %d facet(s), %s, linear deflection %.4g mm, angular deflection %.4g deg, relative %t.",
			facets, closedWord, linear, angular, relative)
	}

	if companion, ok := res["companion_file"].(map[string]any); ok {
		companionBytes := bigIntField(companion, "bytes")
		front.CompanionFile = str(companion, "path")
		front.CompanionBytes = &companionBytes
		fmt.Fprintf(&body, "\n\nglTF also wrote its binary buffer to '%s' (%d bytes), not counted in bytes above.",
			front.CompanionFile, companionBytes)
	}

	if len(skipped) > 0 {
		body.WriteString("\n\nSkipped:")
		for _, item := range skipped {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			fmt.Fprintf(&body, "\n- %s: %s", str(obj, "name"), str(obj, "reason"))
		}
	}

	if names := stringItems(warnings); len(names) > 0 {
		fmt.Fprintf(&body, "\n\nWarnings:\n- %s", strings.Join(names, "\n- "))
	}

	return s.withNotice(render.SuccessResult(front, body.String())), nil, nil
}

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
	DocName              string   `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	Path                 string   `json:"path" jsonschema:"absolute path of the file to write on the machine running FreeCAD; its extension picks the format"`
	ObjectNames          []string `json:"object_names,omitempty" jsonschema:"names of the objects to export, as list_objects shows them (default: the visible top-level objects with geometry)"`
	Overwrite            *bool    `json:"overwrite,omitempty" jsonschema:"replace an existing file at path (default false)"`
	IncludeHidden        *bool    `json:"include_hidden,omitempty" jsonschema:"include hidden top-level objects in the default set (default false)"`
	Recompute            *bool    `json:"recompute,omitempty" jsonschema:"recompute the document before exporting when it needs it (default true)"`
	Quality              *string  `json:"quality,omitempty" jsonschema:"mesh formats and glTF: tessellation preset; coarse for previews, standard for FDM prints, fine for resin and small curved parts (default standard)"`
	LinearDeflection     *float64 `json:"linear_deflection,omitempty" jsonschema:"mesh formats and glTF: largest distance in mm between the surface and its triangles, 0.001 to 100; overrides the preset"`
	AngularDeflectionDeg *float64 `json:"angular_deflection_deg,omitempty" jsonschema:"mesh formats: largest angle in degrees between neighbouring triangles, 0.5 to 90; overrides the preset"`
	Relative             *bool    `json:"relative,omitempty" jsonschema:"mesh formats: linear_deflection is relative to each edge's length (default false)"`
	ASCII                *bool    `json:"ascii,omitempty" jsonschema:"STL only: write ASCII STL instead of binary (default false)"`
	StepUnit             *string  `json:"step_unit,omitempty" jsonschema:"STEP and IGES only: the length unit written to the file (default: FreeCAD's export preference)"`
	StepSchema           *string  `json:"step_schema,omitempty" jsonschema:"STEP only: the application protocol (default: FreeCAD's export preference)"`
	Timeout              *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 300; raise it for fine meshes of large models"`
}

const exportDocumentDescription = `Export objects of an open FreeCAD document to a file for 3D printing or CAD exchange, without any dialog in FreeCAD.

The format follows the extension of path: .stl (binary, or ASCII with ascii true), .ast, .3mf, .amf, .obj, .ply and .off are triangle meshes for slicers, tessellated with the quality preset (coarse, standard, fine) or explicit linear_deflection and angular_deflection_deg; 3MF and AMF keep one object per part and declare millimeters, which slicers prefer. .step/.stp and .iges/.igs keep exact geometry, names and colors; .glb/.gltf write a tessellated scene in meters (.gltf also writes a separate .bin buffer next to it, reported in the reply as companion_file); .brep/.brp write the exact shape; .FCStd writes a copy of the document; .dxf and .svg write 2D geometry projected on the XY plane.

Without object_names the visible top-level objects that have geometry are exported, so a Body is written once, not once per feature; list_objects shows the names. The reply gives the file, its size, the exported and skipped objects and, for meshes, the facet count and whether the mesh is closed. An existing file is only replaced with overwrite true. Use save_document_as to save the document itself, and check_printability before exporting for a printer.`

func (s *Server) registerExportTools() {
	schema := inputSchema[exportDocumentInput](map[string]string{
		"overwrite": "false", "include_hidden": "false", "recompute": "true", "quality": `"standard"`,
		"relative": "false", "ascii": "false", "timeout": "300"})
	schema = withEnum(schema, "quality", "coarse", "standard", "fine")
	schema = withEnum(schema, "step_unit", "MM", "M", "INCH")
	schema = withEnum(schema, "step_schema", "AP203", "AP214IS", "AP242DIS")
	schema = withRange(schema, 0.001, 100, "linear_deflection")
	schema = withRange(schema, 0.5, 90, "angular_deflection_deg")
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "export_document",
		Description: exportDocumentDescription + "\n\n" + filePathsNote,
		InputSchema: withPositiveMax(schema, "timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.exportDocument)
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

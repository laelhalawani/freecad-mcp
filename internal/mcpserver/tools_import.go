package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/laelhalawani/freecad-mcp/internal/freecad"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

type importFileInput struct {
	Path         string   `json:"path" jsonschema:"absolute path of the file to import on the machine running FreeCAD"`
	DocName      *string  `json:"doc_name,omitempty" jsonschema:"the open document to import into; omit it to create a new document named after the file"`
	Merge        *bool    `json:"merge,omitempty" jsonschema:"STEP, IGES and glTF only: merge the file's parts into one compound (default: FreeCAD's import preference)"`
	UseLinkGroup *bool    `json:"use_link_group,omitempty" jsonschema:"STEP, IGES and glTF only: build assemblies from App::Link groups (default: FreeCAD's import preference)"`
	ImportHidden *bool    `json:"import_hidden,omitempty" jsonschema:"STEP, IGES and glTF only: also import objects the file marks hidden (default: FreeCAD's import preference)"`
	Timeout      *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 300; raise it for large assemblies"`
	screenshotOptions
}

type importFileFront struct {
	Document        string `yaml:"document"`
	CreatedDocument bool   `yaml:"created_document"`
	Format          string `yaml:"format"`
	Importer        string `yaml:"importer"`
	ObjectCount     int    `yaml:"object_count"`
	Transaction     string `yaml:"transaction,omitempty"`
}

const importFileDescription = `Import a CAD, mesh or 2D file into a FreeCAD document, without any dialog in FreeCAD.

Formats by extension: .step/.stp, .iges/.igs, .gltf/.glb (assemblies keep their parts and colors), .brep/.brp, .stl/.ast, .obj, .off, .ply, .3mf (triangle meshes), .dxf and .svg (2D geometry). With doc_name the file is added to that open document; without it a new document named after the file is created. The reply lists the created objects with their names for get_object and update_object, the importer used, and objects that are invalid after the recompute. Undo removes the whole import in one step.

Mesh files become Mesh objects, not solids: call mesh_to_solid to turn one into a Part solid, or analyze_mesh and repair_mesh to fix it first. An SVG without absolute units and no recognised Inkscape version marker imports at an assumed 96 dpi rather than asking, with a warning in the reply saying so. Use open_document for .FCStd files.`

// importListCap bounds how many created or invalid objects the body lists, so
// a large assembly import keeps the reply small; the reply keys themselves
// are unaffected, only the body's readable listing.
const importListCap = 100

func (s *Server) registerImportTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "import_file",
		Description: importFileDescription,
		InputSchema: withPositiveMax(inputSchema[importFileInput](mergeDefaults(screenshotDefaults, map[string]string{"timeout": "300"})),
			"timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.importFile)
}

func (s *Server) importFile(ctx context.Context, _ *mcp.CallToolRequest, in importFileInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure("import file", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("import file", err, ""), nil, nil
	}
	docName := ""
	if in.DocName != nil {
		docName = *in.DocName
	}
	options := map[string]any{}
	if in.Merge != nil {
		options["merge"] = *in.Merge
	}
	if in.UseLinkGroup != nil {
		options["use_link_group"] = *in.UseLinkGroup
	}
	if in.ImportHidden != nil {
		options["import_hidden"] = *in.ImportHidden
	}

	res, err := conn.ImportFile(ctx, in.Path, docName, options, in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure("import file", err, largerTimeout("import_file"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("import file", res, "")), nil, nil
	}

	doc := str(res, "document")
	format := str(res, "format")
	importer := str(res, "importer")
	createdDocument, _ := res["created_document"].(bool)
	objectCount := intField(res, "object_count")
	transaction := str(res, "transaction")
	transactionMerged, _ := res["transaction_merged"].(bool)
	createdObjs, _ := res["created_objects"].([]any)
	invalidObjs, _ := res["invalid_objects"].([]any)
	warnings, _ := res["warnings"].([]any)
	rootObject := str(res, "root_object")

	front := importFileFront{
		Document:        doc,
		CreatedDocument: createdDocument,
		Format:          format,
		Importer:        importer,
		ObjectCount:     objectCount,
		Transaction:     transaction,
	}

	var body strings.Builder
	if createdDocument {
		fmt.Fprintf(&body, "Imported '.%s' into new document '%s' (%d object(s) created via %s).\n",
			format, doc, objectCount, importer)
	} else {
		fmt.Fprintf(&body, "Imported '.%s' into document '%s' (%d object(s) created via %s).\n",
			format, doc, objectCount, importer)
	}
	if rootObject != "" {
		fmt.Fprintf(&body, "Root object: %s\n", rootObject)
	}

	if len(createdObjs) > 0 {
		body.WriteString("\nCreated objects:\n")
		shown, more := capList(createdObjs, importListCap)
		for _, item := range shown {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			fmt.Fprintf(&body, "- %s (label %q, type %s)\n", str(obj, "name"), str(obj, "label"), str(obj, "type"))
		}
		if more > 0 {
			fmt.Fprintf(&body, "- and %d more (use list_objects with {\"doc_name\": %q}).\n", more, doc)
		}
	}

	if len(warnings) > 0 {
		body.WriteString("\nWarnings:\n")
		for _, w := range warnings {
			if msg, ok := w.(string); ok {
				fmt.Fprintf(&body, "- %s\n", msg)
			}
		}
	}

	if len(invalidObjs) > 0 {
		invalidTotal := invalidObjectsCount(res)
		fmt.Fprintf(&body, "\n%d object(s) invalid after recompute:\n", invalidTotal)
		shown, _ := capList(invalidObjs, importListCap)
		for _, item := range shown {
			if obj, ok := item.(map[string]any); ok {
				body.WriteString("- " + invalidObjectRow(obj, doc) + "\n")
			}
		}
		// The remainder is against invalidTotal (the addon's true count), not
		// len(invalidObjs): the addon may already have capped that list itself,
		// so counting against it here would understate how many were left out.
		if more := invalidTotal - len(shown); more > 0 {
			fmt.Fprintf(&body, "- and %d more (use list_objects with {\"doc_name\": %q}).\n", more, doc)
		}
	}

	fmt.Fprintf(&body, "\nCall list_objects with {\"doc_name\": %q} to see the document, or get_view with "+
		"{\"doc_name\": %q} for another screenshot.", doc, doc)

	out := render.SuccessResult(front, transactionNote(body.String(), transaction, transactionMerged))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), doc)), nil, nil
}

package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/mcp-wizard/render"
)

type importFileInput struct {
	Path         string   `json:"path"`
	DocName      *string  `json:"doc_name,omitempty"`
	Merge        *bool    `json:"merge,omitempty"`
	UseLinkGroup *bool    `json:"use_link_group,omitempty"`
	ImportHidden *bool    `json:"import_hidden,omitempty"`
	Timeout      *float64 `json:"timeout,omitempty"`
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

// importListCap bounds how many created or invalid objects the body lists, so
// a large assembly import keeps the reply small; the reply keys themselves
// are unaffected, only the body's readable listing.
const importListCap = 100

func (s *Server) registerImportTools() {
	addTool(s.mcpServer, "import_file",
		withPositiveMax(inputSchema[importFileInput](mergeDefaults(screenshotDefaults, map[string]string{"timeout": "300"})),
			"timeout", freecad.DefaultMaxExecuteCodeTime), s.importFile)
}

func (s *Server) importFile(ctx context.Context, _ *mcp.CallToolRequest, in importFileInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure(ctx, "import file", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "import file", err, ""), nil, nil
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
		return s.withNotice(timedFailure(ctx, "import file", err, largerTimeout("import_file"))), nil, nil
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

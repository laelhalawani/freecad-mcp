package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/mcp-wizard/render"
)

type recomputeDocumentInput struct {
	DocName string   `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	Timeout *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 120"`
	screenshotOptions
}

type recomputeFront struct {
	Document     string `yaml:"document"`
	Recomputed   int    `yaml:"recomputed"`
	ObjectCount  int    `yaml:"object_count"`
	InvalidCount int    `yaml:"invalid_count"`
	TouchedCount int    `yaml:"touched_count"`
}

func (s *Server) registerRecomputeTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "recompute_document",
		Description: "Recompute every object of a FreeCAD document and report each one that failed, with FreeCAD's " +
			"status message, and each that is still touched. Use it after a series of changes, after " +
			"delete_object (dependents may break), or when open_document reports that the document needs a " +
			"recompute. Fix a failed object with update_object or remove it with delete_object; failures do not " +
			"make the call fail.",
		InputSchema: withPositiveMax(inputSchema[recomputeDocumentInput](mergeDefaults(screenshotDefaults, map[string]string{"timeout": "120"})),
			"timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.recomputeDocument)
}

func (s *Server) recomputeDocument(ctx context.Context, _ *mcp.CallToolRequest, in recomputeDocumentInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure(ctx, "recompute document", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "recompute document", err, ""), nil, nil
	}
	res, err := conn.RecomputeDocument(ctx, in.DocName, in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure(ctx, "recompute document", err, largerTimeout("recompute_document"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("recompute document", res,
			"Call list_documents with {} to see the open documents.")), nil, nil
	}

	invalidObjs, _ := res["invalid_objects"].([]any)
	touchedObjs, _ := res["touched_objects"].([]any)
	objectCount := intField(res, "object_count")
	invalidCount := intField(res, "invalid_count")
	touchedCount := intField(res, "touched_count")
	front := recomputeFront{
		Document:     in.DocName,
		Recomputed:   intField(res, "recomputed"),
		ObjectCount:  objectCount,
		InvalidCount: invalidCount,
		TouchedCount: touchedCount,
	}

	invalidNames := make(map[string]bool, len(invalidObjs))
	var body strings.Builder
	if invalidCount == 0 {
		fmt.Fprintf(&body, "Document '%s' recomputed cleanly: %d object(s), none invalid.", in.DocName, objectCount)
	} else {
		fmt.Fprintf(&body, "Document '%s' recomputed with %d of %d object(s) invalid:\n", in.DocName, invalidCount, objectCount)
		for _, item := range invalidObjs {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			invalidNames[str(obj, "name")] = true
			body.WriteString("\n- " + invalidObjectRow(obj, in.DocName))
		}
		if boolField(res, "invalid_truncated") {
			fmt.Fprintf(&body, "\n\n(showing the first %d of %d; call recompute_document again after fixing some of these)", len(invalidObjs), invalidCount)
		}
	}
	if touchedCount > 0 {
		var names, extra []string
		for _, item := range touchedObjs {
			if n, ok := item.(string); ok {
				names = append(names, n)
				if !invalidNames[n] {
					extra = append(extra, n)
				}
			}
		}
		// A touched object was skipped during recompute because an upstream
		// dependency failed or otherwise blocked it (Document::recompute skips
		// the dependents of a failed object), or FreeCAD flags it as an
		// anomaly. object_validation treats "touched" as a failed state, so
		// every touched object is already in invalid_objects above, unless
		// the cap on that list (invalid_truncated) left it out; only mention
		// one here that is not accounted for by that.
		switch {
		case len(extra) == 0:
			fmt.Fprintf(&body, "\n\n%d object(s) are still touched after recompute: an upstream dependency failed "+
				"or otherwise blocked their recompute; each is listed above as invalid, so fix or remove the "+
				"blocking object listed there.", len(names))
		default:
			fmt.Fprintf(&body, "\n\n%d object(s) are still touched after recompute: an upstream dependency failed "+
				"or otherwise blocked their recompute; fix or remove the blocking object. Not listed above "+
				"(likely cut off by the invalid-objects cap): %s", len(names), strings.Join(extra, ", "))
		}
		if boolField(res, "touched_truncated") {
			fmt.Fprintf(&body, " (showing the first %d of %d touched)", len(touchedObjs), touchedCount)
		}
	}

	out := render.SuccessResult(front, body.String())
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), in.DocName)), nil, nil
}

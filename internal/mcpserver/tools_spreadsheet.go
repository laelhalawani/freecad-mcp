package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

// maxSpreadsheetCellWrites bounds one update_spreadsheet_cells call.
const maxSpreadsheetCellWrites = 500

type getSpreadsheetCellsInput struct {
	DocName   string   `json:"doc_name"`
	SheetName string   `json:"sheet_name"`
	Cells     []string `json:"cells,omitempty"`
}

type cellUpdate struct {
	Cell    string  `json:"cell"`
	Content *string `json:"content,omitempty"`
	Alias   *string `json:"alias,omitempty"`
}

type updateSpreadsheetCellsInput struct {
	DocName   string       `json:"doc_name"`
	SheetName string       `json:"sheet_name"`
	Cells     []cellUpdate `json:"cells"`
	Recompute *bool        `json:"recompute,omitempty"`
}

type getSpreadsheetCellsFront struct {
	Document  string `yaml:"document"`
	Sheet     string `yaml:"sheet"`
	Count     int    `yaml:"count"`
	Truncated bool   `yaml:"truncated"`
}

type updateSpreadsheetCellsFront struct {
	Document string `yaml:"document"`
	Sheet    string `yaml:"sheet"`
	Updated  int    `yaml:"updated"`
	// ErrorCount counts cells whose own row carries an error (a bad address or
	// a failing expression), not invalid objects: recompute_document names
	// that count invalid_count, so this does too.
	ErrorCount   int    `yaml:"error_count"`
	InvalidCount int    `yaml:"invalid_count"`
	Transaction  string `yaml:"transaction,omitempty"`
}

func (s *Server) registerSpreadsheetTools() {
	addTool(s.mcpServer, "get_spreadsheet_cells", inputSchema[getSpreadsheetCellsInput](nil), s.getSpreadsheetCells)
	addTool(s.mcpServer, "update_spreadsheet_cells",
		withItemRange(inputSchema[updateSpreadsheetCellsInput](map[string]string{"recompute": "true"}),
			"cells", 1, maxSpreadsheetCellWrites), s.updateSpreadsheetCells)
}

func (s *Server) getSpreadsheetCells(ctx context.Context, _ *mcp.CallToolRequest, in getSpreadsheetCellsInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "get spreadsheet cells", err, ""), nil, nil
	}
	res, err := conn.GetSpreadsheetCells(ctx, in.DocName, in.SheetName, in.Cells)
	if err != nil {
		return s.withNotice(failure(ctx, "get spreadsheet cells", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("get spreadsheet cells", res, "")), nil, nil
	}

	doc := str(res, "document")
	sheet := str(res, "sheet")
	count := intField(res, "count")
	truncated, _ := res["truncated"].(bool)
	rows, _ := res["cells"].([]any)

	front := getSpreadsheetCellsFront{Document: doc, Sheet: sheet, Count: count, Truncated: truncated}

	var body strings.Builder
	if count == 0 {
		fmt.Fprintf(&body, "Sheet '%s' of document '%s' has no matching cells.", sheet, doc)
	} else {
		fmt.Fprintf(&body, "%d cell(s) of sheet '%s' in document '%s':\n\n", count, sheet, doc)
		writeCellTable(&body, rows)
		if truncated {
			fmt.Fprintf(&body, "\nTruncated at %d cells; narrow the cells argument to see the rest.", count)
		}
	}

	return s.withNotice(render.SuccessResult(front, body.String())), nil, nil
}

func (s *Server) updateSpreadsheetCells(ctx context.Context, _ *mcp.CallToolRequest, in updateSpreadsheetCellsInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "update spreadsheet cells", err, ""), nil, nil
	}
	cells := make([]map[string]any, len(in.Cells))
	for i, c := range in.Cells {
		m := map[string]any{"cell": c.Cell}
		if c.Content != nil {
			m["content"] = *c.Content
		}
		if c.Alias != nil {
			m["alias"] = *c.Alias
		}
		cells[i] = m
	}

	res, err := conn.UpdateSpreadsheetCells(ctx, in.DocName, in.SheetName, cells, boolOr(in.Recompute, true))
	if err != nil {
		return s.withNotice(failure(ctx, "update spreadsheet cells", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("update spreadsheet cells", res, "")), nil, nil
	}

	doc := str(res, "document")
	sheet := str(res, "sheet")
	updated, _ := res["updated"].([]any)
	rows, _ := res["cells"].([]any)
	invalidObjs, _ := res["invalid_objects"].([]any)
	transactionName := str(res, "transaction")
	merged, _ := res["transaction_merged"].(bool)

	front := updateSpreadsheetCellsFront{
		Document:     doc,
		Sheet:        sheet,
		Updated:      len(updated),
		ErrorCount:   countCellErrors(rows),
		InvalidCount: invalidObjectsCount(res),
		Transaction:  transactionName,
	}

	var body strings.Builder
	fmt.Fprintf(&body, "Updated %d cell(s) of sheet '%s' in document '%s'.\n", len(updated), sheet, doc)
	if len(rows) > 0 {
		body.WriteString("\n")
		writeCellTable(&body, rows)
		// The content column is FreeCAD's stored text, which marks what kind
		// of value a cell holds: a number with a unit is stored as "=5 mm".
		body.WriteString("\nIn the content column FreeCAD marks an expression or a number with a unit with a leading =, " +
			"and text with a leading '; a plain number has no mark.\n")
	}
	if len(invalidObjs) == 0 {
		body.WriteString("\nNo objects became invalid.")
	} else {
		body.WriteString(invalidObjectsBody(res, doc))
		// A sheet invalid because its own cells failed to evaluate is not
		// fixed by update_object (there is no property to set): point at the
		// cells themselves instead.
		if errorCells, ok := res["error_cells"].([]any); ok && len(errorCells) > 0 {
			fmt.Fprintf(&body, "\n\nSheet '%s' has %d erroring cell(s): %s. Call get_spreadsheet_cells with "+
				"{\"doc_name\": %q, \"sheet_name\": %q, \"cells\": %s} to see their error messages.",
				sheet, len(errorCells), strings.Join(stringItems(errorCells), ", "), doc, sheet, jsonStrings(errorCells))
		}
	}

	return s.withNotice(render.SuccessResult(front, transactionNote(body.String(), transactionName, merged))), nil, nil
}

// countCellErrors counts get_spreadsheet_cells-shaped rows that carry a
// non-empty "error" (a bad address or a failing expression), distinct from
// invalid_objects, which lists document objects invalid after recompute.
func countCellErrors(rows []any) int {
	count := 0
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if str(row, "error") != "" {
			count++
		}
	}
	return count
}

// writeCellTable renders get_spreadsheet_cells-shaped rows as a Markdown
// table (cell, alias, content, value, error), each field escaped so a pipe
// or a newline (FreeCAD's own error messages can run to two lines) cannot
// break the table; error is "" for a row that has none.
func writeCellTable(body *strings.Builder, rows []any) {
	body.WriteString("| cell | alias | content | value | error |\n|---|---|---|---|---|\n")
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		value := ""
		if v := row["value"]; v != nil {
			value = fmt.Sprint(v)
		}
		fmt.Fprintf(body, "| %s | %s | %s | %s | %s |\n", mdCell(str(row, "cell")), mdCell(str(row, "alias")),
			mdCell(str(row, "content")), mdCell(value), mdCell(str(row, "error")))
	}
}

// mdCell escapes a value for one Markdown table cell.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

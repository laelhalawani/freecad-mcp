package freecad

import "context"

// GetSpreadsheetCells reads cells of a sheet; cells nil means every non-empty
// cell.
func (c *Connection) GetSpreadsheetCells(ctx context.Context, doc, sheet string, cells []string) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "get_spreadsheet_cells", doc, sheet, optList(cells))
}

// UpdateSpreadsheetCells sets contents and aliases of cells of a sheet. Each
// cell is {"cell", "content"?, "alias"?}.
func (c *Connection) UpdateSpreadsheetCells(ctx context.Context, doc, sheet string, cells []map[string]any, recompute bool) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "update_spreadsheet_cells", doc, sheet, cells, recompute)
}

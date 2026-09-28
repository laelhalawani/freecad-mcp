package freecad

import "context"

// DefaultExportTimeout is export_document's run budget in seconds.
const DefaultExportTimeout = 300.0

// ExportDocument writes objects of doc to path in the format of its
// extension. options holds the given export options (object_names,
// overwrite, include_hidden, recompute, tessellation and format-specific
// keys such as ascii or step_unit); nil uses the addon's defaults.
func (c *Connection) ExportDocument(ctx context.Context, doc, path string, options map[string]any, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "export_document", timeout, DefaultExportTimeout, doc, path, optMap(options))
}

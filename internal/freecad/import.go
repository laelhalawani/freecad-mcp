package freecad

import "context"

// DefaultImportTimeout is import_file's run budget in seconds.
const DefaultImportTimeout = 300.0

// ImportFile imports path into doc, or into a new document named after the
// file when doc is "". options holds merge, use_link_group and import_hidden
// when given.
func (c *Connection) ImportFile(ctx context.Context, path, doc string, options map[string]any, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "import_file", timeout, DefaultImportTimeout, path, optString(doc), optMap(options))
}

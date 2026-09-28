package freecad

import "context"

// DefaultSaveTimeout is the run budget in seconds of save_document and
// save_document_as.
const DefaultSaveTimeout = 120.0

// SaveDocument saves a document to its own file.
func (c *Connection) SaveDocument(ctx context.Context, doc string, recompute bool, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "save_document", timeout, DefaultSaveTimeout, doc, recompute)
}

// SaveDocumentAs saves a document to path, or writes a copy there.
func (c *Connection) SaveDocumentAs(ctx context.Context, doc, path string, overwrite, copyOnly, recompute bool, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "save_document_as", timeout, DefaultSaveTimeout, doc, path, overwrite, copyOnly, recompute)
}

// CloseDocument closes a document; unsaved changes are refused unless
// discardChanges is set.
func (c *Connection) CloseDocument(ctx context.Context, doc string, discardChanges bool) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "close_document", doc, discardChanges)
}

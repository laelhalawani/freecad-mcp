package freecad

import "context"

// DefaultOpenTimeout is open_document's run budget in seconds.
const DefaultOpenTimeout = 120.0

// GetDocuments lists the open documents with their file, state and views, and
// names the active one.
func (c *Connection) GetDocuments(ctx context.Context) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "get_documents")
}

// OpenDocument opens an .FCStd file, or returns the document that already has
// it open.
func (c *Connection) OpenDocument(ctx context.Context, path string, hidden, activate bool, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "open_document", timeout, DefaultOpenTimeout, path, hidden, activate)
}

// ActivateDocument makes a document, and optionally one of its views, the
// active one. viewIndex nil means the document's active view.
func (c *Connection) ActivateDocument(ctx context.Context, doc string, viewIndex *int, createView bool) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "activate_document", doc, opt(viewIndex), createView)
}

package freecad

import "context"

// Undo undoes up to steps transactions of a document.
func (c *Connection) Undo(ctx context.Context, doc string, steps int) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "undo", doc, steps)
}

// Redo redoes up to steps transactions of a document.
func (c *Connection) Redo(ctx context.Context, doc string, steps int) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "redo", doc, steps)
}

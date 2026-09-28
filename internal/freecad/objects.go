package freecad

import "context"

// GetObjects lists the objects of a document: every property of each, or
// with compact one short row per object (name, label, type, state, validity,
// parent, visibility). compact is sent only when set, so the full list keeps
// working with addons that predate it.
func (c *Connection) GetObjects(ctx context.Context, doc string, compact bool) (any, error) {
	if compact {
		return c.call(ctx, c.timeout, "get_objects", doc, true)
	}
	return c.call(ctx, c.timeout, "get_objects", doc)
}

// GetObject returns one object, or nil when the document or object does not
// exist.
func (c *Connection) GetObject(ctx context.Context, doc, obj string) (any, error) {
	return c.call(ctx, c.timeout, "get_object", doc, obj)
}

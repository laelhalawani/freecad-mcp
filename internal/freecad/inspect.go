package freecad

import "context"

// Measure measures kind (distance, angle, length, radius, area, volume) over
// refs, each {"object", "sub"?}.
func (c *Connection) Measure(ctx context.Context, doc, kind string, refs []map[string]any) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "measure", doc, kind, refs)
}

// ListSubelements lists the faces, edges or both (kind "faces", "edges" or
// "all") of obj with their names and geometry.
func (c *Connection) ListSubelements(ctx context.Context, doc, obj, kind string) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "list_subelements", doc, obj, kind)
}

// GetSelection returns the selection of doc, or of every document when doc
// is "".
func (c *Connection) GetSelection(ctx context.Context, doc string) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "get_selection", optString(doc))
}

package freecad

import "context"

// ReleaseSession frees the session lock this session holds (release_session).
func (c *Connection) ReleaseSession(ctx context.Context) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "release_session")
}

// CloseFreeCAD closes every document and quits FreeCAD (close_freecad).
// Unsaved changes are refused unless discardChanges is set.
func (c *Connection) CloseFreeCAD(ctx context.Context, discardChanges bool) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "close_freecad", discardChanges)
}

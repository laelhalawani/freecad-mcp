package freecad

import "context"

// SetView changes what the user sees in doc's 3D view (the active document
// when doc is ""): orientation, framing, visibility, transparency, display
// modes and an animated mode. options holds the given set_view keys.
func (c *Connection) SetView(ctx context.Context, doc string, options map[string]any) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "set_view", optString(doc), options)
}

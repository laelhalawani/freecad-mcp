package freecad

import "context"

// DefaultRecomputeTimeout is recompute_document's run budget in seconds.
const DefaultRecomputeTimeout = 120.0

// RecomputeDocument recomputes a document and reports its invalid and touched
// objects.
func (c *Connection) RecomputeDocument(ctx context.Context, doc string, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "recompute_document", timeout, DefaultRecomputeTimeout, doc)
}

package freecad

import "context"

// DefaultPrintabilityTimeout is check_printability's run budget in seconds.
const DefaultPrintabilityTimeout = 120.0

// CheckPrintability checks the layout of parts of doc on a plate: each part
// inside it, no two overlapping. objectNames nil means the visible top-level
// solids; options holds "bed", [x, y] or [x, y, z] in mm.
func (c *Connection) CheckPrintability(ctx context.Context, doc string, objectNames []string, options map[string]any, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "check_printability", timeout, DefaultPrintabilityTimeout, doc, optList(objectNames), optMap(options))
}

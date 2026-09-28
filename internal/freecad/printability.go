package freecad

import "context"

// DefaultPrintabilityTimeout is check_printability's run budget in seconds.
const DefaultPrintabilityTimeout = 300.0

// CheckPrintability checks objects of doc for 3D printing. objectNames nil
// means the visible top-level solids and meshes; options holds the given
// check options (bed, build_direction, overhang_angle_deg,
// check_self_intersections, tessellation keys); nil uses the addon's
// defaults.
func (c *Connection) CheckPrintability(ctx context.Context, doc string, objectNames []string, options map[string]any, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "check_printability", timeout, DefaultPrintabilityTimeout, doc, optList(objectNames), optMap(options))
}

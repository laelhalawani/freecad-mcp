package freecad

import "context"

// Run budgets in seconds of the mesh tools.
const (
	DefaultAnalyzeMeshTimeout = 120.0
	DefaultRepairMeshTimeout  = 300.0
	DefaultConvertMeshTimeout = 300.0
)

// AnalyzeMesh reports the defects of a mesh object.
func (c *Connection) AnalyzeMesh(ctx context.Context, doc, obj string, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "analyze_mesh", timeout, DefaultAnalyzeMeshTimeout, doc, obj)
}

// RepairMesh runs repair steps on a mesh object; steps nil means the default
// steps.
func (c *Connection) RepairMesh(ctx context.Context, doc, obj string, steps []string, options map[string]any, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "repair_mesh", timeout, DefaultRepairMeshTimeout, doc, obj, optList(steps), optMap(options))
}

// MeshToSolid creates a Part solid from a mesh object.
func (c *Connection) MeshToSolid(ctx context.Context, doc, obj string, options map[string]any, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "mesh_to_solid", timeout, DefaultConvertMeshTimeout, doc, obj, optMap(options))
}

// SolidToMesh creates a mesh object from an object's shape.
func (c *Connection) SolidToMesh(ctx context.Context, doc, obj string, options map[string]any, timeout *float64) (map[string]any, error) {
	return c.callBudget(ctx, "solid_to_mesh", timeout, DefaultConvertMeshTimeout, doc, obj, optMap(options))
}

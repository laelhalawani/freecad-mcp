package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/mcp-wizard/render"
)

// meshRepairSteps are the steps repair_mesh accepts, as the addon names them
// (REPAIR_STEPS in mesh_tools.py).
var meshRepairSteps = []string{
	"fix_indices", "remove_invalid_points", "remove_duplicated_points", "remove_duplicated_facets",
	"fix_degenerations", "fix_deformations", "remove_non_manifolds", "remove_non_manifold_points",
	"fix_self_intersections", "remove_folds", "harmonize_normals", "flip_normals", "fill_holes",
}

type analyzeMeshInput struct {
	DocName string   `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	ObjName string   `json:"obj_name" jsonschema:"the name of a mesh object (Mesh::Feature), as list_objects shows it"`
	Timeout *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 120"`
}

type repairMeshInput struct {
	DocName           string   `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	ObjName           string   `json:"obj_name" jsonschema:"the name of a mesh object (Mesh::Feature), as list_objects shows it"`
	Steps             []string `json:"steps,omitempty" jsonschema:"repair steps to run in this order (default: fix_indices, remove_duplicated_points, remove_duplicated_facets, fix_degenerations, remove_non_manifolds, fix_self_intersections, harmonize_normals, fill_holes)"`
	FillHolesMaxEdges *int     `json:"fill_holes_max_edges,omitempty" jsonschema:"fill_holes closes holes bounded by at most this many edges, 3 to 10000 (default 20)"`
	Timeout           *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 300"`
	screenshotOptions
}

type meshToSolidInput struct {
	DocName    string   `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	ObjName    string   `json:"obj_name" jsonschema:"the name of a mesh object (Mesh::Feature), as list_objects shows it"`
	ResultName *string  `json:"result_name,omitempty" jsonschema:"name for the new Part object (default: obj_name followed by _solid)"`
	Tolerance  *float64 `json:"tolerance,omitempty" jsonschema:"distance in mm within which mesh edges are sewn together, more than 0 and at most 10 (default 0.1)"`
	Refine     *bool    `json:"refine,omitempty" jsonschema:"merge coplanar triangles into larger faces, slower but lighter to model on (default false)"`
	Force      *bool    `json:"force,omitempty" jsonschema:"convert meshes with more than 200000 facets, which can take minutes and much memory (default false)"`
	Timeout    *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 300"`
	screenshotOptions
}

type solidToMeshInput struct {
	DocName              string   `json:"doc_name" jsonschema:"the name of an open document, as list_documents shows it"`
	ObjName              string   `json:"obj_name" jsonschema:"the name of an object with a shape, as list_objects shows it"`
	ResultName           *string  `json:"result_name,omitempty" jsonschema:"name for the new mesh object (default: obj_name followed by _mesh)"`
	Quality              *string  `json:"quality,omitempty" jsonschema:"tessellation preset; coarse for previews, standard for FDM prints, fine for resin and small curved parts (default standard)"`
	LinearDeflection     *float64 `json:"linear_deflection,omitempty" jsonschema:"largest distance in mm between the surface and its triangles, 0.001 to 100; overrides the preset"`
	AngularDeflectionDeg *float64 `json:"angular_deflection_deg,omitempty" jsonschema:"largest angle in degrees between neighbouring triangles, 0.5 to 90; overrides the preset"`
	Relative             *bool    `json:"relative,omitempty" jsonschema:"linear_deflection is relative to each edge's length (default false)"`
	Timeout              *float64 `json:"timeout,omitempty" jsonschema:"seconds for each of the queue and GUI execution budgets, more than 0 and at most 1800; default 300"`
	screenshotOptions
}

type analyzeMeshFront struct {
	Document   string `yaml:"document"`
	Object     string `yaml:"object"`
	Points     int    `yaml:"points"`
	Facets     int    `yaml:"facets"`
	IsSolid    bool   `yaml:"is_solid"`
	IssueCount int    `yaml:"issue_count"`
}

type repairMeshFront struct {
	Document     string `yaml:"document"`
	Object       string `yaml:"object"`
	FacetsBefore int    `yaml:"facets_before"`
	FacetsAfter  int    `yaml:"facets_after"`
	IsSolid      bool   `yaml:"is_solid"`
	Transaction  string `yaml:"transaction,omitempty"`
}

type meshToSolidFront struct {
	Document    string `yaml:"document"`
	Source      string `yaml:"source"`
	ObjectName  string `yaml:"object_name"`
	IsSolid     bool   `yaml:"is_solid"`
	Faces       int    `yaml:"faces"`
	Transaction string `yaml:"transaction,omitempty"`
}

type solidToMeshFront struct {
	Document    string `yaml:"document"`
	Source      string `yaml:"source"`
	ObjectName  string `yaml:"object_name"`
	Facets      int    `yaml:"facets"`
	IsSolid     bool   `yaml:"is_solid"`
	Transaction string `yaml:"transaction,omitempty"`
}

func (s *Server) registerMeshTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "analyze_mesh",
		Description: "Analyze a mesh object (Mesh::Feature, for example an imported STL or 3MF) for the defects that " +
			"break 3D printing and mesh_to_solid: whether it is a closed solid, non-manifold edges, " +
			"self-intersections, wrongly oriented facets, invalid points, corrupted facets and separate " +
			"components. The reply lists the issues and the repair_mesh steps that address them; the document " +
			"is not changed.",
		InputSchema: withPositiveMax(inputSchema[analyzeMeshInput](map[string]string{"timeout": "120"}),
			"timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.analyzeMesh)

	repair := inputSchema[repairMeshInput](mergeDefaults(screenshotDefaults, map[string]string{"fill_holes_max_edges": "20", "timeout": "300"}))
	repair = withEnum(repair, "steps", meshRepairSteps...)
	repair = withRange(repair, 3, 10000, "fill_holes_max_edges")
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "repair_mesh",
		Description: "Repair a mesh object in place by running named repair steps in order, such as removing " +
			"duplicated points and facets, non-manifold edges and self-intersections, harmonizing normals and " +
			"filling holes. Run analyze_mesh first and pass the steps it suggests, or omit steps for a standard " +
			"sequence. The reply compares the mesh before and after; undo reverts the repair in one step.",
		InputSchema: withPositiveMax(repair, "timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.repairMesh)

	toSolid := inputSchema[meshToSolidInput](mergeDefaults(screenshotDefaults, map[string]string{
		"tolerance": "0.1", "refine": "false", "force": "false", "timeout": "300"}))
	toSolid = withPositiveMax(toSolid, "tolerance", 10)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "mesh_to_solid",
		Description: "Convert a mesh object, such as an imported STL, into a new Part solid that booleans, fillets " +
			"and export to STEP can work with. Each triangle becomes a face, so large meshes are slow: more than " +
			"200000 facets need force true, and refine merges coplanar faces. A mesh that is not closed gives a " +
			"shell, not a solid; run analyze_mesh and repair_mesh first. The mesh object is kept.",
		InputSchema: withPositiveMax(toSolid, "timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.meshToSolid)

	toMesh := inputSchema[solidToMeshInput](mergeDefaults(screenshotDefaults, map[string]string{
		"quality": `"standard"`, "relative": "false", "timeout": "300"}))
	toMesh = withEnum(toMesh, "quality", "coarse", "standard", "fine")
	toMesh = withRange(toMesh, 0.001, 100, "linear_deflection")
	toMesh = withRange(toMesh, 0.5, 90, "angular_deflection_deg")
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "solid_to_mesh",
		Description: "Tessellate an object's shape (a Part feature, Body, Link or App::Part) into a new mesh " +
			"object with explicit quality: a preset or linear_deflection and angular_deflection_deg. Use it to " +
			"inspect or repair the triangles a printer will get with analyze_mesh; export_document tessellates " +
			"by itself, so this is not needed before exporting. The source object is kept.",
		InputSchema: withPositiveMax(toMesh, "timeout", freecad.DefaultMaxExecuteCodeTime),
	}, s.solidToMesh)
}

// solidWord renders a mesh's isSolid flag for a reply's body text.
func solidWord(isSolid bool) string {
	if isSolid {
		return "a closed solid"
	}
	return "not a closed solid"
}

func (s *Server) analyzeMesh(ctx context.Context, _ *mcp.CallToolRequest, in analyzeMeshInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure("analyze mesh", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("analyze mesh", err, ""), nil, nil
	}
	res, err := conn.AnalyzeMesh(ctx, in.DocName, in.ObjName, in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure("analyze mesh", err, largerTimeout("analyze_mesh"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("analyze mesh", res,
			fmt.Sprintf("Call list_objects with {\"doc_name\": %q} to see the mesh objects.", in.DocName))), nil, nil
	}

	object := str(res, "object")
	points := intField(res, "points")
	facets := intField(res, "facets")
	isSolid := boolField(res, "is_solid")
	issues, _ := res["issues"].([]any)
	steps, _ := res["suggested_steps"].([]any)

	var body strings.Builder
	if len(issues) == 0 {
		fmt.Fprintf(&body, "Mesh '%s' has no defects: %d points, %d facets, %s.", object, points, facets, solidWord(isSolid))
	} else {
		fmt.Fprintf(&body, "Mesh '%s': %d points, %d facets, %s. %d issue(s):\n", object, points, facets, solidWord(isSolid), len(issues))
		for _, item := range issues {
			if text, ok := item.(string); ok {
				fmt.Fprintf(&body, "\n- %s", text)
			}
		}
	}
	if len(steps) > 0 {
		fmt.Fprintf(&body, "\n\nCall repair_mesh with {\"doc_name\": %q, \"obj_name\": %q, \"steps\": %s} to fix them.",
			in.DocName, in.ObjName, jsonStrings(steps))
	}

	front := analyzeMeshFront{
		Document: in.DocName, Object: object, Points: points, Facets: facets,
		IsSolid: isSolid, IssueCount: len(issues),
	}
	return s.withNotice(render.SuccessResult(front, body.String())), nil, nil
}

func (s *Server) repairMesh(ctx context.Context, _ *mcp.CallToolRequest, in repairMeshInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure("repair mesh", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("repair mesh", err, ""), nil, nil
	}
	options := map[string]any{}
	if in.FillHolesMaxEdges != nil {
		options["fill_holes_max_edges"] = *in.FillHolesMaxEdges
	}
	res, err := conn.RepairMesh(ctx, in.DocName, in.ObjName, in.Steps, options, in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure("repair mesh", err, largerTimeout("repair_mesh"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("repair mesh", res,
			fmt.Sprintf("Call analyze_mesh with {\"doc_name\": %q, \"obj_name\": %q} to see its defects.", in.DocName, in.ObjName))), nil, nil
	}

	object := str(res, "object")
	before, _ := res["before"].(map[string]any)
	after, _ := res["after"].(map[string]any)
	stepReports, _ := res["steps"].([]any)
	issues, _ := res["issues"].([]any)
	afterIsSolid := boolField(after, "is_solid")
	txName, txMerged := transactionFields(res)

	var body strings.Builder
	fmt.Fprintf(&body, "Mesh '%s' repaired: %d -> %d facets, %s.",
		object, intField(before, "facets"), intField(after, "facets"), solidWord(afterIsSolid))
	for _, item := range stepReports {
		step, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fmt.Fprintf(&body, "\n- %s: %d -> %d facets", str(step, "step"), intField(step, "facets_before"), intField(step, "facets_after"))
	}
	if len(issues) > 0 {
		body.WriteString("\n\nRemaining issues:")
		for _, item := range issues {
			if text, ok := item.(string); ok {
				fmt.Fprintf(&body, "\n- %s", text)
			}
		}
	}

	front := repairMeshFront{
		Document: in.DocName, Object: object,
		FacetsBefore: intField(before, "facets"), FacetsAfter: intField(after, "facets"),
		IsSolid: afterIsSolid, Transaction: txName,
	}
	out := render.SuccessResult(front, transactionNote(body.String(), txName, txMerged))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), in.DocName)), nil, nil
}

func (s *Server) meshToSolid(ctx context.Context, _ *mcp.CallToolRequest, in meshToSolidInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure("convert mesh to solid", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("convert mesh to solid", err, ""), nil, nil
	}
	options := map[string]any{}
	if in.ResultName != nil {
		options["result_name"] = *in.ResultName
	}
	if in.Tolerance != nil {
		options["tolerance"] = *in.Tolerance
	}
	if in.Refine != nil {
		options["refine"] = *in.Refine
	}
	if in.Force != nil {
		options["force"] = *in.Force
	}
	res, err := conn.MeshToSolid(ctx, in.DocName, in.ObjName, options, in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure("convert mesh to solid", err, largerTimeout("mesh_to_solid"))), nil, nil
	}
	if !succeeded(res) {
		hint := fmt.Sprintf("Call analyze_mesh with {\"doc_name\": %q, \"obj_name\": %q} to see its defects.", in.DocName, in.ObjName)
		if code, _ := res["code"].(string); code == render.CodeConflict {
			hint = fmt.Sprintf("Call mesh_to_solid again with {\"doc_name\": %q, \"obj_name\": %q, \"force\": true}.", in.DocName, in.ObjName)
		}
		return s.withNotice(reportedCode("convert mesh to solid", res, hint, render.CodeConflict)), nil, nil
	}

	source := str(res, "object")
	name, label, _ := objRef(res, "created_object")
	isSolid := boolField(res, "is_solid")
	faces := intField(res, "faces")
	warnings, _ := res["warnings"].([]any)
	txName, txMerged := transactionFields(res)

	var body strings.Builder
	fmt.Fprintf(&body, "Created '%s' (%s) from mesh '%s': %d face(s), %s.", name, label, source, faces, solidWord(isSolid))
	if isSolid {
		if vol, ok := number(res["volume"]); ok {
			fmt.Fprintf(&body, " Volume %.4g mm^3.", vol)
		}
	}
	for _, item := range warnings {
		if text, ok := item.(string); ok {
			fmt.Fprintf(&body, "\n- %s", text)
		}
	}

	front := meshToSolidFront{
		Document: in.DocName, Source: source, ObjectName: name,
		IsSolid: isSolid, Faces: faces, Transaction: txName,
	}
	out := render.SuccessResult(front, transactionNote(body.String(), txName, txMerged))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), in.DocName)), nil, nil
}

func (s *Server) solidToMesh(ctx context.Context, _ *mcp.CallToolRequest, in solidToMeshInput) (*mcp.CallToolResult, any, error) {
	if err := freecad.CheckTimeout(in.Timeout); err != nil {
		return failure("convert solid to mesh", err, ""), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("convert solid to mesh", err, ""), nil, nil
	}
	options := map[string]any{}
	if in.ResultName != nil {
		options["result_name"] = *in.ResultName
	}
	if in.Quality != nil {
		options["quality"] = *in.Quality
	}
	if in.LinearDeflection != nil {
		options["linear_deflection"] = *in.LinearDeflection
	}
	if in.AngularDeflectionDeg != nil {
		options["angular_deflection_deg"] = *in.AngularDeflectionDeg
	}
	if in.Relative != nil {
		options["relative"] = *in.Relative
	}
	res, err := conn.SolidToMesh(ctx, in.DocName, in.ObjName, options, in.Timeout)
	if err != nil {
		return s.withNotice(timedFailure("convert solid to mesh", err, largerTimeout("solid_to_mesh"))), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("convert solid to mesh", res,
			fmt.Sprintf("Call get_object with {\"doc_name\": %q, \"obj_name\": %q} to see the object.", in.DocName, in.ObjName))), nil, nil
	}

	source := str(res, "object")
	name, label, _ := objRef(res, "created_object")
	facets := intField(res, "facets")
	isSolid := boolField(res, "is_solid")
	txName, txMerged := transactionFields(res)

	var body strings.Builder
	fmt.Fprintf(&body, "Created mesh '%s' (%s) from '%s': %d points, %d facets, %s.",
		name, label, source, intField(res, "points"), facets, solidWord(isSolid))

	front := solidToMeshFront{
		Document: in.DocName, Source: source, ObjectName: name,
		Facets: facets, IsSolid: isSolid, Transaction: txName,
	}
	out := render.SuccessResult(front, transactionNote(body.String(), txName, txMerged))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), in.DocName)), nil, nil
}

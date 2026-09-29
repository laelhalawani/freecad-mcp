//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// quiet keeps a call's reply text-only.
func quiet(m map[string]any) map[string]any {
	m["include_screenshot"] = false
	return m
}

// TestFailedCreateLeavesNothing: an error while setting a property creates no object and the
// error says so.
func TestFailedCreateLeavesNothing(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2EFail")
	fails(t, call(t, cs, "create_object", quiet(map[string]any{"doc_name": "E2EFail", "obj_name": "Box",
		"obj_type": "Part::Box", "obj_properties": map[string]any{"Nosuch": 1}})), "Nothing was created")
	must(t, call(t, cs, "list_objects", map[string]any{"doc_name": "E2EFail", "compact": true}), "count: 0")
}

// TestForceLoadReplies: quantities echo with units, and a force load states its direction and
// Reversed flips it.
func TestForceLoadReplies(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2ELoad")
	must(t, call(t, cs, "create_object", quiet(map[string]any{"doc_name": "E2ELoad", "obj_name": "Box",
		"obj_type": "Part::Box"})), "object_name: Box")
	// Box face 6 is the top face, whose outward normal is +Z.
	must(t, call(t, cs, "create_object", quiet(map[string]any{"doc_name": "E2ELoad", "obj_name": "Load",
		"obj_type": "Fem::ConstraintForce", "obj_properties": map[string]any{
			"References": []any{map[string]any{"object_name": "Box", "face": "Face6"}}, "Force": "100 N"}})),
		"Force: 100.00 N", "acts along (0, 0, 1): the outward normal of Face6")
	must(t, call(t, cs, "update_object", quiet(map[string]any{"doc_name": "E2ELoad", "obj_name": "Load",
		"obj_properties": map[string]any{"Reversed": true}})),
		"acts along (0, 0, -1): reversed, against the outward normal of Face6")
}

// TestDottedPlacement: a dotted name sets one part of a Placement, or binds an expression on
// it, and a bare "=" removes the binding.
func TestDottedPlacement(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2EDot")
	must(t, call(t, cs, "create_object", quiet(map[string]any{"doc_name": "E2EDot", "obj_name": "Box",
		"obj_type": "Part::Box"})), "object_name: Box")
	read := func() reply {
		return call(t, cs, "execute_code", quiet(map[string]any{"code": "b = FreeCAD.getDocument('E2EDot').getObject('Box')\n" +
			"print('z=%s bound=%d' % (b.Placement.Base.z, len(b.ExpressionEngine)))"}))
	}
	must(t, call(t, cs, "update_object", quiet(map[string]any{"doc_name": "E2EDot", "obj_name": "Box",
		"obj_properties": map[string]any{"Placement.Base.z": 7}})), "updated successfully")
	must(t, read(), "z=7.0 bound=0")

	must(t, call(t, cs, "create_object", quiet(map[string]any{"doc_name": "E2EDot", "obj_name": "Params",
		"obj_type": "Spreadsheet::Sheet"})), "object_name: Params")
	must(t, call(t, cs, "update_spreadsheet_cells", map[string]any{"doc_name": "E2EDot", "sheet_name": "Params",
		"cells": []any{map[string]any{"cell": "A1", "content": "5 mm", "alias": "thickness"}}}), "Updated 1 cell(s)")
	must(t, call(t, cs, "update_object", quiet(map[string]any{"doc_name": "E2EDot", "obj_name": "Box",
		"obj_properties": map[string]any{"Placement.Base.z": "=Params.thickness"}})), "updated successfully")
	must(t, read(), "z=5.0 bound=1")
	must(t, call(t, cs, "update_object", quiet(map[string]any{"doc_name": "E2EDot", "obj_name": "Box",
		"obj_properties": map[string]any{"Placement.Base.z": "="}})), "updated successfully")
	must(t, read(), "bound=0")
}

// TestMaterialNumbersAccepted: a number in a string map such as a FEM material is stored as
// its text.
func TestMaterialNumbersAccepted(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2EMat")
	must(t, call(t, cs, "create_object", quiet(map[string]any{"doc_name": "E2EMat", "obj_name": "Steel",
		"obj_type": "Fem::MaterialCommon", "obj_properties": map[string]any{"Material": map[string]any{
			"Name": "Steel", "YoungsModulus": "210 GPa", "PoissonRatio": 0.3}}})), "object_name: Steel")
	must(t, call(t, cs, "execute_code", quiet(map[string]any{"code": "m = FreeCAD.getDocument('E2EMat').getObject('Steel').Material\n" +
		"print('poisson=' + m['PoissonRatio'] + ';')"})), "poisson=0.3;")
}

// TestSolidCountOfFusedBoxes: two separate boxes fused are one Compound of two solids.
func TestSolidCountOfFusedBoxes(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2ESolids")
	for i, x := range []int{0, 50} {
		must(t, call(t, cs, "create_object", quiet(map[string]any{"doc_name": "E2ESolids", "obj_name": []string{"A", "B"}[i],
			"obj_type": "Part::Box", "obj_properties": map[string]any{"Placement": map[string]any{
				"Base": map[string]any{"x": x, "y": 0, "z": 0}}}})), "created successfully")
	}
	must(t, call(t, cs, "create_object", quiet(map[string]any{"doc_name": "E2ESolids", "obj_name": "Fuse",
		"obj_type": "Part::MultiFuse", "obj_properties": map[string]any{"Shapes": []any{"A", "B"}}})), "object_name: Fuse")
	must(t, call(t, cs, "get_object", map[string]any{"doc_name": "E2ESolids", "obj_name": "Fuse"}), `"SolidCount": 2`)
}

// TestMissingFoldersAreCreated: export_document and save_document_as create the missing
// folders of their path and say so.
func TestMissingFoldersAreCreated(t *testing.T) {
	cs := session(t)
	stl := tempPath(t, "out/models/box.stl")
	fcstd := tempPath(t, "saved/deeper/model.FCStd")
	newDoc(t, cs, "E2EFolders")
	must(t, call(t, cs, "create_object", quiet(map[string]any{"doc_name": "E2EFolders", "obj_name": "Box",
		"obj_type": "Part::Box"})), "object_name: Box")

	must(t, call(t, cs, "export_document", map[string]any{"doc_name": "E2EFolders", "path": stl}),
		"did not exist, so it was created")
	if _, err := os.Stat(stl); err != nil {
		t.Fatalf("export wrote no file: %v", err)
	}

	must(t, call(t, cs, "save_document_as", map[string]any{"doc_name": "E2EFolders", "path": fcstd}),
		"did not exist, so it was created")
	if _, err := os.Stat(filepath.FromSlash(fcstd)); err != nil {
		t.Fatalf("save_document_as wrote no file: %v", err)
	}
}

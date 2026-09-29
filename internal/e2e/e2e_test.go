//go:build e2e

// Package e2e drives a built freecad-mcp binary against a running FreeCAD.
//
// Start FreeCAD with the addon's RPC server running, build the binary, then:
//
//	FREECAD_MCP_E2E_BINARY=/path/to/freecad-mcp go test -tags e2e -v ./internal/e2e/
//
// The tests create documents with E2E names and close them, unsaved, when they
// end; a test whose document keeps the file (save_document_as, open_document)
// takes its temp path before newDoc, so the document closes before its folder
// is removed.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func session(t *testing.T) *mcp.ClientSession {
	t.Helper()
	bin := os.Getenv("FREECAD_MCP_E2E_BINARY")
	if bin == "" {
		t.Skip("set FREECAD_MCP_E2E_BINARY to a built freecad-mcp")
	}
	ctx := context.Background()
	cmd := exec.Command(bin, "mcp")
	cmd.Stderr = os.Stderr
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

type reply struct {
	text   string
	images int
	isErr  bool
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) reply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var r reply
	var texts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			texts = append(texts, c.Text)
		case *mcp.ImageContent:
			if len(c.Data) < 8 || string(c.Data[1:4]) != "PNG" {
				t.Errorf("%s: image is not a PNG", name)
			}
			r.images++
		}
	}
	r.text, r.isErr = strings.Join(texts, "\n"), res.IsError
	t.Logf("%s -> error=%v images=%d\n%s", name, r.isErr, r.images, r.text)
	return r
}

func must(t *testing.T, r reply, contains ...string) {
	t.Helper()
	if r.isErr {
		t.Fatalf("unexpected error reply:\n%s", r.text)
	}
	for _, c := range contains {
		if !strings.Contains(r.text, c) {
			t.Fatalf("reply does not contain %q:\n%s", c, r.text)
		}
	}
}

// fails checks that r is an error reply containing every string in contains.
func fails(t *testing.T, r reply, contains ...string) {
	t.Helper()
	if !r.isErr {
		t.Fatalf("expected an error reply:\n%s", r.text)
	}
	for _, c := range contains {
		if !strings.Contains(r.text, c) {
			t.Fatalf("error reply does not contain %q:\n%s", c, r.text)
		}
	}
}

// frontValue returns the value of key in a reply's front matter ("key: value" lines), with
// YAML quotes removed, or "" when the key is absent.
func frontValue(text, key string) string {
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			}
			return v
		}
	}
	return ""
}

// mustDocument checks that r succeeded and its front matter names exactly the document want,
// not a renamed one such as want001, which FreeCAD picks when want is already open.
func mustDocument(t *testing.T, r reply, want string) {
	t.Helper()
	must(t, r)
	if got := frontValue(r.text, "document"); got != want {
		t.Fatalf("reply names document %q, want %q (is a document from an earlier run still open?):\n%s", got, want, r.text)
	}
}

// closeDocCode returns execute_code arguments that close the document named name, unsaved,
// if it is open.
func closeDocCode(name string) map[string]any {
	return map[string]any{"include_screenshot": false,
		"code": fmt.Sprintf("if %q in FreeCAD.listDocuments():\n    FreeCAD.closeDocument(%q)", name, name)}
}

// closeDoc closes the document named name, unsaved, if it is open.
func closeDoc(t *testing.T, cs *mcp.ClientSession, name string) {
	t.Helper()
	must(t, call(t, cs, "execute_code", closeDocCode(name)))
}

// newDoc creates a document named name, first closing one left open by an earlier run so the
// new document gets exactly that name, and closes it, unsaved, when the test ends.
func newDoc(t *testing.T, cs *mcp.ClientSession, name string) {
	t.Helper()
	closeDoc(t, cs, name)
	mustDocument(t, call(t, cs, "create_document", map[string]any{"name": name}), name)
	closeOnCleanup(t, cs, name)
}

// closeOnCleanup closes the document named name, if it is still open, when the test ends.
func closeOnCleanup(t *testing.T, cs *mcp.ClientSession, name string) {
	t.Helper()
	t.Cleanup(func() { call(t, cs, "execute_code", closeDocCode(name)) })
}

// tempPath returns an absolute path with forward slashes inside a test temp dir, on the
// machine running FreeCAD (the e2e suite runs on the same machine).
func tempPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.ToSlash(filepath.Join(t.TempDir(), name))
}

// fileSchema returns the FILE_SCHEMA header entry of a STEP file.
func fileSchema(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	i := strings.Index(s, "FILE_SCHEMA")
	if i < 0 {
		t.Fatalf("%s has no FILE_SCHEMA", path)
	}
	j := strings.Index(s[i:], ";")
	if j < 0 {
		t.Fatalf("%s has an unterminated FILE_SCHEMA", path)
	}
	return s[i : i+j]
}

// TestUnitlessSVGImport: an SVG with no absolute width unit and no Inkscape marker imports at
// an assumed 96 dpi without opening FreeCAD's DPI dialog, which would block the GUI thread.
func TestUnitlessSVGImport(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2ESvg")
	path := tempPath(t, "unitless.svg")
	svg := `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" width="100" height="50" viewBox="0 0 100 50">
  <rect x="10" y="10" width="30" height="20" style="fill:none;stroke:#000000"/>
  <path d="M 50 10 L 90 10 L 90 40 Z" style="fill:none;stroke:#000000"/>
</svg>
`
	if err := os.WriteFile(path, []byte(svg), 0o644); err != nil {
		t.Fatal(err)
	}
	must(t, call(t, cs, "import_file", map[string]any{"path": path, "doc_name": "E2ESvg", "include_screenshot": false}),
		"object_count: 2", "so 96 dpi was assumed automatically")
	must(t, call(t, cs, "get_rpc_status", nil), "freecad: running", "rpc: reachable")
}

// TestStepSchemaRestored: step_schema applies to one export only; the next default export
// writes the same schema as a default export before it.
func TestStepSchemaRestored(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2EStep")
	must(t, call(t, cs, "create_object", map[string]any{"doc_name": "E2EStep", "obj_name": "Box",
		"obj_type": "Part::Box", "include_screenshot": false}), "object_name: Box")
	before, ap242, after := tempPath(t, "before.step"), tempPath(t, "ap242.step"), tempPath(t, "after.step")
	must(t, call(t, cs, "export_document", map[string]any{"doc_name": "E2EStep", "path": before}), "format: step")
	must(t, call(t, cs, "export_document", map[string]any{"doc_name": "E2EStep", "path": ap242, "step_schema": "AP242DIS"}),
		"format: step")
	must(t, call(t, cs, "export_document", map[string]any{"doc_name": "E2EStep", "path": after}), "format: step")

	if s := fileSchema(t, ap242); !strings.Contains(s, "AP242_MANAGED_MODEL_BASED_3D_ENGINEERING_MIM_LF") {
		t.Fatalf("AP242DIS export wrote %s", s)
	}
	if b, a := fileSchema(t, before), fileSchema(t, after); a != b {
		t.Fatalf("default export after AP242DIS wrote %s, before it %s", a, b)
	}
	// With no Scheme preference, FreeCAD 1.1.3's default is AP214IS.
	pref := call(t, cs, "execute_code", map[string]any{"include_screenshot": false,
		"code": "print('scheme=' + FreeCAD.ParamGet('User parameter:BaseApp/Preferences/Mod/Part/STEP').GetString('Scheme') + ';')"})
	must(t, pref)
	if strings.Contains(pref.text, "scheme=;") {
		if s := fileSchema(t, after); !strings.Contains(s, "AUTOMOTIVE_DESIGN") {
			t.Fatalf("default export after AP242DIS wrote %s, want AP214IS (AUTOMOTIVE_DESIGN)", s)
		}
	}
}

// TestMovedPartGlobalPosition: an object inside an App::Part that is moved exports and measures
// at its global position, not its local one.
func TestMovedPartGlobalPosition(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2EAsm")
	must(t, call(t, cs, "create_object", map[string]any{"doc_name": "E2EAsm", "obj_name": "Box", "obj_type": "Part::Box",
		"obj_properties": map[string]any{"Length": 15, "Width": 20, "Height": 30,
			"Placement": map[string]any{"Base": map[string]any{"x": 5, "y": 0, "z": 0}}},
		"include_screenshot": false}), "object_name: Box")
	must(t, call(t, cs, "execute_code", map[string]any{"include_screenshot": false,
		"code": "doc = FreeCAD.getDocument('E2EAsm')\nasm = doc.addObject('App::Part', 'Asm')\n" +
			"inb = doc.addObject('Part::Box', 'InBox')\nasm.addObject(inb)\n" +
			"asm.Placement.Base = FreeCAD.Vector(100, 0, 0)\ndoc.recompute()"}))

	stl := tempPath(t, "inbox.stl")
	must(t, call(t, cs, "export_document", map[string]any{"doc_name": "E2EAsm", "path": stl, "object_names": []string{"InBox"}}),
		"object_count: 1")
	must(t, call(t, cs, "execute_code", map[string]any{"include_screenshot": false,
		"code": fmt.Sprintf("import Mesh\nb = Mesh.Mesh(%q).BoundBox\n"+
			"print('bb', round(b.XMin, 3), round(b.YMin, 3), round(b.ZMin, 3), round(b.XMax, 3), round(b.YMax, 3), round(b.ZMax, 3))", stl)}),
		"bb 100.0 0.0 0.0 110.0 10.0 10.0")

	// Box Face2 is its x = 20 face; InBox Face1 is its local x = 0 face, at x = 100 globally.
	must(t, call(t, cs, "measure", map[string]any{"doc_name": "E2EAsm", "kind": "distance",
		"refs": []map[string]any{{"object": "Box", "sub": "Face2"}, {"object": "InBox", "sub": "Face1"}}}),
		"value: 80")
}

// TestSpreadsheetAliasValidation: aliases are checked for the whole batch before anything is
// written; a unit name or a cell address is refused and the batch's other cells stay unchanged.
func TestSpreadsheetAliasValidation(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2ESheet")
	must(t, call(t, cs, "create_object", map[string]any{"doc_name": "E2ESheet", "obj_name": "Params",
		"obj_type": "Spreadsheet::Sheet", "include_screenshot": false}), "object_name: Params")
	must(t, call(t, cs, "update_spreadsheet_cells", map[string]any{"doc_name": "E2ESheet", "sheet_name": "Params",
		"cells": []map[string]any{{"cell": "B1", "content": "10 mm", "alias": "Length2"}}}), "updated: 1", "Length2")

	fails(t, call(t, cs, "update_spreadsheet_cells", map[string]any{"doc_name": "E2ESheet", "sheet_name": "Params",
		"cells": []map[string]any{{"cell": "C1", "content": "5"}, {"cell": "C2", "alias": "mm"}}}),
		"invalid_input", "'mm' is a unit or a constant")
	fails(t, call(t, cs, "update_spreadsheet_cells", map[string]any{"doc_name": "E2ESheet", "sheet_name": "Params",
		"cells": []map[string]any{{"cell": "C1", "content": "5"}, {"cell": "C2", "alias": "B2"}}}),
		"invalid_input", "'B2' is a cell address")

	cells := call(t, cs, "get_spreadsheet_cells", map[string]any{"doc_name": "E2ESheet", "sheet_name": "Params"})
	must(t, cells, "count: 1", "| B1 | Length2 |")
	if strings.Contains(cells.text, "| C1 ") {
		t.Fatalf("C1 was written by a refused batch:\n%s", cells.text)
	}
}

// TestSpreadsheetErrorCount: cells whose expression fails to evaluate count in error_count.
func TestSpreadsheetErrorCount(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2EErr")
	must(t, call(t, cs, "create_object", map[string]any{"doc_name": "E2EErr", "obj_name": "Params",
		"obj_type": "Spreadsheet::Sheet", "include_screenshot": false}), "object_name: Params")
	must(t, call(t, cs, "update_spreadsheet_cells", map[string]any{"doc_name": "E2EErr", "sheet_name": "Params",
		"cells": []map[string]any{{"cell": "A3", "content": "=1/0"}, {"cell": "B3", "content": "=Nope.x"}}}),
		"updated: 2", "error_count: 2")
}

// TestHiddenOpenKeepsActiveDocument: open_document with hidden and activate false leaves the
// previously active document active.
func TestHiddenOpenKeepsActiveDocument(t *testing.T) {
	cs := session(t)
	path := tempPath(t, "E2EHidden.FCStd")
	newDoc(t, cs, "E2EHidSrc")
	must(t, call(t, cs, "save_document_as", map[string]any{"doc_name": "E2EHidSrc", "path": path}), "copy: false")
	must(t, call(t, cs, "close_document", map[string]any{"doc_name": "E2EHidSrc"}), "closed: E2EHidSrc")

	newDoc(t, cs, "E2EKeep")
	mustDocument(t, call(t, cs, "activate_document", map[string]any{"doc_name": "E2EKeep"}), "E2EKeep")
	// The opened document is named after the file; a leftover E2EHidden would make it E2EHidden001.
	closeDoc(t, cs, "E2EHidden")
	closeOnCleanup(t, cs, "E2EHidden")
	opened := call(t, cs, "open_document", map[string]any{"path": path, "hidden": true, "activate": false})
	mustDocument(t, opened, "E2EHidden")
	must(t, opened, "already_open: false")
	must(t, call(t, cs, "execute_code", map[string]any{"include_screenshot": false,
		"code": "import FreeCADGui\nprint('active', FreeCAD.ActiveDocument.Name, 'gui', FreeCADGui.ActiveDocument.Document.Name)"}),
		"active E2EKeep gui E2EKeep")
}

// TestSaveCopyKeepsSnapshotFileName: a copy of a never saved document does not become that
// document's file in get_rpc_status.
func TestSaveCopyKeepsSnapshotFileName(t *testing.T) {
	cs := session(t)
	// tempPath first: cleanups run last to first, so the document closes before
	// its folder is removed.
	path := tempPath(t, "E2ECopy_copy.FCStd")
	newDoc(t, cs, "E2ECopy")
	must(t, call(t, cs, "save_document_as", map[string]any{"doc_name": "E2ECopy", "path": path, "copy": true}), "copy: true")
	st := call(t, cs, "get_rpc_status", nil)
	must(t, st, `"name": "E2ECopy"`)
	for _, p := range []string{path, filepath.FromSlash(path), strings.ReplaceAll(filepath.FromSlash(path), `\`, `\\`)} {
		if strings.Contains(st.text, p) {
			t.Fatalf("get_rpc_status reports the copy's path %q:\n%s", p, st.text)
		}
	}
}

// TestMeasureMissingSubElement: a sub-element the object does not have is not_found.
func TestMeasureMissingSubElement(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2EMeasure")
	must(t, call(t, cs, "create_object", map[string]any{"doc_name": "E2EMeasure", "obj_name": "Box",
		"obj_type": "Part::Box", "include_screenshot": false}), "object_name: Box")
	fails(t, call(t, cs, "measure", map[string]any{"doc_name": "E2EMeasure", "kind": "area",
		"refs": []map[string]any{{"object": "Box", "sub": "Face99"}}}),
		"not_found", "'Box' has no sub-element 'Face99'")
}

// TestReloadNeverSavedDocument: reloading a document with no file is a conflict that points at
// save_document_as.
func TestReloadNeverSavedDocument(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2EScratch")
	fails(t, call(t, cs, "reload_document", map[string]any{"doc_name": "E2EScratch"}),
		"conflict", "has no file on disk", "save_document_as")
}

// TestDriveLessPathRejected: on Windows a path such as /tmp/x.stl has no drive letter and is
// refused as invalid_input.
func TestDriveLessPathRejected(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive letters exist only on Windows")
	}
	cs := session(t)
	newDoc(t, cs, "E2EDrive")
	must(t, call(t, cs, "create_object", map[string]any{"doc_name": "E2EDrive", "obj_name": "Box",
		"obj_type": "Part::Box", "include_screenshot": false}), "object_name: Box")
	fails(t, call(t, cs, "export_document", map[string]any{"doc_name": "E2EDrive", "path": "/tmp/e2e_drive.stl"}),
		"invalid_input", "has no drive letter")
}

// frontFloat returns the number under key in a reply's front matter.
func frontFloat(t *testing.T, r reply, key string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(frontValue(r.text, key), 64)
	if err != nil {
		t.Fatalf("front matter %s is not a number: %v\n%s", key, err, r.text)
	}
	return v
}

// TestSelectionToMeasure: a face selected the way FreeCAD's tree selects it (on the top-level
// App::Part, with a dotted path through a PartDesign Body to the Pad, which get_selection returns
// together with FreeCAD's element map prefix) can be passed to measure unchanged, and measures at
// its global position.
func TestSelectionToMeasure(t *testing.T) {
	cs := session(t)
	newDoc(t, cs, "E2ESel")
	t.Cleanup(func() {
		call(t, cs, "execute_code", map[string]any{"include_screenshot": false,
			"code": "import FreeCADGui\nFreeCADGui.Selection.clearSelection()"})
	})
	must(t, call(t, cs, "create_object", map[string]any{"doc_name": "E2ESel", "obj_name": "Box", "obj_type": "Part::Box",
		"obj_properties": map[string]any{"Length": 15, "Width": 20, "Height": 30,
			"Placement": map[string]any{"Base": map[string]any{"x": 5, "y": 0, "z": 0}}},
		"include_screenshot": false}), "object_name: Box")
	// A pad of a radius 4 circle, 7 high, in a Body at (0, -40, 50) inside an App::Part at
	// (100, 0, 0): its top face (Face3) is a disc of centre (100, -40, 57).
	must(t, call(t, cs, "execute_code", map[string]any{"include_screenshot": false,
		"code": "import FreeCAD, FreeCADGui, Part\ndoc = FreeCAD.getDocument('E2ESel')\n" +
			"asm = doc.addObject('App::Part', 'Asm')\nbody = doc.addObject('PartDesign::Body', 'Body')\nasm.addObject(body)\n" +
			"xy = [f for f in body.Origin.OriginFeatures if f.Role == 'XY_Plane'][0]\n" +
			"sk = body.newObject('Sketcher::SketchObject', 'Sketch')\nsk.AttachmentSupport = [(xy, '')]\nsk.MapMode = 'FlatFace'\n" +
			"sk.addGeometry(Part.Circle(FreeCAD.Vector(0, 0, 0), FreeCAD.Vector(0, 0, 1), 4), False)\n" +
			"pad = body.newObject('PartDesign::Pad', 'Pad')\npad.Profile = sk\npad.Length = 7\n" +
			"body.Placement.Base = FreeCAD.Vector(0, -40, 50)\nasm.Placement.Base = FreeCAD.Vector(100, 0, 0)\ndoc.recompute()\n" +
			"FreeCADGui.Selection.clearSelection()\nFreeCADGui.Selection.addSelection('E2ESel', 'Asm', 'Body.Pad.Face3')"}))

	sel := call(t, cs, "get_selection", map[string]any{"doc_name": "E2ESel"})
	must(t, sel, "count: 1")
	start := strings.Index(sel.text, "~~~json\n")
	end := strings.LastIndex(sel.text, "\n~~~")
	if start < 0 || end <= start {
		t.Fatalf("get_selection reply has no JSON block:\n%s", sel.text)
	}
	var parsed struct {
		Selection []struct {
			Object      string   `json:"object"`
			SubElements []string `json:"sub_elements"`
		} `json:"selection"`
	}
	if err := json.Unmarshal([]byte(sel.text[start+len("~~~json\n"):end]), &parsed); err != nil {
		t.Fatalf("get_selection JSON: %v\n%s", err, sel.text)
	}
	if len(parsed.Selection) != 1 || len(parsed.Selection[0].SubElements) != 1 {
		t.Fatalf("want one selected sub-element:\n%s", sel.text)
	}
	obj, sub := parsed.Selection[0].Object, parsed.Selection[0].SubElements[0]
	if obj != "Asm" || !strings.HasPrefix(sub, "Body.Pad.") || !strings.HasSuffix(sub, "Face3") {
		t.Fatalf("selection is %s / %s, want Asm / Body.Pad....Face3", obj, sub)
	}
	ref := map[string]any{"object": obj, "sub": sub}

	area := call(t, cs, "measure", map[string]any{"doc_name": "E2ESel", "kind": "area", "refs": []map[string]any{ref}})
	must(t, area, "unit: mm^2")
	if got, want := frontFloat(t, area, "value"), 16*math.Pi; math.Abs(got-want) > 1e-6 {
		t.Fatalf("area of the selected face = %v, want %v", got, want)
	}
	// From the disc's edge to the Box's top face corner (20, 0, 30): the horizontal distance from
	// the disc centre to (20, 0) less the radius, and 57 - 30 vertically.
	dist := call(t, cs, "measure", map[string]any{"doc_name": "E2ESel", "kind": "distance",
		"refs": []map[string]any{ref, {"object": "Box", "sub": "Face6"}}})
	must(t, dist, "unit: mm")
	if got, want := frontFloat(t, dist, "value"), math.Hypot(math.Hypot(80, 40)-4, 27); math.Abs(got-want) > 1e-6 {
		t.Fatalf("distance from the selected face to Box.Face6 = %v, want %v", got, want)
	}
}

func TestAgainstFreeCAD(t *testing.T) {
	cs := session(t)

	must(t, call(t, cs, "get_rpc_status", nil), "version_check: ok")
	// "E2E Doc" becomes the document name E2E_Doc; close one left open by an earlier run so
	// the new document gets exactly that name, and close it however this test ends.
	closeDoc(t, cs, "E2E_Doc")
	closeOnCleanup(t, cs, "E2E_Doc")
	mustDocument(t, call(t, cs, "create_document", map[string]any{"name": "E2E Doc"}), "E2E_Doc")

	box := call(t, cs, "create_object", map[string]any{
		"doc_name": "E2E_Doc", "obj_name": "Box", "obj_type": "Part::Box",
		"obj_properties": map[string]any{"Length": 20, "Width": 10.5, "Height": 5,
			"Placement": map[string]any{"Base": map[string]any{"x": 1, "y": 2, "z": 3}}},
	})
	must(t, box, "object_name: Box")
	if box.images != 1 {
		t.Fatalf("create_object returned %d images", box.images)
	}

	// An integer property rejects floats, so this checks JSON ints stay ints.
	poly := call(t, cs, "create_object", map[string]any{
		"doc_name": "E2E_Doc", "obj_name": "Hex", "obj_type": "Draft::Polygon",
		"obj_properties": map[string]any{"FacesNumber": 6, "Radius": 4}, "include_screenshot": false,
	})
	must(t, poly, "created successfully")

	must(t, call(t, cs, "get_object", map[string]any{"doc_name": "E2E_Doc", "obj_name": "Box", "include_screenshot": false}),
		`"Length"`, "20")
	must(t, call(t, cs, "update_object", map[string]any{"doc_name": "E2E_Doc", "obj_name": "Box",
		"obj_properties": map[string]any{"Length": 25}, "view_name": "Top"}), "updated successfully")
	must(t, call(t, cs, "list_objects", map[string]any{"doc_name": "E2E_Doc", "include_screenshot": false}), "count: 2")
	must(t, call(t, cs, "list_documents", nil), "E2E_Doc")

	code := call(t, cs, "execute_code", map[string]any{
		"code": "doc = FreeCAD.getDocument('E2E_Doc')\nprint('volume', round(doc.getObject('Box').Shape.Volume, 3))", "include_screenshot": false,
	})
	must(t, code, "volume 1312.5")

	view := call(t, cs, "get_view", map[string]any{"view_name": "Isometric", "width": 320, "height": 240})
	if view.isErr || view.images != 1 {
		t.Fatalf("get_view = %+v", view)
	}

	missing := call(t, cs, "get_object", map[string]any{"doc_name": "E2E_Doc", "obj_name": "Nope", "include_screenshot": false})
	if !missing.isErr || !strings.Contains(missing.text, "not_found") {
		t.Fatalf("missing object reply = %+v", missing)
	}

	async := call(t, cs, "execute_code_async", map[string]any{"code": "import time\ntime.sleep(0.5)\nresult = 6 * 7"})
	must(t, async, "job_id:")
	deadline := time.Now().Add(30 * time.Second)
	for {
		st := call(t, cs, "get_async_status", nil)
		if strings.Contains(st.text, `"state": "done"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("async job did not finish: %s", st.text)
		}
		time.Sleep(time.Second)
	}

	headless := call(t, cs, "execute_code_headless", map[string]any{
		"code": "import FreeCAD, Part\nprint('headless', Part.makeBox(1, 2, 3).Volume)", "timeout": 120,
	})
	must(t, headless, "headless 6.0", "exit 0")

	must(t, call(t, cs, "delete_object", map[string]any{"doc_name": "E2E_Doc", "obj_name": "Box", "include_screenshot": false}),
		"deleted successfully")
	must(t, call(t, cs, "execute_code", map[string]any{"code": "FreeCAD.closeDocument('E2E_Doc')", "include_screenshot": false}))
}

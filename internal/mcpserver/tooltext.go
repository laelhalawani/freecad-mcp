package mcpserver

// Every tool's title, description, annotations and parameter descriptions
// live in this file, so the wording is reviewed in one place and the size
// limits in tooltext_test.go apply to all of it. addTool applies a tool's text
// to its schema at registration and panics on a mismatch, so a parameter never
// ships without a description.
//
// The rules the texts follow: a description is at most maxDescriptionBytes
// (some clients cut long ones), the first sentence says what the tool does,
// and every rule that changes how an argument is written sits in that
// argument's description, because the schema reaches every client whole.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// maxDescriptionBytes is the longest tool description.
	maxDescriptionBytes = 1000
	// maxSchemaBytes is the largest input schema, parameter descriptions
	// included: some clients drop every parameter description above 5,000.
	maxSchemaBytes = 4000
	// maxParamDescriptionBytes is the longest parameter description.
	maxParamDescriptionBytes = 800
	// maxInstructionsChars is the longest set of server instructions.
	maxInstructionsChars = 1500
)

// serverInstructions are delivered once per session, before any tool text.
const serverInstructions = `FreeCAD CAD modelling: create, change, measure and simulate (FEM) parts, and import, export and save FreeCAD documents on the computer running FreeCAD. Search for these tools whenever the user asks for CAD, 3D models, parts, FEM, STEP, STL or 3MF files, or 3D printing.
Start: call start_freecad when FreeCAD is not running (it reports already_running otherwise), poll get_rpc_status every 3-5 s until rpc: reachable, then call list_documents and list_objects with compact true.
Rules for every call:
- Property values: lengths in mm and angles in degrees may be numbers; give every other quantity as a string with its unit ("100 N", "2 MPa", "210 GPa"). A bare Force of 100 is 0.1 N.
- A value starting with "=" binds an expression ("=Params.thickness"); "=" alone removes it. Link properties take object names.
- Call list_subelements to find Face and Edge names before measure or FEM References. Never guess them.
- Use the object name each reply returns; FreeCAD may rename (Box001).
- Paths are on the computer running FreeCAD; nothing is transferred.
- Use execute_code only for what no tool covers.
- A call that runs past the user's background limit returns a job_id starting with call-: poll get_async_status with it.
- With remote access, your first call claims FreeCAD for you: save, then call release_session or close_freecad when you stop.`

// toolText is the text of one tool.
type toolText struct {
	Description string
	// Params maps a parameter name to its description; a parameter inside an
	// array of objects is "array[].name". A parameter missing here takes its
	// text from sharedParams.
	Params map[string]string
}

// toolAnnotation is the behaviour hints of one tool, for client approval and
// display.
type toolAnnotation struct {
	readOnly, destructive, openWorld, idempotent bool
}

var (
	annReadOnly    = toolAnnotation{readOnly: true}
	annAdditive    = toolAnnotation{}
	annChanging    = toolAnnotation{destructive: true}
	annCodeRunning = toolAnnotation{destructive: true, openWorld: true}
	// annViewChange changes what the user sees, not the model: a repeat of the
	// same static call leaves the same view.
	annViewChange = toolAnnotation{idempotent: true}
)

// toolAnnotations assigns every tool one of the sets above.
var toolAnnotations = map[string]toolAnnotation{
	"list_documents": annReadOnly, "list_objects": annReadOnly, "get_object": annReadOnly,
	"list_subelements": annReadOnly, "get_selection": annReadOnly, "get_view": annReadOnly,
	"measure": annReadOnly, "get_spreadsheet_cells": annReadOnly, "get_rpc_status": annReadOnly,
	"get_async_status": annReadOnly, "list_parts": annReadOnly, "analyze_mesh": annReadOnly,
	"check_printability": annReadOnly,

	"create_document": annAdditive, "create_object": annAdditive, "insert_part_from_library": annAdditive,
	"import_file": annAdditive, "mesh_to_solid": annAdditive, "solid_to_mesh": annAdditive,
	"activate_document": annAdditive, "open_document": annAdditive, "recompute_document": annAdditive,

	"update_object": annChanging, "update_spreadsheet_cells": annChanging, "save_document": annChanging,
	"reload_document": annChanging, "undo": annChanging, "redo": annChanging, "repair_mesh": annChanging,
	"start_freecad": annChanging, "release_session": annChanging,
	"delete_object": annChanging, "close_document": annChanging, "close_freecad": annChanging,
	"save_document_as": annChanging, "export_document": annChanging,

	"set_view": annViewChange,

	"cancel_job":   annChanging,
	"execute_code": annCodeRunning, "execute_code_async": annCodeRunning,
	"execute_code_headless": annCodeRunning, "run_fem_analysis": annCodeRunning,
}

// sharedParams are the descriptions of parameters that mean the same in every
// tool that has them.
var sharedParams = map[string]string{
	"doc_name":               "document name, as list_documents shows it",
	"obj_name":               "object name, as list_objects shows it (the Name, not the Label)",
	"include_screenshot":     "return a screenshot of the 3D view (default true)",
	"view_name":              "screenshot orientation (default Isometric)",
	"discard_changes":        "close even with unsaved changes, losing them (default false)",
	"quality":                "tessellation preset: coarse for previews, standard for FDM, fine for resin and small curves (default standard)",
	"linear_deflection":      "largest surface-to-triangle distance in mm; overrides quality",
	"angular_deflection_deg": "largest angle between neighbouring triangles, in degrees; overrides quality",
}

// timeoutText is the description of a GUI tool's timeout.
func timeoutText(seconds int) string {
	return fmt.Sprintf("seconds FreeCAD may spend waiting to run and again running (default %d)", seconds)
}

const (
	readPathText = "absolute path of the file on the computer running FreeCAD"
	objPropsText = `properties. Lengths in mm and angles in degrees may be numbers; other quantities are strings with a unit ("100 N", "210 GPa"): a bare Force 100 is 0.1 N. A string starting with "=" is an expression ("=Params.h"); "=" alone removes it. Links take object names: "Box" or ["Box", "Cylinder"]. References: a list of {"object_name": "Box", "face": "Face1"}, {"object_name": "Box", "faces": ["Face1", "Face2"]}, ["Box", "Face1"] or ["Box", ["Face1", "Face2"]]; names from list_subelements. Placement: {"Base": {"x": 0, "y": 0, "z": 0}, "Rotation": {"Axis": {"x": 0, "y": 0, "z": 1}, "Angle": 45}}; one part: "Placement.Base.z": 5 or "=Params.h". Color: {"ViewObject": {"ShapeColor": [0.8, 0.2, 0.2, 1]}}.`
)

// pathText is the description of the path parameter of execute_code and
// execute_code_async.
const pathText = "absolute path of a .py file on the computer running FreeCAD, run instead of code. Pass exactly one of code and path"

// noScreenshotText replaces include_screenshot's shared text on the read-only
// tools that default to none.
const noScreenshotText = "return a screenshot of the 3D view (default false)"

// toolTexts is the text of every tool.
var toolTexts = map[string]toolText{
	// Session and launch.
	"start_freecad": {
		Description: `Start FreeCAD with the MCP RPC server when it is not running, on this computer or on the shared FreeCAD computer this server is configured for. The reply returns at once: poll get_rpc_status every 3-5 s until rpc: reachable (about 15 s, longer on a first start; give up after 120 s). It launches nothing and reports already_running when FreeCAD answers, and refuses when another agent holds FreeCAD. Use open_document for a FreeCAD that already runs. When FreeCAD is not found, set FREECAD_MCP_FREECAD on the computer running FreeCAD.`,
		Params: map[string]string{
			"file": "absolute path of an .FCStd file on the computer running FreeCAD to open once started",
		},
	},
	"get_rpc_status": {
		Description: `Report FreeCAD's state without using its GUI thread, so it answers while FreeCAD starts, is busy or has exited: freecad (running, starting, not_running, exited, unresponsive), rpc (reachable, unreachable), who holds the session, version_check, a stuck GUI operation (gui_dispatch), and the open documents or the launch log tail. While freecad is unresponsive on this computer it samples the FreeCAD process: busy true with cpu_cores means FreeCAD is computing, so wait and poll again. Poll it after start_freecad and call it whenever another tool times out.`,
	},
	"release_session": {
		Description: `Free FreeCAD for other agents now instead of after the idle timeout. With remote access, your first call claims FreeCAD until you stay idle for the configured time; get_rpc_status shows the holder. Documents stay open and unsaved changes stay unsaved: call save_document first.`,
	},
	"close_freecad": {
		Description: `Quit FreeCAD on the computer running it and free the session. Refused while documents have unsaved changes (save first, or pass discard_changes true) and while a task panel or command is open in FreeCAD. Call it when you stop working; start_freecad starts it again.`,
		Params: map[string]string{
			"discard_changes": "quit even with unsaved changes, losing them (default false)",
		},
	},

	// Documents.
	"list_documents": {
		Description: `List the open documents: name (the doc_name other tools take), label, file, unsaved changes, needs recompute, active, and views with their index. Call it first; call create_document or open_document when it is empty.`,
	},
	"create_document": {
		Description: `Create an empty document and make it active. The reply gives the document name FreeCAD chose; pass that as doc_name afterwards.`,
		Params:      map[string]string{"name": "name for the new document, such as Bracket"},
	},
	"open_document": {
		Description: `Open an .FCStd file and make it the active document. A file already open is not reloaded. The reply gives the doc_name, object count and whether it needs recompute_document. Use import_file for STEP, STL, 3MF and other formats and reload_document after the file changed on disk.`,
		Params: map[string]string{
			"path":     "absolute path of the .FCStd file on the computer running FreeCAD",
			"activate": "make it the active document (default true)",
			"hidden":   "open without a 3D view (default false); activate_document with create_view true adds one",
			"timeout":  timeoutText(120),
		},
	},
	"activate_document": {
		Description: `Make an open document active and bring its tab to the front. Tools without doc_name (execute_code, insert_part_from_library) act on the active document.`,
		Params: map[string]string{
			"view_index":  "which view (tab) to show, as list_documents numbers them (default: its active view)",
			"create_view": "add a 3D view when the document has none, as after opening it hidden (default false)",
		},
	},
	"save_document": {
		Description: `Save a document to its own .FCStd file, recomputing first when needed. A never-saved document has no file: use save_document_as. The reply names the file and lists objects still invalid.`,
		Params: map[string]string{
			"recompute": "recompute before saving when needed (default true)",
			"timeout":   timeoutText(120),
		},
	},
	"save_document_as": {
		Description: `Save a document to a new .FCStd file; the document then uses that file and takes its name as label. Missing folders are created. Pass copy true to write a copy and keep the current file. An existing file is replaced only with overwrite true. Use export_document for other formats.`,
		Params: map[string]string{
			"path":      "absolute path to write on the computer running FreeCAD; missing folders are created; .FCStd is appended when missing",
			"copy":      "write a copy and keep the document on its current file (default false)",
			"overwrite": "replace an existing file (default false)",
			"recompute": "recompute before saving when needed (default true)",
			"timeout":   timeoutText(120),
		},
	},
	"close_document": {
		Description: `Close a document and its tabs. Refused while it has unsaved changes unless discard_changes is true: save first. The reply names the document active afterwards.`,
	},
	"reload_document": {
		Description: `Close and reopen a document from its .FCStd file to show changes made on disk, for example by execute_code_headless. Fails for a document never saved to a file.`,
	},

	// Objects.
	"list_objects": {
		Description: `List the objects of a document with type and properties. Pass compact true for one short row per object (name, label, type, state, valid, parent, visible); use it first to learn the object names other tools take. No screenshot unless include_screenshot is true. An unknown document gives an empty list.`,
		Params: map[string]string{
			"compact":            "one short row per object instead of all properties (default false)",
			"include_screenshot": noScreenshotText,
		},
	},
	"get_object": {
		Description: `Get one object with its type and all properties, including its bounding box (fast, and possibly loose on curved parts; check_printability reports the tight size). Use it to check values after create_object or update_object and to see which properties an object has. No screenshot unless include_screenshot is true.`,
		Params:      map[string]string{"include_screenshot": noScreenshotText},
	},
	"list_subelements": {
		Description: `List the faces and edges of an object so you can pick sub-elements for measure and FEM References. Per face: name (Face1), surface type (plane, cylinder, cone, sphere, torus, other), area, centre, and normal (planes) or radius and axis (cylinders, spheres). Per edge: name (Edge1), curve type (line, circle, other), length, and radius and centre (circles). Coordinates are global, in mm. Call it instead of guessing face numbers. Read-only.`,
		Params:      map[string]string{"kind": "which sub-elements to list (default faces)"},
	},
	"create_object": {
		Description: `Create one object in a document. Use the object name the reply returns: FreeCAD may rename (Box001). The reply lists each quantity it set with its unit. An error before the object exists creates nothing; an object created but not computing stays, named in the reply: fix it with update_object or delete_object.
FEM: create Fem::AnalysisPython first; pass analysis_name for its material, constraints and mesh; then call run_fem_analysis. Fem::MaterialCommon takes Material as {"Name": "Steel", "YoungsModulus": "210 GPa", "PoissonRatio": 0.3, "Density": "7900 kg/m^3"}. Fem::FemMeshGmsh takes Shape (the solid's name) and CharacteristicLengthMax/Min in mm, and is meshed on creation. A Fem::ConstraintForce acts along the outward normal of its face and a Fem::ConstraintPressure into it; Reversed true flips either, and the reply states the direction.`,
		Params: map[string]string{
			"obj_type":       `FreeCAD type, such as Part::Box, Part::Cylinder, Part::Cut, PartDesign::Body, Spreadsheet::Sheet, Fem::AnalysisPython, Fem::ConstraintFixed. Of the Python-only types only these work: Part::Tube (needs InnerRadius, OuterRadius, Height), Draft::Circle (Radius), Draft::Rectangle (Length, Height), Draft::Polygon (FacesNumber, Radius), Draft::Wire (Points, optional Closed); build others with execute_code`,
			"obj_name":       "name for the new object; the reply gives the name actually used (Draft types name themselves and keep this as the Label)",
			"analysis_name":  "Fem::AnalysisPython object to add this FEM object to; required for Fem::FemMeshGmsh",
			"obj_properties": objPropsText,
		},
	},
	"update_object": {
		Description: `Set properties of an existing object: dimensions, Placement, links, expressions, colors. obj_properties follows the same rules as in create_object. The reply lists each quantity it set with its unit; check the rest with get_object.`,
		Params:      map[string]string{"obj_properties": objPropsText},
	},
	"delete_object": {
		Description: `Delete an object. Objects that use it (a Part::Cut's Base or Tool) may become invalid: call recompute_document afterwards to see which.`,
	},
	"recompute_document": {
		Description: `Recompute a document and list every object that failed, with FreeCAD's message, and every object still touched. Call it after a series of changes, after delete_object, or when a document needs a recompute. Failures do not fail the call; fix them with update_object or delete_object.`,
		Params:      map[string]string{"timeout": timeoutText(120)},
	},
	"undo": {
		Description: `Undo the last changes to a document, one transaction per step. Each changing tool call is one transaction named after the tool ('MCP: create_object'); edits made by hand in FreeCAD count too. The reply names what was undone and what is left. Refused while a task panel is open in FreeCAD.`,
		Params:      map[string]string{"steps": "how many transactions to undo (default 1)"},
	},
	"redo": {
		Description: `Redo transactions that undo reverted, one per step. A new change after an undo clears what can be redone. Refused while a task panel is open in FreeCAD.`,
		Params:      map[string]string{"steps": "how many transactions to redo (default 1)"},
	},
	"insert_part_from_library": {
		Description: `Insert a part from the FreeCAD parts library into the active document. Get relative_path from list_parts; then place it with update_object.`,
		Params:      map[string]string{"relative_path": "path of the part inside the library, as list_parts shows it"},
	},
	"list_parts": {
		Description: `List the parts in the FreeCAD parts library as relative paths for insert_part_from_library. An empty list means the parts_library addon is not installed: build the shape with create_object.`,
	},

	// Inspection.
	"get_selection": {
		Description: `Read what the user selected in FreeCAD: objects with document, label and type, sub-elements (Face6, Edge12) and picked points. Use it when the user says "this face"; pass the sub-element names to measure unchanged. Empty when nothing is selected.`,
		Params:      map[string]string{"doc_name": "document whose selection to read (default: all open documents)"},
	},
	"measure": {
		Description: `Measure in global coordinates: distance between two objects or sub-elements (with the closest points), angle between two straight edges or planar faces, length of edges, radius of a circular edge or cylindrical or spherical face, area or volume. Get sub-element names from list_subelements or get_selection. The reply gives the value with its unit.`,
		Params: map[string]string{
			"kind":          "distance and angle take two refs; radius takes one; length, area and volume take one or more",
			"refs":          "objects or sub-elements to measure",
			"refs[].object": "object name, as list_objects shows it",
			"refs[].sub":    "sub-element such as Face3, Edge1, Vertex2 from list_subelements, or the full path get_selection returns (Body.Pad.Face3) passed unchanged (default: the whole object)",
		},
	},
	"get_view": {
		Description: `Screenshot a document's 3D view from an orientation, leaving FreeCAD's camera and selection as they were. Use it after changes made without screenshots, or with focus_object to frame one object. Fails when the document has no 3D view (opened hidden) or, without doc_name, when the active window is not a 3D view.`,
		Params: map[string]string{
			"doc_name":     "document to capture (default: the active document's active view)",
			"focus_object": "object name to frame (default: fit all)",
			"width":        "image width in pixels (default: the viewport's width; with width and height both omitted the longest edge is at most 1024)",
			"height":       "image height in pixels (default: the viewport's height; with width and height both omitted the longest edge is at most 1024)",
			"view_name":    "orientation (default Isometric)",
		},
	},
	"set_view": {
		Description: `Set what the user sees in a document's 3D view and leave it there: orientation, framing, which objects are visible, transparency and display modes. It changes the view, not the model: nothing to undo and no unsaved-changes mark. Mode static (default) sets the view now; orbit turns about the vertical axis around the scene; tour flies from stop to stop. An animated mode runs until the user moves the view, the next set_view call, reset true or the document closes; get_view still works meanwhile. reset true restores what earlier set_view calls changed. The reply and its screenshot show the view as set. Use get_view to look without changing anything.`,
		Params: map[string]string{
			"doc_name":              "document whose 3D view to change (default: the active document); its tab is brought to the front",
			"view_name":             "orientation to set (default: keep the current one)",
			"focus":                 "object names to frame (default: everything visible)",
			"show":                  "object names to make visible",
			"hide":                  "object names to hide",
			"isolate":               "object names to show while hiding every other visible object with a shape",
			"transparency":          "object name to transparency from 0 (opaque) to 100",
			"display_mode":          "object name to display mode, such as Flat Lines, Shaded or Wireframe; the error lists the modes an object offers",
			"mode":                  "static sets the view now; orbit turns around the scene; tour visits the stops (default static)",
			"degrees_per_second":    "orbit speed in degrees per second; negative turns the other way (default 15)",
			"stops":                 "tour stops in order (default: each visible object)",
			"stops[].focus":         `object names to frame at this stop, or ["all"]`,
			"stops[].dwell_seconds": "seconds to stay at the stop (default 2)",
			"stops[].view_name":     "orientation at this stop (default: keep the current one)",
			"move_seconds":          "seconds a tour takes to travel between stops (default 2)",
			"loop":                  "repeat the tour until stopped (default false)",
			"reset":                 "restore visibility, transparency and display modes earlier set_view calls changed, and stop any running mode (default false)",
		},
	},
	"get_spreadsheet_cells": {
		Description: `Read cells of a Spreadsheet::Sheet: content as entered (=Length*2), computed value and alias. Without cells, every non-empty cell up to 2000.`,
		Params: map[string]string{
			"sheet_name": "Spreadsheet::Sheet object name, as list_objects shows it",
			"cells":      "addresses (B2), ranges (A1:C10) or aliases (default: every non-empty cell)",
		},
	},
	"update_spreadsheet_cells": {
		Description: `Set content and aliases of spreadsheet cells, then recompute so objects using them update. Create a sheet with create_object and obj_type Spreadsheet::Sheet; bind a property to a cell with update_object and "=Sheet.alias". The reply shows each cell's new value and objects that became invalid.`,
		Params: map[string]string{
			"sheet_name":      "Spreadsheet::Sheet object name, as list_objects shows it",
			"cells":           "1 to 500 cells; each needs content, alias or both",
			"cells[].cell":    "address such as B2, or an existing alias",
			"cells[].content": `a number with optional unit ("10 mm"), text, or an expression starting with = ("=Length*2"); "" clears the cell`,
			"cells[].alias":   `alias for expressions as <sheet_name>.<alias>; "" removes it`,
			"recompute":       "recompute afterwards (default true)",
		},
	},

	// Files.
	"import_file": {
		Description: `Import a CAD, mesh or 2D file without dialogs: .step/.stp, .iges/.igs, .gltf/.glb, .brep/.brp, .stl/.ast, .obj, .off, .ply, .3mf, .dxf, .svg. With doc_name it adds to that document; without, it creates a document named after the file. The reply lists the created object names. Meshes arrive as Mesh objects: call mesh_to_solid for a solid. An SVG without units imports at 96 dpi and the reply warns. Use open_document for .FCStd.`,
		Params: map[string]string{
			"path":           readPathText,
			"doc_name":       "open document to import into (default: a new document named after the file)",
			"merge":          "STEP, IGES and glTF: merge parts into one compound (default: FreeCAD's import preference)",
			"use_link_group": "STEP, IGES and glTF: build assemblies from App::Link groups (default: FreeCAD's import preference)",
			"import_hidden":  "STEP, IGES and glTF: also import objects the file marks hidden (default: FreeCAD's import preference)",
			"timeout":        timeoutText(300),
		},
	},
	"export_document": {
		Description: `Export objects to a file without dialogs; the extension picks the format. Meshes for slicers: .stl, .ast, .3mf, .amf, .obj, .ply, .off (3MF and AMF keep one object per part, in mm). Exact CAD: .step/.stp, .iges/.igs, .brep/.brp. Also .glb/.gltf (meters; .gltf writes a .bin beside it), .FCStd (a copy), .dxf and .svg (projected on XY). Missing folders are created; an existing file is replaced only with overwrite true. Without object_names, the visible top-level objects with geometry are exported (a Body once). With per_object true, path is a folder and each object in object_names is written to its own file named after its label, all in this one call, which reports every file. Call check_printability before exporting for a printer.`,
		Params: map[string]string{
			"path":           "absolute path to write on the computer running FreeCAD (with per_object, the folder to fill); missing folders are created; the extension picks the format",
			"per_object":     "write one file per object in object_names into the folder path, named after each label (default false)",
			"format":         "with per_object, the extension of every file, for example stl or step",
			"object_names":   "object names to export (default: visible top-level objects with geometry)",
			"include_hidden": "also export hidden top-level objects in the default set (default false)",
			"ascii":          "STL only: write ASCII (default false)",
			"relative":       "mesh formats: linear_deflection is relative to edge length (default false)",
			"step_schema":    "STEP only: application protocol (default: FreeCAD's export preference)",
			"step_unit":      "STEP and IGES: length unit written (default: FreeCAD's export preference)",
			"recompute":      "recompute first when needed (default true)",
			"overwrite":      "replace an existing file (default false)",
			"timeout":        timeoutText(300),
		},
	},

	// Mesh and printing.
	"check_printability": {
		Description: `Check a print layout: every listed part lies inside the plate (x and y from its corner, which is 0, 0 unless you pass bed_origin_x and bed_origin_y; z up from 0) and no two parts overlap. Lay each part flat on the plate with its Placement and space the parts apart first, then call this; overlapping complex parts such as threads make the intersection slow. Per part: size, free margin to the plate edges, and what it overlaps; overlap is the volume of the parts' solid intersection, so a part sitting in another's cavity is fine. No meshing, so it is fast. Move parts with update_object until printable is true. printable is true only when something was checked, every part is inside and none overlaps. Find failed features with recompute_document; analyze_mesh checks mesh defects.`,
		Params: map[string]string{
			"object_names": "object names to check (default: visible top-level solids)",
			"bed_x":        "plate width in mm, x from the plate's corner",
			"bed_y":        "plate depth in mm, y from the plate's corner",
			"bed_z":        "build height in mm, z from 0 (default: height not checked)",
			"bed_origin_x": "x of the plate's corner in mm, for a plate laid beside the first (default 0)",
			"bed_origin_y": "y of the plate's corner in mm (default 0)",
			"timeout":      timeoutText(120),
		},
	},
	"analyze_mesh": {
		Description: `Analyze a mesh object (Mesh::Feature, such as an imported STL) for defects that break printing and mesh_to_solid: open edges, non-manifold edges, self-intersections, flipped facets, invalid points, corrupted facets, separate components. The reply lists the repair_mesh steps that fix them. Read-only.`,
		Params: map[string]string{
			"obj_name": "mesh object name (Mesh::Feature), as list_objects shows it",
			"timeout":  timeoutText(120),
		},
	},
	"repair_mesh": {
		Description: `Repair a mesh object in place with named steps in order. Pass the steps analyze_mesh suggests, or omit steps for the standard sequence. The reply compares the mesh before and after; undo reverts it in one step.`,
		Params: map[string]string{
			"obj_name":             "mesh object name (Mesh::Feature), as list_objects shows it",
			"steps":                "repair steps in order (default: fix_indices, remove_duplicated_points, remove_duplicated_facets, fix_degenerations, remove_non_manifolds, fix_self_intersections, harmonize_normals, fill_holes)",
			"fill_holes_max_edges": "fill_holes closes holes with at most this many edges (default 20)",
			"timeout":              timeoutText(300),
		},
	},
	"mesh_to_solid": {
		Description: `Convert a mesh object into a new Part solid for booleans, fillets and STEP export; the mesh stays. Each triangle becomes a face: above 200000 facets pass force true, and refine merges coplanar faces. An open mesh gives a shell, not a solid: run analyze_mesh and repair_mesh first.`,
		Params: map[string]string{
			"obj_name":    "mesh object name (Mesh::Feature), as list_objects shows it",
			"result_name": `name for the new Part object (default: obj_name + "_solid")`,
			"tolerance":   "distance in mm within which edges are sewn (default 0.1)",
			"refine":      "merge coplanar triangles into larger faces, slower (default false)",
			"force":       "convert meshes above 200000 facets, which can take minutes (default false)",
			"timeout":     timeoutText(300),
		},
	},
	"solid_to_mesh": {
		Description: `Tessellate an object's shape (Part feature, Body, Link, App::Part) into a new mesh object; the source stays. Use it to inspect or repair the triangles a printer will get; export_document tessellates by itself.`,
		Params: map[string]string{
			"obj_name":    "name of an object with a shape, as list_objects shows it",
			"result_name": `name for the new mesh object (default: obj_name + "_mesh")`,
			"relative":    "linear_deflection is relative to edge length (default false)",
			"timeout":     timeoutText(300),
		},
	},

	// FEM.
	"run_fem_analysis": {
		Description: `Run CalculiX on a FEM analysis and return max and min von Mises stress (MPa), max displacement (mm), node count, the result object and the working directory. The analysis needs, all made with create_object and analysis_name: a Fem::MaterialCommon, a Fem::FemMeshGmsh of the solid, a Fem::ConstraintFixed and a Fem::ConstraintForce or Fem::ConstraintPressure on faces from list_subelements. A CalculiX solver is added when missing. It colours the 3D view by von Mises stress and hides the FEM mesh and the meshed solid; the reply names them and how to show them again. It blocks FreeCAD's GUI thread until done: do not send other calls meanwhile; get_rpc_status still answers.`,
		Params: map[string]string{
			"analysis_name": "Fem::AnalysisPython object name",
			"timeout":       "seconds to wait for the solver (default 600)",
		},
	},

	// Code.
	"execute_code": {
		Description: `Run Python in FreeCAD's GUI thread and return what it prints; FreeCAD, FreeCADGui and the documents are available. Use it only for what no other tool covers, and only for quick changes: it freezes FreeCAD while it runs. Work that may take more than a few seconds (booleans or distances between complex parts, threads, fillets, meshing) goes to execute_code_headless. It has a time budget to start and another to run (90 s each by default); pass timeout for slower work. Use execute_code_async for long pure-geometry work on shapes already fetched. A script longer than about 30 lines goes in a .py file, run with path. File paths, path included, are on the computer running FreeCAD. Set include_screenshot false for code that does not change the model.`,
		Params: map[string]string{
			"code":    "Python code; print what you need back. Pass exactly one of code and path",
			"path":    pathText,
			"timeout": "seconds to wait to start and again to run (default: the addon's budget, 90 unless it sets another)",
		},
	},
	"execute_code_async": {
		Description: `Run long, CPU-heavy Python (OCCT fuse, cut, loft on shapes already fetched) in a background thread; the call returns a job_id at once: poll get_async_status. The code must not touch the GUI or the document: no FreeCADGui, view or selection calls, no object creation or property changes, no recompute, no save. Doing so can hang FreeCAD. Hand every document or view write to the GUI thread with the injected commit(fn, timeout=120), which runs fn there and returns its result. Pattern: fetch shapes into module variables with execute_code, compute here, then commit(apply). Example:
    fused = base.fuse(addition).removeSplitter()
    def apply():
        obj.Shape = fused
        doc.recompute()
    commit(apply)
Scripts share one namespace across calls. Fuse many additions together first, then apply one boolean to the heavy shape. File paths, path included, are on the computer running FreeCAD.`,
		Params: map[string]string{
			"code": "background-safe Python; send every document or view write through commit(fn). Pass exactly one of code and path",
			"path": pathText,
		},
	},
	"execute_code_headless": {
		Description: `Run a Python script in a separate freecadcmd process without GUI, so an OpenCascade crash kills only that process; use it for helical threads, lofts, sweeps, many-tool booleans and long rebuilds. It runs on the computer running this MCP server, not on the computer running FreeCAD, and shares nothing with execute_code: import FreeCAD and Part, open files with FreeCAD.openDocument(path), save with doc.save() or Shape.exportBrep(), print progress. __file__ is the script's path. Its path parameter is a file on that same computer. A failed script still returns everything it printed, then the traceback. A script that may take minutes runs in the background: the call returns a job_id at once and get_async_status reports it. After it saves an .FCStd open in the GUI, call reload_document.`,
		Params: map[string]string{
			"code":       "complete Python script for freecadcmd. Pass exactly one of code and path",
			"path":       "absolute path of a .py file on the computer running this MCP server, run where it is instead of code. Pass exactly one of code and path",
			"timeout":    "seconds before the process is killed; partial output is kept; omitted: 600 s in the foreground; a timeout over 120 runs in the background unless background is false",
			"background": "true: return a job_id at once and run the script in the background, its output streaming to a file; poll get_async_status; false: wait for the script whatever the timeout (default: background only for a timeout over 120)",
		},
	},
	"get_async_status": {
		Description: `Report background jobs: execute_code_async jobs (running, done or failed, with error and traceback) and execute_code_headless jobs (running, finished or cancelled, exit code, elapsed seconds, the last 200 lines of output). Answers while a job runs. Async jobs are kept until FreeCAD exits; headless jobs until the MCP server exits or a day after they finish. Stop a headless job with cancel_job. A call- job is a call that ran past the background limit: it returns that call's own reply when it ends.`,
		Params: map[string]string{
			"job_id": "job_id from execute_code_async, execute_code_headless or a call that moved to the background (default: all running jobs and up to 20 recent ones)",
		},
	},
	"cancel_job": {
		Description: `Stop a background execute_code_headless job: its process and everything it started end. The reply is the job's status with its last output. A job that had already finished is reported as finished, with its exit code. A call- job cannot be stopped: FreeCAD ends it when the work finishes.`,
		Params: map[string]string{
			"job_id": "job_id of a headless job, from execute_code_headless or get_async_status",
		},
	},
}

// toolTitle is a tool's name in words: "create_object" is "Create object".
func toolTitle(name string) string {
	words := strings.Split(name, "_")
	for i, w := range words {
		switch w {
		case "rpc", "fem":
			words[i] = strings.ToUpper(w)
		case "freecad":
			words[i] = "FreeCAD"
		}
	}
	title := strings.Join(words, " ")
	return strings.ToUpper(title[:1]) + title[1:]
}

// annotationsFor builds the MCP annotations of a tool.
func annotationsFor(name string) *mcp.ToolAnnotations {
	a, ok := toolAnnotations[name]
	if !ok {
		panic(fmt.Sprintf("tool %q has no annotations", name))
	}
	open, destructive := a.openWorld, a.destructive
	ann := &mcp.ToolAnnotations{
		Title:          toolTitle(name),
		ReadOnlyHint:   a.readOnly,
		IdempotentHint: a.readOnly || a.idempotent,
		OpenWorldHint:  &open,
	}
	if !a.readOnly {
		ann.DestructiveHint = &destructive
	}
	return ann
}

// describeSchema sets the description of every parameter of schema from the
// tool's own text, else sharedParams. It panics when a parameter has none, or
// when the tool's text names a parameter the schema does not have.
func describeSchema(name string, schema *jsonschema.Schema, own map[string]string) {
	used := map[string]bool{}
	var walk func(prefix string, s *jsonschema.Schema)
	walk = func(prefix string, s *jsonschema.Schema) {
		for prop, sub := range s.Properties {
			key := prefix + prop
			text, ok := own[key]
			if ok {
				used[key] = true
			} else if text, ok = sharedParams[prop]; !ok || prefix != "" {
				panic(fmt.Sprintf("tool %q: parameter %q has no description", name, key))
			}
			sub.Description = text
			if sub.Items != nil && len(sub.Items.Properties) > 0 {
				walk(key+"[].", sub.Items)
			}
		}
	}
	walk("", schema)
	for key := range own {
		if !used[key] {
			panic(fmt.Sprintf("tool %q: text for unknown parameter %q", name, key))
		}
	}
}

// addTool registers a tool with its text, title and annotations.
func addTool[In any](srv *mcp.Server, name string, schema *jsonschema.Schema,
	handler func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error)) {
	text, ok := toolTexts[name]
	if !ok {
		panic(fmt.Sprintf("tool %q has no text", name))
	}
	describeSchema(name, schema, text.Params)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        name,
		Title:       toolTitle(name),
		Description: text.Description,
		Annotations: annotationsFor(name),
		InputSchema: schema,
	}, handler)
}

// toolTextNames lists the tools that have text, sorted (for tests).
func toolTextNames() []string {
	names := make([]string, 0, len(toolTexts))
	for name := range toolTexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

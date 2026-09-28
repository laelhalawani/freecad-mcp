# Tools

[Back to README](../README.md) · [Installation](installation.md) · [Configuration](configuration.md) · [Code execution](execution.md) · [Remote access](remote-access.md)

FreeCAD MCP exposes 39 tools, grouped below by task; two of them,
`release_session` and `close_freecad`, are listed only while
[remote access](remote-access.md) is on, since they manage the multi-agent
session lock that comes with it. Every tool that changes a document records
its change as one named transaction, next to edits made by hand in FreeCAD,
so `undo` and `redo` cover it. Placements use degrees: a `Rotation`'s `Angle`
is given to `create_object` and `update_object` in degrees, and `get_object`
and `list_objects` report it in degrees.

Most tools take a `doc_name`, the internal name `list_documents` shows (not
necessarily the document's label), and many take `obj_name` the same way from
`list_objects`. A tool that changes a document reports objects left invalid
by the change and how to fix or remove them.

Every path a tool takes is on the computer running FreeCAD; with
[remote access](remote-access.md) that is another computer than the one
running the AI client or the MCP server, and no file is transferred over
this connection: see [file paths](remote-access.md#file-paths).
`execute_code_headless` is the exception: its script runs on the computer
hosting the MCP server, so its paths are on that computer instead.

- [Document lifecycle](#document-lifecycle)
- [Objects](#objects)
- [Measurement and selection](#measurement-and-selection)
- [Mesh operations](#mesh-operations)
- [Spreadsheets](#spreadsheets)
- [Import and export](#import-and-export)
- [Printability](#printability)
- [FEM analysis](#fem-analysis)
- [Recompute, undo and redo](#recompute-undo-and-redo)
- [Code execution](#code-execution)
- [View and screenshots](#view-and-screenshots)
- [Screenshot options](#screenshot-options)
- [Parts library](#parts-library)
- [Launch and status](#launch-and-status)

## Document lifecycle

### `create_document`

Create a new, empty document and make it active. Use it before `create_object`
when no document exists yet.

- `name` (string, required): the name of the document to create.

The reply carries the name FreeCAD gave the document (not always exactly
`name`), which later calls take as `doc_name`.

### `list_documents`

List every open document with its label, file, whether it has unsaved changes
or needs a recompute, whether it is active, and its views (tabs) with their
index. Takes no arguments.

The reply is a table of name, label, file, modified, active and views; an
empty list says to call `create_document` or `open_document`. Use it first to
find the `doc_name` other tools take and the `view_index` `activate_document`
takes.

### `open_document`

Open a `.FCStd` file from an absolute path on the machine running FreeCAD,
without any dialog, and make it active.

- `path` (string, required): absolute path of the `.FCStd` file.
- `hidden` (boolean, default `false`): open without a 3D view tab.
- `activate` (boolean, default `true`): make it the active document.
- `timeout` (number, optional, default 120, up to 1800 seconds): the queue and
  execution budget; raise it for large assemblies.

A file that is already open is not reloaded; the reply says so and names the
document, its object count and whether it needs a recompute. Opened hidden, it
has no view to activate until a later `activate_document` call with
`create_view` true. Use `import_file` for STEP, STL, 3MF and other formats,
and `reload_document` to pick up changes made to an already open file on disk.

### `activate_document`

Make an open document the active one and bring its tab to the front.

- `doc_name` (string, required): the document to activate.
- `view_index` (integer, optional, 0 to 1000): which of the document's views
  to activate, as `list_documents` numbers them; default its active view.
- `create_view` (boolean, default `false`): open a 3D view when the document
  has none, for example after opening it hidden.

Tools without a `doc_name`, such as `execute_code` and
`insert_part_from_library`, act on whichever document this made active. A
document opened hidden needs `create_view` true before `get_view` can
screenshot it.

### `reload_document`

Close and reopen a document to pick up changes made to its `.FCStd` file
outside the FreeCAD GUI, for example by a script run with
`execute_code_headless` that edited and saved it.

- `doc_name` (string, required): the document to reload.

Fails when the document is not open, or was never saved to a file (nothing to
reload from). The open GUI document is otherwise unaware of on-disk changes;
this closes the stale in-memory copy and reopens the file.

### `save_document`

Save an open document to its own `.FCStd` file, without any dialog.

- `doc_name` (string, required): the document to save.
- `recompute` (boolean, default `true`): recompute the document first when it
  needs it.
- `timeout` (number, optional, default 120, up to 1800 seconds).

A document that was never saved has no file yet: use `save_document_as`
instead. The reply names the file and lists objects left invalid by the
recompute; `list_documents` shows which documents have unsaved changes.

### `save_document_as`

Save an open document to a new `.FCStd` file at an absolute path.

- `doc_name` (string, required): the document to save.
- `path` (string, required): absolute path to write; `.FCStd` is appended when
  the name has no extension.
- `overwrite` (boolean, default `false`): replace an existing file at `path`.
- `copy` (boolean, default `false`): write a copy and keep the document on its
  current file and name.
- `recompute` (boolean, default `true`).
- `timeout` (number, optional, default 120, up to 1800 seconds).

Without `copy`, the document then uses that file and its label becomes the
file name. A file another open document already uses is refused. Use
`export_document` for STEP, STL, 3MF and other exchange formats.

### `close_document`

Close an open document and its tabs.

- `doc_name` (string, required): the document to close.
- `discard_changes` (boolean, default `false`): close even with unsaved
  changes, losing them.

A document with unsaved changes is refused unless `discard_changes` is true;
call `save_document` or `save_document_as` first to keep them. The reply names
the document active afterwards, or says none is. FreeCAD asks no questions, so
nothing waits for a person.

## Objects

### `create_object`

Create a new object in a document.

- `doc_name` (string, required): the document to create the object in.
- `obj_type` (string, required): the FreeCAD type, for example `Part::Box`,
  `Part::Cylinder`, `Part::Cut`, `PartDesign::Body`, `Fem::ConstraintFixed`.
- `obj_name` (string, required): the name to give the object.
- `analysis_name` (string, optional): for FEM objects, the FEM analysis to add
  the object to.
- `obj_properties` (object, optional): the properties to set at creation,
  for example `{"Height": 30, "Radius": 10}`.
- `include_screenshot` (boolean, default `true`), `view_name` (string, default
  `"Isometric"`): see [screenshot options](#screenshot-options).

`obj_type` must name a type registered in FreeCAD's type system (most
`Part::`, `PartDesign::` and `Fem::` types). A handful of Python-implemented
types are supported through dedicated factories instead, each needing the
properties listed:

```
Part::Tube        InnerRadius, OuterRadius, Height
Draft::Circle     Radius
Draft::Rectangle  Length, Height
Draft::Polygon    FacesNumber, Radius
Draft::Wire       Points (list of {x, y, z}), optional Closed
```

Any other Python-implemented type must be built with `execute_code` instead.
The Draft factories name objects themselves, so for those the returned object
name can differ from the requested `obj_name`, which becomes the object's
Label instead; always use the returned name in later calls. A `Placement`'s
`Rotation.Angle` is in degrees; `ViewObject.ShapeColor` is `[r, g, b, a]` from
0 to 1.

The reply names the created object and, unless `include_screenshot` is false,
carries a screenshot. An object that was created but failed to compute still
stays in the document under its name, reported as an error naming it, with a
hint to fix it with `update_object` or remove it with `delete_object`.

### `update_object`

Set properties of an existing object, in the same form `create_object` takes.

- `doc_name` (string, required)
- `obj_name` (string, required): the object to update.
- `obj_properties` (object, required): the properties to set.
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).

Use it when `create_object` cannot set a property at creation time, then
verify the result with `get_object`.

### `delete_object`

Delete an object from a document.

- `doc_name` (string, required)
- `obj_name` (string, required): the object to delete.
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).

Objects that depend on it, for example a `Part::Cut` using it as Base or Tool,
may become invalid; call `list_objects` afterwards to check the document.

### `list_objects`

List every object in a document with its type and properties.

- `doc_name` (string, required)
- `compact` (boolean, default `false`): short rows (name, label, type, state,
  valid, parent, visible) instead of every property.
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).
  With `compact`, the screenshot is off unless `include_screenshot` is passed
  as `true`, since the point of a compact call is a quick, cheap read.

Use it before changing a document to see what exists and which names to pass
to `get_object`, `update_object` and `delete_object`. An unknown document
gives an empty list; `list_documents` shows the open ones.

### `get_object`

Get one object with its type and all its properties.

- `doc_name` (string, required)
- `obj_name` (string, required)
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).

Use it to check the values `update_object` or `create_object` set, or to see
which properties an object has before updating it. A missing object is a
not-found error naming `list_objects` and `list_documents` as next steps.

## Measurement and selection

### `measure`

Measure FreeCAD geometry in global coordinates.

- `doc_name` (string, required)
- `kind` (string, required, one of `distance`, `angle`, `length`, `radius`,
  `area`, `volume`).
- `refs` (array, required, at least one entry): the objects or sub-elements to
  measure, each `{"object": name, "sub"?: element}`.

`distance` and `angle` take exactly two refs, `radius` exactly one, `length`,
`area` and `volume` take one or more. `distance` is the shortest distance
between two objects or sub-elements, with the closest points; `angle` is
between two straight edges or planar faces; `length` sums edge length;
`radius` reads a circular edge or a cylindrical or spherical face; `area` and
`volume` sum over faces or solids.

A ref's `sub` is either a plain element name on the object itself (`Face3`,
`Edge1`, `Vertex2`), or the full object path `get_selection` returns for
anything below the top level (`Body.Pad.Face3`); pass `get_selection`'s
`sub_elements` unchanged rather than shortening them, which would pick that
element on the top object instead. Without `sub`, the whole object is used.

The reply gives the value and its unit (`mm`, `deg`, `mm^2`, `mm^3`), and for
`distance` the two closest points. A `sub` naming no face, edge or vertex of an
object that does resolve to a shape is a not-found error; `volume` on an
object with no solid is an invalid-input error. `get_object`'s bounding box
also helps size an object without a full measurement.

### `get_selection`

Read what the user has selected in FreeCAD.

- `doc_name` (string, optional): the document whose selection to read; default
  every open document.

The reply is a table of document, object, label, type and sub-elements
(`Face6`, `Edge12`, and so on), plus the picked points. Use it when the user
refers to "this face" or "the selected edge", then pass the names to `measure`
or `update_object`. An empty result means nothing is selected.

## Mesh operations

### `analyze_mesh`

Analyze a mesh object (`Mesh::Feature`, for example an imported STL or 3MF)
for the defects that break 3D printing and `mesh_to_solid`.

- `doc_name` (string, required)
- `obj_name` (string, required): the mesh object.
- `timeout` (number, optional, default 120, up to 1800 seconds).

Checks whether the mesh is a closed solid, non-manifold edges,
self-intersections, wrongly oriented facets, invalid points, corrupted facets
and separate components. The reply lists the issues found and the
`repair_mesh` steps that address them; the document is not changed.

### `repair_mesh`

Repair a mesh object in place by running named steps in order.

- `doc_name` (string, required)
- `obj_name` (string, required): the mesh object.
- `steps` (array of strings, optional): steps to run, from `fix_indices`,
  `remove_invalid_points`, `remove_duplicated_points`,
  `remove_duplicated_facets`, `fix_degenerations`, `fix_deformations`,
  `remove_non_manifolds`, `remove_non_manifold_points`,
  `fix_self_intersections`, `remove_folds`, `harmonize_normals`,
  `flip_normals`, `fill_holes`. Default: `fix_indices`,
  `remove_duplicated_points`, `remove_duplicated_facets`,
  `fix_degenerations`, `remove_non_manifolds`, `fix_self_intersections`,
  `harmonize_normals`, `fill_holes`.
- `fill_holes_max_edges` (integer, default 20, 3 to 10000): the largest hole
  `fill_holes` closes, by its edge count.
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).
- `timeout` (number, optional, default 300, up to 1800 seconds).

Run `analyze_mesh` first and pass the steps it suggests, or omit `steps` for
the standard sequence above. The reply compares the mesh before and after each
step; undo reverts the whole repair in one step.

### `mesh_to_solid`

Convert a mesh object into a new Part solid that booleans, fillets and export
to STEP can work with.

- `doc_name` (string, required)
- `obj_name` (string, required): the mesh object.
- `result_name` (string, optional): name for the new object; default
  `obj_name` followed by `_solid`.
- `tolerance` (number, default 0.1, up to 10 mm): distance within which mesh
  edges are sewn together.
- `refine` (boolean, default `false`): merge coplanar triangles into larger
  faces, slower but lighter to model on.
- `force` (boolean, default `false`): convert meshes over 200000 facets, which
  can take minutes and much memory.
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).
- `timeout` (number, optional, default 300, up to 1800 seconds).

Each triangle becomes its own face, so the result has as many faces as the
mesh had triangles unless `refine` merges the coplanar ones together
afterwards; `refine` adds time on top of the conversion itself, in exchange
for a solid with far fewer faces to fillet, boolean or export later. A mesh
that is not closed gives a shell, not a solid; run `analyze_mesh` and
`repair_mesh` first. Over 200000 facets without `force` is a conflict error
with a hint to retry with `force` true. The mesh object is kept.

### `solid_to_mesh`

Tessellate an object's shape (a Part feature, Body, Link or `App::Part`) into
a new mesh object.

- `doc_name` (string, required)
- `obj_name` (string, required): the object with a shape.
- `result_name` (string, optional): default `obj_name` followed by `_mesh`.
- `quality` (string, default `"standard"`, one of `coarse`, `standard`,
  `fine`): tessellation preset.
- `linear_deflection` (number, optional, 0.001 to 100 mm): overrides the
  preset's largest distance between the surface and its triangles.
- `angular_deflection_deg` (number, optional, 0.5 to 90 degrees): overrides
  the preset's largest angle between neighbouring triangles.
- `relative` (boolean, default `false`): `linear_deflection` relative to each
  edge's length.
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).
- `timeout` (number, optional, default 300, up to 1800 seconds).

Use it to inspect or repair the triangles a printer will get, with
`analyze_mesh`; `export_document` tessellates by itself, so this is not needed
before exporting. The source object is kept.

## Spreadsheets

### `get_spreadsheet_cells`

Read cells of a FreeCAD spreadsheet (`Spreadsheet::Sheet`).

- `doc_name` (string, required)
- `sheet_name` (string, required): the `Spreadsheet::Sheet` object.
- `cells` (array of strings, optional): addresses such as `B2`, ranges such as
  `A1:C10`, or aliases; default every non-empty cell, up to 2000.

Spreadsheets usually hold the parameters that drive a parametric model through
expressions. The reply is a table of cell, alias, content as entered (such as
`=Length*2`), computed value, and any error for that cell. An unknown sheet
name is a not-found error with a hint to create one with `create_object` and
`obj_type` `Spreadsheet::Sheet`.

### `update_spreadsheet_cells`

Set the content and aliases of cells, then recompute the document so
dependent objects update.

- `doc_name` (string, required)
- `sheet_name` (string, required)
- `cells` (array, required, 1 to 500 entries): each `{"cell": address or
  alias, "content"?: string, "alias"?: string}`, needing at least one of
  `content` or `alias`. Content is a number with an optional unit, text, or an
  expression starting with `=`; an empty string clears a cell or removes an
  alias.
- `recompute` (boolean, default `true`): recompute afterwards so dependent
  objects update.

The reply shows each changed cell's new value and any objects that became
invalid; undo reverts the whole change in one step. A bad address or alias in
the batch (already used, syntactically invalid, or a reserved word such as a
unit or a constant) is rejected before anything is changed. A failure caught
only once FreeCAD applies it rolls this call's own changes back: when it
opened its own transaction, aborting it leaves no undo step at all; joined
into an already-open transaction (a command or task panel active in FreeCAD),
it restores each already-applied cell by hand instead. A sheet left invalid by
its own erroring cells is not fixed with `update_object`; the reply points at
`get_spreadsheet_cells` with the erroring cell addresses instead.

## Import and export

### `import_file`

Import a CAD, mesh or 2D file into a document, without any dialog.

- `path` (string, required): absolute path of the file to import.
- `doc_name` (string, optional): the open document to import into; omit to
  create a new document named after the file.
- `merge` (boolean, optional): STEP, IGES and glTF only, merge the file's
  parts into one compound; default FreeCAD's import preference.
- `use_link_group` (boolean, optional): STEP, IGES and glTF only, build
  assemblies from `App::Link` groups; default FreeCAD's import preference.
- `import_hidden` (boolean, optional): STEP, IGES and glTF only, also import
  objects the file marks hidden; default FreeCAD's import preference.
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).
- `timeout` (number, optional, default 300, up to 1800 seconds): raise it for
  large assemblies.

Supported extensions: `.step`/`.stp`, `.iges`/`.igs`, `.gltf`/`.glb`
(assemblies keep their parts and colors), `.brep`/`.brp`, `.stl`/`.ast`,
`.obj`, `.off`, `.ply`, `.3mf` (triangle meshes), `.dxf` and `.svg` (2D
geometry). The reply lists the created objects with their names for
`get_object` and `update_object`, the importer used, and any objects left
invalid by the recompute. Undo removes the whole import in one step.

Mesh files become Mesh objects, not solids: call `mesh_to_solid` to turn one
into a Part solid, or `analyze_mesh` and `repair_mesh` to fix it first. An SVG
without absolute units and no recognized Inkscape version marker imports at an
assumed 96 dpi rather than asking, with a warning in the reply saying so. Use
`open_document` for `.FCStd` files.

### `export_document`

Export objects of a document to a file for 3D printing or CAD exchange,
without any dialog.

- `doc_name` (string, required)
- `path` (string, required): absolute path to write; its extension picks the
  format.
- `object_names` (array of strings, optional): objects to export; default the
  visible top-level objects with geometry.
- `overwrite` (boolean, default `false`)
- `include_hidden` (boolean, default `false`): include hidden top-level
  objects in the default set.
- `recompute` (boolean, default `true`)
- `quality` (string, default `"standard"`, one of `coarse`, `standard`,
  `fine`): mesh formats and glTF tessellation preset.
- `linear_deflection` (number, optional, 0.001 to 100 mm): overrides the
  preset; mesh formats and glTF.
- `angular_deflection_deg` (number, optional, 0.5 to 90 degrees): overrides
  the preset; mesh formats only.
- `relative` (boolean, default `false`): mesh formats, `linear_deflection`
  relative to each edge's length.
- `ascii` (boolean, default `false`): STL only, write ASCII instead of
  binary.
- `step_unit` (string, optional, one of `MM`, `M`, `INCH`): STEP and IGES
  only; default FreeCAD's export preference.
- `step_schema` (string, optional, one of `AP203`, `AP214IS`, `AP242DIS`):
  STEP only; default FreeCAD's export preference.
- `timeout` (number, optional, default 300, up to 1800 seconds): raise it for
  fine meshes of large models.

The format follows the extension of `path`: `.stl` (binary, or ASCII with
`ascii` true), `.ast`, `.3mf`, `.amf`, `.obj`, `.ply` and `.off` are triangle
meshes for slicers, tessellated with `quality` or explicit
`linear_deflection`/`angular_deflection_deg`; 3MF and AMF keep one object per
part and declare millimeters. `.step`/`.stp` and `.iges`/`.igs` keep exact
geometry, names and colors. `.glb`/`.gltf` write a tessellated scene in
meters; `.gltf` also writes a separate `.bin` buffer next to it, named in the
reply's `companion_file`. `.brep`/`.brp` write the exact shape. `.FCStd`
writes a copy of the document. `.dxf` and `.svg` write 2D geometry projected
on the XY plane.

Without `object_names`, the visible top-level objects with geometry are
exported, so a Body is written once, not once per feature. The reply gives the
file, its size, the exported and skipped objects and, for meshes, the facet
count and whether the mesh is closed. An existing file is only replaced with
`overwrite` true; a missing target directory is an invalid-input error. Use
`save_document_as` to save the document itself, and `check_printability`
before exporting for a printer.

## Printability

### `check_printability`

Check whether objects are ready for 3D printing, before `export_document`
writes them to STL or 3MF.

- `doc_name` (string, required)
- `object_names` (array of strings, optional): default the visible top-level
  solids and meshes.
- `bed_x`, `bed_y`, `bed_z` (numbers, optional, up to 10000 mm each): printer
  bed dimensions; give all three together to check the fit, or omit all
  three.
- `build_direction` (string, default `"+Z"`, one of `+Z`, `-Z`, `+X`, `-X`,
  `+Y`, `-Y`): the model axis that points up on the printer.
- `overhang_angle_deg` (number, default 45, 0 to 89 degrees): overhangs
  steeper than this from vertical count as needing support.
- `check_self_intersections` (boolean, default `true`): also look for
  self-intersecting triangles, slower on large meshes.
- `quality` (string, default `"standard"`, one of `coarse`, `standard`,
  `fine`): tessellation preset for the mesh checks.
- `linear_deflection`, `angular_deflection_deg` (numbers, optional): override
  the preset.
- `timeout` (number, optional, default 300, up to 1800 seconds).

For each object, the reply reports whether its shape is valid, closed and how
many solids it has, FreeCAD's full shape check, whether its tessellated mesh
is closed without non-manifold edges or self-intersections, its size along
the build axes and, with the bed dimensions given, whether it fits (turned by
90 degrees if needed), and the area of overhangs needing support. `printable`
is true only when at least one object was checked and none of them has an
issue; it is false, not vacuously true, when `object_names` names nothing or
no visible top-level solid or mesh exists to check. Mesh objects are checked
as they are; fix them with `repair_mesh`. Invalid shapes usually come from a
failed feature; `recompute_document` shows which one.

## FEM analysis

### `run_fem_analysis`

Run the CalculiX solver on an existing FEM analysis container and return
summary results.

- `doc_name` (string, required)
- `analysis_name` (string, required): the `Fem::AnalysisPython` object.
- `timeout` (integer, optional, default 600, 1 to 3600 seconds, an hour).
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).

Prerequisites in the document, all created with `create_object`:

- A Part-derived solid (for example `Part::Box`, `PartDesign::Body`) as the
  geometry.
- A `Fem::AnalysisPython` container.
- A `Fem::MaterialCommon` assigned to the geometry, added to the analysis.
- A `Fem::FemMeshGmsh` referencing the geometry, added to the analysis (the
  mesh is generated automatically when created).
- At least one `Fem::ConstraintFixed` and one `Fem::ConstraintForce` (or
  `ConstraintPressure`) bound to faces of the geometry, added to the analysis.

A CalculiX solver already in the analysis is reused; a `SolverCcxTools` is
created when it has none. The solver runs synchronously on FreeCAD's GUI
thread, so other tools that need it wait until the analysis finishes; do not
send parallel requests. `get_rpc_status` and `get_async_status` stay
answerable while it runs.

The reply gives the maximum and minimum von Mises stress (MPa), the maximum
displacement (mm), the node count, the result object's name, and the working
directory CalculiX wrote to. On failure it returns the prerequisite check or
solver error with the working directory for triage; `list_objects` shows what
the analysis holds.

See [`examples/cantilever_fem.py`](../examples/cantilever_fem.py) for an
end-to-end example, including geometry, material, mesh, constraints, and an
analytical comparison. For long analyses, configure the client to allow the
[queue and execution timeout budgets](execution.md#gui-dispatch-timeouts).

## Recompute, undo and redo

### `recompute_document`

Recompute every object of a document and report each that failed or is still
touched.

- `doc_name` (string, required)
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).
- `timeout` (number, optional, default 120, up to 1800 seconds).

Use it after a series of changes, after `delete_object` (dependents may
break), or when `open_document` reports the document needs a recompute. The
reply lists each failed object with FreeCAD's status message, and each object
still touched afterwards. Fix a failed object with `update_object` or remove
it with `delete_object`; failures do not make the call itself fail.

### `undo`

Undo the last changes to a document, one transaction per step.

- `doc_name` (string, required)
- `steps` (integer, default 1, 1 to 100): how many transactions to walk.
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).

Every tool that changes a document records its changes as one transaction
named after the tool, such as `MCP: create_object`, next to edits made by hand
in FreeCAD. The reply names the transactions undone and those left to undo or
redo. Refused while a task panel is open in FreeCAD.

### `redo`

Redo changes that `undo` reverted, one transaction per step, in the order
they were undone.

- `doc_name` (string, required)
- `steps` (integer, default 1, 1 to 100)
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).

A new change made after an undo clears what can be redone. Refused while a
task panel is open in FreeCAD.

## Code execution

See [code execution](execution.md) for execution modes, shared script state,
background job tracking, and GUI dispatch timeout handling in detail.

### `execute_code`

Execute Python code in FreeCAD on its GUI thread and wait for the result. The
safe default for automation the other tools do not cover: `FreeCAD`,
`FreeCADGui` and the document are available, and whatever the code prints is
returned.

- `code` (string, required): the Python code to execute.
- `timeout` (number, optional, up to 1800 seconds): seconds for each of the
  queue and GUI execution budgets, overriding the 90 second default; raise it
  for other slow work that must run on the GUI thread.
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).

Use `import_file` and `export_document` for file exchange instead of
scripting it here; prefer `execute_code_async` for heavy pure-geometry work
that touches neither the document nor the GUI, and `execute_code_headless`
for OCCT work that may crash FreeCAD. Without a large enough `timeout`, a
slow call reports a timeout while the task keeps running, and its result is
lost.

### `execute_code_async`

Execute Python code in FreeCAD without waiting for completion, for
long-running background computations that do not touch the GUI or mutate the
document tree directly.

- `code` (string, required): background-safe Python code; use `commit(fn)` for
  every document and view write.

The call returns at once with a `job_id`; poll `get_async_status` with it. The
code runs in a background thread and must not call `FreeCADGui` APIs,
manipulate the active view or selection, create or edit document objects,
change object properties, call `doc.recompute()`, or save documents directly,
since FreeCAD documents and the scenegraph are not thread-safe. Every document
or view write instead goes through the injected `commit(fn, timeout=120)`
helper, which queues `fn` on the GUI thread, waits for it, and raises
`RuntimeError` on failure or timeout. Use this only when the heavy part is
long-running OCCT geometry or other CPU-bound work that would exceed
`execute_code`'s budget.

### `get_async_status`

Report the state of background jobs started by `execute_code_async`.

- `job_id` (string, optional): the job to report on; omit to list all running
  jobs and up to 20 recently finished ones.

Reports whether a job is running, done or failed, with the error and
traceback of a failed job. It does not use the GUI thread, so it answers even
while a job runs. History is held in memory until FreeCAD exits. An unknown
`job_id` is a not-found error.

### `execute_code_headless`

Run a Python script in a separate `freecadcmd` process, isolated from the
running GUI.

- `code` (string, required): a complete Python script for `freecadcmd`.
- `timeout` (number, optional, default 600, up to 604800 seconds, a week):
  seconds to wait before killing the process; partial output is kept on
  timeout.

Use this for OCCT work that can crash or block FreeCAD: helical threads
(`makeHelix` + `makePipeShell`), lofts and sweeps, booleans with many or
B-spline tools, long parametric rebuilds. A native crash only kills the
helper process; the GUI and its open documents survive, and the reply reports
the crash (by signal or, on Windows, exception name) together with the
script's output.

The script runs on the machine hosting this MCP server, independently of
`FREECAD_MCP_HOST`, in a fresh process without GUI: import `FreeCAD` and
`Part` yourself, open documents from disk (`FreeCAD.openDocument(path)`), and
save results with `doc.save()`/`saveAs()` or `Shape.exportBrep()`. Nothing
from the `execute_code` namespace is available. Print progress to stdout; it
is returned when the process ends. After the script saves a `.FCStd` that is
open in the GUI, call `reload_document` to show the result.

## View and screenshots

### `get_view`

Get a screenshot of a document's 3D view from the given orientation.

- `doc_name` (string, optional): the document whose 3D view to capture;
  default the active document's active view.
- `view_name` (string, default `"Isometric"`): one of `Isometric`, `Front`,
  `Top`, `Right`, `Back`, `Left`, `Bottom`, `Dimetric`, `Trimetric`.
- `width`, `height` (integers, optional, 1 to 2048 pixels).
- `focus_object` (string, optional): an object to frame; default fit all
  objects in the view.

With `doc_name`, that document's 3D view is captured, switching to it and
back if it is not already the active window; without it, the active
document's active view is used. Either way, FreeCAD's camera and selection
are left exactly as found afterwards. When `width` and `height` are both
omitted, the image has the viewport's size, scaled down to keep its aspect
ratio when the longest edge exceeds 1024 pixels; with only one given, the
other is the viewport's size in that direction. A reply carries at most 1
MiB, so an image too large to fit is refused with a hint to ask for a smaller
one.

Fails when no document is open, `doc_name` is not an open document, that
document has no 3D view (opened hidden, or all its 3D views closed), or,
without `doc_name`, the active window is not a 3D view, such as a TechDraw
page or a spreadsheet.

## Screenshot options

The following tools can return an optional screenshot of the model after the
change: `create_object`, `update_object`, `delete_object`, `list_objects`,
`get_object`, `import_file`, `repair_mesh`, `mesh_to_solid`, `solid_to_mesh`,
`recompute_document`, `undo`, `redo`, `execute_code`,
`insert_part_from_library`, and `run_fem_analysis`.

| Parameter | Default | Purpose |
| --- | --- | --- |
| `include_screenshot` | `true` (`false` for a compact `list_objects` call) | Set to `false` for text-only feedback, such as analytical scripts or intermediate steps. |
| `view_name` | `"Isometric"` | Orient the returned screenshot, for example `"Front"`, `"Top"`, or `"Right"`. |

The [`FREECAD_MCP_ONLY_TEXT_FEEDBACK` setting](configuration.md#text-feedback-and-screenshots)
suppresses these optional screenshots regardless of `include_screenshot`.

Use `get_view` to request a screenshot explicitly; it is available even with
`FREECAD_MCP_ONLY_TEXT_FEEDBACK` on, and takes `width`, `height` and
`focus_object` parameters the optional screenshots above do not. See
[view and screenshots](#view-and-screenshots) for its full details.

## Parts library

### `insert_part_from_library`

Insert a part from the [FreeCAD parts library](https://github.com/FreeCAD/FreeCAD-library)
addon into the active document.

- `relative_path` (string, required): the part's path inside the parts
  library, as `list_parts` shows it.
- `include_screenshot`, `view_name`: see [screenshot options](#screenshot-options).

Call `list_parts` first to find the part's relative path, then position it
with `update_object` and check it with `get_object`. A `relative_path` that
escapes the parts library, such as one using `..` to climb out of it, is
rejected as invalid; a path that stays inside the library but does not exist
there is a not-found error. Both hint to call `list_parts` to see the
available parts.

### `list_parts`

List the parts available in the FreeCAD parts library addon, as relative
paths for `insert_part_from_library`. Takes no arguments.

The list is empty when the parts library addon is not installed in FreeCAD;
build the shape with `create_object` instead in that case.

## Launch and status

### `start_freecad`

Start FreeCAD's GUI, with the MCP addon's RPC server, on the machine that
runs FreeCAD, when it is not running yet: the local machine, or, with
[remote access](remote-access.md) on, the computer sharing FreeCAD, through
its listener.

- `file` (string, optional): absolute path of an `.FCStd` file to open once
  FreeCAD has started, on the machine that runs FreeCAD.

The tool first checks whether FreeCAD already answers; if so it reports state
`already_running` and launches nothing, unless another agent holds it, in
which case it returns the "in use" error instead (see
[multi-agent rules](remote-access.md#multi-agent-rules)). Otherwise it starts
FreeCAD detached, with a startup macro that starts the RPC server on the
configured port, so the addon's auto-start setting does not matter. When
another FreeCAD window is already open without the RPC server, that FreeCAD
receives the request instead (state `forwarded`). For a FreeCAD that already
runs, use `open_document` instead of `file`.

FreeCAD takes about 15 seconds to start, longer on its first start. The reply
returns at once: call `get_rpc_status` every few seconds until it reports
`rpc: reachable`, then continue with `list_documents`. A second call while a
launch is already starting reuses it instead of starting FreeCAD again, and
says so. Set `FREECAD_MCP_FREECAD` in the client's config when FreeCAD is
installed somewhere the server does not find. With remote access on, the
client's config has no effect there instead: set `FREECAD_MCP_FREECAD` on
the FreeCAD computer itself before turning on Share this PC, or turn Share
this PC on again after setting it, so freecad-mcp detects it there.

### `get_rpc_status`

Check FreeCAD's health without using its GUI thread, so it answers even while
FreeCAD has not started yet, is still starting, has exited, or a GUI
operation elsewhere is stuck. Takes no arguments.

Reports `freecad` (`running`, `starting`, `not_running`, `exited` or
`unresponsive`) and `rpc` (`reachable` or `unreachable`). While reachable, the
reply carries the full status, including `gui_dispatch` (names a GUI
operation still stuck) and `version_check` (`ok`, or whether the addon or this
server needs updating), and the open documents. While not reachable, it
carries the process id, elapsed time since `start_freecad`, exit code and
launch log tail known so far, plus the documents last seen before FreeCAD
stopped answering.

With [remote access](remote-access.md) on, the reply also carries
`session_lock` (`off`, `free`, `yours` or `other`; `unknown` while FreeCAD is
down and remote access is known to be on regardless, since its actual state
cannot be read without FreeCAD; left out entirely when that is not knowable
either, because FreeCAD has not answered at all yet) and, while it is held,
`session_holder`, `session_idle_seconds` and `session_frees_in_seconds`: who
holds FreeCAD, or that this agent does, how long they have been idle, and
when the claim frees on its own. See
[multi-agent rules](remote-access.md#multi-agent-rules) for what claims it
and how `release_session` and `close_freecad` manage it.
While reachable, the reply also names the computer FreeCAD actually runs on
("FreeCAD is running on \<hostname\> and its RPC server answered"). When this
MCP server is configured to reach FreeCAD through `localhost` but another
computer answers there instead, every call, not only this one, is refused
until the setup is fixed: see
[WSL and Windows on the same computer](remote-access.md#wsl-and-windows-on-the-same-computer)
for why that happens and how to fix it.

This is the tool to poll after `start_freecad` until it reports `rpc:
reachable`, and to call whenever another tool times out, fails because
FreeCAD is in use by another agent, or FreeCAD's state is unclear. Once it
reports running, use `list_documents` for the full per-document detail.

### `release_session`

Free the FreeCAD session this agent holds, so another agent can use FreeCAD
at once instead of waiting for the idle timeout. Listed only while
[remote access](remote-access.md) is on. Takes no arguments.

With remote access on, an agent's first call to FreeCAD, other than
`start_freecad` and `get_rpc_status`, claims FreeCAD for that agent until it
has been idle for the configured timeout; `get_rpc_status` shows who holds it
and when it frees. This only frees this agent's own session: documents stay
open and unsaved changes stay unsaved, so call `save_document` first. When a
job this agent started (`execute_code_async`, or a GUI operation that outlived
its own timeout) is still running, the session stays held until that job
ends, then frees on its own.

### `close_freecad`

Quit FreeCAD on the computer that runs it and free the session, for example
at the end of a work session. Listed only while
[remote access](remote-access.md) is on.

- `discard_changes` (boolean, optional, default false): quit even when
  documents have unsaved changes, losing them.

Refused while there are unsaved changes and `discard_changes` is not true
(save them first with `save_document` or `save_document_as`), or while a
task panel or command is open in FreeCAD. FreeCAD closes a moment after the
reply; `start_freecad` opens it again.

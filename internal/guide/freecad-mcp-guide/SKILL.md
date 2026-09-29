---
name: freecad-mcp-guide
description: Workflows and rules for the freecad MCP server's tools. Use when building or changing FreeCAD models through create_object and update_object, choosing faces or edges with list_subelements, setting values with units or expressions, setting up and running a FEM analysis, checking and exporting parts for 3D printing, designing printed fits, threads and clips for a print material such as PETG or PLA, working with files on the FreeCAD computer, or sharing one FreeCAD with other agents.
metadata:
  generator: freecad-mcp
  version: "0.4.6"
---

# FreeCAD MCP guide

Call the tools of the freecad server by their bare names, such as create_object. Your client may add a prefix.

## Start

1. Call start_freecad when FreeCAD is not running. It reports already_running when FreeCAD answers.
2. Poll get_rpc_status every 3 to 5 seconds until it reports rpc: reachable. Give up after 120 seconds.
3. Call list_documents. Call create_document or open_document when it is empty.
4. Call list_objects with compact true to learn the object names.

## Rules

- Lengths in mm and angles in degrees may be numbers. Give every other quantity as a string with its unit: "100 N", "2 MPa", "210 GPa". A bare Force of 100 is 0.1 N. Read values.md.
- A value starting with "=" binds an expression: "=Params.thickness". "=" alone removes it.
- Link properties take object names: "Box", or ["Box", "Cylinder"] for a list.
- Never guess Face or Edge names. Call list_subelements first. Read subelements.md.
- Use the object name each reply returns. FreeCAD may rename Box to Box001.
- Paths are on the computer running FreeCAD. Nothing is transferred. Read files.md.
- Use execute_code only for what no tool covers. Read code.md.
- An error before the object exists creates nothing: fix the argument and call create_object again. An object that was created but does not compute stays and the reply names it: fix it with update_object or delete_object. Never repeat the call with the same name.
- Every changing call is one undo step. Call undo to revert.
- Changing tools attach a screenshot by default. Pass include_screenshot false on intermediate steps and call get_view at the end. get_object and list_objects attach none unless include_screenshot is true.

## Checklists

Build a part:
1. For a part that will be 3D printed and has holes, fits, threads or clips, read the material file for its material first, such as material-petg.md. Ask for the material when it is not known.
2. create_object for each solid, with Placement and dimensions in obj_properties.
3. create_object for booleans such as Part::Cut with Base and Tool as object names.
4. recompute_document after a series of changes. Fix each failed object with update_object.
5. get_object to check values. get_view to look at the result.
6. save_document_as for a new file, save_document for an existing one.

Change a part:
1. list_objects with compact true, then get_object for the object.
2. update_object with only the properties that change.
3. recompute_document, then check the reply for invalid objects.

Print a part:
1. Check that fits, threads and clips follow the material file for the part's material.
2. Lay each part flat on the plate with update_object on Placement, then check_printability with bed_x and bed_y.
3. Fix what it reports. Read printing.md.
4. export_document to .stl or .3mf with overwrite true when replacing.

Show the user a part:
1. set_view with view_name and focus to leave the 3D view as they should see it. Use mode orbit to turn around it, or mode tour with stops to visit objects one by one.
2. Call set_view with mode static to stop a running orbit or tour. get_view looks without changing the view.

Simulate a part (FEM):
1. Read fem.md. Create the analysis, material and mesh with create_object and analysis_name.
2. list_subelements on the solid for the fixed and loaded faces.
3. Create the constraints with analysis_name and those faces.
4. run_fem_analysis. Send no other call while it runs.

Drive a part from parameters:
1. Read parametric.md. Put the numbers in a Spreadsheet::Sheet and bind properties with "=".

Share FreeCAD with other agents:
1. Read session.md. Save, then call release_session or close_freecad when you stop.

## Guide files

- values.md: quantities, units, expressions, links, Placement, colors. Read before any create_object or update_object that sets more than a plain length.
- subelements.md: choosing faces and edges. Read before measure or a FEM constraint.
- fem.md: FEM from analysis to result. Read before the first FEM object.
- files.md: paths, formats, import, export, save. Read before touching a file.
- session.md: remote access, holding and releasing FreeCAD. Read when a call is refused as in use, or when get_rpc_status shows a holder.
- printing.md: laying parts on the plate, the layout check, export. Read before exporting for a printer.
- material-petg.md, material-pla.md: clearances, threads, clips, strength and limits for one print material. Read the user's material before modelling parts that fit together.
- parametric.md: spreadsheets as parameters. Read before binding a property to a cell.
- code.md: execute_code, execute_code_async, execute_code_headless. Read before writing a script.

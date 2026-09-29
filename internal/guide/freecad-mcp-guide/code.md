# Code

Use a tool when one exists. Use code only for what no tool covers.

## Scripts

- Build in small steps: one part or one feature per call. Print progress.
- A script longer than about 30 lines goes in a .py file. Write it with your own file tools and run it with path instead of code. To fix it, edit the file and run it again. Never send the whole script again.
- execute_code and execute_code_async read path on the computer running FreeCAD. With remote access that may not be your computer: then pass code instead. execute_code_headless reads path on the computer running this MCP server.
- A call that runs longer than the limit (30 minutes unless the user sets another) moves to the background and returns a job_id starting with call-. Poll get_async_status with it every minute or two. When the call ends, it returns the call's own reply. FreeCAD cannot stop such a call: cancel_job refuses it.

## execute_code

- Runs Python on FreeCAD's GUI thread. FreeCAD, FreeCADGui and the documents are available.
- Use it for quick changes only: it freezes FreeCAD while it runs. Work that may take more than a few seconds (booleans or distances between complex parts, threads, fillets, meshing) goes to execute_code_headless.
- Print what you need back. Whatever the code prints is the reply.
- It has a time budget to start and another to run, 90 seconds each unless the addon sets another. Pass timeout for slower work.
- Pass include_screenshot false for code that does not change the model.

## execute_code_async

- For long, CPU heavy geometry work such as a fuse, cut or loft on shapes you already fetched.
- It returns a job_id at once. Poll get_async_status with it.
- The code runs off the GUI thread. It must not touch the GUI or the document: no FreeCADGui, no view or selection calls, no object creation, no property changes, no recompute, no save. Doing so can hang FreeCAD.
- Hand every document or view write to the GUI thread with commit(fn), which runs fn there and returns its result.
- Pattern: fetch shapes into module variables with execute_code, compute in the background, then commit(apply).

## execute_code_headless

- Runs a script in a separate freecadcmd process without a GUI. An OpenCascade crash kills only that process.
- Use it for helical threads, lofts, sweeps, booleans with many tools and long rebuilds.
- It runs on the computer running this MCP server and shares nothing with execute_code. Import FreeCAD and Part, open files with FreeCAD.openDocument(path), save with doc.save() or Shape.exportBrep(), and print progress.
- After it saves a file that is open in FreeCAD, call reload_document.
- A script that may take minutes runs in the background: pass background true, or a timeout over 120 seconds. The call returns a job_id at once and the output streams to a file. Call get_async_status with the job_id every 30 to 60 seconds: it reports running or finished, the exit code, the elapsed seconds and the last 200 lines of output. Pass cancel true to stop the job. A finished job's output file is removed after you read it.
- Print progress in a long script, so get_async_status shows how far it got. Keep foreground calls short.
- A failed script still returns everything it printed before the error, then the traceback. The script runs as __main__ with __file__ set to its own path. Code you pass runs from a temporary file that is deleted afterwards, so do not look for files beside it; a script you pass as path stays where it is.

## Printable threads

Build a thread as a swept profile along a helix, in execute_code_headless, then fuse it to the core and trim the ends:

```python
import FreeCAD, Part
pitch, depth, major, length = 2.0, 1.0, 12.0, 10.0
r = major / 2
root = r - depth
helix = Part.makeHelix(pitch, length, root)
inset = 0.3  # the profile reaches into the core, so the fuse overlaps instead of touching
profile = Part.makePolygon([
    FreeCAD.Vector(root - inset, 0, -0.45 * pitch),
    FreeCAD.Vector(r, 0, -0.1 * pitch),
    FreeCAD.Vector(r, 0, 0.1 * pitch),
    FreeCAD.Vector(root - inset, 0, 0.45 * pitch),
    FreeCAD.Vector(root - inset, 0, -0.45 * pitch)])
thread = Part.Wire(helix).makePipeShell([profile], True, True)
core = Part.makeCylinder(root, length + pitch, FreeCAD.Vector(0, 0, -pitch / 2))
bolt = core.fuse(thread).common(Part.makeCylinder(r + 1, length)).removeSplitter()
print(bolt.isValid(), len(bolt.Solids), bolt.Volume)
```

- A profile that only touches the core (its inner edge on the core surface) gives an invalid fuse. Let it reach inside the core.
- Save the result with Shape.exportBrep(path) or into a document, then load it in the GUI with execute_code or reload_document.
- Leave clearance of 0.2 to 0.4 mm between a printed thread and its nut.

## Rounding edges

- Round with one or two Shape.makeFillet(radius, edges) calls per part, one call for each set of edges that share a radius.
- For the edges a call fails on, make one Shape.makeChamfer(size, edges) call instead.
- Never retry edge by edge in a loop, and never search the whole shape for edges again after each try: every retry costs a full boolean.
- Run the script with execute_code_headless, print a line after each call, and use a background job for anything that may take minutes.

## Traps

- Assigning a translated shape to obj.Shape sets obj.Placement from the shape's location, so setting obj.Placement afterwards throws the translation away. Set the Placement alone on an untransformed shape, or bake the move into the geometry with Shape.transformGeometry(matrix) so the shape carries an identity location.
- A Placement or Rotation with angle 0 loses its axis: FreeCAD.Rotation(FreeCAD.Vector(0, 0, 1), 0) is the identity and reads back as axis (0, 0, 1) angle 0 whatever axis you gave. Do not read an axis from a zero angle.
- Shape.BoundBox is loose on curved shapes, and get_object reports it. Use Shape.optimalBoundingBox(False, False) for the true size; check_printability reports that one.
- A Part.Compound, which booleans often return, has no CenterOfMass. Take Shape.Solids[0], or iterate Shape.Solids and use each solid's CenterOfMass.
- A helical thread cutter swept along a Part.makeHelix whose height is not a whole number of pitches can silently give an empty or zero-volume solid. Make the helix height a whole number of turns, then check the cutter's Volume > 0 before cutting.
- Heavy geometry (sweeps, threads, many-tool booleans) belongs in execute_code_headless: a crash there does not take FreeCAD down.

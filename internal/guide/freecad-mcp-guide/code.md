# Code

Use a tool when one exists. Use code only for what no tool covers.

## execute_code

- Runs Python on FreeCAD's GUI thread. FreeCAD, FreeCADGui and the documents are available.
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

# Files and paths

## Where paths live

- Every path is on the computer running FreeCAD. With remote access that is another computer than yours.
- Nothing is transferred. Copy files between computers yourself, for example with scp.
- execute_code_headless is the exception: its script runs on the computer running this MCP server, so its paths are on that computer.
- Pass absolute paths. Relative paths are refused.

## Which tool for which file

| Task | Tool |
| --- | --- |
| Open a FreeCAD file | open_document |
| Bring in STEP, IGES, glTF, BREP, STL, OBJ, PLY, 3MF, DXF or SVG | import_file |
| Save the document to its own file | save_document |
| Save to a new file or write a copy | save_document_as |
| Write STL, 3MF, STEP, IGES, BREP, glTF, DXF, SVG or a copy as .FCStd | export_document |
| Show changes a script wrote to a file | reload_document |

## Rules

- import_file with doc_name adds to that document. Without doc_name it creates a document named after the file.
- A mesh file arrives as a Mesh object. Call mesh_to_solid for a solid. Read printing.md.
- An SVG without units imports at 96 dpi. The reply warns.
- save_document needs a document that has a file. A new document needs save_document_as first.
- export_document and save_document_as create missing folders and say so in the reply.
- An existing file is replaced only with overwrite true.
- export_document without object_names writes the visible top-level objects with geometry. A Body is written once.
- Formats: 3MF and AMF keep one object per part, in mm. glTF is in meters, and .gltf writes a .bin beside it. DXF and SVG are projected on XY.
- Check the reply for skipped objects and warnings.

## Scripts that write files

1. Run the script with execute_code_headless. Save with doc.save().
2. Call reload_document for the document if it is open in FreeCAD.

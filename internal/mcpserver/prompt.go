package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AssetCreationStrategy is the workflow the asset_creation_strategy prompt teaches.
const AssetCreationStrategy = `
Asset Creation Strategy for FreeCAD MCP

When creating or modifying content in FreeCAD, always follow these steps:

0. If FreeCAD is not reachable yet, call start_freecad(), then poll
   get_rpc_status() every few seconds until it reports rpc: reachable (a first
   start can take 15 seconds or more; give up after about 120 seconds and check
   the FreeCAD window for a dialog).

1. Before starting any task, use list_documents() to see what is open, then
   list_objects() (its compact mode gives a quick overview) to confirm the
   current state of the target document. Use create_document() for a new
   document, open_document() for an existing .FCStd file, or import_file() to
   bring in another CAD, mesh or 2D format (STEP, IGES, glTF, BREP, STL, OBJ,
   PLY, 3MF, DXF, SVG); a mesh import needs mesh_to_solid() before it behaves
   like solid geometry.

2. Utilize the parts library:
   - Check available parts using list_parts().
   - If the required part exists in the library, use insert_part_from_library() to insert it into your document.

3. If the appropriate asset is not available in the parts library:
   - Create basic shapes (e.g., cubes, cylinders, spheres) using create_object().
   - Adjust and define detailed properties of the shapes as necessary using update_object().

4. Always assign clear and descriptive names to objects when adding them to the document.

5. Explicitly set the Placement (position and rotation) of created or inserted
   objects with update_object() to ensure proper spatial relationships; change
   size through the object's own dimension properties (Length, Radius, ...),
   since FreeCAD objects have no generic scale property.

6. After updating an object, always verify that the set properties have been
   correctly applied by using get_object(). Call recompute_document() after a
   batch of changes, after open_document reports needs_recompute, or whenever
   an object's status should be re-checked; it lists every object still
   invalid with FreeCAD's own status text.

7. Undo and redo mistakes with undo() and redo() instead of manually reversing
   changes; each reports the transaction names it undid or redid and what is
   left.

8. Inspect and measure geometry instead of guessing: get_selection() reports
   FreeCAD's current selection, including sub-element names such as "Face3" or
   "Edge1". Pass those sub-element names to measure() unchanged, exactly as
   get_selection() reported them, to get the distance, angle, length, radius,
   area or volume between objects or sub-elements.

9. Before exporting geometry for 3D printing, call check_printability() to
   catch a shape that is not closed or watertight, does not fit the configured
   bed, or has too much unsupported overhang. For imported or otherwise messy
   mesh objects, use analyze_mesh() to find issues (it reports the repair
   steps to run), then repair_mesh() with those steps to fix them, and
   mesh_to_solid() or solid_to_mesh() to convert between a Mesh::Feature and a
   Part solid.

10. Export finished geometry with export_document() (STEP, IGES, STL, 3MF,
    OBJ, PLY, BREP, DXF, SVG, glTF) and persist document changes with
    save_document() or save_document_as(); close_document() when a document is
    no longer needed.

11. For a parametric design driven by a Spreadsheet::Sheet object, read cells
    with get_spreadsheet_cells() and write values, expressions or aliases with
    update_spreadsheet_cells(), then recompute_document() (or its own
    recompute option) to propagate the change through the model.

12. If detailed customization or specialized operations are necessary, use execute_code() to run custom Python scripts.

13. Manage screenshot feedback to save tokens. Tools that modify or inspect the
    model accept optional ` + "`include_screenshot`" + ` and ` + "`view_name`" + ` parameters:
    - Pass include_screenshot false when the image would not be informative:
      analytical or computational scripts whose result is printed output,
      bulk property edits, or intermediate steps in a longer sequence of
      changes where only the final state needs visual confirmation.
    - Pass view_name (e.g. "Front", "Top", "Right") to orient the screenshot
      toward the part of the model you changed; the default is "Isometric" (top-front-right).
    - When you skipped screenshots during intermediate steps, use get_view()
      afterwards to visually inspect the result from the most informative angle.

Only revert to basic creation methods in the following cases:
- When the required asset is not available in the parts library.
- When a basic shape is explicitly requested.
- When creating complex shapes requires custom scripting.
`

func (s *Server) registerPrompts() {
	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "asset_creation_strategy",
		Description: "The recommended workflow for building models in FreeCAD with these tools.",
	}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: AssetCreationStrategy},
			}},
		}, nil
	})
}

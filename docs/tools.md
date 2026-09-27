# Tools

[Back to README](../README.md) · [Configuration](configuration.md) · [Code execution](execution.md)

## Available tools

| Tool | Purpose |
| --- | --- |
| `create_document` | Create a new FreeCAD document. |
| `list_documents` | List open documents. |
| `reload_document` | Close and reopen a saved document to pick up external file changes, such as results from a headless script. |
| `create_object` | Create an object in a document. |
| `update_object` | Set properties of an existing object. |
| `delete_object` | Delete an object from a document. |
| `list_objects` | List all objects in a document with their properties. |
| `get_object` | Get one object in a document. |
| `get_view` | Get a screenshot of the active view. |
| `execute_code` | Execute Python code on FreeCAD's GUI thread. |
| `execute_code_async` | Start a background computation and return its job ID; use `commit()` for document and view access. |
| `get_async_status` | Get background job state and failure tracebacks without using the GUI thread. |
| `execute_code_headless` | Run a script in a separate `freecadcmd` process and return its exit status and output. |
| `get_rpc_status` | Report RPC and GUI-dispatch health, addon version, and version check without using the GUI thread. |
| `insert_part_from_library` | Insert a part from the [FreeCAD parts library](https://github.com/FreeCAD/FreeCAD-library). |
| `list_parts` | List parts in the [FreeCAD parts library](https://github.com/FreeCAD/FreeCAD-library). |
| `run_fem_analysis` | Run CalculiX on an existing analysis and return summary results. |

See [code execution](execution.md) for execution modes, shared script state,
background job tracking, and timeout handling.

Placements use degrees: a `Rotation`'s `Angle` is given to `create_object` and
`update_object` in degrees, and `get_object` and `list_objects` report it in
degrees.

## Screenshot options

The following tools return optional screenshots: `create_object`, `update_object`,
`delete_object`, `execute_code`, `insert_part_from_library`, `list_objects`,
`get_object`, and `run_fem_analysis`.

| Parameter | Default | Purpose |
| --- | --- | --- |
| `include_screenshot` | `true` | Set to `false` for text-only feedback, such as analytical scripts or intermediate steps. |
| `view_name` | `"Isometric"` | Orient the returned screenshot, for example `"Front"`, `"Top"`, or `"Right"`. |

The [`FREECAD_MCP_ONLY_TEXT_FEEDBACK` setting](configuration.md#text-feedback-and-screenshots)
suppresses these optional screenshots regardless of `include_screenshot`.

Use `get_view` to request a screenshot explicitly; it is available even with
`FREECAD_MCP_ONLY_TEXT_FEEDBACK` on. It takes optional `view_name` (default
`Isometric`), `width`, `height`, and `focus_object` parameters. Supported views
are `Isometric`, `Front`, `Top`, `Right`, `Back`, `Left`, `Bottom`, `Dimetric`,
and `Trimetric`.

`width` and `height` are pixels from 1 to 2048. With both omitted, the image
has the viewport's size, scaled down to keep its aspect ratio when its longest
edge exceeds 1024 pixels. With only one given, the other is the viewport's size
in that direction. A reply carries at most 1 MiB, so a PNG larger than about
740 KiB is refused with a hint to ask for a smaller size; an optional
screenshot that would not fit is left out. `get_view` reports an error when no
document is open, or when the active view cannot be captured, for example when
a TechDraw page or a spreadsheet is active.

## FEM analysis

`run_fem_analysis` runs the CalculiX solver on an existing FEM analysis
container (a `Fem::AnalysisPython`, created with `create_object`). It uses a
CalculiX solver already in the analysis, or creates a `SolverCcxTools` when
there is none, and returns the maximum and minimum von Mises stress (MPa), the
maximum displacement (mm), the node count, the name of the result object, and
the solver's working directory. `timeout` is from 1 to 604800 seconds (a
week); the default is 600. The solver runs on FreeCAD's GUI thread, so other tools that need
it wait until the analysis finishes; `get_rpc_status` and `get_async_status`
stay available.

See [`examples/cantilever_fem.py`](../examples/cantilever_fem.py) for an end-to-end
example, including geometry, material, mesh, constraints, and an analytical
comparison. For long analyses, configure the client to allow the
[queue and execution timeout budgets](execution.md#gui-dispatch-timeouts).

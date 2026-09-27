// Package addon embeds the FreeCAD addon so the binary can install it into
// FreeCAD's Mod directory. The addon itself is Python and runs inside
// FreeCAD with FreeCAD's bundled interpreter.
package addon

import "embed"

// Name is the addon's directory name inside FreeCAD's Mod directory.
const Name = "FreeCADMCP"

// Files holds addon/FreeCADMCP. "all:" keeps __init__.py, which a plain
// pattern would skip for its leading underscore.
//
//go:embed all:FreeCADMCP
var Files embed.FS

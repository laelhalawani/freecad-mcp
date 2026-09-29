# Spreadsheets as parameters

Keep the numbers that drive a model in one Spreadsheet::Sheet. Change a cell and the model follows.

## Set up

1. Create the sheet:
   {"obj_type": "Spreadsheet::Sheet", "obj_name": "Params"}
2. Fill it and name the cells with update_spreadsheet_cells:
   {"sheet_name": "Params", "cells": [{"cell": "A1", "content": "thickness"}, {"cell": "B1", "content": "5 mm", "alias": "thickness"}]}
3. Bind a property with update_object:
   {"obj_properties": {"Height": "=Params.thickness"}}

Bind one part of a Placement the same way: {"Placement.Base.z": "=Params.thickness"}. Read values.md.

## Rules

- The alias is used as <sheet name>.<alias>, here Params.thickness.
- content is a number with an optional unit ("10 mm"), text, or an expression starting with "=" ("=B1 * 2").
- An empty content clears a cell. An empty alias removes the alias.
- update_spreadsheet_cells recomputes by default, so bound properties update at once. The reply shows each new value and the objects that became invalid.
- In the content column of the reply, FreeCAD marks an expression or a number with a unit with a leading =, and text with a leading '. A plain number has no mark.
- Read cells back with get_spreadsheet_cells. Without cells it returns every non-empty cell.
- To unbind, call update_object with the property set to "=".

## Through cuts

- Make the height of a through-cut tool exceed the driven thickness, for example "=Params.thickness + 2 mm", so the hole stays through when the thickness changes.

## Undo

- Each update is one transaction. Call undo to revert a cell change and everything it moved.

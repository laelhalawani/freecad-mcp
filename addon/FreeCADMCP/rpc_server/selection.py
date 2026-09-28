"""Report what the user selected in FreeCAD."""

from typing import Any

import FreeCADGui

from rpc_server.gui_task import run_on_gui
from rpc_server.lookup import require_document


# Fixed budget: get_selection takes no timeout argument.
_TIMEOUT = 60.0


def _get_selection_gui(doc_name: str | None) -> dict[str, Any]:
    # A misspelt or closed document name would otherwise resolve no document
    # and quietly report an empty selection (Gui/Selection/Selection.cpp
    # getObjectList "*" or a named document that is not open both give []).
    if doc_name:
        _doc, error = require_document(doc_name)
        if error is not None:
            return error
    # resolve=0: report sub-element names as selected, without resolving
    # through links or grouped sub-objects.
    items = FreeCADGui.Selection.getSelectionEx(doc_name or "*", 0)
    rows: list[dict[str, Any]] = []
    for item in items:
        obj = item.Object
        label = getattr(obj, "Label", item.ObjectName) if obj is not None else item.ObjectName
        type_id = getattr(obj, "TypeId", item.TypeName) if obj is not None else item.TypeName
        rows.append(
            {
                "document": item.DocumentName,
                "object": item.ObjectName,
                "label": label,
                "type": type_id,
                "sub_elements": list(item.SubElementNames),
                "picked_points": [[p.x, p.y, p.z] for p in item.PickedPoints],
            }
        )
    return {"success": True, "count": len(rows), "selection": rows}


def get_selection(doc_name: str | None = None) -> dict[str, Any]:
    """Return the selection of ``doc_name``, or of every document.

    Reply: ``{"success", "count", "selection": [{"document", "object",
    "label", "type", "sub_elements", "picked_points"}]}``. GUI thread, 60 s.
    """
    return run_on_gui(lambda: _get_selection_gui(doc_name or None), _TIMEOUT, "get_selection")

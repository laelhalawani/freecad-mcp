"""Print-layout check: parts lie inside the plate and do not overlap.

The agent lays each part flat on the plate (positions in the document, x and y
from 0, z up from 0); this checks the layout without meshing anything: every
part's tight bounding box lies inside the plate box, and no two parts share
volume (the volume of their solid intersection, not only of their bounding
boxes, which a bracket around another part would overlap). Each part reports
its size and its free margin to the plate edges.

Boolean common of two heavy shapes can take long, and it runs on FreeCAD's GUI
thread, so pairs are checked only while the call's timeout has time left; the
pairs left are listed as not checked, never guessed.

GUI thread only.
"""

import math
import time
from typing import Any

from rpc_server import tessellation
from rpc_server.errors import INVALID_INPUT, fail
from rpc_server.gui_task import resolve_timeout, run_on_gui
from rpc_server.lookup import require_document, require_object
from rpc_server.options import check_options
from rpc_server.plate import TOUCH_TOLERANCE_MM as _TOUCH_TOLERANCE_MM
from rpc_server.plate import plate_margins
from rpc_server.serialize import bound_box_list, finite_or_none, tight_bound_box, visibility_of


PRINTABILITY_TIMEOUT = 120.0

# The MCP tool name, used for the run_on_gui timeout hint.
_TOOL_NAME = "check_printability"

# Keys accepted in ``options``.
PRINTABILITY_OPTIONS = ("bed", "origin")

# Once the remaining share of the call's timeout drops under this fraction, the
# intersection of the pairs still to check is skipped rather than risk running
# past the caller's budget.
_BUDGET_GUARD_FRACTION = 0.25

# Two boxes closer than this many mm on every axis count as touching; the
# intersection volume must exceed this many mm^3 to count as an overlap, so
# parts that only share a face do not.
_OVERLAP_VOLUME_MM3 = 1e-3


def check_printability(
    doc_name: str,
    object_names: list[str] | None = None,
    options: dict[str, Any] | None = None,
    timeout: Any = None,
) -> dict[str, Any]:
    """Check that parts lie inside a plate and do not overlap.

    ``options["bed"]`` is ``[x, y]`` or ``[x, y, z]`` in mm, the plate (and
    build height) the parts must lie inside; ``options["origin"]`` is the
    plate's ``[x, y]`` corner (default ``[0, 0]``, z starts at 0). Reply:
    ``{"success", "document", "printable", "settings", "objects", "overlaps",
    "not_checked"}``. GUI thread, default timeout ``PRINTABILITY_TIMEOUT``. No
    transaction.
    """
    timeout = resolve_timeout(timeout, PRINTABILITY_TIMEOUT)
    if isinstance(timeout, dict):
        return timeout

    options, error = check_options(options, PRINTABILITY_OPTIONS)
    if error is not None:
        return error

    bed = options.get("bed")
    if (
        not isinstance(bed, (list, tuple))
        or len(bed) not in (2, 3)
        or any(isinstance(v, bool) or not isinstance(v, (int, float)) or v <= 0 for v in bed)
    ):
        return fail(
            INVALID_INPUT,
            "bed must be [x, y] or [x, y, z] in mm, each greater than 0",
            "Pass the plate size, for example bed_x 220 and bed_y 220, and bed_z for the build height.",
        )
    plate = [float(v) for v in bed]
    raw_origin = options.get("origin", [0, 0])
    if (
        not isinstance(raw_origin, (list, tuple))
        or len(raw_origin) != 2
        or any(isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) for v in raw_origin)
    ):
        return fail(
            INVALID_INPUT,
            "origin must be [x, y] in mm, finite numbers",
            "Pass the plate's corner, for example bed_origin_x 230 for a second plate beside the first.",
        )
    origin = [float(v) for v in raw_origin]

    def task() -> dict[str, Any]:
        return _check_printability_gui(doc_name, object_names, plate, origin, timeout)

    return run_on_gui(task, timeout, "check_printability", tool=_TOOL_NAME)


def _check_printability_gui(
    doc_name: str,
    object_names: list[str] | None,
    plate: list[float],
    origin: list[float],
    timeout: float,
) -> dict[str, Any]:
    start = time.monotonic()
    quarter = timeout * _BUDGET_GUARD_FRACTION

    def budget_ok() -> bool:
        return (timeout - (time.monotonic() - start)) >= quarter

    doc, error = require_document(doc_name)
    if error is not None:
        return error
    objects, error = _resolve_objects(doc, object_names)
    if error is not None:
        return error

    parts = [_part(obj, plate, origin) for obj in objects]
    overlaps, not_checked = _overlaps(parts, budget_ok)
    for part in parts:
        for overlap in overlaps:
            if part["name"] in (overlap["a"], overlap["b"]):
                other = overlap["b"] if part["name"] == overlap["a"] else overlap["a"]
                part["issues"].append(f"overlaps {other} by {overlap['volume_mm3']:.2f} mm^3")

    results = [{k: v for k, v in part.items() if not k.startswith("_")} for part in parts]
    # An empty result is never printable: nothing was checked.
    printable = bool(results) and all(not item["issues"] for item in results) and not not_checked
    return {
        "success": True,
        "document": doc.Name,
        "printable": printable,
        "settings": {"bed": plate, "origin": origin},
        "objects": results,
        "overlaps": overlaps,
        "not_checked": not_checked,
    }


def _resolve_objects(doc: Any, object_names: list[str] | None) -> tuple[list[Any], dict[str, Any] | None]:
    """The parts to check, or a failure reply.

    Given names must all resolve; without them, the visible top-level objects
    (as the FreeCAD tree shows them) that have a solid.
    """
    if object_names:
        objects = []
        for name in object_names:
            obj, error = require_object(doc, name)
            if error is not None:
                return [], error
            objects.append(obj)
        return objects, None

    objects = []
    for obj in tessellation.tree_root_objects(doc):
        if not visibility_of(obj, True):
            continue
        shape = tessellation.shape_of(obj)
        try:
            has_solid = shape is not None and len(shape.Solids) > 0
        except Exception:
            has_solid = False
        if has_solid:
            objects.append(obj)
    return objects, None


def _part(obj: Any, plate: list[float], origin: list[float]) -> dict[str, Any]:
    """One part's size, position, margins and whether it lies in the plate."""
    part: dict[str, Any] = {
        "name": obj.Name,
        "label": obj.Label,
        "size": None,
        "bound_box": None,
        "margin_mm": None,
        "free_margin_mm": None,
        "inside": False,
        "issues": [],
        "_shape": None,
        "_box": None,
    }
    shape = tessellation.shape_of(obj)
    try:
        has_solid = shape is not None and len(shape.Solids) > 0
    except Exception:
        has_solid = False
    if not has_solid:
        part["issues"].append("has no solid shape to place on the plate")
        return part
    bb = tight_bound_box(shape)
    box = (bb.XMin, bb.YMin, bb.ZMin, bb.XMax, bb.YMax, bb.ZMax)
    part["_shape"], part["_box"] = shape, box
    part["size"] = [finite_or_none(round(box[i + 3] - box[i], 4)) for i in range(3)]
    part["bound_box"] = bound_box_list(bb)

    margins, free, problems = plate_margins(box, plate, origin)
    part["margin_mm"] = margins
    part["free_margin_mm"] = free
    part["inside"] = not problems
    part["issues"].extend(problems)
    return part


def _boxes_overlap(a: tuple, b: tuple) -> bool:
    return all(
        a[i] < b[i + 3] - _TOUCH_TOLERANCE_MM and b[i] < a[i + 3] - _TOUCH_TOLERANCE_MM for i in range(3)
    )


def _overlaps(parts: list[dict[str, Any]], budget_ok: Any) -> tuple[list[dict], list[dict]]:
    """Pairs of parts that share volume, and the pairs left unchecked."""
    overlaps: list[dict] = []
    not_checked: list[dict] = []
    for i, first in enumerate(parts):
        for second in parts[i + 1:]:
            if first["_shape"] is None or second["_shape"] is None:
                continue
            if not _boxes_overlap(first["_box"], second["_box"]):
                continue
            if not budget_ok():
                not_checked.append({"a": first["name"], "b": second["name"],
                                    "reason": "the time budget ran low; call again with a larger timeout"})
                continue
            try:
                volume = float(first["_shape"].common(second["_shape"]).Volume)
            except Exception as e:
                not_checked.append({"a": first["name"], "b": second["name"],
                                    "reason": f"the intersection failed: {type(e).__name__}: {e}"})
                continue
            if volume > _OVERLAP_VOLUME_MM3:
                overlaps.append({"a": first["name"], "b": second["name"], "volume_mm3": round(volume, 4)})
    return overlaps, not_checked

"""The set_view handler: leave a chosen view on the user's screen.

Changes what the user sees, not the model: no transaction, and nothing to undo
in the document. Runs on the GUI thread (60 s budget). See view_mode.py for the
camera engine.
"""

from typing import Any

import FreeCAD
import FreeCADGui

from rpc_server import view_mode
from rpc_server.errors import INVALID_INPUT, NOT_FOUND, UNAVAILABLE, fail, tool_call
from rpc_server.gui_task import run_on_gui
from rpc_server.lookup import require_document
from rpc_server.view_manager import _VIEW_DISPATCH, _document_capture_view, apply_view_orientation

MODES = ("static", "orbit", "tour")

# Fixed budget: set_view takes no timeout argument.
_TIMEOUT = 60.0


def _names(value: Any, what: str) -> tuple[list[str] | None, dict | None]:
    """A list of object names from ``value`` (None, one name or a list)."""
    if value is None:
        return [], None
    if isinstance(value, str):
        return [value], None
    if isinstance(value, (list, tuple)) and all(isinstance(v, str) for v in value):
        return list(value), None
    return None, fail(INVALID_INPUT, f"{what} must be an object name or a list of object names, not {value!r}")


def _resolve_view(doc_name: str | None) -> tuple[Any, Any, dict | None]:
    """The document and its 3D view, or a fail() reply."""
    if not doc_name:
        doc = FreeCAD.ActiveDocument
        if doc is None:
            return None, None, fail(
                UNAVAILABLE,
                "No document is open in FreeCAD, so there is no 3D view to change.",
                "Call " + tool_call("create_document", {"name": "MyDocument"}) + " or open_document first.",
            )
        doc_name = doc.Name
    doc, error = require_document(doc_name)
    if error is not None:
        return None, None, error
    view = _document_capture_view(FreeCADGui.getDocument(doc_name))
    if view is None:
        return None, None, fail(
            UNAVAILABLE,
            f"Document '{doc_name}' has no 3D view (it may be open hidden, or all its 3D views are closed).",
            "Call " + tool_call("activate_document", {"doc_name": doc_name, "create_view": True})
            + " to create one, then call set_view again.",
        )
    return doc, view, None


def _orientation_of(view: Any, name: str) -> tuple:
    """The camera orientation of a standard view, leaving the camera as it was.
    The caller has turned the view's navigation animation off, so the
    orientation is in place when it is read and no glide moves the camera later."""
    saved = view.getCamera()
    apply_view_orientation(view, name)
    quat = view_mode.read_pose(view).quat
    view.setCamera(saved)
    return quat


def _effective_visible(obj: Any, show: list[str], hide: list[str], isolate: list[str]) -> bool:
    """Whether ``obj`` will be visible after the visibility changes."""
    if obj.Name in show or obj.Name in isolate:
        return True
    if obj.Name in hide or isolate:
        return False
    return bool(obj.ViewObject.Visibility)


def _tour_stops(doc: Any, stops: Any) -> tuple[list[dict] | None, dict | None]:
    """The stops of a tour, checked: ``{"focus": [names] or ["all"], "dwell",
    "view_name"?}``; the caller turns a view name into an orientation. Without
    stops the tour visits each visible object in turn."""
    if not stops:
        return [
            {"focus": [o.Name], "dwell": view_mode.DEFAULT_DWELL_SECONDS}
            for o in view_mode.drawn_objects(doc)
            if not getattr(o, "Group", None)  # a group's members get their own stops
        ], None
    out = []
    for i, stop in enumerate(stops):
        if not isinstance(stop, dict):
            return None, fail(INVALID_INPUT, f"stops[{i}] must be an object, not {stop!r}")
        names, error = _names(stop.get("focus", "all"), f"stops[{i}].focus")
        if error is not None:
            return None, error
        if names != ["all"]:
            for name in names:
                if doc.getObject(name) is None:
                    return None, fail(
                        NOT_FOUND,
                        f"stops[{i}] names object '{name}', which is not in document '{doc.Name}'.",
                        "Call " + tool_call("list_objects", {"doc_name": doc.Name, "compact": True})
                        + " to see the object names.",
                    )
        dwell = stop.get("dwell_seconds", view_mode.DEFAULT_DWELL_SECONDS)
        if isinstance(dwell, bool) or not isinstance(dwell, (int, float)) or dwell < 0:
            return None, fail(INVALID_INPUT, f"stops[{i}].dwell_seconds must be 0 or more, not {dwell!r}")
        entry: dict[str, Any] = {"focus": names, "dwell": float(dwell)}
        stop_view = stop.get("view_name")
        if stop_view:
            if stop_view not in _VIEW_DISPATCH:
                return None, fail(INVALID_INPUT, f"stops[{i}].view_name {stop_view!r} is not a view name")
            entry["view_name"] = stop_view
        out.append(entry)
    return out, None


def _set_view_gui(doc_name: str | None, options: dict[str, Any]) -> dict[str, Any]:
    view_mode.import_coin()  # pivy.coin before any camera node is touched
    doc, view, error = _resolve_view(doc_name)
    if error is not None:
        return error

    mode = options.get("mode") or "static"
    if mode not in MODES:
        return fail(INVALID_INPUT, f"invalid mode: {mode!r}; mode must be one of {', '.join(MODES)}")
    view_name = options.get("view_name")
    if view_name and view_name not in _VIEW_DISPATCH:
        return fail(INVALID_INPUT, f"invalid view_name: {view_name!r}; one of {', '.join(_VIEW_DISPATCH)}")
    focus, error = _names(options.get("focus"), "focus")
    if error is not None:
        return error
    show, error = _names(options.get("show"), "show")
    if error is not None:
        return error
    hide, error = _names(options.get("hide"), "hide")
    if error is not None:
        return error
    isolate, error = _names(options.get("isolate"), "isolate")
    if error is not None:
        return error
    transparency = options.get("transparency") or {}
    display_mode = options.get("display_mode") or {}
    if not isinstance(transparency, dict) or not isinstance(display_mode, dict):
        return fail(INVALID_INPUT, "transparency and display_mode must map object names to values")

    speed = options.get("degrees_per_second")
    if speed is None:
        speed = view_mode.DEFAULT_ORBIT_SPEED
    move = options.get("move_seconds")
    if move is None:
        move = view_mode.DEFAULT_MOVE_SECONDS
    for key, value in (("degrees_per_second", speed), ("move_seconds", move)):
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            return fail(INVALID_INPUT, f"{key} must be a number, not {value!r}")
    if speed == 0 or move <= 0:
        return fail(INVALID_INPUT, "degrees_per_second must not be 0 and move_seconds must be more than 0")

    focus_objects = []
    for name in focus:
        obj = doc.getObject(name)
        if obj is None or getattr(obj, "ViewObject", None) is None:
            return fail(
                NOT_FOUND,
                f"Object '{name}' has no view in document '{doc.Name}'.",
                "Call " + tool_call("list_objects", {"doc_name": doc.Name, "compact": True})
                + " to see the object names.",
            )
        focus_objects.append(obj)

    # Everything is checked before the first change on screen.
    by_name, error = view_mode.check_visual_changes(doc, show, hide, isolate, transparency, display_mode)
    if error is not None:
        return error
    if focus_objects and not any(
        _effective_visible(o, show, hide, isolate) and view_mode._union_box([o]) is not None
        for o in focus_objects
    ):
        return fail(
            INVALID_INPUT,
            "None of the focus objects has a shape on screen to frame.",
            "Make them visible with show, or name other objects.",
        )
    stops = None
    if mode == "tour":
        stops, error = _tour_stops(doc, options.get("stops"))
        if error is not None:
            return error
        if not stops:
            return fail(
                INVALID_INPUT,
                "The tour has no stops: no object is visible and no stops were given.",
                "Pass stops, or make an object visible, then call set_view again.",
            )

    reply: dict[str, Any] = {"success": True, "document": doc.Name}
    gui_doc = FreeCADGui.getDocument(doc.Name)
    was_modified = bool(gui_doc.Modified)
    view_mode.clear_stop(doc.Name)
    animated = view.isAnimationEnabled()
    # The navigation animation is off from the first orientation change to the
    # last camera write: a glide that started earlier would move the camera
    # after this call and read as someone else's change.
    view.setAnimationEnabled(False)
    try:
        if options.get("reset"):
            reply["reset"] = view_mode.reset(doc)
        else:
            view_mode.stop_mode(doc.Name, "replaced")

        changes = view_mode.apply_visual_changes(doc, by_name, show, hide, isolate, transparency, display_mode)
        for key in ("shown", "hidden", "transparency", "display_mode"):
            if changes[key]:
                reply[key] = changes[key]

        # Bring the document's tab to the front, and leave it there: the point
        # is to show the user.
        try:
            FreeCAD.setActiveDocument(doc.Name)
            FreeCADGui.getMainWindow().setActiveWindow(view)
        except Exception:
            pass

        if view_name:
            apply_view_orientation(view, view_name)
        for stop in stops or []:
            if stop.get("view_name"):
                stop["quat"] = _orientation_of(view, stop["view_name"])
        if focus_objects:
            pose = view_mode.fit_pose(view, focus_objects, sphere=mode == "orbit")
            if pose is None:
                return fail(
                    INVALID_INPUT,
                    "None of the focus objects has a shape on screen to frame.",
                    "Make them visible with show, or name other objects.",
                )
            view_mode.apply_pose(view, pose)
        else:
            # Everything drawn once this call's show, hide and isolate are
            # applied (hidden groups included), fitted as tightly as an
            # explicit focus is; fitAll would also count what is hidden. An
            # orbit frames the sphere around them, so no angle cuts them off.
            pose = view_mode.fit_pose(view, view_mode.drawn_objects(doc), sphere=mode == "orbit")
            if pose is None:
                view.fitAll()
            else:
                view_mode.apply_pose(view, pose)
        if mode == "orbit":
            view_mode.start_orbit(view, doc.Name, float(speed))
        elif mode == "tour":
            view_mode.start_tour(view, doc.Name, stops, float(move), bool(options.get("loop")))
    finally:
        view.setAnimationEnabled(animated)
        # FreeCAD marks a visibility, transparency or display mode change as an
        # unsaved change of the document. These are view settings, not the
        # model, so the mark is put back as it was: closing the document is not
        # blocked.
        gui_doc.Modified = was_modified

    reply.update(view_mode.status(doc.Name))
    reply["mode"] = mode
    pose = view_mode.read_pose(view)
    reply["camera"] = {
        "type": "Orthographic" if pose.ortho else "Perspective",
        "view_direction": [round(c, 4) + 0.0 for c in view_mode._direction(pose.quat)],
    }
    return reply


def set_view(doc_name: str | None = None, options: dict[str, Any] | None = None) -> dict[str, Any]:
    """Change what the user sees in ``doc_name``'s 3D view (default: the active
    document): orientation, framing, visibility, transparency, display modes,
    and a mode ("static", "orbit" or "tour").

    ``options`` keys: ``view_name``, ``focus`` (name or list), ``show``,
    ``hide``, ``isolate`` (lists), ``transparency`` and ``display_mode`` (maps
    of object name to value), ``mode``, ``degrees_per_second`` (orbit),
    ``stops`` (tour: ``{"focus", "dwell_seconds", "view_name"?}``),
    ``move_seconds`` and ``loop`` (tour), ``reset``. Reply: ``{"success",
    "document", "mode", "running", "stopped"?, "camera", "shown"?, "hidden"?,
    "transparency"?, "display_mode"?, "reset"?}``. GUI thread, 60 s. No
    transaction.
    """
    opts = dict(options or {})
    return run_on_gui(lambda: _set_view_gui(doc_name, opts), _TIMEOUT, "set_view")

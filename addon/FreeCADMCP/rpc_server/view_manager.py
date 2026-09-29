"""Active-view orientation, sizing, and screenshot capture."""

import base64
import os
import tempfile
from typing import Any
from xmlrpc.client import Fault

import FreeCAD
import FreeCADGui

from rpc_server import view_mode
from rpc_server.agent_log import agent_warning
from rpc_server.errors import UNAVAILABLE, fail, tool_call
from rpc_server.gui_dispatch import _flush_gui_events, dispatch_to_gui
from rpc_server.lookup import require_document


_VIEW_DISPATCH = {
    "Isometric": "viewIsometric",
    "Front": "viewFront",
    "Top": "viewTop",
    "Right": "viewRight",
    "Back": "viewBack",
    "Left": "viewLeft",
    "Bottom": "viewBottom",
    "Dimetric": "viewDimetric",
    "Trimetric": "viewTrimetric",
}


def _get_view_size(view: Any) -> tuple[int, int]:
    try:
        size = view.getSize()
        if isinstance(size, (list, tuple)) and len(size) >= 2:
            return max(1, int(size[0])), max(1, int(size[1]))
        return max(1, int(size.width())), max(1, int(size.height()))
    except Exception:
        return 1024, 768


# Longest edge used when the caller does not ask for a specific size. The
# screenshot's cost to an LLM client scales with its pixel count, and hosts
# commonly downscale anything larger than ~1.5k px before the model ever sees
# it, so rendering at the full window size just inflates the payload. An
# explicit width/height is honoured within MIN/MAX_SCREENSHOT_EDGE.
MAX_AUTO_SCREENSHOT_EDGE = 1024

# Accepted range for each screenshot dimension; the Go get_view schema uses
# the same bounds. Larger images rarely fit the 1 MiB an MCP reply carries.
MIN_SCREENSHOT_EDGE = 1
MAX_SCREENSHOT_EDGE = 2048


def _clamp_edge(value: int) -> int:
    return min(MAX_SCREENSHOT_EDGE, max(MIN_SCREENSHOT_EDGE, int(value)))


def _scale_to_max_edge(width: int, height: int, max_edge: int) -> tuple[int, int]:
    longest = max(width, height)
    if longest <= max_edge:
        return width, height
    scale = max_edge / longest
    return max(1, int(width * scale)), max(1, int(height * scale))


def _resolve_screenshot_size(
    view: Any,
    width: int | None,
    height: int | None,
) -> tuple[int, int]:
    view_width, view_height = _get_view_size(view)
    if width is None and height is None:
        return _scale_to_max_edge(view_width, view_height, MAX_AUTO_SCREENSHOT_EDGE)
    resolved_width = view_width if width is None else width
    resolved_height = view_height if height is None else height
    return _clamp_edge(resolved_width), _clamp_edge(resolved_height)


# The view_name that captures the view as the user sees it, changing nothing
# (set_view's screenshot).
CURRENT_VIEW = "Current"

_STD_COMMAND_DISPATCH = {
    "Isometric": "Std_ViewIsometric",
    "Front": "Std_ViewFront",
    "Top": "Std_ViewTop",
    "Right": "Std_ViewRight",
    "Back": "Std_ViewRear",
    "Left": "Std_ViewLeft",
    "Bottom": "Std_ViewBottom",
    "Dimetric": "Std_ViewDimetric",
    "Trimetric": "Std_ViewTrimetric",
}


def apply_view_orientation(view: Any, view_name: str) -> None:
    method_name = _VIEW_DISPATCH.get(view_name)
    if method_name is None:
        raise ValueError(f"Invalid view name: {view_name}")
    if hasattr(view, method_name):
        getattr(view, method_name)()
    else:
        # Fallback for views that lack the direct Python method
        # (e.g. some FreeCAD versions / view types)
        cmd = _STD_COMMAND_DISPATCH.get(view_name)
        if cmd:
            FreeCADGui.runCommand(cmd)
        else:
            agent_warning(
                f"apply_view_orientation: no method or command for '{view_name}'\n"
            )


def save_active_screenshot(
    view: Any,
    doc: Any,
    save_path: str,
    view_name: str = "Isometric",
    width: int | None = None,
    height: int | None = None,
    focus_object: str | None = None,
):
    """Save a PNG of ``view`` (a 3D view belonging to ``doc``) to ``save_path``.

    Returns ``True`` on success, or an error string on failure, as the other
    GUI-thread handlers do. ``view`` must already be the active window (the
    caller activates it and restores the previous one); this function only
    reorients the camera and, when ``focus_object`` is given, the selection.
    Saving and restoring the camera and selection the caller found is the
    caller's job (``get_active_screenshot`` does it), since only it knows the
    full extent of what changed across a whole capture.
    """
    try:
        if not hasattr(view, "saveImage"):
            return "Current view does not support screenshots"

        if view_name == CURRENT_VIEW:
            # As the user sees it: no orientation, no framing, no camera change.
            resolved_width, resolved_height = _resolve_screenshot_size(view, width, height)
            view.saveImage(save_path, resolved_width, resolved_height, "Current")
            return True

        apply_view_orientation(view, view_name)

        focused_selection = False
        # The resolved object we frame on (when focus_object is given), kept so
        # the framing can be re-applied synchronously right before saveImage().
        focus_target = None

        if focus_object:
            obj = doc.getObject(focus_object) if doc else None
            if obj:
                FreeCADGui.Selection.clearSelection()
                FreeCADGui.Selection.addSelection(obj)
                FreeCADGui.SendMsgToActiveView("ViewSelection")
                focused_selection = True
                focus_target = obj
                _flush_gui_events()
                FreeCADGui.Selection.clearSelection()
            else:
                view.fitAll()
        else:
            view.fitAll()

        _flush_gui_events()
        # On macOS, when the FreeCAD window is not exposed (fully occluded or
        # minimized), saveImage() right after pumping the event loop grabs a blank
        # frame. Re-issuing the framing synchronously forces a redraw first. The
        # flush above is kept intentionally - Linux needs it for the stale-frame
        # fix (#51/#53).
        if focused_selection and focus_target is not None:
            FreeCADGui.Selection.addSelection(focus_target)
            FreeCADGui.SendMsgToActiveView("ViewSelection")
            FreeCADGui.Selection.clearSelection()
        else:
            view.fitAll()
        resolved_width, resolved_height = _resolve_screenshot_size(view, width, height)
        # View3DInventorPy.saveImage(filename, width, height, background,
        # comment, samples): the fifth argument is the comment embedded in the
        # image, so it is left at FreeCAD's default. The capture method comes
        # from the "SavePicture" parameter (the "Creation method" option of
        # FreeCAD's Save Image dialog), not from an argument.
        view.saveImage(save_path, resolved_width, resolved_height, "Current")

        if focused_selection:
            FreeCADGui.Selection.clearSelection()
            _flush_gui_events(delay_ms=0)
        return True
    except Exception as e:
        return str(e)


def _nothing_to_capture(code: str, reason: str, error: str, hint: str) -> dict[str, Any]:
    """Reply when there is no 3D view to capture: adds ``reason`` to a fail() dict."""
    reply = fail(code, error, hint)
    reply["reason"] = reason
    return reply


def _no_document_reply() -> dict[str, Any]:
    return _nothing_to_capture(
        UNAVAILABLE,
        "no_document",
        "No document is open in FreeCAD, so there is no 3D view to capture.",
        "Call " + tool_call("create_document", {"name": "MyDocument"}) + " or "
        + tool_call("open_document", {"path": "C:/path/to/file.FCStd"})
        + ", then call get_view again.",
    )


# The MDI view class name FreeCADGui registers for a 3D view, as
# Document.mdiViewsOfType expects it (Gui/DocumentPyImp.cpp mdiViewsOfType,
# used the same way in Mod/Show/SceneDetails/Camera.py and ClipPlane.py).
_VIEW3D_TYPE = "Gui::View3DInventor"


def _document_capture_view(gui_doc: Any) -> Any:
    """Return a 3D view of ``gui_doc``, or ``None`` when it has none.

    A document can have several 3D views (or none, when opened hidden or with
    every 3D view closed); ``Document.ActiveView`` can also be a non-3D view
    (the last-used view of any type, Gui/Document.cpp getActiveView), so it is
    only preferred when it is itself one of the document's 3D views.
    """
    try:
        views = gui_doc.mdiViewsOfType(_VIEW3D_TYPE)
    except Exception:
        views = []
    if not views:
        return None
    try:
        active = gui_doc.ActiveView
    except Exception:
        active = None
    for candidate in views:
        if candidate == active:
            return candidate
    return views[-1]


def _save_selection() -> list[tuple[str, str, tuple[str, ...]]]:
    """Snapshot every document's selection so it can be rebuilt afterwards."""
    try:
        return [
            (sel.DocumentName, sel.ObjectName, tuple(sel.SubElementNames))
            for sel in FreeCADGui.Selection.getSelectionEx("*", 0)
        ]
    except Exception:
        return []


def _restore_selection(saved: list[tuple[str, str, tuple[str, ...]]]) -> None:
    """Clear the current selection and rebuild ``saved`` (from ``_save_selection``).

    Each addition is its own try/except: one entry that fails to re-select
    (an object renamed or removed during the capture, say) must not stop the
    rest of the saved selection from being restored.
    """
    try:
        FreeCADGui.Selection.clearSelection()
    except Exception:
        pass
    for doc_name, obj_name, subs in saved:
        if subs:
            for sub in subs:
                try:
                    FreeCADGui.Selection.addSelection(doc_name, obj_name, sub)
                except Exception:
                    pass
        else:
            try:
                FreeCADGui.Selection.addSelection(doc_name, obj_name)
            except Exception:
                pass


def _screenshot_reply(data: bytes, app_doc: Any) -> Any:
    """The GUI task's result for captured PNG ``data``: the reply tuple, or the
    error string get_active_screenshot turns into a Fault."""
    if not data:
        return "could not save the screenshot: FreeCAD wrote an empty image"
    return ({
        "success": True,
        "image": base64.b64encode(data).decode("ascii"),
        "document": app_doc.Name,
    },)


def _capture_current(view: Any, app_doc: Any, width: int | None, height: int | None) -> Any:
    """Capture ``view`` exactly as it is on screen (view_name Current)."""
    fd, tmp_path = tempfile.mkstemp(suffix=".png")
    os.close(fd)
    try:
        saved = save_active_screenshot(view, app_doc, tmp_path, CURRENT_VIEW, width, height)
        if saved is not True:
            return f"could not save the screenshot: {saved}"
        with open(tmp_path, "rb") as f:
            data = f.read()
    finally:
        try:
            os.remove(tmp_path)
        except OSError:
            pass
    return _screenshot_reply(data, app_doc)


def get_active_screenshot(
    view_name: str = "Isometric",
    width: int | None = None,
    height: int | None = None,
    focus_object: str | None = None,
    doc_name: str | None = None,
) -> dict[str, Any]:
    """Capture a 3D view as a base64-encoded PNG.

    Runs on the RPC thread and dispatches the capture to the GUI thread.
    ``doc_name`` given captures that document's 3D view, activating it (and
    restoring the previously active MDI window afterwards) when it is not
    already the active one; not given, the active document's active view.
    Either way, the view's camera and FreeCAD's selection are restored to
    what they were before the call; with ``view_name`` "Current" they are not
    touched at all, and a running orbit or tour is captured as it runs.
    Returns ``{"success": True, "image", "document"}``, or a failure with a
    ``reason`` when there is nothing to capture (no_document,
    document_not_found, no_3d_view, not_3d_view). Any other failure, including
    a GUI dispatch timeout, raises a Fault that says what went wrong.
    """

    def task():
        mw = FreeCADGui.getMainWindow()
        try:
            prev_window = mw.getActiveWindow()
        except Exception:
            prev_window = None

        if doc_name:
            _doc, doc_error = require_document(doc_name)
            if doc_error is not None:
                doc_error["reason"] = "document_not_found"
                return (doc_error,)
            gui_doc = FreeCADGui.getDocument(doc_name)
            app_doc = FreeCAD.getDocument(doc_name)
            view = _document_capture_view(gui_doc)
            if view is None:
                return (_nothing_to_capture(
                    UNAVAILABLE,
                    "no_3d_view",
                    f"Document '{doc_name}' has no 3D view (it may be open hidden, "
                    "or all its 3D views are closed).",
                    "Call " + tool_call(
                        "activate_document", {"doc_name": doc_name, "create_view": True}
                    ) + " to create one, then call get_view again.",
                ),)
        else:
            try:
                gui_doc = FreeCADGui.ActiveDocument
            except Exception:
                gui_doc = None
            if gui_doc is None:
                return (_no_document_reply(),)
            app_doc = gui_doc.Document
            try:
                view = gui_doc.ActiveView
            except Exception:
                view = None
            if view is None or not hasattr(view, "saveImage"):
                view_type = type(view).__name__ if view is not None else "None"
                agent_warning(
                    f"MCP RPC: view type '{view_type}' does not support screenshots\n"
                )
                return (_nothing_to_capture(
                    UNAVAILABLE,
                    "not_3d_view",
                    "FreeCAD's active window is not a 3D view that can be captured "
                    "(for example a TechDraw page or a spreadsheet is active).",
                    "Call " + tool_call("get_view", {"doc_name": gui_doc.Document.Name})
                    + " to capture that document's 3D view.",
                ),)

        # Bring the target view to the front only when it is not already
        # there, and only ever switch back to what the user had.
        switched_window = False
        if prev_window is None or view != prev_window:
            try:
                mw.setActiveWindow(view)
                # Set as soon as the switch itself succeeds: a raise from the
                # flush below must not stop the finally block from switching
                # back to what the user had before this call.
                switched_window = True
                _flush_gui_events()
            except Exception:
                pass

        if view_name == CURRENT_VIEW:
            # As the user sees it: the camera, the selection, a running orbit
            # or tour and the navigation animation are all left alone, so none
            # of the pause and restore below applies. Only the window switch
            # above is undone.
            try:
                return _capture_current(view, app_doc, width, height)
            finally:
                if switched_window and prev_window is not None:
                    try:
                        mw.setActiveWindow(prev_window)
                    except Exception:
                        pass

        # A camera mode running on this view (set_view's orbit or tour) is
        # paused for the capture, which moves the camera and puts it back, and
        # resumed in the finally block below.
        mode_token = view_mode.pause(app_doc.Name)

        try:
            saved_camera = view.getCamera()
        except Exception:
            saved_camera = None
        saved_selection = _save_selection()

        # Disable navigation animation for the capture: with it on,
        # apply_view_orientation and fitAll start an asynchronous camera
        # animation (Gui/Navigation/NavigationStyle.cpp startAnimation,
        # FixedTimeAnimation) that can still be running when saveImage reads
        # the framebuffer (a mid-rotation capture) and that overwrites the
        # camera restore below once it finishes (NavigationAnimation.cpp
        # FixedTimeAnimation::onStop sets the exact target orientation).
        try:
            anim_was_enabled = view.isAnimationEnabled()
            view.setAnimationEnabled(False)
        except Exception:
            anim_was_enabled = None

        # The GUI thread owns the temporary file from creation to removal:
        # a caller that timed out never deletes it while saveImage writes.
        # Created inside the try so a failure here still runs the restores
        # in finally, instead of leaving the window, camera or selection
        # switched.
        data = b""
        tmp_path = None
        try:
            fd, tmp_path = tempfile.mkstemp(suffix=".png")
            os.close(fd)
            saved = save_active_screenshot(view, app_doc, tmp_path, view_name, width, height, focus_object)
            if saved is not True:
                return f"could not save the screenshot: {saved}"
            with open(tmp_path, "rb") as f:
                data = f.read()
        finally:
            if tmp_path is not None:
                try:
                    os.remove(tmp_path)
                except OSError:
                    pass
            _restore_selection(saved_selection)
            # Stop any animation still in flight (a fallback belt-and-braces
            # measure: disabling it above should already have prevented one)
            # before restoring the camera, so a late animation tick cannot
            # overwrite the restore.
            try:
                view.stopAnimating()
            except Exception:
                pass
            if saved_camera:
                try:
                    view.setCamera(saved_camera)
                except Exception:
                    pass
            if anim_was_enabled is not None:
                try:
                    view.setAnimationEnabled(anim_was_enabled)
                except Exception:
                    pass
            view_mode.resume(mode_token)
            if switched_window and prev_window is not None:
                try:
                    mw.setActiveWindow(prev_window)
                except Exception:
                    pass
        return _screenshot_reply(data, app_doc)

    res = dispatch_to_gui(task, operation_name="get_active_screenshot")
    if isinstance(res, tuple):
        return res[0]
    if isinstance(res, dict):
        code = res.get("code", "GUI_DISPATCH_FAILED")
        message = str(res.get("error", res))
    else:
        code = "SCREENSHOT_FAILED"
        message = str(res)
    agent_warning(f"MCP RPC: screenshot failed: {message}\n")
    raise Fault(1, f"{code}: {message}")

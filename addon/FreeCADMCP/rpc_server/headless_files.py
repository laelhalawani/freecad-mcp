"""Files saved without a GUI: show what the file stores.

A file written by freecadcmd has no GuiDocument.xml. FreeCAD's GUI then builds
each object's view provider without saved view data, the provider's own
Visibility starts hidden, and that overwrites the Visibility the file stores
on the object (Gui/Document.cpp slotNewObject, ViewProviderDocumentObject).
Every object opens hidden: the 3D view draws nothing, nothing can be framed
and get_view returns an empty image, until something sets Visibility.

``show_stored_visibility`` sets the view providers back to the Visibility the
file stores, once, right after the file is opened, and frames the objects from
the Isometric direction (the file stores no camera either), without leaving the
document modified. open_document and reload_document call it; every other open
(start_freecad's file argument, File > Open, a double click) is caught by the
document observer ``install`` registers, which does the same once the view is
laid out.
"""

import xml.etree.ElementTree as ET
import zipfile

import FreeCADGui

from rpc_server.agent_log import agent_warning

# An Origin's axes, planes and point keep the view provider's own default:
# FreeCAD shows them only when the user asks, whatever the file stores.
_ORIGIN_TYPES = frozenset({"App::Origin", "App::Line", "App::Plane", "App::Point"})


def stored_visibility(path: str) -> dict[str, bool] | None:
    """The Visibility each object of the .FCStd file stores, by object name,
    or None when the file has GUI data (GuiDocument.xml) or cannot be read as
    a document. An object that stores no Visibility is visible, as FreeCAD
    creates it."""
    try:
        with zipfile.ZipFile(path) as archive:
            if "GuiDocument.xml" in archive.namelist():
                return None
            root = ET.fromstring(archive.read("Document.xml"))
    except (OSError, KeyError, zipfile.BadZipFile, ET.ParseError):
        return None
    visibility: dict[str, bool] = {}
    for obj in root.iterfind("./ObjectData/Object"):
        name = obj.get("name")
        if not name:
            continue
        visibility[name] = True
        for prop in obj.iterfind("./Properties/Property"):
            if prop.get("name") == "Visibility":
                flag = prop.find("Bool")
                if flag is not None:
                    visibility[name] = flag.get("value") == "true"
    return visibility


def _frame_isometric(doc) -> None:
    """Point the 3D view of ``doc`` at its drawn objects from the Isometric
    direction, as set_view static does. A file without GUI data stores no
    camera, so FreeCAD leaves its default one (a small orthographic view at the
    origin), which shows a corner of the model or nothing."""
    from rpc_server import view_mode
    from rpc_server.view_manager import _document_capture_view, apply_view_orientation

    view = _document_capture_view(FreeCADGui.getDocument(doc.Name))
    if view is None:
        return
    view_mode.import_coin()
    # No glide: it would move the camera after this call.
    animated = view.isAnimationEnabled()
    view.setAnimationEnabled(False)
    try:
        apply_view_orientation(view, "Isometric")
        pose = view_mode.fit_pose(view, view_mode.drawn_objects(doc))
        if pose is not None:
            view_mode.apply_pose(view, pose)
    finally:
        view.setAnimationEnabled(animated)


# The documents (by name) whose opening has been dealt with: restored, or found
# not to need it. open_document and reload_document restore right after their
# own open, and the observer below restores every other open; whichever comes
# first records the name here, so a document is never restored twice. A document
# opened hidden has no GUI document yet and is not recorded, so it is restored
# when it gets one.
_handled: set[str] = set()

# The observer waits for the 3D view to be laid out before it frames: at most
# this many checks, LAYOUT_CHECK_MS apart, then it frames anyway.
LAYOUT_CHECK_MS = 100
LAYOUT_CHECKS = 50


def _later(ms: int, callback) -> None:
    """Run ``callback`` on the GUI thread after ``ms`` (a Qt single shot)."""
    from PySide import QtCore

    QtCore.QTimer.singleShot(ms, callback)


def _gui_document(doc):
    """The GUI document of ``doc``, or None when it was opened hidden."""
    try:
        return FreeCADGui.getDocument(doc.Name)
    except Exception:
        return None


def _camera_pose(view) -> tuple:
    """Where the camera of ``view`` stands: its orientation and position. The
    rest of the camera (near and far distances, aspect ratio) FreeCAD changes by
    itself on the first redraw and on a resize, so it says nothing about
    whether someone moved the view."""
    from rpc_server import view_mode

    node = view_mode.camera_node(view)
    return tuple(node.orientation.getValue().getValue()) + tuple(node.position.getValue().getValue())


def _same_pose(a: tuple, b: tuple) -> bool:
    return len(a) == len(b) and all(abs(x - y) <= 1e-6 * max(1.0, abs(x), abs(y)) for x, y in zip(a, b))


def _camera_of(name: str):
    """The camera pose of the document called ``name`` now, or None."""
    try:
        import FreeCAD
        from rpc_server.view_manager import _document_capture_view

        view = _document_capture_view(_gui_document(FreeCAD.getDocument(name)))
        return _camera_pose(view) if view is not None else None
    except Exception:
        return None


def _restore(doc, path: str, wait_for_layout: bool, camera=None) -> None:
    """Restore ``doc``, opened from ``path``, if it has no GUI data: the stored
    visibility now, and the framing now, or once its 3D view is laid out. With
    ``camera`` (the pose when the document opened), a camera moved since is
    left alone. Never raises: a genuine failure is reported as an agent message
    (no popup) and the document stays as FreeCAD opened it."""
    try:
        gui_doc = _gui_document(doc)
        if gui_doc is None:
            return
        _handled.add(doc.Name)
        stored = stored_visibility(path)
        if stored is None:
            return
        was_modified = bool(gui_doc.Modified)
        try:
            for obj in doc.Objects:
                view = getattr(obj, "ViewObject", None)
                if view is None or obj.TypeId in _ORIGIN_TYPES or obj.Name not in stored:
                    continue
                if bool(view.Visibility) != stored[obj.Name]:
                    view.Visibility = stored[obj.Name]
            if not wait_for_layout:
                _frame_isometric(doc)
        finally:
            gui_doc.Modified = was_modified
        if wait_for_layout:
            if camera is None:
                camera = _camera_of(doc.Name)
            _later(LAYOUT_CHECK_MS, lambda: _frame_when_laid_out(doc.Name, camera, None, 0))
    except Exception as exc:
        agent_warning(f"MCP RPC: could not show the objects of '{path}': {exc}\n")


def _frame_when_laid_out(name: str, camera, last_size, checks: int) -> None:
    """Frame the document called ``name`` once its 3D view has the size it will
    keep (the same on two checks in a row, in a shown window), or after
    LAYOUT_CHECKS checks. ``camera`` is the camera pose (orientation and
    position) when the document opened: if it has moved since (the person moved
    the view, or an agent set one or started an orbit or a tour), the camera is
    theirs and is left alone."""
    try:
        import FreeCAD
        from rpc_server import view_mode
        from rpc_server.view_manager import _document_capture_view

        doc = FreeCAD.getDocument(name)
        gui_doc = _gui_document(doc)
        view = _document_capture_view(gui_doc) if gui_doc is not None else None
        if view is None:
            return
        if view_mode.status(name).get("running") or (camera is not None and not _same_pose(_camera_pose(view), camera)):
            return
        size = tuple(view.getSize())
        try:
            shown = bool(FreeCADGui.getMainWindow().isVisible())
        except Exception:
            shown = True
        if not (shown and size == last_size) and checks < LAYOUT_CHECKS:
            _later(LAYOUT_CHECK_MS, lambda: _frame_when_laid_out(name, camera, size, checks + 1))
            return
        was_modified = bool(gui_doc.Modified)
        try:
            _frame_isometric(doc)
        finally:
            gui_doc.Modified = was_modified
    except Exception as exc:
        agent_warning(f"MCP RPC: could not frame '{name}': {exc}\n")


def show_stored_visibility(doc, path: str) -> None:
    """Set the view providers of ``doc``, just opened from ``path``, to the
    Visibility the file stores when it has no GUI data (see the module text),
    and frame them in the view. open_document and reload_document call it right
    after their own open, when the view is laid out.
    A document opened hidden has no GUI document and no view providers: there
    is nothing to set, silently."""
    _restore(doc, path, wait_for_layout=False)


class _OpenObserver:
    """Restores every document that opens without GUI data, whatever opened it:
    start_freecad's file argument, File > Open, a double click, the recent
    files. FreeCAD activates a document once it has finished opening and its
    GUI document exists, and tells the document observers."""

    def slotActivateDocument(self, doc) -> None:  # noqa: N802 (FreeCAD name)
        try:
            name = doc.Name
            if name in _handled:
                return
            # The camera as it is now: a set_view that lands before the restore
            # runs is a camera someone chose, and is not overwritten. After the
            # call that opened it returns: open_document restores first and
            # records the name, and this then does nothing.
            camera = _camera_of(name)
            _later(0, lambda: self._restore_named(name, camera))
        except Exception:
            pass

    def slotDeletedDocument(self, doc) -> None:  # noqa: N802 (FreeCAD name)
        try:
            _handled.discard(doc.Name)
        except Exception:
            pass

    @staticmethod
    def _restore_named(name: str, camera=None) -> None:
        # A timer callback: nothing may raise into Qt's handler, whatever state
        # a closing document is in.
        try:
            import FreeCAD

            if name in _handled:
                return
            try:
                doc = FreeCAD.getDocument(name)
            except Exception:
                return
            _restore(doc, doc.FileName, wait_for_layout=True, camera=camera)
        except Exception as exc:
            agent_warning(f"MCP RPC: could not restore '{name}': {exc}\n")


_observer: "_OpenObserver | None" = None


def install() -> None:
    """Register the observer with FreeCAD (idempotent). Called when the addon's
    GUI part loads, so it also sees the file FreeCAD opens from its command
    line, before any RPC server runs."""
    global _observer
    if _observer is not None:
        return
    import FreeCAD

    _observer = _OpenObserver()
    FreeCAD.addDocumentObserver(_observer)

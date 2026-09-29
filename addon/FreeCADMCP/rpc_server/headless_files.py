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
document modified.
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


def show_stored_visibility(doc, path: str) -> None:
    """Set the view providers of ``doc``, just opened from ``path``, to the
    Visibility the file stores when it has no GUI data (see the module text),
    and frame them in the view.
    A document opened hidden has no GUI document and no view providers: there
    is nothing to set, silently. Never raises: a genuine failure is reported
    as an agent message (no popup) and the document stays as FreeCAD opened it."""
    try:
        stored = stored_visibility(path)
        if stored is None:
            return
        try:
            gui_doc = FreeCADGui.getDocument(doc.Name)
        except Exception:
            return
        if gui_doc is None:
            return
        was_modified = bool(gui_doc.Modified)
        try:
            for obj in doc.Objects:
                view = getattr(obj, "ViewObject", None)
                if view is None or obj.TypeId in _ORIGIN_TYPES or obj.Name not in stored:
                    continue
                if bool(view.Visibility) != stored[obj.Name]:
                    view.Visibility = stored[obj.Name]
            _frame_isometric(doc)
        finally:
            gui_doc.Modified = was_modified
    except Exception as exc:
        agent_warning(f"MCP RPC: could not show the objects of '{path}': {exc}\n")

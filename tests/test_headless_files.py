"""A file saved without GUI data opens showing what the file stores."""

import importlib.util
import sys
import types
import zipfile
from pathlib import Path

import pytest

import test_gui_dispatch  # noqa: F401  puts the addon directory on sys.path

HEADLESS_FILES = Path(__file__).resolve().parents[1] / "addon/FreeCADMCP/rpc_server/headless_files.py"

# The Document.xml layout FreeCAD writes (object data trimmed to the properties read).
DOCUMENT_XML = """<?xml version='1.0' encoding='utf-8'?>
<Document SchemaVersion="4">
  <Objects Count="4">
    <Object type="Part::Box" name="Box" id="1" />
    <Object type="Part::Feature" name="Hidden" id="2" />
    <Object type="Part::Cylinder" name="NoFlag" id="3" />
    <Object type="App::Origin" name="Origin" id="4" />
  </Objects>
  <ObjectData Count="4">
    <Object name="Box" Extensions="True">
      <Extensions Count="1"><Extension type="App::GroupExtension" name="GroupExtension"><Bool value="false"/></Extension></Extensions>
      <Properties Count="2">
        <Property name="Label" type="App::PropertyString"><String value="Box"/></Property>
        <Property name="Visibility" type="App::PropertyBool" status="648"><Bool value="true"/></Property>
      </Properties>
    </Object>
    <Object name="Hidden">
      <Properties Count="1">
        <Property name="Visibility" type="App::PropertyBool" status="648"><Bool value="false"/></Property>
      </Properties>
    </Object>
    <Object name="NoFlag">
      <Properties Count="1"><Property name="Label" type="App::PropertyString"><String value="NoFlag"/></Property></Properties>
    </Object>
    <Object name="Origin">
      <Properties Count="1">
        <Property name="Visibility" type="App::PropertyBool" status="648"><Bool value="true"/></Property>
      </Properties>
    </Object>
  </ObjectData>
</Document>
"""


@pytest.fixture
def headless_files(monkeypatch: pytest.MonkeyPatch, camera_calls: list):
    gui_doc = types.SimpleNamespace(Modified=False)
    printed: list[tuple[str, str]] = []
    freecad = types.ModuleType("FreeCAD")
    freecad.Console = types.SimpleNamespace(
        PrintWarning=lambda m: printed.append(("PrintWarning", m)),
        PrintDeveloperWarning=lambda m: printed.append(("PrintDeveloperWarning", m)),
    )
    freecad_gui = types.ModuleType("FreeCADGui")
    freecad_gui.getDocument = lambda _name: gui_doc
    freecad_gui.printed = printed
    monkeypatch.setitem(sys.modules, "FreeCAD", freecad)
    monkeypatch.setitem(sys.modules, "FreeCADGui", freecad_gui)
    spec = importlib.util.spec_from_file_location("_headless_files_test", HEADLESS_FILES)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module, gui_doc


def write_fcstd(path: Path, *, gui_data: bool) -> str:
    with zipfile.ZipFile(path, "w") as archive:
        archive.writestr("Document.xml", DOCUMENT_XML)
        if gui_data:
            archive.writestr("GuiDocument.xml", "<Document/>")
    return str(path)


def test_stored_visibility_is_read_from_a_file_without_gui_data(headless_files, tmp_path: Path) -> None:
    module, _ = headless_files
    stored = module.stored_visibility(write_fcstd(tmp_path / "h.FCStd", gui_data=False))
    assert stored == {"Box": True, "Hidden": False, "NoFlag": True, "Origin": True}


def test_a_file_with_gui_data_or_not_a_document_is_left_alone(headless_files, tmp_path: Path) -> None:
    module, _ = headless_files
    assert module.stored_visibility(write_fcstd(tmp_path / "gui.FCStd", gui_data=True)) is None
    (tmp_path / "junk.FCStd").write_text("not a zip")
    assert module.stored_visibility(str(tmp_path / "junk.FCStd")) is None
    assert module.stored_visibility(str(tmp_path / "missing.FCStd")) is None


class View:
    """A view provider: setting Visibility marks the GUI document modified, as FreeCAD does."""

    def __init__(self, gui_doc, visible=False):
        self._gui_doc, self._visible = gui_doc, visible

    @property
    def Visibility(self):
        return self._visible

    @Visibility.setter
    def Visibility(self, value):
        self._visible = value
        self._gui_doc.Modified = True


def objects(gui_doc):
    def obj(name, type_id):
        return types.SimpleNamespace(Name=name, TypeId=type_id, ViewObject=View(gui_doc))

    return [obj("Box", "Part::Box"), obj("Hidden", "Part::Feature"), obj("NoFlag", "Part::Cylinder"),
            obj("Origin", "App::Origin")]


def test_objects_open_showing_what_the_file_stores_and_the_document_stays_unmodified(headless_files, tmp_path: Path) -> None:
    module, gui_doc = headless_files
    doc = types.SimpleNamespace(Name="h", Objects=objects(gui_doc))
    module.show_stored_visibility(doc, write_fcstd(tmp_path / "h.FCStd", gui_data=False))
    visible = {o.Name: o.ViewObject.Visibility for o in doc.Objects}
    # Hidden stores false; an Origin keeps the provider's own default.
    assert visible == {"Box": True, "Hidden": False, "NoFlag": True, "Origin": False}
    assert gui_doc.Modified is False


def test_a_document_that_was_already_modified_stays_modified(headless_files, tmp_path: Path) -> None:
    module, gui_doc = headless_files
    gui_doc.Modified = True
    doc = types.SimpleNamespace(Name="h", Objects=objects(gui_doc))
    module.show_stored_visibility(doc, write_fcstd(tmp_path / "h.FCStd", gui_data=False))
    assert gui_doc.Modified is True


def test_a_document_opened_hidden_has_nothing_to_show_and_says_nothing(headless_files, tmp_path: Path) -> None:
    module, gui_doc = headless_files

    def no_gui_document(_name):
        raise RuntimeError("no GUI document")

    sys.modules["FreeCADGui"].getDocument = no_gui_document
    doc = types.SimpleNamespace(Name="h", Objects=objects(gui_doc))
    module.show_stored_visibility(doc, write_fcstd(tmp_path / "h.FCStd", gui_data=False))
    assert sys.modules["FreeCADGui"].printed == []

    sys.modules["FreeCADGui"].getDocument = lambda _name: None
    module.show_stored_visibility(doc, write_fcstd(tmp_path / "h.FCStd", gui_data=False))
    assert sys.modules["FreeCADGui"].printed == []


def test_a_genuine_failure_is_a_developer_warning_not_a_popup(headless_files, tmp_path: Path) -> None:
    module, gui_doc = headless_files

    class Broken(View):
        @property
        def Visibility(self):
            raise RuntimeError("view provider broke")

    obj = types.SimpleNamespace(Name="Box", TypeId="Part::Box", ViewObject=Broken(gui_doc))
    doc = types.SimpleNamespace(Name="h", Objects=[obj])
    module.show_stored_visibility(doc, write_fcstd(tmp_path / "h.FCStd", gui_data=False))
    printed = sys.modules["FreeCADGui"].printed
    assert [kind for kind, _ in printed] == ["PrintDeveloperWarning"]
    assert "view provider broke" in printed[0][1]


@pytest.fixture
def camera_calls(monkeypatch: pytest.MonkeyPatch) -> list:
    """The camera side of the module, recording what is asked of it."""
    class Calls(list):
        # The sizes the view reports, one per check; the last one repeats.
        sizes = [(1000, 700)]
        # The view's camera: FreeCAD's text (which carries the near and far
        # distances and the aspect ratio), its orientation and position, and
        # whether set_view runs a mode.
        camera = "camera 0"
        orientation = (0.0, 0.0, 0.0, 1.0)
        position = (0.0, 0.0, 10.0)
        running = False

    calls = Calls()
    view = types.SimpleNamespace(
        isAnimationEnabled=lambda: True,
        setAnimationEnabled=lambda on: calls.append(("animation", on)),
        getSize=lambda: calls.sizes.pop(0) if len(calls.sizes) > 1 else calls.sizes[0],
        getCamera=lambda: calls.camera,
    )
    view_mode = types.SimpleNamespace(
        import_coin=lambda: None,
        drawn_objects=lambda _doc: ["drawn"],
        fit_pose=lambda _view, objects: calls.append(("fit", objects)) or "pose",
        apply_pose=lambda _view, pose: calls.append(("apply", pose)),
        status=lambda _name: {"running": calls.running},
        camera_node=lambda _view: types.SimpleNamespace(
            orientation=types.SimpleNamespace(getValue=lambda: types.SimpleNamespace(getValue=lambda: calls.orientation)),
            position=types.SimpleNamespace(getValue=lambda: types.SimpleNamespace(getValue=lambda: calls.position)),
        ),
    )
    view_manager = types.SimpleNamespace(
        _document_capture_view=lambda _gui_doc: view,
        apply_view_orientation=lambda _view, name: calls.append(("orient", name)),
    )
    package = types.ModuleType("rpc_server")
    package.view_mode, package.view_manager = view_mode, view_manager
    for name, module in (("rpc_server", package), ("rpc_server.view_mode", view_mode), ("rpc_server.view_manager", view_manager)):
        monkeypatch.setitem(sys.modules, name, module)
    return calls


def test_a_file_without_gui_data_is_framed_from_the_isometric_direction(
    headless_files, camera_calls: list, tmp_path: Path
) -> None:
    module, gui_doc = headless_files
    doc = types.SimpleNamespace(Name="h", Objects=objects(gui_doc))
    module.show_stored_visibility(doc, write_fcstd(tmp_path / "h.FCStd", gui_data=False))
    assert camera_calls == [
        ("animation", False), ("orient", "Isometric"), ("fit", ["drawn"]), ("apply", "pose"), ("animation", True),
    ]
    assert gui_doc.Modified is False


def test_a_file_with_gui_data_keeps_its_saved_camera(headless_files, camera_calls: list, tmp_path: Path) -> None:
    module, gui_doc = headless_files
    doc = types.SimpleNamespace(Name="h", Objects=objects(gui_doc))
    module.show_stored_visibility(doc, write_fcstd(tmp_path / "gui.FCStd", gui_data=True))
    assert camera_calls == []


class Watched:
    """The observer installed against a stub FreeCAD, with the Qt timer replaced by a queue
    the test drains: what FreeCAD does when a document opens, without FreeCAD."""

    def __init__(self, module, gui_doc, tmp_path: Path) -> None:
        self.module, self.gui_doc, self.tmp_path = module, gui_doc, tmp_path
        self.queue: list = []
        self.docs: dict = {}
        self.freecad = sys.modules["FreeCAD"]
        self.freecad.getDocument = lambda name: self.docs[name]
        self.freecad.addDocumentObserver = lambda observer: setattr(self, "observer", observer)
        module._later = lambda _ms, callback: self.queue.append(callback)
        module.install()

    def open(self, name: str, *, gui_data: bool):
        """A document FreeCAD opened from a file, and activated."""
        path = write_fcstd(self.tmp_path / f"{name}.FCStd", gui_data=gui_data)
        doc = types.SimpleNamespace(Name=name, FileName=path, Objects=objects(self.gui_doc))
        self.docs[name] = doc
        self.observer.slotActivateDocument(doc)
        return doc

    def drain(self) -> None:
        while self.queue:
            self.queue.pop(0)()


@pytest.fixture
def watched(headless_files, camera_calls: list, tmp_path: Path) -> Watched:
    module, gui_doc = headless_files
    return Watched(module, gui_doc, tmp_path)


def visible_names(doc) -> list[str]:
    return [o.Name for o in doc.Objects if o.ViewObject.Visibility]


def test_a_document_opened_by_anything_else_is_restored_once_its_view_is_laid_out(
    watched: Watched, camera_calls: list
) -> None:
    doc = watched.open("h", gui_data=False)
    assert visible_names(doc) == []  # nothing happens inside the call that opened it
    watched.queue.pop(0)()
    assert visible_names(doc) == ["Box", "NoFlag"]  # Hidden stores false, an Origin keeps its default
    assert camera_calls == []  # the view is not known to be laid out yet
    watched.drain()
    assert camera_calls == [
        ("animation", False), ("orient", "Isometric"), ("fit", ["drawn"]), ("apply", "pose"), ("animation", True),
    ]
    assert watched.gui_doc.Modified is False


def test_the_observer_waits_until_the_view_size_settles(watched: Watched, camera_calls: list) -> None:
    camera_calls.sizes[:] = [(640, 480), (900, 600), (1000, 700)]
    watched.open("h", gui_data=False)
    watched.drain()
    # 640x480 and 900x600 are still changing; the framing came at 1000x700, read twice.
    assert camera_calls.count(("apply", "pose")) == 1 and camera_calls.sizes == [(1000, 700)]


def test_the_observer_frames_after_the_last_check_when_the_size_never_settles(
    watched: Watched, camera_calls: list
) -> None:
    camera_calls.sizes[:] = [(i, i) for i in range(1, watched.module.LAYOUT_CHECKS + 5)]
    watched.open("h", gui_data=False)
    watched.drain()
    assert camera_calls.count(("apply", "pose")) == 1


def test_a_moved_camera_is_left_alone(watched: Watched, camera_calls: list) -> None:
    for change in ({"orientation": (0.0, 0.7071, 0.0, 0.7071)}, {"position": (5.0, 0.0, 10.0)}):
        camera_calls.orientation, camera_calls.position = (0.0, 0.0, 0.0, 1.0), (0.0, 0.0, 10.0)
        doc = watched.open("h", gui_data=False)
        watched.queue.pop(0)()
        assert visible_names(doc) == ["Box", "NoFlag"]
        for field, value in change.items():  # the person moved the view, or an agent set one
            setattr(camera_calls, field, value)
        watched.drain()
        assert camera_calls == []
        watched.observer.slotDeletedDocument(doc)
        watched.queue.clear()


def test_a_camera_that_only_freecad_touched_is_still_framed(watched: Watched, camera_calls: list) -> None:
    doc = watched.open("h", gui_data=False)
    watched.queue.pop(0)()
    # Its text changes by itself (near, far, aspect ratio) on the first redraw and on a
    # resize; orientation and position are the same to well under a millionth.
    camera_calls.camera = "camera 0, nearDistance 1.5 farDistance 900 aspectRatio 2.1"
    camera_calls.position = (0.0, 0.0, 10.0 + 1e-9)
    watched.drain()
    assert camera_calls.count(("apply", "pose")) == 1 and visible_names(doc) == ["Box", "NoFlag"]


def test_a_set_view_between_the_open_and_the_restore_is_kept(watched: Watched, camera_calls: list) -> None:
    doc = watched.open("h", gui_data=False)  # the pose is recorded as the document opens
    camera_calls.orientation = (0.5, 0.5, 0.5, 0.5)  # set_view lands before the restore runs
    watched.drain()
    assert visible_names(doc) == ["Box", "NoFlag"] and camera_calls == []


def test_a_running_orbit_or_tour_is_not_reframed(watched: Watched, camera_calls: list) -> None:
    watched.open("h", gui_data=False)
    watched.queue.pop(0)()
    camera_calls.running = True
    watched.drain()
    assert camera_calls == []


def test_an_object_the_person_hides_after_the_restore_stays_hidden(watched: Watched, camera_calls: list) -> None:
    doc = watched.open("h", gui_data=False)
    watched.drain()
    assert visible_names(doc) == ["Box", "NoFlag"]
    doc.Objects[0].ViewObject.Visibility = False
    framed = list(camera_calls)
    watched.observer.slotActivateDocument(doc)  # a tab switch, an activation
    watched.drain()
    assert visible_names(doc) == ["NoFlag"] and camera_calls == framed


def test_a_failure_inside_the_observer_stays_quiet(watched: Watched, camera_calls: list) -> None:
    printed = sys.modules["FreeCADGui"].printed
    broken = types.SimpleNamespace(Name="b", Objects=[])  # no FileName: reading it raises
    watched.docs["b"] = broken
    watched.observer.slotActivateDocument(broken)
    watched.drain()  # would raise into Qt's timer handler
    # A view that cannot be measured fails the framing the same way.
    doc = watched.open("h", gui_data=False)
    watched.queue.pop(0)()
    sys.modules["rpc_server.view_manager"]._document_capture_view = lambda _gui_doc: types.SimpleNamespace(
        getCamera=lambda: camera_calls.camera, getSize=lambda: 1 / 0
    )
    watched.drain()
    kinds = [kind for kind, _ in printed]
    assert kinds and set(kinds) == {"PrintDeveloperWarning"}  # no PrintWarning, no PrintError
    assert visible_names(doc) == ["Box", "NoFlag"] and camera_calls == []


def test_a_document_with_gui_data_is_left_as_freecad_restored_it(watched: Watched, camera_calls: list) -> None:
    doc = watched.open("g", gui_data=True)
    watched.drain()
    assert visible_names(doc) == [] and camera_calls == []
    assert watched.gui_doc.Modified is False


def test_open_document_and_the_observer_do_not_restore_twice(watched: Watched, camera_calls: list) -> None:
    doc = watched.open("h", gui_data=False)  # FreeCAD activates it while open_document is still running
    watched.module.show_stored_visibility(doc, doc.FileName)  # open_document's own restore
    framed = list(camera_calls)
    assert framed and visible_names(doc) == ["Box", "NoFlag"]
    watched.drain()
    assert camera_calls == framed
    watched.observer.slotActivateDocument(doc)  # switching back to the tab later
    assert watched.queue == []


def test_a_document_opened_hidden_is_restored_when_it_gets_a_view(watched: Watched, camera_calls: list) -> None:
    gui_document = sys.modules["FreeCADGui"].getDocument
    sys.modules["FreeCADGui"].getDocument = lambda _name: (_ for _ in ()).throw(RuntimeError("no GUI document"))
    doc = watched.open("h", gui_data=False)
    watched.drain()
    assert visible_names(doc) == [] and camera_calls == []
    assert sys.modules["FreeCADGui"].printed == []
    sys.modules["FreeCADGui"].getDocument = gui_document
    watched.observer.slotActivateDocument(doc)
    watched.drain()
    assert visible_names(doc) == ["Box", "NoFlag"] and camera_calls


def test_a_closed_document_that_opens_again_is_restored_again(watched: Watched, camera_calls: list) -> None:
    doc = watched.open("h", gui_data=False)
    watched.drain()
    watched.observer.slotDeletedDocument(doc)
    again = watched.open("h", gui_data=False)
    watched.drain()
    assert visible_names(again) == ["Box", "NoFlag"]
    assert camera_calls.count(("apply", "pose")) == 2


def test_a_file_with_gui_data_keeps_what_freecad_restored(headless_files, tmp_path: Path) -> None:
    module, gui_doc = headless_files
    doc = types.SimpleNamespace(Name="h", Objects=objects(gui_doc))
    module.show_stored_visibility(doc, write_fcstd(tmp_path / "gui.FCStd", gui_data=True))
    assert not any(o.ViewObject.Visibility for o in doc.Objects)
    assert gui_doc.Modified is False

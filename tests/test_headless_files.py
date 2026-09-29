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
    calls: list = []
    view = types.SimpleNamespace(
        isAnimationEnabled=lambda: True, setAnimationEnabled=lambda on: calls.append(("animation", on))
    )
    view_mode = types.SimpleNamespace(
        import_coin=lambda: None,
        drawn_objects=lambda _doc: ["drawn"],
        fit_pose=lambda _view, objects: calls.append(("fit", objects)) or "pose",
        apply_pose=lambda _view, pose: calls.append(("apply", pose)),
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


def test_a_file_with_gui_data_keeps_what_freecad_restored(headless_files, tmp_path: Path) -> None:
    module, gui_doc = headless_files
    doc = types.SimpleNamespace(Name="h", Objects=objects(gui_doc))
    module.show_stored_visibility(doc, write_fcstd(tmp_path / "gui.FCStd", gui_data=True))
    assert not any(o.ViewObject.Visibility for o in doc.Objects)
    assert gui_doc.Modified is False

"""set_view frames the sphere for an orbit and the tight box for everything else."""

import importlib.util
import sys
import types
from pathlib import Path

import pytest

VIEW_CONTROL = Path(__file__).resolve().parents[1] / "addon/FreeCADMCP/rpc_server/view_control.py"


class RecordingViewMode(types.SimpleNamespace):
    """The camera engine's side of set_view, recording how each framing was asked for."""

    def __init__(self) -> None:
        pose = types.SimpleNamespace(ortho=False, quat=(0.0, 0.0, 0.0, 1.0))
        super().__init__(
            fits=[],
            DEFAULT_ORBIT_SPEED=15.0,
            DEFAULT_MOVE_SECONDS=2.0,
            DEFAULT_DWELL_SECONDS=2.0,
            import_coin=lambda: None,
            clear_stop=lambda _name: None,
            stop_mode=lambda _name, _reason="": None,
            check_visual_changes=lambda *_args: ({}, None),
            apply_visual_changes=lambda *_args: {"shown": [], "hidden": [], "transparency": {}, "display_mode": {}},
            _union_box=lambda _objects: object(),
            drawn_objects=lambda doc: list(doc.Objects),
            apply_pose=lambda _view, _pose: None,
            start_orbit=lambda *_args: None,
            start_tour=lambda *_args: None,
            status=lambda _name: {},
            read_pose=lambda _view: pose,
            _direction=lambda _quat: (0.0, 0.0, -1.0),
        )
        self.fit_pose = self._fit_pose

    def _fit_pose(self, view, objects, quat=None, sphere=False):
        self.fits.append(sphere)
        return object()


@pytest.fixture
def view_control(monkeypatch: pytest.MonkeyPatch):
    box = types.SimpleNamespace(Name="Box", ViewObject=types.SimpleNamespace(Visibility=True))
    doc = types.SimpleNamespace(Name="Doc", Objects=[box], getObject=lambda name: box if name == "Box" else None)
    view = types.SimpleNamespace(
        isAnimationEnabled=lambda: True, setAnimationEnabled=lambda _on: None, fitAll=lambda: None
    )
    gui_doc = types.SimpleNamespace(Modified=False)
    freecad = types.ModuleType("FreeCAD")
    freecad.ActiveDocument = doc
    freecad.setActiveDocument = lambda _name: None
    freecad_gui = types.ModuleType("FreeCADGui")
    freecad_gui.getDocument = lambda _name: gui_doc
    freecad_gui.getMainWindow = lambda: types.SimpleNamespace(setActiveWindow=lambda _view: None)

    view_mode = RecordingViewMode()
    errors = types.ModuleType("rpc_server.errors")
    errors.INVALID_INPUT = errors.NOT_FOUND = errors.UNAVAILABLE = ""
    errors.fail = lambda *args, **kwargs: {"success": False, "error": args}
    errors.tool_call = lambda *args, **kwargs: ""
    gui_task = types.ModuleType("rpc_server.gui_task")
    gui_task.run_on_gui = lambda *args, **kwargs: None
    lookup = types.ModuleType("rpc_server.lookup")
    lookup.require_document = lambda _name: (doc, None)
    view_manager = types.ModuleType("rpc_server.view_manager")
    view_manager._VIEW_DISPATCH = {"Isometric": None}
    view_manager._document_capture_view = lambda _gui_doc: view
    view_manager.apply_view_orientation = lambda _view, _name: None
    package = types.ModuleType("rpc_server")
    package.view_mode = view_mode
    for name, module in (
        ("FreeCAD", freecad),
        ("FreeCADGui", freecad_gui),
        ("rpc_server", package),
        ("rpc_server.view_mode", view_mode),
        ("rpc_server.errors", errors),
        ("rpc_server.gui_task", gui_task),
        ("rpc_server.lookup", lookup),
        ("rpc_server.view_manager", view_manager),
    ):
        monkeypatch.setitem(sys.modules, name, module)
    spec = importlib.util.spec_from_file_location("_view_control_test", VIEW_CONTROL)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module, view_mode


@pytest.mark.parametrize("focus", [None, ["Box"]], ids=["everything", "focus"])
@pytest.mark.parametrize("mode", ["static", "orbit", "tour"])
def test_only_an_orbit_frames_the_sphere(view_control, mode: str, focus: list[str] | None) -> None:
    module, view_mode = view_control
    options = {"mode": mode, **({"focus": focus} if focus else {})}
    reply = module._set_view_gui(None, options)
    assert reply["success"] is True, reply
    assert view_mode.fits == [mode == "orbit"]

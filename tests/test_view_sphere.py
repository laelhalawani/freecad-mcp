"""An orbit frames the sphere around the model, so no angle cuts it off."""

import importlib.util
import math
import sys
import types
from pathlib import Path

import pytest

VIEW_MODE = Path(__file__).resolve().parents[1] / "addon/FreeCADMCP/rpc_server/view_mode.py"

# A 40 x 20 x 10 box: the sphere around it has half its diagonal as radius.
RADIUS = math.dist((0, 0, 0), (40, 20, 10)) / 2
CENTER = (20.0, 10.0, 5.0)
QUAT = (0.0, 0.0, 0.0, 1.0)


@pytest.fixture
def view_mode(monkeypatch: pytest.MonkeyPatch) -> types.ModuleType:
    """view_mode with the FreeCAD side stubbed: the sphere maths needs none of it."""
    errors = types.ModuleType("rpc_server.errors")
    errors.INVALID_INPUT = errors.NOT_FOUND = ""
    errors.fail = errors.tool_call = lambda *args, **kwargs: None
    agent_log = types.ModuleType("rpc_server.agent_log")
    agent_log.agent_warning = lambda _message: None
    for name, module in (
        ("FreeCAD", types.ModuleType("FreeCAD")),
        ("FreeCADGui", types.ModuleType("FreeCADGui")),
        ("rpc_server.errors", errors),
        ("rpc_server.agent_log", agent_log),
    ):
        monkeypatch.setitem(sys.modules, name, module)
    spec = importlib.util.spec_from_file_location("_view_mode_test", VIEW_MODE)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def camera(focal: float = 10.0, height_angle: float = math.radians(45)) -> types.SimpleNamespace:
    return types.SimpleNamespace(
        focalDistance=types.SimpleNamespace(getValue=lambda: focal),
        heightAngle=types.SimpleNamespace(getValue=lambda: height_angle),
    )


@pytest.mark.parametrize("aspect", [4 / 3, 1.0, 0.5])
def test_orthographic_height_holds_the_sphere_on_the_limiting_side(view_mode, aspect: float) -> None:
    pose = view_mode._sphere_pose(camera(), CENTER, RADIUS, QUAT, aspect, True)
    assert pose.ortho and pose.focus == CENTER
    # The height is the diameter with the margin; a view narrower than tall
    # needs it wider by the aspect.
    assert pose.zoom == pytest.approx(2 * RADIUS * view_mode.FIT_MARGIN * max(1.0, 1 / aspect))
    assert pose.zoom * min(1.0, aspect) >= 2 * RADIUS * view_mode.FIT_MARGIN - 1e-9
    # The camera starts outside the sphere.
    assert pose.distance > RADIUS


@pytest.mark.parametrize("aspect", [4 / 3, 1.0, 0.5])
def test_perspective_distance_fits_the_smaller_field_of_view(view_mode, aspect: float) -> None:
    angle = math.radians(45)
    pose = view_mode._sphere_pose(camera(height_angle=angle), CENTER, RADIUS, QUAT, aspect, False)
    half_v = angle / 2
    half_h = math.atan(math.tan(half_v) * aspect)
    # The sphere touches the smaller half angle, less the margin.
    assert RADIUS / pose.distance == pytest.approx(math.sin(min(half_v, half_h)) / view_mode.FIT_MARGIN)
    assert pose.distance > RADIUS
    assert pose.zoom == pose.distance and not pose.ortho


def test_ortho_camera_already_farther_back_keeps_its_distance(view_mode) -> None:
    pose = view_mode._sphere_pose(camera(focal=500.0), CENTER, RADIUS, QUAT, 1.0, True)
    assert pose.distance == 500.0

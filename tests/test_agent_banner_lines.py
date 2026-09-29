"""The agent banner is two lines at most."""

import importlib.util
import sys
import time
import types
from pathlib import Path

import pytest

AGENT_OVERLAY = Path(__file__).resolve().parents[1] / "addon/FreeCADMCP/rpc_server/agent_overlay.py"


@pytest.fixture
def agent_overlay(monkeypatch: pytest.MonkeyPatch) -> types.ModuleType:
    """agent_overlay with the Qt and FreeCAD side stubbed: the text of the banner needs none of it."""
    qt_core = types.SimpleNamespace(QObject=object, Signal=lambda *_args: None, Slot=lambda *_args: (lambda f: f))
    pyside = types.ModuleType("PySide")
    pyside.QtCore = qt_core
    pyside.QtGui = types.SimpleNamespace()
    pyside.QtWidgets = types.SimpleNamespace(QWidget=object)
    package = types.ModuleType("rpc_server")
    package.request_context = types.ModuleType("rpc_server.request_context")
    package.session_lock = types.ModuleType("rpc_server.session_lock")
    for name, module in (
        ("FreeCAD", types.ModuleType("FreeCAD")),
        ("FreeCADGui", types.ModuleType("FreeCADGui")),
        ("PySide", pyside),
        ("rpc_server", package),
    ):
        monkeypatch.setitem(sys.modules, name, module)
    spec = importlib.util.spec_from_file_location("_agent_overlay_test", AGENT_OVERLAY)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def activity(label: str, phrase: str, blocking: bool = False) -> dict:
    return {"label": label, "phrase": phrase, "started": time.monotonic() - 3, "blocking": blocking}


def test_banner_is_two_lines_with_the_blocking_note_merged(agent_overlay) -> None:
    head, note = agent_overlay._lines([activity("Claude", "Recomputing the document", blocking=True)])
    assert head == "Claude: Recomputing the document (3 s)"
    assert note == agent_overlay.BLOCKING_LINE + " " + agent_overlay.WARNING_LINE


def test_banner_stays_two_lines_for_several_activities_without_blocking(agent_overlay) -> None:
    lines = agent_overlay._lines([activity("Claude", "Working"), activity("Other", "Working")])
    assert lines == ["Claude: Working (3 s) and 1 more", agent_overlay.WARNING_LINE]

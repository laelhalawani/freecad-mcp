"""Agent-facing console messages stay out of the Notification Area."""

import importlib.util
import sys
import types
from pathlib import Path

import pytest

AGENT_LOG = Path(__file__).resolve().parents[1] / "addon/FreeCADMCP/rpc_server/agent_log.py"


class Console:
    """The part of FreeCAD.Console the module uses; observers is {name: {kind: on}}."""

    def __init__(self, observers=None, developer=True):
        self.printed = []
        self.observers = observers if observers is not None else {}
        self.PrintError = lambda m: self.printed.append(("PrintError", m))
        self.PrintWarning = lambda m: self.printed.append(("PrintWarning", m))
        if developer:
            self.PrintDeveloperError = lambda m: self.printed.append(("PrintDeveloperError", m))
            self.PrintDeveloperWarning = lambda m: self.printed.append(("PrintDeveloperWarning", m))

    def GetStatus(self, observer, kind):
        return self.observers.get(observer, {}).get(kind)

    def SetStatus(self, observer, kind, status):
        self.observers[observer][kind] = status


@pytest.fixture
def agent_log(monkeypatch: pytest.MonkeyPatch):
    def load(console: Console) -> types.ModuleType:
        freecad = types.ModuleType("FreeCAD")
        freecad.Console = console
        monkeypatch.setitem(sys.modules, "FreeCAD", freecad)
        spec = importlib.util.spec_from_file_location("_agent_log_test", AGENT_LOG)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    return load


def test_agent_messages_use_the_developer_kind(agent_log) -> None:
    console = Console()
    module = agent_log(console)
    module.agent_error("boom\n")
    module.agent_warning("careful\n")
    assert console.printed == [("PrintDeveloperError", "boom\n"), ("PrintDeveloperWarning", "careful\n")]


def test_agent_messages_fall_back_where_freecad_has_no_developer_kind(agent_log) -> None:
    console = Console(developer=False)
    module = agent_log(console)
    module.agent_error("boom\n")
    module.agent_warning("careful\n")
    assert console.printed == [("PrintError", "boom\n"), ("PrintWarning", "careful\n")]


def test_notification_area_is_quiet_only_while_the_call_runs(agent_log) -> None:
    observer = {"Err": True, "Wrn": True, "Critical": True, "Notification": True}
    console = Console({"NotificationAreaObserver": observer})
    module = agent_log(console)
    with module.quiet_notifications():
        assert observer == {"Err": False, "Wrn": False, "Critical": True, "Notification": True}
    assert observer == {"Err": True, "Wrn": True, "Critical": True, "Notification": True}


def test_notification_area_is_restored_after_an_error_and_never_enabled_by_us(agent_log) -> None:
    observer = {"Err": True, "Wrn": False}  # warnings were already off: stay off
    console = Console({"NotificationAreaObserver": observer})
    module = agent_log(console)
    with pytest.raises(RuntimeError):
        with module.quiet_notifications():
            raise RuntimeError("agent code failed")
    assert observer == {"Err": True, "Wrn": False}


def test_overlapping_calls_and_jobs_restore_only_when_the_last_one_ends(agent_log) -> None:
    observer = {"Err": True, "Wrn": False}
    console = Console({"NotificationAreaObserver": observer})
    module = agent_log(console)
    with module.quiet_notifications():
        with module.quiet_notifications():
            assert observer == {"Err": False, "Wrn": False}
        assert observer == {"Err": False, "Wrn": False}, "the inner one ending must not restore"
    assert observer == {"Err": True, "Wrn": False}


def test_a_failed_restore_is_logged_not_swallowed(agent_log) -> None:
    observer = {"Err": True, "Wrn": True}
    console = Console({"NotificationAreaObserver": observer})
    logged = []
    console.PrintLog = logged.append
    real_set = console.SetStatus

    def set_status(name, kind, status):
        if status:
            raise RuntimeError("cannot restore")
        real_set(name, kind, status)

    console.SetStatus = set_status
    module = agent_log(console)
    with module.quiet_notifications():
        pass
    assert len(logged) == 2 and all("cannot restore" in line for line in logged)


def test_nothing_changes_where_freecad_has_no_notification_area(agent_log) -> None:
    module = agent_log(Console({}))
    with module.quiet_notifications():
        pass
    bare = types.SimpleNamespace()  # a FreeCAD whose Console has no GetStatus at all
    module = agent_log(bare)
    with module.quiet_notifications():
        pass

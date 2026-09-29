"""The settings file holds the auth token and is shared with the Go installer."""

import importlib
import json
import os
import stat
import sys
import types
from pathlib import Path

import pytest

ADDON_DIR = Path(__file__).resolve().parents[1] / "addon" / "FreeCADMCP"
if str(ADDON_DIR) not in sys.path:
    sys.path.insert(0, str(ADDON_DIR))


@pytest.fixture
def settings(tmp_path, monkeypatch):
    stub = types.ModuleType("FreeCAD")
    stub.getUserAppDataDir = lambda: str(tmp_path)
    stub.Console = types.SimpleNamespace(
        PrintWarning=lambda _message: None, PrintError=lambda _message: None
    )
    monkeypatch.setitem(sys.modules, "FreeCAD", stub)
    # A fresh import, bound to this test's stub: `from rpc_server import
    # settings` would return the module an earlier test left on the package.
    monkeypatch.delitem(sys.modules, "rpc_server.settings", raising=False)
    module = importlib.import_module("rpc_server.settings")
    return module, tmp_path / "freecad_mcp_settings.json"


def test_a_non_ascii_token_written_as_utf8_reads_back_intact(settings) -> None:
    module, path = settings
    # The Go installer rewrites the file as raw UTF-8 when it toggles auto-start.
    path.write_bytes(json.dumps({"auth_token": "pässwörd-€"}, ensure_ascii=False).encode("utf-8"))
    assert module.load_settings()["auth_token"] == "pässwörd-€"


def test_a_missing_file_gives_the_default_background_limit(settings) -> None:
    module, _path = settings
    assert module.load_settings()["background_after_minutes"] == 30


def test_saved_settings_round_trip(settings) -> None:
    module, path = settings
    module.save_settings({**module.load_settings(), "auth_token": "tökén"})
    assert path.exists()
    assert module.load_settings()["auth_token"] == "tökén"


@pytest.mark.skipif(os.name == "nt", reason="POSIX permissions")
def test_the_file_is_readable_only_by_the_user(settings) -> None:
    module, path = settings
    path.write_text("{}")
    path.chmod(0o644)  # as an older version left it
    module.save_settings({"auth_token": "secret"})
    assert stat.S_IMODE(path.stat().st_mode) == 0o600

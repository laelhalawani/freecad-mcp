"""execute_file and execute_file_async run a script file as execute_code and
execute_code_async run code, compiled under the file's own path."""

from pathlib import Path
import threading
import types
from unittest.mock import MagicMock

import pytest

from test_rpc_handlers import rpc_module, wait_for_job


def write_script(tmp_path: Path, source: str, name: str = "my script.py") -> str:
    path = tmp_path / name
    path.write_text(source, encoding="utf-8")
    return str(path)


def test_file_runs_with_file_set_only_for_the_run_and_shares_the_namespace(
    rpc_module: types.ModuleType, tmp_path: Path,
) -> None:
    rpc = rpc_module.FreeCADRPC()
    path = write_script(tmp_path, "shared_value = 41\nprint(__file__)\n")
    result = rpc.execute_file(path)
    assert result["success"] is True
    assert result["message"].endswith(path + "\n")
    assert "__file__" not in rpc_module._EXEC_NAMESPACE
    assert rpc.execute_code("print(shared_value + 1)")["message"].endswith("42\n")


def test_file_with_a_byte_order_mark_runs(rpc_module: types.ModuleType, tmp_path: Path) -> None:
    path = tmp_path / "bom.py"
    path.write_bytes(b"\xef\xbb\xbfprint('bom ok')\n")
    assert rpc_module.FreeCADRPC().execute_file(str(path))["message"].endswith("bom ok\n")


def test_error_in_a_file_names_the_file_and_its_line(
    rpc_module: types.ModuleType, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    logged: list[str] = []
    monkeypatch.setattr(rpc_module.FreeCAD.Console, "PrintError", logged.append)
    path = write_script(tmp_path, "import math\nprint('start')\n\nvalue = 1 / 0\n")
    result = rpc_module.FreeCADRPC().execute_file(path)
    assert result["success"] is False
    assert result["code"] == "freecad_error"
    assert result["error"] == f"ZeroDivisionError: division by zero ({path}, line 4)"
    # The Report View gets the full traceback, citing the file as well.
    assert any(f'File "{path}", line 4' in message for message in logged)
    assert "__file__" not in rpc_module._EXEC_NAMESPACE


def test_syntax_error_in_a_file_names_the_file_and_its_line(
    rpc_module: types.ModuleType, tmp_path: Path,
) -> None:
    path = write_script(tmp_path, "x = 1\n\ny = (\n")
    result = rpc_module.FreeCADRPC().execute_file(path)
    assert result["success"] is False
    assert result["error"] == f"SyntaxError: '(' was never closed ({path}, line 3)"


def test_inline_code_still_reports_the_plain_error(rpc_module: types.ModuleType) -> None:
    result = rpc_module.FreeCADRPC().execute_code("1 / 0")
    assert result == {"success": False, "error": "ZeroDivisionError: division by zero"}


def test_missing_or_relative_or_unreadable_file_is_refused_before_any_run(
    rpc_module: types.ModuleType, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    dispatch = MagicMock()
    monkeypatch.setattr(rpc_module, "dispatch_to_gui", dispatch)
    rpc = rpc_module.FreeCADRPC()
    missing = str(tmp_path / "missing.py")
    not_text = tmp_path / "binary.py"
    not_text.write_bytes(b"\xff\xfe\x00\x80")
    for method in (rpc.execute_file, rpc.execute_file_async):
        result = method(missing)
        assert result["success"] is False and result["code"] == "not_found" and missing in result["error"]
        result = method(str(tmp_path))
        assert result["success"] is False and result["code"] == "not_found"
        result = method("script.py")
        assert result["success"] is False and result["code"] == "invalid_input"
        result = method(str(not_text))
        assert result["success"] is False and result["code"] == "invalid_input" and str(not_text) in result["error"]
    dispatch.assert_not_called()


def test_invalid_timeout_is_refused_before_the_file_is_read(
    rpc_module: types.ModuleType, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    dispatch = MagicMock()
    monkeypatch.setattr(rpc_module, "dispatch_to_gui", dispatch)
    path = write_script(tmp_path, "must_not_run = True\n")
    result = rpc_module.FreeCADRPC().execute_file(path, 0)
    assert result["success"] is False and "timeout" in result["error"]
    dispatch.assert_not_called()
    assert "must_not_run" not in rpc_module._EXEC_NAMESPACE


def test_overlapping_file_runs_keep_file_until_the_last_one_ends(
    rpc_module: types.ModuleType, tmp_path: Path,
) -> None:
    rpc = rpc_module.FreeCADRPC()
    started_a, release_a = threading.Event(), threading.Event()
    started_b, release_b = threading.Event(), threading.Event()
    freecad = rpc_module.FreeCAD
    freecad.started_a, freecad.release_a = started_a, release_a
    freecad.started_b, freecad.release_b = started_b, release_b
    path_a = write_script(tmp_path, "FreeCAD.started_a.set()\nFreeCAD.release_a.wait(5)\n", "a.py")
    path_b = write_script(
        tmp_path,
        "FreeCAD.started_b.set()\nFreeCAD.release_b.wait(5)\nFreeCAD.file_seen_by_b = __file__\n",
        "b.py",
    )
    try:
        job_a = rpc.execute_file_async(path_a)["job_id"]
        assert started_a.wait(2)
        job_b = rpc.execute_file_async(path_b)["job_id"]
        assert started_b.wait(2)
        # The first run ends while the second still runs: __file__ must stay.
        release_a.set()
        assert wait_for_job(rpc, job_a)["state"] == "done"
        assert rpc_module._EXEC_NAMESPACE["__file__"] == path_b
    finally:
        release_a.set()
        release_b.set()
    job = wait_for_job(rpc, job_b)
    assert job["state"] == "done", job
    assert freecad.file_seen_by_b == path_b
    assert "__file__" not in rpc_module._EXEC_NAMESPACE


def test_async_file_runs_and_a_failure_cites_the_file_and_line(
    rpc_module: types.ModuleType, tmp_path: Path,
) -> None:
    rpc = rpc_module.FreeCADRPC()
    ok = write_script(tmp_path, "async_value = 7\nprint(__file__)\n", "ok.py")
    started = rpc.execute_file_async(ok)
    assert started["success"] is True
    job = wait_for_job(rpc, started["job_id"])
    assert job["state"] == "done" and job["file"] == ok
    assert rpc.execute_code("print(async_value)")["message"].endswith("7\n")
    assert "__file__" not in rpc_module._EXEC_NAMESPACE

    bad = write_script(tmp_path, "import math\n\nraise ValueError('line three')\n", "bad.py")
    job = wait_for_job(rpc, rpc.execute_file_async(bad)["job_id"])
    assert job["state"] == "failed" and job["error"] == "ValueError: line three"
    assert f'File "{bad}", line 3' in job["traceback"]

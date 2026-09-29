"""A FreeCAD whose RPC port is taken says so in its status bar."""

import errno
import socket
import threading
import time
import types
from collections.abc import Iterator
from contextlib import contextmanager
from unittest.mock import MagicMock
from xmlrpc.server import SimpleXMLRPCServer

import pytest

from test_rpc_command_feedback import commands
from test_rpc_handlers import rpc_module


@pytest.fixture
def port_commands(commands: tuple) -> tuple:
    """The commands fixture, plus the warning line start_rpc_server prints when the port is taken."""
    commands[2].PrintWarning = MagicMock()
    return commands


@contextmanager
def fake_addon(status: dict) -> Iterator[int]:
    """An XML-RPC server that answers get_rpc_status with ``status``; yields its port."""
    server = SimpleXMLRPCServer(("127.0.0.1", 0), logRequests=False, allow_none=True)
    server.register_function(lambda: status, "get_rpc_status")
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server.server_address[1]
    finally:
        server.shutdown()
        server.server_close()


def start_on_taken_port(rpc_module: types.ModuleType, port: int) -> OSError:
    """The real start_rpc_server failure for a port another socket holds."""
    with pytest.raises(OSError) as info:
        rpc_module.start_rpc_server(port=port)
    assert info.value.errno in rpc_module._PORT_IN_USE_ERRNOS
    return info.value


def wait_for_message(status_bar: MagicMock) -> str:
    """The message the probe thread put in the status bar, with its timeout 0."""
    deadline = time.monotonic() + 5
    while not status_bar.showMessage.called and time.monotonic() < deadline:
        time.sleep(0.01)
    status_bar.showMessage.assert_called_once()
    message, timeout = status_bar.showMessage.call_args.args
    assert timeout == 0, "the message must stay until something replaces it"
    return message


def test_port_held_by_another_freecad_names_its_pid(port_commands: tuple) -> None:
    module, rpc_module, console, status_bar = port_commands
    with fake_addon({"pid": 4321, "hostname": socket.gethostname()}) as port:
        exc = start_on_taken_port(rpc_module, port)
        module.report_port_taken(port, exc)
        message = wait_for_message(status_bar)
    assert message == (
        "Agents are not connected to this window: "
        f"another FreeCAD (pid 4321) already serves them on port {port}."
    )
    console.PrintError.assert_called_once_with(message + "\n")


def test_port_held_by_another_program_says_so(port_commands: tuple) -> None:
    module, rpc_module, _console, status_bar = port_commands
    with socket.socket() as occupied:
        occupied.bind(("127.0.0.1", 0))
        occupied.listen()
        port = occupied.getsockname()[1]
        exc = start_on_taken_port(rpc_module, port)
        module.report_port_taken(port, exc)
        message = wait_for_message(status_bar)
    assert message == f"Agents are not connected to this window: port {port} is in use by another program."


def test_answer_from_another_computer_is_not_named_as_a_freecad_here(port_commands: tuple) -> None:
    # WSL's own localhost forwarding can answer for a FreeCAD on another host.
    module, rpc_module, _console, status_bar = port_commands
    with fake_addon({"pid": 4321, "hostname": socket.gethostname() + "-elsewhere"}) as port:
        exc = start_on_taken_port(rpc_module, port)
        module.report_port_taken(port, exc)
        message = wait_for_message(status_bar)
    assert message.endswith(f"port {port} is in use by another program.")


def test_other_start_failures_are_left_to_the_caller(port_commands: tuple) -> None:
    module, _rpc_module, _console, status_bar = port_commands
    module.report_port_taken(9875, OSError(errno.EACCES, "permission denied"))
    module.report_port_taken(9875, RuntimeError("cannot start server thread"))
    time.sleep(0.1)
    status_bar.showMessage.assert_not_called()

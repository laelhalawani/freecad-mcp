"""Persistence of MCP RPC server settings under FreeCAD's user app data dir."""

import json
import os
import tempfile
import threading
import time

import FreeCAD


_SETTINGS_FILENAME = "freecad_mcp_settings.json"

_DEFAULT_SETTINGS = {
    # On: the freecad-mcp listener accepts other devices, and the session
    # lock is on. The RPC server itself always binds 127.0.0.1.
    "remote_enabled": False,
    # Devices the listener accepts (comma-separated addresses or subnets,
    # never empty). The RPC server itself accepts loopback only.
    "allowed_ips": "127.0.0.1",
    "auto_start_rpc": False,
    # The password, set in freecad-mcp's "Share this PC"; empty means none.
    "auth_token": "",
    # Idle time after which another agent can take FreeCAD, 1 to 1440.
    "session_timeout_minutes": 30,
    # Minutes a call may keep the agent waiting before the MCP server hands
    # it a job id to poll instead, 1 to 1440.
    "background_after_minutes": 30,
    # The listener's port on this computer.
    "listener_port": 9876,
}

# Callbacks told about a changed settings file (see on_change and poll).
_change_callbacks = []

# poll() watcher state. A sentinel (rather than None, a valid "file absent"
# reading) marks "never checked yet", so the first poll of a process always
# applies the settings on disk (see poll()'s docstring). _last_good_stat only
# ever advances past a file state that was actually read and applied: a
# broken file is retried on every later poll instead of being remembered as
# "already handled", and the settings already applied (the password, the
# session lock) are kept rather than reset to the defaults.
_POLL_INTERVAL_S = 2.0
_UNSET = object()
_poll_lock = threading.Lock()
_last_poll_check = float("-inf")
_last_good_stat = _UNSET
# True while the settings file exists but could not be read or parsed (or
# holds a wrongly typed auth_token): the RPC server refuses every call until
# it clears, rather than running with the password or the session lock
# reset to "none" (see rpc_server.py's _dispatch and unreadable()).
_settings_unreadable = False


def _get_settings_path():
    return os.path.join(FreeCAD.getUserAppDataDir(), _SETTINGS_FILENAME)


# The file is UTF-8 whatever the locale: freecad-mcp's installer writes it too.
_ENCODING = "utf-8"


def load_settings():
    """Read the settings file, falling back to the defaults for a missing
    file, a missing key, or a file that cannot be read or parsed.

    For display only (the toolbar commands, the status widget): a value read
    here is never applied to the running server. poll() and start_rpc_server
    use load_settings_or_raise instead, so a transient or malformed file
    never silently drops the password or turns the session lock off.
    """
    try:
        return load_settings_or_raise()
    except FileNotFoundError:
        return dict(_DEFAULT_SETTINGS)
    except Exception as e:
        FreeCAD.Console.PrintWarning(f"Failed to load MCP settings: {e}\n")
        return dict(_DEFAULT_SETTINGS)


def load_settings_or_raise():
    """Read the settings file, filling in defaults for missing keys.

    Raises when the file exists but is not valid JSON, is not a JSON object,
    or holds ``auth_token`` as anything but a string (a hand-edited file):
    the caller then keeps its last good settings instead of treating this as
    "no password, remote access off".
    """
    path = _get_settings_path()
    if not os.path.exists(path):
        return dict(_DEFAULT_SETTINGS)
    with open(path, "r", encoding=_ENCODING) as f:
        settings = json.load(f)
    if not isinstance(settings, dict):
        raise ValueError(f"{path} does not hold a JSON object")
    if "auth_token" in settings and not isinstance(settings["auth_token"], str):
        raise ValueError(f"{path}: auth_token must be a string")
    for key, value in _DEFAULT_SETTINGS.items():
        if key not in settings:
            settings[key] = value
    return settings


def unreadable():
    """Report whether the settings file is known to be unreadable right now."""
    return _settings_unreadable


def on_change(callback):
    """Register ``callback(settings)`` to run when the settings file changes.

    freecad-mcp rewrites the file ("Share this PC", the install wizard), so a
    new password or lock setting applies without restarting the RPC server.
    Registering the same callback twice has no effect.
    """
    if callback not in _change_callbacks:
        _change_callbacks.append(callback)


def poll():
    """Call the registered callbacks with the new settings if the file changed.

    Checks the file's modification time and size at most every 2 s; called
    on every RPC request and by the status widget's timer. Never raises.

    _poll_lock is held for the whole check-and-apply, including
    load_settings_or_raise() and the callbacks: a throttled concurrent caller
    then waits for an apply already in progress instead of returning past it,
    so it never reports state older than the change it just missed applying
    itself. The callbacks only ever acquire session_lock's own lock, never
    this one, so this nesting has one fixed order and cannot deadlock against
    it. The very first poll of a process treats the file as changed (instead
    of only recording a baseline), so a settings write between server start
    and that first poll is still picked up; the callbacks are idempotent, so
    reapplying settings that turn out to be the same ones the server already
    started with is harmless.

    A file that exists but cannot be read or parsed (or holds a wrongly
    typed auth_token) is not applied: the previous settings stay in effect
    (_settings_unreadable becomes true, refusing every RPC call, see
    rpc_server.py's _dispatch), one warning is printed, and _last_good_stat
    is not advanced, so the very next poll tries to read it again rather
    than waiting for it to change further.
    """
    global _last_poll_check, _last_good_stat, _settings_unreadable
    now = time.monotonic()
    with _poll_lock:
        if now - _last_poll_check < _POLL_INTERVAL_S:
            return
        _last_poll_check = now
        try:
            st = os.stat(_get_settings_path())
            current = (st.st_mtime, st.st_size)
        except OSError:
            current = None
        previous = _last_good_stat
        first_poll = previous is _UNSET
        if not first_poll and current == previous:
            return
        try:
            settings = load_settings_or_raise()
        except Exception as e:
            if not _settings_unreadable:
                FreeCAD.Console.PrintWarning(
                    "MCP RPC: settings file could not be read, keeping the "
                    f"previous settings until it can be read again: {e}\n"
                )
            _settings_unreadable = True
            return
        was_unreadable = _settings_unreadable
        _settings_unreadable = False
        _last_good_stat = current
        for callback in list(_change_callbacks):
            try:
                callback(settings)
            except Exception as e:
                FreeCAD.Console.PrintWarning(
                    f"MCP RPC: settings change callback failed: {type(e).__name__}: {e}\n"
                )
        if was_unreadable:
            FreeCAD.Console.PrintMessage("MCP RPC: settings file is readable again.\n")


def save_settings(settings):
    """Write the settings, readable only by the user: they hold the auth token.

    The file is replaced atomically: the settings are written to a temporary
    file in the same directory, which then takes the settings file's place, so
    a crash or a failed write leaves the previous settings intact.
    """
    path = _get_settings_path()
    tmp_path = None
    try:
        # mkstemp creates the file with mode 0600 on POSIX.
        fd, tmp_path = tempfile.mkstemp(
            prefix=_SETTINGS_FILENAME + ".", suffix=".tmp", dir=os.path.dirname(path)
        )
        with open(fd, "w", encoding=_ENCODING) as f:
            if os.name != "nt":
                os.fchmod(f.fileno(), 0o600)  # exactly 0600, whatever the umask
            json.dump(settings, f, indent=2)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp_path, path)
        tmp_path = None
    except Exception as e:
        FreeCAD.Console.PrintError(f"Failed to save MCP settings: {e}\n")
    finally:
        if tmp_path is not None:
            try:
                os.remove(tmp_path)
            except OSError:
                pass

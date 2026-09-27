"""Persistence of MCP RPC server settings under FreeCAD's user app data dir."""

import json
import os
import tempfile

import FreeCAD


_SETTINGS_FILENAME = "freecad_mcp_settings.json"

_DEFAULT_SETTINGS = {
    "remote_enabled": False,
    "allowed_ips": "127.0.0.1",
    "auto_start_rpc": False,
    "auth_token": "",  # empty = authentication disabled
}


def _get_settings_path():
    return os.path.join(FreeCAD.getUserAppDataDir(), _SETTINGS_FILENAME)


# The file is UTF-8 whatever the locale: freecad-mcp's installer writes it too.
_ENCODING = "utf-8"


def load_settings():
    path = _get_settings_path()
    if os.path.exists(path):
        try:
            with open(path, "r", encoding=_ENCODING) as f:
                settings = json.load(f)
            for key, value in _DEFAULT_SETTINGS.items():
                if key not in settings:
                    settings[key] = value
            return settings
        except Exception as e:
            FreeCAD.Console.PrintWarning(f"Failed to load MCP settings: {e}\n")
    return dict(_DEFAULT_SETTINGS)


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

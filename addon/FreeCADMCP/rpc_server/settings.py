"""Persistence of MCP RPC server settings under FreeCAD's user app data dir."""

import json
import os

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
    """Write the settings, readable only by the user: they hold the auth token."""
    path = _get_settings_path()
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with open(fd, "w", encoding=_ENCODING) as f:
            if os.name != "nt":
                os.fchmod(f.fileno(), 0o600)  # also for a file an older version created
            json.dump(settings, f, indent=2)
    except Exception as e:
        FreeCAD.Console.PrintError(f"Failed to save MCP settings: {e}\n")

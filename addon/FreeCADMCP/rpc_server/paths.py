"""Shared absolute-path validation.

A path that is still relative is ``invalid_input`` (a drive-less Windows path
gets its own message naming that); every such failure carries a "For
example: ..." hint pointing at a real, writable path, built by
``home_example``.
"""

import os
from typing import Any

from rpc_server.errors import INVALID_INPUT, fail


def home_example(name: str) -> str:
    """A real, writable absolute path for a hint's example, forward-slashed.

    The user's home directory, not the FreeCAD process's own working
    directory: that is usually its install or launcher directory (e.g.
    "D:\\Program Files\\FreeCAD 1.1\\bin" on Windows), not somewhere an agent
    should write a file. Shared by every hint that suggests a path (a
    relative or drive-less path here, a never-saved document elsewhere).
    """
    home = os.path.expanduser("~").replace(os.sep, "/")
    return f"{home}/{name}"


def ensure_parent_directory(path: str) -> tuple[str | None, dict[str, Any] | None]:
    """Create ``path``'s folder (and any missing parents) when it does not exist.

    Returns ``(created_directory, None)``, where ``created_directory`` is None
    when the folder already existed, or ``(None, fail())`` when it cannot be
    created. FreeCAD's writers do not all raise a catchable error for a
    missing folder; without one the write silently does nothing.
    """
    directory = os.path.dirname(path)
    if not directory or os.path.isdir(directory):
        return None, None
    try:
        os.makedirs(directory, exist_ok=True)
    except OSError as e:
        return None, fail(
            INVALID_INPUT,
            f"the folder '{directory}' does not exist and could not be created: {e}",
            "Check that the drive exists and is writable, or choose another path.",
        )
    return os.path.abspath(directory), None


def require_absolute_path(path: Any) -> tuple[str, None] | tuple[None, dict[str, Any]]:
    """Return ``(expanded_path, None)``, or ``(None, fail())`` when ``path`` cannot be used.

    Checks ``path``'s type before touching the filesystem: ``os.path.expanduser``
    on a non-string value (``None``, a number) raises ``TypeError``, which
    would otherwise escape as a raw Fault instead of an ``invalid_input``
    reply. ``os.path.isabs`` alone accepts a drive-less root such as
    "/tmp/x.stl" or "\\x.stl" on Windows: it is "absolute" only in the sense
    that it does not depend on the current directory, but it still resolves
    against whatever drive the FreeCAD process happens to be running from,
    silently landing an agent's POSIX-style path on the wrong drive. A drive
    letter or UNC share is required there too, with its own message naming
    the path, since "Pass an absolute path" alone looks wrong to an agent
    that already did.
    """
    if not isinstance(path, str) or not path:
        return None, fail(
            INVALID_INPUT,
            "Pass an absolute path.",
            f"For example: {home_example('file.ext')}",
        )
    expanded = os.path.expanduser(path)
    if not os.path.isabs(expanded):
        example_name = os.path.basename(expanded) or "file.ext"
        return None, fail(
            INVALID_INPUT,
            "Pass an absolute path.",
            f"For example: {home_example(example_name)}",
        )
    if os.name == "nt" and not os.path.splitdrive(expanded)[0]:
        example_name = os.path.basename(expanded) or "file.ext"
        return None, fail(
            INVALID_INPUT,
            f"'{path}' has no drive letter; it resolves against the FreeCAD "
            "process's own drive, which is likely not what was meant.",
            f"For example: {home_example(example_name)}",
        )
    return expanded, None

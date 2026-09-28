"""Who sent the XML-RPC request being handled on this thread.

The request handler (ip_filter.TokenAuthRequestHandler.parse_request) stores
the X-FreeCAD-MCP-Session and X-FreeCAD-MCP-Client headers and the client
address here for every request; FreeCADRPC._dispatch and the session lock
read them. The XML-RPC server handles each request on its own thread
(ThreadingMixIn), so a thread-local holds exactly the current request.
"""

import threading
from typing import NamedTuple


class Context(NamedTuple):
    """The current request's identity; None where a header was absent or invalid."""

    session: str | None  # X-FreeCAD-MCP-Session; None counts as the session "anonymous"
    client: str | None  # X-FreeCAD-MCP-Client, the agent's label
    ip: str | None  # the client address


_EMPTY = Context(None, None, None)
_local = threading.local()


def set(session: str | None, client: str | None, ip: str | None) -> None:
    """Record the identity of the request this thread handles."""
    _local.ctx = Context(session, client, ip)


def clear() -> None:
    """Forget the identity, for example before a kept-alive connection's next request."""
    _local.ctx = _EMPTY


def get() -> Context:
    """Return the identity of the request this thread handles."""
    return getattr(_local, "ctx", _EMPTY)

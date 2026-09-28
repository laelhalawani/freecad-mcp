"""IP-filtered, optionally token-authenticated XML-RPC server and helpers.

The server also refuses requests a web page could have sent. The IP allowlist
cannot stop those: the browser runs on an allowed machine, so a malicious page
could otherwise call execute_code (CSRF, or DNS rebinding to read the reply).
"""

import base64
import hmac
import ipaddress
import re
import socket
import sys
import time
from email.message import Message
from socketserver import ThreadingMixIn
from xmlrpc.server import SimpleXMLRPCRequestHandler, SimpleXMLRPCServer

import FreeCAD

from rpc_server import request_context, session_lock
from rpc_server.settings import poll as poll_settings


_XML_MEDIA_TYPES = frozenset({"text/xml", "application/xml"})

# Headers identifying the calling MCP server session. The listener forwards
# both and adds none of its own.
_HEADER_SESSION = "X-FreeCAD-MCP-Session"
_HEADER_CLIENT = "X-FreeCAD-MCP-Client"
_HEADER_LOCK = "X-FreeCAD-MCP-Lock"

_SESSION_RE = re.compile(r"^[A-Za-z0-9._:-]{1,128}$")


def _valid_session(value: str | None) -> str | None:
    """The session header, or None when absent or not 1 to 128 chars of [A-Za-z0-9._:-]."""
    if value is None:
        return None
    value = value.strip()
    return value if _SESSION_RE.match(value) else None


def _valid_client(value: str | None) -> str | None:
    """The client label header, or None when absent, not valid UTF-8, empty, too long or not printable.

    http.server decodes header bytes one character per byte (latin-1);
    encoding back to those bytes and decoding as UTF-8 recovers the text the
    sender wrote, so length and printability are checked in characters, not
    raw bytes.
    """
    if value is None:
        return None
    try:
        value = value.encode("latin-1").decode("utf-8")
    except UnicodeError:
        return None
    value = value.strip()
    return value if value and len(value) <= 80 and value.isprintable() else None


def _host_name(host_header: str) -> str:
    """Return the host of a Host header value, without port or brackets."""
    host = host_header.strip().lower()
    if host.startswith("["):  # IPv6 literal, e.g. [::1]:9875
        return host[1:].split("]", 1)[0]
    return host.rsplit(":", 1)[0].rstrip(".")


def _is_loopback_name(name: str) -> bool:
    if name == "localhost":
        return True
    try:
        return ipaddress.ip_address(name).is_loopback
    except ValueError:
        return False


def browser_request_rejection(
    headers: Message, loopback_only: bool
) -> tuple[int, str] | None:
    """Return ``(status, reason)`` for a request to refuse, or None to accept it.

    Browsers attach Origin to every POST, and can send an XML body to another
    origin only after a CORS preflight, which this server never answers.
    xmlrpc.client sends text/xml without Origin, so MCP clients are unaffected.
    The Host check stops DNS rebinding where it is decidable: a server bound to
    loopback is only ever addressed as localhost.
    """
    if headers.get("Origin") is not None:
        return 403, "requests from web pages are not accepted"
    media_type = (headers.get("Content-Type") or "").split(";", 1)[0].strip().lower()
    if media_type not in _XML_MEDIA_TYPES:
        return 415, "XML-RPC requests must use Content-Type text/xml"
    host = headers.get("Host")
    if loopback_only and host is not None and not _is_loopback_name(_host_name(host)):
        return 403, "the local RPC server only accepts requests addressed to localhost"
    return None


# A refused request's body is read and dropped, up to this size and for at
# most this long in total, so an unauthenticated peer cannot use it to hold
# a thread.
_DISCARD_MAX_BYTES = 16 * 1024 * 1024
_DISCARD_TIMEOUT_S = 5.0

# Longest wait for any single read or write on a request's socket. A peer that
# stops sending (for example in the middle of its headers) is disconnected
# after this long instead of holding a request thread forever. Time spent
# running the called method is not limited by it.
_REQUEST_TIMEOUT_S = 30.0


class BrowserGuardRequestHandler(SimpleXMLRPCRequestHandler):
    """Refuse requests a web page could have sent before they are dispatched."""

    # StreamRequestHandler.setup applies this to the connection.
    timeout = _REQUEST_TIMEOUT_S

    def end_headers(self) -> None:
        """Add X-FreeCAD-MCP-Lock to every reply, including the 401/403/415 ones."""
        try:
            enabled = bool(session_lock.snapshot().get("enabled", False))
        except Exception:
            enabled = False
        self.send_header(_HEADER_LOCK, "on" if enabled else "off")
        super().end_headers()

    def discard_body(self) -> None:
        """Read the unread body of a request that is being refused.

        Closing a socket with unread input sends a TCP reset instead of a
        normal close, and on Windows the reset can reach the client before it
        reads the reply: it then sees "connection aborted" instead of the
        status that says why (401 for a wrong token, 403, 415). A body sent
        without Content-Length (chunked) is not read.
        """
        try:
            remaining = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            return
        if remaining <= 0 or remaining > _DISCARD_MAX_BYTES:
            return
        deadline = time.monotonic() + _DISCARD_TIMEOUT_S
        read = getattr(self.rfile, "read1", self.rfile.read)
        try:
            while remaining > 0:
                left = deadline - time.monotonic()
                if left <= 0:
                    return
                if self.timeout is not None:
                    left = min(left, self.timeout)
                self.connection.settimeout(left)
                # read1 returns what has arrived instead of waiting for the
                # whole chunk, so the deadline holds for a slow sender too.
                chunk = read(min(remaining, 64 * 1024))
                if not chunk:
                    return
                remaining -= len(chunk)
        except OSError:
            pass  # the reply is still sent; the client may just see a reset
        finally:
            # The reply is written with the handler's own timeout, not with
            # whatever was left of the discard deadline.
            try:
                self.connection.settimeout(self.timeout)
            except OSError:
                pass

    def do_POST(self) -> None:
        rejection = browser_request_rejection(self.headers, self.server.loopback_only)
        if rejection is None:
            super().do_POST()
            return
        status, reason = rejection
        FreeCAD.Console.PrintWarning(
            f"MCP RPC: Rejected request from {self.client_address[0]}: {reason}\n"
        )
        self.discard_body()
        body = reason.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")  # the unread body must not be parsed as a request
        self.end_headers()
        self.wfile.write(body)


def authorization_ok(header_value: str, token: str) -> bool:
    """Check an ``Authorization`` header against the configured token.

    Accepts ``Bearer <token>`` and HTTP Basic (token in the password field,
    username ignored) so stdlib clients can use ``http://:token@host:port``
    URIs. Comparisons are constant-time and never raise.

    ``header_value`` is the header as http.server decodes it, one character
    per byte (latin-1), and ``token`` is matched as UTF-8, which is how
    clients send a non-ASCII token. Both are compared as bytes:
    ``hmac.compare_digest`` refuses str values with non-ASCII characters.
    """
    if not isinstance(header_value, str) or not isinstance(token, str):
        return False
    try:
        raw = header_value.encode("latin-1")
    except UnicodeEncodeError:
        return False  # not a value read from the wire
    expected = token.encode("utf-8", "surrogatepass")
    if raw.startswith(b"Bearer "):
        # bytes.strip removes ASCII whitespace only; str.strip would also
        # remove U+0085 and U+00A0, which are bytes of UTF-8 characters here.
        supplied = raw[len(b"Bearer "):].strip()
        return hmac.compare_digest(supplied, expected)
    if raw.startswith(b"Basic "):
        try:
            decoded = base64.b64decode(raw[len(b"Basic "):].strip(), validate=True)
        except ValueError:
            return False
        _, _, password = decoded.partition(b":")
        return hmac.compare_digest(password, expected)
    return False


class TokenAuthRequestHandler(BrowserGuardRequestHandler):
    """Request handler that enforces the server's auth token when one is set."""

    def parse_request(self):
        if not super().parse_request():
            return False
        # Reset first so a reused (keep-alive) connection never keeps a
        # previous request's identity if anything below raises.
        request_context.clear()
        request_context.set(
            _valid_session(self.headers.get(_HEADER_SESSION)),
            _valid_client(self.headers.get(_HEADER_CLIENT)),
            self.client_address[0],
        )
        # Pick up a changed password before checking it, so the first
        # request after a change in freecad-mcp is checked against the new
        # one instead of the one the server started with.
        poll_settings()
        token = getattr(self.server, "auth_token", "")
        if not token:
            return True  # authentication disabled
        if authorization_ok(self.headers.get("Authorization", ""), token):
            return True
        FreeCAD.Console.PrintWarning(
            f"MCP RPC: Rejected unauthenticated request from {self.client_address[0]}\n"
        )
        self.discard_body()
        self.send_error(401, "Unauthorized: valid auth token required")
        return False


class FilteredXMLRPCServer(ThreadingMixIn, SimpleXMLRPCServer):
    """XML-RPC server that filters connections by allowed IP addresses/subnets
    and, when a token is configured, requires an Authorization header.

    Threaded so get_rpc_status stays answerable while a wedged GUI task blocks
    another request. Document queries and synchronous modelling handlers
    serialise onto the GUI thread through dispatch_to_gui. The opt-in
    execute_code_async worker retains its existing background execution.

    daemon_threads must stay true: ThreadingMixIn.server_close() joins
    non-daemon request threads, which would make Stop wait out the stuck
    operation.
    """

    daemon_threads = True

    def __init__(self, addr, allowed_ips_str="127.0.0.1", auth_token="", **kwargs):
        self._allowed_networks = _parse_allowed_ips(allowed_ips_str)
        self.auth_token = auth_token or ""
        # Remote clients address the server by its LAN name or IP, so only a
        # loopback-bound server can require a localhost Host header.
        self.loopback_only = _is_loopback_name(str(addr[0]).lower())
        kwargs.setdefault("requestHandler", TokenAuthRequestHandler)
        super().__init__(addr, **kwargs)

    def server_bind(self):
        """Bind, exclusively on Windows: SO_EXCLUSIVEADDRUSE (final live
        check) makes this bind fail loudly instead of quietly sharing
        127.0.0.1:<port> with whatever already holds it, most notably WSL's
        own localhost port forwarding for the same port when FreeCAD is
        shared from inside WSL and on Windows on the same machine at once
        (see remote-access.md's WSL section). getattr guards a constant
        that exists only on Windows Python builds; sys.platform keeps this
        a no-op everywhere else, where the plain bind already fails the
        same way a second listener on the same port would.

        SO_REUSEADDR and SO_EXCLUSIVEADDRUSE conflict on the same socket on
        Windows (WinError 10022, WSAEINVAL) when both are set;
        SimpleXMLRPCServer's own allow_reuse_address is True in FreeCAD's
        Python, which would otherwise set SO_REUSEADDR right after this in
        TCPServer.server_bind, so it is turned off here first, only when
        this is actually setting the exclusive option (stage 7 blocker: the
        RPC server never started on Windows with both set).
        """
        if sys.platform == "win32":
            exclusive = getattr(socket, "SO_EXCLUSIVEADDRUSE", None)
            if exclusive is not None:
                self.allow_reuse_address = False
                self.socket.setsockopt(socket.SOL_SOCKET, exclusive, 1)
        super().server_bind()

    def verify_request(self, request, client_address):
        client_ip = client_address[0]
        try:
            addr = ipaddress.ip_address(client_ip)
            for network in self._allowed_networks:
                if addr in network:
                    return True
        except ValueError:
            pass
        FreeCAD.Console.PrintWarning(
            f"MCP RPC: Rejected connection from {client_ip}\n"
        )
        return False


_COMMA_SEP_RE = re.compile(r"^\s*[^,\s]+(\s*,\s*[^,\s]+)*\s*$")


def validate_allowed_ips(allowed_ips_str):
    """Validate a comma-separated string of IP addresses/subnets.

    Returns a ``(valid, errors)`` tuple.  ``valid`` is a list of normalised
    entry strings that passed validation; ``errors`` is a list of
    human-readable error messages (empty when the input is fully valid).

    Checks performed:
    1. The overall string is well-formed comma-separated (no leading/trailing
       commas, no empty entries between commas, not blank).
    2. Each individual entry is a valid IPv4/IPv6 address or CIDR subnet
       (validated via the stdlib ``ipaddress`` module).
    """
    errors = []

    if not allowed_ips_str or not allowed_ips_str.strip():
        return [], ["Input must not be empty."]

    if not _COMMA_SEP_RE.match(allowed_ips_str):
        return [], [
            "Malformed list: check for leading/trailing commas, "
            "double commas, or missing separators."
        ]

    valid = []
    for entry in allowed_ips_str.split(","):
        entry = entry.strip()
        try:
            ipaddress.ip_network(entry, strict=False)
            valid.append(entry)
        except ValueError:
            errors.append(f"Invalid IP/subnet: '{entry}'")
    return valid, errors


def _parse_allowed_ips(allowed_ips_str):
    """Parse a comma-separated string of IPs/subnets into a list of ip_network objects."""
    valid, errors = validate_allowed_ips(allowed_ips_str)
    for msg in errors:
        FreeCAD.Console.PrintWarning(f"MCP RPC: {msg}, skipping\n")
    return [ipaddress.ip_network(entry, strict=False) for entry in valid]

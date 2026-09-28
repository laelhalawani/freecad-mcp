"""Validate a handler's ``options`` argument: must be an object, and every
key in it must be one the handler recognises."""

from typing import Any

from rpc_server.errors import INVALID_INPUT, fail


def check_options(
    options: Any, allowed: tuple[str, ...]
) -> tuple[dict[str, Any], dict[str, Any] | None]:
    """Return ``(options or {}, None)``, or ``({}, fail())`` for a bad value.

    ``options`` must be an object (a JSON/XML-RPC struct); ``None`` means
    "not given" and normalises to ``{}``. Any key outside ``allowed`` is
    ``invalid_input``, naming the accepted keys.
    """
    if options is None:
        return {}, None
    if not isinstance(options, dict):
        return {}, fail(INVALID_INPUT, "options must be an object.")
    unknown = sorted(set(options) - set(allowed))
    if unknown:
        return {}, fail(
            INVALID_INPUT,
            f"Unknown option(s) {', '.join(unknown)}; accepted: {', '.join(allowed)}.",
        )
    return options, None

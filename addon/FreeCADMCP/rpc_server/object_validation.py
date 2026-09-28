"""Post-recompute validity checks for FreeCAD document objects."""

from typing import Any, Iterable


_FAILED_STATES = {"invalid", "error", "touched"}

MAX_LISTED_OBJECTS = 200
"""Row cap for invalid_objects_report() and any caller-built list of the same
shape (recompute_document's touched_objects, for instance).

A document with tens of thousands of flagged objects would otherwise return
a reply of several megabytes; invalid_objects_report()'s invalid_count still
gives the true total when invalid_truncated is set.
"""


def object_states(obj: Any) -> list[str]:
    """Return FreeCAD's state labels without assuming a concrete container."""
    try:
        raw_state = obj.State
    except Exception:
        return []

    if isinstance(raw_state, str):
        return [raw_state]

    try:
        return [str(item) for item in raw_state]
    except Exception:
        return []


def object_status(obj: Any) -> str:
    """Return ``obj.getStatusString()``, stripped, or "" when it is unavailable."""
    get_status = getattr(obj, "getStatusString", None)
    try:
        return str(get_status()).strip() if callable(get_status) else ""
    except Exception:
        return ""


def object_validity_error(obj: Any, *, exclude_touched: bool = False) -> str | None:
    """Return a diagnostic when ``obj`` is invalid, otherwise ``None``.

    Shape presence is deliberately not used as the discriminator. Containers,
    groups, spreadsheets, and empty sketches can all be valid without a shape.

    Touched counts as a failure by default: after an actual recompute,
    ``isValid()`` only reflects the Error bit, but a dependent of an object
    whose recompute failed is skipped entirely (never reaches its own
    recompute, and is never purged of Touched), so it stays Touched with
    ``isValid() == True`` even though it did not compute. A caller that runs
    no recompute of its own before checking (a document just opened, or
    freshly loaded state before any change) passes ``exclude_touched=True``,
    since there a bare Touched only means "not recomputed yet", not broken.
    """
    name = str(getattr(obj, "Name", "<unknown>"))
    states = object_states(obj)
    failed_state_names = _FAILED_STATES - {"touched"} if exclude_touched else _FAILED_STATES
    failed_states = [
        state for state in states if state.strip().casefold() in failed_state_names
    ]
    is_valid = getattr(obj, "isValid", None)

    if callable(is_valid):
        try:
            is_valid_result = bool(is_valid())
        except Exception as exc:
            return (
                f"Object '{name}' exists, but its validity could not be checked after "
                f"recompute: {type(exc).__name__}: {exc}. Fix or remove the object "
                "before building on it."
            )
    else:
        is_valid_result = True

    if is_valid_result and not failed_states:
        return None

    reason = object_status(obj)

    state = ", ".join(states) if states else "unknown"

    detail = f": {reason}" if reason else ""
    return (
        f"Object '{name}' exists but failed to compute{detail} "
        f"(State: {state}). Fix the cause or remove the object before building "
        "on it."
    )


def invalid_object_row(obj: Any) -> dict[str, Any]:
    """Build one row for ``obj``.

    ``{"name", "label", "type", "state": [str], "status": str}``, the shared
    shape every mutating and document-listing reply's ``invalid_objects``
    uses. Does not itself decide whether ``obj`` is invalid; call
    ``object_validity_error`` (directly, or through ``invalid_objects_report``)
    for that.
    """
    return {
        "name": str(getattr(obj, "Name", "")),
        "label": str(getattr(obj, "Label", "")),
        "type": str(getattr(obj, "TypeId", "")),
        "state": object_states(obj),
        "status": object_status(obj),
    }


def invalid_objects_report(
    objects: Iterable[Any], limit: int = MAX_LISTED_OBJECTS, *, exclude_touched: bool = False
) -> dict[str, Any]:
    """Return ``{"invalid_objects", "invalid_count", "invalid_truncated"}`` for ``objects``.

    Every mutating and document-listing reply's three invalid-object keys,
    built together in a single pass over ``objects`` so ``object_validity_error``
    runs once per object rather than once for the capped rows and again for
    the true count. ``objects`` is any iterable of FreeCAD document objects,
    most often a document's ``.Objects``, but a caller that only wants the
    objects it just created or touched may pass that narrower list instead.
    ``exclude_touched`` is passed through to ``object_validity_error``.
    ``invalid_objects`` is capped at ``limit`` rows; ``invalid_count`` is
    always the true total, and ``invalid_truncated`` is set once the cap cuts
    the list short.
    """
    rows: list[dict[str, Any]] = []
    count = 0
    for obj in objects:
        if object_validity_error(obj, exclude_touched=exclude_touched) is None:
            continue
        count += 1
        if len(rows) < limit:
            rows.append(invalid_object_row(obj))
    return {
        "invalid_objects": rows,
        "invalid_count": count,
        "invalid_truncated": count > len(rows),
    }

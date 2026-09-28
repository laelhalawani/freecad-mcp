"""Read and write spreadsheet cells.

Uses the Spreadsheet::Sheet API (Mod/Spreadsheet/App/Sheet.pyi): set, get,
getContents, setAlias, getAlias, getCellFromAlias, getNonEmptyCells. A cell
argument may be a plain address (B2), a range (A1:C3, reads only) or an
existing alias; ranges and aliases are resolved to plain addresses before a
cell is read or written, so every reply names cells by address.
"""

import re
from typing import Any

from rpc_server.errors import FREECAD_ERROR, INVALID_INPUT, NOT_FOUND, fail, tool_call
from rpc_server.gui_task import run_on_gui
from rpc_server.lookup import require_document
from rpc_server.object_validation import invalid_objects_report
from rpc_server.serialize import serialize_value
from rpc_server.transactions import active_document, transaction


MAX_READ_CELLS = 2000
MAX_WRITE_CELLS = 500

# FreeCAD's own sheet limits (App/Range.cpp: CellAddress::MAX_ROWS/MAX_COLUMNS):
# rows 1 to 16384, columns A to ZZ (26*26 + 26 = 702). Converting a column with
# more letters than that to an index is quadratic in its letter count, so
# addresses and ranges are rejected before any such conversion runs.
MAX_ROWS = 16384
MAX_COLUMNS = 702

# Fixed run budget: neither method takes a timeout argument.
_TIMEOUT = 60.0

_CELL_RE = re.compile(r"^([A-Za-z]+)(\d+)$")
_RANGE_RE = re.compile(r"^([A-Za-z]+\d+):([A-Za-z]+\d+)$")
# Sheet.cpp's isValidAlias (via PropertySheet::isValidAlias): a letter, then
# letters, digits or underscores.
_ALIAS_RE = re.compile(r"^[A-Za-z][_A-Za-z0-9]*$")
# PropertySheet::isValidCellAddressName: one or two uppercase letters, each
# optionally preceded by '$', then digits. Case-sensitive and only 1-2
# letters, unlike _CELL_RE, so an alias like "Length2" or "d2" does not match.
_ADDRESS_LIKE_RE = re.compile(r"^\$?([A-Z]{1,2})\$?([0-9]{1,5})$")

# ExpressionParser::isTokenAUnit / isTokenAConstant (App/Expression.l, tokens
# returning UNIT or CONSTANT), the reserved-name half of
# PropertySheet::isValidAlias. Case-sensitive: only these exact spellings are
# reserved, so "Length" or "meter" remain valid aliases.
_RESERVED_ALIAS_TOKENS = frozenset(
    {
        "nm", "um", "mm", "cm", "dm", "m", "km", "l", "ml", "Hz", "kHz", "MHz", "GHz", "THz", "ug",
        "mg", "g", "kg", "t", "s", "min", "h", "A", "mA", "kA", "MA", "K", "mK", "uK", "mol", "mmol",
        "cd", "in", "ft", "thou", "mil", "yd", "mi", "mph", "sqft", "cft", "lb", "lbm", "oz", "st",
        "cwt", "lbf", "N", "mN", "kN", "MN", "Pa", "kPa", "MPa", "GPa", "bar", "mbar", "Torr", "mTorr",
        "uTorr", "psi", "ksi", "Mpsi", "W", "mW", "kW", "VA", "V", "kV", "mV", "MS", "kS", "S", "mS",
        "uS", "Ohm", "kOhm", "MOhm", "C", "T", "G", "Wb", "F", "mF", "uF", "nF", "pF", "H", "mH", "uH",
        "nH", "J", "mJ", "kJ", "Nm", "VAs", "CV", "Ws", "kWh", "eV", "keV", "MeV", "cal", "kcal",
        "deg", "rad", "gon", "M", "AS", "pi", "e", "None", "True", "true", "False", "false",
    }
)


def _column_to_index(letters: str) -> int:
    """A -> 1, B -> 2, ..., Z -> 26, AA -> 27, ..."""
    index = 0
    for ch in letters.upper():
        index = index * 26 + (ord(ch) - ord("A") + 1)
    return index


def _index_to_column(index: int) -> str:
    letters = ""
    while index > 0:
        index, remainder = divmod(index - 1, 26)
        letters = chr(remainder + ord("A")) + letters
    return letters


def _endpoint_out_of_range(col_letters: str, row_str: str) -> bool:
    """True when a cell address token exceeds FreeCAD's sheet limits.

    Checked by string length before any int()/index conversion: that
    conversion is where the cost of a crafted, much-too-long token becomes
    quadratic (App/Range.cpp: CellAddress::MAX_ROWS/MAX_COLUMNS).
    """
    if len(col_letters) > 2 or len(row_str) > 5:
        return True
    return int(row_str) < 1 or int(row_str) > MAX_ROWS


def _range_bounds_error(ref: str) -> str | None:
    """Reject a requested range whose endpoints exceed FreeCAD's sheet limits.

    Needs no FreeCAD state, so it runs before dispatch and before any
    expansion: converting a much-too-long column to an index is quadratic in
    its letter count, so a crafted range such as "<50000 letters>1:...2" must
    never reach that conversion, let alone the GUI thread.
    """
    match = _RANGE_RE.match(ref)
    if not match:
        return None
    for token in (match.group(1), match.group(2)):
        cell = _CELL_RE.match(token)
        if cell and _endpoint_out_of_range(cell.group(1), cell.group(2)):
            return (
                f"'{ref}' is out of range: FreeCAD sheets go up to column ZZ "
                f"(column {MAX_COLUMNS}) and row {MAX_ROWS}."
            )
    return None


def _validate_read_refs(cells: Any) -> dict[str, Any] | None:
    """Reject a ``cells`` that is not a list of strings, or any entry outside
    FreeCAD's sheet limits."""
    if not isinstance(cells, list):
        return fail(INVALID_INPUT, f"cells must be a list of strings, got {type(cells).__name__}.")
    for i, ref in enumerate(cells):
        if not isinstance(ref, str):
            return fail(
                INVALID_INPUT,
                f"cells[{i}] must be a string cell address, range or alias, got {type(ref).__name__}.",
            )
        error = _range_bounds_error(ref)
        if error is not None:
            return fail(INVALID_INPUT, error)
    return None


def _cell_address_error(address: str) -> str | None:
    """Reject an address that is not a plain, in-range FreeCAD cell address."""
    match = _CELL_RE.match(address)
    if not match or _endpoint_out_of_range(match.group(1), match.group(2)):
        return (
            f"'{address}' is not a valid cell address: FreeCAD sheets use a column "
            f"(A to ZZ) and a row (1 to {MAX_ROWS}), such as B2."
        )
    return None


def _looks_like_cell_address(candidate: str) -> bool:
    """PropertySheet::isValidCellAddressName (PropertySheet.cpp:129-142): one
    or two uppercase letters, each optionally preceded by '$' (an absolute
    reference), then a row FreeCAD accepts (1 to MAX_ROWS). Case-sensitive
    and narrower than a general cell-shaped string: 'd2' and 'Length2' do not
    match, and isValidAlias accepts them.
    """
    match = _ADDRESS_LIKE_RE.match(candidate)
    return bool(match) and 1 <= int(match.group(2)) <= MAX_ROWS


def _alias_conflict(sheet: Any, address: str, alias: str) -> str | None:
    """Reject an alias FreeCAD's own Sheet::setAlias would refuse
    (Mod/Spreadsheet/App/Sheet.cpp:1541-1561, PropertySheet::isValidAlias):
    syntactically invalid, a real cell address, a reserved unit or constant
    name, or already the alias of a different cell.
    """
    if not _ALIAS_RE.match(alias):
        return f"'{alias}' is not a valid alias: use a letter, then letters, digits or underscores."
    if _looks_like_cell_address(alias):
        return f"'{alias}' is a cell address, which FreeCAD does not allow as an alias."
    if alias in _RESERVED_ALIAS_TOKENS:
        return f"'{alias}' is a unit or a constant that expressions recognise, which FreeCAD does not allow as an alias."
    try:
        existing = sheet.getCellFromAlias(alias)
    except Exception:
        existing = None
    if existing and existing != address:
        return f"'{alias}' is already the alias of cell {existing}."
    return None


def _iter_range(ref: str):
    """Yield 'A1:C3''s individual cell addresses lazily, row by row.

    Yields ``ref`` unchanged, once, when it is not a recognisable range. A
    generator so a huge range (A1:ZZ99999) is never materialised in full: the
    caller stops pulling once it has enough addresses.
    """
    match = _RANGE_RE.match(ref)
    if not match:
        yield ref
        return
    start, end = _CELL_RE.match(match.group(1)), _CELL_RE.match(match.group(2))
    if not start or not end:
        yield ref
        return
    col1, row1 = _column_to_index(start.group(1)), int(start.group(2))
    col2, row2 = _column_to_index(end.group(1)), int(end.group(2))
    if col1 > col2:
        col1, col2 = col2, col1
    if row1 > row2:
        row1, row2 = row2, row1
    for row in range(row1, row2 + 1):
        for col in range(col1, col2 + 1):
            yield f"{_index_to_column(col)}{row}"


def _resolve_cell(sheet: Any, ref: str) -> str:
    """Return the plain address ``ref`` names, resolving it as an alias first.

    ``getCellFromAlias`` gives "" (not an alias) for a plain address or a
    range, which is returned unchanged.
    """
    try:
        resolved = sheet.getCellFromAlias(ref)
    except Exception:
        resolved = None
    return resolved if resolved else ref


def _collect_addresses(sheet: Any, refs: list[str], limit: int) -> tuple[list[str], bool]:
    """Resolve and expand ``refs`` lazily, stopping once more than ``limit``
    addresses have been seen.

    Never materialises a whole range before truncating: a huge range (A1:ZZ99999)
    stops expanding as soon as ``limit`` is reached. Returns the addresses
    (at most ``limit``) and whether more existed beyond them.
    """
    addresses: list[str] = []
    truncated = False
    for ref in refs:
        for address in _iter_range(_resolve_cell(sheet, ref)):
            if len(addresses) >= limit:
                truncated = True
                break
            addresses.append(address)
        if truncated:
            break
    return addresses, truncated


_ERR_PREFIX = "ERR:"


def _error_message(value: Any) -> str | None:
    """The message of a FreeCAD-reported cell error, or None when ``value``
    is not one.

    FreeCAD 1.1's ``Sheet::recomputeCell`` (Mod/Spreadsheet/App/Sheet.cpp:
    936-950) does not leave a failed expression raising when read back:
    it stores ``"ERR: <message>"`` as the cell's own value (and separately
    marks the cell in ``cellErrors``, which is not exposed to Python), so
    ``sheet.get()`` succeeds and returns that string instead of raising.
    """
    if isinstance(value, str) and value.startswith(_ERR_PREFIX):
        return value[len(_ERR_PREFIX):].strip() or value
    return None


def _read_cell(sheet: Any, address: str) -> dict[str, Any]:
    """Return one row of get_spreadsheet_cells: cell, content, value, alias."""
    row: dict[str, Any] = {"cell": address}
    try:
        content = sheet.getContents(address)
    except Exception as e:
        row["content"] = ""
        row["value"] = None
        row["alias"] = ""
        row["error"] = f"{type(e).__name__}: {e}"
        return row
    row["content"] = content
    try:
        alias = sheet.getAlias(address)
    except Exception:
        alias = None
    row["alias"] = alias or ""
    if not content:
        # Nothing entered: get() would raise for the missing property, and
        # that is not an error, just an empty cell.
        row["value"] = None
        return row
    try:
        value = sheet.get(address)
    except Exception as e:
        row["value"] = None
        row["error"] = f"{type(e).__name__}: {e}"
        return row
    row["value"] = serialize_value(value)
    error_message = _error_message(value)
    if error_message is not None:
        row["error"] = error_message
    return row


def _error_cell_addresses(sheet: Any, limit: int = 20) -> list[str]:
    """Addresses of non-empty cells whose computed value is a FreeCAD-reported
    error (see ``_error_message``), capped at ``limit`` since this is only
    used to point a caller at the cells to fix, not to report every one.
    """
    addresses: list[str] = []
    for address in sheet.getNonEmptyCells():
        if len(addresses) >= limit:
            break
        try:
            value = sheet.get(address)
        except Exception:
            addresses.append(address)
            continue
        if _error_message(value) is not None:
            addresses.append(address)
    return addresses


def _find_sheet(doc_name: str, sheet_name: str):
    """Return (document, sheet, None) or (None, None, fail reply)."""
    doc, error = require_document(doc_name)
    if error is not None:
        return None, None, error
    sheet = doc.getObject(sheet_name)
    if sheet is None or sheet.TypeId != "Spreadsheet::Sheet":
        return None, None, fail(
            NOT_FOUND,
            f"'{sheet_name}' is not a Spreadsheet::Sheet object in document '{doc_name}'.",
            "Call "
            + tool_call(
                "create_object",
                {"doc_name": doc_name, "obj_type": "Spreadsheet::Sheet", "obj_name": "Params"},
            )
            + " to create one.",
        )
    return doc, sheet, None


def get_spreadsheet_cells(
    doc_name: str,
    sheet_name: str,
    cells: list[str] | None = None,
) -> dict[str, Any]:
    """Return the content, value and alias of cells of a sheet.

    Reply: ``{"success", "document", "sheet", "count", "truncated",
    "cells"}``. GUI thread, 60 s.
    """
    if cells:
        error = _validate_read_refs(cells)
        if error is not None:
            return error

    def task() -> dict[str, Any]:
        doc, sheet, error = _find_sheet(doc_name, sheet_name)
        if error is not None:
            return error
        if cells:
            addresses, truncated = _collect_addresses(sheet, cells, MAX_READ_CELLS)
        else:
            non_empty = list(sheet.getNonEmptyCells())
            truncated = len(non_empty) > MAX_READ_CELLS
            addresses = non_empty[:MAX_READ_CELLS]
        rows = [_read_cell(sheet, address) for address in addresses]
        return {
            "success": True,
            "document": doc.Name,
            "sheet": sheet.Name,
            "count": len(rows),
            "truncated": truncated,
            "cells": rows,
        }

    return run_on_gui(task, _TIMEOUT, "get_spreadsheet_cells")


def _validate_cell_updates(
    doc_name: str, sheet_name: str, cells: Any
) -> list[dict[str, Any]] | dict[str, Any]:
    """Normalise and validate ``cells`` (no FreeCAD state needed).

    ``doc_name`` and ``sheet_name`` are only used to write a copy-pasteable
    hint; they are not looked up here. Returns the normalised list, or a
    fail() reply.
    """
    if not isinstance(cells, list) or not (1 <= len(cells) <= MAX_WRITE_CELLS):
        return fail(
            INVALID_INPUT,
            f"cells must be a list of 1 to {MAX_WRITE_CELLS} entries, each {{'cell', 'content'?, 'alias'?}}.",
        )
    normalized = []
    for i, entry in enumerate(cells):
        if not isinstance(entry, dict):
            return fail(INVALID_INPUT, f"cells[{i}] must be an object with a 'cell' address or alias.")
        unknown = sorted(set(entry) - {"cell", "content", "alias"})
        if unknown:
            return fail(
                INVALID_INPUT,
                f"cells[{i}] has unknown keys {unknown}; accepted keys are cell, content, alias.",
            )
        cell = entry.get("cell")
        if not isinstance(cell, str) or not cell:
            return fail(INVALID_INPUT, f"cells[{i}] needs a non-empty 'cell' address or alias.")
        if _RANGE_RE.match(cell):
            return fail(
                INVALID_INPUT,
                f"cells[{i}] ('{cell}') is a range; address one cell at a time, ranges are read-only.",
                "Call update_spreadsheet_cells once per cell, e.g. "
                + tool_call(
                    "update_spreadsheet_cells",
                    {"doc_name": doc_name, "sheet_name": sheet_name, "cells": [{"cell": "A1", "content": "..."}]},
                ),
            )
        content = entry.get("content")
        alias = entry.get("alias")
        if content is None and alias is None:
            return fail(INVALID_INPUT, f"cells[{i}] ('{cell}') needs 'content', 'alias', or both.")
        if content is not None and not isinstance(content, str):
            return fail(INVALID_INPUT, f"cells[{i}].content must be a string, got {type(content).__name__}.")
        if alias is not None and not isinstance(alias, str):
            return fail(INVALID_INPUT, f"cells[{i}].alias must be a string, got {type(alias).__name__}.")
        normalized.append({"cell": cell, "content": content, "alias": alias})
    return normalized


def update_spreadsheet_cells(
    doc_name: str,
    sheet_name: str,
    cells: list[dict[str, Any]],
    recompute: bool = True,
) -> dict[str, Any]:
    """Set contents and aliases of cells of a sheet.

    Reply: ``{"success", "document", "sheet", "updated", "cells",
    "invalid_objects", "invalid_count", "invalid_truncated", "error_cells"?,
    "transaction", "transaction_merged"}``. ``error_cells`` is present, and
    lists this sheet's own erroring cell addresses, when an object is
    reported invalid: the usual "fix or remove the object" hint does not
    apply to a sheet whose value comes from its cells. GUI thread, 60 s,
    transaction ``MCP: update_spreadsheet_cells``.
    """
    normalized = _validate_cell_updates(doc_name, sheet_name, cells)
    if isinstance(normalized, dict):
        return normalized

    def task() -> dict[str, Any]:
        doc, sheet, error = _find_sheet(doc_name, sheet_name)
        if error is not None:
            return error

        # Validate every target before changing anything: an address FreeCAD
        # would reject, or an alias its own rules would refuse (Sheet.cpp
        # setAlias: "Invalid alias", "Alias already defined"), must not leave
        # earlier cells of the same batch applied and later ones silently
        # skipped. claimed_aliases catches two entries of this same batch
        # requesting the same new alias, which getCellFromAlias cannot see
        # yet (nothing has been applied at validation time).
        resolved: list[tuple[str, dict[str, Any]]] = []
        claimed_aliases: dict[str, str] = {}
        for item in normalized:
            address = _resolve_cell(sheet, item["cell"])
            addr_error = _cell_address_error(address)
            if addr_error:
                return fail(INVALID_INPUT, f"cells entry '{item['cell']}': {addr_error}")
            if item["alias"]:
                claimant = claimed_aliases.get(item["alias"])
                if claimant is not None and claimant != address:
                    return fail(
                        INVALID_INPUT,
                        f"cells entry '{item['cell']}': alias '{item['alias']}' is also requested for "
                        f"cell {claimant} earlier in this same call.",
                    )
                alias_error = _alias_conflict(sheet, address, item["alias"])
                if alias_error:
                    return fail(INVALID_INPUT, f"cells entry '{item['cell']}': {alias_error}")
                claimed_aliases[item["alias"]] = address
            resolved.append((address, item))

        # Snapshot every target's current content and alias before touching
        # any of them, so a mid-batch failure (an alias rule pre-validation
        # cannot check without FreeCAD, such as a reserved word) can restore
        # them: partial application would otherwise leave the model silently
        # half-updated with no way to tell which cells actually changed.
        originals: dict[str, tuple[str, str | None]] = {}
        for address, _item in resolved:
            if address in originals:
                continue
            try:
                original_content = sheet.getContents(address)
            except Exception:
                original_content = ""
            try:
                original_alias = sheet.getAlias(address)
            except Exception:
                original_alias = None
            originals[address] = (original_content, original_alias)

        # active_document holds doc active for as long as its transaction can
        # be open, so FreeCAD attaches the transaction to doc itself instead
        # of opening an empty linked "-> <name>" one in whatever document the
        # GUI has focused (transactions.active_document docstring).
        updated: list[str] = []
        did_recompute = False
        with active_document(doc), transaction("update_spreadsheet_cells") as tx:
            failure: dict[str, Any] | None = None
            try:
                for address, item in resolved:
                    # Recorded right after each call, not once per item: if
                    # content succeeds and the alias then raises, the content
                    # change already stuck and must still be rolled back.
                    if item["content"] is not None:
                        sheet.set(address, item["content"])
                        if address not in updated:
                            updated.append(address)
                    if item["alias"] is not None:
                        sheet.setAlias(address, item["alias"] or None)
                        if address not in updated:
                            updated.append(address)
            except Exception as e:
                # Pre-validation above rejects what it can recognise; this is
                # the safety net for whatever it cannot (a reserved word, a
                # property-name clash).
                rollback_problems: list[str] = []
                if tx.opened:
                    # This call's own transaction: abort it rather than
                    # restore by hand (Transaction.abort's docstring: a plain
                    # restore cannot undo every effect of what was applied).
                    tx.abort()
                    message = f"{type(e).__name__}: {e}; the batch was rolled back, no cell was changed"
                else:
                    # Joined the user's own open transaction (a command or
                    # task panel is active): aborting would discard their
                    # pending edit too, not just this call's, so restore by
                    # hand instead, in reverse order, best-effort.
                    for address in reversed(updated):
                        original_content, original_alias = originals[address]
                        try:
                            sheet.set(address, original_content)
                        except Exception as rollback_exc:
                            rollback_problems.append(f"{address}: {rollback_exc}")
                        try:
                            sheet.setAlias(address, original_alias)
                        except Exception as rollback_exc:
                            rollback_problems.append(f"{address}: {rollback_exc}")
                    if rollback_problems:
                        message = (
                            f"{type(e).__name__}: {e}; the batch was rolled back, but the restore could not "
                            "fully undo it: " + "; ".join(rollback_problems)
                        )
                    else:
                        message = f"{type(e).__name__}: {e}; the batch was rolled back, no cell was changed"
                failure = fail(
                    FREECAD_ERROR,
                    message,
                    details={"attempted": [address for address, _item in resolved]},
                )
                updated = []
                if recompute:
                    try:
                        doc.recompute()
                    except Exception:
                        pass
            if recompute and updated:
                doc.recompute()
                did_recompute = True

        if failure is not None:
            failure.update(tx.reply_fields())
            return failure

        invalid = invalid_objects_report(doc.Objects, exclude_touched=not did_recompute)
        reply = {
            "success": True,
            "document": doc.Name,
            "sheet": sheet.Name,
            "updated": updated,
            "cells": [_read_cell(sheet, address) for address in updated],
            # No recompute ran (recompute was false, or nothing was actually
            # applied): a bare Touched state then only means "not recomputed
            # yet", not broken (object_validity_error's exclude_touched).
            **invalid,
        }
        if invalid["invalid_objects"]:
            error_cells = _error_cell_addresses(sheet)
            if error_cells:
                reply["error_cells"] = error_cells
        reply.update(tx.reply_fields())
        return reply

    return run_on_gui(task, _TIMEOUT, "update_spreadsheet_cells")

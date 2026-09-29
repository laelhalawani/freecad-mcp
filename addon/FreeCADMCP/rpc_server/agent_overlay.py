"""A banner over FreeCAD's 3D view area while an agent works.

The person at the computer sees that an agent is changing the model and should
not edit meanwhile. The banner never takes control: it is transparent for mouse
events, never takes focus, and the person can always keep working or use Force
release.

What shows it (an activity, with the agent's label, a phrase and a start time):

- A GUI-thread call (gui_dispatch): shown once it has run about a second. A
  timer on the GUI thread cannot fire while a call blocks that thread, so the
  calls that may block long (BLOCKING) draw the banner right before they start,
  forced to the screen, and add that FreeCAD may not respond until they finish.
- A background job of an agent: an execute_code_async job (rpc_server) or a
  headless job of the MCP server, which reports itself through keep_alive every
  few seconds; a job not heard from for STALE_S is dropped.

It hides when the last activity ends. Calls that only read (get_*, list_*,
measure) and set_view change no model and show nothing.

Placement: a child of the MDI area's viewport (Gui/MainWindow.cpp keeps the
document windows in a QMdiArea), raised above the document windows, along the
top edge. The text is drawn with QPainter.drawText, so it is plain text
whatever an agent names itself.

Qt objects are touched on the GUI thread only. begin/end calls of other threads
only change the activity table; the GUI-thread timer picks them up.
"""

import threading
import time
from typing import Any

import FreeCAD
import FreeCADGui
from PySide import QtCore, QtGui, QtWidgets

from rpc_server import request_context, session_lock

# A call that is not known to block long shows the banner after this long.
SHOW_AFTER_S = 1.0
# A job reported by the MCP server that has not been heard from for this long
# (three missed reports) has ended without saying so.
STALE_S = 60.0
POLL_MS = 250
# How long the pre-draw of a blocking call lets the event loop run so the
# banner reaches the screen.
PREDRAW_S = 0.08

WARNING_LINE = "An agent is changing this model; please don't edit until it finishes."
BLOCKING_LINE = "FreeCAD may not respond."
# The banner is a compact two-line bar, translucent enough (alpha of 255) to
# show the model behind it.
PAD_Y = 3
BACKGROUND_ALPHA = 205

PHRASES = {
    "execute_code": "Running a script",
    "async_commit": "Applying the result of a background script",
    "recompute_document": "Recomputing the document",
    "run_fem_analysis": "Running a FEM analysis",
    "import_file": "Importing a file",
    "export_document": "Exporting files",
    "repair_mesh": "Repairing a mesh",
    "mesh_to_solid": "Converting a mesh to a solid",
    "solid_to_mesh": "Converting a solid to a mesh",
    "analyze_mesh": "Analyzing a mesh",
    "check_printability": "Checking the print layout",
    "open_document": "Opening a document",
    "reload_document": "Reloading a document",
    "save_document": "Saving the document",
    "save_document_as": "Saving the document",
    "close_document": "Closing a document",
    "create_document": "Creating a document",
    "create_object": "Creating an object",
    "edit_object": "Changing an object",
    "delete_object": "Deleting an object",
    "insert_part_from_library": "Inserting a part",
    "update_spreadsheet_cells": "Changing spreadsheet cells",
}
# Calls that may block the GUI thread for long: the banner is drawn before they
# start.
BLOCKING = frozenset(
    {
        "execute_code",
        "recompute_document",
        "run_fem_analysis",
        "import_file",
        "export_document",
        "repair_mesh",
        "mesh_to_solid",
        "solid_to_mesh",
        "analyze_mesh",
        "check_printability",
        "open_document",
        "reload_document",
    }
)
# Calls that change no model: they never show the banner.
_SILENT_PREFIXES = ("get_", "list_")
_SILENT = frozenset({"measure", "set_view", "activate_document"})

_lock = threading.Lock()
_activities: "dict[str, dict[str, Any]]" = {}
_seq = 0

_banner: "_Banner | None" = None
_timer: "QtCore.QTimer | None" = None
_filter: "Any" = None
_parent: "QtWidgets.QWidget | None" = None
_quit_connected = False
_warned = False
last_error = ""  # the last failure of the banner itself, for diagnosis


def caller_label() -> str:
    """The label of the agent whose request the calling (RPC) thread serves."""
    try:
        label = request_context.get().client
    except Exception:
        label = ""
    if not label:
        try:
            snap = session_lock.snapshot()
            if snap.get("enabled") and snap.get("held"):
                label = snap.get("holder") or ""
        except Exception:
            label = ""
    return label or "An agent"


def is_silent(operation: str) -> bool:
    return operation in _SILENT or operation.startswith(_SILENT_PREFIXES)


def _add(key: str, label: str, phrase: str, started: float, visible_at: float, blocking: bool, remote: bool) -> None:
    global _seq
    with _lock:
        _seq += 1
        _activities[key] = {
            "label": label,
            "phrase": phrase,
            "started": started,
            "visible_at": visible_at,
            "blocking": blocking,
            "remote": remote,
            "seen": time.monotonic(),
            "seq": _seq,
        }


def _remove(key: str) -> None:
    with _lock:
        _activities.pop(key, None)


# --- Activities ---------------------------------------------------------------


def task_begin(task_id: int, operation: str, label: str) -> None:
    """A GUI-thread call starts (GUI thread). A blocking one draws the banner
    at once, before it takes the thread."""
    if is_silent(operation):
        return
    now = time.monotonic()
    blocking = operation in BLOCKING
    _add(
        f"gui:{task_id}",
        label,
        PHRASES.get(operation, "Working"),
        now,
        now if blocking else now + SHOW_AFTER_S,
        blocking,
        False,
    )
    if blocking:
        try:
            ensure()
            _sync(force_paint=True)
        except Exception as e:
            _warn(e)


def task_end(task_id: int) -> None:
    """The GUI-thread call ended (GUI thread)."""
    _remove(f"gui:{task_id}")
    if _banner is not None:
        _sync()


def job_begin(key: str, phrase: str, label: str) -> None:
    """A background job of an agent started (any thread)."""
    now = time.monotonic()
    _add(f"job:{key}", label, phrase, now, now + SHOW_AFTER_S, False, False)
    _request_ensure()


def job_end(key: str) -> None:
    """The background job ended (any thread)."""
    _remove(f"job:{key}")


def job_seen(key: str, phrase: str, label: str, elapsed: float) -> None:
    """The MCP server reports a job of its own is running, and for how long
    (any thread). Repeated reports keep it alive."""
    now = time.monotonic()
    try:
        elapsed = max(0.0, float(elapsed))
    except (TypeError, ValueError):
        elapsed = 0.0
    started = now - elapsed
    with _lock:
        old = _activities.get(f"mcp:{key}")
    if old is None:
        _add(f"mcp:{key}", label or "An agent", phrase or "Running a script", started, max(started + SHOW_AFTER_S, now), False, True)
    else:
        with _lock:
            old.update(label=label or old["label"], phrase=phrase or old["phrase"], started=started, seen=now)
    _request_ensure()


def job_gone(key: str) -> None:
    """The MCP server says its job ended (any thread)."""
    _remove(f"mcp:{key}")


# --- Banner -------------------------------------------------------------------


def _visible_now() -> "list[dict[str, Any]]":
    now = time.monotonic()
    with _lock:
        for key in [k for k, a in _activities.items() if a["remote"] and now - a["seen"] > STALE_S]:
            del _activities[key]
        return sorted((a for a in _activities.values() if a["visible_at"] <= now), key=lambda a: a["seq"])


def _lines(active: "list[dict[str, Any]]") -> "list[str]":
    now = time.monotonic()
    first = active[0]
    head = f"{first['label']}: {first['phrase']} ({int(max(0.0, now - first['started']))} s)"
    if len(active) > 1:
        head += f" and {len(active) - 1} more"
    # The note that matters most to the person comes first: a narrow view
    # elides the end of the line.
    note = WARNING_LINE
    if any(a["blocking"] for a in active):
        note = BLOCKING_LINE + " " + note
    return [head, note]


def _sampled_chrome_color() -> "QtGui.QColor | None":
    """The rendered background colour of FreeCAD's main window chrome, or None."""
    try:
        window = FreeCADGui.getMainWindow()
        # findChild, never menuBar()/statusBar(): those create the bar when it
        # is missing.
        for kind in (QtWidgets.QMenuBar, QtWidgets.QStatusBar):
            widget = window.findChild(kind)
            if widget is None or widget.width() < 8 or widget.height() < 2:
                continue
            image = widget.grab(QtCore.QRect(widget.width() - 4, widget.height() // 2, 1, 1)).toImage()
            if image.isNull():
                continue
            color = QtGui.QColor(image.pixelColor(0, 0))
            if color.alpha() > 0:  # a transparent pixel says nothing about the theme
                return color
    except Exception:
        pass
    return None


class _Banner(QtWidgets.QWidget):
    """The banner: plain text on a semi-transparent panel, never active."""

    def __init__(self, parent: QtWidgets.QWidget):
        super().__init__(parent)
        self._lines: "list[str]" = []
        self._background = QtGui.QColor(45, 45, 48, BACKGROUND_ALPHA)
        self._foreground = QtGui.QColor(240, 240, 240)
        self.setAttribute(QtCore.Qt.WA_TransparentForMouseEvents, True)
        self.setAttribute(QtCore.Qt.WA_ShowWithoutActivating, True)
        self.setFocusPolicy(QtCore.Qt.NoFocus)
        self.setObjectName("mcpAgentBanner")

    def set_lines(self, lines: "list[str]") -> bool:
        if lines == self._lines:
            return False
        self._lines = lines
        return True

    def place(self) -> None:
        parent = self.parentWidget()
        if parent is None:
            return
        metrics = QtGui.QFontMetrics(self.font())
        height = 2 * PAD_Y + len(self._lines) * metrics.height()
        self.setGeometry(0, 0, parent.width(), height)

    def refresh_colors(self) -> None:
        """Follow the theme FreeCAD shows now. A FreeCAD style sheet colours
        its widgets without changing their palettes, so the palette says little
        about what is on screen: the colour of the main window's own chrome (the
        empty end of the menu bar, else of the status bar) is read as rendered,
        and the text is dark or light by that colour's luminance, whatever
        the palette holds."""
        color = _sampled_chrome_color()
        if color is None:
            color = QtGui.QColor(self.palette().color(QtGui.QPalette.Window))
        luminance = 0.299 * color.red() + 0.587 * color.green() + 0.114 * color.blue()
        self._background = QtGui.QColor(color.red(), color.green(), color.blue(), BACKGROUND_ALPHA)
        self._foreground = QtGui.QColor(20, 20, 20) if luminance >= 128 else QtGui.QColor(240, 240, 240)

    def paintEvent(self, event) -> None:  # noqa: N802 (Qt name)
        painter = QtGui.QPainter(self)
        try:
            painter.fillRect(self.rect(), self._background)
            accent = QtGui.QColor(240, 160, 32)
            painter.fillRect(QtCore.QRect(0, 0, 4, self.height()), accent)
            painter.fillRect(QtCore.QRect(0, self.height() - 1, self.width(), 1), accent)
            painter.setPen(self._foreground)
            y = PAD_Y
            for index, text in enumerate(self._lines):
                font = QtGui.QFont(self.font())
                font.setBold(index == 0)
                painter.setFont(font)
                metrics = painter.fontMetrics()
                line = metrics.height()
                room = self.width() - 20
                painter.drawText(
                    QtCore.QRect(12, y, room, line),
                    QtCore.Qt.AlignVCenter | QtCore.Qt.AlignLeft,
                    metrics.elidedText(text, QtCore.Qt.ElideRight, room),
                )
                y += line
        finally:
            painter.end()


class _ResizeFilter(QtCore.QObject):
    """Keeps the banner on the top edge when the MDI area resizes."""

    def eventFilter(self, obj, event):  # noqa: N802 (Qt name)
        try:
            if event.type() == QtCore.QEvent.Resize and _banner is not None and _banner.isVisible():
                _banner.place()
        except Exception:
            pass
        return False


def _find_parent() -> "QtWidgets.QWidget | None":
    window = FreeCADGui.getMainWindow()
    if window is None:
        return None
    area = window.findChild(QtWidgets.QMdiArea)
    if area is not None:
        return area.viewport()
    return window.centralWidget()


def _start_timer(banner: "QtWidgets.QWidget") -> None:
    """(Re)create the poll timer as a child of ``banner``."""
    global _timer
    if _timer is not None:
        # Replacing a stopped timer: delete the old one, or it lingers as a
        # child of the banner.
        try:
            _timer.stop()
            _timer.deleteLater()
        except RuntimeError:
            pass
    _timer = QtCore.QTimer(banner)
    _timer.setInterval(POLL_MS)
    _timer.timeout.connect(_poll)
    _timer.start()


def ensure() -> None:
    """Create the banner and its poll timer, or repair them (idempotent). GUI
    thread only. Called at install, before a blocking call, and, through
    ``_request_ensure``, when a background job reports in: the MDI area may not
    have existed at install, and the main window may have been rebuilt since."""
    global _banner, _timer, _filter, _parent, _quit_connected
    if _banner is not None:
        try:
            _banner.objectName()
            # The timer is a child of the banner; make sure it still runs.
            if _timer is None or not _timer.isActive():
                _start_timer(_banner)
            return
        except RuntimeError:
            # A main window rebuilt under us (the parent got deleted, and the
            # banner's children, the timer among them, with it): start over.
            _banner = None
            _timer = None
    try:
        parent = _find_parent()
        if parent is None:
            return
        banner = _Banner(parent)
        banner.hide()
        _filter = _ResizeFilter(banner)
        parent.installEventFilter(_filter)
        _parent = parent
        _banner = banner
        _start_timer(banner)
        if not _quit_connected:
            app = QtWidgets.QApplication.instance()
            if app is not None:
                app.aboutToQuit.connect(remove)
                _quit_connected = True
    except Exception as e:
        _warn(e)


def _warn(error: Exception) -> None:
    global _warned, last_error
    last_error = f"{type(error).__name__}: {error}"
    if not _warned:
        _warned = True
        FreeCAD.Console.PrintWarning(f"MCP: the agent banner failed: {type(error).__name__}: {error}\n")


class _Poster(QtCore.QObject):
    """Carries an ensure request from any thread to the GUI thread (a queued
    signal): job activity arrives on other threads, and Qt objects are created
    on the GUI thread only."""

    request = QtCore.Signal()

    @QtCore.Slot()
    def run(self) -> None:
        """Runs on this object's (the GUI) thread, whichever thread emitted."""
        ensure()


_poster: "_Poster | None" = None


def _request_ensure() -> None:
    """Ask the GUI thread to create or repair the banner, when it is missing
    (any thread)."""
    poster = _poster
    if poster is None:
        return
    try:
        if _banner is None or _timer is None:
            poster.request.emit()
    except Exception:
        pass


def _poll() -> None:
    try:
        if _banner is None:
            ensure()
        _sync()
    except Exception as e:
        _warn(e)


def _sync(force_paint: bool = False) -> None:
    """Show, update or hide the banner from the activity table. GUI thread."""
    banner = _banner
    if banner is None:
        return
    try:
        active = _visible_now()
        if not active:
            if banner.isVisible():
                banner.hide()
            return
        changed = banner.set_lines(_lines(active))
        banner.place()
        if not banner.isVisible():
            banner.refresh_colors()
            banner.show()
            changed = True
        banner.raise_()
        if force_paint:
            # The window is composed and presented when the event loop handles
            # its update request, which would only happen after the call ends.
            # Run the loop once, briefly and without user input, so the banner
            # is on screen before the GUI thread blocks. gui_dispatch does not
            # start a second task from here: it is already running one.
            # An update request is handled after a short frame timer, so the
            # loop runs for a moment (at most PREDRAW_S), not just once.
            banner.repaint()
            flags = QtCore.QEventLoop.ExcludeUserInputEvents | QtCore.QEventLoop.ExcludeSocketNotifiers
            end = time.monotonic() + PREDRAW_S
            while time.monotonic() < end:
                QtWidgets.QApplication.processEvents(flags, 20)
                time.sleep(0.005)
        elif changed:
            banner.update()
    except RuntimeError:
        # The banner's C++ object is gone (main window closed).
        return


def install() -> None:
    """Create the banner (idempotent), with the RPC server. GUI thread."""
    global _poster
    if _poster is None:
        _poster = _Poster()
        _poster.request.connect(_poster.run, QtCore.Qt.QueuedConnection)
    ensure()


def remove() -> None:
    """Remove the banner and forget every activity (the RPC server stops or
    FreeCAD quits). GUI thread."""
    global _banner, _timer, _filter, _parent
    with _lock:
        _activities.clear()
    try:
        if _timer is not None:
            _timer.stop()
        if _parent is not None and _filter is not None:
            _parent.removeEventFilter(_filter)
        if _banner is not None:
            _banner.hide()
            _banner.deleteLater()
    except Exception:
        pass
    _banner = _timer = _filter = _parent = None

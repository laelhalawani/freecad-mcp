"""What the user sees: static poses, orbit and tour, for the set_view tool.

Everything here runs on FreeCAD's GUI thread. An animated mode is one
``QTimer`` per 3D view (the engine) that writes the camera node directly with
pivy; a camera field write redraws the view by itself (verified on FreeCAD
1.1.3), so no explicit redraw is needed. FreeCAD's own animation calls are not
used: ``startAnimating`` has no state and get_view kills it, ``viewPosition``
blocks, and ``fitAll`` runs a nested event loop that would run the ticks
re-entrantly.

Interrupt rule: each tick compares the live camera with the pose this engine
wrote last. Any difference (a mouse drag, the wheel, the NaviCube, another
tool) stops the mode. A mode also stops on ``stop_mode``, ``reset``, when its
document closes or its view is destroyed (the view is looked up again by
document name on every tick), and when the RPC server stops.

get_view pauses the engine around its capture (``pause`` and ``resume``): the
capture changes the camera and restores it, which would otherwise read as a
foreign change.
"""

import math
import time
from typing import Any

import FreeCAD
import FreeCADGui

from rpc_server.agent_log import agent_warning
from rpc_server.errors import INVALID_INPUT, NOT_FOUND, fail, tool_call

# Timer interval and the longest step of progress one tick may take, so a stall
# of the GUI thread (a recompute, an import) does not make a mode lurch.
TICK_MS = 33
MAX_DT = 0.25

# Default speed of an orbit in degrees per second, and the default seconds a
# tour takes to travel between two stops and to stay at one.
DEFAULT_ORBIT_SPEED = 15.0
DEFAULT_MOVE_SECONDS = 2.0
DEFAULT_DWELL_SECONDS = 2.0

# fit_pose leaves this much room around the framed objects: 5 percent of the
# view's half height.
FIT_MARGIN = 1.05

# How far a camera value may differ from what was written before the camera
# counts as moved by someone else.
_POSE_EPSILON = 1e-4


def import_coin() -> Any:
    """pivy.coin, imported. A camera node is only usable once pivy.coin has
    been imported in this process ("No SWIG wrapped library loaded" otherwise),
    so every function that touches a camera node calls this first."""
    from pivy import coin

    return coin


_coin = import_coin


def camera_node(view: Any) -> Any:
    """The camera node of ``view``, with pivy.coin imported first."""
    import_coin()
    return view.getCameraNode()


# --- Poses --------------------------------------------------------------------


class Pose:
    """A camera as focus point, view direction, distance and zoom.

    ``zoom`` is the orthographic height, or the focal distance for a
    perspective camera (whose height angle stays as it is).
    """

    def __init__(self, focus, quat, distance, zoom, ortho):
        self.focus = tuple(focus)
        self.quat = tuple(quat)
        self.distance = float(distance)
        self.zoom = float(zoom)
        self.ortho = bool(ortho)


def _is_ortho(cam: Any) -> bool:
    coin = _coin()
    return not cam.isOfType(coin.SoPerspectiveCamera.getClassTypeId())


def _direction(quat: tuple) -> tuple:
    coin = _coin()
    return tuple(coin.SbRotation(*quat).multVec(coin.SbVec3f(0, 0, -1)).getValue())


def read_pose(view: Any) -> Pose:
    """The live camera of ``view`` as a Pose."""
    cam = camera_node(view)
    quat = cam.orientation.getValue().getValue()
    pos = cam.position.getValue().getValue()
    distance = cam.focalDistance.getValue()
    d = _direction(quat)
    focus = tuple(pos[i] + distance * d[i] for i in range(3))
    ortho = _is_ortho(cam)
    zoom = cam.height.getValue() if ortho else distance
    return Pose(focus, quat, distance, zoom, ortho)


def _signature(cam: Any) -> tuple:
    """Every camera value a mode writes, to tell that someone else moved it."""
    ortho = _is_ortho(cam)
    return (
        tuple(cam.position.getValue().getValue())
        + tuple(cam.orientation.getValue().getValue())
        + (cam.focalDistance.getValue(), cam.height.getValue() if ortho else 0.0)
    )


def _same_signature(a: tuple, b: tuple) -> bool:
    return all(abs(x - y) <= _POSE_EPSILON * max(1.0, abs(x), abs(y)) for x, y in zip(a, b))


def apply_pose(view: Any, pose: Pose) -> tuple:
    """Write ``pose`` to the live camera and return its signature."""
    coin = _coin()
    cam = camera_node(view)
    d = _direction(pose.quat)
    pos = tuple(pose.focus[i] - pose.distance * d[i] for i in range(3))
    cam.orientation.setValue(coin.SbRotation(*pose.quat))
    cam.position.setValue(coin.SbVec3f(*pos))
    cam.focalDistance.setValue(pose.distance)
    if pose.ortho:
        cam.height.setValue(pose.zoom)
    return _signature(cam)


def _union_box(objects: list) -> Any:
    """The scene box around the view objects of ``objects``, or None."""
    coin = _coin()
    lo = [math.inf] * 3
    hi = [-math.inf] * 3
    for obj in objects:
        view_object = getattr(obj, "ViewObject", None)
        if view_object is None:
            continue
        try:
            bb = view_object.getBoundingBox()
            if not bb.isValid():
                continue
            corners = ((bb.XMin, bb.YMin, bb.ZMin), (bb.XMax, bb.YMax, bb.ZMax))
        except Exception:
            continue
        for i in range(3):
            lo[i] = min(lo[i], corners[0][i])
            hi[i] = max(hi[i], corners[1][i])
    if lo[0] == math.inf:
        return None
    return coin.SbBox3f(*lo, *hi)


def fit_pose(view: Any, objects: list, quat: tuple | None = None, sphere: bool = False) -> Pose | None:
    """The pose that frames ``objects`` from the direction ``quat`` (default:
    the live one), or None when none has a bounding box.

    The fit is to the box itself, not to Coin's bounding sphere around it
    (``viewBoundingBox``), which frames an elongated part loosely: every corner
    of the box is projected on the view, and the camera is placed so the
    projection fills the view with ``FIT_MARGIN`` to spare. Nothing on screen or
    in the selection changes.

    With ``sphere`` the fit is to the sphere around the box (half its diagonal
    as radius, centred on it) instead: the same from every direction about the
    vertical axis, so a turning camera (orbit) never loses part of the model.
    """
    coin = _coin()
    box = _union_box(objects)
    if box is None:
        return None
    live = camera_node(view)
    ortho = _is_ortho(live)
    if quat is None:
        quat = live.orientation.getValue().getValue()
    lo = box.getMin().getValue()
    hi = box.getMax().getValue()
    center = tuple((lo[i] + hi[i]) / 2 for i in range(3))
    rot = coin.SbRotation(*quat)
    right = rot.multVec(coin.SbVec3f(1, 0, 0)).getValue()
    up = rot.multVec(coin.SbVec3f(0, 1, 0)).getValue()
    forward = _direction(quat)
    width, height = _view_size(view)
    aspect = width / height

    if sphere:
        return _sphere_pose(live, center, math.dist(lo, hi) / 2, tuple(quat), aspect, ortho)

    def dot(a: tuple, b: tuple) -> float:
        return a[0] * b[0] + a[1] * b[1] + a[2] * b[2]

    # Each corner: its sideways and upward offset on the view, and how far
    # beyond the box centre it lies along the view direction.
    corners = []
    for x in (lo[0], hi[0]):
        for y in (lo[1], hi[1]):
            for z in (lo[2], hi[2]):
                off = (x - center[0], y - center[1], z - center[2])
                # The half height this corner needs: its own, or its width
                # scaled to the view's aspect.
                need = max(abs(dot(off, up)), abs(dot(off, right)) / aspect)
                corners.append((need, dot(off, forward)))
    reach = max(abs(t) for _, t in corners)
    if ortho:
        zoom = max(2 * max(n for n, _ in corners) * FIT_MARGIN, 1e-3)
        # Far enough back that the whole box is in front of the camera.
        distance = max(live.focalDistance.getValue(), 2 * reach + 1.0)
        return Pose(center, tuple(quat), distance, zoom, True)

    # Perspective: the box's projection is not symmetric about its centre's
    # projection (near corners are larger), so the aim point and the distance
    # are refined together: shift the aim so the projected extents are centred,
    # then scale the distance so they fill the view less FIT_MARGIN.
    tan_half = math.tan(live.heightAngle.getValue() / 2)
    offsets = []
    for x in (lo[0], hi[0]):
        for y in (lo[1], hi[1]):
            for z in (lo[2], hi[2]):
                offsets.append((x, y, z))
    aim = list(center)
    distance = max(need * FIT_MARGIN / tan_half - t for need, t in corners)
    distance = max(distance, reach * 1.01 + 1e-3)
    floor = reach * 1.01 + 1e-3
    for _ in range(12):
        prs, pus = [], []
        for corner in offsets:
            off = (corner[0] - aim[0], corner[1] - aim[1], corner[2] - aim[2])
            depth = distance + dot(off, forward)
            if depth <= 1e-6:
                depth = 1e-6
            prs.append(dot(off, right) / depth)
            pus.append(dot(off, up) / depth)
        mid_r = (max(prs) + min(prs)) / 2
        mid_u = (max(pus) + min(pus)) / 2
        for i in range(3):
            aim[i] += right[i] * mid_r * distance + up[i] * mid_u * distance
        half_w = (max(prs) - min(prs)) / 2
        half_h = (max(pus) - min(pus)) / 2
        ratio = max(half_h / tan_half, half_w / (tan_half * aspect)) * FIT_MARGIN
        distance = max(distance * ratio, floor)
    return Pose(tuple(aim), tuple(quat), distance, distance, ortho)


def _sphere_pose(live: Any, center: tuple, radius: float, quat: tuple, aspect: float, ortho: bool) -> Pose:
    """The pose that frames the sphere of ``radius`` around ``center`` with
    ``FIT_MARGIN`` to spare, on the limiting side of a view of ``aspect``."""
    if ortho:
        zoom = max(2 * radius * FIT_MARGIN * max(1.0, 1 / aspect), 1e-3)
        # Far enough back that the whole sphere is in front of the camera.
        distance = max(live.focalDistance.getValue(), 2 * radius + 1.0)
        return Pose(center, quat, distance, zoom, True)
    # Perspective: the sphere touches the edge of the smaller field of view
    # when the camera is radius / sin(half angle) from its centre.
    half_v = live.heightAngle.getValue() / 2
    half = min(half_v, math.atan(math.tan(half_v) * aspect))
    distance = max(radius * FIT_MARGIN / math.sin(half), radius * 1.01 + 1e-3)
    return Pose(center, quat, distance, distance, False)


def _view_size(view: Any) -> tuple[float, float]:
    try:
        size = view.getSize()
        return max(1.0, float(size[0])), max(1.0, float(size[1]))
    except Exception:
        return 1024.0, 768.0


def _smoothstep(s: float) -> float:
    s = min(1.0, max(0.0, s))
    return s * s * (3.0 - 2.0 * s)


def blend(a: Pose, b: Pose, s: float, bump: float = 0.0) -> Pose:
    """The pose a fraction ``s`` of the way from ``a`` to ``b``: focus linear,
    orientation by slerp, zoom in log space with ``bump`` extra pull-back in
    the middle (a zoom out, a travel, a zoom in). ``s`` is already eased."""
    coin = _coin()
    focus = tuple(a.focus[i] + (b.focus[i] - a.focus[i]) * s for i in range(3))
    quat = coin.SbRotation.slerp(coin.SbRotation(*a.quat), coin.SbRotation(*b.quat), s).getValue()
    pull = 1.0 + bump * math.sin(math.pi * s)
    zoom = math.exp(math.log(a.zoom) + (math.log(b.zoom) - math.log(a.zoom)) * s) * pull
    distance = a.distance + (b.distance - a.distance) * s
    if not a.ortho:
        distance = zoom
    return Pose(focus, quat, distance, zoom, a.ortho)


# --- Engine -------------------------------------------------------------------

_engines: dict[str, "_Engine"] = {}
_last_stop: dict[str, str] = {}


def _main_window() -> Any:
    return FreeCADGui.getMainWindow()


def _document_view(doc_name: str, bound: Any = None) -> Any:
    """The 3D view ``bound`` of the document called ``doc_name`` while it is
    still one of the document's views; without ``bound``, its capture view.
    None when there is no such view."""
    from rpc_server.view_manager import _VIEW3D_TYPE, _document_capture_view

    try:
        gui_doc = FreeCADGui.getDocument(doc_name)
        if bound is None:
            return _document_capture_view(gui_doc)
        for candidate in gui_doc.mdiViewsOfType(_VIEW3D_TYPE):
            if candidate == bound:
                return candidate
        return None
    except Exception:
        return None


def _mouse_pressed() -> bool:
    try:
        from PySide import QtCore, QtWidgets

        return QtWidgets.QApplication.mouseButtons() != QtCore.Qt.NoButton
    except Exception:
        return False


class _Engine:
    """One running mode of one view."""

    kind = ""

    def __init__(self, doc_name: str):
        from PySide import QtCore

        self.doc_name = doc_name
        self.view: Any = None  # the view the mode started on; other views of the document are ignored
        self.last: tuple | None = None
        self.clock = time.monotonic()
        self.paused = False
        self.timer = QtCore.QTimer(_main_window())
        self.timer.setInterval(TICK_MS)
        self.timer.timeout.connect(self._tick)

    def start(self, view: Any) -> None:
        self.view = view
        self.last = _signature(camera_node(view))
        self.clock = time.monotonic()
        _engines[self.doc_name] = self
        _last_stop.pop(self.doc_name, None)
        self.timer.start()

    def _tick(self) -> None:
        try:
            if _engines.get(self.doc_name) is not self:
                self.timer.stop()
                return
            view = _document_view(self.doc_name, self.view)
            if view is None:
                stop_mode(self.doc_name, "document_closed")
                return
            if _mouse_pressed():
                return
            cam = camera_node(view)
            if self.last is not None and not _same_signature(self.last, _signature(cam)):
                stop_mode(self.doc_name, "user")
                return
            now = time.monotonic()
            dt = min(now - self.clock, MAX_DT)
            self.clock = now
            self.step(view, dt)
        except Exception as e:
            agent_warning(f"MCP RPC: {self.kind} stopped: {type(e).__name__}: {e}\n")
            stop_mode(self.doc_name, "error")

    def step(self, view: Any, dt: float) -> None:
        raise NotImplementedError

    def describe(self) -> dict[str, Any]:
        return {"mode": self.kind}


class _Orbit(_Engine):
    kind = "orbit"

    def __init__(self, doc_name: str, degrees_per_second: float):
        super().__init__(doc_name)
        self.rate = math.radians(degrees_per_second)

    def step(self, view: Any, dt: float) -> None:
        # The rotation is about the world Z axis through the live focus point,
        # the same maths as FreeCAD's own spin (a pre-multiplied rotation).
        coin = _coin()
        pose = read_pose(view)
        turn = coin.SbRotation(coin.SbVec3f(0, 0, 1), self.rate * dt)
        pose.quat = (turn * coin.SbRotation(*pose.quat)).getValue()
        self.last = apply_pose(view, pose)

    def describe(self) -> dict[str, Any]:
        return {"mode": "orbit", "degrees_per_second": round(math.degrees(self.rate), 3)}


class _Tour(_Engine):
    kind = "tour"

    def __init__(self, doc_name: str, stops: list[dict], move_seconds: float, loop: bool):
        super().__init__(doc_name)
        self.stops = stops
        self.move = max(0.1, move_seconds)
        self.loop = loop
        self.index = 0
        self.phase = "begin"  # begin, travel, dwell
        self.phase_time = 0.0
        self.origin: Pose | None = None
        self.target: Pose | None = None
        self.bump = 0.0
        self.skipped = 0

    def _resolve(self, view: Any, stop: dict) -> Pose | None:
        """The pose that frames a stop, from the objects as they are now; None
        when every named object is gone."""
        doc = FreeCAD.getDocument(self.doc_name)
        names = stop["focus"]
        if names == ["all"]:
            objects = drawn_objects(doc)
        else:
            objects = [o for o in (doc.getObject(n) for n in names) if o is not None]
        quat = stop.get("quat")
        return fit_pose(view, objects, quat)

    def step(self, view: Any, dt: float) -> None:
        for _ in range(len(self.stops) + 1):
            if self.phase == "begin":
                if self.index >= len(self.stops):
                    if self.loop and self.skipped < len(self.stops):
                        self.index, self.skipped = 0, 0
                    else:
                        stop_mode(self.doc_name, "finished")
                        return
                stop = self.stops[self.index]
                target = self._resolve(view, stop)
                if target is None:
                    self.index += 1
                    self.skipped += 1
                    continue
                self.skipped = 0
                self.origin = read_pose(view)
                self.target = target
                travel = math.dist(self.origin.focus, target.focus)
                self.bump = min(2.0, 0.5 * travel / max(self.origin.zoom, target.zoom, 1e-6))
                self.phase, self.phase_time = "travel", 0.0
            break
        else:
            stop_mode(self.doc_name, "finished")
            return
        self.phase_time += dt
        if self.phase == "travel":
            s = self.phase_time / self.move
            self.last = apply_pose(view, blend(self.origin, self.target, _smoothstep(s), self.bump))
            if s >= 1.0:
                self.phase, self.phase_time = "dwell", 0.0
        elif self.phase == "dwell":
            if self.phase_time >= self.stops[self.index]["dwell"]:
                self.index += 1
                self.phase = "begin"

    def describe(self) -> dict[str, Any]:
        return {"mode": "tour", "stops": len(self.stops), "loop": self.loop}


def _hidden_by_group(obj: Any) -> bool:
    """Whether a group (a Group, Part or Body) that holds ``obj``, at any
    depth, is hidden: FreeCAD does not draw the objects of a hidden group."""
    seen = 0
    parent = obj
    while seen < 64:
        seen += 1
        try:
            parent = parent.getParentGroup() or parent.getParentGeoFeatureGroup()
        except Exception:
            return False
        if parent is None:
            return False
        view_object = getattr(parent, "ViewObject", None)
        try:
            if view_object is not None and not view_object.Visibility:
                return True
        except Exception:
            pass
    return False


# The axes, planes and point of an Origin report Visibility true whether or not
# the origin is shown, and their boxes are much larger than a part's: they never
# count when framing.
_ORIGIN_TYPES = frozenset({"App::Origin", "App::Line", "App::Plane", "App::Point"})


def drawn_objects(doc: Any) -> list:
    """The objects of ``doc`` that are drawn and have a scene box: visible, in
    no hidden group, and with a valid ``ViewObject.getBoundingBox()``. Unlike
    ``visible_shape_objects`` this includes objects without a Shape attribute
    (an App::Part, an App::Link), the same test a focus is checked with."""
    out = []
    for obj in doc.Objects:
        view_object = getattr(obj, "ViewObject", None)
        if view_object is None or obj.TypeId in _ORIGIN_TYPES:
            continue
        try:
            if view_object.Visibility and not _hidden_by_group(obj) and _union_box([obj]) is not None:
                out.append(obj)
        except Exception:
            continue
    return out


def visible_shape_objects(doc: Any) -> list:
    """The objects of ``doc`` that have a shape and are drawn: visible, and in
    no hidden group. Only isolate uses it (it hides what else is shown, and must
    not hide a group that holds the isolated object); framing uses
    ``drawn_objects``."""
    out = []
    for obj in doc.Objects:
        view_object = getattr(obj, "ViewObject", None)
        if view_object is None or not hasattr(obj, "Shape"):
            continue
        try:
            if view_object.Visibility and not obj.Shape.isNull() and not _hidden_by_group(obj):
                out.append(obj)
        except Exception:
            continue
    return out


def stop_mode(doc_name: str, reason: str = "stopped") -> bool:
    """Stop the mode running on ``doc_name``'s view. True when one ran."""
    engine = _engines.pop(doc_name, None)
    if engine is None:
        return False
    try:
        engine.timer.stop()
        engine.timer.deleteLater()
    except Exception:
        pass
    _last_stop[doc_name] = reason
    return True


def stop_all(reason: str = "stopped") -> None:
    """Stop every mode (the RPC server is stopping)."""
    for name in list(_engines):
        stop_mode(name, reason)


def remove() -> None:
    """The RPC server is stopping: no timer may outlive it."""
    stop_all("server_stopped")


def is_running(doc_name: str) -> bool:
    return doc_name in _engines


def pause(doc_name: str) -> Any:
    """Stop the timer of ``doc_name``'s mode without ending it; hand the result
    to ``resume``. None when no mode runs there."""
    engine = _engines.get(doc_name)
    if engine is None:
        return None
    engine.timer.stop()
    engine.paused = True
    return engine


def resume(token: Any) -> None:
    """Restart a mode paused by ``pause``, from the camera as it is now (the
    caller has restored it), unless the mode ended meanwhile."""
    if token is None or _engines.get(token.doc_name) is not token:
        return
    view = _document_view(token.doc_name, token.view)
    if view is None:
        stop_mode(token.doc_name, "document_closed")
        return
    token.last = _signature(camera_node(view))
    token.clock = time.monotonic()
    token.paused = False
    token.timer.start()


def status(doc_name: str) -> dict[str, Any]:
    """``{"running", "mode"?, "stopped"?}`` for ``doc_name``."""
    engine = _engines.get(doc_name)
    if engine is not None:
        return {"running": True, **engine.describe()}
    reason = _last_stop.get(doc_name)
    return {"running": False, **({"stopped": reason} if reason else {})}


def start_orbit(view: Any, doc_name: str, degrees_per_second: float) -> None:
    stop_mode(doc_name, "replaced")
    _Orbit(doc_name, degrees_per_second).start(view)


def start_tour(view: Any, doc_name: str, stops: list[dict], move_seconds: float, loop: bool) -> None:
    stop_mode(doc_name, "replaced")
    _Tour(doc_name, stops, move_seconds, loop).start(view)


# --- Visibility, transparency and display mode --------------------------------

# What earlier set_view calls changed, per document and object, with the value
# each had before the first change, so reset can put it back.
_changed: dict[str, dict[str, dict[str, Any]]] = {}


def _remember(doc_name: str, obj: Any, key: str, value: Any) -> None:
    saved = _changed.setdefault(doc_name, {}).setdefault(obj.Name, {})
    saved.setdefault(key, value)


def _objects_named(doc: Any, names: list[str]) -> tuple[list, dict | None]:
    """The objects called ``names``, or a fail() reply naming the first unknown."""
    objects = []
    for name in names:
        obj = doc.getObject(name)
        if obj is None or getattr(obj, "ViewObject", None) is None:
            return [], fail(
                NOT_FOUND,
                f"Object '{name}' has no view in document '{doc.Name}'.",
                "Call " + tool_call("list_objects", {"doc_name": doc.Name, "compact": True})
                + " to see the object names.",
            )
        objects.append(obj)
    return objects, None


def clear_stop(doc_name: str) -> None:
    """Forget why the last mode of ``doc_name`` stopped: a new set_view call
    must not report an old reason."""
    _last_stop.pop(doc_name, None)


def check_visual_changes(
    doc: Any,
    show: list[str],
    hide: list[str],
    isolate: list[str],
    transparency: dict[str, float],
    display_mode: dict[str, str],
) -> tuple[dict[str, Any], dict | None]:
    """Check every name and value of a visual change without changing anything.
    Returns ``({name: object}, None)``, or ``({}, fail() reply)``."""
    named = list(show) + list(hide) + list(isolate) + list(transparency) + list(display_mode)
    by_name: dict[str, Any] = {}
    objects, error = _objects_named(doc, sorted(set(named)))
    if error is not None:
        return {}, error
    for obj in objects:
        by_name[obj.Name] = obj
    for name, value in transparency.items():
        if not isinstance(value, (int, float)) or isinstance(value, bool) or not 0 <= value <= 100:
            return {}, fail(
                INVALID_INPUT,
                f"transparency of '{name}' must be a number from 0 to 100, not {value!r}",
            )
    for name, mode in display_mode.items():
        allowed = list(by_name[name].ViewObject.listDisplayModes())
        if mode not in allowed:
            return {}, fail(
                INVALID_INPUT,
                f"display mode {mode!r} is not one '{name}' offers; allowed: {', '.join(allowed)}",
            )
    return by_name, None


def apply_visual_changes(
    doc: Any,
    by_name: dict[str, Any],
    show: list[str],
    hide: list[str],
    isolate: list[str],
    transparency: dict[str, float],
    display_mode: dict[str, str],
) -> dict[str, Any]:
    """Change visibility, transparency and display modes as
    ``check_visual_changes`` approved (``by_name`` is its result), recording the
    old values. Returns ``{"success", "shown", "hidden", "transparency",
    "display_mode"}``."""
    shown: list[str] = []
    hidden: list[str] = []
    shown_names = set(show) | set(isolate)
    hide_names = set(hide)
    if isolate:
        for obj in visible_shape_objects(doc):
            if obj.Name not in shown_names:
                hide_names.add(obj.Name)
    for name in sorted(shown_names):
        obj = by_name[name]
        _remember(doc.Name, obj, "visibility", bool(obj.ViewObject.Visibility))
        obj.ViewObject.Visibility = True
        shown.append(name)
    for name in sorted(hide_names - shown_names):
        obj = doc.getObject(name)
        if obj is None or getattr(obj, "ViewObject", None) is None:
            continue
        _remember(doc.Name, obj, "visibility", bool(obj.ViewObject.Visibility))
        obj.ViewObject.Visibility = False
        hidden.append(name)
    for name, value in transparency.items():
        vo = by_name[name].ViewObject
        _remember(doc.Name, by_name[name], "transparency", int(vo.Transparency))
        vo.Transparency = int(round(value))
    for name, mode in display_mode.items():
        vo = by_name[name].ViewObject
        _remember(doc.Name, by_name[name], "display_mode", str(vo.DisplayMode))
        vo.DisplayMode = mode
    return {
        "success": True,
        "shown": shown,
        "hidden": hidden,
        "transparency": {k: int(round(v)) for k, v in transparency.items()},
        "display_mode": dict(display_mode),
    }


def reset(doc: Any) -> dict[str, Any]:
    """Stop the mode and restore what earlier set_view calls changed."""
    stopped = stop_mode(doc.Name, "reset")
    restored: list[str] = []
    for name, saved in _changed.pop(doc.Name, {}).items():
        obj = doc.getObject(name)
        vo = getattr(obj, "ViewObject", None) if obj is not None else None
        if vo is None:
            continue
        try:
            if "visibility" in saved:
                vo.Visibility = saved["visibility"]
            if "transparency" in saved:
                vo.Transparency = saved["transparency"]
            if "display_mode" in saved:
                vo.DisplayMode = saved["display_mode"]
            restored.append(name)
        except Exception:
            continue
    return {"restored": restored, "stopped_mode": stopped}

"""Shape tessellation with explicit, deterministic quality settings.

Used by export_document (mesh formats and glTF), check_printability and
solid_to_mesh. ``MeshPart.meshFromShape`` cleans the triangulation of the shape
it meshes (Mod/MeshPart/App/Mesher.cpp:227-232), so it always gets a copy of
the object's shape, and its arguments are passed as keywords only: the
function tries several signatures and a positional call can match the wrong one
(Mod/MeshPart/App/AppMeshPartPy.cpp:472-521).

GUI thread only. Part and MeshPart are imported inside the functions that need them.
"""

import math
from typing import Any

from rpc_server.errors import INVALID_INPUT, fail


# (linear deflection in mm, angular deflection in degrees)
PRESETS: dict[str, tuple[float, float]] = {
    "coarse": (0.1, 20.0),  # quick previews
    "standard": (0.02, 8.0),  # normal FDM prints
    "fine": (0.005, 3.0),  # resin, small curved parts
}
DEFAULT_QUALITY = "standard"

LINEAR_RANGE = (0.001, 100.0)  # mm
ANGULAR_RANGE = (0.5, 90.0)  # degrees


class Settings:
    """Resolved tessellation settings."""

    def __init__(
        self,
        quality: str,
        linear_deflection: float,
        angular_deflection_deg: float,
        relative: bool,
    ) -> None:
        self.quality = quality
        self.linear_deflection = linear_deflection
        self.angular_deflection_deg = angular_deflection_deg
        self.relative = relative

    @property
    def angular_deflection_rad(self) -> float:
        return math.radians(self.angular_deflection_deg)

    def as_dict(self) -> dict[str, Any]:
        return {
            "quality": self.quality,
            "linear_deflection": self.linear_deflection,
            "angular_deflection_deg": self.angular_deflection_deg,
            "relative": self.relative,
        }


def _number_in(value: Any, name: str, bounds: tuple[float, float]) -> float | dict[str, Any]:
    low, high = bounds
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return fail(INVALID_INPUT, f"{name} must be a number from {low:g} to {high:g}, not {value!r}")
    number = float(value)
    if not math.isfinite(number) or number < low or number > high:
        return fail(INVALID_INPUT, f"{name} must be from {low:g} to {high:g}, not {value!r}")
    return number


def resolve_settings(
    quality: Any = None,
    linear_deflection: Any = None,
    angular_deflection_deg: Any = None,
    relative: Any = None,
) -> Settings | dict[str, Any]:
    """Return the settings for the given options, or a failure reply.

    ``quality`` picks a preset (default ``standard``); an explicit
    ``linear_deflection`` (mm) or ``angular_deflection_deg`` overrides the
    preset's value. ``relative`` makes the linear deflection relative to the
    edge length (default false).
    """
    if quality is None or quality == "":
        quality = DEFAULT_QUALITY
    if quality not in PRESETS:
        return fail(
            INVALID_INPUT,
            f"quality must be one of {', '.join(PRESETS)}, not {quality!r}",
        )
    linear, angular = PRESETS[quality]
    if linear_deflection is not None:
        linear = _number_in(linear_deflection, "linear_deflection", LINEAR_RANGE)
        if isinstance(linear, dict):
            return linear
    if angular_deflection_deg is not None:
        angular = _number_in(angular_deflection_deg, "angular_deflection_deg", ANGULAR_RANGE)
        if isinstance(angular, dict):
            return angular
    if relative is None:
        relative = False
    if not isinstance(relative, bool):
        return fail(INVALID_INPUT, f"relative must be true or false, not {relative!r}")
    return Settings(quality, linear, angular, relative)


def is_mesh_feature(obj: Any) -> bool:
    """True for a Mesh::Feature (or a type derived from it)."""
    try:
        return bool(obj.isDerivedFrom("Mesh::Feature"))
    except Exception:
        return False


def parent_geo_feature_group(obj: Any) -> Any:
    """``obj.getParentGeoFeatureGroup()``, or None when it has none or raises.

    Works for any DocumentObject, including an App::Link, which is not
    itself a GeoFeature (App/DocumentObjectPyImp.cpp:801-818).
    """
    try:
        return obj.getParentGeoFeatureGroup()
    except Exception:
        return None


def container_placement(obj: Any) -> Any:
    """The enclosing App::Part or PartDesign Body's global placement, or None.

    ``obj``'s own Placement is applied wherever its shape or mesh is read
    from (``Part.getShape``, ``obj.Mesh``); this is only the extra transform
    an enclosing container contributes, taken from the parent group's own
    ``getGlobalPlacement()`` (App/GeoFeature.cpp:315-345, a GeoFeaturePy-only
    method that would raise on ``obj`` itself for a Link, but not on its
    parent group, which is always a Body or an App::Part). That call already
    accumulates any further nesting, so a caller applies the result once, on
    top of ``obj``'s own placement, never in place of it: nothing is applied
    twice.
    """
    grp = parent_geo_feature_group(obj)
    if grp is None:
        return None
    try:
        return grp.getGlobalPlacement()
    except Exception:
        return None


def apply_container_placement(shape: Any, obj: Any) -> Any:
    """Return a copy of ``shape`` with ``container_placement(obj)`` applied.

    ``shape`` is assumed to already carry ``obj``'s own Placement (directly,
    or through a subname path that accumulates it and that of every object
    named in it); this applies only the further transform an enclosing
    container contributes, once, on top of that. Shared by every route that
    ends up with a shape positioned as far as ``obj`` itself but no further:
    ``shape_of`` for a whole object or a plain element name picked out of
    it, and a subname path resolved directly with ``Part.getShape``.
    """
    shape = shape.copy()
    container = container_placement(obj)
    if container is not None and not container.isIdentity():
        shape.transformShape(container.toMatrix())
    return shape


def shape_of(obj: Any) -> Any:
    """Return a copy of ``obj``'s shape in global coordinates, or None.

    ``Part.getShape`` resolves Part features, Bodies, Links and App::Part
    containers (Mod/Part/App/AppPartPy.cpp:753), but only applies obj's own
    Placement, not that of an enclosing App::Part or PartDesign Body; see
    ``container_placement``. The copy keeps meshing from stripping the
    triangulation of the document's own shape.
    """
    import Part

    try:
        shape = Part.getShape(obj)
    except Exception:
        return None
    if shape is None or shape.isNull():
        return None
    return apply_container_placement(shape, obj)


def mesh_shape(shape: Any, settings: Settings) -> Any:
    """Tessellate ``shape`` (already a copy) into a Mesh.Mesh."""
    import MeshPart

    return MeshPart.meshFromShape(
        Shape=shape,
        LinearDeflection=settings.linear_deflection,
        AngularDeflection=settings.angular_deflection_rad,
        Relative=settings.relative,
    )


def mesh_object(obj: Any, settings: Settings) -> Any:
    """Return a Mesh.Mesh of ``obj`` in global coordinates, or None.

    A Mesh::Feature is copied as it is, with its global placement (its Mesh
    carries the object's own placement as its transform,
    Mod/Mesh/App/MeshFeature.cpp:61-66). Any other object is tessellated from
    its shape; None means it has no geometry.
    """
    if is_mesh_feature(obj):
        mesh = obj.Mesh.copy()
        try:
            mesh.Placement = obj.getGlobalPlacement()
        except Exception:
            pass
        return mesh
    shape = shape_of(obj)
    if shape is None:
        return None
    return mesh_shape(shape, settings)


def parent_map(doc: Any) -> dict[str, str]:
    """Return child object Name -> parent object Name, as FreeCAD's tree view
    nests them.

    Two ways a parent's tree node claims a child, checked in this order so a
    PartDesign feature's parent is its Body rather than a container the Body
    itself sits in:

    - ``getParentGeoFeatureGroup()`` (a Body's or App::Part's own contents,
      App/DocumentObjectPyImp.cpp:801-814).
    - A ViewProvider's ``claimChildren()`` when the object has one, else its
      ``Group`` (App::Part, DocumentObjectGroup). ``claimChildren()`` on a
      Link to another document's object returns that object's own children,
      in that other document (Gui/ViewProviderLink.cpp claimChildren): a bare
      name would then claim same-named objects of ``doc`` (a repeated "Body"
      or "Box"), so a claimed child is only counted when it belongs to
      ``doc`` itself.

    The single source of both ``tree_root_objects`` (a top-level object is
    one absent from this map) and a compact object list's "parent" field, so
    the two never disagree about what counts as top level.
    """
    parents: dict[str, str] = {}
    for obj in doc.Objects:
        parent_group = parent_geo_feature_group(obj)
        if parent_group is not None:
            try:
                parents[obj.Name] = parent_group.Name
            except Exception:
                pass

    for obj in doc.Objects:
        children = None
        vp = getattr(obj, "ViewObject", None)
        claim_children = getattr(vp, "claimChildren", None) if vp is not None else None
        if callable(claim_children):
            try:
                children = claim_children()
            except Exception:
                children = None
        if children is None:
            children = getattr(obj, "Group", None) or []
        for child in children:
            child_doc = getattr(child, "Document", None)
            if child_doc is None or child_doc.Name != doc.Name:
                continue
            name = getattr(child, "Name", None)
            if name:
                parents.setdefault(name, str(getattr(obj, "Name", "")))

    return parents


def tree_root_objects(doc: Any) -> list:
    """Return doc's top-level objects, as FreeCAD's tree view shows them.

    ``doc.RootObjects`` (dependency-graph roots, App/Document.cpp:3748-3759)
    excludes anything another object references: an App::Link's target, an
    expression dependency, a TechDraw view's source, or the Base/Tool of a
    boolean. Those still get their own row in the tree, so this instead
    excludes only what ``parent_map`` claims as someone else's child.
    """
    claimed = parent_map(doc)
    return [obj for obj in doc.Objects if obj.Name not in claimed]

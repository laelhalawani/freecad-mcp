"""Describe the direction a FEM force or pressure constraint acts in.

A ``Fem::ConstraintForce`` acts along ``DirectionVector``: the outward normal
of its first referenced face, or the direction of its ``Direction`` link when
one is set, flipped when ``Reversed`` is true (Mod/Fem/App/FemConstraintForce.cpp
``onChanged``). A ``Fem::ConstraintPressure`` presses into its faces, and
``Reversed`` pulls outward (femsolver/calculix/write_constraint_pressure.py
writes P with the sign flipped). Both read the face normal FreeCAD keeps in
``NormalDirection``.
"""

import math
from typing import Any

from rpc_server.property_mapper import quantity_text


FORCE = "Fem::ConstraintForce"
PRESSURE = "Fem::ConstraintPressure"


def _vector(vector: Any) -> list[float]:
    # Adding 0.0 turns a negative zero into a plain zero; a reply cannot carry
    # a value that is not finite.
    return [
        (round(c, 4) if math.isfinite(c) else 0.0) + 0.0 for c in (vector.x, vector.y, vector.z)
    ]


def _vector_text(vector: list[float]) -> str:
    return "(" + ", ".join(f"{c:g}" for c in vector) + ")"


def _first_face(obj: Any) -> str | None:
    """The first face named in References, or None."""
    try:
        for _target, subs in obj.References:
            for sub in subs:
                if str(sub).startswith("Face"):
                    return str(sub)
    except Exception:
        pass
    return None


def load_info(obj: Any) -> dict[str, Any] | None:
    """Describe a force or pressure constraint, or None for any other object.

    Reply: ``{"name", "kind" ("force" or "pressure"), "magnitude" (with its
    unit), "direction" ([x, y, z], left out while no face is referenced),
    "reversed", "text"}``; ``text`` is the sentence a reply shows. Meant to run
    after a recompute, so the face normal is current.
    """
    type_id = getattr(obj, "TypeId", "")
    if type_id not in (FORCE, PRESSURE):
        return None
    try:
        reversed_ = bool(obj.Reversed)
        face = _first_face(obj)
        of = f"the outward normal of {face}"
        link = getattr(obj, "Direction", None) if type_id == FORCE else None
        has_link = bool(link and link[0] is not None)
        if type_id == FORCE:
            kind, magnitude = "force", quantity_text(obj.Force)
        else:
            kind, magnitude = "pressure", quantity_text(obj.Pressure)
        if face is None and not has_link:
            # FreeCAD keeps a default vector until a face is referenced; it is
            # not a direction the load acts in.
            return {
                "name": obj.Name,
                "kind": kind,
                "magnitude": magnitude,
                "reversed": reversed_,
                "text": f"{kind.capitalize()} {magnitude}: no face is referenced yet, "
                "so no direction is known yet.",
            }
        if type_id == FORCE:
            direction = _vector(obj.DirectionVector)
            if has_link:
                subs = link[1] if len(link) > 1 else []
                target = f"{link[0].Name}.{subs[0]}" if subs else link[0].Name
                what = f"the direction of {target}"
            else:
                what = of
            how = f"reversed, against {what}" if reversed_ else what
        else:
            normal = _vector(obj.NormalDirection)
            direction = normal if reversed_ else [-c + 0.0 for c in normal]
            how = (
                f"reversed, out of the face, along {of}"
                if reversed_
                else f"into the face, against {of}"
            )
    except Exception:
        return None
    text = f"{kind.capitalize()} {magnitude} acts along {_vector_text(direction)}: {how}."
    return {
        "name": obj.Name,
        "kind": kind,
        "magnitude": magnitude,
        "direction": direction,
        "reversed": reversed_,
        "text": text,
    }


def analysis_loads(analysis: Any) -> list[dict[str, Any]]:
    """Every force and pressure constraint of a FEM analysis, described."""
    loads = []
    for member in getattr(analysis, "Group", []):
        info = load_info(member)
        if info is not None:
            loads.append(info)
    return loads

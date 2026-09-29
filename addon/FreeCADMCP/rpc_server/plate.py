"""The margin maths of the print-layout check: a part's box against a plate.

Pure functions, no FreeCAD: printability.py measures the shapes, this decides
what the numbers mean.
"""

# Two boxes closer than this many mm on every axis count as touching, and a box
# may reach this far past a plate edge before it counts as outside.
TOUCH_TOLERANCE_MM = 1e-6


def plate_margins(
    box: tuple, plate: list[float], origin: list[float]
) -> tuple[dict[str, float], float, list[str]]:
    """The margins of a part's box ``(xmin, ymin, zmin, xmax, ymax, zmax)`` to a
    plate: ``(margins, free margin, problems)``.

    The plate spans x from ``origin[0]`` for ``plate[0]``, y likewise, and z from
    0 up to ``plate[2]`` when the build height is given. The free margin is the
    distance from the nearest side of the box to a plate edge: x and y on both
    sides, and the top when the build height is given. z_low is where the part
    sits on the plate, so it is 0 by design and is not part of it.
    """
    limits = list(plate) + [None] * (3 - len(plate))
    starts = list(origin) + [0.0]  # the plate starts at (origin x, origin y, z = 0)
    margins: dict[str, float] = {}
    problems: list[str] = []
    for i, axis in enumerate("xyz"):
        low = box[i] - starts[i]
        # + 0.0 turns a rounded -0.0 into 0.0, so no margin prints as "-0".
        margins[f"{axis}_low"] = round(low, 4) + 0.0
        if low < -TOUCH_TOLERANCE_MM:
            problems.append(f"extends below {axis} = {starts[i]:g} (the plate edge) by {-low:.4g} mm")
        limit = limits[i]
        if limit is None:
            continue
        high = starts[i] + limit - box[i + 3]
        margins[f"{axis}_high"] = round(high, 4) + 0.0
        if high < -TOUCH_TOLERANCE_MM:
            problems.append(f"extends past {axis} = {starts[i] + limit:g} (the plate edge) by {-high:.4g} mm")
    free = min(v for k, v in margins.items() if k != "z_low")
    return margins, free, problems

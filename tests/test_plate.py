"""The plate margin maths of check_printability (pure, no FreeCAD)."""

import importlib.util
from pathlib import Path

_path = Path(__file__).resolve().parent.parent / "addon" / "FreeCADMCP" / "rpc_server" / "plate.py"
_spec = importlib.util.spec_from_file_location("plate", _path)
plate = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(plate)


def test_part_inside_a_plate_at_the_origin():
    margins, free, problems = plate.plate_margins((5, 5, 0, 25, 25, 10), [220, 220], [0, 0])
    assert problems == []
    assert margins["x_low"] == 5 and margins["x_high"] == 195
    assert free == 5  # the seat (z_low = 0) does not count


def test_second_plate_beside_the_first_uses_its_corner():
    box = (400, 300, 0, 420, 320, 10)
    _, _, outside = plate.plate_margins(box, [220, 220], [0, 0])
    assert len(outside) == 2 and "x = 220" in outside[0]
    margins, free, problems = plate.plate_margins(box, [220, 220], [380, 250])
    assert problems == []
    assert margins["x_low"] == 20 and margins["y_low"] == 50
    assert margins["x_high"] == 180 and margins["y_high"] == 150
    assert free == 20


def test_build_height_counts_only_when_given_and_no_negative_zero():
    margins, free, problems = plate.plate_margins((0, 0, 0, 10, 10, 30), [100, 100, 25], [0, 0])
    assert problems == ["extends past z = 25 (the plate edge) by 5 mm"]
    assert margins["z_high"] == -5 and free == -5
    assert str(plate.plate_margins((0, 0, 0, 10, 10, 10), [100, 100], [0, 0])[0]["x_low"]) == "0.0"

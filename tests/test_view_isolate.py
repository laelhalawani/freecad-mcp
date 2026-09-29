"""Isolating keeps what an object is drawn through, and what it draws."""

import types

from test_view_sphere import view_mode  # noqa: F401  the fixture that loads view_mode


class Obj:
    """An object of a document: its view provider, its group and what it holds."""

    def __init__(self, name: str, type_id: str, visible: bool = True, parent: "Obj | None" = None, shape: bool = True):
        self.Name = name
        self.TypeId = type_id
        self.ViewObject = types.SimpleNamespace(Visibility=visible)
        self.Group: list[Obj] = []
        self._parent = parent
        if parent is not None:
            parent.Group.append(self)
        if shape:
            self.Shape = types.SimpleNamespace(isNull=lambda: False)

    def getParentGroup(self):
        return self._parent

    def getParentGeoFeatureGroup(self):
        return self._parent


def build(nested: bool = False, body_visible: bool = True, part_visible: bool = True) -> types.SimpleNamespace:
    """A Body (an Origin, a visible Sketch, a Pad and a second Pad) next to a loose Box,
    the Body inside an App::Part when ``nested``."""
    part = Obj("Part", "App::Part", part_visible) if nested else None
    body = Obj("Body", "PartDesign::Body", body_visible, part)
    Obj("Origin", "App::Origin", True, body, shape=False)
    sketch = Obj("Sketch", "Sketcher::SketchObject", True, body)
    pad = Obj("Pad", "PartDesign::Pad", True, body)
    pad2 = Obj("Pad2", "PartDesign::Pad", True, body)
    box = Obj("Box", "Part::Box")
    objects = [o for o in (part, body, *body.Group, box) if o is not None]
    doc = types.SimpleNamespace(Name="Doc", Objects=objects, getObject=lambda name: next(o for o in objects if o.Name == name))
    return types.SimpleNamespace(doc=doc, part=part, body=body, sketch=sketch, pad=pad, pad2=pad2, box=box)


def isolate(view_mode, scene, *objects):
    by_name = {o.Name: o for o in objects}
    return view_mode.apply_visual_changes(scene.doc, by_name, [], [], list(by_name), {}, {})


def visible(*objects) -> list[bool]:
    return [o.ViewObject.Visibility for o in objects]


def test_isolated_feature_keeps_its_body_and_hides_the_rest(view_mode) -> None:
    s = build()
    reply = isolate(view_mode, s, s.pad)
    assert reply["shown"] == ["Body", "Pad"]
    assert visible(s.body, s.pad) == [True, True]
    assert visible(s.sketch, s.pad2, s.box) == [False, False, False]
    view_mode._union_box = lambda _objects: object()  # the scene box needs Coin
    assert view_mode.drawn_objects(s.doc) == [s.body, s.pad]


def test_isolated_feature_of_a_hidden_body_shows_the_body_not_its_other_members(view_mode) -> None:
    s = build(body_visible=False)
    reply = isolate(view_mode, s, s.pad)
    assert reply["shown"] == ["Body", "Pad"]
    assert "Sketch" in reply["hidden"] and "Box" not in reply["shown"]
    assert visible(s.body, s.sketch) == [True, False]


def test_isolated_feature_in_a_body_in_a_part_keeps_both_containers(view_mode) -> None:
    s = build(nested=True, part_visible=False, body_visible=False)
    reply = isolate(view_mode, s, s.pad)
    assert reply["shown"] == ["Body", "Pad", "Part"]
    assert visible(s.part, s.body, s.pad) == [True, True, True]
    assert visible(s.sketch, s.pad2, s.box) == [False, False, False]


def test_isolating_the_body_itself_keeps_what_it_holds(view_mode) -> None:
    s = build()
    reply = isolate(view_mode, s, s.body)
    assert reply["shown"] == ["Body"] and reply["hidden"] == ["Box"]
    assert visible(s.body, s.sketch, s.pad, s.pad2, s.box) == [True, True, True, True, False]


def test_isolating_the_part_keeps_the_body_and_its_features(view_mode) -> None:
    s = build(nested=True)
    reply = isolate(view_mode, s, s.part)
    assert reply["shown"] == ["Part"] and reply["hidden"] == ["Box"]
    assert visible(s.part, s.body, s.pad, s.pad2) == [True, True, True, True]


def test_isolating_the_body_inside_a_part_keeps_the_part_and_hides_its_other_members(view_mode) -> None:
    s = build(nested=True)
    other = Obj("Other", "Part::Box", True, s.part)
    s.doc.Objects.append(other)
    reply = isolate(view_mode, s, s.body)
    assert reply["shown"] == ["Body", "Part"] and "Other" in reply["hidden"]
    assert visible(s.part, s.body, s.pad, other, s.box) == [True, True, True, False, False]


def test_two_isolated_features_of_one_body_stay_and_the_rest_goes(view_mode) -> None:
    s = build()
    reply = isolate(view_mode, s, s.pad, s.pad2)
    assert reply["shown"] == ["Body", "Pad", "Pad2"]
    assert visible(s.body, s.pad, s.pad2) == [True, True, True]
    assert visible(s.sketch, s.box) == [False, False]


def test_reset_puts_the_hidden_body_back(view_mode) -> None:
    s = build(body_visible=False)
    isolate(view_mode, s, s.pad)
    view_mode.reset(s.doc)
    assert visible(s.body, s.sketch, s.box) == [False, True, True]


def test_isolating_a_loose_object_changes_no_container(view_mode) -> None:
    s = build()
    reply = isolate(view_mode, s, s.box)
    assert reply["shown"] == ["Box"]
    assert visible(s.body, s.pad) == [False, False]


def focus_visible(view_mode, s, focus, isolated, hide=()):
    kept, below = view_mode.isolation_scope(list(isolated))
    return view_mode.effective_visible(focus, [], list(hide), [o.Name for o in isolated], kept, below)


def test_focus_on_a_feature_is_drawn_while_its_body_is_isolated(view_mode) -> None:
    s = build(body_visible=False)
    assert focus_visible(view_mode, s, s.pad, [s.body])
    s = build(nested=True)
    assert focus_visible(view_mode, s, s.pad, [s.part])


def test_focus_outside_the_isolated_objects_is_not_drawn(view_mode) -> None:
    s = build()
    assert not focus_visible(view_mode, s, s.box, [s.body])
    assert not focus_visible(view_mode, s, s.pad2, [s.pad])


def test_focus_on_a_feature_of_a_body_the_call_hides_is_not_drawn(view_mode) -> None:
    s = build()
    assert focus_visible(view_mode, s, s.pad, [])
    assert not focus_visible(view_mode, s, s.pad, [], hide=["Body"])

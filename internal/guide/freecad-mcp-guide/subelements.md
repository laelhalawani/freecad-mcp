# Choosing faces and edges

Face1 and Edge1 numbers change when the shape changes. Never guess them.

## Find them

Call list_subelements with doc_name and obj_name. kind is faces (default), edges or all.

A face row gives:
- name, such as Face3
- surface: plane, cylinder, cone, sphere, torus or other
- area in mm^2 and centre in global coordinates
- normal for a plane, pointing out of the solid
- radius and axis for a cylinder or sphere

An edge row gives name, curve (line, circle, other), length, and radius and centre for a circle.

## Pick by geometry

- Bottom face: the plane with normal (0, 0, -1) and the lowest centre z.
- Top face: the plane with normal (0, 0, 1) and the highest centre z.
- Side face facing +X: the plane with normal (1, 0, 0).
- Hole wall: a cylinder face. Its radius is the hole radius. Its axis is the hole direction.
- Hole edge: a circle edge with that radius.
- Several equal faces, such as a bolt pattern: compare centres, then list them all in one entry with "faces".

## Use them

- measure: {"object": "Body", "sub": "Face3"}. Pass a sub exactly as listed.
- FEM constraints and other References: {"object_name": "Body", "faces": ["Face3", "Face7"]}.

## Renumbering

- Call list_subelements again after any change to the object: a boolean, a new feature, a changed dimension. Face names from before may point to other faces now.
- A face name belongs to one object. Ask for the object that holds the shape you mean: the Body or Part, not the sketch under it.

## When the user points

- Call get_selection when the user says "this face" or "the selected edge".
- It returns object names and sub-elements. Pass them to measure unchanged. A name below the top level looks like Body.Pad.Face3. Keep the whole path.

# Property values

Applies to obj_properties in create_object and update_object.

## Units

- Lengths in mm and angles in degrees may be plain numbers: {"Length": 20, "Height": 5.5}.
- Every other quantity is a string with its unit. A bare number is in FreeCAD base units (mm, kg, s), so it is almost never what you mean.

| Property | Wrong | Right |
| --- | --- | --- |
| Force | 100 (0.1 N) | "100 N" |
| Pressure | 2 (0.002 MPa) | "2 MPa" |
| YoungsModulus | 210 | "210 GPa" |
| Density | 7900 | "7900 kg/m^3" |

- A length may also carry a unit: "2 in", "0.5 m".
- The reply of create_object and update_object lists each quantity it set, and get_object and list_objects show every quantity, in the units FreeCAD prefers for it ("100.00 N", "2000.00 kPa"). Read it. A wrong unit shows there at once, for example Force: 100.00 mN.
- A string map such as a FEM material takes numbers too: {"Material": {"Name": "Steel", "YoungsModulus": "210 GPa", "PoissonRatio": 0.3, "Density": "7900 kg/m^3"}}. FreeCAD stores each value as text.

## Expressions

- A string starting with "=" binds an expression: {"Height": "=Params.thickness * 2"}.
- Params is the object name of a spreadsheet, thickness an alias in it. See parametric.md.
- Other objects work too: {"Length": "=Box.Width + 5 mm"}.
- "=" alone removes the binding and keeps the current value: {"Height": "="}.
- An expression that cannot be parsed is an error that names the property, and create_object creates nothing. An expression that parses but points at something missing leaves the object created but not computing: the reply names it. Fix it with update_object or delete_object.

## One part of a value

- A dotted name sets one part and leaves the rest: {"Placement.Base.z": 5} moves the object up to z = 5, {"Placement.Base.x": 10, "Placement.Base.y": 0} sets two parts.
- {"Placement.Rotation.Angle": 30} turns the object 30 degrees about its current axis, in degrees. An object that is not turned yet has the axis (0, 0, 1). To change the axis, set a whole Rotation: {"Placement": {"Rotation": {"Axis": {"x": 0, "y": 1, "z": 0}, "Angle": 30}}}.
- The settable parts of a Placement are Base.x, Base.y, Base.z, Rotation.Angle, Rotation.Yaw, Rotation.Pitch and Rotation.Roll (all in degrees), and Rotation.Axis.x, .y and .z (FreeCAD renormalises the axis, so it can change the angle). A part takes a number or an expression, never a whole dict.
- A dotted name gets no quantity line in the reply; read the value with get_object.
- A dotted name takes an expression too: {"Placement.Base.z": "=Params.thickness"}. "=" alone removes it: {"Placement.Base.z": "="}.
- A wrong part name is an error that names the path.

## Links

- A link property takes an object name: {"Base": "Box", "Tool": "Cylinder"}.
- A link list takes a list of names: {"Shapes": ["Box", "Cylinder"]} on Part::MultiFuse and Part::MultiCommon.
- Create the linked objects first. An unknown name is an error.

## References

- References is always a list. Each entry is one of:
  - {"object_name": "Box", "face": "Face1"}
  - {"object_name": "Box", "faces": ["Face1", "Face2"]}
  - ["Box", "Face1"]
  - ["Box", ["Face1", "Face2"]]
- Get the names from list_subelements. Read subelements.md.

## Placement

{"Placement": {"Base": {"x": 10, "y": 0, "z": 0}, "Rotation": {"Axis": {"x": 0, "y": 0, "z": 1}, "Angle": 45}}}

- Angle is in degrees.
- Set Placement with update_object after create_object, or in the same create_object call.
- Size comes from the object's own properties (Length, Radius, Height). There is no scale property.

## Colors

{"ViewObject": {"ShapeColor": [0.8, 0.2, 0.2, 1.0]}}

- The four numbers are red, green, blue, alpha from 0 to 1.

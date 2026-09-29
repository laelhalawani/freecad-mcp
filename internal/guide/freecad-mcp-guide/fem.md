# FEM from analysis to result

Solve a static stress problem with CalculiX. Create every FEM object with create_object and pass analysis_name so it joins the analysis.

## Steps

1. The solid exists. Note its object name, for example Body.
2. Create the analysis:
   {"obj_type": "Fem::AnalysisPython", "obj_name": "Analysis"}
3. Create the material:
   {"obj_type": "Fem::MaterialCommon", "obj_name": "Steel", "analysis_name": "Analysis", "obj_properties": {"Material": {"Name": "Steel", "YoungsModulus": "210 GPa", "PoissonRatio": 0.3, "Density": "7900 kg/m^3"}}}
4. Create the mesh. It is generated when created:
   {"obj_type": "Fem::FemMeshGmsh", "obj_name": "Mesh", "analysis_name": "Analysis", "obj_properties": {"Shape": "Body", "CharacteristicLengthMax": 5, "CharacteristicLengthMin": 1}}
5. Call list_subelements on the solid. Pick the fixed face and the loaded face.
6. Create a fixed support:
   {"obj_type": "Fem::ConstraintFixed", "obj_name": "Fixed", "analysis_name": "Analysis", "obj_properties": {"References": [{"object_name": "Body", "faces": ["Face5"]}]}}
7. Create the load, a force or a pressure:
   {"obj_type": "Fem::ConstraintForce", "obj_name": "Load", "analysis_name": "Analysis", "obj_properties": {"References": [{"object_name": "Body", "faces": ["Face3"]}], "Force": "500 N"}}
   {"obj_type": "Fem::ConstraintPressure", "obj_name": "Pressure", "analysis_name": "Analysis", "obj_properties": {"References": [{"object_name": "Body", "faces": ["Face3"]}], "Pressure": "2 MPa"}}
8. Call run_fem_analysis with doc_name and analysis_name.

## Rules

- Force and Pressure are strings with units. A bare number is in base units. Read values.md.
- CharacteristicLengthMax and Min are in mm. A smaller size gives more elements and a longer run. Start near one tenth of the smallest part dimension.
- A Fem::ConstraintForce acts along the outward normal of its face: a force on a top face acts up. Set Reversed true to push down into the face. A Fem::ConstraintPressure acts into its face, against the outward normal. Reversed true flips either.
- The reply of create_object and update_object for a load states the direction as a vector and in words, for example "Force 500.00 N acts along (0, 0, 1): the outward normal of Face3." Before a face is referenced the reply says no direction is known yet. Check it against what you meant. If a load acts the wrong way, call update_object with Reversed true on it.
- The analysis needs one material, one mesh, at least one fixed support and at least one load.

## Read the result

- run_fem_analysis returns max and min von Mises stress in MPa, max displacement in mm, the node count, the result object, the working directory, and every load with its magnitude and direction.
- Its screenshot is coloured by von Mises stress, the way FreeCAD shows a result, and the reply gives the range of the colour scale in MPa. The solid and the mesh are hidden so the result shows. Show one again with update_object and {"ViewObject": {"Visibility": true}}. A shown solid covers the stress colours: hide it again, or use get_view to look. The colour bar is labelled in Pa (1e6 Pa = 1 MPa).
- The colours need a 3D view. Without one the reply carries only the numbers.
- Compare max von Mises stress with the yield strength of the material. Report the ratio.
- Tell the user the load, the material and the mesh size with the result.

## Errors

- A prerequisite error names what is missing. Create it with create_object and analysis_name, then run again.
- A solver error names the working directory. The solver output files are there.
- A timeout does not stop the solver. Call get_rpc_status, then run again with a larger timeout when needed.
- run_fem_analysis blocks FreeCAD's GUI thread until it finishes. Send no other call meanwhile. get_rpc_status still answers.

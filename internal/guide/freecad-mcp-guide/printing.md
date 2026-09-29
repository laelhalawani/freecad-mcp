# Printing

## Check

Call check_printability before every export for a printer. Pass bed_x, bed_y and bed_z in mm to check the fit.

It reports per object: valid and closed shape, solid count, whether the tessellated mesh is closed without non-manifold edges or self-intersections, the bounding box size, the overhang area that needs support, and the bed fit (turned 90 degrees if needed).

- printable is true only when something was checked and nothing has an issue.
- Set build_direction to the axis that points up on the printer. The default is +Z.
- overhang_angle_deg is measured from vertical. The default is 45.
- Tell the user the size, the overhang area and whether it fits.

## Fix

- Invalid or open shape from a failed feature: call recompute_document, then fix the object it names with update_object.
- Mesh object with defects: call analyze_mesh. It lists the repair_mesh steps that fix them. Call repair_mesh with those steps.
- Mesh that must become a solid: call mesh_to_solid. Above 200000 facets pass force true. An open mesh gives a shell, so repair first.
- Too big for the bed: change the dimensions with update_object, or split the part.

## Export

- Call export_document with a .stl or .3mf path.
- 3MF keeps one object per part and declares mm. Slicers prefer it.
- quality is coarse for previews, standard for FDM, fine for resin and small curved parts. linear_deflection and angular_deflection_deg override it.
- Pass overwrite true when the file exists.
- Call solid_to_mesh to inspect the triangles a printer will get. Export does not need it.

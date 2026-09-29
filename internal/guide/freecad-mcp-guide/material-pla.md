# PLA

Design rules for FDM parts in PLA with a 0.4 mm nozzle. Sizes in mm. "Per side" is the gap on each face. The values are starting points: settle them for the user's printer with the test coupon below. Keep each clearance in a Params cell (read parametric.md), so a coupon result changes the whole model.

## Accuracy

- PLA shrinks least of the common filaments: holes print 0.1 to 0.2 undersize on diameter; outer faces print 0.05 to 0.1 oversize.
- A hole for a bought part (screw, rod, bearing, insert): model a vertical hole (axis along z) at nominal + 0.2 on diameter. A horizontal hole sags at the top: above 3 diameter model a teardrop with its 45 degree point up; below 3, nominal + 0.3.
- Two printed parts that fit together: model the gaps under Fits. They already cover the print error of both parts; do not add the hole allowance as well.
- The first layer flares 0.2 to 0.5 outward: chamfer every bottom outer edge 0.5 x 45 degrees.

## Fits

- Sliding (slots, rails, dovetails): 0.20 per side.
- Rotating (a pin turning in a hole): 0.25 per side.
- Press: aim for 0.1 to 0.2 interference on diameter at printed size; more cracks PLA. Model the hole at pin + 0.05 on diameter for a metal pin, at pin + 0.1 for a printed pin.
- Avoid clips in PLA: it is brittle (under 2 % stretch across layers) and a held clip creeps loose. Use screws, or PETG for the clip. A PLA clip that cannot be avoided is single use and bends only slightly.

## Threads

- Profile: 45 degree flanks with a flat crest and a flat root, each at least 0.5 wide. Never a sharp V.
- Clearance on both parts, never only one: nut +0.17 and bolt -0.13 on the radius (0.30 radial in total).
- Engagement, the thread depth minus the total radial clearance, at least 0.6: thread depth at least 1.0.
- Pitch at least 2 x depth + 1.0, which leaves the two flats. M10 x 3 with depth 1.0 works; M10 x 2 with depth 1.0 leaves no flats and jams.
- Smallest reliable thread M8 x 3 with depth 1.0. Below that use heat-set inserts or captive nuts.
- Print both parts with the thread axis vertical. Chamfer both entries 1.0 x 45 degrees.
- PLA threads crack at the root under torque: use heat-set inserts for anything tightened with a tool or opened often. Insert hole 4.0 to 4.4 for M3, 1.0 deeper than the insert, wall around it at least 1.6.
- Captive nut: pocket at the nut's flats + 0.2 per side, loaded from the side.

## Strength

- Across layers PLA holds about 77 % of its in-plane strength and breaks suddenly, with almost no stretch.
- Lay a part loaded in bending so the bending runs along the layer lines, not across them. Never load a thin stem across its layers.
- Walls: 0.8 minimum (two lines), 1.6 for structural and threaded walls. Use multiples of 0.4.

## Heat and creep

- Softens near 55 to 60 C: keep service below 45 C. Never in a car, a sunny window or next to a warm motor.
- PLA creeps more than the other common filaments: a press fit, a clamp or a screw preload relaxes within days. Design clamps to close onto a hard stop and put a washer or metal sleeve under screw heads.

## Limits

- Minimum wall and feature 0.8; minimum hole 1.0 vertical, 2.0 horizontal.
- Overhangs up to 45 degrees from vertical without supports.
- Bridges up to 10 print cleanly; beyond that add a rib or chamfer the roof at 45 degrees.
- Outer edges rounded or chamfered at least 0.5; inside corners radius at least 0.5, since PLA cracks from sharp inside corners.

## Test coupon

Print these small samples on the user's printer before large parts, then set the Params cells to the ones that fit best:
- hole ladder 3, 5 and 10 diameter at +0.0 to +0.5 in steps of 0.1, vertical and horizontal
- pin and slot at 0.10, 0.20 and 0.30 per side
- press pair: a printed pin in holes at pin + 0.0 to + 0.3 on diameter in steps of 0.1
- the part's thread: nut and bolt 10 tall at 0.20, 0.30 and 0.40 radial gap, printed upright

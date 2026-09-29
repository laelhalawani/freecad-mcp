# PETG

Design rules for FDM parts in PETG with a 0.4 mm nozzle. Sizes in mm. "Per side" is the gap on each face. The values are starting points: settle them for the user's printer with the test coupon below. Keep each clearance in a Params cell (read parametric.md), so a coupon result changes the whole model.

## Accuracy

- Holes print 0.1 to 0.3 undersize on diameter; outer faces print 0.05 to 0.15 oversize.
- A hole for a bought part (screw, rod, bearing, insert): model a vertical hole (axis along z) at nominal + 0.3 on diameter. A horizontal hole sags at the top: above 3 diameter model a teardrop with its 45 degree point up; below 3, nominal + 0.4.
- Two printed parts that fit together: model the gaps under Fits. They already cover the print error of both parts; do not add the hole allowance as well.
- The first layer flares 0.2 to 0.5 outward: chamfer every bottom outer edge 0.5 x 45 degrees.

## Fits

- Sliding (slots, rails, dovetails): 0.25 per side.
- Rotating (a pin turning in a hole): 0.30 per side; 0.35 under side load. PETG on PETG grips.
- Press: aim for 0.1 to 0.2 interference on diameter at printed size, never more than 0.25. Model the hole at pin + 0.1 on diameter for a metal pin, at pin + 0.2 for a printed pin.
- Clips work in PETG: it bends about 5 % before it yields. Gap 0.5 per side around a latch, beam at least 5 wide, root fillet at least half the beam thickness, and the beam bending in the xy plane, never across layers.

## Threads

- Profile: 45 degree flanks with a flat crest and a flat root, each at least 0.5 wide. Never a sharp V.
- Clearance on both parts, never only one: nut +0.20 and bolt -0.15 on the radius (0.35 radial in total).
- Engagement, the thread depth minus the total radial clearance, at least 0.6: thread depth at least 1.0.
- Pitch at least 2 x depth + 1.0, which leaves the two flats. M10 x 3 with depth 1.0 works; M10 x 2 with depth 1.0 leaves no flats and jams.
- Smallest reliable thread M8 x 3 with depth 1.0. Below that use heat-set inserts or captive nuts.
- Print both parts with the thread axis vertical. Chamfer both entries 1.0 x 45 degrees.
- Heat-set insert: hole 4.0 to 4.4 for M3, 1.0 deeper than the insert, wall around it at least 1.6.
- Captive nut: pocket at the nut's flats + 0.2 per side, loaded from the side.

## Strength

- Across layers PETG holds about 84 % of its in-plane strength and stretches less than half as far.
- Lay a part loaded in bending so the bending runs along the layer lines, not across them.
- Walls: 0.8 minimum (two lines), 1.6 for structural and threaded walls. Use multiples of 0.4.

## Heat and creep

- Softens near 70 to 80 C: keep service below 60 C. A car in the sun or a hot window is too hot.
- Bolted joints lose preload: put a washer or metal sleeve under the head. Clamps close onto a hard stop, not onto their own preload.

## Limits

- Minimum wall and feature 0.8; minimum hole 1.5 vertical, 2.5 horizontal.
- Overhangs up to 45 degrees from vertical without supports; 40 for clean faces.
- Bridges sag beyond 5: add a rib or chamfer the roof at 45 degrees.
- Outer edges rounded or chamfered at least 0.5; inside corners radius at least 0.5.

## Traps

- PETG strings across gaps under 3; keep such gaps away from faces that must stay clean.
- Large parts need a flat base at least 1 thick to come off the bed whole.
- Sliding faces stick: narrow the contact to ribs.

## Test coupon

Print these small samples on the user's printer before large parts, then set the Params cells to the ones that fit best:
- hole ladder 3, 5 and 10 diameter at +0.0 to +0.5 in steps of 0.1, vertical and horizontal
- pin and slot at 0.15, 0.25 and 0.35 per side
- press pair: a printed pin in holes at pin + 0.0 to + 0.3 on diameter in steps of 0.1
- the part's thread: nut and bolt 10 tall at 0.30, 0.35 and 0.40 radial gap, printed upright

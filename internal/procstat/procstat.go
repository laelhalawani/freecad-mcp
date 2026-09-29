// Package procstat measures how busy a process is from its CPU time, so
// get_rpc_status can tell a FreeCAD that is computing from one that is stuck
// behind a dialog.
package procstat

import (
	"context"
	"time"
)

// BusyCores is the CPU use, in cores, from which a process counts as busy: an
// idle FreeCAD sits far below it, a meshing or boolean thread near one core, and
// a single core at half load is clearly working.
const BusyCores = 0.5

// Window is how long Cores samples.
const Window = 2 * time.Second

// Cores returns the average number of cores process pid used over window: its
// CPU time (user plus kernel) between two samples, over the wall time. It
// returns an error where the process cannot be read or the platform has no
// reading.
func Cores(ctx context.Context, pid int, window time.Duration) (float64, error) {
	first, err := cpuSeconds(pid)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(window):
	}
	second, err := cpuSeconds(pid)
	if err != nil {
		return 0, err
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0, nil
	}
	return max(0, second-first) / elapsed, nil
}

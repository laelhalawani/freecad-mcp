//go:build !windows && !linux && !darwin

package procstat

import "errors"

func cpuSeconds(pid int) (float64, error) {
	return 0, errors.New("process CPU time is not available on this platform")
}

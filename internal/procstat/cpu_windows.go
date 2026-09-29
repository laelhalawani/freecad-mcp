package procstat

import (
	"syscall"
	"time"
)

// processQueryLimitedInformation is the access GetProcessTimes needs.
const processQueryLimitedInformation = 0x1000

func cpuSeconds(pid int) (float64, error) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(h)
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	// A FILETIME counts 100 ns ticks.
	ticks := func(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return (time.Duration(ticks(kernel)+ticks(user)) * 100 * time.Nanosecond).Seconds(), nil
}

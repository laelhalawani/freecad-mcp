package procstat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// clockTicks is the kernel's USER_HZ, 100 on every Linux platform in use.
const clockTicks = 100.0

func cpuSeconds(pid int) (float64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The command name in parentheses may hold spaces; the fields after it
	// start with the state, so utime and stime are its 12th and 13th.
	text := string(data)
	end := strings.LastIndex(text, ")")
	if end < 0 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 13 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	utime, err := strconv.ParseFloat(fields[11], 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseFloat(fields[12], 64)
	if err != nil {
		return 0, err
	}
	return (utime + stime) / clockTicks, nil
}

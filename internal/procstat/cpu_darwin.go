package procstat

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// cpuSeconds reads the process's cumulative CPU time from ps, which prints it
// as [[hh:]mm:]ss.cc.
func cpuSeconds(pid int) (float64, error) {
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return 0, fmt.Errorf("no process %d", pid)
	}
	total := 0.0
	for _, part := range strings.Split(text, ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, err
		}
		total = total*60 + v
	}
	return total, nil
}

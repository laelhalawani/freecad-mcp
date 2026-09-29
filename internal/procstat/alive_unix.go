//go:build !windows

package procstat

import (
	"errors"
	"syscall"
)

// Alive reports whether process pid exists and has not exited. A process this
// user may not signal still exists.
func Alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

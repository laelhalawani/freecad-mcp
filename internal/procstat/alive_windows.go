package procstat

import "syscall"

const (
	// stillActive is the exit code GetExitCodeProcess reports for a running process.
	stillActive = 259
	// errorAccessDenied and errorInvalidParameter are what OpenProcess returns
	// for a process this user may not open and for a pid that does not exist.
	errorAccessDenied     syscall.Errno = 5
	errorInvalidParameter syscall.Errno = 87
)

// Alive reports whether process pid exists and has not exited. A process this
// user may not open (access denied) exists. A pid that Windows has reused for
// another process reads as alive.
func Alive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return err != errorInvalidParameter
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

//go:build !windows

package winsession

// Of returns ErrUnsupported: only Windows has sessions.
func Of(pid int) (uint32, error) { return 0, ErrUnsupported }

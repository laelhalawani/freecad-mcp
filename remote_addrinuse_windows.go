//go:build windows

package main

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// isAddrInUse reports whether err is a failed bind because the port is
// already in use. syscall.EADDRINUSE on Windows is a value Go's syscall
// package invents for portable code (APPLICATION_ERROR + n, not a real
// Windows error code), so it never actually matches a real bind failure
// here; that comes back as windows.WSAEADDRINUSE (10048), which
// syscall.Errno.Is does not recognise on its own (it maps only permission,
// exist, not-exist and unsupported), so it is checked for explicitly too.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, windows.WSAEADDRINUSE)
}

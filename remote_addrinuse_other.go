//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// isAddrInUse reports whether err is a failed bind because the port is
// already in use.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

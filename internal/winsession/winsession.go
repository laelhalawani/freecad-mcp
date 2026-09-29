// Package winsession reports which Windows session a process runs in. Session
// 0 is where services and processes started from an SSH login run; it has no
// desktop, so a GUI application started there is invisible to the user.
package winsession

import "errors"

// ErrUnsupported is returned by Of on a system that has no Windows sessions.
var ErrUnsupported = errors.New("Windows sessions are not available on this system")

// Hidden reports whether a process in session id has no desktop.
func Hidden(id uint32) bool { return id == 0 }

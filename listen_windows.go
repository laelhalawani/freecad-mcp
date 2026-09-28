//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// hideConsole detaches this process from its console window when it is the
// only process attached to it, which is the case for a logon start (Task
// Scheduler opens a console just for it): a logon start then shows at most a
// brief flash before the window closes. Started from an interactive
// terminal, the shell is attached to the same console too, so the count is
// greater than one and the console is left alone.
func hideConsole() {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	getConsoleProcessList := kernel32.NewProc("GetConsoleProcessList")
	freeConsole := kernel32.NewProc("FreeConsole")

	var pids [2]uint32
	n, _, _ := getConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n == 1 {
		freeConsole.Call()
	}
}

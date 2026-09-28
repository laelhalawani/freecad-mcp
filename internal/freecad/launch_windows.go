//go:build windows

package freecad

import (
	"errors"
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup  = 0x00000200
	detachedProcess        = 0x00000008
	createBreakawayFromJob = 0x01000000

	// errorAccessDenied is what CreateProcess returns for
	// CREATE_BREAKAWAY_FROM_JOB when the current job object does not allow
	// breakaway.
	errorAccessDenied syscall.Errno = 5
)

// startDetached starts name with args and env detached from this process: a
// new process group so it does not receive this process's console control
// events, no console window, and broken away from any job object an AI
// client's launcher put this server in (most run MCP servers in a
// kill-on-close job, which would otherwise take the FreeCAD process down with
// it). A job that forbids breakaway refuses CreateProcess with access denied;
// this is retried once without CREATE_BREAKAWAY_FROM_JOB.
func startDetached(name string, args, env []string) (*exec.Cmd, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess | createBreakawayFromJob}
	err := cmd.Start()
	if err == nil {
		return cmd, nil
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) || errno != errorAccessDenied {
		return nil, err
	}
	cmd = exec.Command(name, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

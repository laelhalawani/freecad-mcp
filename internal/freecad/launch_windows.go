//go:build windows

package freecad

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup  = 0x00000200
	detachedProcess        = 0x00000008
	createBreakawayFromJob = 0x01000000
	normalPriorityClass    = 0x00000020

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
// it). CREATE_NORMAL_PRIORITY_CLASS is set explicitly so FreeCAD, a GUI
// application the user works in, never inherits a below-normal priority
// class from whatever started this process (the listener's own Task
// Scheduler task has no <Priority> element, which Task Scheduler defaults to
// 7, below normal). A job that forbids breakaway refuses CreateProcess with
// access denied; this is retried once without CREATE_BREAKAWAY_FROM_JOB. Its
// working directory is the user's home, not whatever directory the task or
// service that started this process was given (Task Scheduler and launchd
// set none), so a relative path an agent's execute_code opens or exports
// then lands somewhere predictable.
func startDetached(name string, args, env []string) (*exec.Cmd, error) {
	dir := homeDirOrEmpty()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess | createBreakawayFromJob | normalPriorityClass,
	}
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
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess | normalPriorityClass}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// homeDirOrEmpty returns the user's home directory, or "" (cmd.Dir's
// "inherit this process's own" default) when it cannot be found.
func homeDirOrEmpty() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

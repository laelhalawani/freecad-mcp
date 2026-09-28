//go:build !windows

package freecad

import (
	"os/exec"
	"syscall"
)

// startDetached starts name with args and env in a new session, detached
// from this process's controlling terminal and process group, so signals or
// a process-group kill aimed at this server do not reach the FreeCAD it
// launched.
func startDetached(name string, args, env []string) (*exec.Cmd, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

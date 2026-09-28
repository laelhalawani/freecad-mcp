//go:build !windows

package freecad

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetached starts name with args and env in a new session, detached
// from this process's controlling terminal and process group, so signals or
// a process-group kill aimed at this server do not reach the FreeCAD it
// launched. Its working directory is the user's home, not whatever
// directory the OS gave this process (for example a systemd user service
// without WorkingDirectory= runs in the home already, but a listener
// started by launchd or Task Scheduler is not guaranteed one): a relative
// path an agent's execute_code opens or exports then lands somewhere
// predictable instead of wherever this process happened to start in.
func startDetached(name string, args, env []string) (*exec.Cmd, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Dir = homeDirOrEmpty()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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

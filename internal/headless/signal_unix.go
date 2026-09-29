//go:build !windows

package headless

import (
	"os"
	"os/exec"
	"syscall"
)

// crashSignal reports the signal that killed the process, if any, with the
// negative signal number Python's returncode would carry.
func crashSignal(state *os.ProcessState) (string, int, bool) {
	if state == nil {
		return "", 0, false
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return "", 0, false
	}
	return signalName(ws.Signal()), -int(ws.Signal()), true
}

// killTree runs the command in its own process group and kills the whole
// group on timeout, so the freecadcmd a wrapper script started ends too.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// startTree starts cmd in its own process group and returns a kill that ends
// the whole group (safe to call more than once, and after the process ended).
// A hard kill of this process leaves the group running: nothing cheap covers it.
func startTree(cmd *exec.Cmd) (kill func(), err error) {
	killTree(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	return func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }, nil
}

func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGBUS:
		return "SIGBUS"
	case syscall.SIGFPE:
		return "SIGFPE"
	case syscall.SIGILL:
		return "SIGILL"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	return sig.String()
}

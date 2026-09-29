//go:build windows

package headless

import (
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no signals; a native crash ends the process with an NTSTATUS
// exception code as its exit status.
var exceptions = map[uint32]string{
	0xC0000005: "EXCEPTION_ACCESS_VIOLATION",
	0xC00000FD: "EXCEPTION_STACK_OVERFLOW",
	0xC000001D: "EXCEPTION_ILLEGAL_INSTRUCTION",
	0xC0000094: "EXCEPTION_INT_DIVIDE_BY_ZERO",
	0xC0000409: "STATUS_STACK_BUFFER_OVERRUN",
	0xC0000374: "STATUS_HEAP_CORRUPTION",
}

// crashSignal reports a native crash. The exit code already names it, so no
// separate number is returned. A code with no known name still counts as a
// crash when it is an NTSTATUS error (0xC0000000 to 0xCFFFFFFF, such as an
// unlisted exception) or -1 (0xFFFFFFFF, what a hard abort leaves): the name is
// then empty.
func crashSignal(state *os.ProcessState) (string, int, bool) {
	if state == nil {
		return "", 0, false
	}
	code := uint32(state.ExitCode())
	if name, ok := exceptions[code]; ok {
		return name, 0, true
	}
	if code>>28 == 0xC || code == 0xFFFFFFFF {
		return "", 0, true
	}
	return "", 0, false
}

// killTree is a no-op on Windows: startTree puts the process in a job object.
func killTree(*exec.Cmd) {}

// startTree starts cmd inside a job object that kills every process in it when
// the handle closes, so a cancel, a timeout, the end of the run and even a hard
// kill of this process end the whole tree (a .cmd wrapper, a subprocess the
// script started). The returned kill terminates the tree and closes the handle;
// it is safe to call more than once. When the process cannot join a job (it is
// already in one that forbids it) only the process itself is ended.
func startTree(cmd *exec.Cmd) (kill func(), err error) {
	job, jerr := windows.CreateJobObject(nil, nil)
	if jerr == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, jerr = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); jerr != nil {
			windows.CloseHandle(job)
		}
	}
	var mu sync.Mutex
	closed := false
	inJob := false
	terminate := func() error {
		mu.Lock()
		defer mu.Unlock()
		if inJob && !closed {
			return windows.TerminateJobObject(job, 1)
		}
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.Cancel = terminate
	if err := cmd.Start(); err != nil {
		if jerr == nil {
			windows.CloseHandle(job)
		}
		return nil, err
	}
	if jerr == nil {
		if h, oerr := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)); oerr == nil {
			if windows.AssignProcessToJobObject(job, h) == nil {
				inJob = true
			}
			windows.CloseHandle(h)
		}
		if !inJob {
			windows.CloseHandle(job)
			closed = true
		}
	}
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if inJob && !closed {
			windows.TerminateJobObject(job, 1)
			windows.CloseHandle(job)
			closed = true
		} else if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}, nil
}

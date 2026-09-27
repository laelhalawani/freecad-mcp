//go:build windows

package headless

import (
	"os"
	"os/exec"
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
// separate number is returned.
func crashSignal(state *os.ProcessState) (string, int, bool) {
	if state == nil {
		return "", 0, false
	}
	name, ok := exceptions[uint32(state.ExitCode())]
	return name, 0, ok
}

// killTree is a no-op on Windows: freecadcmd.exe is started directly, and a
// timeout kills it. A .cmd wrapper set in FREECAD_MCP_FREECADCMD may leave its
// child running.
func killTree(*exec.Cmd) {}

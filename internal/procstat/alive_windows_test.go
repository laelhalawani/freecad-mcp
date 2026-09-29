package procstat

import (
	"os"
	"testing"
)

// TestAliveOnRealProcesses: this process is alive, the System process (pid 4)
// exists although a normal user may not open it, and a pid no process holds is
// not alive.
func TestAliveOnRealProcesses(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("this process reads as dead")
	}
	if !Alive(4) {
		t.Error("the System process reads as dead")
	}
	if Alive(0x7ffffff0) {
		t.Error("a pid nothing holds reads as alive")
	}
}

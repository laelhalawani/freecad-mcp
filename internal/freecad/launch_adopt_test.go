package freecad

import (
	"os"
	"os/exec"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

func TestAdoptedLaunchIsExitedOnceItsProcessIsGone(t *testing.T) {
	l := NewLauncher(domain.Settings{})
	l.Adopt(LaunchState{State: LaunchStarted, PID: os.Getpid(), LogPath: "freecad-gui.log"})
	if got := l.State(); got.State != LaunchStarted {
		t.Fatalf("a launch whose process runs reads %q", got.State)
	}

	// A real process that has already exited: this test binary asked to run nothing.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	l.Adopt(LaunchState{State: LaunchStarted, PID: cmd.Process.Pid, LogPath: "freecad-gui.log"})
	got := l.State()
	if got.State != LaunchExited || got.ExitedAt.IsZero() || got.LogPath != "freecad-gui.log" {
		t.Fatalf("adopted launch of a dead process = %+v", got)
	}
}

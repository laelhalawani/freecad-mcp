package mcpserver

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

// fakeGUIEnv makes this test binary act as the FreeCAD GUI command (TestMain).
const fakeGUIEnv = "FREECAD_MCP_TEST_FAKE_GUI"

func TestStartFreeCADForwardedToAWindowWithoutTheAgentConnectionSaysSo(t *testing.T) {
	t.Setenv(fakeGUIEnv, "1")
	old := forwardedAnswerWait
	forwardedAnswerWait = 100 * time.Millisecond
	t.Cleanup(func() { forwardedAnswerWait = old })
	// The GUI command exits 0 at once (a launch FreeCAD forwarded to an open
	// window) and nothing answers on the port (port 9), as with a window that
	// has no RPC server.
	cs := session(t, domain.Settings{Host: "127.0.0.1", Port: 9, FreecadGUI: []string{os.Args[0]}})

	res := call(t, cs, "start_freecad", nil)
	if !res.IsError {
		t.Fatalf("a forwarded launch nothing answers for succeeded: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{
		"did not answer on port 9 within", "no agent connection", "wait and call get_rpc_status before closing it",
		"Start RPC Server", "Auto-Start Server", "save, then close that FreeCAD and call start_freecad",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("reply lacks %q:\n%s", want, text)
		}
	}
}

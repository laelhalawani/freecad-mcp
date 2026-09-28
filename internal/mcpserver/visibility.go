package mcpserver

// Runtime visibility of the remote-only tools release_session and
// close_freecad: they are listed only while the session lock is on (remote
// access), learned from the addon's X-FreeCAD-MCP-Lock header and
// get_rpc_status's session key. The SDK sends tools/list_changed when they
// are added or removed.

import (
	"sync"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
)

// remoteToolsState is the Server's record of whether the remote-only tools
// are listed.
type remoteToolsState struct {
	mu sync.Mutex
	on bool
}

// listenerRunning is listenerapi.Running, held in a variable so a test can
// replace it without a real listener lock file.
var listenerRunning = listenerapi.Running

// initialRemoteTools decides the tools' visibility before FreeCAD answers:
// on when the configured host is not loopback, or when this machine's
// listener is running (listenerapi.Running); off otherwise. The first reply
// from FreeCAD corrects it.
func initialRemoteTools(settings domain.Settings) bool {
	return !domain.IsLoopbackHost(settings.Host) || listenerRunning()
}

// setRemoteTools adds (on) or removes (off) both remote-only tools. It is
// idempotent (a call that repeats the current state does nothing) and safe
// from any goroutine: onLock (every addon reply) and observeStatus (every
// get_rpc_status reply) can call it concurrently with each other and with the
// initial call from New. The whole check-and-mutate, including the
// AddTool/RemoveTools calls, runs under remote.mu so two concurrent calls for
// opposite states can never interleave and leave s.remote.on disagreeing with
// which tools are actually registered; mcp.AddTool and RemoveTools only
// schedule their tools/list_changed notification on a timer (they never call
// back into setRemoteTools synchronously), so holding the lock across them
// cannot deadlock.
func (s *Server) setRemoteTools(on bool) {
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	if s.remote.on == on {
		return
	}
	s.remote.on = on
	if on {
		s.addSessionTools()
	} else {
		s.mcpServer.RemoveTools(sessionToolNames...)
	}
}

// observeStatus reads the session key of a get_rpc_status reply and updates
// the tools' visibility from its enabled field. A reply with no session key
// (an old addon, or a reply that failed before that field was built) is
// ignored: it says nothing about whether the lock is on.
func (s *Server) observeStatus(status map[string]any) {
	session, ok := status["session"].(map[string]any)
	if !ok {
		return
	}
	s.setRemoteTools(boolField(session, "enabled"))
}

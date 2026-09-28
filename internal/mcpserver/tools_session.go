package mcpserver

// The remote-only tools release_session and close_freecad, listed only while
// the session lock is on (visibility.go), and the session fields
// get_rpc_status reports.

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

// sessionToolNames are the remote-only tools, added and removed together.
var sessionToolNames = []string{"release_session", "close_freecad"}

// freeForOthers ends both remote-only tools' descriptions.
const freeForOthers = "Remember to always save your project and close the app when you are not working or are " +
	"taking a longer break, to free it for other agents."

const releaseSessionDescription = "Free the FreeCAD session this agent holds, so another agent can use FreeCAD at " +
	"once instead of waiting for the idle timeout. With remote access on, an agent's first call to FreeCAD (anything " +
	"but start_freecad and get_rpc_status) claims FreeCAD for that agent until it has been idle for the configured " +
	"time; get_rpc_status shows who holds it and when it frees. This only frees this agent's own session: documents " +
	"stay open and unsaved changes stay unsaved, so save them first with save_document. " + freeForOthers

const closeFreeCADDescription = "Quit FreeCAD on the computer that runs it and free the session, for example at " +
	"the end of a work session. Documents with unsaved changes are refused unless discard_changes is true: save " +
	"them first with save_document or save_document_as. It is also refused while a task panel or command is open " +
	"in FreeCAD. FreeCAD closes a moment after the reply; start_freecad opens it again. " + freeForOthers

// sessionExplanation ends get_rpc_status's session sentence while the lock is
// on, naming the two tools that manage it.
const sessionExplanation = "With remote access on, one agent at a time holds FreeCAD; only get_rpc_status works " +
	"for the others until it frees. release_session frees your own session; close_freecad quits FreeCAD."

type releaseSessionFront struct {
	Released    bool   `yaml:"released"`
	SessionLock string `yaml:"session_lock"`
	Holder      string `yaml:"session_holder,omitempty"`
}

type closeFreeCADInput struct {
	DiscardChanges *bool `json:"discard_changes,omitempty" jsonschema:"quit even when documents have unsaved changes, losing them (default false)"`
}

type closeFreeCADFront struct {
	Closing   bool     `yaml:"closing"`
	Closed    []string `yaml:"closed_documents,omitempty"`
	Discarded []string `yaml:"discarded,omitempty"`
}

// addSessionTools registers release_session and close_freecad (setRemoteTools
// calls it when the lock turns on).
func (s *Server) addSessionTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "release_session",
		Description: releaseSessionDescription,
		InputSchema: inputSchema[struct{}](nil),
	}, s.releaseSession)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "close_freecad",
		Description: closeFreeCADDescription,
		InputSchema: inputSchema[closeFreeCADInput](map[string]string{"discard_changes": "false"}),
	}, s.closeFreeCAD)
}

// releaseSession frees the session lock this agent holds. It is exempt from
// the lock itself, so it never claims or is refused, but it still needs a
// live connection to call the addon.
func (s *Server) releaseSession(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "release the session", err, ""), nil, nil
	}
	res, err := conn.ReleaseSession(ctx)
	if err != nil {
		return s.withNotice(failure(ctx, "release the session", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("release the session", res, "")), nil, nil
	}

	enabled := boolField(res, "enabled")
	released := boolField(res, "released")
	pending := boolField(res, "pending")
	holder := str(res, "holder")

	front := releaseSessionFront{Released: released}
	var body string
	switch {
	case !enabled:
		front.SessionLock = "off"
		body = "Remote access is off on the FreeCAD computer, so there is no session to release."
	case released:
		front.SessionLock = "free"
		body = "Released your session: FreeCAD is free for other agents."
	case pending:
		// The caller's own job (execute_code_async, or a GUI task that
		// outlived its timeout) is still running: the addon keeps the lock
		// until it ends, then job_finished frees it.
		front.SessionLock = "pending"
		body = "FreeCAD stays with this agent until its running job ends, then it is freed."
	case holder != "":
		front.SessionLock = "other"
		front.Holder = holder
		if ownLabel := callerLabel(ctx); ownLabel != "" && holder == ownLabel {
			// The addon only ever reports a holder different from the
			// caller at all (a call from the session that already holds it
			// just refreshes it instead), so an equal label is never the
			// caller reading its own hold back; it shares the label with a
			// genuinely different session, most likely this same agent
			// from before a restart or another window of the same app,
			// but possibly a concurrent one (live-fixes review L2, N2:
			// worded the same way sessionInUseMessage, session.go, already
			// is).
			body = fmt.Sprintf("Another session with the same name (%s) holds FreeCAD, for example this agent "+
				"before a restart, or another window of the same app; release_session only frees your own "+
				"session.", holder)
		} else {
			body = fmt.Sprintf("Another agent (%s) holds FreeCAD; release_session only frees your own session.", holder)
		}
	default:
		front.SessionLock = "free"
		body = "This agent did not hold FreeCAD; nothing was released."
	}

	return s.withNotice(render.SuccessResult(front, body)), nil, nil
}

// closeFreeCAD quits FreeCAD and frees the session. On success the connector
// is reset, so the next call reconnects and re-checks the addon version once
// FreeCAD is back.
func (s *Server) closeFreeCAD(ctx context.Context, _ *mcp.CallToolRequest, in closeFreeCADInput) (*mcp.CallToolResult, any, error) {
	discard := in.DiscardChanges != nil && *in.DiscardChanges

	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "close FreeCAD", err, ""), nil, nil
	}
	res, err := conn.CloseFreeCAD(ctx, discard)
	if err != nil {
		return s.withNotice(failure(ctx, "close FreeCAD", err, "")), nil, nil
	}
	if !succeeded(res) {
		// The addon's own hint always wins for these conflicts (unsaved
		// changes, a task panel or command open, anything else that could
		// prompt on close), so no fallback hint or hintCodes is given here.
		return s.withNotice(reportedCode("close FreeCAD", res, "")), nil, nil
	}

	s.fc.reset()

	closed := stringItems(res["closed"])
	discarded := stringItems(res["discarded"])
	front := closeFreeCADFront{Closing: true, Closed: closed, Discarded: discarded}
	body := fmt.Sprintf("FreeCAD is closing on %s; the session is free. start_freecad opens it again.", s.config.FreeCAD.Host)
	if len(discarded) > 0 {
		body += fmt.Sprintf(" Discarded unsaved changes in: %s.", strings.Join(discarded, ", "))
	}

	return s.withNotice(render.SuccessResult(front, body)), nil, nil
}

// sessionFields are the session lock fields of get_rpc_status's front matter
// (tools_status.go embeds them).
type sessionFields struct {
	// SessionLock is off (remote access off), free, yours (this agent holds
	// FreeCAD) or other (another agent does).
	SessionLock string `yaml:"session_lock"`
	// SessionHolder is the holder's label, for yours and other.
	SessionHolder string `yaml:"session_holder,omitempty"`
	// SessionIdleSeconds is how long the holder has been idle.
	SessionIdleSeconds *int `yaml:"session_idle_seconds,omitempty"`
	// SessionFreesInSeconds is when the lock frees if the holder stays idle.
	SessionFreesInSeconds *int `yaml:"session_frees_in_seconds,omitempty"`
}

// sessionFront reads the session key of a get_rpc_status reply (nil when
// FreeCAD did not answer, or the key absent) into the front matter fields,
// and returns the body sentence that says it in words ("" when the lock is
// off).
func sessionFront(ctx context.Context, status map[string]any) (sessionFields, string) {
	session, _ := status["session"].(map[string]any)
	if !boolField(session, "enabled") {
		return sessionFields{SessionLock: "off"}, ""
	}

	holder := str(session, "holder")
	idle, hasIdle := intFromStatus(session, "idle_seconds")
	frees, hasFrees := intFromStatus(session, "frees_in_seconds")
	timeoutSeconds, _ := intFromStatus(session, "timeout_seconds")
	timeoutMinutes := timeoutSeconds / 60

	fields := sessionFields{}
	var state string
	switch {
	case !boolField(session, "held"):
		fields.SessionLock = "free"
		state = "FreeCAD is free; the first call of any agent other than start_freecad and get_rpc_status takes " +
			"it for that agent."
	case boolField(session, "yours"):
		fields.SessionLock = "yours"
		fields.SessionHolder = holder
		if hasIdle {
			fields.SessionIdleSeconds = &idle
		}
		if hasFrees {
			fields.SessionFreesInSeconds = &frees
		}
		state = fmt.Sprintf("This agent holds FreeCAD (idle %s). Save your work and call release_session or "+
			"close_freecad when you stop; it frees on its own after %d min without activity.",
			formatIdleDuration(idle), timeoutMinutes)
	default:
		fields.SessionLock = "other"
		fields.SessionHolder = holder
		if hasIdle {
			fields.SessionIdleSeconds = &idle
		}
		if hasFrees {
			fields.SessionFreesInSeconds = &frees
		}
		who := holder
		if ownLabel := callerLabel(ctx); ownLabel != "" && holder == ownLabel {
			// Same reasoning as release_session's own holder text above
			// (live-fixes review L2, N2).
			who = fmt.Sprintf("another session with the same name (%s), for example this agent before a "+
				"restart, or another window of the same app", holder)
		}
		if boolField(session, "busy") {
			state = fmt.Sprintf("FreeCAD is in use by %s, which is working now; it frees %d min after that "+
				"agent's last call, unless it releases it earlier. The person at the FreeCAD computer can also "+
				"free it with Force release.", who, timeoutMinutes)
		} else {
			state = fmt.Sprintf("FreeCAD is in use by %s, idle %s; it frees in %s unless that agent works again "+
				"or releases it.", who, formatIdleDuration(idle), formatFreesDuration(frees))
		}
	}

	return fields, state + " " + sessionExplanation
}

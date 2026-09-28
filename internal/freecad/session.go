package freecad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
)

// Fault codes of the addon's session lock.
const (
	// FaultSessionInUse refuses a call while another session holds FreeCAD:
	// "SESSION_IN_USE " followed by a JSON object.
	FaultSessionInUse = 4230
	// FaultSessionReleased tells a session, once, that the person at the
	// FreeCAD computer released it: "SESSION_RELEASED " followed by JSON.
	FaultSessionReleased = 4231
)

// The addon's Fault strings are a fixed word, a space, then the JSON payload.
const (
	inUsePrefix    = "SESSION_IN_USE "
	releasedPrefix = "SESSION_RELEASED "
)

// SessionInUseError is a call refused because another agent's session holds
// FreeCAD. All durations are in seconds and relative, never clock times.
type SessionInUseError struct {
	Holder         string // the holder's label, for example "claude-code on LAPTOP-2"
	IdleSeconds    int    // how long the holder has been idle
	FreesInSeconds int    // when the lock frees if the holder stays idle
	TimeoutSeconds int    // the configured idle timeout
	Busy           bool   // the holder has a call or an async job running
}

func (e *SessionInUseError) Error() string {
	return fmt.Sprintf("FreeCAD is in use by another agent (%s)", e.Holder)
}

// SessionReleasedError is the one-time notice that the person at the FreeCAD
// computer force-released this session.
type SessionReleasedError struct {
	ReleasedSecondsAgo int
}

func (e *SessionReleasedError) Error() string {
	return "the person at the FreeCAD computer released this session"
}

// sessionInUsePayload is the JSON object that follows "SESSION_IN_USE " in
// the addon's Fault string (session_lock.py).
type sessionInUsePayload struct {
	Busy           bool   `json:"busy"`
	FreesInSeconds int    `json:"frees_in_seconds"`
	Holder         string `json:"holder"`
	IdleSeconds    int    `json:"idle_seconds"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// sessionReleasedPayload is the JSON object that follows "SESSION_RELEASED "
// in the addon's Fault string.
type sessionReleasedPayload struct {
	ReleasedSecondsAgo int `json:"released_seconds_ago"`
}

// sessionFault turns the addon's session lock Faults (FaultSessionInUse,
// FaultSessionReleased) into *SessionInUseError and *SessionReleasedError,
// and returns any other error unchanged. A fault with the right code but a
// payload that fails to decode (an addon mismatch) is returned unchanged
// too, so it falls through to the generic FreeCAD-reported-error handling
// instead of a session error with zero fields.
func sessionFault(err error) error {
	var fault *xmlrpc.Fault
	if !errors.As(err, &fault) {
		return err
	}
	switch fault.Code {
	case FaultSessionInUse:
		if payload, ok := strings.CutPrefix(fault.String, inUsePrefix); ok {
			var p sessionInUsePayload
			if json.Unmarshal([]byte(payload), &p) == nil {
				return &SessionInUseError{
					Holder:         p.Holder,
					IdleSeconds:    p.IdleSeconds,
					FreesInSeconds: p.FreesInSeconds,
					TimeoutSeconds: p.TimeoutSeconds,
					Busy:           p.Busy,
				}
			}
		}
	case FaultSessionReleased:
		if payload, ok := strings.CutPrefix(fault.String, releasedPrefix); ok {
			var p sessionReleasedPayload
			if json.Unmarshal([]byte(payload), &p) == nil {
				return &SessionReleasedError{ReleasedSecondsAgo: p.ReleasedSecondsAgo}
			}
		}
	}
	return err
}

// WithSession returns a context whose FreeCAD calls identify the MCP session:
// id (at most 128 characters of [A-Za-z0-9._:-]) and label (at most 80
// printable characters) go out as the X-FreeCAD-MCP-Session and
// X-FreeCAD-MCP-Client headers.
func WithSession(ctx context.Context, id, label string) context.Context {
	return xmlrpc.WithHeaders(ctx, map[string]string{
		domain.HeaderSession: id,
		domain.HeaderClient:  label,
	})
}

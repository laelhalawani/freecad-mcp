package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/freecad-mcp/internal/freecad"
)

// The server's own deadline keeps the busy hint (wait, do not repeat) even for
// a tool that takes a timeout; only the addon's own timeout gets the larger
// timeout hint.
func TestDeadlineKeepsBusyHint(t *testing.T) {
	res := timedFailure(context.Background(), "check printability",
		&freecad.TimeoutError{Method: "check_printability", After: 270 * time.Second}, "Call again with a larger timeout.")
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "do not repeat the call") {
		t.Fatalf("busy hint missing: %s", text)
	}
	if strings.Contains(text, "larger timeout") {
		t.Fatalf("larger timeout hint must not replace the busy hint: %s", text)
	}
}

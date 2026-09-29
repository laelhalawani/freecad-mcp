package mcpserver

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
)

func TestSessionLabelEndsWithTheTagOfItsSessionID(t *testing.T) {
	fc := addon(t, nil)
	cs := session(t, settingsFor(fc))
	call(t, cs, "get_view", map[string]any{})
	calls := fc.CallsTo("get_active_screenshot")
	if len(calls) != 1 {
		t.Fatalf("addon calls = %d", len(calls))
	}
	client, id := calls[0].Header.Get(domain.HeaderClient), calls[0].Header.Get(domain.HeaderSession)
	if !regexp.MustCompile(`^test on .+ #[0-9a-f]{4}$`).MatchString(client) {
		t.Errorf("client label = %q, want <client> on <host> #<4 hex>", client)
	}
	if !strings.HasSuffix(client, sessionTag(id)) {
		t.Errorf("client label %q does not end with the tag of session %q (%q)", client, id, sessionTag(id))
	}
}

func TestSessionTagIsStableAndTellsSessionsApart(t *testing.T) {
	if sessionTag("0123456789abcdef") != sessionTag("0123456789abcdef") {
		t.Error("the tag changed for the same session id")
	}
	if sessionTag("0123456789abcdef") == sessionTag("0123456789abcdee") {
		t.Error("two session ids got the same tag")
	}
}

func TestHolderIsWordedAsAnotherSession(t *testing.T) {
	const own = "claude-code on WILD-DESKTOP #9c1e"
	for name, tc := range map[string]struct{ holder, own, want string }{
		"same name, other session":  {"claude-code on WILD-DESKTOP #a3f2", own, "another session (claude-code on WILD-DESKTOP #a3f2; this session is " + own + ")"},
		"another app":               {"codex on WILD-DESKTOP #a3f2", own, "another session (codex on WILD-DESKTOP #a3f2)"},
		"no label of its own":       {"codex on WILD-DESKTOP #a3f2", "", "another session (codex on WILD-DESKTOP #a3f2)"},
		"a client without the tags": {"claude-code on WILD-DESKTOP", own, "another session (claude-code on WILD-DESKTOP; this session is " + own + ")"},
	} {
		if got := holderWho(tc.holder, tc.own); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestSessionInUseNamesBothTagsForTheSameApp(t *testing.T) {
	msg := sessionInUseMessage(&freecad.SessionInUseError{
		Holder: "claude-code on WILD-DESKTOP #a3f2", IdleSeconds: 30, FreesInSeconds: 1500,
	}, "claude-code on WILD-DESKTOP #9c1e")
	for _, want := range []string{"another session (claude-code on WILD-DESKTOP #a3f2; this session is claude-code on WILD-DESKTOP #9c1e)", "idle 30 s"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

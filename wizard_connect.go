package main

// The install wizard's "FreeCAD was not found" choice: install FreeCAD
// first (the wizard ends with exitCancelled), or use FreeCAD on another
// computer (host, test, password, test again). The connection is saved by
// applyConnectChoice after registration.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/freecad-mcp/internal/remote"
)

// connectPhase is the sub-screen of connectHostStep.
type connectPhase int

const (
	connectHostPhase connectPhase = iota
	connectPasswordPhase
)

// connectState is the "FreeCAD on another computer" choice. The password is
// held in memory only, until applyConnectChoice.
type connectState struct {
	// InstallFirst is set when the user chose "Install FreeCAD on this
	// computer first": runWizard prints installFirstText and exits with
	// exitCancelled.
	InstallFirst bool
	// Chosen is set when the user chose "Use FreeCAD on another computer"
	// and the connection was tested; the addon is then not installed.
	Chosen   bool
	Host     string
	Port     int
	Password string

	// The rest is connectHostStep's own view state; it does not survive
	// past the wizard.
	phase connectPhase
	// portFocus is true while tab has moved focus to the Port field on the
	// host screen; typing then goes there (digits only) instead of Host.
	portFocus bool
	testing   bool
	// message is the current [ok]/[warn]/[fail] line(s) (joined by "\n"),
	// or "" before the first test.
	message string
	// ready is set once a test succeeded; enter then moves on.
	ready bool
}

// connectSteps are the steps after the addon step when FreeCAD was not found
// on this computer; each skips itself otherwise.
func connectSteps(ctx context.Context) []flow.Step[AppState] {
	return []flow.Step[AppState]{&connectHostStep{ctx: ctx}}
}

// connectSkipMsg is emitted by Init when this step does not apply, so
// Update can return flow.Skip (the flow.Step interface has no Init
// directive of its own).
type connectSkipMsg struct{}

// connectResultMsg carries the outcome of a testConnect call.
type connectResultMsg struct {
	result connectResult
}

type connectHostStep struct {
	ctx context.Context
}

func (s *connectHostStep) ID() string { return "freecad-connect" }

func (s *connectHostStep) Title(*AppState) string { return "FreeCAD on another computer" }

// Hints has no "q cancel": both phases are text-entry screens, where q
// types like any other rune (only ctrl+c quits; see Update).
func (s *connectHostStep) Hints(state *AppState) []struct{ Key, Label string } {
	c := &state.Connect
	if c.testing {
		return nil
	}
	if c.phase == connectPasswordPhase {
		label := "test again"
		if c.ready {
			label = "continue"
		}
		return []struct{ Key, Label string }{{"enter", label}, {"esc", "back"}}
	}
	label := "test connection"
	switch {
	case c.ready:
		label = "continue"
	case c.message != "":
		label = "retry"
	}
	return []struct{ Key, Label string }{{"tab", "switch field"}, {"enter", label}, {"esc", "back"}}
}

func (s *connectHostStep) Init(state *AppState) tea.Cmd {
	if state.Addon.Phase != addonNotFound {
		// FreeCAD was found this time (or is being located again): any
		// earlier "Use FreeCAD on another computer" choice no longer
		// applies, so finishWizard must not save it.
		state.Connect.Chosen = false
		return func() tea.Msg { return connectSkipMsg{} }
	}
	c := &state.Connect
	if c.Port == 0 {
		c.Port = domain.DefaultListenerPort
	}
	c.phase = connectHostPhase
	c.testing = false
	c.portFocus = false
	return nil
}

func (s *connectHostStep) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	c := &state.Connect
	switch m := msg.(type) {
	case connectSkipMsg:
		return flow.Skip, nil
	case connectResultMsg:
		c.testing = false
		applyConnectResult(c, m.result)
		return flow.Continue, nil
	case tea.KeyMsg:
		key := m.String()
		// Both phases are text-entry screens (host, password): only ctrl+c
		// quits here, so q (and every other rune) types into the field.
		if key == "ctrl+c" {
			return flow.Quit, nil
		}
		if c.testing {
			return flow.Continue, nil
		}
		if c.phase == connectPasswordPhase {
			return s.updatePassword(key, m, state)
		}
		return s.updateHost(key, m, state)
	}
	if tui.IsSpinMsg(msg) && c.testing {
		state.Spinner.Frame++
		return flow.Continue, tui.Spinner()
	}
	return flow.Continue, nil
}

func (s *connectHostStep) updateHost(key string, m tea.KeyMsg, state *AppState) (flow.Directive, tea.Cmd) {
	c := &state.Connect
	switch key {
	case "esc":
		state.Retreating = true
		c.Password = ""
		c.ready = false
		c.message = ""
		c.Chosen = false
		return flow.Back, nil
	case "tab":
		c.portFocus = !c.portFocus
		return flow.Continue, nil
	case "enter":
		if c.ready {
			return flow.Next, nil
		}
		host := strings.TrimSpace(c.Host)
		if host == "" {
			return flow.Continue, nil
		}
		if err := domain.ValidateHost(host); err != nil {
			c.message = "[fail] " + err.Error()
			return flow.Continue, nil
		}
		if err := validatePortValue(c.Port); err != nil {
			c.message = "[fail] " + err.Error()
			return flow.Continue, nil
		}
		c.testing = true
		c.message = ""
		return flow.Continue, tea.Batch(tui.Spinner(), testConnectCmd(s.ctx, c.Host, c.Port, ""))
	case "backspace":
		if c.portFocus {
			c.Port /= 10
		} else if c.Host != "" {
			c.Host = trimLastRune(c.Host)
		}
		c.ready = false
		c.message = ""
	default:
		if len(m.Runes) == 0 {
			return flow.Continue, nil
		}
		if c.portFocus {
			for _, r := range m.Runes {
				if r < '0' || r > '9' {
					continue
				}
				if next := c.Port*10 + int(r-'0'); next <= 65535 {
					c.Port = next
				}
			}
		} else {
			c.Host += string(m.Runes)
		}
		c.ready = false
		c.message = ""
	}
	return flow.Continue, nil
}

func (s *connectHostStep) updatePassword(key string, m tea.KeyMsg, state *AppState) (flow.Directive, tea.Cmd) {
	c := &state.Connect
	switch key {
	case "esc":
		// One screen back: the host screen, secrets cleared (pitfall).
		c.phase = connectHostPhase
		c.Password = ""
		c.message = ""
		c.ready = false
	case "enter":
		if c.ready {
			return flow.Next, nil
		}
		if err := validatePassword(c.Password); err != nil {
			c.message = "[fail] " + err.Error()
			return flow.Continue, nil
		}
		c.testing = true
		c.message = ""
		return flow.Continue, tea.Batch(tui.Spinner(), testConnectCmd(s.ctx, c.Host, c.Port, c.Password))
	case "backspace":
		c.Password = trimLastRune(c.Password)
		c.ready = false
		c.message = ""
	default:
		if len(m.Runes) > 0 {
			c.Password += string(m.Runes)
			c.ready = false
			c.message = ""
		}
	}
	return flow.Continue, nil
}

// applyConnectResult classifies a testConnect result by the listener
// marker, matching whatever the status code was, into the connectState's
// view fields.
func applyConnectResult(c *connectState, r connectResult) {
	switch {
	case r.PasswordRequired:
		c.ready = false
		if c.phase == connectHostPhase {
			c.phase = connectPasswordPhase
			c.message = ""
			return
		}
		c.message = passwordNotAcceptedLine
	case r.NotListener:
		c.phase = connectHostPhase
		c.ready = false
		c.message = notListenerLine(c.Host, c.Port)
	case r.Err != nil:
		c.ready = false
		c.message = connectErrLine(r.Err, c.Host, c.Port)
	default:
		lines := []string{connectSuccessLine(r.Status, c.Host, c.Port)}
		if w := protocolWarningLine(r.Status); w != "" {
			lines = append(lines, w)
		}
		c.message = strings.Join(lines, "\n")
		c.ready = true
		c.Chosen = true
	}
}

func connectSuccessLine(st listenerapi.Status, host string, port int) string {
	if st.RPC == listenerapi.RPCReachable {
		return fmt.Sprintf("[ok] freecad-mcp %s on %s answers. FreeCAD is running.", st.Version, hostPort(host, port))
	}
	return fmt.Sprintf(
		"[ok] freecad-mcp %s on %s answers. FreeCAD is not running; agents start it with start_freecad.",
		st.Version, hostPort(host, port))
}

// connectErrLine renders a testConnect failure that is neither
// PasswordRequired nor NotListener: a listener that answered but refused
// (marker present, *remote.Error) gets its own reason, not "Could not
// reach", which is reserved for a genuine network failure (nothing
// answered, or something without the marker fell through here).
func connectErrLine(err error, host string, port int) string {
	var refusal *remote.Error
	if errors.As(err, &refusal) {
		return connectRefusalLine(refusal, host)
	}
	return fmt.Sprintf(
		"[fail] Could not reach %s (%v). Check that \"Share this PC\" is on there, that its firewall "+
			"allows freecad-mcp, and that this computer's IP address is in its allowed list.",
		hostPort(host, port), err)
}

// connectRefusalLine renders a listener's own refusal. It matches on
// listenerapi.Error.Reason first (the exact refusal the listener wrote);
// the status code is only a fallback for a reply that somehow carries the
// marker without a Reason.
func connectRefusalLine(e *remote.Error, host string) string {
	reason := e.Body.Reason
	if reason == "" {
		switch e.StatusCode {
		case http.StatusForbidden:
			reason = listenerapi.ReasonRemoteOff
		case http.StatusTooManyRequests:
			reason = listenerapi.ReasonRateLimited
		}
	}
	switch reason {
	case listenerapi.ReasonRemoteOff:
		// Remote access is turned off there; the only 403 a plain status
		// test can reach (a 403 from the Origin check needs a browser).
		return fmt.Sprintf(
			"[fail] Remote access is off on %s. Turn on \"Share this PC\" in freecad-mcp on that computer.", host)
	case listenerapi.ReasonRateLimited:
		wait := 60 // the listener's own default when it sends no Retry-After.
		if e.RetryAfter > 0 {
			wait = e.RetryAfter
		}
		return fmt.Sprintf(
			"[fail] The listener on %s refuses this computer for %d s after too many wrong passwords. "+
				"Check the password, then test again after that.", host, wait)
	default:
		msg := strings.TrimSpace(e.Body.Error)
		if msg == "" {
			msg = e.Error()
		}
		if e.Body.Hint != "" {
			return "[fail] " + msg + " " + e.Body.Hint
		}
		return "[fail] " + msg
	}
}

// testConnectCmd runs testConnect off the UI goroutine.
func testConnectCmd(ctx context.Context, host string, port int, password string) tea.Cmd {
	return func() tea.Msg {
		return connectResultMsg{result: testConnect(ctx, host, port, password)}
	}
}

// trimLastRune drops the last rune of s, for backspace handling. It is the
// one shared copy for package main; app_connect.go and wizard_share.go use
// it too.
func trimLastRune(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}

// hostFieldDisplay is the Host field's display text; focused adds the
// trailing cursor mark (the Port field carries its own, so both are never
// marked at once).
func hostFieldDisplay(host string, focused bool) string {
	if host == "" {
		return strings.Repeat("_", 15)
	}
	if focused {
		return host + "_"
	}
	return host
}

func maskedFieldDisplay(value string) string {
	return maskValue(value) + "_"
}

func (s *connectHostStep) View(state *AppState) string {
	theme := tui.DefaultTheme
	c := &state.Connect
	var b strings.Builder
	switch {
	case c.testing:
		fmt.Fprintf(&b, "  %s Testing the connection to %s...\n", tui.SpinFrame(state.Spinner.Frame), hostPort(c.Host, c.Port))
	case c.phase == connectPasswordPhase:
		fmt.Fprintf(&b, "  %s asks for a password: the one set in \"Share this PC\" on that computer.\n\n", c.Host)
		fmt.Fprintf(&b, "  Password: %s\n", maskedFieldDisplay(c.Password))
		if c.message != "" {
			b.WriteString("\n  " + c.message + "\n")
		}
		passwordEnterLabel := "test again"
		if c.ready {
			passwordEnterLabel = "continue"
		}
		b.WriteString(tui.Footer(theme, tui.Hints(theme,
			tui.Hint{Key: "enter", Label: passwordEnterLabel}, tui.Hint{Key: "esc", Label: "back"})))
	default:
		b.WriteString("  Enter the IP address or host name of the computer that runs FreeCAD. That computer\n" +
			"  needs freecad-mcp installed with \"Share this PC\" turned on.\n\n")
		portText := fmt.Sprintf("%d", c.Port)
		if c.portFocus {
			portText += "_"
		}
		fmt.Fprintf(&b, "  Host: %s     Port: %s\n", hostFieldDisplay(c.Host, !c.portFocus), portText)
		if c.message != "" {
			b.WriteString("\n")
			for _, line := range strings.Split(c.message, "\n") {
				b.WriteString("  " + line + "\n")
			}
		}
		enterLabel := "test connection"
		switch {
		case c.ready:
			enterLabel = "continue"
		case c.message != "":
			enterLabel = "retry"
		}
		b.WriteString(tui.Footer(theme, tui.Hints(theme,
			tui.Hint{Key: "tab", Label: "switch field"}, tui.Hint{Key: "enter", Label: enterLabel}, tui.Hint{Key: "esc", Label: "back"})))
	}
	return tui.Section(theme, s.Title(state), b.String())
}

// applyConnectChoice saves the host, port and password of FreeCAD on
// another computer into the credential store (domain.HostKey, PortKey,
// TokenKey), printing one [ok] or [fail] line, when that was chosen. It
// returns an exit code.
func applyConnectChoice(ctx context.Context, w io.Writer, state *AppState, dryRun bool) int {
	c := &state.Connect
	if !c.Chosen {
		return 0
	}
	if dryRun {
		fmt.Fprintf(w, "  would save: agents on this computer use FreeCAD on %s\n", hostPort(c.Host, c.Port))
		return 0
	}
	if err := saveConnection(ctx, c.Host, c.Port, c.Password); err != nil {
		fmt.Fprintf(w, "  [fail] save the connection to %s: %v\n", hostPort(c.Host, c.Port), err)
		return 1
	}
	fmt.Fprintf(w, "  [ok] Saved: agents on this computer use FreeCAD on %s.\n", hostPort(c.Host, c.Port))
	return 0
}

// installFirstText is the block runWizard prints after the wizard closed
// on "Install FreeCAD on this computer first".
func installFirstText() string {
	command := strings.TrimSpace(os.Getenv(domain.EnvInstallCommand))
	if command == "" {
		command = domain.BinaryName + " install"
	}
	return "  Install FreeCAD 1.0 or newer from https://www.freecad.org/downloads.php and start it once.\n" +
		"  Then run the same install command again:\n\n" +
		"    " + command + "\n\n" +
		"  Nothing was changed on this computer."
}

package main

// The app's "Connect to FreeCAD on another computer" page: host, port and
// password, an async Test connection and Save, and "use this computer
// instead" (which removes the saved connection), each reporting through the
// shared remote.go logic (testConnect, saveConnection, clearConnection).
//
// Fields are plain strings edited with backspace and typed runes, the same
// hand-rolled key handling as app_share.go and wizard_connect.go's host and
// password screens (connectSuccessLine and connectErrLine are reused from
// wizard_connect.go, same package). This avoids bubbles/textinput, which
// pulls in github.com/atotto/clipboard, a dependency go.sum does not have
// and this page does not add.
//
// t, s and d act as shortcuts only while no field is focused
// (p.focus == connectFieldNone); with a field focused, every printable key
// (including those letters, space, "y" and "n") types into it instead, so a
// host or password containing one of them can still be typed. Tab starts
// editing at the Host field; esc leaves editing without leaving the page, a
// second esc then leaves it. Footer: "tab edit fields   t test connection
// s save   d use this computer instead   esc back" with no field focused,
// "tab next field   esc stop editing" while editing one.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

// connectField values are the tab order of this page's fields.
const (
	connectFieldHost = iota
	connectFieldPort
	connectFieldPassword
	connectFieldCount
)

// connectFieldNone is p.focus while no field is being edited: t, s and d
// then act as the page's shortcuts. Tab from here starts editing the Host
// field; esc from a field returns here without leaving the page (a second
// esc then leaves it). While a field is focused every printable key,
// including "t", "s", "d" and space, types into it instead of triggering an
// action, so a password (or host) containing one of those can still be typed.
const connectFieldNone = -1

// connectPage is the Connect page's state. Host, port and password start
// from the stored connection (loadCredential), if any, and are written only
// on Save (through remote.go's saveConnection); nothing is written while the
// page is only being edited or tested.
type connectPage struct {
	ctx context.Context

	host     string
	port     string
	password string
	focus    int

	testing bool
	saving  bool
	// clearing is true while "use this computer instead" is removing the
	// stored connection, after the y/n confirmation.
	clearing        bool
	confirmingClear bool
	spinnerFrame    int

	resultLines []string
}

func newConnectPage(ctx context.Context) appPage {
	p := &connectPage{ctx: ctx, port: strconv.Itoa(domain.DefaultListenerPort), focus: connectFieldNone}

	if token, host, port, err := loadCredentials(ctx); err == nil && host != "" {
		p.host = host
		if port != "" {
			p.port = port
		}
		// The stored password belongs to this stored connection; without a
		// host it would be this computer's own "Share this PC" password
		// (or none), which does not belong on another computer's field.
		if token != "" {
			p.password = token
		}
	}

	return p
}

func (p *connectPage) Init() tea.Cmd { return nil }

// connectTestDoneMsg carries a Test connection result.
type connectTestDoneMsg struct{ lines []string }

// connectSaveDoneMsg carries a Save result.
type connectSaveDoneMsg struct{ err error }

// connectClearDoneMsg carries a "use this computer instead" result.
type connectClearDoneMsg struct{ err error }

func (p *connectPage) busy() bool { return p.testing || p.saving || p.clearing }

func (p *connectPage) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch m := msg.(type) {
	case connectTestDoneMsg:
		p.testing = false
		p.resultLines = m.lines
		return false, nil
	case connectSaveDoneMsg:
		p.saving = false
		if m.err != nil {
			p.resultLines = []string{"[fail] " + m.err.Error()}
		} else {
			p.resultLines = []string{"Saved. AI clients use it the next time they start freecad-mcp (restart them)."}
		}
		return false, nil
	case connectClearDoneMsg:
		p.clearing = false
		if m.err != nil {
			p.resultLines = []string{"[fail] " + m.err.Error()}
		} else {
			p.host = ""
			p.port = strconv.Itoa(domain.DefaultListenerPort)
			p.password = ""
			p.resultLines = []string{"Removed. AI clients use FreeCAD on this computer the next time they start freecad-mcp."}
		}
		return false, nil
	}

	if tui.IsSpinMsg(msg) {
		if p.testing || p.saving || p.clearing {
			p.spinnerFrame++
			return false, tui.Spinner()
		}
		return false, nil
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false, nil
	}

	if p.confirmingClear {
		switch key.String() {
		case "y":
			p.confirmingClear = false
			p.clearing = true
			p.resultLines = nil
			return false, p.startClear()
		case "n", "esc":
			p.confirmingClear = false
		}
		return false, nil
	}

	if p.busy() {
		return false, nil
	}

	// While a field is focused, every printable key (including "t", "s",
	// "d" and space) types into it; only tab and esc are reserved, so a
	// host or password that itself contains one of the shortcut letters can
	// still be typed. t/s/d act as shortcuts only with no field focused.
	if p.focus != connectFieldNone {
		switch key.String() {
		case "esc":
			p.focus = connectFieldNone
		case "tab":
			p.focus = (p.focus + 1) % connectFieldCount
		case "backspace":
			p.editField(func(s string) string {
				r := []rune(s)
				if len(r) == 0 {
					return s
				}
				return string(r[:len(r)-1])
			})
			if p.focus == connectFieldHost {
				// The prefilled password belongs to the host that was
				// loaded, not whatever the user is now editing it into.
				p.password = ""
			}
		default:
			if len(key.Runes) > 0 {
				text := string(key.Runes)
				p.editField(func(s string) string { return s + digitsOnlyIfPort(p.focus, text) })
				if p.focus == connectFieldHost {
					p.password = ""
				}
			}
		}
		return false, nil
	}

	switch key.String() {
	case "esc":
		return true, nil
	case "tab":
		p.focus = connectFieldHost
	case "t":
		return false, p.startTest()
	case "s":
		return false, p.startSave()
	case "d":
		p.confirmingClear = true
		p.resultLines = nil
	}
	return false, nil
}

// editField applies edit to the currently focused field.
func (p *connectPage) editField(edit func(string) string) {
	switch p.focus {
	case connectFieldHost:
		p.host = edit(p.host)
	case connectFieldPort:
		p.port = edit(p.port)
	case connectFieldPassword:
		p.password = edit(p.password)
	}
}

// digitsOnlyIfPort keeps only decimal digits of text when field is the port
// field; every other field accepts any typed text unchanged.
func digitsOnlyIfPort(field int, text string) string {
	if field != connectFieldPort {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// connectPageFields reads and validates the current host and port, leaving a
// fail line in resultLines and returning ok false when either is invalid.
func (p *connectPage) connectPageFields() (host string, port int, password string, ok bool) {
	host = strings.TrimSpace(p.host)
	if host == "" {
		p.resultLines = []string{"[fail] Enter a host first."}
		return "", 0, "", false
	}
	if err := domain.ValidateHost(host); err != nil {
		p.resultLines = []string{"[fail] " + err.Error()}
		return "", 0, "", false
	}
	port, err := validatePort(p.port)
	if err != nil {
		p.resultLines = []string{"[fail] " + err.Error()}
		return "", 0, "", false
	}
	if err := validatePassword(p.password); err != nil {
		p.resultLines = []string{"[fail] " + err.Error()}
		return "", 0, "", false
	}
	return host, port, p.password, true
}

func (p *connectPage) startTest() tea.Cmd {
	host, port, password, ok := p.connectPageFields()
	if !ok {
		return nil
	}
	p.testing = true
	p.spinnerFrame = 0
	p.resultLines = nil
	ctx := p.ctx
	return tea.Batch(tui.Spinner(), func() tea.Msg {
		res := testConnect(ctx, host, port, password)
		return connectTestDoneMsg{lines: formatConnectTest(res, host, port)}
	})
}

func (p *connectPage) startSave() tea.Cmd {
	host, port, password, ok := p.connectPageFields()
	if !ok {
		return nil
	}
	p.saving = true
	p.spinnerFrame = 0
	p.resultLines = nil
	ctx := p.ctx
	return tea.Batch(tui.Spinner(), func() tea.Msg {
		return connectSaveDoneMsg{err: saveConnection(ctx, host, port, password)}
	})
}

func (p *connectPage) startClear() tea.Cmd {
	p.spinnerFrame = 0
	ctx := p.ctx
	return tea.Batch(tui.Spinner(), func() tea.Msg {
		return connectClearDoneMsg{err: clearConnection(ctx)}
	})
}

// formatConnectTest turns a testConnect result into the page's result
// lines: a 200 (plus a protocol warning when the two versions differ), a
// simplified line asking for a password, "not a listener", or a failure
// (connectErrLine, shared with wizard_connect.go, tells a network failure
// from a listener's own refusal).
func formatConnectTest(res connectResult, host string, port int) []string {
	switch {
	case res.PasswordRequired:
		return []string{"That computer asks for a password. Enter it and test again."}
	case res.NotListener:
		return []string{notListenerLine(host, port)}
	case res.Err != nil:
		return []string{connectErrLine(res.Err, host, port)}
	default:
		lines := []string{connectSuccessLine(res.Status, host, port)}
		if w := protocolWarningLine(res.Status); w != "" {
			lines = append(lines, w)
		}
		return lines
	}
}

// connectRowPrefix marks the focused field's row with "> " in place of the
// usual two-space indent (as app_share.go's rowPrefix does).
func (p *connectPage) connectRowPrefix(field int) string {
	if p.focus == field {
		return "> "
	}
	return "  "
}

// connectFieldDisplay is a field's display text: its value with a trailing
// cursor mark while focused and non-empty, or the placeholder unchanged
// while empty (focused or not), so the page starts out showing a plain
// blank field rather than a stray cursor mark.
func connectFieldDisplay(value, placeholder string, focused bool) string {
	if value == "" {
		return placeholder
	}
	if focused {
		return value + "_"
	}
	return value
}

func (p *connectPage) View() string {
	theme := tui.DefaultTheme
	var b strings.Builder

	b.WriteString("  Use FreeCAD that runs on another computer. That computer needs freecad-mcp with\n")
	b.WriteString("  \"Share this PC\" turned on.\n\n")

	hostText := connectFieldDisplay(p.host, strings.Repeat("_", 15), p.focus == connectFieldHost)
	fmt.Fprintf(&b, "%s%-11s%s\n", p.connectRowPrefix(connectFieldHost), "Host:", hostText)

	portText := connectFieldDisplay(p.port, strings.Repeat("_", 15), p.focus == connectFieldPort)
	fmt.Fprintf(&b, "%s%-11s%s\n", p.connectRowPrefix(connectFieldPort), "Port:", portText)

	passwordText := connectFieldDisplay(maskValue(p.password), strings.Repeat("_", 8), p.focus == connectFieldPassword)
	fmt.Fprintf(&b, "%s%-11s%s  (only if that computer set one)\n",
		p.connectRowPrefix(connectFieldPassword), "Password:", passwordText)

	switch {
	case p.confirmingClear:
		b.WriteString("\n  Use FreeCAD on this computer instead? The saved host, port and password are removed. (y/n)\n")
	case p.testing:
		where := p.host + ":" + p.port
		if port, err := strconv.Atoi(p.port); err == nil {
			where = hostPort(p.host, port)
		}
		fmt.Fprintf(&b, "\n  %s Testing the connection to %s...\n", tui.SpinFrame(p.spinnerFrame), where)
	case p.saving:
		fmt.Fprintf(&b, "\n  %s Saving...\n", tui.SpinFrame(p.spinnerFrame))
	case p.clearing:
		fmt.Fprintf(&b, "\n  %s Removing...\n", tui.SpinFrame(p.spinnerFrame))
	case len(p.resultLines) > 0:
		b.WriteString("\n")
		for _, l := range p.resultLines {
			b.WriteString("  " + l + "\n")
		}
	}

	if p.focus != connectFieldNone {
		b.WriteString(tui.Footer(theme, tui.Hints(theme,
			tui.Hint{Key: "tab", Label: "next field"},
			tui.Hint{Key: "esc", Label: "stop editing"},
		)))
	} else if !p.confirmingClear {
		b.WriteString(tui.Footer(theme, tui.Hints(theme,
			tui.Hint{Key: "tab", Label: "edit fields"},
			tui.Hint{Key: "t", Label: "test connection"},
			tui.Hint{Key: "s", Label: "save"},
			tui.Hint{Key: "d", Label: "use this computer instead"},
			tui.Hint{Key: "esc", Label: "back"},
		)))
	}

	return tui.Section(theme, "Connect to FreeCAD on another computer", b.String())
}

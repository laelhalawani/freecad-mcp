package main

// The app's "Share this PC (remote access)" page: remote access on or off,
// allowed IPs, password, port and session timeout, an async Test connection
// and Save, each reporting through the shared remote.go logic (applyShare,
// applyUnshare, testShare). Test connection only proves the port answers
// from this computer; it says so, and points to the Connect page for a real
// test from another device.

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/async"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
)

// sharePhase is the page's loading state: sharePhaseLoading while the
// installed addon and its current settings are located, sharePhaseForm once
// the fields hold a starting value.
type sharePhase int

const (
	sharePhaseLoading sharePhase = iota
	sharePhaseForm
)

// shareFields are the tab order of the page's fields; 0 is the checkbox.
const (
	shareFieldEnabled = iota
	shareFieldAllowedIPs
	shareFieldPassword
	shareFieldPort
	shareFieldMinutes
	shareFieldCount
)

// shareFieldNone is p.focus while no field is being edited: t and s then act
// as the page's shortcuts, the same model as the Connect page (app_connect.go).
// Tab from here starts editing at the checkbox; esc from a field returns
// here without leaving the page (a second esc then leaves it). This is the
// fix for a password (or allowed IP list) containing "t" or "s": while a
// field is focused every printable key, including those letters and space,
// types into it instead of triggering an action.
const shareFieldNone = -1

// shareReport is the text applyShare or applyUnshare wrote, kept as its own
// type so its async.Result does not collide with the app's own
// async.Result[string] (used by the doctor and connection reports).
type shareReport string

// shareLoad is the page's starting state: the installed addon copies and the
// remote access settings of the first one, or the defaults when none is
// installed. loadErr is set when the settings file exists but could not be
// read or parsed: the page then shows it instead of silently offering to
// save over whatever the file actually holds.
type shareLoad struct {
	targets  []addoninstall.Target
	settings addoninstall.RemoteSettings
	loadErr  error
}

type sharePage struct {
	ctx context.Context

	phase        sharePhase
	spinnerFrame int
	targets      []addoninstall.Target
	// loadError is set when the settings file could not be read or parsed;
	// Test and Save both refuse while it is set, rather than acting on the
	// defaults the form was left showing.
	loadError error

	enabled     bool
	allowedIPs  string
	password    string
	portText    string
	minutesText string
	focus       int

	testing   bool
	testLines []shareTestLine

	saving     bool
	saveOutput string
}

func newSharePage(ctx context.Context) appPage { return &sharePage{ctx: ctx} }

func (p *sharePage) Init() tea.Cmd {
	p.phase = sharePhaseLoading
	ctx := p.ctx
	return tea.Batch(tui.Spinner(), async.Load(func() (shareLoad, error) {
		targets := addoninstall.LocateInstalled(ctx, freecadCommand())
		settings := addoninstall.DefaultRemoteSettings()
		var loadErr error
		if len(targets) > 0 {
			s, err := addoninstall.ReadRemoteSettings(targets[0])
			if err != nil {
				loadErr = err
			} else {
				settings = s
			}
		}
		return shareLoad{targets: targets, settings: settings, loadErr: loadErr}, nil
	}))
}

func (p *sharePage) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch m := msg.(type) {
	case async.Result[shareLoad]:
		p.targets = m.Value.targets
		p.loadError = m.Value.loadErr
		s := m.Value.settings
		p.enabled = s.RemoteEnabled
		p.allowedIPs = s.AllowedIPs
		if !s.RemoteEnabled {
			// Not shared yet: suggest a subnet. Once remote access is on,
			// the saved list is kept as is (including the loopback-only
			// SSH-tunnel setup, "127.0.0.1").
			if subnets := suggestedAllowedIPs(); len(subnets) > 0 {
				p.allowedIPs = strings.Join(subnets, ", ")
			}
		}
		p.password = s.AuthToken
		p.portText = strconv.Itoa(s.ListenerPort)
		p.minutesText = strconv.Itoa(s.SessionTimeoutMinutes)
		p.phase = sharePhaseForm
		p.focus = shareFieldNone
		return false, nil

	case async.Result[[]shareTestLine]:
		p.testing = false
		p.testLines = m.Value
		return false, nil

	case async.Result[shareReport]:
		p.saving = false
		p.saveOutput = string(m.Value)
		return false, nil

	case tea.KeyMsg:
		if p.phase == sharePhaseLoading {
			if m.String() == "esc" {
				return true, nil
			}
			return false, nil
		}
		if p.testing || p.saving {
			return false, nil
		}
		// t and s act as shortcuts only with no field focused; a field
		// focused reserves only tab and esc, so a password (or allowed IP
		// list) containing "t" or "s" can still be typed (same model as the
		// Connect page, app_connect.go).
		if p.focus == shareFieldNone {
			switch m.String() {
			case "esc":
				return true, nil
			case "tab":
				p.focus = shareFieldEnabled
			case "t":
				return false, p.startTest()
			case "s":
				return false, p.startSave()
			}
			return false, nil
		}
		switch m.String() {
		case "esc":
			p.focus = shareFieldNone
			return false, nil
		case "tab":
			p.focus = (p.focus + 1) % shareFieldCount
			return false, nil
		case "shift+tab":
			p.focus = (p.focus - 1 + shareFieldCount) % shareFieldCount
			return false, nil
		}
		if p.focus == shareFieldEnabled {
			if m.Type == tea.KeySpace {
				p.enabled = !p.enabled
			}
			return false, nil
		}
		ptr := p.fieldValue(p.focus)
		if ptr == nil {
			return false, nil
		}
		numeric := p.focus == shareFieldPort || p.focus == shareFieldMinutes
		switch m.Type {
		case tea.KeyBackspace:
			if r := []rune(*ptr); len(r) > 0 {
				*ptr = string(r[:len(r)-1])
			}
		case tea.KeySpace:
			if !numeric {
				*ptr += " "
			}
		case tea.KeyRunes:
			for _, r := range m.Runes {
				if numeric && (r < '0' || r > '9') {
					continue
				}
				*ptr += string(r)
			}
		}
		return false, nil
	}

	if tui.IsSpinMsg(msg) {
		if p.phase == sharePhaseLoading || p.testing || p.saving {
			p.spinnerFrame++
			return false, tui.Spinner()
		}
	}
	return false, nil
}

// fieldValue returns a pointer to the editable text of field, or nil for the
// checkbox, which has no text.
func (p *sharePage) fieldValue(field int) *string {
	switch field {
	case shareFieldAllowedIPs:
		return &p.allowedIPs
	case shareFieldPassword:
		return &p.password
	case shareFieldPort:
		return &p.portText
	case shareFieldMinutes:
		return &p.minutesText
	}
	return nil
}

// options builds shareOptions from the fields on screen, or an error naming
// the first field that does not parse (the wizard's own wording, so the two
// pages agree): a port or timeout out of range is refused, not silently
// replaced with the default.
func (p *sharePage) options() (shareOptions, error) {
	if p.loadError != nil {
		return shareOptions{}, p.loadError
	}
	port, err := validatePort(p.portText)
	if err != nil {
		return shareOptions{}, err
	}
	minutes, err := validateMinutes(p.minutesText)
	if err != nil {
		return shareOptions{}, err
	}
	if err := validatePassword(p.password); err != nil {
		return shareOptions{}, err
	}
	return shareOptions{
		AllowedIPs:     p.allowedIPs,
		Password:       p.password,
		TimeoutMinutes: minutes,
		Port:           port,
	}, nil
}

// startTest runs testShare with the values on screen; nothing is saved.
func (p *sharePage) startTest() tea.Cmd {
	opts, err := p.options()
	if err != nil {
		p.testLines = []shareTestLine{{Text: "[fail] " + err.Error()}}
		return nil
	}
	ctx := p.ctx
	p.testing = true
	p.testLines = nil
	return tea.Batch(tui.Spinner(), async.Load(func() ([]shareTestLine, error) {
		return testShare(ctx, opts), nil
	}))
}

// startSave applies the fields on screen: share on through applyShare, or
// share off through applyUnshare when the checkbox is cleared.
func (p *sharePage) startSave() tea.Cmd {
	if p.loadError != nil {
		p.saveOutput = "  [fail] " + p.loadError.Error() + "\n"
		return nil
	}
	opts, err := p.options()
	if err != nil && p.enabled {
		p.saveOutput = "  [fail] " + err.Error() + "\n"
		return nil
	}
	ctx := p.ctx
	targets := p.targets
	enabled := p.enabled
	p.saving = true
	p.saveOutput = ""
	return tea.Batch(tui.Spinner(), async.Load(func() (shareReport, error) {
		var buf bytes.Buffer
		switch {
		case len(targets) == 0:
			buf.WriteString("  No installed FreeCAD addon was found; install it first (menu > Install or update the FreeCAD addon).\n")
		case enabled:
			applyShare(ctx, &buf, targets, opts)
		default:
			applyUnshare(ctx, &buf, targets)
		}
		return shareReport(buf.String()), nil
	}))
}

func (p *sharePage) View() string {
	theme := tui.DefaultTheme
	var b strings.Builder

	if p.phase == sharePhaseLoading {
		fmt.Fprintf(&b, "  %s Looking for the installed FreeCAD addon...\n", tui.SpinFrame(p.spinnerFrame))
		return tui.Section(theme, "Share this PC (remote access)", b.String())
	}

	mark := "[ ]"
	if p.enabled {
		mark = "[x]"
	}

	if p.loadError != nil {
		fmt.Fprintf(&b, "  [fail] %v\n\n", p.loadError)
	}

	b.WriteString("  Agents on other devices can use FreeCAD on this computer once freecad-mcp is\n")
	b.WriteString("  installed on each of them too (every station installs it separately).\n\n")
	b.WriteString("  The running FreeCAD server can be secured with a password set here; the FreeCAD\n")
	b.WriteString("  addon uses the same one. Agents on this PC get it automatically; on other devices\n")
	b.WriteString("  you type it when connecting. While remote access is off it is fine to leave it\n")
	b.WriteString("  empty.\n\n")

	b.WriteString(p.rowPrefix(shareFieldEnabled) + mark + " Allow other devices to use FreeCAD on this computer\n")
	b.WriteString(p.rowPrefix(shareFieldAllowedIPs) + fmt.Sprintf("%-14s%s\n", "Allowed IPs:", p.allowedIPs))
	b.WriteString(p.rowPrefix(shareFieldPassword) + fmt.Sprintf("%-14s%s  (optional)\n", "Password:", maskValue(p.password)))
	b.WriteString(p.rowPrefix(shareFieldPort) + fmt.Sprintf("%-14s%s\n", "Port:", p.portText))
	b.WriteString(p.rowPrefix(shareFieldMinutes) + "How long to keep the session assigned to an agent? (minutes): " + p.minutesText + "\n")
	b.WriteString("    With remote access on, several agents may try to use FreeCAD. The session lock\n")
	b.WriteString("    stops one agent from taking control while another works. Agents can free it\n")
	b.WriteString("    themselves; after this long without activity it becomes available to another\n")
	b.WriteString("    agent.\n\n")

	b.WriteString("  Connections are not encrypted; on an untrusted network use an SSH tunnel\n")
	b.WriteString("  (" + remoteAccessDocsURL + ").\n\n")

	switch {
	case p.testing:
		fmt.Fprintf(&b, "  %s Testing connection...\n\n", tui.SpinFrame(p.spinnerFrame))
	case len(p.testLines) > 0:
		for _, line := range p.testLines {
			fmt.Fprintf(&b, "  %s\n", line.Text)
		}
		b.WriteString("\n")
	}

	switch {
	case p.saving:
		fmt.Fprintf(&b, "  %s Saving...\n\n", tui.SpinFrame(p.spinnerFrame))
	case p.saveOutput != "":
		b.WriteString(p.saveOutput)
		if !strings.HasSuffix(p.saveOutput, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if p.focus != shareFieldNone {
		b.WriteString(tui.Footer(theme, tui.Hints(theme,
			tui.Hint{Key: "tab", Label: "next field"},
			tui.Hint{Key: "space", Label: "toggle (on the checkbox)"},
			tui.Hint{Key: "esc", Label: "stop editing"},
		)))
	} else {
		b.WriteString(tui.Footer(theme, tui.Hints(theme,
			tui.Hint{Key: "tab", Label: "edit fields"},
			tui.Hint{Key: "t", Label: "test connection"},
			tui.Hint{Key: "s", Label: "save"},
			tui.Hint{Key: "esc", Label: "back"},
		)))
	}
	return tui.Section(theme, "Share this PC (remote access)", b.String())
}

// rowPrefix marks the focused field's row with "> " in place of the usual
// two-space indent.
func (p *sharePage) rowPrefix(field int) string {
	if p.focus == field {
		return "> "
	}
	return "  "
}

package main

// The install wizard's "Share this PC with other devices?" steps, shown
// after the addon step when FreeCAD was found: allowed IPs, password and
// session timeout. They are applied after registration by applyShareChoice
// (settings and credentials) and applyShareListener (autostart and start),
// in the order finishWizard calls them: the share settings, then the addon,
// then the listener, so the listener only starts once the addon it talks to
// is in place.
//
// applyShareChoice and applyShareListener call remote.go's own
// applyShareSettings/applyUnshareSettings and applyShareListenerRegistration
// directly, rather than its applyShare/applyUnshare (which bundle the
// settings and the listener step together for the `share` one-shot command,
// which has no addon-install step to sequence around), so the wizard and the
// command write and report every step identically while still installing
// the addon between the settings and the listener.

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/domain"
)

// shareState is the "Share this PC" choice. The password is held in memory
// only, until applyShareChoice.
type shareState struct {
	// Asked is set once the share question was answered; nothing is applied
	// without it.
	Asked bool
	// Initial is remote_enabled as the wizard found it, where the choice
	// starts; On is the answer. On false while Initial is true turns remote
	// access off and removes the listener.
	Initial        bool
	On             bool
	AllowedIPs     string
	Password       string
	TimeoutMinutes int
	Port           int

	// The rest is the steps' own view state; it does not survive past the
	// wizard.
	//
	// primed marks that Initial, On, AllowedIPs, TimeoutMinutes, Port and
	// existingPassword were loaded from the addon settings and the LAN
	// subnets, so revisiting the choice screen (esc from a later screen,
	// then forward again) does not discard what the user already chose.
	primed bool
	// existingPassword is the password already set (Initial true and it was
	// not empty); the password screen shows it as "a password is set" and
	// keeps it when Password is left empty, rather than removing it.
	existingPassword string
	// minutesText is the minutes screen's own editing buffer; TimeoutMinutes
	// is only updated once it parses as a valid value.
	minutesText string
	// message is the current validation error on the allowed IPs or minutes
	// screen, or "" before one.
	message string
	// readErr is set when the addon settings could not be read (message then
	// holds the error and recovery guidance): the choice screen refuses Yes
	// while it is set, so enter cannot save the defaults over a file that
	// failed to read for some other reason (a hand-edited auth_token, for
	// example) — see shareChoiceStep.Update.
	readErr bool
}

// shareSkipMsg is emitted by a share step's Init when it does not apply, so
// Update can return flow.Skip (the flow.Step interface has no Init
// directive of its own).
type shareSkipMsg struct{}

// shareSteps are the steps after the addon step (and, when FreeCAD was not
// found, after connectSteps) when FreeCAD was found; each skips itself
// otherwise.
func shareSteps(ctx context.Context) []flow.Step[AppState] {
	return []flow.Step[AppState]{
		&shareChoiceStep{},
		&shareAllowedIPsStep{},
		&sharePasswordStep{},
		&shareMinutesStep{},
	}
}

// --- Share this PC with other devices? ---

type shareChoiceStep struct{}

func (s *shareChoiceStep) ID() string { return "share-choice" }

func (s *shareChoiceStep) Title(*AppState) string { return "Share this PC" }

func (s *shareChoiceStep) Hints(*AppState) []struct{ Key, Label string } {
	return []struct{ Key, Label string }{{"↑↓", "move"}, {"enter", "continue"}, {"esc", "back"}, {"q", "cancel"}}
}

func (s *shareChoiceStep) Init(state *AppState) tea.Cmd {
	if state.Addon.Phase != addonChoosing {
		return func() tea.Msg { return shareSkipMsg{} }
	}
	sh := &state.Share
	if !sh.primed {
		settings := addoninstall.DefaultRemoteSettings()
		if len(state.Addon.Targets) > 0 {
			v, err := addoninstall.ReadRemoteSettings(state.Addon.Targets[0])
			if err != nil {
				// Shown on this screen (below) instead of silently priming
				// from the defaults, which would offer to save over
				// whatever the file actually holds. readErr also blocks Yes
				// (see Update), so enter cannot write those defaults either.
				sh.message = "[fail] " + err.Error() +
					"; fix or delete the file, or open `freecad-mcp` > Share this PC to change these settings."
				sh.readErr = true
			} else {
				settings = v
			}
		}
		sh.Initial = settings.RemoteEnabled
		sh.On = settings.RemoteEnabled
		if settings.AllowedIPs != "" && !(settings.AllowedIPs == addoninstall.DefaultAllowedIPs && !sh.Initial) {
			// Keep the saved list unless there is nothing deliberately
			// chosen to keep: never saved, or nothing but the plain default
			// while remote access is off (still a first time, not a real
			// choice, live check L8). While remote access is already on,
			// a saved plain default is instead the loopback-only SSH-tunnel
			// setup (contract 5.1), chosen on purpose: replacing it with a
			// LAN suggestion just because this screen was revisited would
			// expose that tunnel-only setup to the whole LAN (live-fixes
			// review M2). A list other than the plain default is kept
			// either way, so an off-then-on cycle never drops a custom one
			// such as a WSL range added next to a LAN subnet.
			sh.AllowedIPs = settings.AllowedIPs
		} else {
			sh.AllowedIPs = strings.Join(suggestedAllowedIPs(), ", ")
		}
		if sh.AllowedIPs == "" {
			sh.AllowedIPs = addoninstall.DefaultAllowedIPs
		}
		// A password already set is kept unless the user types a new one,
		// whether or not remote access happens to be on right now: the
		// Share page already allows leaving remote access off with a
		// password saved for later, and turning sharing back on here must
		// not silently drop it.
		sh.existingPassword = settings.AuthToken
		sh.TimeoutMinutes = settings.SessionTimeoutMinutes
		sh.Port = settings.ListenerPort
		sh.primed = true
	}
	return nil
}

func (s *shareChoiceStep) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	sh := &state.Share
	switch m := msg.(type) {
	case shareSkipMsg:
		return flow.Skip, nil
	case tea.KeyMsg:
		switch m.String() {
		case "q", "ctrl+c":
			return flow.Quit, nil
		case "esc":
			state.Retreating = true
			sh.Password = ""
			return flow.Back, nil
		case "up", "down", "k", "j", " ", "space", "left", "right":
			sh.On = !sh.On
		case "enter":
			// While the settings file failed to read, Yes is refused: Asked
			// would otherwise let applyShareChoice write the (unrelated)
			// defaults over a file that failed to read for some other
			// reason. No changes nothing when Initial is already false, so
			// it may still proceed.
			if sh.readErr && sh.On {
				return flow.Continue, nil
			}
			sh.Asked = true
			return flow.Next, nil
		}
	}
	return flow.Continue, nil
}

func (s *shareChoiceStep) View(state *AppState) string {
	theme := tui.DefaultTheme
	sh := &state.Share
	var b strings.Builder
	b.WriteString("  Share this PC with other devices?\n\n")
	b.WriteString("  Agents on other devices can then use FreeCAD on this computer, once freecad-mcp is\n")
	b.WriteString("  installed on each of them too (every station installs it separately). A small\n")
	b.WriteString("  listener starts with this computer and opens FreeCAD when a remote agent asks.\n\n")
	if sh.On {
		b.WriteString("    No, only agents on this computer use FreeCAD\n")
		b.WriteString("  > Yes, share this PC with other devices\n")
	} else {
		b.WriteString("  > No, only agents on this computer use FreeCAD\n")
		b.WriteString("    Yes, share this PC with other devices\n")
	}
	if sh.message != "" {
		b.WriteString("\n  " + sh.message + "\n")
	}
	b.WriteString(tui.Footer(theme, tui.Hints(theme,
		tui.Hint{Key: "↑↓", Label: "move"}, tui.Hint{Key: "enter", Label: "continue"},
		tui.Hint{Key: "esc", Label: "back"}, tui.Hint{Key: "q", Label: "cancel"})))
	return tui.Section(theme, s.Title(state), b.String())
}

// --- Allowed IPs ---

type shareAllowedIPsStep struct{}

func (s *shareAllowedIPsStep) ID() string { return "share-allowed-ips" }

func (s *shareAllowedIPsStep) Title(*AppState) string { return "Share this PC" }

func (s *shareAllowedIPsStep) Hints(*AppState) []struct{ Key, Label string } {
	return []struct{ Key, Label string }{{"enter", "continue"}, {"esc", "back"}}
}

func (s *shareAllowedIPsStep) Init(state *AppState) tea.Cmd {
	if !state.Share.On {
		return func() tea.Msg { return shareSkipMsg{} }
	}
	state.Share.message = ""
	return nil
}

func (s *shareAllowedIPsStep) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	if _, ok := msg.(shareSkipMsg); ok {
		return flow.Skip, nil
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return flow.Continue, nil
	}
	sh := &state.Share
	switch k.String() {
	case "ctrl+c":
		return flow.Quit, nil
	case "esc":
		state.Retreating = true
		return flow.Back, nil
	case "enter":
		if _, err := domain.ParseAllowedIPs(sh.AllowedIPs); err != nil {
			sh.message = err.Error()
			return flow.Continue, nil
		}
		sh.message = ""
		return flow.Next, nil
	case "backspace":
		sh.AllowedIPs = trimLastRune(sh.AllowedIPs)
		sh.message = ""
	default:
		if len(k.Runes) > 0 {
			sh.AllowedIPs += string(k.Runes)
			sh.message = ""
		}
	}
	return flow.Continue, nil
}

func (s *shareAllowedIPsStep) View(state *AppState) string {
	theme := tui.DefaultTheme
	sh := &state.Share
	var b strings.Builder
	b.WriteString("  Which devices may connect? IP addresses or subnets, comma-separated.\n")
	b.WriteString("  Example: 192.168.1.0/24, 10.0.0.5\n\n")
	fmt.Fprintf(&b, "  Allowed IPs: %s_\n", sh.AllowedIPs)
	if sh.message != "" {
		b.WriteString("\n  " + sh.message + "\n")
	}
	b.WriteString(tui.Footer(theme, tui.Hints(theme,
		tui.Hint{Key: "enter", Label: "continue"}, tui.Hint{Key: "esc", Label: "back"})))
	return tui.Section(theme, s.Title(state), b.String())
}

// --- Password ---

type sharePasswordStep struct{}

func (s *sharePasswordStep) ID() string { return "share-password" }

func (s *sharePasswordStep) Title(*AppState) string { return "Share this PC" }

func (s *sharePasswordStep) Hints(*AppState) []struct{ Key, Label string } {
	return []struct{ Key, Label string }{{"enter", "continue"}, {"esc", "back"}}
}

func (s *sharePasswordStep) Init(state *AppState) tea.Cmd {
	if !state.Share.On {
		return func() tea.Msg { return shareSkipMsg{} }
	}
	return nil
}

func (s *sharePasswordStep) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	if _, ok := msg.(shareSkipMsg); ok {
		return flow.Skip, nil
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return flow.Continue, nil
	}
	sh := &state.Share
	switch k.String() {
	case "ctrl+c":
		return flow.Quit, nil
	case "esc":
		state.Retreating = true
		return flow.Back, nil
	case "enter":
		if err := validatePassword(sh.Password); err != nil {
			sh.message = err.Error()
			return flow.Continue, nil
		}
		sh.message = ""
		return flow.Next, nil
	case "backspace":
		sh.Password = trimLastRune(sh.Password)
		sh.message = ""
	default:
		if len(k.Runes) > 0 {
			sh.Password += string(k.Runes)
			sh.message = ""
		}
	}
	return flow.Continue, nil
}

func (s *sharePasswordStep) View(state *AppState) string {
	theme := tui.DefaultTheme
	sh := &state.Share
	var b strings.Builder
	if sh.existingPassword != "" {
		b.WriteString("  Password other devices must give. Leave it empty to keep the current one; remove it\n")
		b.WriteString("  on the Share this PC page.\n")
	} else {
		b.WriteString("  Password other devices must give (optional; leave empty for none).\n")
	}
	b.WriteString("  The same password protects FreeCAD's RPC server on this computer, and agents here\n")
	b.WriteString("  use it too. Without a password, anyone on an allowed IP address (or on this\n")
	b.WriteString("  computer) can run code in FreeCAD. Connections are not encrypted: use this on a\n")
	b.WriteString("  network you trust, or an SSH tunnel (see " + remoteAccessDocsURL + ").\n\n")
	if sh.Password == "" && sh.existingPassword != "" {
		b.WriteString("  Password: (a password is set; leave empty to keep it, type to replace it)\n")
	} else {
		fmt.Fprintf(&b, "  Password: %s\n", maskedFieldDisplay(sh.Password))
	}
	if sh.message != "" {
		b.WriteString("\n  " + sh.message + "\n")
	}
	b.WriteString(tui.Footer(theme, tui.Hints(theme,
		tui.Hint{Key: "enter", Label: "continue"}, tui.Hint{Key: "esc", Label: "back"})))
	return tui.Section(theme, s.Title(state), b.String())
}

// --- Minutes ---

type shareMinutesStep struct{}

func (s *shareMinutesStep) ID() string { return "share-minutes" }

func (s *shareMinutesStep) Title(*AppState) string { return "Share this PC" }

func (s *shareMinutesStep) Hints(*AppState) []struct{ Key, Label string } {
	return []struct{ Key, Label string }{{"enter", "continue"}, {"esc", "back"}}
}

func (s *shareMinutesStep) Init(state *AppState) tea.Cmd {
	if !state.Share.On {
		return func() tea.Msg { return shareSkipMsg{} }
	}
	sh := &state.Share
	if sh.minutesText == "" {
		sh.minutesText = strconv.Itoa(sh.TimeoutMinutes)
	}
	sh.message = ""
	return nil
}

func (s *shareMinutesStep) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	if _, ok := msg.(shareSkipMsg); ok {
		return flow.Skip, nil
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return flow.Continue, nil
	}
	sh := &state.Share
	switch k.String() {
	case "ctrl+c":
		return flow.Quit, nil
	case "esc":
		state.Retreating = true
		return flow.Back, nil
	case "enter":
		n, err := validateMinutes(sh.minutesText)
		if err != nil {
			sh.message = err.Error()
			return flow.Continue, nil
		}
		sh.TimeoutMinutes = n
		sh.message = ""
		return flow.Next, nil
	case "backspace":
		sh.minutesText = trimLastRune(sh.minutesText)
		sh.message = ""
	default:
		for _, r := range k.Runes {
			if r >= '0' && r <= '9' {
				sh.minutesText += string(r)
			}
		}
		sh.message = ""
	}
	return flow.Continue, nil
}

func (s *shareMinutesStep) View(state *AppState) string {
	theme := tui.DefaultTheme
	sh := &state.Share
	var b strings.Builder
	b.WriteString("  How long to keep the session assigned to an agent?\n\n")
	b.WriteString("  With remote access on, several agents may try to use FreeCAD at the same time. The\n")
	b.WriteString("  session lock stops one agent from taking control while another works. Agents can\n")
	b.WriteString("  free it themselves; after this long without activity it becomes available to\n")
	b.WriteString("  another agent.\n\n")
	fmt.Fprintf(&b, "  Minutes: %s_\n", sh.minutesText)
	if sh.message != "" {
		b.WriteString("\n  " + sh.message + "\n")
	}
	b.WriteString(tui.Footer(theme, tui.Hints(theme,
		tui.Hint{Key: "enter", Label: "continue"}, tui.Hint{Key: "esc", Label: "back"})))
	return tui.Section(theme, s.Title(state), b.String())
}

// --- Apply ---

// applyShareChoice writes the share settings into every located addon
// settings file (addoninstall.UpdateSettings) and the password into the
// credential store, or turns remote access off, printing one [ok] or [fail]
// line per step. It returns an exit code. The listener is not touched here;
// applyShareListener runs after the addon install (see finishWizard).
func applyShareChoice(ctx context.Context, w io.Writer, state *AppState, dryRun bool) int {
	sh := &state.Share
	if !sh.Asked {
		return 0
	}
	switch {
	case sh.On:
		password := sh.Password
		if password == "" && sh.existingPassword != "" {
			// Left empty: keep the password already set, rather than
			// removing it (only the Share page removes one, with an
			// explicit empty save).
			password = sh.existingPassword
		}
		return applyShareSettingsOn(ctx, w, state.Addon.Targets, shareOptions{
			AllowedIPs:     sh.AllowedIPs,
			Password:       password,
			TimeoutMinutes: sh.TimeoutMinutes,
			Port:           sh.Port,
		}, dryRun)
	case sh.Initial:
		return applyShareSettingsOff(w, state.Addon.Targets, dryRun)
	default:
		// Chosen No while remote access was already off: nothing to apply.
		return 0
	}
}

// applyShareSettingsOn writes opts into every target's addon settings and
// mirrors the password into the credential store, through remote.go's own
// applyShareSettings, so the `share` command and the wizard write and report
// the settings step identically.
func applyShareSettingsOn(ctx context.Context, w io.Writer, targets []addoninstall.Target, opts shareOptions, dryRun bool) int {
	if applyShareSettings(ctx, w, targets, opts, dryRun) {
		return 0
	}
	return 1
}

// applyShareSettingsOff turns remote access off in every target's addon
// settings, through remote.go's own applyUnshareSettings.
func applyShareSettingsOff(w io.Writer, targets []addoninstall.Target, dryRun bool) int {
	if applyUnshareSettings(w, targets, dryRun) {
		return 0
	}
	return 1
}

// applyShareListener registers and starts the listener (share on) or stops
// and unregisters it (share turned off), printing the plan's lines. handled
// reports whether it acted on the listener, so finishWizard does not restart
// it again.
func applyShareListener(ctx context.Context, w io.Writer, state *AppState, dryRun bool) (code int, handled bool) {
	sh := &state.Share
	if !sh.Asked || len(state.Addon.Targets) == 0 {
		return 0, false
	}
	switch {
	case sh.On:
		return applyShareListenerOn(w, state.Addon.Targets, sh.Port, sh.AllowedIPs, dryRun)
	case sh.Initial:
		return applyShareListenerOff(w, dryRun)
	default:
		return 0, false
	}
}

// applyShareListenerOn registers the listener (recorded against the first
// located target) and starts or restarts it, through remote.go's own
// applyShareListenerRegistration, so the `share` command and the wizard
// report the listener steps identically.
func applyShareListenerOn(w io.Writer, targets []addoninstall.Target, port int, allowedIPs string, dryRun bool) (int, bool) {
	if dryRun {
		fmt.Fprintln(w, "  would register and start the freecad-mcp listener")
		return 0, true
	}
	if !applyShareListenerRegistration(w, targets) {
		return 1, true
	}
	shareOnResultLine(w, port, allowedIPs)
	return 0, true
}

// applyShareListenerOff stops and removes the listener entry.
func applyShareListenerOff(w io.Writer, dryRun bool) (int, bool) {
	if dryRun {
		fmt.Fprintln(w, "  would stop and remove the listener")
		return 0, true
	}
	if !applyUnshareListener(w) {
		return 1, true
	}
	return 0, true
}

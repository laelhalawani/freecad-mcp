package main

// Install wizard plumbing. The wizard writes nothing until the AI clients
// are registered: the addon, "FreeCAD on another computer" and "Share this
// PC" choices (with any password) are only recorded, and applied after
// registration (finishWizard). Leaving the wizard before that point (q,
// ctrl+c, closing it, SIGINT/SIGTERM, or "Install FreeCAD on this computer
// first") leaves the machine unchanged, and the process exits with
// exitCancelled so the install scripts can roll back the binary they just
// placed.

import (
	"context"
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/cli"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

// exitCancelled is the exit status of a wizard left before anything was
// written. install.ps1 and install.sh rely on it to undo their own changes.
const exitCancelled = 3

// harnessSelection adapts mcp-wizard v0.1.1's client list:
//
//   - It renders every key of HarnessState.Selected as checked, including
//     clients the user switched off (their value is false). Dropping false
//     entries after each update makes the list show what will be registered.
//   - Its enter does nothing while no client is selected. Here the addon is
//     worth installing on its own (no AI client yet, or one registered by
//     hand), so enter moves on and registration reports that nothing was
//     configured.
//   - It pre-selects clients whose entry was edited by hand or runs another
//     program (see clients.go), and registering replaces that entry. Those
//     start unticked here, with a note saying why, so what is there is only
//     replaced on request.
//   - Revisiting the step detects the clients again, but the library keeps
//     taking keys meanwhile, which would act on the previous list. Keys other
//     than cancel wait until the new list is shown.
type harnessSelection struct {
	flow.Step[AppState]
	// name is the server's name in the client configs.
	name string
	// findUnticked returns the detected clients that start unticked.
	findUnticked func([]harness.Harness) map[harness.ID]bool
}

func (h harnessSelection) Init(state *AppState) tea.Cmd {
	// The library fills Selected when detection finishes. Keys are held
	// back until then, so Selected turning non-nil marks the detection.
	state.Harness.Selected = nil
	state.harnessDetecting = true
	state.UntickedClients = nil
	return h.Step.Init(state)
}

func (h harnessSelection) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch key := k.String(); {
		case state.harnessDetecting && key != "q" && key != "ctrl+c":
			return flow.Continue, nil
		case !state.harnessDetecting && key == "enter" && !anySelected(state.Harness.Selected):
			return flow.Next, nil
		}
	}
	d, cmd := h.Step.Update(msg, state)
	if state.harnessDetecting && state.Harness.Selected != nil {
		state.harnessDetecting = false
		if h.findUnticked != nil {
			unticked := h.findUnticked(state.Harness.Detections)
			for _, c := range state.Harness.Detections {
				if unticked[c.ID] && c.Selectable() {
					delete(state.Harness.Selected, c.ID)
					state.UntickedClients = append(state.UntickedClients, c.Name)
				}
			}
		}
	}
	pruneUnselected(&state.Harness.Selected)
	return d, cmd
}

func (h harnessSelection) View(state *AppState) string {
	view := h.Step.View(state)
	if len(state.UntickedClients) == 0 || state.harnessDetecting {
		return view
	}
	note := fmt.Sprintf("  %s already has a %q entry that setup did not write (edited by hand,\n"+
		"  or another program's). It starts unticked so the entry is kept; ticking it replaces it.\n",
		state.UntickedClients[0], h.name)
	if len(state.UntickedClients) > 1 {
		note = fmt.Sprintf("  %s already have a %q entry that setup did not write (edited by hand,\n"+
			"  or another program's). They start unticked so the entries are kept; ticking one replaces it.\n",
			strings.Join(state.UntickedClients, ", "), h.name)
	}
	// Above the footer, which is the last line.
	if i := strings.LastIndex(view, "\n"); i >= 0 {
		return view[:i] + "\n" + note + view[i:]
	}
	return view + "\n" + note
}

func anySelected[K comparable](selected map[K]bool) bool {
	for _, on := range selected {
		if on {
			return true
		}
	}
	return false
}

func pruneUnselected[K comparable](selected *map[K]bool) {
	for id, on := range *selected {
		if !on {
			delete(*selected, id)
		}
	}
}

// loginInput lets a password be typed in full in `freecad-mcp login`:
// mcp-wizard v0.1.1's login step quits on q on its input screen as well, so a
// password containing q could not be entered. On the input screen q goes into
// the input the way the library adds other keys; on the Sign in / Skip
// screen q still quits, and ctrl+c quits on both.
type loginInput struct {
	flow.Step[AppState]
}

func (l loginInput) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && k.String() == "q" && state.Login.Stage >= 0 && !state.Login.Submitting {
		state.Login.Input += string(k.Runes)
		return flow.Continue, nil
	}
	return l.Step.Update(msg, state)
}

// runsUnattended reports whether install or add skip the wizard: --yes, or
// a flag the wizard would ignore (--token and the other credential flags,
// --clients, --all). `install` (with or without --scope project) can receive
// all of them; `add` accepts only --all and --yes of these. An unattended
// install changes no remote access settings.
func runsUnattended(cmd cli.Command) bool {
	return cmd.Yes || cmd.All || len(cmd.Clients) > 0 || len(cmd.Credentials) > 0
}

// applyGuard keeps ctrl+c from ending the wizard while the registration
// writes are running: the library step accepts it, and exiting then would
// kill the goroutine in the middle of writing a client's config file.
type applyGuard struct {
	flow.Step[AppState]
	dryRun bool
}

func (g applyGuard) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && k.String() == "ctrl+c" && !state.Results.Done && !g.dryRun {
		return flow.Continue, nil
	}
	return g.Step.Update(msg, state)
}

// stepIndex returns the position of the step with the given ID, or -1.
func stepIndex(steps []flow.Step[AppState], id string) int {
	for i, s := range steps {
		if s.ID() == id {
			return i
		}
	}
	return -1
}

// wizardOutcome classifies how the wizard ended.
type wizardOutcome int

const (
	outcomeCompleted   wizardOutcome = iota // registration ran
	outcomeCancelled                        // left before registration: nothing written
	outcomeInterrupted                      // left during registration: some clients may be registered
	outcomeFailed                           // a step failed, or the wizard could not start, before registration
)

// classifyWizard decides the outcome from the flow's state. runCode is
// tui.Run's result: non-zero without a recorded failure means the terminal
// UI could not run at all. applyStarted reports whether the flow reached the
// registration step; in a dry run that step writes nothing. signalled
// reports that the wizard's context was cancelled (SIGINT or SIGTERM), which
// ends tui.Run with a non-zero code; before registration that is a cancel.
func classifyWizard(base *flow.BaseState, runCode int, applyStarted, dryRun, signalled bool) wizardOutcome {
	switch {
	case base.Settled:
		return outcomeCompleted
	case applyStarted && !dryRun:
		return outcomeInterrupted
	case signalled:
		return outcomeCancelled
	case base.Failure != nil || runCode != 0:
		return outcomeFailed
	default:
		return outcomeCancelled
	}
}

// finishWizard applies what the wizard recorded once registration ran, in
// this order: the connection to FreeCAD on another computer or the share
// settings, the addon, then the listener. With nothing to change for the
// listener, a registered one is restarted so it runs this binary. It returns
// the exit status.
func finishWizard(ctx context.Context, w io.Writer, state *AppState, dryRun bool, registrationCode int) int {
	code := registrationCode
	code = max(code, applyConnectChoice(ctx, w, state, dryRun))
	code = max(code, applyShareChoice(ctx, w, state, dryRun))
	if !state.Connect.Chosen {
		code = max(code, finishAddon(w, state.Addon, dryRun))
	}
	listenerCode, handled := applyShareListener(ctx, w, state, dryRun)
	code = max(code, listenerCode)
	if !handled && !dryRun {
		code = max(code, restartListener(ctx, w))
	}
	return code
}

// restartListener is restartListenerIfRegistered, called through a variable
// so tests can replace it and never restart a real listener.
var restartListener = restartListenerIfRegistered

// finishAddon installs the addon into every located target with the chosen
// auto-start setting, or says FreeCAD was not found.
func finishAddon(w io.Writer, a addonState, dryRun bool) int {
	fmt.Fprintln(w, "\n  FreeCAD addon")
	if len(a.Targets) == 0 {
		fmt.Fprintln(w, freecadNotFound)
		return 0
	}
	if dryRun {
		for _, t := range a.Targets {
			fmt.Fprintf(w, "  would install the FreeCAD addon into %s\n", t.AddonDir())
		}
		return 0
	}
	var results []addonResult
	for _, t := range a.Targets {
		results = append(results, installAddon(t, a.AutoStart))
	}
	return reportAddonResults(w, results)
}

const interruptedMessage = "  Setup was interrupted while registering the AI clients, so some may be registered.\n" +
	"  Run `" + domain.BinaryName + " install` to finish, or `" + domain.BinaryName + " uninstall --all` to remove everything."

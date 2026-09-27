package main

// Install wizard plumbing. The wizard writes nothing until the AI clients
// are registered: the token is held in memory and the addon choice is only
// recorded, and both are applied after registration. Leaving the wizard
// before that point (q, ctrl+c, or closing it) leaves the machine unchanged,
// and the process exits with exitCancelled so the install scripts can roll
// back the binary they just placed.

import (
	"context"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/secret"

	"github.com/laelhalawani/freecad-mcp/internal/domain"
)

// exitCancelled is the exit status of a wizard left before anything was
// written. install.ps1 and install.sh rely on it to undo their own changes.
const exitCancelled = 3

// harnessSelection works around mcp-wizard v0.1.1's client list, which
// renders every key of HarnessState.Selected as checked, including clients
// the user switched off (their value is false). Dropping false entries after
// each update makes the list show what will actually be registered.
type harnessSelection struct {
	flow.Step[AppState]
}

func (h harnessSelection) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	d, cmd := h.Step.Update(msg, state)
	pruneUnselected(&state.Harness.Selected)
	return d, cmd
}

func pruneUnselected[K comparable](selected *map[K]bool) {
	for id, on := range *selected {
		if !on {
			delete(*selected, id)
		}
	}
}

// deferredStore lets the login step see a stored token but keeps a new one
// in memory until flush, so leaving the wizard does not write credentials.
type deferredStore struct {
	inner   secret.Store
	pending *secret.Session
}

func (s *deferredStore) Load(ctx context.Context) (*secret.Session, bool, error) {
	return s.inner.Load(ctx)
}

func (s *deferredStore) Save(_ context.Context, sess *secret.Session) error {
	s.pending = sess.Clone()
	return nil
}

func (s *deferredStore) Delete(context.Context) error { return nil }

func (s *deferredStore) Path() string { return s.inner.Path() }

// loginFresh makes each visit of the login step start clean: going back to
// it drops a token typed on an earlier visit and the library's Skipped flag,
// which it only resets in one branch, so both reflect the last visit.
type loginFresh struct {
	flow.Step[AppState]
	store *deferredStore
}

func (l loginFresh) Init(state *AppState) tea.Cmd {
	if l.store != nil {
		l.store.pending = nil
	}
	state.Login.Skipped = false
	return l.Step.Init(state)
}

// flush writes the token entered in the wizard, if any.
func (s *deferredStore) flush(ctx context.Context) error {
	if s.pending == nil || len(s.pending.Values) == 0 {
		return nil
	}
	return s.inner.Save(ctx, s.pending)
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
// registration step; in a dry run that step writes nothing.
func classifyWizard(base *flow.BaseState, runCode int, applyStarted, dryRun bool) wizardOutcome {
	switch {
	case base.Settled:
		return outcomeCompleted
	case base.Failure != nil || runCode != 0:
		if applyStarted && !dryRun {
			return outcomeInterrupted
		}
		return outcomeFailed
	case applyStarted && !dryRun:
		return outcomeInterrupted
	default:
		return outcomeCancelled
	}
}

// finishWizard applies what the wizard recorded once registration ran: the
// token, then the addon. It returns the exit status.
func finishWizard(ctx context.Context, w io.Writer, state *AppState, store *deferredStore, dryRun bool, registrationCode int) int {
	code := registrationCode
	// A token typed and later skipped (back, then "Skip for now") is dropped.
	if !dryRun && store != nil && !state.Login.Skipped {
		if err := store.flush(ctx); err != nil {
			fmt.Fprintf(w, "  [fail] could not save the RPC auth token: %v\n", err)
			code = 1
		}
	}
	a := state.Addon
	fmt.Fprintln(w, "\n  FreeCAD addon")
	if len(a.Targets) == 0 {
		fmt.Fprintln(w, freecadNotFound)
		return code
	}
	if dryRun {
		for _, t := range a.Targets {
			fmt.Fprintf(w, "  would install the FreeCAD addon into %s\n", t.AddonDir())
		}
		return code
	}
	var results []addonResult
	for _, t := range a.Targets {
		results = append(results, installAddon(t, a.AutoStart))
	}
	return max(code, printAddonResults(w, results, a.AutoStart))
}

const interruptedMessage = "  Setup was interrupted while registering the AI clients, so some may be registered.\n" +
	"  Run `" + domain.BinaryName + " install` to finish, or `" + domain.BinaryName + " uninstall --all` to remove everything."

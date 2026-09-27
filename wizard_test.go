package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/installer"
	"github.com/sairaph/mcp-wizard/secret"

	"github.com/laelhalawani/freecad-mcp/internal/addoninstall"
)

// readyHarnessStep returns mcp-wizard's real harness step, wrapped as the
// wizard wraps it, after its detection finished, showing two fake clients.
func readyHarnessStep(t *testing.T) (flow.Step[AppState], *AppState) {
	t.Helper()
	det, err := harness.New(harness.ServerSpec{Name: "freecad-mcp-test", Command: "freecad-mcp-test", Args: []string{"mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	state := &AppState{}
	step := harnessSelection{Step: installer.HarnessStep(context.Background(), det, harnessState, installer.HarnessStepOptions{AllDetected: true})}
	batch, ok := step.Init(state)().(tea.BatchMsg)
	if !ok {
		t.Fatal("Init did not return a batch")
	}
	for _, cmd := range batch {
		if cmd != nil {
			step.Update(cmd(), state) // the spinner tick and the detection result
		}
	}
	state.Harness.Detections = []harness.Harness{
		{ID: "claude-code", Name: "Claude Code", State: harness.Detected, Installed: true},
		{ID: "cursor", Name: "Cursor", State: harness.Detected, Installed: true},
	}
	state.Harness.Selected = map[harness.ID]bool{"claude-code": true, "cursor": true}
	state.Harness.Cursor = 0
	return step, state
}

func rowFor(t *testing.T, view, name string) string {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, name) {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", name, view)
	return ""
}

func TestDeselectedClientIsShownAsDeselected(t *testing.T) {
	step, state := readyHarnessStep(t)
	space := tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}

	step.Update(space, state)
	view := step.View(state)
	if row := rowFor(t, view, "Claude Code"); !strings.Contains(row, "○") || strings.Contains(row, "●") {
		t.Fatalf("deselected client still shown as selected: %q", row)
	}
	if row := rowFor(t, view, "Cursor"); !strings.Contains(row, "●") {
		t.Fatalf("other client not shown as selected: %q", row)
	}
	if state.Harness.Selected["claude-code"] {
		t.Fatal("the selection itself was not changed")
	}

	step.Update(space, state)
	if row := rowFor(t, step.View(state), "Claude Code"); !strings.Contains(row, "●") {
		t.Fatalf("selecting again not shown: %q", row)
	}
}

func TestAllNoneTogglesTheWholeList(t *testing.T) {
	step, state := readyHarnessStep(t)
	a := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}}
	step.Update(a, state) // everything was selected: clear all
	view := step.View(state)
	for _, name := range []string{"Claude Code", "Cursor"} {
		if row := rowFor(t, view, name); strings.Contains(row, "●") {
			t.Fatalf("%s still shown as selected after none: %q", name, row)
		}
	}
	step.Update(a, state)
	view = step.View(state)
	for _, name := range []string{"Claude Code", "Cursor"} {
		if row := rowFor(t, view, name); !strings.Contains(row, "●") {
			t.Fatalf("%s not shown as selected after all: %q", name, row)
		}
	}
}

func TestEnterWithNoClientSelectedMovesOn(t *testing.T) {
	step, state := readyHarnessStep(t)
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	step.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}}, state) // none
	if d, _ := step.Update(enter, state); d != flow.Next {
		t.Fatalf("enter with no client selected: %v, want Next (the addon is still installed)", d)
	}
	step.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, state)
	if d, _ := step.Update(enter, state); d != flow.Next {
		t.Fatalf("enter with a client selected: %v, want Next", d)
	}
}

func TestEnterBeforeDetectionFinishesDoesNothing(t *testing.T) {
	det, err := harness.New(harness.ServerSpec{Name: "freecad-mcp-test", Command: "freecad-mcp-test", Args: []string{"mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	state := &AppState{}
	state.Harness.Selected = map[harness.ID]bool{} // left over from an earlier visit
	step := harnessSelection{Step: installer.HarnessStep(context.Background(), det, harnessState, installer.HarnessStepOptions{AllDetected: true})}
	step.Init(state) // detection has started but not finished
	if d, _ := step.Update(tea.KeyMsg{Type: tea.KeyEnter}, state); d != flow.Continue {
		t.Fatalf("enter while detecting: %v, want Continue", d)
	}
}

func TestKeysWaitForTheListWhenTheStepIsRevisited(t *testing.T) {
	step, state := readyHarnessStep(t)
	step.Init(state) // back to the list: detection runs again
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'a'}}, {Type: tea.KeySpace, Runes: []rune{' '}}, {Type: tea.KeyEnter}} {
		if d, _ := step.Update(key, state); d != flow.Continue {
			t.Fatalf("%q while detecting again: %v, want Continue", key.String(), d)
		}
	}
	if state.Harness.Selected != nil {
		t.Fatalf("a key acted on the previous list: %v", state.Harness.Selected)
	}
	if d, _ := step.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}, state); d != flow.Quit {
		t.Fatalf("q while detecting: %v, want Quit", d)
	}
}

func TestApplyStepIsFoundByID(t *testing.T) {
	det, err := harness.New(harness.ServerSpec{Name: "t", Command: "t"})
	if err != nil {
		t.Fatal(err)
	}
	steps := []flow.Step[AppState]{
		harnessSelection{Step: installer.HarnessStep(context.Background(), det, harnessState, installer.HarnessStepOptions{})},
		newAddonStep(context.Background()),
		applyGuard{installer.ApplyStep(context.Background(), det, harnessState, resultsState, installer.ApplyStepOptions{}), false},
	}
	if got := stepIndex(steps, "apply"); got != 2 {
		t.Fatalf("stepIndex = %d, want 2", got)
	}
}

func TestCtrlCIsIgnoredWhileRegistrationRuns(t *testing.T) {
	inner := quitStep{}
	state := &AppState{}
	ctrlC := tea.KeyMsg{Type: tea.KeyCtrlC}
	if d, _ := (applyGuard{inner, false}).Update(ctrlC, state); d != flow.Continue {
		t.Fatalf("ctrl+c ended the wizard during registration: %v", d)
	}
	state.Results.Done = true
	if d, _ := (applyGuard{inner, false}).Update(ctrlC, state); d != flow.Quit {
		t.Fatalf("ctrl+c after registration was swallowed: %v", d)
	}
	state.Results.Done = false
	if d, _ := (applyGuard{inner, true}).Update(ctrlC, state); d != flow.Quit {
		t.Fatalf("ctrl+c during a dry run was swallowed: %v", d)
	}
}

// quitStep quits on any key, like the library's apply step on ctrl+c.
type quitStep struct{}

func (quitStep) ID() string                                    { return "apply" }
func (quitStep) Title(*AppState) string                        { return "" }
func (quitStep) Hints(*AppState) []struct{ Key, Label string } { return nil }
func (quitStep) Init(*AppState) tea.Cmd                        { return nil }
func (quitStep) View(*AppState) string                         { return "" }
func (quitStep) Update(tea.Msg, *AppState) (flow.Directive, tea.Cmd) {
	return flow.Quit, nil
}

func TestDeferredStoreWritesOnlyOnFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store := &deferredStore{inner: secret.NewFileStore(path)}
	sess := secret.NewSession()
	sess.Set("token", "s3cret")
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Save wrote the credentials file before flush: %v", err)
	}
	if err := store.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := secret.NewFileStore(path).Load(context.Background())
	if err != nil || !ok || got.GetString("token") != "s3cret" {
		t.Fatalf("after flush: %v %v %v", got, ok, err)
	}
}

func TestDeferredStoreFlushWithoutTokenWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store := &deferredStore{inner: secret.NewFileStore(path)}
	if err := store.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a skipped token step wrote a file: %v", err)
	}
}

func TestClassifyWizard(t *testing.T) {
	cases := []struct {
		name         string
		base         flow.BaseState
		runCode      int
		applyStarted bool
		dryRun       bool
		want         wizardOutcome
	}{
		{"quit before registration", flow.BaseState{}, 0, false, false, outcomeCancelled},
		{"quit during registration", flow.BaseState{}, 0, true, false, outcomeInterrupted},
		{"registration ran", flow.BaseState{Settled: true}, 0, true, false, outcomeCompleted},
		{"a step failed", flow.BaseState{Failure: os.ErrPermission}, 1, false, false, outcomeFailed},
		{"the terminal UI could not start", flow.BaseState{}, 1, false, false, outcomeFailed},
		{"registration failed in flight", flow.BaseState{Failure: os.ErrPermission}, 1, true, false, outcomeInterrupted},
		{"quit while a dry run plans", flow.BaseState{}, 0, true, true, outcomeCancelled},
		{"a dry run's plan failed", flow.BaseState{Failure: os.ErrPermission}, 1, true, true, outcomeFailed},
		{"a dry run completed", flow.BaseState{Settled: true}, 0, true, true, outcomeCompleted},
	}
	for _, c := range cases {
		if got := classifyWizard(&c.base, c.runCode, c.applyStarted, c.dryRun, false); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRevisitingTheLoginStepStartsClean(t *testing.T) {
	store := &deferredStore{inner: secret.NewFileStore(filepath.Join(t.TempDir(), "c.json"))}
	sess := secret.NewSession()
	sess.Set("token", "from-first-visit")
	store.Save(context.Background(), sess)
	state := &AppState{}
	state.Login.Skipped = true
	step := loginFresh{installer.LoginStep(context.Background(), installer.LoginConfig{ID: "t", Store: store}, loginState), store}
	step.Init(state)
	if store.pending != nil || state.Login.Skipped {
		t.Fatalf("second visit kept pending=%v skipped=%v", store.pending, state.Login.Skipped)
	}
}

func TestGoingBackPassesALoginStepThatSkipsItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	sess := secret.NewSession()
	sess.Set("token", "stored")
	if err := secret.NewFileStore(path).Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	store := &deferredStore{inner: secret.NewFileStore(path)}
	login := installer.LoginConfig{ID: "t", Store: store, Stages: []installer.LoginStage{{Field: installer.LoginField{Name: "token"}}}}
	step := loginFresh{installer.LoginStep(context.Background(), login, loginState), store}

	arrive := func(state *AppState) flow.Directive {
		cmd := step.Init(state)
		if cmd == nil {
			t.Fatal("with a stored token the login step did not skip itself")
		}
		d, _ := step.Update(cmd(), state)
		return d
	}
	if d := arrive(&AppState{}); d != flow.Skip {
		t.Fatalf("moving forward: %v, want Skip", d)
	}
	state := &AppState{Retreating: true} // esc on the addon step
	if d := arrive(state); d != flow.Back {
		t.Fatalf("moving back: %v, want Back, or the client list cannot be reached", d)
	}
	if d := arrive(state); d != flow.Skip {
		t.Fatalf("moving forward again: %v, want Skip", d)
	}
}

func TestSkippedTokenIsNotSaved(t *testing.T) {
	credPath := filepath.Join(t.TempDir(), "credentials.json")
	store := &deferredStore{inner: secret.NewFileStore(credPath)}
	sess := secret.NewSession()
	sess.Set("token", "typed-then-skipped")
	store.Save(context.Background(), sess)

	state := &AppState{}
	state.Login.Skipped = true // went back and chose "Skip for now"
	var out bytes.Buffer
	finishWizard(context.Background(), &out, state, store, false, 0)
	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Fatalf("a skipped token was saved: %v", err)
	}
}

func TestFinishWizardInstallsTheAddonAndSavesTheToken(t *testing.T) {
	dataDir := t.TempDir()
	credPath := filepath.Join(t.TempDir(), "credentials.json")
	store := &deferredStore{inner: secret.NewFileStore(credPath)}
	sess := secret.NewSession()
	sess.Set("token", "abc")
	store.Save(context.Background(), sess)

	state := &AppState{}
	state.Addon = addonState{Targets: []addoninstall.Target{{UserDataDir: dataDir}}, AutoStart: true}
	var out bytes.Buffer
	code := finishWizard(context.Background(), &out, state, store, false, 0)
	if code != 0 {
		t.Fatalf("code %d:\n%s", code, out.String())
	}
	target := addoninstall.Target{UserDataDir: dataDir}
	if _, err := addoninstall.InstalledVersion(target); err != nil {
		t.Fatalf("addon not installed: %v\n%s", err, out.String())
	}
	if !addoninstall.AutoStart(target) {
		t.Fatal("auto-start not turned on")
	}
	if _, err := os.Stat(credPath); err != nil {
		t.Fatalf("token not saved: %v", err)
	}
}

func TestFinishWizardDryRunWritesNothing(t *testing.T) {
	dataDir := t.TempDir()
	credPath := filepath.Join(t.TempDir(), "credentials.json")
	store := &deferredStore{inner: secret.NewFileStore(credPath)}
	sess := secret.NewSession()
	sess.Set("token", "abc")
	store.Save(context.Background(), sess)

	state := &AppState{}
	state.Addon = addonState{Targets: []addoninstall.Target{{UserDataDir: dataDir}}, AutoStart: true}
	var out bytes.Buffer
	finishWizard(context.Background(), &out, state, store, true, 0)
	if entries, _ := os.ReadDir(dataDir); len(entries) != 0 {
		t.Fatalf("dry run wrote into the FreeCAD data directory: %v", entries)
	}
	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Fatalf("dry run saved the token: %v", err)
	}
	if !strings.Contains(out.String(), "would install the FreeCAD addon") {
		t.Fatalf("output:\n%s", out.String())
	}
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sairaph/mcp-wizard/cli"
	"github.com/sairaph/mcp-wizard/command"
	"github.com/sairaph/mcp-wizard/doctor"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/installer"
	"github.com/sairaph/mcp-wizard/secret"
	"github.com/sairaph/mcp-wizard/tui"
	"github.com/sairaph/mcp-wizard/update"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/mcpserver"
)

// version is set by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

var oneShotCommands = command.New()

func init() {
	// One-shot CLI commands share their logic with the install wizard and the
	// TUI app (see freecad.go).
	registerFreeCADCommands(oneShotCommands)
}

var usageSpecs = []cli.Spec{
	{Name: "mcp", Description: "Run the MCP server (default when not in a terminal)"},
	{Name: "install", Description: "Install the FreeCAD addon and register the server with AI clients"},
	{Name: "uninstall", Description: "Remove AI client integration (--all: also the addon, listener, password, cache and program)"},
	{Name: "add", Description: "Register the server in this project's AI client configs"},
	{Name: "login", Description: "Store FreeCAD's password (login --token <password>)"},
	{Name: "doctor", Description: "Diagnose the installation"},
	{Name: "update", Description: "Update to the latest release"},
	{Name: "version", Description: "Print the version"},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bare invocation in a terminal opens the app; everywhere else (an AI
	// client spawning us, a pipe) it is the MCP server, which cli.Parse
	// reports as "mcp".
	if opts := updateOptions(); opts.InstallDir != "" {
		update.RemoveStaleBinaries(opts.InstallDir, opts.BinaryName)
	}
	if len(os.Args) == 1 && tui.IsInteractive() {
		os.Exit(runApp(ctx))
	}

	cmd, err := cli.Parse(os.Args[1:])
	if err != nil {
		if errors.Is(err, cli.ErrUsage) {
			printUsage(os.Stdout)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}

	switch cmd.Name {
	case "mcp":
		os.Exit(runMCPServer(ctx, cmd))
	case "install", "configure":
		if cmd.Scope == string(harness.ScopeProject) {
			os.Exit(runAdd(ctx, cmd))
		}
		os.Exit(runInstall(ctx, cmd))
	case "uninstall":
		os.Exit(runUninstall(ctx, cmd))
	case "add":
		os.Exit(runAdd(ctx, cmd))
	case "login":
		os.Exit(runLogin(ctx, cmd))
	case "doctor":
		os.Exit(runDoctor(ctx))
	case "update":
		os.Exit(runUpdate(ctx, cmd))
	case "help":
		printUsage(os.Stdout)
	case "version":
		fmt.Println(version)
	default:
		if ok, code := oneShotCommands.Dispatch(ctx, cmd.Name, cmd.Args); ok {
			os.Exit(code)
		}
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd.Name)
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func printUsage(w *os.File) {
	cli.Usage(w, usageSpecs)
	oneShotCommands.PrintUsage(w)
}

// --- Shared helpers ---

// AppState is shared across install wizard steps.
type AppState struct {
	flow.BaseState
	Harness installer.HarnessState
	Login   installer.LoginState // `freecad-mcp login` only; install has no login step
	Addon   addonState
	// Connect is the "FreeCAD on another computer" choice (wizard_connect.go)
	// and Share the "Share this PC" choice (wizard_share.go).
	Connect connectState
	Share   shareState
	Results installer.ResultsState
	// UntickedClients names the clients left unticked because their entry
	// was edited or runs another program; harnessDetecting is set while the
	// client list is being detected (see harnessSelection).
	UntickedClients  []string
	harnessDetecting bool
	// Retreating is set by a step that goes back.
	Retreating bool
}

func harnessState(s *AppState) *installer.HarnessState { return &s.Harness }
func loginState(s *AppState) *installer.LoginState     { return &s.Login }
func resultsState(s *AppState) *installer.ResultsState { return &s.Results }

func serverName(cmd cli.Command) string {
	if cmd.ServerName != "" {
		return cmd.ServerName
	}
	return domain.ServerName
}

func newDetector(name string) (*harness.Detector, error) {
	exe, err := harness.ResolveExecutable()
	if err != nil {
		return nil, err
	}
	return harness.New(harness.ServerSpec{
		Name:    name,
		Command: exe,
		Args:    []string{"mcp"},
		Env:     domain.DefaultEnv(),
	})
}

// selectIDs picks the harnesses to register. --clients wins, then --all, and
// by default every selectable client that is not configured yet. Replacing
// an entry that was edited, or that runs another program (see clients.go),
// would drop what is there, so those are returned in kept instead; --all
// replaces edited entries too, and only --clients replaces another
// program's. The returned reason explains an empty selection.
func selectIDs(harnesses []harness.Harness, entries map[harness.ID]clientEntry, cmd cli.Command) (ids []harness.ID, kept []harness.Harness, reason string) {
	if len(cmd.Clients) > 0 {
		var unknown []string
		for _, want := range cmd.Clients {
			found := false
			for _, h := range harnesses {
				if strings.EqualFold(string(h.ID), want) && h.Selectable() {
					ids = append(ids, h.ID)
					found = true
				}
			}
			if !found {
				unknown = append(unknown, want)
			}
		}
		if len(unknown) > 0 {
			return nil, nil, "no detected client matches --clients " + strings.Join(unknown, ",")
		}
		return ids, nil, ""
	}
	// Default: installed clients that are not configured yet. --all: every
	// installed or configured client (in project scope that means every
	// client found on this machine gets a project entry).
	candidates := 0
	for _, h := range harnesses {
		if !h.Selectable() || !h.Relevant() {
			continue
		}
		candidates++
		e := entries[h.ID]
		switch {
		case e.kind == entryForeign, !cmd.All && e.kind == entryEdited:
			kept = append(kept, h)
		case cmd.All, !h.Configured:
			ids = append(ids, h.ID)
		}
	}
	if len(ids) == 0 && len(kept) == 0 && candidates > 0 {
		return nil, nil, "every detected client is already configured (use --all to re-register)"
	}
	return ids, kept, ""
}

func exitCodeFor(results []harness.Result) int {
	for _, r := range results {
		if r.State == harness.ApplyFailed {
			return 1
		}
	}
	return 0
}

func byID(harnesses []harness.Harness) map[harness.ID]harness.Harness {
	m := make(map[harness.ID]harness.Harness, len(harnesses))
	for _, h := range harnesses {
		m[h.ID] = h
	}
	return m
}

// saveCredentialsFromFlags stores --email/--token given to an unattended
// install so the server can use them without a login step.
func saveCredentialsFromFlags(ctx context.Context, store secret.Store, cmd cli.Command) error {
	if len(cmd.Credentials) == 0 {
		return nil
	}
	sess, _, err := store.Load(ctx)
	if err != nil {
		return err
	}
	for k, v := range cmd.Credentials {
		if k == "token" {
			k = domain.TokenKey
			v = strings.TrimSpace(v)
		}
		sess.Set(k, v)
	}
	return store.Save(ctx, sess)
}

// loadCredential returns the stored value for key from the one credential
// store (domain.CredentialPath), or "" when none is stored. A caller that
// needs more than one key calls loadCredentials instead, which opens the
// store once rather than once per key.
func loadCredential(ctx context.Context, key string) (string, error) {
	sess, exists, err := secret.NewFileStore(domain.CredentialPath()).Load(ctx)
	if err != nil || !exists {
		return "", err
	}
	return strings.TrimSpace(sess.GetString(key)), nil
}

// loadCredentials reads token, host and port from the credential store in
// one file load, each "" when not stored: serverSettings and newConnectPage
// both need every field, and calling loadCredential three times would open
// the store three times for the one read.
func loadCredentials(ctx context.Context) (token, host, port string, err error) {
	sess, exists, err := secret.NewFileStore(domain.CredentialPath()).Load(ctx)
	if err != nil || !exists {
		return "", "", "", err
	}
	return strings.TrimSpace(sess.GetString(domain.TokenKey)),
		strings.TrimSpace(sess.GetString(domain.HostKey)),
		strings.TrimSpace(sess.GetString(domain.PortKey)),
		nil
}

// --- Install / add ---

func runInstall(ctx context.Context, cmd cli.Command) int {
	detector, err := newDetector(serverName(cmd))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	credStore := secret.NewFileStore(domain.CredentialPath())

	if tui.IsInteractive() && !runsUnattended(cmd) {
		return runWizard(ctx, detector, harness.Scope{}, cmd, "freecad-mcp setup", true)
	}
	return runUnattended(ctx, detector, harness.Scope{}, credStore, cmd, harness.Present)
}

// projectDir resolves --dir (default: the working directory) to an absolute path.
func projectDir(cmd cli.Command) (string, error) {
	dir := cmd.Dir
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = cwd
	}
	return filepath.Abs(dir)
}

func runAdd(ctx context.Context, cmd cli.Command) int {
	dir, err := projectDir(cmd)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	detector, err := newDetector(serverName(cmd))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	scope := harness.ProjectScopeDir(dir)
	// One credential store per user: `add` registers project client entries,
	// and every one of them reads domain.CredentialPath.
	credStore := secret.NewFileStore(domain.CredentialPath())

	if tui.IsInteractive() && !runsUnattended(cmd) {
		return runWizard(ctx, detector, scope, cmd, "freecad-mcp project setup", false)
	}
	return runUnattended(ctx, detector, scope, credStore, cmd, harness.Present)
}

// runWizard drives the interactive install: pick clients, the FreeCAD addon
// (or FreeCAD on another computer), share this PC, register. machine adds the
// per-machine steps (another computer, share this PC), which `add` leaves
// out. Nothing is written before the registration step; the rest follows it
// (see finishWizard).
func runWizard(ctx context.Context, detector *harness.Detector, scope harness.Scope, cmd cli.Command, title string, machine bool) int {
	state := &AppState{}
	steps := []flow.Step[AppState]{
		harnessSelection{
			Step: installer.HarnessStep(ctx, detector, harnessState, installer.HarnessStepOptions{AllDetected: true, Scope: scope}),
			name: serverName(cmd),
			findUnticked: func(hs []harness.Harness) map[harness.ID]bool {
				return untickedClients(clientEntries(ctx, detector, scope, hs))
			},
		},
		newAddonStep(ctx),
	}
	if machine {
		steps = append(steps, connectSteps(ctx)...)
		steps = append(steps, shareSteps(ctx)...)
	}
	steps = append(steps,
		applyGuard{installer.ApplyStep(ctx, detector, harnessState, resultsState, installer.ApplyStepOptions{Scope: scope, DryRun: cmd.DryRun}), cmd.DryRun})
	applyIndex := stepIndex(steps, "apply")
	f := flow.New(steps, state)
	code := tui.Run(ctx, f, tui.Options{Title: title})

	// "Install FreeCAD on this computer first" ends the wizard before
	// anything was written. Its text, with the exact command to re-run, is
	// printed here, after the terminal UI has closed, so it stays on screen.
	if state.Connect.InstallFirst && !state.Settled {
		fmt.Println(installFirstText())
		return exitCancelled
	}

	switch classifyWizard(&state.BaseState, code, f.Current() >= applyIndex, cmd.DryRun, ctx.Err() != nil) {
	case outcomeCancelled:
		fmt.Println("  Setup cancelled; nothing was changed.")
		return exitCancelled
	case outcomeInterrupted:
		fmt.Fprintln(os.Stderr, interruptedMessage)
		return 1
	case outcomeFailed:
		if state.Failure != nil {
			fmt.Fprintln(os.Stderr, state.Failure)
		} else {
			fmt.Fprintln(os.Stderr, "  The setup wizard could not run in this terminal; nothing was changed.\n"+
				"  Run `"+domain.BinaryName+" install --yes` to install without it.")
		}
		return 1
	}
	if state.Failure != nil {
		// Registration failed for some clients; the rest still get the addon.
		fmt.Fprintln(os.Stderr, state.Failure)
	}
	code = finishWizard(ctx, os.Stdout, state, cmd.DryRun, code)
	return max(code, installSkills(os.Stdout, scope, selectedIDs(state.Harness.Selected), cmd.DryRun, false))
}

func runUnattended(ctx context.Context, detector *harness.Detector, scope harness.Scope, credStore secret.Store, cmd cli.Command, desired harness.DesiredState) int {
	if desired != harness.Present {
		return registerUnattended(ctx, detector, scope, credStore, cmd, desired)
	}
	// The addon is installed per machine, so it comes first: an install whose
	// clients are all configured already still updates it. A failed addon
	// install is reported but does not stop the registration.
	addonCode := installAddonUnattended(ctx, cmd.DryRun)
	// Credentials are saved before registration, which can return early
	// (every client configured already), so re-running install to store a
	// token always works.
	if !cmd.DryRun {
		if err := saveCredentialsFromFlags(ctx, credStore, cmd); err != nil {
			fmt.Fprintf(os.Stderr, "  Could not save credentials: %v\n", err)
			addonCode = 1
		}
	}
	return max(addonCode, registerUnattended(ctx, detector, scope, credStore, cmd, desired))
}

func registerUnattended(ctx context.Context, detector *harness.Detector, scope harness.Scope, credStore secret.Store, cmd cli.Command, desired harness.DesiredState) int {
	harnesses := detector.DetectIn(ctx, scope)

	entries := clientEntries(ctx, detector, scope, harnesses)
	name := serverName(cmd)

	if desired == harness.Present {
		ids, kept, reason := selectIDs(harnesses, entries, cmd)
		if reason != "" {
			fmt.Fprintf(os.Stderr, "  %s\n", reason)
			if len(cmd.Clients) > 0 {
				return 2
			}
			// Every client is registered already; the guide skill is still
			// written for them.
			return installSkills(os.Stdout, scope, skillClients(harnesses, entries, nil), cmd.DryRun, true)
		}
		printKept(os.Stdout, kept, entries, scope, name, cmd.DryRun)
		code := 0
		if len(ids) > 0 || len(kept) == 0 {
			code = registerIDs(ctx, detector, scope, harnesses, ids, cmd.DryRun, desired)
		}
		return max(code, installSkills(os.Stdout, scope, skillClients(harnesses, entries, ids), cmd.DryRun, true))
	}
	// Removal targets every entry that runs freecad-mcp, edited ones too;
	// another program's entry under the same name only when named.
	wanted, unknown := matchClients(harnesses, cmd.Clients)
	if len(unknown) > 0 {
		fmt.Fprintf(os.Stderr, "  no known client matches --clients %s\n", strings.Join(unknown, ","))
		return 2
	}
	var ids []harness.ID
	var foreign []harness.Harness
	for _, h := range harnesses {
		e, found := entries[h.ID]
		switch {
		case !found:
		case len(cmd.Clients) > 0:
			if wanted[h.ID] {
				ids = append(ids, h.ID)
			}
		case e.ours():
			ids = append(ids, h.ID)
		default:
			foreign = append(foreign, h)
		}
	}
	printSkippedForeign(os.Stdout, foreign, entries, scope, name)
	if len(ids) == 0 && len(foreign) > 0 {
		return 0
	}
	code := registerIDs(ctx, detector, scope, harnesses, ids, cmd.DryRun, desired)
	// The guide skill goes with the clients it was written for, except from a
	// folder another client that stays registered still reads.
	removing := map[harness.ID]bool{}
	for _, id := range ids {
		removing[id] = true
	}
	var remaining []harness.ID
	for id, e := range entries {
		if e.ours() && !removing[id] {
			remaining = append(remaining, id)
		}
	}
	return max(code, removeSkills(os.Stdout, scope, ids, remaining, cmd.DryRun))
}

// registerIDs adds or removes the server in the given harnesses, replacing a
// differing same-name entry: callers only pass harnesses meant to change.
func registerIDs(ctx context.Context, detector *harness.Detector, scope harness.Scope, harnesses []harness.Harness, ids []harness.ID, dryRun bool, desired harness.DesiredState) int {
	enabling := desired == harness.Present
	if len(ids) == 0 {
		if enabling {
			installer.PrintNoClients(os.Stdout, domain.BinaryName, false)
		} else {
			fmt.Println("  No clients are configured.")
		}
		return 0
	}
	if dryRun {
		return printPlan(ctx, detector, scope, ids, desired)
	}

	results := detector.ApplyIn(ctx, scope, ids, desired, harness.ConflictReplace)
	installer.PrintResultsWithScope(os.Stdout, results, scope, enabling, false)
	installer.PrintReloadHints(os.Stdout, results, byID(harnesses))
	return exitCodeFor(results)
}

func printPlan(ctx context.Context, detector *harness.Detector, scope harness.Scope, ids []harness.ID, desired harness.DesiredState) int {
	changes, err := detector.PlanResultsIn(ctx, scope, ids, desired, harness.ConflictReplace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	installer.PrintChanges(os.Stdout, changes, scope)
	return 0
}

// --- Uninstall ---

func runUninstall(ctx context.Context, cmd cli.Command) int {
	detector, err := newDetector(serverName(cmd))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	scope := harness.Scope{}
	if cmd.Scope == string(harness.ScopeProject) {
		// --all removes the program and everything installed for the user,
		// which a project-scoped uninstall must not touch.
		if cmd.All {
			fmt.Fprintln(os.Stderr, "  --all removes the program itself, so it cannot be combined with --scope project.\n"+
				"  Run `"+domain.BinaryName+" uninstall --scope project` to remove this project's client entries,\n"+
				"  or `"+domain.BinaryName+" uninstall --all` to remove everything installed for your user.")
			return 2
		}
		dir, err := projectDir(cmd)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		scope = harness.ProjectScopeDir(dir)
	} else if cmd.All {
		// --all removes everything, not only the client registrations, so
		// it cannot leave some clients registered to the deleted program.
		if len(cmd.Clients) > 0 {
			fmt.Fprintln(os.Stderr, "  --all removes the program itself, so it cannot be combined with --clients.\n"+
				"  Run `"+domain.BinaryName+" uninstall --clients ...` to remove only some registrations.")
			return 2
		}
		return runUninstallAll(ctx, detector, cmd)
	}
	return runUnattended(ctx, detector, scope, nil, cmd, harness.Absent)
}

// --- Login ---

func runLogin(ctx context.Context, cmd cli.Command) int {
	credStore := secret.NewFileStore(domain.CredentialPath())

	if len(cmd.Credentials) > 0 {
		if err := saveCredentialsFromFlags(ctx, credStore, cmd); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("  Credentials saved to %s\n", credStore.Path())
		return 0
	}
	if !tui.IsInteractive() {
		fmt.Fprintln(os.Stderr, "login requires an interactive terminal, or pass credentials as flags")
		return 1
	}

	state := &AppState{}
	step := loginInput{installer.LoginStep(ctx, domain.LoginConfig(credStore), loginState)}
	f := flow.New([]flow.Step[AppState]{step}, state)
	return tui.Run(ctx, f, tui.Options{Title: "freecad-mcp login"})
}

// --- Doctor ---

func updateOptions() update.Options {
	opts := update.Options{
		Owner:          domain.Owner,
		Repo:           domain.Repo,
		CurrentVersion: version,
		AssetName:      domain.AssetName,
		BinaryName:     domain.BinaryName,
	}
	if exe, err := harness.ResolveExecutable(); err == nil {
		opts.InstallDir = filepath.Dir(exe)
		opts.BinaryName = filepath.Base(exe)
	}
	return opts
}

func newDoctor(ctx context.Context) *doctor.Runner {
	opts := updateOptions()
	r := doctor.New(
		executableCheck{},
		doctor.PathCheck{Dir: opts.InstallDir},
		credentialsCheck{path: domain.CredentialPath()},
		clientsCheck{},
	)
	r.Add(freecadChecks()...)
	r.Add(remoteChecks()...)
	if version != "dev" {
		r.Add(doctor.UpdateCheck{Opts: opts})
	}
	return r
}

func runDoctor(ctx context.Context) int {
	return newDoctor(ctx).Run(ctx, os.Stdout)
}

// credentialsCheck reports whether a FreeCAD password is stored. It is only
// needed when one is set in "Share this PC" on the computer running FreeCAD,
// so none is not a problem.
type credentialsCheck struct{ path string }

func (c credentialsCheck) Name() string { return "FreeCAD password" }

func (c credentialsCheck) Run(ctx context.Context) doctor.Result {
	if os.Getenv(domain.EnvToken) != "" {
		return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: "set by " + domain.EnvToken}
	}
	token, err := loadCredential(ctx, domain.TokenKey)
	if err != nil {
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: err.Error()}
	}
	if token == "" {
		return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: "none stored; only needed when a password is set in `" +
			domain.BinaryName + "` > Share this PC on the computer running FreeCAD (on this computer that stores it too)"}
	}
	return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: "stored in " + c.path}
}

// clientsCheck lists the AI clients that have this server registered,
// including entries the user has edited (see clients.go).
type clientsCheck struct{}

func (clientsCheck) Name() string { return "AI clients" }

func (clientsCheck) Run(ctx context.Context) doctor.Result {
	detector, err := newDetector(domain.ServerName)
	if err != nil {
		return doctor.Result{Name: "AI clients", Status: doctor.Fail, Detail: err.Error()}
	}
	harnesses := detector.Detect(ctx)
	entries := clientEntries(ctx, detector, harness.Scope{}, harnesses)
	var configured []string
	outdated := false
	for _, h := range harnesses {
		switch e := entries[h.ID]; e.kind {
		case entryConfigured:
			configured = append(configured, h.Name)
		case entryEdited:
			configured = append(configured, h.Name+" (entry edited)")
		case entryOutdated:
			configured = append(configured, h.Name+" (runs "+e.command+")")
			outdated = true
		}
	}
	if len(configured) == 0 {
		return doctor.Result{Name: "AI clients", Status: doctor.Warn, Detail: "no client is configured; run `freecad-mcp install`"}
	}
	if outdated {
		return doctor.Result{Name: "AI clients", Status: doctor.Warn, Detail: strings.Join(configured, ", ") + "; run `freecad-mcp install --yes` to register this copy instead"}
	}
	return doctor.Result{Name: "AI clients", Status: doctor.OK, Detail: strings.Join(configured, ", ")}
}

// --- Update ---

func runUpdate(ctx context.Context, cmd cli.Command) int {
	opts := updateOptions()

	// `update --from <file>` swaps in a binary that was already downloaded
	// and verified by other means, then lets it update FreeCAD's copy of the
	// addon, as a download does.
	if len(cmd.Args) >= 2 && cmd.Args[0] == "--from" {
		if err := update.SwapFrom(ctx, cmd.Args[1], opts); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("  Updated.")
		code := runNewBinaryAddonRefresh(ctx, filepath.Join(opts.InstallDir, opts.BinaryName))
		return max(code, restartListener(ctx, os.Stdout))
	}

	if version == "dev" {
		fmt.Println("  This is a development build; build from source to update.")
		return 0
	}
	latest, available, err := update.Check(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Update check failed: %v\n", err)
		return 1
	}
	if !available {
		fmt.Printf("  freecad-mcp %s is up to date.\n", version)
		return 0
	}
	fmt.Printf("  Updating freecad-mcp %s -> %s\n", version, latest)
	if err := update.SelfUpdate(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "  Update failed: %v\n", err)
		return 1
	}
	fmt.Printf("  Updated to %s.\n", latest)
	// The new binary carries the matching addon; let it update FreeCAD's copy.
	code := runNewBinaryAddonRefresh(ctx, filepath.Join(opts.InstallDir, opts.BinaryName))
	// A registered listener still runs the old binary until it restarts.
	return max(code, restartListener(ctx, os.Stdout))
}

// --- MCP Server ---

func runMCPServer(ctx context.Context, cmd cli.Command) int {
	transport := os.Getenv("TRANSPORT")

	// The stored credential is FreeCAD's RPC password, so it is never sent
	// to an endpoint named on the command line.
	if cmd.Remote != "" {
		fmt.Fprintf(os.Stderr, "  %s serves FreeCAD's MCP tools itself and does not bridge to other MCP servers,\n"+
			"  so `mcp --remote %s` is refused: the stored password belongs to FreeCAD's RPC server\n"+
			"  and is only sent to it. Run `%s mcp` without --remote.\n", domain.BinaryName, cmd.Remote, domain.BinaryName)
		return 2
	}

	if transport == "" {
		transport = "stdio"
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	settings, err := serverSettings(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	srv := mcpserver.New(mcpserver.Config{
		Version:   version,
		Transport: transport,
		HTTPAddr:  addr,
		FreeCAD:   settings,
	})
	err = srv.Run(ctx)
	if err == nil || errors.Is(err, context.Canceled) || isClientHangup(err) {
		return 0
	}
	fmt.Fprintln(os.Stderr, err)
	return 1
}

// isClientHangup reports whether the server stopped because the client closed
// the transport (stdin EOF for stdio), which is how MCP sessions normally end.
// The SDK formats this as "server is closing: EOF" without wrapping io.EOF, so
// the check is textual.
func isClientHangup(err error) bool {
	return strings.HasSuffix(err.Error(), io.EOF.Error())
}

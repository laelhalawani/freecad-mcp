package main

// FreeCAD-specific commands, install step and doctor checks. They share one
// set of functions so the install wizard, `install --yes`, the one-shot
// commands and the TUI app behave the same way.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/command"
	"github.com/sairaph/mcp-wizard/doctor"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/laelhalawani/freecad-mcp/internal/addoninstall"
	"github.com/laelhalawani/freecad-mcp/internal/domain"
	"github.com/laelhalawani/freecad-mcp/internal/freecad"
	"github.com/laelhalawani/freecad-mcp/internal/headless"
)

// serverSettings reads the MCP server's settings: the environment first,
// then the token stored with `login`.
func serverSettings(ctx context.Context) (domain.Settings, error) {
	token, err := loadCredential(ctx, domain.TokenKey)
	if err != nil {
		return domain.Settings{}, fmt.Errorf("read the stored token: %w", err)
	}
	return domain.SettingsFromEnv(token)
}

// freecadCommand is the freecadcmd override from the environment, if any.
func freecadCommand() []string {
	if v := strings.TrimSpace(os.Getenv(domain.EnvFreecadCmd)); v != "" {
		if cmd, err := domain.SplitCommand(v); err == nil {
			return cmd
		}
	}
	return nil
}

// --- One-shot commands ---

func registerFreeCADCommands(r *command.Registry) {
	r.Register(command.Handler{
		Name:        "install-addon",
		Description: "Install or update the FreeCAD addon",
		Usage:       "install-addon [--user-data-dir <dir>] [--no-autostart] [--dry-run]",
		Run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("install-addon", flag.ContinueOnError)
			dir := fs.String("user-data-dir", "", "FreeCAD user data directory (default: ask FreeCAD)")
			noAuto := fs.Bool("no-autostart", false, "do not turn on starting the RPC server with FreeCAD")
			refresh := fs.Bool("refresh", false, "only update existing installs, keeping their settings (used by update)")
			dryRun := fs.Bool("dry-run", false, "show where the addon would go without writing")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			var targets []addoninstall.Target
			if *dir != "" {
				targets = []addoninstall.Target{{UserDataDir: *dir, Source: "flag"}}
			}
			if *refresh {
				return refreshAddon(ctx, os.Stdout, targets, *dryRun)
			}
			return installAddonReport(ctx, os.Stdout, targets, *dryRun, withAutoStart(!*noAuto))
		},
	})
	r.Register(command.Handler{
		Name:        "uninstall-addon",
		Description: "Remove the FreeCAD addon",
		Usage:       "uninstall-addon [--user-data-dir <dir>]",
		Run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("uninstall-addon", flag.ContinueOnError)
			dir := fs.String("user-data-dir", "", "FreeCAD user data directory (default: ask FreeCAD)")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			targets := []addoninstall.Target{{UserDataDir: *dir, Source: "flag"}}
			if *dir == "" {
				targets = addoninstall.Locate(ctx, freecadCommand())
			}
			if len(targets) == 0 {
				fmt.Println("  FreeCAD's user data directory was not found; nothing to remove.")
				return 0
			}
			code := 0
			for _, t := range targets {
				removed, err := addoninstall.Uninstall(t)
				switch {
				case err != nil:
					fmt.Fprintf(os.Stderr, "  [fail] %s: %v\n", t.AddonDir(), err)
					code = 1
				case removed:
					fmt.Printf("  [ok]   removed %s\n", t.AddonDir())
				default:
					fmt.Printf("  [ok]   no addon in %s\n", t.ModDir())
				}
			}
			fmt.Println("  Restart FreeCAD to unload it.")
			return code
		},
	})
	r.Register(command.Handler{
		Name:        "check-connection",
		Description: "Check that FreeCAD's RPC server answers",
		Usage:       "check-connection",
		Run: func(ctx context.Context, args []string) int {
			return connectionReport(ctx, os.Stdout)
		},
	})
}

type installOptions struct{ autoStart bool }

type installOption func(*installOptions)

func withAutoStart(on bool) installOption { return func(o *installOptions) { o.autoStart = on } }

// addonResult is the outcome for one FreeCAD user data directory.
type addonResult struct {
	Target   addoninstall.Target
	Replaced bool
	Previous string
	Err      error
}

func installAddon(t addoninstall.Target, autoStart bool) addonResult {
	r := addonResult{Target: t}
	r.Previous, _ = addoninstall.InstalledVersion(t)
	r.Replaced, r.Err = addoninstall.Install(t)
	if r.Err == nil && autoStart {
		if err := addoninstall.SetAutoStart(t, true); err != nil {
			r.Err = fmt.Errorf("installed, but could not turn on auto-start: %w", err)
		}
	}
	return r
}

func printAddonResults(w io.Writer, results []addonResult, autoStart bool) int {
	version, _, _ := addoninstall.EmbeddedVersion()
	code := 0
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(w, "  [fail] %s\n         %v\n", r.Target.AddonDir(), r.Err)
			code = 1
			continue
		}
		verb := "installed"
		if r.Replaced && r.Previous != "" {
			verb = "updated " + r.Previous + " ->"
		} else if r.Replaced {
			verb = "replaced with"
		}
		fmt.Fprintf(w, "  [ok]   %s addon %s\n         %s\n", verb, version, r.Target.AddonDir())
	}
	if code == 0 && len(results) > 0 {
		if autoStart {
			fmt.Fprintln(w, "  The RPC server starts with FreeCAD. Restart FreeCAD if it is running.")
		} else {
			fmt.Fprintln(w, "  Restart FreeCAD, select the MCP Addon workbench and click Start RPC Server.")
		}
	}
	return code
}

const freecadNotFound = "  FreeCAD was not found. Install FreeCAD (and start it once), then run\n" +
	"  `" + domain.BinaryName + " install-addon`, or pass --user-data-dir with the folder\n" +
	"  FreeCAD.getUserAppDataDir() prints in FreeCAD's Python console."

// installAddonReport installs the addon into targets (located when empty)
// and writes a report. It returns a process exit code.
func installAddonReport(ctx context.Context, w io.Writer, targets []addoninstall.Target, dryRun bool, opts ...installOption) int {
	o := installOptions{autoStart: true}
	for _, opt := range opts {
		opt(&o)
	}
	if len(targets) == 0 {
		targets = addoninstall.Locate(ctx, freecadCommand())
	}
	if len(targets) == 0 {
		fmt.Fprintln(w, freecadNotFound)
		return 1
	}
	if dryRun {
		for _, t := range targets {
			fmt.Fprintf(w, "  would install the FreeCAD addon into %s\n", t.AddonDir())
		}
		return 0
	}
	var results []addonResult
	for _, t := range targets {
		results = append(results, installAddon(t, o.autoStart))
	}
	return printAddonResults(w, results, o.autoStart)
}

// refreshAddon updates the addon where it is installed already, so an updated
// binary and the addon it talks to stay in step. Settings are left alone.
func refreshAddon(ctx context.Context, w io.Writer, targets []addoninstall.Target, dryRun bool) int {
	if len(targets) == 0 {
		targets = addoninstall.Locate(ctx, freecadCommand())
	}
	if len(targets) == 0 {
		fmt.Fprintln(w, freecadNotFound)
		return 0
	}
	want, _, _ := addoninstall.EmbeddedVersion()
	var results []addonResult
	for _, t := range targets {
		got, err := addoninstall.InstalledVersion(t)
		if err != nil || got == want {
			continue
		}
		if dryRun {
			fmt.Fprintf(w, "  would update the FreeCAD addon %s -> %s in %s\n", got, want, t.AddonDir())
			continue
		}
		results = append(results, installAddon(t, false))
	}
	if len(results) == 0 {
		if !dryRun {
			fmt.Fprintln(w, "  The installed FreeCAD addon is up to date.")
		}
		return 0
	}
	return printAddonResults(w, results, false)
}

// runNewBinaryAddonRefresh runs `install-addon --refresh` with the binary an
// update just installed, since this process still holds the old addon.
func runNewBinaryAddonRefresh(ctx context.Context, exe string) int {
	cmd := exec.CommandContext(ctx, exe, "install-addon", "--refresh")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "  Could not update the FreeCAD addon (%v); run `%s install-addon`.\n", err, domain.BinaryName)
		return 1
	}
	return 0
}

// installAddonUnattended is the addon part of `install --yes`. A machine
// without FreeCAD still gets its clients registered, so a missing FreeCAD
// is reported but does not fail the install; the caller registers the
// clients whatever this returns and folds the code into its exit status.
// A dry run asks freecadcmd for its data directory, which is FreeCAD's own
// read-only query, but writes nothing itself.
func installAddonUnattended(ctx context.Context, dryRun bool) int {
	fmt.Println("  FreeCAD addon")
	defer fmt.Println()
	targets := addoninstall.Locate(ctx, freecadCommand())
	if len(targets) == 0 {
		fmt.Println(freecadNotFound)
		return 0
	}
	return installAddonReport(ctx, os.Stdout, targets, dryRun)
}

// connectionReport checks the RPC server the MCP server would use.
func connectionReport(ctx context.Context, w io.Writer) int {
	settings, err := serverSettings(ctx)
	if err != nil {
		fmt.Fprintf(w, "  [fail] %v\n", err)
		return 1
	}
	conn := freecad.NewConnection(settings.Host, settings.Port, settings.Token, 5*time.Second)
	defer conn.Close()
	ok, err := conn.Ping(ctx)
	if err != nil || !ok {
		fmt.Fprintf(w, "  [fail] FreeCAD RPC server at %s: %v\n", conn.URL(), rpcProblem(err))
		return 1
	}
	fmt.Fprintf(w, "  [ok]   FreeCAD RPC server at %s answers\n", conn.URL())
	if warning := conn.CheckAddonVersion(ctx, version); warning != "" {
		fmt.Fprintf(w, "  [warn] %s\n", warning)
		return 0
	}
	fmt.Fprintln(w, "  [ok]   the addon matches this server")
	return 0
}

func rpcProblem(err error) string {
	if err == nil {
		return "it did not answer ping"
	}
	msg := err.Error()
	if strings.Contains(msg, "401") {
		return "the addon requires an auth token; run `" + domain.BinaryName + " login --token <token>`"
	}
	return msg + " (is FreeCAD running with the RPC server started?)"
}

// --- Doctor checks ---

// executableCheck is doctor.ExecutableCheck, except on Windows: there Go
// reports no execute permission bits, so the library check always fails, and
// a file that exists and is running is executable by definition.
type executableCheck struct{}

func (executableCheck) Name() string { return doctor.ExecutableCheck{}.Name() }

func (c executableCheck) Run(ctx context.Context) doctor.Result {
	if runtime.GOOS != "windows" {
		return doctor.ExecutableCheck{}.Run(ctx)
	}
	path, err := os.Executable()
	if err != nil {
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: fmt.Sprintf("cannot determine executable: %v", err)}
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: fmt.Sprintf("cannot stat %s: %v", path, err)}
	}
	return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: path}
}

func freecadChecks() []doctor.Check {
	return []doctor.Check{freecadInstallCheck{}, addonCheck{}, rpcCheck{}}
}

type freecadInstallCheck struct{}

func (freecadInstallCheck) Name() string { return "FreeCAD" }

func (c freecadInstallCheck) Run(ctx context.Context) doctor.Result {
	if v := strings.TrimSpace(os.Getenv(domain.EnvFreecadCmd)); v != "" {
		cmd, err := domain.SplitCommand(v)
		if err != nil {
			return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: fmt.Sprintf("%s: %v (the MCP server will not start)", domain.EnvFreecadCmd, err)}
		}
		if _, err := exec.LookPath(cmd[0]); err != nil {
			return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: fmt.Sprintf("%s: %v", domain.EnvFreecadCmd, err)}
		}
		return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: strings.Join(cmd, " ") + " (from " + domain.EnvFreecadCmd + ")"}
	}
	cmd := headless.Detect(ctx)
	if cmd == nil {
		return doctor.Result{Name: c.Name(), Status: doctor.Warn, Detail: "freecadcmd not found; execute_code_headless needs it (set " + domain.EnvFreecadCmd + " if FreeCAD is installed elsewhere)"}
	}
	return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: strings.Join(cmd, " ")}
}

type addonCheck struct{}

func (addonCheck) Name() string { return "FreeCAD addon" }

func (c addonCheck) Run(ctx context.Context) doctor.Result {
	targets := addoninstall.Locate(ctx, freecadCommand())
	if len(targets) == 0 {
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: "FreeCAD's user data directory was not found; start FreeCAD once, then run `" + domain.BinaryName + " install-addon`"}
	}
	want, _, _ := addoninstall.EmbeddedVersion()
	var problems, found []string
	for _, t := range targets {
		got, err := addoninstall.InstalledVersion(t)
		switch {
		case errors.Is(err, os.ErrNotExist):
			problems = append(problems, "not installed in "+t.ModDir())
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", t.AddonDir(), err))
		case got != want:
			problems = append(problems, fmt.Sprintf("%s is %s, this server ships %s", t.AddonDir(), got, want))
		default:
			auto := "manual start"
			if addoninstall.AutoStart(t) {
				auto = "auto-start on"
			}
			found = append(found, fmt.Sprintf("%s %s (%s)", got, t.AddonDir(), auto))
		}
	}
	if len(problems) > 0 {
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: strings.Join(problems, "; ") + "; run `" + domain.BinaryName + " install-addon`"}
	}
	return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: strings.Join(found, "; ")}
}

type rpcCheck struct{}

func (rpcCheck) Name() string { return "FreeCAD RPC server" }

func (c rpcCheck) Run(ctx context.Context) doctor.Result {
	settings, err := serverSettings(ctx)
	if err != nil {
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: err.Error()}
	}
	conn := freecad.NewConnection(settings.Host, settings.Port, settings.Token, 3*time.Second)
	defer conn.Close()
	conn.VersionCheckTimeout = 3 * time.Second
	ok, err := conn.Ping(ctx)
	if err != nil || !ok {
		// FreeCAD not running is normal when nothing is being modelled.
		return doctor.Result{Name: c.Name(), Status: doctor.Warn, Detail: fmt.Sprintf("%s: %s", conn.URL(), rpcProblem(err))}
	}
	if warning := conn.CheckAddonVersion(ctx, version); warning != "" {
		return doctor.Result{Name: c.Name(), Status: doctor.Warn, Detail: warning}
	}
	return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: conn.URL() + " answers; versions match"}
}

// --- Install wizard step ---

type addonPhase int

const (
	addonLocating addonPhase = iota
	addonChoosing
	addonInstalling
	addonDone
)

type addonState struct {
	Phase     addonPhase
	Targets   []addoninstall.Target
	AutoStart bool
	Results   []addonResult
	Report    string
}

type addonLocatedMsg struct{ targets []addoninstall.Target }
type addonInstalledMsg struct{ results []addonResult }

type addonStep struct {
	ctx    context.Context
	dryRun bool
}

func newAddonStep(ctx context.Context, dryRun bool) flow.Step[AppState] {
	return &addonStep{ctx: ctx, dryRun: dryRun}
}

func (s *addonStep) ID() string { return "freecad-addon" }

func (s *addonStep) Title(*AppState) string { return "FreeCAD addon" }

func (s *addonStep) Hints(state *AppState) []struct{ Key, Label string } {
	switch state.Addon.Phase {
	case addonChoosing:
		return []struct{ Key, Label string }{{"space", "toggle"}, {"enter", "install"}, {"esc", "back"}}
	case addonDone:
		return []struct{ Key, Label string }{{"enter", "continue"}}
	}
	return nil
}

func (s *addonStep) Init(state *AppState) tea.Cmd {
	state.Addon = addonState{Phase: addonLocating, AutoStart: true}
	ctx := s.ctx
	return tea.Batch(tui.Spinner(), func() tea.Msg {
		return addonLocatedMsg{targets: addoninstall.Locate(ctx, freecadCommand())}
	})
}

func (s *addonStep) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	a := &state.Addon
	switch m := msg.(type) {
	case addonLocatedMsg:
		a.Targets = m.targets
		if len(a.Targets) == 0 {
			a.Phase, a.Report = addonDone, freecadNotFound
		} else {
			a.Phase = addonChoosing
		}
		return flow.Continue, nil
	case addonInstalledMsg:
		a.Results = m.results
		var b strings.Builder
		if printAddonResults(&b, m.results, a.AutoStart) != 0 {
			// Registration still runs, but the wizard must not exit 0.
			for _, r := range m.results {
				if r.Err != nil {
					state.Failure = fmt.Errorf("FreeCAD addon: %s: %w", r.Target.AddonDir(), r.Err)
					break
				}
			}
		}
		a.Phase, a.Report = addonDone, b.String()
		return flow.Continue, nil
	case tea.KeyMsg:
		key := m.String()
		if key == "ctrl+c" || key == "q" && a.Phase != addonInstalling {
			return flow.Quit, nil
		}
		switch a.Phase {
		case addonChoosing:
			switch key {
			case " ", "space":
				a.AutoStart = !a.AutoStart
			case "esc":
				return flow.Back, nil
			case "enter":
				if s.dryRun {
					var b strings.Builder
					for _, t := range a.Targets {
						fmt.Fprintf(&b, "  would install the FreeCAD addon into %s\n", t.AddonDir())
					}
					a.Phase, a.Report = addonDone, b.String()
					return flow.Continue, nil
				}
				a.Phase = addonInstalling
				targets, auto := a.Targets, a.AutoStart
				return flow.Continue, tea.Batch(tui.Spinner(), func() tea.Msg {
					var results []addonResult
					for _, t := range targets {
						results = append(results, installAddon(t, auto))
					}
					return addonInstalledMsg{results: results}
				})
			}
		case addonDone:
			if key == "enter" {
				return flow.Next, nil
			}
		}
	}
	if tui.IsSpinMsg(msg) && (a.Phase == addonLocating || a.Phase == addonInstalling) {
		state.Spinner.Frame++
		return flow.Continue, tui.Spinner()
	}
	return flow.Continue, nil
}

func (s *addonStep) View(state *AppState) string {
	theme := tui.DefaultTheme
	a := &state.Addon
	var b strings.Builder
	switch a.Phase {
	case addonLocating:
		fmt.Fprintf(&b, "  %s Asking FreeCAD where its addons live...\n", tui.SpinFrame(state.Spinner.Frame))
	case addonInstalling:
		fmt.Fprintf(&b, "  %s Installing the addon...\n", tui.SpinFrame(state.Spinner.Frame))
	case addonChoosing:
		version, _, _ := addoninstall.EmbeddedVersion()
		fmt.Fprintf(&b, "  The MCP server talks to FreeCAD through an addon (version %s).\n  It will be installed into:\n\n", version)
		for _, t := range a.Targets {
			line := "    " + t.AddonDir()
			if v, err := addoninstall.InstalledVersion(t); err == nil {
				if v == version {
					line += "  (" + v + " is installed; it is reinstalled)"
				} else {
					line += "  (replaces " + v + ")"
				}
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
		b.WriteString(tui.ToggleList(theme, []tui.ToggleItem{{
			ID: "autostart", Label: "Start the RPC server with FreeCAD", Tier: "recommended", Checked: a.AutoStart,
		}}, 0))
		b.WriteString(tui.Footer(theme, tui.Hints(theme,
			tui.Hint{Key: "space", Label: "toggle"}, tui.Hint{Key: "enter", Label: "install"}, tui.Hint{Key: "esc", Label: "back"})))
	case addonDone:
		b.WriteString(a.Report)
		if !strings.HasSuffix(a.Report, "\n") {
			b.WriteString("\n")
		}
		b.WriteString(tui.Footer(theme, tui.Hints(theme, tui.Hint{Key: "enter", Label: "continue"})))
	}
	return tui.Section(theme, s.Title(state), b.String())
}

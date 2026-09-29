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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/command"
	"github.com/sairaph/mcp-wizard/doctor"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/headless"
)

// serverSettings reads the MCP server's settings: the environment first,
// then the credential store (the password, and the host and port of FreeCAD
// on another computer).
func serverSettings(ctx context.Context) (domain.Settings, error) {
	token, host, port, err := loadCredentials(ctx)
	if err != nil {
		return domain.Settings{}, fmt.Errorf("read the stored credentials: %w", err)
	}
	return domain.SettingsFromEnv(token, host, port)
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
			noAuto := fs.Bool("no-autostart", false, "turn off starting the RPC server with FreeCAD (default: keep the current setting, on for a new install)")
			refresh := fs.Bool("refresh", false, "only update existing installs, keeping their settings, and rewrite the guide skill where it was installed (used by update)")
			dryRun := fs.Bool("dry-run", false, "show where the addon would go without writing")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			var targets []addoninstall.Target
			if *dir != "" {
				targets = []addoninstall.Target{{UserDataDir: *dir, Source: "flag"}}
			}
			if *refresh {
				return max(refreshAddon(ctx, os.Stdout, targets, *dryRun), refreshSkills(os.Stdout, *dryRun))
			}
			var opts []installOption
			if *noAuto {
				opts = append(opts, withAutoStart(false))
			}
			return installAddonReport(ctx, os.Stdout, targets, *dryRun, opts...)
		},
	})
	r.Register(command.Handler{
		Name:        "uninstall-addon",
		Description: "Remove the FreeCAD addon from every FreeCAD data folder that holds it",
		Usage:       "uninstall-addon [--user-data-dir <dir>]",
		Run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("uninstall-addon", flag.ContinueOnError)
			dir := fs.String("user-data-dir", "", "FreeCAD user data directory (default: every one that holds the addon)")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			targets := []addoninstall.Target{{UserDataDir: *dir, Source: "flag"}}
			if *dir == "" {
				targets = addoninstall.LocateInstalled(ctx, freecadCommand())
			}
			if len(targets) == 0 {
				fmt.Println("  No FreeCAD data folder holds the addon; nothing to remove.")
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
	registerRemoteCommands(r)
	registerSettingsCommand(r)
}

// autoStartChoice is what an install does with the addon's "start the RPC
// server with FreeCAD" setting.
type autoStartChoice int

const (
	// autoStartKeep keeps an existing setting and turns it on when unset.
	autoStartKeep autoStartChoice = iota
	autoStartOn
	autoStartOff
	// autoStartUntouched leaves the settings file alone.
	autoStartUntouched
)

type installOptions struct{ autoStart autoStartChoice }

type installOption func(*installOptions)

// withAutoStart turns auto-start on or off instead of keeping the current
// setting.
func withAutoStart(on bool) installOption {
	return func(o *installOptions) {
		o.autoStart = autoStartOff
		if on {
			o.autoStart = autoStartOn
		}
	}
}

// addonResult is the outcome for one FreeCAD user data directory.
type addonResult struct {
	Target   addoninstall.Target
	Replaced bool
	Previous string
	// AutoStart is the auto-start setting the install left in Target.
	AutoStart bool
	Err       error
}

// installAddon installs the addon into t and turns auto-start on or off as
// the user chose.
func installAddon(t addoninstall.Target, autoStart bool) addonResult {
	choice := autoStartOff
	if autoStart {
		choice = autoStartOn
	}
	return installAddonWith(t, choice)
}

func installAddonWith(t addoninstall.Target, choice autoStartChoice) addonResult {
	r := addonResult{Target: t}
	r.Previous, _ = addoninstall.InstalledVersion(t)
	r.Replaced, r.Err = addoninstall.Install(t)
	if r.Err != nil {
		return r
	}
	current, set := addoninstall.AutoStartSetting(t)
	r.AutoStart = current
	want := current
	switch choice {
	case autoStartUntouched:
		return r
	case autoStartOn:
		want = true
	case autoStartOff:
		want = false
	case autoStartKeep:
		if set {
			return r
		}
		want = true
	}
	if set && want == current {
		return r
	}
	if err := addoninstall.SetAutoStart(t, want); err != nil {
		state := "off"
		if want {
			state = "on"
		}
		r.Err = fmt.Errorf("installed, but could not turn %s auto-start: %w", state, err)
		return r
	}
	r.AutoStart = want
	return r
}

// reportAddonResults reports each install and how to start the RPC server,
// following the auto-start setting each install left (addonResult.AutoStart).
// It returns a process exit code.
func reportAddonResults(w io.Writer, results []addonResult) int {
	version, _, _ := addoninstall.EmbeddedVersion()
	var on, off int
	for _, r := range results {
		switch {
		case r.Err != nil:
		case r.AutoStart:
			on++
		default:
			off++
		}
	}
	mixed := on > 0 && off > 0
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
		note := ""
		if mixed {
			note = "  (auto-start off)"
			if r.AutoStart {
				note = "  (auto-start on)"
			}
		}
		fmt.Fprintf(w, "  [ok]   %s addon %s\n         %s%s\n", verb, version, r.Target.AddonDir(), note)
	}
	if code == 0 && len(results) > 0 {
		switch {
		case mixed:
			fmt.Fprintln(w, "  Restart FreeCAD if it is running. Where auto-start is off, select the MCP Addon\n  workbench and click Start RPC Server.")
		case on > 0:
			fmt.Fprintln(w, "  The RPC server starts with FreeCAD. Restart FreeCAD if it is running.")
		default:
			fmt.Fprintln(w, "  Restart FreeCAD, select the MCP Addon workbench and click Start RPC Server.")
		}
	}
	return code
}

const freecadNotFound = "  FreeCAD was not found. Install FreeCAD (and start it once), then run\n" +
	"  `" + domain.BinaryName + " install-addon`, or pass --user-data-dir with the folder\n" +
	"  FreeCAD.getUserAppDataDir() prints in FreeCAD's Python console."

// installAddonReport installs the addon into targets (located when empty)
// and writes a report. Each target keeps its auto-start setting, which is
// turned on where it is unset, unless an option chooses otherwise. It returns
// a process exit code.
func installAddonReport(ctx context.Context, w io.Writer, targets []addoninstall.Target, dryRun bool, opts ...installOption) int {
	o := installOptions{autoStart: autoStartKeep}
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
		results = append(results, installAddonWith(t, o.autoStart))
	}
	return reportAddonResults(w, results)
}

// refreshAddon updates the addon where it is installed already, in every
// FreeCAD data folder that holds a copy (see addoninstall.LocateInstalled),
// so an updated binary and the addon it talks to stay in step. Settings are
// left alone.
func refreshAddon(ctx context.Context, w io.Writer, targets []addoninstall.Target, dryRun bool) int {
	if len(targets) == 0 {
		targets = addoninstall.LocateInstalled(ctx, freecadCommand())
	}
	if len(targets) == 0 {
		fmt.Fprintf(w, "  No installed FreeCAD addon was found; `%s install-addon` installs it.\n", domain.BinaryName)
		return 0
	}
	want, wantProtocol, _ := addoninstall.EmbeddedVersion()
	var results []addonResult
	for _, t := range targets {
		// A copy is current only when both its version and its protocol match:
		// a pre-release can share the version number but not the protocol.
		// One whose version cannot be read is replaced too.
		got, protocol, err := addoninstall.InstalledRelease(t)
		// A copy is current only when its files are the embedded ones: the
		// same version number can come from a different build.
		if errors.Is(err, os.ErrNotExist) || err == nil && addoninstall.SameRelease(got, protocol) && addoninstall.SameContent(t) {
			continue
		}
		if dryRun {
			from := got
			if err != nil {
				from = "an unreadable copy"
			} else if got == want && protocol == wantProtocol {
				from = got + " (other files)"
			} else if got == want {
				from = fmt.Sprintf("%s (protocol %d)", got, protocol)
			}
			fmt.Fprintf(w, "  would update the FreeCAD addon %s -> %s (protocol %d) in %s\n", from, want, wantProtocol, t.AddonDir())
			continue
		}
		results = append(results, installAddonWith(t, autoStartUntouched))
	}
	if len(results) == 0 {
		if !dryRun {
			fmt.Fprintln(w, "  Every installed copy of the FreeCAD addon is up to date.")
		}
		return 0
	}
	return reportAddonResults(w, results)
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
	return []doctor.Check{freecadInstallCheck{}, freecadGUICheck{}, addonCheck{}, rpcCheck{}}
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

type freecadGUICheck struct{}

func (freecadGUICheck) Name() string { return "FreeCAD GUI" }

// Run checks that start_freecad can find a GUI executable to launch: the
// FREECAD_MCP_FREECAD override if set, else auto-detection on PATH or in the
// standard install locations.
func (c freecadGUICheck) Run(ctx context.Context) doctor.Result {
	if v := strings.TrimSpace(os.Getenv(domain.EnvFreecadGUI)); v != "" {
		cmd, err := domain.SplitCommand(v)
		if err != nil {
			return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: fmt.Sprintf("%s: %v (the MCP server will not start)", domain.EnvFreecadGUI, err)}
		}
		if _, err := exec.LookPath(cmd[0]); err != nil {
			return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: fmt.Sprintf("%s: %v", domain.EnvFreecadGUI, err)}
		}
		return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: strings.Join(cmd, " ") + " (from " + domain.EnvFreecadGUI + ")"}
	}
	cmd := headless.DetectGUI(ctx)
	if cmd == nil {
		return doctor.Result{Name: c.Name(), Status: doctor.Warn, Detail: "FreeCAD's GUI executable was not found; start_freecad needs it (set " + domain.EnvFreecadGUI + " if FreeCAD is installed elsewhere)"}
	}
	return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: strings.Join(cmd, " ")}
}

type addonCheck struct{}

func (addonCheck) Name() string { return "FreeCAD addon" }

// Run checks every installed copy of the addon, in each FreeCAD data folder
// that holds one, and that the folder FreeCAD reports holds one.
func (c addonCheck) Run(ctx context.Context) doctor.Result {
	located := addoninstall.LocateAll(ctx, freecadCommand())
	targets := located.Installed
	if len(targets) == 0 {
		if len(located.DataDirs) == 0 {
			return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: "FreeCAD's user data directory was not found; start FreeCAD once, then run `" + domain.BinaryName + " install-addon`"}
		}
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: "not installed in " + located.DataDirs[0].ModDir() + "; run `" + domain.BinaryName + " install-addon`"}
	}
	want, wantProtocol, _ := addoninstall.EmbeddedVersion()
	// FreeCAD loads addons only from the folder it reports, which comes first
	// and is where install-addon installs. A problem there fails the check.
	// Without a reported folder, install-addon installs into every folder
	// found on disk, so a problem anywhere fails it. A copy in another folder
	// (for another FreeCAD version or profile) only warns, with the command
	// that updates or removes that copy.
	reported := ""
	if first := located.DataDirs[0]; first.Source == "freecad" {
		reported = first.UserDataDir
	}
	var problems, warnings, found []string
	if reported != "" && targets[0].UserDataDir != reported {
		problems = append(problems, "not installed in "+located.DataDirs[0].ModDir()+", the folder FreeCAD loads addons from")
	}
	for _, t := range targets {
		got, protocol, err := addoninstall.InstalledRelease(t)
		problem := ""
		switch {
		case err != nil:
			problem = fmt.Sprintf("%s: %v", t.AddonDir(), err)
		case got != want:
			problem = fmt.Sprintf("%s is %s, this server ships %s", t.AddonDir(), got, want)
		case protocol != wantProtocol:
			// A pre-release can share the version number but not the protocol.
			problem = fmt.Sprintf("%s is %s with protocol %d, this server ships %s with protocol %d",
				t.AddonDir(), got, protocol, want, wantProtocol)
		case !addoninstall.SameContent(t):
			problem = fmt.Sprintf("%s is %s but its files differ from the ones this server ships (a rebuild with the same version); run `%s install-addon`",
				t.AddonDir(), got, domain.BinaryName)
		default:
			auto := "manual start"
			if on, set := addoninstall.AutoStartSetting(t); !set {
				// The settings file is missing or unreadable right now
				// (live check L4): say so plainly, rather than guessing
				// "manual start" from a file this could not actually read.
				auto = "auto-start unknown: its settings file could not be read"
			} else if on {
				auto = "auto-start on"
			}
			found = append(found, fmt.Sprintf("%s %s (%s)", got, t.AddonDir(), auto))
			continue
		}
		if reported == "" || t.UserDataDir == reported {
			problems = append(problems, problem)
		} else {
			warnings = append(warnings, fmt.Sprintf("%s (run `%s install-addon --user-data-dir \"%s\"` to update it, or `%s uninstall-addon --user-data-dir \"%s\"` to remove it)",
				problem, domain.BinaryName, t.UserDataDir, domain.BinaryName, t.UserDataDir))
		}
	}
	switch {
	case len(problems) > 0:
		detail := strings.Join(problems, "; ") + "; run `" + domain.BinaryName + " install-addon`"
		if len(warnings) > 0 {
			detail += "; also " + strings.Join(warnings, "; ")
		}
		return doctor.Result{Name: c.Name(), Status: doctor.Fail, Detail: detail}
	case len(warnings) > 0:
		return doctor.Result{Name: c.Name(), Status: doctor.Warn, Detail: strings.Join(append(found, warnings...), "; ")}
	}
	return doctor.Result{Name: c.Name(), Status: doctor.OK, Detail: strings.Join(found, "; ")}
}

// --- Install wizard step ---
//
// The step only records the choice. The wizard installs the addon after the
// AI clients were registered (see runWizard), so leaving the wizard before
// that point changes nothing on the machine.

type addonPhase int

const (
	addonLocating addonPhase = iota
	addonChoosing
	addonNotFound
)

type addonState struct {
	Phase     addonPhase
	Targets   []addoninstall.Target
	AutoStart bool
	// NotFoundCursor is the cursor row of the addonNotFound choice (0:
	// install FreeCAD first, 1: use FreeCAD on another computer).
	NotFoundCursor int
}

type addonLocatedMsg struct {
	targets   []addoninstall.Target
	autoStart bool
}

// currentAutoStart is where the wizard's auto-start toggle starts for
// targets: on when a target has it on or none has it set, off when every
// target that has it set has it off.
func currentAutoStart(targets []addoninstall.Target) bool {
	anySet := false
	for _, t := range targets {
		on, set := addoninstall.AutoStartSetting(t)
		if on {
			return true
		}
		anySet = anySet || set
	}
	return !anySet
}

type addonStep struct {
	ctx context.Context
}

func newAddonStep(ctx context.Context) flow.Step[AppState] {
	return &addonStep{ctx: ctx}
}

func (s *addonStep) ID() string { return "freecad-addon" }

func (s *addonStep) Title(*AppState) string { return "FreeCAD addon" }

func (s *addonStep) Hints(state *AppState) []struct{ Key, Label string } {
	switch state.Addon.Phase {
	case addonChoosing:
		return []struct{ Key, Label string }{{"space", "toggle"}, {"enter", "continue"}, {"esc", "back"}, {"q", "cancel"}}
	case addonNotFound:
		return []struct{ Key, Label string }{{"↑↓", "move"}, {"enter", "select"}, {"esc", "back"}, {"q", "cancel"}}
	}
	return nil
}

func (s *addonStep) Init(state *AppState) tea.Cmd {
	state.Addon = addonState{Phase: addonLocating, AutoStart: true}
	ctx := s.ctx
	return tea.Batch(tui.Spinner(), func() tea.Msg {
		targets := addoninstall.Locate(ctx, freecadCommand())
		return addonLocatedMsg{targets: targets, autoStart: currentAutoStart(targets)}
	})
}

func (s *addonStep) Update(msg tea.Msg, state *AppState) (flow.Directive, tea.Cmd) {
	a := &state.Addon
	switch m := msg.(type) {
	case addonLocatedMsg:
		a.Targets = m.targets
		a.AutoStart = m.autoStart
		if len(a.Targets) == 0 {
			a.Phase = addonNotFound
		} else {
			a.Phase = addonChoosing
		}
		return flow.Continue, nil
	case tea.KeyMsg:
		key := m.String()
		if key == "ctrl+c" || key == "q" {
			return flow.Quit, nil
		}
		if a.Phase == addonLocating {
			return flow.Continue, nil
		}
		switch key {
		case " ", "space":
			if a.Phase == addonChoosing {
				a.AutoStart = !a.AutoStart
			}
		case "up", "k":
			if a.Phase == addonNotFound && a.NotFoundCursor > 0 {
				a.NotFoundCursor--
			}
		case "down", "j":
			if a.Phase == addonNotFound && a.NotFoundCursor < 1 {
				a.NotFoundCursor++
			}
		case "esc":
			state.Retreating = true
			return flow.Back, nil
		case "enter":
			if a.Phase == addonNotFound {
				if a.NotFoundCursor == 0 {
					// "Install FreeCAD on this computer first" ends the
					// wizard; runWizard prints installFirstText itself once
					// the terminal UI has closed, so it stays on screen.
					state.Connect.InstallFirst = true
					return flow.Quit, nil
				}
				// "Use FreeCAD on another computer": connectSteps runs next.
			}
			return flow.Next, nil
		}
	}
	if tui.IsSpinMsg(msg) && a.Phase == addonLocating {
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
	case addonChoosing:
		version, _, _ := addoninstall.EmbeddedVersion()
		fmt.Fprintf(&b, "  The MCP server talks to FreeCAD through an addon (version %s).\n  When setup finishes, it is installed into:\n\n", version)
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
			tui.Hint{Key: "space", Label: "toggle"}, tui.Hint{Key: "enter", Label: "continue"},
			tui.Hint{Key: "esc", Label: "back"}, tui.Hint{Key: "q", Label: "cancel"})))
	case addonNotFound:
		// Exact wording shown to the user; kept as is rather than paraphrased.
		b.WriteString("  FreeCAD was not found on this computer.\n\n")
		styles := theme.Styles()
		options := []string{"Install FreeCAD on this computer first", "Use FreeCAD on another computer"}
		for i, opt := range options {
			prefix := "  "
			if i == a.NotFoundCursor {
				prefix = styles.Cursor.Render(">") + " "
			}
			b.WriteString("  " + prefix + opt + "\n")
		}
		b.WriteString(tui.Footer(theme, tui.Hints(theme,
			tui.Hint{Key: "↑↓", Label: "move"}, tui.Hint{Key: "enter", Label: "select"},
			tui.Hint{Key: "esc", Label: "back"}, tui.Hint{Key: "q", Label: "cancel"})))
	}
	return tui.Section(theme, s.Title(state), b.String())
}

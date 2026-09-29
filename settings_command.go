package main

// The `settings` command shows and changes the general settings kept in the
// addon settings file (freecad_mcp_settings.json in FreeCAD's user data
// directory), the ones that are not about remote access.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sairaph/mcp-wizard/command"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/domain"
)

func registerSettingsCommand(r *command.Registry) {
	r.Register(command.Handler{
		Name:        "settings",
		Description: "Show or change the general settings (minutes after which a long call moves to the background)",
		Usage:       "settings [--background-after <minutes>] [--user-data-dir <dir>]",
		Run: func(ctx context.Context, args []string) int {
			return settingsCommand(ctx, args, os.Stdout, os.Stderr)
		},
	})
}

// settingsCommand is the `settings` command: without flags it prints the
// current settings, with --background-after it validates and saves that one,
// keeping every other key of the file.
func settingsCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("settings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	minutes := fs.Int("background-after", 0, fmt.Sprintf("minutes a call may keep the agent waiting before it moves to the background and the agent polls it, %d to %d (default: keep the current value, or %d the first time)",
		addoninstall.MinBackgroundAfterMinutes, addoninstall.MaxBackgroundAfterMinutes, addoninstall.DefaultBackgroundAfterMinutes))
	dir := fs.String("user-data-dir", "", "FreeCAD user data directory holding the addon settings (default: the first FreeCAD installation with the addon)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "  settings: unexpected argument %q; the options are --background-after and --user-data-dir\n", fs.Arg(0))
		return 2
	}
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })
	if passed["background-after"] && (*minutes < addoninstall.MinBackgroundAfterMinutes || *minutes > addoninstall.MaxBackgroundAfterMinutes) {
		fmt.Fprintf(stderr, "  settings: --background-after must be %d to %d minutes\n",
			addoninstall.MinBackgroundAfterMinutes, addoninstall.MaxBackgroundAfterMinutes)
		return 2
	}

	target := addoninstall.Target{UserDataDir: *dir, Source: "flag"}
	if *dir == "" {
		targets := addoninstall.LocateInstalled(ctx, freecadCommand())
		if len(targets) == 0 {
			fmt.Fprintln(stderr, "  No installed FreeCAD addon was found; install it first (`"+domain.BinaryName+" install-addon`), or pass --user-data-dir.")
			return 1
		}
		target = targets[0]
	}

	if passed["background-after"] {
		if err := addoninstall.UpdateSettings(target, map[string]any{addoninstall.KeyBackgroundAfter: *minutes}); err != nil {
			fmt.Fprintf(stderr, "  [fail] %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "  [ok]   saved %s\n", addoninstall.SettingsPath(target))
	}
	current, err := addoninstall.ReadGeneralSettings(target)
	if err != nil {
		fmt.Fprintf(stderr, "  [fail] %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "  %s: %d\n", addoninstall.KeyBackgroundAfter, current.BackgroundAfterMinutes)
	fmt.Fprintln(stdout, "  A call that keeps the agent waiting longer moves to the background; the agent then polls it with get_async_status.")
	if passed["background-after"] {
		fmt.Fprintln(stdout, "  FreeCAD reads it at once. Restart AI clients so their freecad-mcp picks it up.")
	}
	return 0
}

package main

// The listener across updates and reconfiguration: `update` and the install
// wizard restart a registered listener so it runs the new binary (on Windows
// the running one is the renamed old file).

import (
	"context"
	"fmt"
	"io"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/autostart"
)

// restartListenerIfRegistered restarts the listener through the OS
// mechanism when it is registered, printing one [ok] or [fail] line, and
// does nothing otherwise. It returns an exit code. Before restarting, it
// re-registers the entry with the FreeCAD this computer currently detects
// (reregisterListener), so a FreeCAD installed, upgraded or removed since
// the entry was last written is picked up rather than a possibly stale
// --freecad path; a failure there is a warning, not a reason to skip the
// restart.
func restartListenerIfRegistered(ctx context.Context, w io.Writer) int {
	state, err := autostart.Status()
	if err != nil {
		listenerRegistrationCheckFailedLine(w, err)
		return 1
	}
	if !state.Registered {
		return 0
	}
	if err := reregisterListener(ctx); err != nil {
		fmt.Fprintf(w, "  [warn] could not record the current FreeCAD again: %v\n", err)
	}
	if err := autostart.Restart(); err != nil {
		fmt.Fprintf(w, "  [fail] restart the listener: %v\n", err)
		return 1
	}
	fmt.Fprintln(w, "  [ok]   restarted the listener")
	return 0
}

// reregisterListener rebuilds the listener's autostart entry (listenerEntry,
// remote.go) and registers it again, without starting or restarting the
// listener itself (restartListenerIfRegistered does that next): only the
// entry's --freecad path and the executable's own location, which may have
// moved with this update, need to be current before the restart. Nothing
// located is not an error: there is then nothing to record.
func reregisterListener(ctx context.Context) error {
	targets := addoninstall.LocateInstalled(ctx, freecadCommand())
	if len(targets) == 0 {
		return nil
	}
	entry, err := listenerEntry(targets)
	if err != nil {
		return err
	}
	return autostart.Register(entry)
}

package main

// `uninstall --all` removes what `install` and the install scripts put on
// the machine: the user-level AI client registrations, the FreeCAD addon
// from every FreeCAD data folder that holds it and its settings file from
// every FreeCAD data folder that has one, the stored password, the listener
// and its files, the cache, and the binary the install script placed
// together with its PATH entry. What `add` wrote into a project stays: its
// client entries (`uninstall --scope project` removes those, per project).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sairaph/mcp-wizard/cli"
	"github.com/sairaph/mcp-wizard/harness"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/autostart"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/headless"
)

func runUninstallAll(ctx context.Context, detector *harness.Detector, cmd cli.Command) int {
	w := os.Stdout
	verb := "removed"
	if cmd.DryRun {
		verb = "would remove"
	}
	fmt.Fprintln(w, "  AI clients")
	clientsCode := runUnattended(ctx, detector, harness.Scope{}, nil, cmd, harness.Absent)
	code := clientsCode

	fmt.Fprintln(w, "\n  FreeCAD addon")
	// The addon folder goes wherever the addon is installed. Its settings
	// file, which can hold the password, goes from every FreeCAD data
	// folder that has one: uninstall-addon keeps it when it removes the
	// addon, so it can outlive the addon folder.
	found := addoninstall.LocateAll(ctx, freecadCommand())
	var paths []string
	for _, t := range found.Installed {
		paths = append(paths, t.AddonDir())
	}
	for _, t := range found.DataDirs {
		paths = append(paths, filepath.Join(t.UserDataDir, addoninstall.SettingsFile))
	}
	code = max(code, removePaths(w, paths, verb, cmd.DryRun))

	fmt.Fprintln(w, "\n  Listener")
	code = max(code, stopListener(w, cmd.DryRun))

	fmt.Fprintln(w, "\n  Stored password and cache")
	paths = []string{
		domain.CredentialPath(),
		domain.ListenerLockPath(),
		domain.ListenerLogPath(),
		domain.ListenerLogPath() + ".1",
		domain.ListenerStdoutPath(),
	}
	if cache, err := headless.CacheDir(); err == nil {
		paths = append(paths, cache)
	}
	// Headless scripts for a Snap FreeCAD, in whichever snap ran them.
	if dir, err := headless.SnapDir("*"); err == nil {
		matches, _ := filepath.Glob(dir)
		paths = append(paths, matches...)
	}
	code = max(code, removePaths(w, paths, verb, cmd.DryRun))

	fmt.Fprintln(w, "\n  Program")
	code = max(code, removeProgram(w, clientsCode != 0, cmd.DryRun))
	return code
}

// removeProgram removes the installed binary unless removing a client
// registration failed: that client still runs the program, so deleting it
// would leave the client pointing at a missing file.
func removeProgram(w io.Writer, clientsFailed, dryRun bool) int {
	if clientsFailed {
		fmt.Fprintln(w, "  [skip] keeping the program and its PATH entry: an AI client registration could not be")
		fmt.Fprintln(w, "         removed (see above), and that client would be left pointing at a deleted program.")
		fmt.Fprintln(w, "         Fix the reported problem (close the client if it holds its config file open),")
		fmt.Fprintf(w, "         then run `%s uninstall --all` again.\n", domain.BinaryName)
		return 1
	}
	return removeInstalledBinary(w, dryRun)
}

// stopListener stops and unregisters the listener through the OS mechanism
// (autostart.Unregister, which stops it first and never errors when nothing
// is registered), reporting the outcome.
func stopListener(w io.Writer, dryRun bool) int {
	state, err := autostart.Status()
	if err != nil {
		listenerRegistrationCheckFailedLine(w, err)
		return 1
	}
	if !state.Registered {
		fmt.Fprintln(w, "  nothing to remove")
		return 0
	}
	if dryRun {
		fmt.Fprintln(w, "  [ok]   would stop and unregister the listener")
		return 0
	}
	if err := autostart.Unregister(); err != nil {
		fmt.Fprintf(w, "  [fail] stop and unregister the listener: %v\n", err)
		return 1
	}
	fmt.Fprintln(w, "  [ok]   stopped and unregistered the listener")
	return 0
}

// removePaths removes each existing path and says so when none exists.
func removePaths(w io.Writer, paths []string, verb string, dryRun bool) int {
	code, found := 0, false
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			found = true
		}
		code = max(code, removePath(w, p, verb, dryRun))
	}
	if !found {
		fmt.Fprintln(w, "  nothing to remove")
	}
	return code
}

// removePath deletes a file or directory, reporting what it did.
func removePath(w io.Writer, path, verb string, dryRun bool) int {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if !dryRun {
		if err := os.RemoveAll(path); err != nil {
			fmt.Fprintf(w, "  [fail] %s: %v\n", path, err)
			return 1
		}
		// The credentials file lives in its own directory; drop it when empty.
		if dir := filepath.Dir(path); filepath.Base(dir) == "."+domain.BinaryName {
			_ = os.Remove(dir)
		}
	}
	fmt.Fprintf(w, "  [ok]   %s %s\n", verb, path)
	return 0
}

// removeInstalledBinary removes the install directory and its PATH entry,
// but only for a binary the install scripts placed: a development build
// elsewhere is left alone.
func removeInstalledBinary(w io.Writer, dryRun bool) int {
	installDir := domain.InstallDir()
	exe, err := harness.ResolveExecutable()
	if err != nil {
		fmt.Fprintf(w, "  [fail] cannot locate this program: %v\n", err)
		return 1
	}
	if !inInstallDir(exe, installDir) {
		fmt.Fprintf(w, "  %s is not in the install directory %s; leaving it in place.\n", exe, installDir)
		return 0
	}
	root := filepath.Dir(installDir)
	if dryRun {
		fmt.Fprintf(w, "  [ok]   would remove %s\n", root)
		fmt.Fprintf(w, "  [ok]   would remove %s from PATH\n", installDir)
		return 0
	}
	code := 0
	removed, err := removeFromUserPath(installDir)
	switch {
	case err != nil:
		fmt.Fprintf(w, "  [fail] PATH: %v\n", err)
		code = 1
	case removed:
		fmt.Fprintf(w, "  [ok]   removed %s from PATH\n", installDir)
	}
	if err := removeInstallRoot(root); err != nil {
		fmt.Fprintf(w, "  [fail] %s: %v\n", root, err)
		return 1
	}
	if runtime.GOOS == "windows" {
		// A running program cannot delete itself on Windows; a helper does
		// it once this process has exited.
		fmt.Fprintf(w, "  [ok]   scheduled removal of %s once this command exits\n", root)
		fmt.Fprintln(w, "         If an AI client is still running freecad-mcp, close it first, or the folder stays.")
	} else {
		fmt.Fprintf(w, "  [ok]   removed %s\n", root)
	}
	fmt.Fprintln(w, "  Open a new terminal for the PATH change to apply.")
	return code
}

// inInstallDir reports whether exe, a path with its symlinks resolved, sits
// in installDir. installDir is resolved the same way, so a home directory
// reached through a symlink still matches; when it cannot be resolved (it
// does not exist) it is compared as given.
func inInstallDir(exe, installDir string) bool {
	if resolved, err := filepath.EvalSymlinks(installDir); err == nil {
		installDir = resolved
	}
	return samePath(filepath.Dir(exe), installDir)
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// removePathListEntry drops every entry equal to dir from a PATH value.
func removePathListEntry(value, dir string, sep string, fold bool) (string, bool) {
	clean := func(s string) string {
		s = strings.TrimRight(strings.TrimSpace(s), `\/`)
		if fold {
			s = strings.ToLower(s)
		}
		return s
	}
	want := clean(dir)
	var kept []string
	removed := false
	for _, entry := range strings.Split(value, sep) {
		if entry != "" && clean(entry) == want {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	return strings.Join(kept, sep), removed
}

// removeRCBlock drops the two lines install.sh appends to a shell profile:
// "# added by freecad-mcp installer" and the export line naming dir.
func removeRCBlock(content, dir string) (string, bool) {
	lines := strings.Split(content, "\n")
	marker := "# added by " + domain.BinaryName + " installer"
	var out []string
	removed := false
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == marker && i+1 < len(lines) && strings.Contains(lines[i+1], dir) {
			i++
			removed = true
			// install.sh writes a blank line before the marker.
			if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) == "" {
				out = out[:n-1]
			}
			continue
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n"), removed
}

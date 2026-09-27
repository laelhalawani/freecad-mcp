// Package headless runs FreeCAD scripts in a separate freecadcmd process.
//
// Heavy OCCT work (helical sweeps, lofts, many-tool booleans) can crash
// OpenCascade. Inside the GUI process that kills FreeCAD together with every
// unsaved document; here it only kills the helper process, and the caller
// gets the exit status and the script's output back.
package headless

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

// FlatpakApp is the Flatpak application id of FreeCAD.
const FlatpakApp = "org.freecad.FreeCAD"

var pathNames = []string{"freecadcmd", "FreeCADCmd", "freecadcmd.exe", "freecad.cmd"}

// Detect finds a headless FreeCAD command: PATH first, then the platform's
// install locations, then the Flatpak. It returns nil when none is found.
func Detect(ctx context.Context) []string {
	for _, name := range pathNames {
		if path, err := exec.LookPath(name); err == nil {
			return []string{path}
		}
	}
	for _, path := range installedCandidates() {
		if isFile(path) {
			return []string{path}
		}
	}
	if flatpak, err := exec.LookPath("flatpak"); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if exec.CommandContext(ctx, flatpak, "info", FlatpakApp).Run() == nil {
			return []string{flatpak, "run", "--command=freecadcmd", FlatpakApp}
		}
	}
	return nil
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// installedCandidates lists freecadcmd paths of standard installations,
// newest first where the directory name carries the version.
func installedCandidates() []string {
	var out []string
	switch runtime.GOOS {
	case "windows":
		for _, dir := range windowsInstallDirs() {
			out = append(out, filepath.Join(dir, "bin", "freecadcmd.exe"), filepath.Join(dir, "bin", "FreeCADCmd.exe"))
		}
	case "darwin":
		for _, app := range []string{"/Applications/FreeCAD.app", filepath.Join(os.Getenv("HOME"), "Applications", "FreeCAD.app")} {
			out = append(out,
				filepath.Join(app, "Contents", "Resources", "bin", "freecadcmd"),
				filepath.Join(app, "Contents", "Resources", "bin", "FreeCADCmd"),
				filepath.Join(app, "Contents", "MacOS", "FreeCADCmd"),
			)
		}
	default:
		out = append(out, "/snap/bin/freecad.cmd", "/usr/bin/freecadcmd", "/usr/local/bin/freecadcmd")
	}
	return out
}

// globDirs returns directories matching pattern, sorted newest (highest
// name) first.
func globDirs(pattern string) []string {
	matches, _ := filepath.Glob(pattern)
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	return matches
}

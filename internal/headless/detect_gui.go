package headless

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// guiPathNames are the GUI executable names looked up on PATH, in order.
var guiPathNames = []string{"freecad", "FreeCAD", "freecad.exe", "FreeCAD.exe"}

// DetectGUI finds FreeCAD's GUI executable: PATH first, then the platform's
// install locations (the same installs Detect uses for freecadcmd), then the
// Flatpak. It returns nil when none is found. start_freecad uses this to
// launch FreeCAD's GUI when FREECAD_MCP_FREECAD is not set.
func DetectGUI(ctx context.Context) []string {
	for _, name := range guiPathNames {
		if path, err := exec.LookPath(name); err == nil {
			return []string{path}
		}
	}
	for _, path := range installedGUICandidates() {
		if isFile(path) {
			return []string{path}
		}
	}
	if flatpak, err := exec.LookPath("flatpak"); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if exec.CommandContext(ctx, flatpak, "info", FlatpakApp).Run() == nil {
			return []string{flatpak, "run", FlatpakApp}
		}
	}
	return nil
}

// installedGUICandidates lists the GUI executable paths of standard
// installations, following the same install directories installedCandidates
// uses for freecadcmd (see windowsInstallDirs).
func installedGUICandidates() []string {
	var out []string
	switch runtime.GOOS {
	case "windows":
		for _, dir := range windowsInstallDirs() {
			out = append(out, filepath.Join(dir, "bin", "freecad.exe"), filepath.Join(dir, "bin", "FreeCAD.exe"))
		}
	case "darwin":
		for _, app := range []string{"/Applications/FreeCAD.app", filepath.Join(os.Getenv("HOME"), "Applications", "FreeCAD.app")} {
			out = append(out, filepath.Join(app, "Contents", "MacOS", "FreeCAD"))
		}
	default:
		out = append(out, "/snap/bin/freecad", "/usr/bin/freecad", "/usr/local/bin/freecad")
	}
	return out
}

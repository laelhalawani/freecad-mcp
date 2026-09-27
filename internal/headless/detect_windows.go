//go:build windows

package headless

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// windowsInstallDirs returns FreeCAD installation directories from the
// uninstall registry entries the FreeCAD installer writes, in registry order,
// then the FreeCAD* folders in Program Files and in the per-user
// %LOCALAPPDATA%\Programs, without duplicates.
func windowsInstallDirs() []string {
	seen := map[string]bool{}
	var dirs []string
	add := func(dir string) {
		dir = filepath.Clean(dir)
		key := strings.ToLower(dir)
		if dir == "." || seen[key] {
			return
		}
		seen[key] = true
		dirs = append(dirs, dir)
	}
	for _, dir := range registryInstallDirs() {
		add(dir)
	}
	for _, env := range []string{"ProgramFiles", "ProgramW6432", "LOCALAPPDATA"} {
		root := os.Getenv(env)
		if root == "" {
			continue
		}
		if env == "LOCALAPPDATA" {
			root = filepath.Join(root, "Programs")
		}
		for _, dir := range globDirs(filepath.Join(root, "FreeCAD*")) {
			add(dir)
		}
	}
	return dirs
}

func registryInstallDirs() []string {
	var dirs []string
	roots := []struct {
		key  registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
	}
	for _, root := range roots {
		k, err := registry.OpenKey(root.key, root.path, registry.ENUMERATE_SUB_KEYS|registry.READ)
		if err != nil {
			continue
		}
		names, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, name := range names {
			sub, err := registry.OpenKey(root.key, root.path+`\`+name, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			display, _, _ := sub.GetStringValue("DisplayName")
			location, _, _ := sub.GetStringValue("InstallLocation")
			uninstall, _, _ := sub.GetStringValue("UninstallString")
			sub.Close()
			if !strings.HasPrefix(strings.ToLower(display), "freecad") {
				continue
			}
			if location = strings.Trim(location, `" `); location != "" {
				dirs = append(dirs, location)
			}
			if uninstall = strings.Trim(uninstall, `" `); uninstall != "" {
				dirs = append(dirs, filepath.Dir(uninstall))
			}
		}
	}
	return dirs
}

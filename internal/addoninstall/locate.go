// Package addoninstall installs the embedded FreeCAD addon into FreeCAD's
// user Mod directory and manages the addon's settings file.
package addoninstall

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sairaph/freecad-mcp/internal/headless"
)

// Target is one FreeCAD user data directory the addon can be installed into.
type Target struct {
	UserDataDir string // FreeCAD.getUserAppDataDir()
	Source      string // "freecad" when FreeCAD reported it, "scan" when found on disk, "flag" when given
}

// ModDir is the directory FreeCAD loads addons from.
func (t Target) ModDir() string { return filepath.Join(t.UserDataDir, "Mod") }

// AddonDir is where the addon lives once installed.
func (t Target) AddonDir() string { return filepath.Join(t.ModDir(), "FreeCADMCP") }

const userDataMarker = "FREECAD_MCP_USERAPPDATA="

// Locate finds the FreeCAD user data directories to install into. It asks
// FreeCAD itself first, because only FreeCAD knows its versioned directory
// (v1-1 and so on); when no FreeCAD command is found it falls back to the
// directories FreeCAD creates on each platform. command overrides the
// detected freecadcmd.
func Locate(ctx context.Context, command []string) []Target {
	var targets []Target
	seen := map[string]bool{}
	add := func(dir, source string) {
		dir = filepath.Clean(dir)
		key := dir
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			key = strings.ToLower(dir)
		}
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, Target{UserDataDir: dir, Source: source})
	}
	if dir := AskFreeCAD(ctx, command); dir != "" {
		add(dir, "freecad")
		return targets
	}
	for _, dir := range scanUserDataDirs() {
		add(dir, "scan")
	}
	return targets
}

// LocateInstalled finds every FreeCAD user data directory that holds this
// addon: the one FreeCAD reports and each one found on disk, so a copy
// installed for another FreeCAD version or profile is found too. command
// overrides the detected freecadcmd.
func LocateInstalled(ctx context.Context, command []string) []Target {
	return LocateAll(ctx, command).Installed
}

// Installations lists the FreeCAD user data directories found for an
// uninstall.
type Installations struct {
	// DataDirs are the directory FreeCAD reports, every versioned or
	// unversioned directory found on disk, and the platform base directories
	// that exist: every place the addon's settings file can be, including
	// where the addon was installed before uninstall-addon removed it.
	DataDirs []Target
	// Installed are the DataDirs whose addon folder holds this addon.
	Installed []Target
}

// LocateAll asks FreeCAD once for its user data directory and combines it
// with the directories found on disk. command overrides the detected
// freecadcmd.
func LocateAll(ctx context.Context, command []string) Installations {
	reported := AskFreeCAD(ctx, command)
	candidates := scanUserDataDirs()
	for _, base := range baseDirs() {
		if info, err := os.Stat(base); err == nil && info.IsDir() {
			candidates = append(candidates, base)
		}
	}
	var all Installations
	all.DataDirs = dataDirsOf(reported, candidates)
	all.Installed = installedIn(all.DataDirs)
	return all
}

// dataDirsOf returns reported (when not "") and the candidates, in that
// order and without duplicates.
func dataDirsOf(reported string, candidates []string) []Target {
	var targets []Target
	seen := map[string]bool{}
	add := func(dir, source string) {
		dir = filepath.Clean(dir)
		key := dir
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			key = strings.ToLower(dir)
		}
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, Target{UserDataDir: dir, Source: source})
	}
	if reported != "" {
		add(reported, "freecad")
	}
	for _, dir := range candidates {
		add(dir, "scan")
	}
	return targets
}

// installedIn keeps the targets whose addon folder holds
// rpc_server/version.py.
func installedIn(targets []Target) []Target {
	var out []Target
	for _, t := range targets {
		if info, err := os.Stat(filepath.Join(t.AddonDir(), filepath.FromSlash(versionFile))); err == nil && !info.IsDir() {
			out = append(out, t)
		}
	}
	return out
}

// AskFreeCAD runs freecadcmd to print FreeCAD.getUserAppDataDir(). It
// returns "" when FreeCAD cannot be found or asked.
func AskFreeCAD(ctx context.Context, command []string) string {
	if len(command) == 0 {
		command = headless.Detect(ctx)
	}
	if len(command) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// The path is printed hex-encoded UTF-8: a console code page could not
	// carry a non-ASCII profile directory intact.
	code := "import FreeCAD; print('" + userDataMarker + "' + FreeCAD.getUserAppDataDir().encode('utf-8').hex())"
	args := append(append([]string{}, command[1:]...), "-c", code)
	cmd := exec.CommandContext(ctx, command[0], args...)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	dir := parseUserDataDir(out)
	if info, err := os.Stat(dir); dir == "" || err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

func parseUserDataDir(out []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if encoded, ok := strings.CutPrefix(line, userDataMarker); ok && encoded != "" {
			dir, err := hex.DecodeString(encoded)
			if err != nil || !utf8.Valid(dir) {
				return ""
			}
			return filepath.Clean(string(dir))
		}
	}
	return ""
}

var versionedDir = regexp.MustCompile(`^v(\d+)-(\d+)$`)

// newerVersion orders "v1-10" after "v1-9", which a string sort does not.
func newerVersion(a, b string) bool {
	ma, mb := versionedDir.FindStringSubmatch(a), versionedDir.FindStringSubmatch(b)
	for i := 1; i <= 2; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

// scanUserDataDirs returns FreeCAD user data directories that exist on disk.
// Versioned subdirectories (v1-1) come first, newest first; the base itself
// is a data directory too when it has no versioned subdirectories or holds
// a Mod directory of its own (an older, unversioned FreeCAD).
func scanUserDataDirs() []string {
	var dirs []string
	for _, base := range baseDirs() {
		info, err := os.Stat(base)
		if err != nil || !info.IsDir() {
			continue
		}
		entries, _ := os.ReadDir(base)
		var versions []string
		for _, e := range entries {
			if e.IsDir() && versionedDir.MatchString(e.Name()) {
				versions = append(versions, e.Name())
			}
		}
		sort.Slice(versions, func(i, j int) bool { return newerVersion(versions[i], versions[j]) })
		for _, v := range versions {
			dirs = append(dirs, filepath.Join(base, v))
		}
		if mod, err := os.Stat(filepath.Join(base, "Mod")); len(versions) == 0 || err == nil && mod.IsDir() {
			dirs = append(dirs, base)
		}
	}
	return dirs
}

// baseDirs lists the per-platform locations FreeCAD keeps user data in.
func baseDirs() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return []string{filepath.Join(appData, "FreeCAD")}
	case "darwin":
		return []string{filepath.Join(home, "Library", "Application Support", "FreeCAD")}
	default:
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(home, ".local", "share")
		}
		return []string{
			filepath.Join(data, "FreeCAD"),
			filepath.Join(home, ".FreeCAD"),
			filepath.Join(home, "snap", "freecad", "common"),
			filepath.Join(home, ".var", "app", headless.FlatpakApp, "data", "FreeCAD"),
		}
	}
}

package addoninstall

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/laelhalawani/freecad-mcp/addon"
)

// SettingsFile is the addon's settings file in FreeCAD's user data directory.
const SettingsFile = "freecad_mcp_settings.json"

var (
	versionRe  = regexp.MustCompile(`(?m)^__version__\s*=\s*["']([^"']+)["']`)
	protocolRe = regexp.MustCompile(`(?m)^PROTOCOL_VERSION\s*=\s*(\d+)`)
)

const versionFile = "rpc_server/version.py"

// EmbeddedVersion returns the version and protocol of the embedded addon.
func EmbeddedVersion() (version string, protocol int, err error) {
	data, err := fs.ReadFile(addon.Files, path.Join(addon.Name, versionFile))
	if err != nil {
		return "", 0, err
	}
	return parseVersion(data)
}

// InstalledVersion returns the version of the addon installed in t, or
// os.ErrNotExist when there is none.
func InstalledVersion(t Target) (string, error) {
	data, err := os.ReadFile(filepath.Join(t.AddonDir(), filepath.FromSlash(versionFile)))
	if err != nil {
		return "", err
	}
	v, _, err := parseVersion(data)
	return v, err
}

func parseVersion(data []byte) (string, int, error) {
	v := versionRe.FindSubmatch(data)
	p := protocolRe.FindSubmatch(data)
	if v == nil || p == nil {
		return "", 0, errors.New("version.py has no __version__ or PROTOCOL_VERSION")
	}
	protocol, err := strconv.Atoi(string(p[1]))
	if err != nil {
		return "", 0, err
	}
	return string(v[1]), protocol, nil
}

func skipped(name string) bool {
	return name == "__pycache__" || strings.HasSuffix(name, ".pyc") || strings.HasPrefix(name, ".")
}

func suffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Install writes the embedded addon into t's Mod directory, replacing an
// existing copy. The new copy is written first and swapped in, so a failed
// write leaves the installed addon untouched. Staging and backup copies live
// in the user data directory beside Mod (the same volume, so renames are
// atomic), never inside Mod, where FreeCAD would load a leftover copy as a
// second addon.
func Install(t Target) (replaced bool, err error) {
	modDir := t.ModDir()
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", modDir, err)
	}
	staging := filepath.Join(t.UserDataDir, ".freecad-mcp-new-"+suffix())
	defer os.RemoveAll(staging)
	if err := writeTree(staging); err != nil {
		return false, err
	}

	final := t.AddonDir()
	var old string
	if _, err := os.Stat(final); err == nil {
		replaced = true
		old = filepath.Join(t.UserDataDir, ".freecad-mcp-old-"+suffix())
		if err := os.Rename(final, old); err != nil {
			return false, fmt.Errorf("move the installed addon aside (close FreeCAD if a file in it is open): %w", err)
		}
	}
	if err := os.Rename(staging, final); err != nil {
		if old != "" {
			if rerr := os.Rename(old, final); rerr != nil {
				return false, fmt.Errorf("install into %s: %w; restoring the previous addon also failed (%v): it is in %s", final, err, rerr, old)
			}
		}
		return false, fmt.Errorf("install into %s: %w", final, err)
	}
	if old != "" {
		if err := os.RemoveAll(old); err != nil {
			return replaced, fmt.Errorf("installed, but the previous copy in %s could not be removed: %w", old, err)
		}
	}
	return replaced, nil
}

func writeTree(dest string) error {
	return fs.WalkDir(addon.Files, addon.Name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != addon.Name && skipped(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, addon.Name), "/")
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(addon.Files, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// Uninstall removes the addon from t. It reports false when none was there.
func Uninstall(t Target) (bool, error) {
	dir := t.AddonDir()
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return true, os.RemoveAll(dir)
}

// SetAutoStart turns the addon's "start the RPC server with FreeCAD" setting
// on or off, keeping every other setting.
func SetAutoStart(t Target, on bool) error {
	file := filepath.Join(t.UserDataDir, SettingsFile)
	settings := map[string]any{}
	// The file holds the addon's auth token, so a rewrite keeps its
	// permissions; a new file is readable by its owner only.
	mode := os.FileMode(0o600)
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("%s is not valid JSON; fix or delete it: %w", file, err)
		}
		if settings == nil { // the file held JSON null
			settings = map[string]any{}
		}
		if info, err := os.Stat(file); err == nil {
			mode = info.Mode().Perm()
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	settings["auto_start_rpc"] = on
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.UserDataDir, 0o755); err != nil {
		return err
	}
	tmp := file + ".tmp-" + suffix()
	if err := writeSynced(tmp, out, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func writeSynced(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// AutoStart reports the addon's auto-start setting in t.
func AutoStart(t Target) bool {
	data, err := os.ReadFile(filepath.Join(t.UserDataDir, SettingsFile))
	if err != nil {
		return false
	}
	var settings struct {
		AutoStart bool `json:"auto_start_rpc"`
	}
	return json.Unmarshal(data, &settings) == nil && settings.AutoStart
}

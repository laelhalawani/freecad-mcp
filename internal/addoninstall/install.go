package addoninstall

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sairaph/freecad-mcp/addon"
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
	v, _, err := InstalledRelease(t)
	return v, err
}

// InstalledRelease returns the version and protocol of the addon installed
// in t, or os.ErrNotExist when there is none. A copy that predates the
// protocol number reports protocol 0, so it differs from any current one.
func InstalledRelease(t Target) (version string, protocol int, err error) {
	data, err := os.ReadFile(filepath.Join(t.AddonDir(), filepath.FromSlash(versionFile)))
	if err != nil {
		return "", 0, err
	}
	v := versionRe.FindSubmatch(data)
	if v == nil {
		return "", 0, errors.New("version.py has no __version__")
	}
	if p := protocolRe.FindSubmatch(data); p != nil {
		if protocol, err = strconv.Atoi(string(p[1])); err != nil {
			return "", 0, err
		}
	}
	return string(v[1]), protocol, nil
}

// SameRelease reports whether an installed copy with this version and
// protocol matches the embedded addon, which then has nothing to update.
func SameRelease(version string, protocol int) bool {
	want, wantProtocol, err := EmbeddedVersion()
	return err == nil && version == want && protocol == wantProtocol
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

// walkEmbedded visits every embedded addon entry Install writes, in a fixed
// order: rel is its slash path below the addon folder ("" for the folder
// itself) and p its path in addon.Files.
func walkEmbedded(visit func(p, rel string, d fs.DirEntry) error) error {
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
		return visit(p, strings.TrimPrefix(strings.TrimPrefix(p, addon.Name), "/"), d)
	})
}

// contentHashFile records, in an installed addon, the hash of the embedded
// files it was written from. Its name starts with a dot, so it is never part
// of the hash itself.
const contentHashFile = ".content-sha256"

// EmbeddedContentHash is the SHA-256 of the files Install writes: each path
// and content, in order. Two builds with the same version number but
// different files differ here.
func EmbeddedContentHash() (string, error) {
	h := sha256.New()
	err := walkEmbedded(func(p, rel string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(addon.Files, p)
		if err != nil {
			return err
		}
		addToHash(h, rel, data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// addToHash adds one file to a content hash.
func addToHash(h hash.Hash, rel string, data []byte) {
	fmt.Fprintf(h, "%s\x00%d\x00", rel, len(data))
	h.Write(data)
}

// embeddedExtensions are the file extensions the embedded addon has (.py,
// .xml, ...), so a stray such as desktop.ini or Thumbs.db an operating system
// adds to the folder is not taken for a difference.
func embeddedExtensions() (map[string]bool, error) {
	exts := map[string]bool{}
	err := walkEmbedded(func(_, rel string, d fs.DirEntry) error {
		if !d.IsDir() {
			exts[strings.ToLower(path.Ext(rel))] = true
		}
		return nil
	})
	return exts, err
}

// installedContentHash is EmbeddedContentHash over the files actually in dir,
// skipping what Install never writes (bytecode FreeCAD leaves, dot-files, and
// files whose extension the embedded addon does not have).
func installedContentHash(dir string) (string, error) {
	exts, err := embeddedExtensions()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && skipped(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !exts[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		addToHash(h, filepath.ToSlash(rel), data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SameContent reports whether the addon installed in t is the embedded one:
// it carries the record Install writes, that record names the files embedded
// now, and the files in the folder still hash to it (no stale, missing or
// edited file). A copy without the record (installed by an earlier release)
// counts as different, so refresh replaces it once.
func SameContent(t Target) bool {
	want, err := EmbeddedContentHash()
	if err != nil {
		return false
	}
	got, err := os.ReadFile(filepath.Join(t.AddonDir(), contentHashFile))
	if err != nil || strings.TrimSpace(string(got)) != want {
		return false
	}
	have, err := installedContentHash(t.AddonDir())
	return err == nil && have == want
}

func writeTree(dest string) error {
	hash, err := EmbeddedContentHash()
	if err != nil {
		return err
	}
	err = walkEmbedded(func(p, rel string, d fs.DirEntry) error {
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
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, contentHashFile), []byte(hash+"\n"), 0o644)
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
	return UpdateSettings(t, map[string]any{KeyAutoStart: on})
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

// RemoveSettings deletes the addon's settings file from t. It reports false
// when there was none.
func RemoveSettings(t Target) (bool, error) {
	err := os.Remove(filepath.Join(t.UserDataDir, SettingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// AutoStart reports the addon's auto-start setting in t.
func AutoStart(t Target) bool {
	on, _ := AutoStartSetting(t)
	return on
}

// AutoStartSetting reports the addon's auto-start setting in t and whether it
// is set at all. It is unset when the settings file is missing or unreadable,
// or holds no true or false value for it.
func AutoStartSetting(t Target) (on, set bool) {
	data, err := os.ReadFile(filepath.Join(t.UserDataDir, SettingsFile))
	if err != nil {
		return false, false
	}
	var settings struct {
		AutoStart *bool `json:"auto_start_rpc"`
	}
	if err := json.Unmarshal(data, &settings); err != nil || settings.AutoStart == nil {
		return false, false
	}
	return *settings.AutoStart, true
}

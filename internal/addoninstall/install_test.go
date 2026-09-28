package addoninstall

import (
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

func TestEmbeddedAddonMatchesTheServer(t *testing.T) {
	version, protocol, err := EmbeddedVersion()
	if err != nil {
		t.Fatal(err)
	}
	if protocol != domain.ProtocolVersion {
		t.Fatalf("addon protocol %d, server protocol %d", protocol, domain.ProtocolVersion)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+`).MatchString(version) {
		t.Fatalf("addon version %q", version)
	}
}

// Addon Manager metadata is one more copy of the version.
func TestPackageXMLVersionMatches(t *testing.T) {
	want, _, _ := EmbeddedVersion()
	data, err := os.ReadFile(filepath.Join("..", "..", "addon", "FreeCADMCP", "package.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Version string `xml:"version"`
	}
	if err := xml.Unmarshal(data, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Version != want {
		t.Fatalf("package.xml version %q, version.py %q", pkg.Version, want)
	}
}

func TestInstallWritesTheAddon(t *testing.T) {
	target := Target{UserDataDir: t.TempDir()}
	replaced, err := Install(target)
	if err != nil || replaced {
		t.Fatalf("Install = %v, %v", replaced, err)
	}
	for _, f := range []string{"InitGui.py", "Init.py", "package.xml", "rpc_server/__init__.py", "rpc_server/rpc_server.py", "rpc_server/version.py"} {
		if _, err := os.Stat(filepath.Join(target.AddonDir(), filepath.FromSlash(f))); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	filepath.WalkDir(target.AddonDir(), func(p string, d os.DirEntry, err error) error {
		if d != nil && (d.Name() == "__pycache__" || filepath.Ext(d.Name()) == ".pyc") {
			t.Errorf("bytecode installed: %s", p)
		}
		return nil
	})
	got, err := InstalledVersion(target)
	want, _, _ := EmbeddedVersion()
	if err != nil || got != want {
		t.Fatalf("InstalledVersion = %q, %v; want %q", got, err, want)
	}
	entries, _ := os.ReadDir(target.ModDir())
	if len(entries) != 1 {
		t.Fatalf("Mod holds %v; staging directories must not remain", entries)
	}
}

func TestInstallReplacesAnOlderCopy(t *testing.T) {
	target := Target{UserDataDir: t.TempDir()}
	stale := filepath.Join(target.AddonDir(), "stale.py")
	if err := os.MkdirAll(target.AddonDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	replaced, err := Install(target)
	if err != nil || !replaced {
		t.Fatalf("Install = %v, %v", replaced, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a file of the old copy survived: %v", err)
	}
	removed, err := Uninstall(target)
	if err != nil || !removed {
		t.Fatalf("Uninstall = %v, %v", removed, err)
	}
	if removed, err := Uninstall(target); err != nil || removed {
		t.Fatalf("second Uninstall = %v, %v", removed, err)
	}
}

func TestSetAutoStartKeepsOtherSettings(t *testing.T) {
	target := Target{UserDataDir: t.TempDir()}
	file := filepath.Join(target.UserDataDir, SettingsFile)
	if err := os.WriteFile(file, []byte(`{"remote_enabled": true, "allowed_ips": "10.0.0.0/8", "auth_token": "abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if AutoStart(target) {
		t.Fatal("auto-start reported on before it was set")
	}
	if err := SetAutoStart(target, true); err != nil {
		t.Fatal(err)
	}
	if !AutoStart(target) {
		t.Fatal("auto-start not on")
	}
	var got map[string]any
	data, _ := os.ReadFile(file)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["remote_enabled"] != true || got["allowed_ips"] != "10.0.0.0/8" || got["auth_token"] != "abc" {
		t.Fatalf("settings lost: %v", got)
	}
}

func TestSetAutoStartRefusesToOverwriteBrokenSettings(t *testing.T) {
	target := Target{UserDataDir: t.TempDir()}
	file := filepath.Join(target.UserDataDir, SettingsFile)
	os.WriteFile(file, []byte("{not json"), 0o644)
	if err := SetAutoStart(target, true); err == nil {
		t.Fatal("broken settings were overwritten")
	}
}

func TestParseUserDataDir(t *testing.T) {
	dir := `C:\Users\Łukasz\AppData\Roaming\FreeCAD\v1-1\`
	out := []byte("FreeCAD 1.1.3, Libs: 1.1.3\r\n" + userDataMarker + hex.EncodeToString([]byte(dir)) + "\r\n")
	if got := parseUserDataDir(out); got != filepath.Clean(dir) {
		t.Fatalf("parsed %q", got)
	}
	for _, bad := range []string{"no marker\n", userDataMarker + "zz\n", userDataMarker + "ff\n"} {
		if got := parseUserDataDir([]byte(bad)); got != "" {
			t.Fatalf("parsed %q from %q", got, bad)
		}
	}
}

func TestVersionedDirsSortNumerically(t *testing.T) {
	v := []string{"v1-9", "v1-10", "v0-21", "v2-0"}
	sort.Slice(v, func(i, j int) bool { return newerVersion(v[i], v[j]) })
	if !reflect.DeepEqual(v, []string{"v2-0", "v1-10", "v1-9", "v0-21"}) {
		t.Fatalf("order = %v", v)
	}
}

func TestInstallLeavesNothingBehindInModOrTheDataDir(t *testing.T) {
	target := Target{UserDataDir: t.TempDir()}
	for i := 0; i < 2; i++ {
		if _, err := Install(target); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{target.UserDataDir, target.ModDir()} {
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("%s holds %v", dir, entries)
		}
	}
}

func TestSetAutoStartHandlesNullAndKeepsPermissions(t *testing.T) {
	target := Target{UserDataDir: t.TempDir()}
	file := filepath.Join(target.UserDataDir, SettingsFile)
	if err := os.WriteFile(file, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetAutoStart(target, true); err != nil {
		t.Fatalf("null settings: %v", err)
	}
	if !AutoStart(target) {
		t.Fatal("auto-start not on")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(file)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600 kept", info.Mode().Perm())
		}
	}
}

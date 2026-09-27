package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemovePathListEntry(t *testing.T) {
	dir := `C:\Users\me\AppData\Local\freecad-mcp\bin`
	value := `C:\Users\me\AppData\Local\freecad-mcp\bin;C:\tools;;C:\USERS\ME\APPDATA\LOCAL\FREECAD-MCP\BIN\;C:\other`
	got, removed := removePathListEntry(value, dir, ";", true)
	if !removed || got != `C:\tools;;C:\other` {
		t.Fatalf("got %q, %v", got, removed)
	}
	if _, removed := removePathListEntry(`C:\tools`, dir, ";", true); removed {
		t.Fatal("removed an entry that was not there")
	}
	// Case-sensitive systems keep a differently cased entry.
	if got, removed := removePathListEntry("/home/me/.freecad-mcp/bin:/usr/bin:/HOME/ME/.FREECAD-MCP/BIN", "/home/me/.freecad-mcp/bin", ":", false); !removed || got != "/usr/bin:/HOME/ME/.FREECAD-MCP/BIN" {
		t.Fatalf("got %q, %v", got, removed)
	}
}

func TestRemoveRCBlockRemovesExactlyWhatInstallShAdded(t *testing.T) {
	dir := "/home/me/.freecad-mcp/bin"
	before := "alias ll='ls -l'\nexport EDITOR=vim\n"
	added := before + "\n# added by freecad-mcp installer\nexport PATH=\"" + dir + ":$PATH\"\n"
	got, removed := removeRCBlock(added, dir)
	if !removed || got != before {
		t.Fatalf("removed=%v\ngot:\n%q\nwant:\n%q", removed, got, before)
	}
	// A user's own line naming the directory is not touched.
	own := before + "export PATH=\"" + dir + ":$PATH\"\n"
	if got, removed := removeRCBlock(own, dir); removed || got != own {
		t.Fatalf("changed a line the installer did not add: %q", got)
	}
}

func TestRemovePathReportsAndDeletes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "x.json")
	os.WriteFile(file, []byte("{}"), 0o600)
	var out bytes.Buffer
	if code := removePath(&out, file, "would remove", true); code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("dry run deleted the file")
	}
	if code := removePath(&out, file, "removed", false); code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("file not deleted")
	}
	if code := removePath(&out, file, "removed", false); code != 0 {
		t.Fatal("a missing path is not an error")
	}
	if !strings.Contains(out.String(), "would remove "+file) || !strings.Contains(out.String(), "removed "+file) {
		t.Fatalf("output:\n%s", out.String())
	}
}

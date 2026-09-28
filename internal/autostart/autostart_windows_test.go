//go:build windows

package autostart

import (
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

// TestTaskArgumentsOmitsDefaultRPCPort covers live check L1 (schtasks
// registration): the task's command line adds --rpc-port only when it
// differs from the addon default, and quotes an argument that needs it
// (syscall.EscapeArg), such as a UserDataDir with an internal space.
func TestTaskArgumentsOmitsDefaultRPCPort(t *testing.T) {
	got := taskArguments(Entry{UserDataDir: `C:\Users\a\AppData\Roaming\FreeCAD`})
	want := `listen --user-data-dir C:\Users\a\AppData\Roaming\FreeCAD`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTaskArgumentsIncludesNonDefaultRPCPort(t *testing.T) {
	got := taskArguments(Entry{UserDataDir: `C:\data`, RPCPort: 9999})
	if !strings.Contains(got, "--rpc-port 9999") {
		t.Fatalf("got %q, want it to contain --rpc-port 9999", got)
	}
	if strings.Contains(taskArguments(Entry{UserDataDir: `C:\data`, RPCPort: domain.DefaultRPCPort}), "--rpc-port") {
		t.Fatal("the addon's own default port should not be passed explicitly")
	}
}

func TestTaskArgumentsQuotesPathsWithSpaces(t *testing.T) {
	got := taskArguments(Entry{UserDataDir: `C:\Users\a b\FreeCAD`, FreeCADPath: `C:\Program Files\FreeCAD\bin\freecad.exe`})
	if !strings.Contains(got, `"C:\Users\a b\FreeCAD"`) {
		t.Fatalf("user data dir not quoted: %q", got)
	}
	if !strings.Contains(got, `--freecad "C:\Program Files\FreeCAD\bin\freecad.exe"`) {
		t.Fatalf("freecad path not quoted: %q", got)
	}
}

// TestTaskDefinitionXMLPriority covers live check L2: the task XML sets
// <Priority>4</Priority> (Task Scheduler's "normal"), not the default 7
// (below normal) a task gets with no Priority element at all.
func TestTaskDefinitionXMLPriority(t *testing.T) {
	data, err := taskDefinitionXML(Entry{UserDataDir: `C:\data`}, `DOMAIN\user`)
	if err != nil {
		t.Fatal(err)
	}
	doc := utf16LEToString(t, data)
	if !strings.Contains(doc, "<Priority>4</Priority>") {
		t.Fatalf("task XML missing <Priority>4</Priority>:\n%s", doc)
	}
}

// TestTaskDefinitionXMLIsUTF16LEWithBOM covers the encoding schtasks /xml
// requires: a leading 0xFF 0xFE byte order mark, then UTF-16LE, so a user
// name or path with non-ASCII characters survives.
func TestTaskDefinitionXMLIsUTF16LEWithBOM(t *testing.T) {
	data, err := taskDefinitionXML(Entry{UserDataDir: `C:\data`}, `DOMAIN\usér`)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xFE {
		t.Fatalf("missing UTF-16LE BOM: % x", data[:min(4, len(data))])
	}
	doc := utf16LEToString(t, data)
	if !strings.Contains(doc, `DOMAIN\usér`) {
		t.Fatalf("user id not round-tripped: %s", doc)
	}
	if !strings.Contains(doc, `<UserId>DOMAIN\usér</UserId>`) {
		t.Fatalf("logon trigger user id missing:\n%s", doc)
	}
}

// utf16LEToString decodes a UTF-16LE buffer with a leading BOM back to a
// Go string, the reverse of utf16LEWithBOM, so a test can assert on the XML
// text directly.
func utf16LEToString(t *testing.T, data []byte) string {
	t.Helper()
	if len(data) < 2 {
		t.Fatal("buffer too short for a BOM")
	}
	data = data[2:] // drop 0xFF 0xFE
	if len(data)%2 != 0 {
		t.Fatal("odd number of bytes after the BOM")
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = uint16(data[2*i]) | uint16(data[2*i+1])<<8
	}
	return string(utf16.Decode(units))
}

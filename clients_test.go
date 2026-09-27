package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/cli"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/installer"
)

// The tests use project scope in a temporary directory, so they never touch
// the real client configs of the machine running them.

const testCommand = "/opt/freecad-mcp/bin/freecad-mcp"

const editedCursorConfig = `{
  "mcpServers": {
    "freecad": {
      "command": "/opt/freecad-mcp/bin/freecad-mcp",
      "args": ["mcp"],
      "env": {"TRANSPORT": "stdio", "FREECAD_MCP_HOST": "192.168.1.100"}
    },
    "other": {"command": "other-server"}
  }
}
`

// The Python server this project was forked from registers the same name.
const foreignCursorConfig = `{
  "mcpServers": {
    "freecad": {"command": "uvx", "args": ["freecad-mcp"]}
  }
}
`

const outdatedCursorConfig = `{
  "mcpServers": {
    "freecad": {"command": "/home/me/src/freecad-mcp/freecad-mcp", "args": ["mcp"], "env": {"TRANSPORT": "stdio"}}
  }
}
`

func testDetector(t *testing.T) *harness.Detector {
	t.Helper()
	old := ourCommand
	ourCommand = func() string { return testCommand }
	t.Cleanup(func() { ourCommand = old })
	det, err := harness.New(harness.ServerSpec{
		Name:    "freecad",
		Command: testCommand,
		Args:    []string{"mcp"},
		Env:     map[string]string{"TRANSPORT": "stdio"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return det
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// servers returns the entries under topKey in a JSON client config.
func servers(t *testing.T, path, topKey string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	m, _ := doc[topKey].(map[string]any)
	return m
}

// cursorEntry returns what clientEntries reports for Cursor in a project
// whose .cursor/mcp.json holds config.
func cursorEntry(t *testing.T, config string) (clientEntry, bool) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".cursor", "mcp.json"), config)
	det, scope := testDetector(t), harness.ProjectScopeDir(dir)
	entries := clientEntries(context.Background(), det, scope, det.DetectIn(context.Background(), scope))
	if _, found := entries["vscode"]; found {
		t.Fatal("a client without a config file was reported as having an entry")
	}
	e, found := entries["cursor"]
	return e, found
}

func TestEntriesAreClassifiedByWhatTheyRun(t *testing.T) {
	cases := []struct {
		name, config string
		want         entryKind
	}{
		{"edited by hand", editedCursorConfig, entryEdited},
		{"another program", foreignCursorConfig, entryForeign},
		{"another freecad-mcp", outdatedCursorConfig, entryOutdated},
		{"as install writes it", `{"mcpServers": {"freecad": {"command": "/opt/freecad-mcp/bin/freecad-mcp", "args": ["mcp"], "env": {"TRANSPORT": "stdio"}}}}`, entryConfigured},
	}
	for _, c := range cases {
		e, found := cursorEntry(t, c.config)
		if !found || e.kind != c.want {
			t.Errorf("%s: found=%v kind=%v, want %v", c.name, found, e.kind, c.want)
		}
	}
	if _, found := cursorEntry(t, `{"mcpServers": {"other": {"command": "x"}}}`); found {
		t.Error("an entry under another name was reported")
	}
}

func TestEntryCommandReadsEveryConfigFormat(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"a.json": `{"mcpServers": {"freecad": {"command": "/x/freecad-mcp"}}}`,
		// JSONC with a comment, OpenCode's command array under "mcp"
		"b.jsonc": "{\n // comment\n \"mcp\": {\"freecad\": {\"type\": \"local\", \"command\": [\"/x/freecad-mcp\", \"mcp\"]}},\n}",
		// Zed's own form
		"c.json": `{"context_servers": {"freecad": {"command": {"path": "/x/freecad-mcp", "args": ["mcp"]}}}}`,
		"d.toml": "[mcp_servers.freecad]\ncommand = \"/x/freecad-mcp\"\nargs = [\"mcp\"]\n",
		"e.yaml": "name: Local\nmcpServers:\n  - name: other\n    command: o\n  - name: freecad\n    command: /x/freecad-mcp\n",
	}
	for file, content := range cases {
		path := filepath.Join(dir, file)
		writeFile(t, path, content)
		if got, ok := entryCommand(path, "freecad"); !ok || got != "/x/freecad-mcp" {
			t.Errorf("%s: %q %v", file, got, ok)
		}
	}
	// Claude Code nests per-project entries; only the top level counts.
	path := filepath.Join(dir, "claude.json")
	writeFile(t, path, `{"projects": {"/p": {"mcpServers": {"freecad": {"command": "/x/freecad-mcp"}}}}}`)
	if got, ok := entryCommand(path, "freecad"); ok {
		t.Errorf("a nested project entry was read as the global one: %q", got)
	}
}

func TestIsOurBinary(t *testing.T) {
	for command, want := range map[string]bool{
		"/home/me/.freecad-mcp/bin/freecad-mcp":                     true,
		`C:\Users\me\AppData\Local\freecad-mcp\bin\freecad-mcp.exe`: true,
		"freecad-mcp":           true,
		"uvx":                   false,
		"/usr/bin/freecad-mcp2": false,
		"python":                false,
	} {
		if got := isOurBinary(command); got != want {
			t.Errorf("isOurBinary(%q) = %v, want %v", command, got, want)
		}
	}
}

func TestUninstallRemovesAnEditedEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".cursor", "mcp.json")
	writeFile(t, path, editedCursorConfig)

	code := registerUnattended(context.Background(), testDetector(t), harness.ProjectScopeDir(dir), nil, cli.Command{}, harness.Absent)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	got := servers(t, path, "mcpServers")
	if _, ok := got["freecad"]; ok {
		t.Fatal("the edited entry is still there")
	}
	if _, ok := got["other"]; !ok {
		t.Fatal("another server's entry was removed")
	}
}

func TestUninstallLeavesAnotherProgramsEntryUnlessNamed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".cursor", "mcp.json")
	writeFile(t, path, foreignCursorConfig)
	det, scope := testDetector(t), harness.ProjectScopeDir(dir)

	if code := registerUnattended(context.Background(), det, scope, nil, cli.Command{}, harness.Absent); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if raw, _ := os.ReadFile(path); string(raw) != foreignCursorConfig {
		t.Fatalf("uninstall changed another program's entry:\n%s", raw)
	}
	if code := registerUnattended(context.Background(), det, scope, nil, cli.Command{Clients: []string{"cursor"}}, harness.Absent); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if _, ok := servers(t, path, "mcpServers")["freecad"]; ok {
		t.Fatal("--clients cursor did not remove the entry")
	}
}

func TestUninstallHonoursClients(t *testing.T) {
	dir := t.TempDir()
	cursor := filepath.Join(dir, ".cursor", "mcp.json")
	vscode := filepath.Join(dir, ".vscode", "mcp.json")
	writeFile(t, cursor, editedCursorConfig)
	writeFile(t, vscode, `{"servers": {"freecad": {"type": "stdio", "command": "freecad-mcp", "args": ["mcp"]}}}`)

	code := registerUnattended(context.Background(), testDetector(t), harness.ProjectScopeDir(dir), nil, cli.Command{Clients: []string{"Cursor"}}, harness.Absent)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if _, ok := servers(t, cursor, "mcpServers")["freecad"]; ok {
		t.Fatal("the named client's entry was not removed")
	}
	if _, ok := servers(t, vscode, "servers")["freecad"]; !ok {
		t.Fatal("a client not named in --clients lost its entry")
	}
}

func TestUninstallRejectsAnUnknownClient(t *testing.T) {
	code := registerUnattended(context.Background(), testDetector(t), harness.ProjectScopeDir(t.TempDir()), nil, cli.Command{Clients: []string{"no-such-client"}}, harness.Absent)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
}

func TestUninstallAllRefusesClients(t *testing.T) {
	if code := runUninstall(context.Background(), cli.Command{All: true, Clients: []string{"cursor"}, DryRun: true}); code != 2 {
		t.Fatalf("exit code %d, want 2: --all would delete the program other clients still run", code)
	}
}

func TestInstallKeepsAnEditedEntryUnlessNamed(t *testing.T) {
	for _, config := range []string{editedCursorConfig, foreignCursorConfig} {
		dir := t.TempDir()
		path := filepath.Join(dir, ".cursor", "mcp.json")
		writeFile(t, path, config)
		det, scope := testDetector(t), harness.ProjectScopeDir(dir)

		registerUnattended(context.Background(), det, scope, nil, cli.Command{}, harness.Present)
		if raw, _ := os.ReadFile(path); string(raw) != config {
			t.Fatalf("a default install changed the entry:\n%s", raw)
		}

		if code := registerUnattended(context.Background(), det, scope, nil, cli.Command{Clients: []string{"cursor"}}, harness.Present); code != 0 {
			t.Fatalf("exit code %d", code)
		}
		entry, _ := servers(t, path, "mcpServers")["freecad"].(map[string]any)
		if entry["command"] != testCommand {
			t.Fatalf("--clients cursor did not replace the entry: %v", entry)
		}
	}
}

func TestInstallAllReplacesAnEditedEntryButNotAnotherProgramsEntry(t *testing.T) {
	for config, replaced := range map[string]bool{editedCursorConfig: true, foreignCursorConfig: false} {
		dir := t.TempDir()
		path := filepath.Join(dir, ".cursor", "mcp.json")
		writeFile(t, path, config)
		registerUnattended(context.Background(), testDetector(t), harness.ProjectScopeDir(dir), nil, cli.Command{All: true}, harness.Present)
		raw, _ := os.ReadFile(path)
		if got := string(raw) != config; got != replaced {
			t.Fatalf("--all replaced=%v, want %v:\n%s", got, replaced, raw)
		}
	}
}

func TestInstallUpdatesAnOutdatedEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".cursor", "mcp.json")
	writeFile(t, path, outdatedCursorConfig)
	registerUnattended(context.Background(), testDetector(t), harness.ProjectScopeDir(dir), nil, cli.Command{}, harness.Present)
	entry, _ := servers(t, path, "mcpServers")["freecad"].(map[string]any)
	if entry["command"] != testCommand {
		t.Fatalf("an entry running another freecad-mcp was not pointed at this one: %v", entry)
	}
}

func TestWizardStartsWithAKeptClientUnticked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".cursor", "mcp.json"), editedCursorConfig)
	ctx := context.Background()
	det, scope := testDetector(t), harness.ProjectScopeDir(dir)
	step := harnessSelection{
		Step: installer.HarnessStep(ctx, det, harnessState, installer.HarnessStepOptions{AllDetected: true, Scope: scope}),
		name: "freecad",
		findUnticked: func(hs []harness.Harness) map[harness.ID]bool {
			return untickedClients(clientEntries(ctx, det, scope, hs))
		},
	}
	state := &AppState{}
	batch, ok := step.Init(state)().(tea.BatchMsg)
	if !ok {
		t.Fatal("Init did not return a batch")
	}
	for _, cmd := range batch {
		if cmd != nil {
			step.Update(cmd(), state)
		}
	}
	if state.Harness.Selected == nil || state.harnessDetecting {
		t.Fatal("detection did not finish")
	}
	if state.Harness.Selected["cursor"] {
		t.Fatal("the client with an edited entry starts ticked, so registering would drop the user's changes")
	}
	if len(state.UntickedClients) != 1 || state.UntickedClients[0] != "Cursor" {
		t.Fatalf("UntickedClients = %v", state.UntickedClients)
	}
	if view := step.View(state); !strings.Contains(view, "starts unticked so the entry is kept") || !strings.Contains(view, "Cursor") {
		t.Fatalf("no note explains the unticked client:\n%s", view)
	}
}

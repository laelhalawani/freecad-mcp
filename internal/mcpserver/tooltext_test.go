package mcpserver

import (
	"context"
	"encoding/json"
	"io/fs"
	"regexp"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/guide"
)

// listAllTools lists every tool, the remote-only ones included: a host that is
// not loopback turns them on.
func listAllTools(t *testing.T) map[string]*toolInfo {
	t.Helper()
	cs := session(t, domain.Settings{Host: "192.0.2.1", Port: 9})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*toolInfo{}
	for _, tool := range res.Tools {
		data, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		out[tool.Name] = &toolInfo{
			description: tool.Description, schemaBytes: len(data), schema: &schema,
			titled: tool.Title != "" && tool.Annotations != nil && tool.Annotations.Title != "",
		}
	}
	return out
}

type toolInfo struct {
	description string
	schemaBytes int
	schema      *jsonschema.Schema
	titled      bool
}

// walkParams calls visit with every parameter of s, nested ones too.
func walkParams(prefix string, s *jsonschema.Schema, visit func(path, name string, p *jsonschema.Schema)) {
	for name, p := range s.Properties {
		visit(prefix+name, name, p)
		if p.Items != nil && len(p.Items.Properties) > 0 {
			walkParams(prefix+name+"[].", p.Items, visit)
		}
	}
}

func TestToolTextStaysWithinTheStandard(t *testing.T) {
	tools := listAllTools(t)
	if len(tools) != len(toolTexts) {
		t.Errorf("%d tools listed, %d have text", len(tools), len(toolTexts))
	}
	for name, info := range tools {
		if _, ok := toolTexts[name]; !ok {
			t.Errorf("%s has no entry in toolTexts", name)
		}
		if n := len(info.description); n == 0 || n > maxDescriptionBytes {
			t.Errorf("%s: description is %d bytes, want 1 to %d", name, n, maxDescriptionBytes)
		}
		if info.schemaBytes > maxSchemaBytes {
			t.Errorf("%s: schema is %d bytes, want at most %d", name, info.schemaBytes, maxSchemaBytes)
		}
		if !info.titled {
			t.Errorf("%s: missing title or annotations", name)
		}
		walkParams("", info.schema, func(path, _ string, p *jsonschema.Schema) {
			if n := len(p.Description); n == 0 || n > maxParamDescriptionBytes {
				t.Errorf("%s.%s: description is %d bytes, want 1 to %d", name, path, n, maxParamDescriptionBytes)
			}
		})
	}
	if n := len([]rune(serverInstructions)); n > maxInstructionsChars {
		t.Errorf("server instructions are %d characters, want at most %d", n, maxInstructionsChars)
	}
}

// snakeToken matches names such as create_object and include_screenshot.
var snakeToken = regexp.MustCompile(`\b[a-z]+(?:_[a-z0-9]+)+\b`)

// otherWords are the snake_case words the texts use that are neither a tool
// nor a parameter: reply fields, states, product names and property names.
// Every other snake_case word must be a tool or a parameter, so a misspelt or
// invented tool name fails here.
var otherWords = map[string]bool{
	"already_running": true, "not_running": true, "version_check": true, "gui_dispatch": true,
	"asset_creation_strategy": true, "object_name": true, "object_names": true,
	"parts_library": true,
}

// TestTextsNameOnlyExistingTools fails when a tool description, parameter
// description, the server instructions or a file of the guide uses a
// snake_case word that is not a tool, a parameter, a repair step or one of
// otherWords.
func TestTextsNameOnlyExistingTools(t *testing.T) {
	tools := listAllTools(t)
	known := map[string]bool{}
	for w := range otherWords {
		known[w] = true
	}
	for _, step := range meshRepairSteps {
		known[step] = true
	}
	for name, info := range tools {
		known[name] = true
		walkParams("", info.schema, func(_, param string, _ *jsonschema.Schema) { known[param] = true })
	}

	check := func(source, text string) {
		for _, tok := range snakeToken.FindAllString(text, -1) {
			if !known[tok] {
				t.Errorf("%s uses %q, which is not a tool, parameter or listed word", source, tok)
			}
		}
	}
	check("server instructions", serverInstructions)
	for name, info := range tools {
		check(name+" description", info.description)
		walkParams("", info.schema, func(path, _ string, p *jsonschema.Schema) {
			check(name+"."+path, p.Description)
		})
	}
	err := fs.WalkDir(guide.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(guide.FS(), path)
		if err != nil {
			return err
		}
		check("guide "+path, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

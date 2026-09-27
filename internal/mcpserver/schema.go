package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ViewName is a named camera orientation for screenshots.
type ViewName string

// ViewNames are the orientations the addon's get_active_screenshot accepts.
var ViewNames = []any{"Isometric", "Front", "Top", "Right", "Back", "Left", "Bottom", "Dimetric", "Trimetric"}

const defaultView = "Isometric"

func viewOrDefault(v string) string {
	if v == "" {
		return defaultView
	}
	return v
}

// Properties is a JSON object of FreeCAD property values. It keeps the JSON
// number kinds, so 30 reaches FreeCAD as an int and 30.5 as a float, as a
// Python client would send them; integer properties reject floats.
type Properties struct {
	values map[string]any
}

// UnmarshalJSON decodes the object with json.Number for numbers.
func (p *Properties) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		p.values = nil
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("properties must be a JSON object: %w", err)
	}
	p.values = m
	return nil
}

// MarshalJSON encodes the object.
func (p Properties) MarshalJSON() ([]byte, error) {
	if p.values == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(p.values)
}

// Map returns the values, never nil.
func (p *Properties) Map() map[string]any {
	if p == nil || p.values == nil {
		return map[string]any{}
	}
	return p.values
}

// rawProperties reads obj_properties from the request's original arguments.
// The typed input cannot be used for this: when the SDK applies schema
// defaults it re-encodes the arguments from a map[string]any, which turns
// 1.0 into 1 before Properties.UnmarshalJSON sees it.
func rawProperties(req *mcp.CallToolRequest) (map[string]any, error) {
	var args struct {
		Properties Properties `json:"obj_properties"`
	}
	if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
	}
	return args.Properties.Map(), nil
}

var typeSchemas = map[reflect.Type]*jsonschema.Schema{
	reflect.TypeFor[ViewName]():   {Type: "string", Enum: ViewNames},
	reflect.TypeFor[Properties](): {Type: "object"},
}

// inputSchema infers the schema of T, applying the custom types above and
// the given property defaults (JSON literals).
func inputSchema[T any](defaults map[string]string) *jsonschema.Schema {
	s, err := jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: typeSchemas})
	if err != nil {
		panic(err)
	}
	for name, value := range defaults {
		prop, ok := s.Properties[name]
		if !ok {
			panic(fmt.Sprintf("inputSchema: no property %q", name))
		}
		prop.Default = json.RawMessage(value)
	}
	return s
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

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

// orientationNames returns ViewNames as strings, for withEnum.
func orientationNames() []string {
	names := make([]string, len(ViewNames))
	for i, v := range ViewNames {
		names[i] = v.(string)
	}
	return names
}

// maxViewSize bounds get_view's width and height, as the addon clamps them
// (MAX_SCREENSHOT_EDGE in view_manager.py). A larger image rarely fits the
// 1 MiB a reply carries; imageBudget checks the actual size.
const maxViewSize = 2048

func viewOrDefault(v string) string {
	if v == "" {
		return defaultView
	}
	return v
}

// viewString returns the view an optional view_name argument names, or "".
func viewString(v *ViewName) string {
	if v == nil {
		return ""
	}
	return string(*v)
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
// the given property defaults (JSON literals). An optional enum property,
// such as a *ViewName, accepts only its listed values: the inferred "null"
// alternative is dropped, since null is not one of them.
func inputSchema[T any](defaults map[string]string) *jsonschema.Schema {
	s, err := jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: typeSchemas})
	if err != nil {
		panic(err)
	}
	for _, prop := range s.Properties {
		if len(prop.Enum) > 0 {
			dropNullAlt(prop)
		}
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

// withRange sets an inclusive minimum and maximum on the named numeric
// properties of s, which struct tags cannot express.
func withRange(s *jsonschema.Schema, lo, hi float64, names ...string) *jsonschema.Schema {
	for _, name := range names {
		prop, ok := s.Properties[name]
		if !ok {
			panic(fmt.Sprintf("withRange: no property %q", name))
		}
		prop.Minimum, prop.Maximum = jsonschema.Ptr(lo), jsonschema.Ptr(hi)
	}
	return s
}

// withPositiveMax requires the named numeric property of s to be greater
// than 0 and at most hi.
func withPositiveMax(s *jsonschema.Schema, name string, hi float64) *jsonschema.Schema {
	prop, ok := s.Properties[name]
	if !ok {
		panic(fmt.Sprintf("withPositiveMax: no property %q", name))
	}
	prop.ExclusiveMinimum, prop.Maximum = jsonschema.Ptr(0.0), jsonschema.Ptr(hi)
	return s
}

// withEnum restricts the named property of s to values: a string property
// directly, an array property through its items. As in inputSchema, the
// inferred "null" alternative of an optional property is dropped, since null
// is not one of the values.
func withEnum(s *jsonschema.Schema, name string, values ...string) *jsonschema.Schema {
	prop, ok := s.Properties[name]
	if !ok {
		panic(fmt.Sprintf("withEnum: no property %q", name))
	}
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	dropNullAlt(prop)
	target := prop
	if prop.Type == "array" {
		if prop.Items == nil {
			panic(fmt.Sprintf("withEnum: array property %q has no items schema", name))
		}
		target = prop.Items
	}
	target.Enum = enum
	return s
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

// mergeDefaults combines several property-default maps (as inputSchema
// takes) into one, later maps overriding earlier ones for the same key.
func mergeDefaults(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// withItemRange requires the named array property of s to have between lo
// and hi items, inclusive. Struct tags cannot express this.
func withItemRange(s *jsonschema.Schema, name string, lo, hi int) *jsonschema.Schema {
	prop, ok := s.Properties[name]
	if !ok {
		panic(fmt.Sprintf("withItemRange: no property %q", name))
	}
	prop.MinItems, prop.MaxItems = jsonschema.Ptr(lo), jsonschema.Ptr(hi)
	dropNullAlt(prop)
	return s
}

// withMinItems requires the named array property of s to have at least
// atLeast items, dropping the inferred null alternative an optional-looking
// slice property gets (as withEnum drops it for an enum), since the property
// is only ever a JSON array.
func withMinItems(s *jsonschema.Schema, name string, atLeast int) *jsonschema.Schema {
	prop, ok := s.Properties[name]
	if !ok {
		panic(fmt.Sprintf("withMinItems: no property %q", name))
	}
	prop.MinItems = jsonschema.Ptr(atLeast)
	dropNullAlt(prop)
	return s
}

// dropNullAlt removes prop's inferred "null" alternative, a no-op when it has
// none: a slice or enum property built from a Go pointer type infers
// ["null", "<type>"], but the property is only ever present with a real
// value, never JSON null, once its minimum length or its enum is set.
func dropNullAlt(prop *jsonschema.Schema) {
	if len(prop.Types) == 2 && prop.Types[0] == "null" {
		prop.Type, prop.Types = prop.Types[1], nil
	}
}

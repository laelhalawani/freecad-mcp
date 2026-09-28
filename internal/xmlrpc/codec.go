// Package xmlrpc is a minimal XML-RPC client for the FreeCAD addon, which
// serves Python's SimpleXMLRPCServer with allow_none.
//
// Values map to Go as follows. Encoding: nil -> <nil/>, bool -> boolean,
// integers -> int (i8 beyond 32 bits), float64 -> double, string -> string,
// []byte -> base64, slices -> array, map[string]T and structs are not
// supported beyond map[string]any. json.Number encodes as int when it is
// integral and as double otherwise, so JSON input keeps its number kinds.
// Decoding: int/i4/i8 -> int64, double -> float64, boolean -> bool,
// string -> string, base64 -> []byte, dateTime.iso8601 -> string,
// struct -> map[string]any, array -> []any, nil -> nil.
package xmlrpc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Fault is an XML-RPC fault returned by the server.
type Fault struct {
	Code   int
	String string
}

func (f *Fault) Error() string {
	return fmt.Sprintf("XML-RPC fault %d: %s", f.Code, f.String)
}

// missingMethodRe is SimpleXMLRPCServer's exact wording for a method it does
// not have: <class 'Exception'>:method "x" is not supported.
var missingMethodRe = regexp.MustCompile(`^<class '[^']*'>:method ".*" is not supported$`)

// MissingMethod reports whether the fault says the server lacks the called
// method. Any other fault, even one whose text mentions "is not supported",
// is a failure inside a method the server has.
func (f *Fault) MissingMethod() bool {
	return missingMethodRe.MatchString(f.String)
}

// settingsUnreadablePrefix is the start of the fault rpc_server.py's
// _dispatch raises for every call, ping included, while the addon settings
// file cannot be read or parsed (contract 6.1): "FreeCAD MCP settings could
// not be read; save them again with freecad-mcp > Share this PC".
const settingsUnreadablePrefix = "FreeCAD MCP settings could not be read"

// SettingsUnreadable reports whether the fault is the addon's settings file
// being unreadable. Unlike a call-specific failure, this means FreeCAD is
// up and every call is refused the same way (never that FreeCAD is not
// running), until the file is fixed and saved again.
func (f *Fault) SettingsUnreadable() bool {
	return strings.HasPrefix(f.String, settingsUnreadablePrefix)
}

// EncodeCall renders a methodCall document.
func EncodeCall(method string, params ...any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\"?>\n<methodCall>\n<methodName>")
	escape(&b, method)
	b.WriteString("</methodName>\n<params>\n")
	for i, p := range params {
		b.WriteString("<param>\n")
		if err := encodeValue(&b, p); err != nil {
			return nil, fmt.Errorf("param %d: %w", i+1, err)
		}
		b.WriteString("\n</param>\n")
	}
	b.WriteString("</params>\n</methodCall>\n")
	return b.Bytes(), nil
}

func escape(b *bytes.Buffer, s string) {
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '\r':
			// A literal CR would be normalised away by the XML parser.
			b.WriteString("&#13;")
		default:
			b.WriteRune(r)
		}
	}
}

func encodeValue(b *bytes.Buffer, v any) error {
	b.WriteString("<value>")
	if err := encodeInner(b, v); err != nil {
		return err
	}
	b.WriteString("</value>")
	return nil
}

func encodeInner(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("<nil/>")
	case bool:
		if x {
			b.WriteString("<boolean>1</boolean>")
		} else {
			b.WriteString("<boolean>0</boolean>")
		}
	case string:
		b.WriteString("<string>")
		escape(b, x)
		b.WriteString("</string>")
	case int:
		encodeInt(b, int64(x))
	case int32:
		encodeInt(b, int64(x))
	case int64:
		encodeInt(b, x)
	case float64:
		return encodeDouble(b, x)
	case float32:
		return encodeDouble(b, float64(x))
	case json.Number:
		if i, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			encodeInt(b, i)
			return nil
		}
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return fmt.Errorf("number %q: %w", x, err)
		}
		return encodeDouble(b, f)
	case []byte:
		b.WriteString("<base64>")
		b.WriteString(base64.StdEncoding.EncodeToString(x))
		b.WriteString("</base64>")
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("<struct>")
		for _, k := range keys {
			b.WriteString("<member><name>")
			escape(b, k)
			b.WriteString("</name>")
			if err := encodeValue(b, x[k]); err != nil {
				return fmt.Errorf("member %q: %w", k, err)
			}
			b.WriteString("</member>")
		}
		b.WriteString("</struct>")
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			b.WriteString("<array><data>")
			for i := 0; i < rv.Len(); i++ {
				if err := encodeValue(b, rv.Index(i).Interface()); err != nil {
					return fmt.Errorf("item %d: %w", i, err)
				}
			}
			b.WriteString("</data></array>")
			return nil
		}
		return fmt.Errorf("unsupported type %T", v)
	}
	return nil
}

func encodeInt(b *bytes.Buffer, i int64) {
	if i >= math.MinInt32 && i <= math.MaxInt32 {
		b.WriteString("<int>" + strconv.FormatInt(i, 10) + "</int>")
		return
	}
	b.WriteString("<i8>" + strconv.FormatInt(i, 10) + "</i8>")
}

func encodeDouble(b *bytes.Buffer, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("cannot encode non-finite number %v", f)
	}
	b.WriteString("<double>" + strconv.FormatFloat(f, 'g', -1, 64) + "</double>")
	return nil
}

// EncodeResponse renders a methodResponse carrying v.
func EncodeResponse(v any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\"?>\n<methodResponse>\n<params>\n<param>\n")
	if err := encodeValue(&b, v); err != nil {
		return nil, err
	}
	b.WriteString("\n</param>\n</params>\n</methodResponse>\n")
	return b.Bytes(), nil
}

// EncodeFault renders a methodResponse carrying a fault.
func EncodeFault(code int, msg string) []byte {
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\"?>\n<methodResponse>\n<fault>\n")
	_ = encodeValue(&b, map[string]any{"faultCode": code, "faultString": msg})
	b.WriteString("\n</fault>\n</methodResponse>\n")
	return b.Bytes()
}

// DecodeCall parses a methodCall document.
func DecodeCall(r io.Reader) (method string, params []any, err error) {
	d := xml.NewDecoder(r)
	root, err := nextStart(d)
	if err != nil {
		return "", nil, err
	}
	if root.Name.Local != "methodCall" {
		return "", nil, fmt.Errorf("xmlrpc: expected <methodCall>, got <%s>", root.Name.Local)
	}
	for {
		tok, err := d.Token()
		if err != nil {
			if err == io.EOF {
				return method, params, nil
			}
			return "", nil, fmt.Errorf("xmlrpc: %w", err)
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch el.Name.Local {
		case "methodName":
			if method, err = readText(d); err != nil {
				return "", nil, err
			}
		case "params":
			params = []any{}
		case "param":
			v, err := expectValue(d)
			if err != nil {
				return "", nil, err
			}
			params = append(params, v)
		default:
			return "", nil, fmt.Errorf("xmlrpc: unexpected <%s> in call", el.Name.Local)
		}
	}
}

// DecodeResponse parses a methodResponse document. A fault is returned as a
// *Fault error.
func DecodeResponse(r io.Reader) (any, error) {
	d := xml.NewDecoder(r)
	root, err := nextStart(d)
	if err != nil {
		return nil, err
	}
	if root.Name.Local != "methodResponse" {
		return nil, fmt.Errorf("xmlrpc: expected <methodResponse>, got <%s>", root.Name.Local)
	}
	el, err := nextStart(d)
	if err != nil {
		return nil, err
	}
	switch el.Name.Local {
	case "params":
		param, err := nextStart(d)
		if err != nil {
			return nil, err
		}
		if param.Name.Local != "param" {
			return nil, fmt.Errorf("xmlrpc: expected <param>, got <%s>", param.Name.Local)
		}
		return expectValue(d)
	case "fault":
		v, err := expectValue(d)
		if err != nil {
			return nil, err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("xmlrpc: fault is not a struct")
		}
		f := &Fault{}
		if c, ok := m["faultCode"].(int64); ok {
			f.Code = int(c)
		}
		f.String, _ = m["faultString"].(string)
		return nil, f
	default:
		return nil, fmt.Errorf("xmlrpc: unexpected <%s> in response", el.Name.Local)
	}
}

// nextStart returns the next start element, skipping text, comments and
// processing instructions. An end element before it is an error.
func nextStart(d *xml.Decoder) (xml.StartElement, error) {
	for {
		tok, err := d.Token()
		if err != nil {
			if err == io.EOF {
				return xml.StartElement{}, errors.New("xmlrpc: unexpected end of document")
			}
			return xml.StartElement{}, fmt.Errorf("xmlrpc: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return t, nil
		case xml.EndElement:
			return xml.StartElement{}, fmt.Errorf("xmlrpc: unexpected </%s>", t.Name.Local)
		}
	}
}

func expectValue(d *xml.Decoder) (any, error) {
	el, err := nextStart(d)
	if err != nil {
		return nil, err
	}
	if el.Name.Local != "value" {
		return nil, fmt.Errorf("xmlrpc: expected <value>, got <%s>", el.Name.Local)
	}
	return decodeValue(d)
}

// decodeValue reads the content of a <value> whose start tag was consumed,
// up to and including its end tag.
func decodeValue(d *xml.Decoder) (any, error) {
	var text strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("xmlrpc: %w", err)
		}
		switch t := tok.(type) {
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			// A value without a type element is a string.
			return text.String(), nil
		case xml.StartElement:
			v, err := decodeTyped(d, t)
			if err != nil {
				return nil, err
			}
			if err := skipToEnd(d); err != nil {
				return nil, err
			}
			return v, nil
		}
	}
}

// skipToEnd consumes whitespace up to the closing tag of the current element.
func skipToEnd(d *xml.Decoder) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("xmlrpc: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return nil
		case xml.StartElement:
			return fmt.Errorf("xmlrpc: unexpected <%s>", t.Name.Local)
		}
	}
}

func readText(d *xml.Decoder) (string, error) {
	var text strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return "", fmt.Errorf("xmlrpc: %w", err)
		}
		switch t := tok.(type) {
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			return text.String(), nil
		case xml.StartElement:
			return "", fmt.Errorf("xmlrpc: unexpected <%s> in scalar", t.Name.Local)
		}
	}
}

func decodeTyped(d *xml.Decoder, el xml.StartElement) (any, error) {
	switch el.Name.Local {
	case "nil":
		return nil, skipToEnd(d)
	case "string":
		return readText(d)
	case "int", "i4", "i8", "i1", "i2", "biginteger":
		s, err := readText(d)
		if err != nil {
			return nil, err
		}
		i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("xmlrpc: bad integer %q", s)
		}
		return i, nil
	case "boolean":
		s, err := readText(d)
		if err != nil {
			return nil, err
		}
		switch strings.TrimSpace(s) {
		case "1":
			return true, nil
		case "0":
			return false, nil
		}
		return nil, fmt.Errorf("xmlrpc: bad boolean %q", s)
	case "double", "float", "bigdecimal":
		s, err := readText(d)
		if err != nil {
			return nil, err
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("xmlrpc: bad double %q", s)
		}
		return f, nil
	case "dateTime.iso8601":
		return readText(d)
	case "base64":
		s, err := readText(d)
		if err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			return nil, fmt.Errorf("xmlrpc: bad base64: %w", err)
		}
		return data, nil
	case "struct":
		return decodeStruct(d)
	case "array":
		return decodeArray(d)
	}
	return nil, fmt.Errorf("xmlrpc: unknown type <%s>", el.Name.Local)
}

func decodeStruct(d *xml.Decoder) (map[string]any, error) {
	m := map[string]any{}
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("xmlrpc: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return m, nil
		case xml.StartElement:
			if t.Name.Local != "member" {
				return nil, fmt.Errorf("xmlrpc: expected <member>, got <%s>", t.Name.Local)
			}
			name, value, err := decodeMember(d)
			if err != nil {
				return nil, err
			}
			m[name] = value
		}
	}
}

func decodeMember(d *xml.Decoder) (string, any, error) {
	var (
		name     string
		value    any
		gotName  bool
		gotValue bool
	)
	for {
		tok, err := d.Token()
		if err != nil {
			return "", nil, fmt.Errorf("xmlrpc: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			if !gotName || !gotValue {
				return "", nil, errors.New("xmlrpc: member needs a name and a value")
			}
			return name, value, nil
		case xml.StartElement:
			switch t.Name.Local {
			case "name":
				if name, err = readText(d); err != nil {
					return "", nil, err
				}
				gotName = true
			case "value":
				if value, err = decodeValue(d); err != nil {
					return "", nil, err
				}
				gotValue = true
			default:
				return "", nil, fmt.Errorf("xmlrpc: unexpected <%s> in member", t.Name.Local)
			}
		}
	}
}

func decodeArray(d *xml.Decoder) ([]any, error) {
	data, err := nextStart(d)
	if err != nil {
		return nil, err
	}
	if data.Name.Local != "data" {
		return nil, fmt.Errorf("xmlrpc: expected <data>, got <%s>", data.Name.Local)
	}
	items := []any{}
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("xmlrpc: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			// </data>; the caller's skipToEnd consumes </array>.
			return items, skipToEnd(d)
		case xml.StartElement:
			if t.Name.Local != "value" {
				return nil, fmt.Errorf("xmlrpc: expected <value>, got <%s>", t.Name.Local)
			}
			v, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
	}
}

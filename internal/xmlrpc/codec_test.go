package xmlrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func roundTrip(t *testing.T, v any) any {
	t.Helper()
	body, err := EncodeResponse(v)
	if err != nil {
		t.Fatalf("encode %#v: %v", v, err)
	}
	got, err := DecodeResponse(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return got
}

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		in, want any
	}{
		{nil, nil},
		{true, true},
		{false, false},
		{42, int64(42)},
		{int64(1) << 40, int64(1) << 40},
		{-7, int64(-7)},
		{2.5, 2.5},
		{"plain", "plain"},
		{"a < b & c > d", "a < b & c > d"},
		{"line1\r\nline2\ttab", "line1\r\nline2\ttab"},
		{"unicode é中", "unicode é中"},
		{[]byte{0, 1, 2, 255}, []byte{0, 1, 2, 255}},
		{[]any{1, "two", nil}, []any{int64(1), "two", nil}},
		{[]string{}, []any{}},
		{map[string]any{"b": 1, "a": []any{true}, "c": map[string]any{}}, map[string]any{"b": int64(1), "a": []any{true}, "c": map[string]any{}}},
	}
	for _, c := range cases {
		if got := roundTrip(t, c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("round trip %#v = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestJSONNumbersKeepTheirKind(t *testing.T) {
	body, err := EncodeCall("m", map[string]any{"Height": json.Number("30"), "Ratio": json.Number("0.3"), "Big": json.Number("1e3")})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{"<int>30</int>", "<double>0.3</double>", "<double>1000</double>"} {
		if !strings.Contains(s, want) {
			t.Errorf("call %s does not contain %s", s, want)
		}
	}
}

func TestNonFiniteNumbersAreRejected(t *testing.T) {
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := EncodeCall("m", f); err == nil {
			t.Errorf("EncodeCall(%v) succeeded", f)
		}
	}
}

func TestUnsupportedTypeIsRejected(t *testing.T) {
	if _, err := EncodeCall("m", struct{}{}); err == nil {
		t.Fatal("EncodeCall(struct{}{}) succeeded")
	}
}

func TestDecodeCall(t *testing.T) {
	body, err := EncodeCall("execute_code", "x = 1", 600.0, nil)
	if err != nil {
		t.Fatal(err)
	}
	method, params, err := DecodeCall(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if method != "execute_code" || !reflect.DeepEqual(params, []any{"x = 1", 600.0, nil}) {
		t.Fatalf("DecodeCall = %q %#v", method, params)
	}
}

func TestFault(t *testing.T) {
	_, err := DecodeResponse(bytes.NewReader(EncodeFault(1, `<class 'Exception'>:method "x" is not supported`)))
	var f *Fault
	if !errors.As(err, &f) {
		t.Fatalf("err = %v, want *Fault", err)
	}
	if f.Code != 1 || !f.MissingMethod() {
		t.Fatalf("fault = %+v, missing method %v", f, f.MissingMethod())
	}
	_, err = DecodeResponse(bytes.NewReader(EncodeFault(1, "RuntimeError: status exploded")))
	if !errors.As(err, &f) || f.MissingMethod() {
		t.Fatalf("a failure inside a method must not read as a missing method: %v", err)
	}
}

// Python's xmlrpc.client and SimpleXMLRPCServer write these forms.
func TestDecodePythonForms(t *testing.T) {
	doc := `<?xml version='1.0'?>
<methodResponse>
<params>
<param>
<value><struct>
<member>
<name>untyped</name>
<value>bare string</value>
</member>
<member>
<name>i4</name>
<value><i4>7</i4></value>
</member>
<member>
<name>empty</name>
<value><string></string></value>
</member>
<member>
<name>none</name>
<value><nil/></value></member>
<member>
<name>list</name>
<value><array><data>
<value><double>1.5</double></value>
</data></array></value>
</member>
</struct></value>
</param>
</params>
</methodResponse>
`
	got, err := DecodeResponse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"untyped": "bare string", "i4": int64(7), "empty": "", "none": nil, "list": []any{1.5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestMalformedResponses(t *testing.T) {
	for _, doc := range []string{
		"",
		"<methodResponse>",
		"<other/>",
		"<methodResponse><params><param><value><int>x</int></value></param></params></methodResponse>",
		"<methodResponse><params><param><value><boolean>2</boolean></value></param></params></methodResponse>",
		"<methodResponse><params><param><value><mystery/></value></param></params></methodResponse>",
	} {
		if _, err := DecodeResponse(strings.NewReader(doc)); err == nil {
			t.Errorf("DecodeResponse(%q) succeeded", doc)
		}
	}
}

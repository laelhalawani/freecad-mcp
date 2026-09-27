package xmlrpc

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// python finds an interpreter; FREECAD_MCP_TEST_PYTHON overrides the search.
func python(t *testing.T) string {
	t.Helper()
	for _, c := range []string{os.Getenv("FREECAD_MCP_TEST_PYTHON"), "python3", "python"} {
		if c == "" {
			continue
		}
		if path, err := exec.LookPath(c); err == nil && exec.Command(path, "-c", "pass").Run() == nil {
			return path
		}
	}
	t.Skip("no Python interpreter; set FREECAD_MCP_TEST_PYTHON")
	return ""
}

// TestPythonReadsWhatWeSend parses our methodCall with Python's own
// xmlrpc.client.loads, the parser SimpleXMLRPCServer uses in the addon.
func TestPythonReadsWhatWeSend(t *testing.T) {
	py := python(t)
	body, err := EncodeCall("execute_code",
		"line1\r\nline2 & <tag> é",
		map[string]any{"Height": json.Number("30"), "Ratio": json.Number("1.0"), "Big": int64(1) << 40, "None": nil},
		[]any{true, 2.5},
	)
	if err != nil {
		t.Fatal(err)
	}
	script := `import sys, json, xmlrpc.client
params, method = xmlrpc.client.loads(sys.stdin.buffer.read(), use_builtin_types=True)
p = params[1]
print(json.dumps({"method": method, "code": params[0], "types": {k: type(v).__name__ for k, v in p.items()}, "list": params[2]}))`
	cmd := exec.Command(py, "-c", script)
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, stderr.String())
	}
	var got struct {
		Method string            `json:"method"`
		Code   string            `json:"code"`
		Types  map[string]string `json:"types"`
		List   []any             `json:"list"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got.Method != "execute_code" || got.Code != "line1\r\nline2 & <tag> é" {
		t.Fatalf("method %q code %q", got.Method, got.Code)
	}
	want := map[string]string{"Height": "int", "Ratio": "float", "Big": "int", "None": "NoneType"}
	for k, v := range want {
		if got.Types[k] != v {
			t.Errorf("%s arrived as %s, want %s", k, got.Types[k], v)
		}
	}
	if len(got.List) != 2 || got.List[0] != true || got.List[1] != 2.5 {
		t.Errorf("list = %v", got.List)
	}
}

// TestWeReadWhatPythonSends decodes a response written by Python's own
// xmlrpc.client.dumps with allow_none, as the addon sends it.
func TestWeReadWhatPythonSends(t *testing.T) {
	py := python(t)
	script := `import sys, xmlrpc.client
v = {"success": True, "n": 7, "f": 0.5, "none": None, "s": "a\r\nb & <c>", "list": [1, "x"], "empty": {}}
sys.stdout.buffer.write(xmlrpc.client.dumps((v,), methodresponse=True, allow_none=True).encode("utf-8"))`
	cmd := exec.Command(py, "-c", script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, stderr.String())
	}
	v, err := DecodeResponse(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	m := v.(map[string]any)
	// Python's marshaller refuses ints beyond 32 bits, so the addon never
	// sends one; that direction needs no <i8> case.
	if m["success"] != true || m["n"] != int64(7) || m["f"] != 0.5 || m["none"] != nil {
		t.Fatalf("decoded %#v", m)
	}
	// Python writes a bare CR, which the XML parser turns into LF.
	if s := m["s"].(string); !strings.Contains(s, "b & <c>") {
		t.Fatalf("string = %q", s)
	}
}

//go:build e2e

// Package e2e drives a built freecad-mcp binary against a running FreeCAD.
//
// Start FreeCAD with the addon's RPC server running, build the binary, then:
//
//	FREECAD_MCP_E2E_BINARY=/path/to/freecad-mcp go test -tags e2e -v ./internal/e2e/
package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func session(t *testing.T) *mcp.ClientSession {
	t.Helper()
	bin := os.Getenv("FREECAD_MCP_E2E_BINARY")
	if bin == "" {
		t.Skip("set FREECAD_MCP_E2E_BINARY to a built freecad-mcp")
	}
	ctx := context.Background()
	cmd := exec.Command(bin, "mcp")
	cmd.Stderr = os.Stderr
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

type reply struct {
	text   string
	images int
	isErr  bool
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) reply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var r reply
	var texts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			texts = append(texts, c.Text)
		case *mcp.ImageContent:
			if len(c.Data) < 8 || string(c.Data[1:4]) != "PNG" {
				t.Errorf("%s: image is not a PNG", name)
			}
			r.images++
		}
	}
	r.text, r.isErr = strings.Join(texts, "\n"), res.IsError
	t.Logf("%s -> error=%v images=%d\n%s", name, r.isErr, r.images, r.text)
	return r
}

func must(t *testing.T, r reply, contains ...string) {
	t.Helper()
	if r.isErr {
		t.Fatalf("unexpected error reply:\n%s", r.text)
	}
	for _, c := range contains {
		if !strings.Contains(r.text, c) {
			t.Fatalf("reply does not contain %q:\n%s", c, r.text)
		}
	}
}

func TestAgainstFreeCAD(t *testing.T) {
	cs := session(t)

	must(t, call(t, cs, "get_rpc_status", nil), "version_check: ok")
	doc := call(t, cs, "create_document", map[string]any{"name": "E2E Doc"})
	must(t, doc, "E2E_Doc")

	box := call(t, cs, "create_object", map[string]any{
		"doc_name": "E2E_Doc", "obj_name": "Box", "obj_type": "Part::Box",
		"obj_properties": map[string]any{"Length": 20, "Width": 10.5, "Height": 5,
			"Placement": map[string]any{"Base": map[string]any{"x": 1, "y": 2, "z": 3}}},
	})
	must(t, box, "object_name: Box")
	if box.images != 1 {
		t.Fatalf("create_object returned %d images", box.images)
	}

	// An integer property rejects floats, so this checks JSON ints stay ints.
	poly := call(t, cs, "create_object", map[string]any{
		"doc_name": "E2E_Doc", "obj_name": "Hex", "obj_type": "Draft::Polygon",
		"obj_properties": map[string]any{"FacesNumber": 6, "Radius": 4}, "include_screenshot": false,
	})
	must(t, poly, "created successfully")

	must(t, call(t, cs, "get_object", map[string]any{"doc_name": "E2E_Doc", "obj_name": "Box", "include_screenshot": false}),
		`"Length"`, "20")
	must(t, call(t, cs, "update_object", map[string]any{"doc_name": "E2E_Doc", "obj_name": "Box",
		"obj_properties": map[string]any{"Length": 25}, "view_name": "Top"}), "updated successfully")
	must(t, call(t, cs, "list_objects", map[string]any{"doc_name": "E2E_Doc", "include_screenshot": false}), "count: 2")
	must(t, call(t, cs, "list_documents", nil), "E2E_Doc")

	code := call(t, cs, "execute_code", map[string]any{
		"code": "doc = FreeCAD.getDocument('E2E_Doc')\nprint('volume', round(doc.getObject('Box').Shape.Volume, 3))", "include_screenshot": false,
	})
	must(t, code, "volume 1312.5")

	view := call(t, cs, "get_view", map[string]any{"view_name": "Isometric", "width": 320, "height": 240})
	if view.isErr || view.images != 1 {
		t.Fatalf("get_view = %+v", view)
	}

	missing := call(t, cs, "get_object", map[string]any{"doc_name": "E2E_Doc", "obj_name": "Nope", "include_screenshot": false})
	if !missing.isErr || !strings.Contains(missing.text, "not_found") {
		t.Fatalf("missing object reply = %+v", missing)
	}

	async := call(t, cs, "execute_code_async", map[string]any{"code": "import time\ntime.sleep(0.5)\nresult = 6 * 7"})
	must(t, async, "job_id:")
	deadline := time.Now().Add(30 * time.Second)
	for {
		st := call(t, cs, "get_async_status", nil)
		if strings.Contains(st.text, `"state": "done"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("async job did not finish: %s", st.text)
		}
		time.Sleep(time.Second)
	}

	headless := call(t, cs, "execute_code_headless", map[string]any{
		"code": "import FreeCAD, Part\nprint('headless', Part.makeBox(1, 2, 3).Volume)", "timeout": 120,
	})
	must(t, headless, "headless 6.0", "exit 0")

	must(t, call(t, cs, "delete_object", map[string]any{"doc_name": "E2E_Doc", "obj_name": "Box", "include_screenshot": false}),
		"deleted successfully")
	must(t, call(t, cs, "execute_code", map[string]any{"code": "FreeCAD.closeDocument('E2E_Doc')", "include_screenshot": false}))
}

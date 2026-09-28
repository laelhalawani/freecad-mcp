package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/laelhalawani/freecad-mcp/internal/domain"
	"github.com/laelhalawani/freecad-mcp/internal/xmlrpc/xmlrpctest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\nfake")

// addon returns a fake addon that answers the handshake and records calls.
func addon(t *testing.T, extra map[string]xmlrpctest.Handler) *xmlrpctest.Server {
	handlers := map[string]xmlrpctest.Handler{
		"ping": func([]any) (any, error) { return true, nil },
		// Mirrors FreeCADRPC.get_rpc_status (rpc_server.py): success, rpc_server,
		// gui_dispatch, async_jobs_running, addon_version, protocol_version,
		// execute_code_timeout, max_execute_code_timeout, merged with
		// status_snapshot.snapshot()'s freecad_version, pid, rpc_started_at,
		// documents and active_document when the snapshot succeeded (a real
		// addon sends snapshot_error instead when it raised).
		"get_rpc_status": func([]any) (any, error) {
			return map[string]any{"success": true, "rpc_server": "running", "addon_version": "0.1.25", "protocol_version": domain.ProtocolVersion,
				"execute_code_timeout": 90, "max_execute_code_timeout": 1800, "gui_dispatch": map[string]any{"state": "healthy"},
				"async_jobs_running": []any{},
				"freecad_version":    "1.1.3", "pid": int64(4242), "rpc_started_at": 1700000000.0,
				"documents": []any{}, "active_document": ""}, nil
		},
		// A protocol-3 addon replies with a struct, not the bare base64 string
		// older addons sent (view_manager.get_active_screenshot).
		"get_active_screenshot": func([]any) (any, error) {
			return map[string]any{"success": true, "image": base64.StdEncoding.EncodeToString(pngBytes), "document": ""}, nil
		},
	}
	for k, v := range extra {
		handlers[k] = v
	}
	return xmlrpctest.New(t, handlers)
}

func session(t *testing.T, settings domain.Settings) *mcp.ClientSession {
	t.Helper()
	srv := New(Config{Version: "1.2.3", FreeCAD: settings})
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.MCPServer().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func settingsFor(s *xmlrpctest.Server) domain.Settings {
	return domain.Settings{Host: s.Host, Port: s.Port}
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func texts(res *mcp.CallToolResult) []string {
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return out
}

func images(res *mcp.CallToolResult) int {
	n := 0
	for _, c := range res.Content {
		if _, ok := c.(*mcp.ImageContent); ok {
			n++
		}
	}
	return n
}

func TestToolsAreListedWithTheirSchemas(t *testing.T) {
	cs := session(t, domain.Settings{Host: "127.0.0.1", Port: 9})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"activate_document", "analyze_mesh", "check_printability", "close_document",
		"create_document", "create_object", "delete_object", "execute_code",
		"execute_code_async", "execute_code_headless", "export_document", "get_async_status", "get_object",
		"get_rpc_status", "get_selection", "get_spreadsheet_cells", "get_view", "import_file",
		"insert_part_from_library", "list_documents", "list_objects", "list_parts", "measure",
		"mesh_to_solid", "open_document", "recompute_document", "redo", "reload_document", "repair_mesh",
		"run_fem_analysis", "save_document", "save_document_as", "solid_to_mesh", "start_freecad", "undo",
		"update_object", "update_spreadsheet_cells"}
	var got []string
	schemas := map[string]map[string]any{}
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
		data, _ := json.Marshal(tool.InputSchema)
		var m map[string]any
		json.Unmarshal(data, &m)
		schemas[tool.Name] = m
		if len(tool.Description) < 120 {
			t.Errorf("%s: description too short for a model to use: %q", tool.Name, tool.Description)
		}
	}
	if !reflect.DeepEqual(sortStrings(got), want) {
		t.Fatalf("tools = %v\nwant %v", got, want)
	}
	props := schemas["create_object"]["properties"].(map[string]any)
	view := props["view_name"].(map[string]any)
	if !reflect.DeepEqual(view["enum"], ViewNames) || view["default"] != "Isometric" {
		t.Errorf("view_name schema = %v", view)
	}
	if props["include_screenshot"].(map[string]any)["default"] != true {
		t.Errorf("include_screenshot schema = %v", props["include_screenshot"])
	}
	required := schemas["create_object"]["required"].([]any)
	if !reflect.DeepEqual(required, []any{"doc_name", "obj_type", "obj_name"}) {
		t.Errorf("create_object required = %v", required)
	}
	if !reflect.DeepEqual(schemas["update_object"]["required"], []any{"doc_name", "obj_name", "obj_properties"}) {
		t.Errorf("update_object required = %v", schemas["update_object"]["required"])
	}
}

func sortStrings(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestCreateObjectKeepsJSONNumberKinds(t *testing.T) {
	fc := addon(t, map[string]xmlrpctest.Handler{
		"create_object": func([]any) (any, error) { return map[string]any{"success": true, "object_name": "Pipe"}, nil },
	})
	cs := session(t, settingsFor(fc))
	// Raw JSON, because Go would encode 1.0 as 1 before it left the client.
	// Defaults (include_screenshot, view_name) are applied to this call, which
	// is the path where the SDK re-encodes arguments.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_object", Arguments: json.RawMessage(
		`{"doc_name": "Doc", "obj_name": "Pipe", "obj_type": "Part::Tube",
		  "obj_properties": {"OuterRadius": 125, "InnerRadius": 115.5, "Ratio": 1.0,
		                     "ViewObject": {"LineColor": [1.0, 0.0, 0.0]}, "Placement": {"Base": {"x": 0}}}}`)})
	if err != nil || res.IsError {
		t.Fatalf("create_object failed: %v %v", err, texts(res))
	}
	params := fc.CallsTo("create_object")[0].Params
	data := params[1].(map[string]any)
	props := data["Properties"].(map[string]any)
	if props["OuterRadius"] != int64(125) || props["InnerRadius"] != 115.5 || props["Ratio"] != 1.0 {
		t.Fatalf("properties sent as %#v", props)
	}
	color := props["ViewObject"].(map[string]any)["LineColor"].([]any)
	if color[0] != 1.0 || color[1] != 0.0 {
		t.Fatalf("color sent as %#v; integer 1 would read as 1/255", color)
	}
	if data["Name"] != "Pipe" || data["Type"] != "Part::Tube" || data["Analysis"] != nil {
		t.Fatalf("obj_data = %#v", data)
	}
	if !strings.Contains(texts(res)[0], "Object 'Pipe' created successfully") || images(res) != 1 {
		t.Fatalf("reply = %v with %d images", texts(res), images(res))
	}
}

func TestScreenshotsFollowTheOptions(t *testing.T) {
	fc := addon(t, map[string]xmlrpctest.Handler{
		"delete_object": func([]any) (any, error) { return map[string]any{"success": true, "object_name": "Box"}, nil },
	})
	cs := session(t, settingsFor(fc))
	if res := call(t, cs, "delete_object", map[string]any{"doc_name": "D", "obj_name": "Box", "view_name": "Top"}); images(res) != 1 {
		t.Fatal("no screenshot by default")
	}
	if got := fc.CallsTo("get_active_screenshot")[0].Params[0]; got != "Top" {
		t.Fatalf("view sent as %v", got)
	}
	if res := call(t, cs, "delete_object", map[string]any{"doc_name": "D", "obj_name": "Box", "include_screenshot": false}); images(res) != 0 {
		t.Fatal("screenshot despite include_screenshot false")
	}

	textOnly := settingsFor(fc)
	textOnly.OnlyTextFeedback = true
	cs2 := session(t, textOnly)
	if res := call(t, cs2, "delete_object", map[string]any{"doc_name": "D", "obj_name": "Box"}); images(res) != 0 {
		t.Fatal("screenshot despite only-text feedback")
	}
}

func TestUpdateObjectRequiresAnObject(t *testing.T) {
	fc := addon(t, nil)
	cs := session(t, settingsFor(fc))
	res := call(t, cs, "update_object", map[string]any{"doc_name": "D", "obj_name": "Box", "obj_properties": nil})
	if !res.IsError || len(fc.CallsTo("edit_object")) != 0 {
		t.Fatalf("null obj_properties accepted: %v", texts(res))
	}
}

func TestFreeCADStoppingAfterConnectPointsAtStartingIt(t *testing.T) {
	// The list_documents tool calls the addon's get_documents method, not one
	// named list_documents.
	fc := addon(t, map[string]xmlrpctest.Handler{
		"get_documents": func([]any) (any, error) {
			return map[string]any{"success": true, "active_document": "", "count": 0, "documents": []any{}}, nil
		},
	})
	cs := session(t, settingsFor(fc))
	call(t, cs, "list_documents", nil)
	fc.Close()
	res := call(t, cs, "list_documents", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "get_rpc_status") {
		t.Fatalf("reply = %v", text)
	}
	// get_rpc_status itself never errors while FreeCAD is down; its own next
	// step should send the model to start_freecad instead of looping.
	status := strings.Join(texts(call(t, cs, "get_rpc_status", nil)), "\n")
	if !strings.Contains(status, "not_running") || !strings.Contains(status, "start_freecad") {
		t.Fatalf("status reply = %v", status)
	}
}

func TestInvalidViewIsRejectedBySchema(t *testing.T) {
	fc := addon(t, nil)
	cs := session(t, settingsFor(fc))
	res := call(t, cs, "get_view", map[string]any{"view_name": "Sideways"})
	if !res.IsError {
		t.Fatalf("an unknown view was accepted: %v", texts(res))
	}
	if len(fc.CallsTo("get_active_screenshot")) != 0 {
		t.Fatal("an unknown view reached FreeCAD")
	}
}

func TestFailureIsAStructuredError(t *testing.T) {
	fc := addon(t, map[string]xmlrpctest.Handler{
		"edit_object": func([]any) (any, error) {
			return map[string]any{"success": false, "error": "Object 'Nope' not found"}, nil
		},
	})
	cs := session(t, settingsFor(fc))
	res := call(t, cs, "update_object", map[string]any{"doc_name": "D", "obj_name": "Nope", "obj_properties": map[string]any{"Length": 5}})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: freecad_error") || !strings.Contains(text, "Object 'Nope' not found") || !strings.Contains(text, "hint:") {
		t.Fatalf("reply = %v (isError %v)", text, res.IsError)
	}
}

func TestWarningIsShownOnceInTheNextToolReply(t *testing.T) {
	fc := addon(t, map[string]xmlrpctest.Handler{
		"get_rpc_status": func([]any) (any, error) { return map[string]any{"success": true}, nil },
		"get_documents": func([]any) (any, error) {
			return map[string]any{
				"success": true, "active_document": "Doc", "count": 1,
				"documents": []any{map[string]any{
					"name": "Doc", "label": "Doc", "file_name": "", "modified": false,
					"needs_recompute": false, "partial": false, "object_count": 0,
					"active": true, "views": []any{},
				}},
			}, nil
		},
	})
	cs := session(t, settingsFor(fc))
	first := texts(call(t, cs, "list_documents", nil))
	if len(first) != 2 || !strings.HasPrefix(first[0], "Warning: The FreeCAD addon does not report a version") || !strings.Contains(first[1], "Doc") {
		t.Fatalf("first reply = %v", first)
	}
	second := texts(call(t, cs, "list_documents", nil))
	if len(second) != 1 || !strings.Contains(second[0], "Doc") {
		t.Fatalf("second reply = %v", second)
	}
}

func TestFreeCADNotRunningIsUnavailable(t *testing.T) {
	// Reserve a port, then free it: nothing listens there until "FreeCAD starts".
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(portStr)
	l.Close()

	cs := session(t, domain.Settings{Host: host, Port: port})
	res := call(t, cs, "list_documents", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") || !strings.Contains(text, "Make sure the FreeCAD addon is running") {
		t.Fatalf("reply while FreeCAD is down = %v", text)
	}
}

func TestReconnectAfterFreeCADStarts(t *testing.T) {
	fc := addon(t, map[string]xmlrpctest.Handler{
		"get_rpc_status": func([]any) (any, error) { return map[string]any{"success": true}, nil },
		"get_documents": func([]any) (any, error) {
			return map[string]any{"success": true, "active_document": "", "count": 0, "documents": []any{}}, nil
		},
	})
	var up atomic.Bool
	fc.Handle("ping", func([]any) (any, error) { return up.Load(), nil })
	cs := session(t, settingsFor(fc))
	if res := call(t, cs, "list_documents", nil); !res.IsError {
		t.Fatal("succeeded while ping failed")
	}
	if n := len(fc.CallsTo("get_rpc_status")); n != 0 {
		t.Fatalf("version checked before FreeCAD answered (%d)", n)
	}
	up.Store(true)
	res := call(t, cs, "list_documents", nil)
	if res.IsError || !strings.HasPrefix(texts(res)[0], "Warning:") {
		t.Fatalf("reply after start = %v", texts(res))
	}
}

func TestRejectedTokenIsExplained(t *testing.T) {
	fc := addon(t, nil)
	fc.Token = "s3cret"
	cs := session(t, domain.Settings{Host: fc.Host, Port: fc.Port, Token: "wrong-token"})
	res := call(t, cs, "list_documents", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: authentication") || !strings.Contains(text, "auth token") ||
		!strings.Contains(text, domain.EnvToken) || strings.Contains(text, "wrong-token") ||
		strings.Contains(text, "Make sure the FreeCAD addon is running") {
		t.Fatalf("reply = %v", text)
	}
}

func TestAsyncTexts(t *testing.T) {
	fc := addon(t, map[string]xmlrpctest.Handler{
		"execute_code_async": func([]any) (any, error) { return map[string]any{"success": true, "job_id": "job-7"}, nil },
	})
	cs := session(t, settingsFor(fc))
	started := strings.Join(texts(call(t, cs, "execute_code_async", map[string]any{"code": "x = 1"})), "\n")
	if !strings.Contains(started, "job_id: job-7") || !strings.Contains(started, `get_async_status with {"job_id": "job-7"}`) {
		t.Fatalf("start reply = %v", started)
	}

	fc.Handle("get_async_status", func([]any) (any, error) {
		return map[string]any{"success": true, "job": map[string]any{
			"id": "job-7", "state": "failed", "error": "ValueError: boom", "traceback": "Traceback ...\nValueError: boom"}}, nil
	})
	failed := strings.Join(texts(call(t, cs, "get_async_status", map[string]any{"job_id": "job-7"})), "\n")
	if !strings.Contains(failed, "Async job job-7: failed\nError: ValueError: boom\nTraceback ...\nValueError: boom") {
		t.Fatalf("status reply = %v", failed)
	}

	fc.Handle("get_async_status", func([]any) (any, error) {
		return map[string]any{"success": true, "jobs": []any{map[string]any{"id": "job-1", "state": "done"}}}, nil
	})
	all := strings.Join(texts(call(t, cs, "get_async_status", nil)), "\n")
	if !strings.Contains(all, `"id": "job-1"`) || !strings.Contains(all, "count: 1") {
		t.Fatalf("list reply = %v", all)
	}

	fc.Handle("execute_code_async", func([]any) (any, error) { return map[string]any{"success": true}, nil })
	older := strings.Join(texts(call(t, cs, "execute_code_async", map[string]any{"code": "pass"})), "\n")
	if !strings.Contains(older, "get_object") || !strings.Contains(older, "Report View") {
		t.Fatalf("older addon reply = %v", older)
	}
}

func TestRPCStatusReportsVersionCheck(t *testing.T) {
	fc := addon(t, nil)
	cs := session(t, settingsFor(fc))
	ok := strings.Join(texts(call(t, cs, "get_rpc_status", nil)), "\n")
	if !strings.Contains(ok, "version_check: ok") || !strings.Contains(ok, `"version_check": "ok"`) {
		t.Fatalf("status = %v", ok)
	}

	old := addon(t, map[string]xmlrpctest.Handler{})
	old.Handle("get_rpc_status", nil)
	cs2 := session(t, settingsFor(old))
	res := call(t, cs2, "get_rpc_status", nil)
	// The first reply also carries the connection's version notice, so look
	// at the tool's own result: the last content item.
	own := texts(res)[len(texts(res))-1]
	if !res.IsError || !strings.Contains(own, "code: freecad_error") || !strings.Contains(own, "no get_rpc_status") {
		t.Fatalf("old addon status = %v", texts(res))
	}
}

func TestExecuteCodeTimeoutIsForwarded(t *testing.T) {
	fc := addon(t, map[string]xmlrpctest.Handler{
		"execute_code": func([]any) (any, error) {
			return map[string]any{"success": true, "message": "Python code executed successfully.\nOutput: hi\n"}, nil
		},
	})
	cs := session(t, settingsFor(fc))
	res := call(t, cs, "execute_code", map[string]any{"code": "print('hi')", "timeout": 600, "include_screenshot": false})
	if res.IsError || !strings.Contains(texts(res)[0], "Output: hi") {
		t.Fatalf("reply = %v", texts(res))
	}
	if got := fc.CallsTo("execute_code")[0].Params; !reflect.DeepEqual(got, []any{"print('hi')", 600.0}) {
		t.Fatalf("sent %#v", got)
	}
	res = call(t, cs, "execute_code", map[string]any{"code": "x", "timeout": -1})
	if !res.IsError || !strings.Contains(strings.Join(texts(res), ""), "code: invalid_input") {
		t.Fatalf("negative timeout reply = %v", texts(res))
	}
}

func TestPromptIsServed(t *testing.T) {
	cs := session(t, domain.Settings{Host: "127.0.0.1", Port: 9})
	res, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "asset_creation_strategy"})
	if err != nil {
		t.Fatal(err)
	}
	if text := res.Messages[0].Content.(*mcp.TextContent).Text; !strings.Contains(text, "Asset Creation Strategy") {
		t.Fatalf("prompt = %q", text)
	}
}

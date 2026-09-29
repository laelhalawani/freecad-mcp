package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

// statusText calls get_rpc_status on a server whose FreeCAD process lives in
// the Windows session lookup names.
func statusText(t *testing.T, lookup func(pid int) (uint32, error)) string {
	t.Helper()
	fc := addon(t, nil)
	srv := New(Config{Version: "1.2.3", FreeCAD: settingsFor(fc)})
	srv.sessionOf = lookup
	return strings.Join(texts(call(t, sessionOf(t, srv), "get_rpc_status", nil)), "\n")
}

func TestStatusReportsFreeCADInHiddenSession(t *testing.T) {
	var asked int
	text := statusText(t, func(pid int) (uint32, error) {
		asked = pid
		return 0, nil
	})
	if asked != 4242 {
		t.Fatalf("looked up pid %d, want the addon's own pid 4242", asked)
	}
	for _, want := range []string{"desktop: hidden", "hidden Windows session", "close_freecad", "start_freecad"} {
		if !strings.Contains(text, want) {
			t.Errorf("status reply lacks %q:\n%s", want, text)
		}
	}
}

func TestStatusSaysNothingForAVisibleOrUnknownSession(t *testing.T) {
	for name, lookup := range map[string]func(int) (uint32, error){
		"session 1":      func(int) (uint32, error) { return 1, nil },
		"lookup failure": func(int) (uint32, error) { return 0, errors.New("no such process") },
	} {
		text := statusText(t, lookup)
		if strings.Contains(text, "desktop") || strings.Contains(text, "hidden") {
			t.Errorf("%s: status reply mentions the desktop:\n%s", name, text)
		}
		if !strings.Contains(text, "rpc: reachable") {
			t.Errorf("%s: status reply lost its normal content:\n%s", name, text)
		}
	}
}

func TestGetViewOffersCurrentOnlyOnGetView(t *testing.T) {
	cs := session(t, domain.Settings{Host: "127.0.0.1", Port: 9})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	enums := map[string][]any{}
	for _, tool := range res.Tools {
		data, _ := json.Marshal(tool.InputSchema)
		var m struct {
			Properties map[string]struct {
				Enum    []any `json:"enum"`
				Default any   `json:"default"`
			} `json:"properties"`
		}
		json.Unmarshal(data, &m)
		if view, ok := m.Properties["view_name"]; ok {
			enums[tool.Name] = view.Enum
			if tool.Name == "get_view" && view.Default != "Isometric" {
				t.Errorf("get_view default view = %v, want Isometric", view.Default)
			}
		}
	}
	if want := append(append([]any{}, ViewNames...), "Current"); !reflect.DeepEqual(enums["get_view"], want) {
		t.Errorf("get_view view_name enum = %v, want %v", enums["get_view"], want)
	}
	for name, enum := range enums {
		if name != "get_view" && !reflect.DeepEqual(enum, ViewNames) {
			t.Errorf("%s view_name enum = %v, want the orientations only", name, enum)
		}
	}
}

func TestGetViewCurrentRefusesFocusObject(t *testing.T) {
	fc := addon(t, nil)
	cs := session(t, settingsFor(fc))
	res := call(t, cs, "get_view", map[string]any{"view_name": "Current", "focus_object": "Box"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: invalid_input") ||
		!strings.Contains(text, "Current captures the view as it is; omit focus_object or pick an orientation") {
		t.Fatalf("reply = %v (isError %v)", text, res.IsError)
	}
	if len(fc.CallsTo("get_active_screenshot")) != 0 {
		t.Fatal("the refused call reached FreeCAD")
	}
}

func TestGetViewCurrentSendsCurrentToTheAddon(t *testing.T) {
	fc := addon(t, nil)
	cs := session(t, settingsFor(fc))
	res := call(t, cs, "get_view", map[string]any{"view_name": "Current"})
	if res.IsError || images(res) != 1 {
		t.Fatalf("reply = %v (isError %v, %d images)", texts(res), res.IsError, images(res))
	}
	if got := fc.CallsTo("get_active_screenshot"); len(got) != 1 || got[0].Params[0] != "Current" {
		t.Fatalf("addon calls = %+v", got)
	}
	if text := strings.Join(texts(res), "\n"); !strings.Contains(text, "view as it is on screen") {
		t.Errorf("reply does not say the view was captured as is: %s", text)
	}
}

package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

// tourStop is one stop of a set_view tour.
type tourStop struct {
	Focus        []string  `json:"focus,omitempty"`
	DwellSeconds *float64  `json:"dwell_seconds,omitempty"`
	ViewName     *ViewName `json:"view_name,omitempty"`
}

// setViewInput embeds screenshotOptions: its view_name is the orientation to
// set, and the screenshot shows the view as set.
type setViewInput struct {
	DocName          *string            `json:"doc_name,omitempty"`
	Focus            []string           `json:"focus,omitempty"`
	Show             []string           `json:"show,omitempty"`
	Hide             []string           `json:"hide,omitempty"`
	Isolate          []string           `json:"isolate,omitempty"`
	Transparency     map[string]float64 `json:"transparency,omitempty"`
	DisplayMode      map[string]string  `json:"display_mode,omitempty"`
	Mode             *string            `json:"mode,omitempty"`
	DegreesPerSecond *float64           `json:"degrees_per_second,omitempty"`
	Stops            []tourStop         `json:"stops,omitempty"`
	MoveSeconds      *float64           `json:"move_seconds,omitempty"`
	Loop             *bool              `json:"loop,omitempty"`
	Reset            *bool              `json:"reset,omitempty"`
	screenshotOptions
}

type setViewFront struct {
	Document string `yaml:"document"`
	Mode     string `yaml:"mode"`
	Running  bool   `yaml:"running"`
	Stopped  string `yaml:"stopped,omitempty"`
}

// setViewDefaults gives set_view's screenshot on, and no default orientation:
// without view_name the current orientation stays.
var setViewDefaults = map[string]string{"include_screenshot": "true", "mode": `"static"`}

func (s *Server) registerSetViewTool() {
	schema := inputSchema[setViewInput](setViewDefaults)
	schema = withEnum(schema, "mode", "static", "orbit", "tour")
	addTool(s.mcpServer, "set_view", schema, s.setView)
}

// setViewOptions builds the addon's options map from the given arguments.
func setViewOptions(in setViewInput) map[string]any {
	opts := map[string]any{}
	if len(in.Focus) > 0 {
		opts["focus"] = in.Focus
	}
	if v := viewString(in.ViewName); v != "" {
		opts["view_name"] = v
	}
	for key, names := range map[string][]string{"show": in.Show, "hide": in.Hide, "isolate": in.Isolate} {
		if len(names) > 0 {
			opts[key] = names
		}
	}
	if len(in.Transparency) > 0 {
		m := map[string]any{}
		for k, v := range in.Transparency {
			m[k] = v
		}
		opts["transparency"] = m
	}
	if len(in.DisplayMode) > 0 {
		m := map[string]any{}
		for k, v := range in.DisplayMode {
			m[k] = v
		}
		opts["display_mode"] = m
	}
	if in.Mode != nil {
		opts["mode"] = *in.Mode
	}
	if in.DegreesPerSecond != nil {
		opts["degrees_per_second"] = *in.DegreesPerSecond
	}
	if in.MoveSeconds != nil {
		opts["move_seconds"] = *in.MoveSeconds
	}
	if in.Loop != nil {
		opts["loop"] = *in.Loop
	}
	if in.Reset != nil {
		opts["reset"] = *in.Reset
	}
	if len(in.Stops) > 0 {
		stops := make([]any, 0, len(in.Stops))
		for _, st := range in.Stops {
			m := map[string]any{}
			if len(st.Focus) > 0 {
				m["focus"] = st.Focus
			}
			if st.DwellSeconds != nil {
				m["dwell_seconds"] = *st.DwellSeconds
			}
			if v := viewString(st.ViewName); v != "" {
				m["view_name"] = v
			}
			stops = append(stops, m)
		}
		opts["stops"] = stops
	}
	return opts
}

func (s *Server) setView(ctx context.Context, _ *mcp.CallToolRequest, in setViewInput) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "set view", err, ""), nil, nil
	}
	doc := ""
	if in.DocName != nil {
		doc = *in.DocName
	}
	res, err := conn.SetView(ctx, doc, setViewOptions(in))
	if err != nil {
		return s.withNotice(failure(ctx, "set view", err, "")), nil, nil
	}
	if !succeeded(res) {
		return s.withNotice(reportedCode("set view", res,
			"Call list_documents with {} to see the open documents, and list_objects for the object names.")), nil, nil
	}

	mode := str(res, "mode")
	running := boolField(res, "running")
	front := setViewFront{Document: str(res, "document"), Mode: mode, Running: running, Stopped: str(res, "stopped")}

	var body strings.Builder
	fmt.Fprintf(&body, "The 3D view of '%s' is set.", front.Document)
	if cam, ok := res["camera"].(map[string]any); ok {
		fmt.Fprintf(&body, " %s camera looking along %s.", str(cam, "type"), formatVector(cam["view_direction"]))
	}
	switch mode {
	case "orbit":
		fmt.Fprintf(&body, "\n\nOrbiting about the vertical axis at %s degrees per second until the user moves the view, "+
			"the next set_view call, or the document closes.", formatNumber(res["degrees_per_second"]))
	case "tour":
		fmt.Fprintf(&body, "\n\nTouring %s stop(s); it ends at the last stop unless loop is true, or earlier when the user "+
			"moves the view, on the next set_view call, or when the document closes.", formatNumber(res["stops"]))
	}
	if running {
		body.WriteString(" get_view pauses the mode for its capture and resumes it.")
	}
	for _, line := range []struct{ key, label string }{{"shown", "Shown"}, {"hidden", "Hidden"}} {
		if names := stringItems(res[line.key]); len(names) > 0 {
			fmt.Fprintf(&body, "\n\n%s: %s.", line.label, strings.Join(names, ", "))
		}
	}
	if tr, ok := res["transparency"].(map[string]any); ok && len(tr) > 0 {
		body.WriteString("\n\nTransparency: " + mapText(tr, "%") + ".")
	}
	if dm, ok := res["display_mode"].(map[string]any); ok && len(dm) > 0 {
		body.WriteString("\n\nDisplay mode: " + mapText(dm, "") + ".")
	}
	if rst, ok := res["reset"].(map[string]any); ok {
		if names := stringItems(rst["restored"]); len(names) > 0 {
			fmt.Fprintf(&body, "\n\nReset restored: %s.", strings.Join(names, ", "))
		} else {
			body.WriteString("\n\nReset: nothing earlier set_view calls changed was left to restore.")
		}
	}
	out := render.SuccessResult(front, body.String())
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, "Current", front.Document)), nil, nil
}

// mapText renders an object-to-value reply map as "a 40%, b 60%", sorted.
func mapText(m map[string]any, suffix string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			parts = append(parts, k+" "+v)
			continue
		}
		parts = append(parts, k+" "+formatNumber(m[k])+suffix)
	}
	return strings.Join(parts, ", ")
}

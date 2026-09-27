package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/laelhalawani/freecad-mcp/internal/domain"
	"github.com/laelhalawani/freecad-mcp/internal/freecad"
	"github.com/laelhalawani/freecad-mcp/internal/xmlrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

type femInput struct {
	DocName      string `json:"doc_name" jsonschema:"the name of the FreeCAD document"`
	AnalysisName string `json:"analysis_name" jsonschema:"the name of the Fem::AnalysisPython object"`
	Timeout      *int   `json:"timeout,omitempty" jsonschema:"seconds to wait for the solver, from 1 to 604800 (a week); default 600"`
	screenshotOptions
}

type statusFront struct {
	VersionCheck string `yaml:"version_check"`
}

type femFront struct {
	Analysis        string   `yaml:"analysis"`
	Success         bool     `yaml:"success"`
	MaxVonMisesMPa  *float64 `yaml:"max_von_mises_mpa,omitempty"`
	MaxDisplacement *float64 `yaml:"max_displacement_mm,omitempty"`
	NodeCount       *int64   `yaml:"node_count,omitempty"`
}

// maxFEMTimeout bounds the solver wait (a week), so the reply timeout derived
// from it cannot overflow.
const maxFEMTimeout = 7 * 24 * 3600

const femDescription = `Run the CalculiX solver on an existing FEM analysis container and return summary results.

Prerequisites in the document, all created with create_object:
- A Part-derived solid (e.g. Part::Box, PartDesign::Body) acting as the geometry.
- A Fem::AnalysisPython container.
- A Fem::MaterialCommon assigned to the geometry, added to the analysis.
- A Fem::FemMeshGmsh referencing the geometry, added to the analysis (the mesh is generated automatically when created).
- At least one Fem::ConstraintFixed and one Fem::ConstraintForce (or ConstraintPressure) bound to faces of the geometry, added to the analysis.

A CalculiX solver already in the analysis is reused; a SolverCcxTools is created when it has none. The solver runs synchronously on the FreeCAD GUI thread, so every other tool that needs the GUI thread waits until it finishes; do not send parallel requests. get_rpc_status and get_async_status do not use the GUI thread and stay answerable while it runs.

Returns the maximum and minimum von Mises stress (MPa), the maximum displacement (mm), the node count, the name of the result object, and the working directory CalculiX wrote to. On failure it returns the prerequisite check or solver error with the working directory for triage; list_objects shows what the analysis holds.`

func (s *Server) registerStatusTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "get_rpc_status",
		Description: "Get the health of FreeCAD's RPC server and its GUI dispatch. It does not use FreeCAD's GUI " +
			"thread, so it answers after a GUI operation timed out: a stuck state names the operation still running " +
			"and means FreeCAD may need a restart. version_check is \"ok\" or says whether the addon or this server " +
			"needs updating. Use it when other tools time out.",
		InputSchema: inputSchema[struct{}](nil),
	}, s.getRPCStatus)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "run_fem_analysis",
		Description: femDescription,
		InputSchema: withPositiveMax(inputSchema[femInput](mergeDefaults(screenshotDefaults, map[string]string{"timeout": "600"})), "timeout", maxFEMTimeout),
	}, s.runFEMAnalysis)
}

func mergeDefaults(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func (s *Server) getRPCStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("get RPC status", err, ""), nil, nil
	}
	status, err := conn.GetRPCStatus(ctx)
	if err != nil {
		var fault *xmlrpc.Fault
		if errors.As(err, &fault) && fault.MissingMethod() {
			return s.withNotice(render.ErrorResult(render.Error{
				Code:    codeFreeCAD,
				Message: freecad.AddonVersionWarning(nil, s.config.Version),
				Hint:    "Run `" + domain.BinaryName + " install-addon` and restart FreeCAD.",
			})), nil, nil
		}
		return s.withNotice(failure("get RPC status", err, "")), nil, nil
	}
	check := "ok"
	if m, ok := status.(map[string]any); ok {
		if w := freecad.AddonVersionWarning(m, s.config.Version); w != "" {
			check = w
		}
		m["version_check"] = check
	}
	return s.withNotice(render.SuccessResult(statusFront{VersionCheck: check}, jsonBlock(status))), nil, nil
}

// femFailureHint returns the hint for a failed analysis. The addon's GUI
// dispatch words its own failures: the analysis ran past its timeout ("timed
// out after"), did not start before its queue budget ran out ("gave up
// after"), or never started because an earlier GUI operation is still stuck
// ("unavailable"). larger says how to allow more time.
func femFailureHint(res map[string]any, larger string) string {
	e := str(res, "error")
	switch {
	case strings.HasPrefix(e, "GUI dispatch timed out after"):
		return "The analysis did not finish within its timeout. Call get_rpc_status to see whether it is still " +
			"running on FreeCAD's GUI thread. " + larger
	case strings.HasPrefix(e, "GUI dispatch gave up after"):
		return "The analysis did not start in time: earlier GUI operations held FreeCAD's GUI thread for the whole " +
			"timeout, which also bounds the wait to start. Call get_rpc_status to see what is running. A larger " +
			"timeout also allows a longer wait to start; the most is " + fmt.Sprint(maxFEMTimeout) + " seconds."
	case strings.HasPrefix(e, "GUI dispatch unavailable"):
		return "The analysis did not start: another GUI operation timed out and is still running on FreeCAD's GUI " +
			"thread. Call get_rpc_status and wait until it reports the dispatch healthy, then retry, or restart FreeCAD."
	}
	return "Check the prerequisites listed in this tool's description with list_objects; the details that follow include the working directory."
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	}
	return 0, false
}

func formatMeasure(v any, unit string) string {
	if f, ok := number(v); ok {
		return fmt.Sprintf("%s %s", fmt.Sprintf("%.4g", f), unit)
	}
	return fmt.Sprintf("unavailable (%s)", unit)
}

func (s *Server) runFEMAnalysis(ctx context.Context, _ *mcp.CallToolRequest, in femInput) (*mcp.CallToolResult, any, error) {
	timeout := 600
	if in.Timeout != nil {
		timeout = *in.Timeout
	}
	if timeout <= 0 || timeout > maxFEMTimeout {
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("timeout must be a positive number of seconds, at most %d", maxFEMTimeout),
			Hint:    "Omit timeout for the 600 s default.",
		}), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure("run FEM analysis", err, ""), nil, nil
	}
	larger := fmt.Sprintf("For a long analysis, call run_fem_analysis again with a larger timeout (this run allowed %d seconds, at most %d).",
		timeout, maxFEMTimeout)
	res, err := conn.RunFEMAnalysis(ctx, in.DocName, in.AnalysisName, timeout)
	if err != nil {
		return s.withNotice(timedFailure("run FEM analysis", err, larger)), nil, nil
	}
	if !succeeded(res) {
		out := render.ErrorResult(render.Error{
			Code:    codeFreeCAD,
			Message: shortMessage(fmt.Sprintf("FEM analysis '%s' failed: %v", in.AnalysisName, res["error"])),
			Hint:    femFailureHint(res, larger),
		})
		out.Content = append(out.Content, &mcp.TextContent{Text: jsonBlock(res)})
		return s.withNotice(out), nil, nil
	}
	front := femFront{Analysis: in.AnalysisName, Success: true}
	if f, ok := number(res["max_von_mises_MPa"]); ok {
		front.MaxVonMisesMPa = &f
	}
	if f, ok := number(res["max_displacement_mm"]); ok {
		front.MaxDisplacement = &f
	}
	if n, ok := res["node_count"].(int64); ok {
		front.NodeCount = &n
	}
	res["summary"] = fmt.Sprintf("FEM analysis '%s' solved. max von Mises = %s, max displacement = %s (%v nodes).",
		in.AnalysisName, formatMeasure(res["max_von_mises_MPa"], "MPa"), formatMeasure(res["max_displacement_mm"], "mm"), res["node_count"])
	out := render.SuccessResult(front, res["summary"].(string)+"\n\n"+jsonBlock(res))
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName))), nil, nil
}

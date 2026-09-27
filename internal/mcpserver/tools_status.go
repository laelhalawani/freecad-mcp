package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/laelhalawani/freecad-mcp/internal/freecad"
	"github.com/laelhalawani/freecad-mcp/internal/xmlrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

type femInput struct {
	DocName      string `json:"doc_name" jsonschema:"the name of the FreeCAD document"`
	AnalysisName string `json:"analysis_name" jsonschema:"the name of the Fem::AnalysisPython object"`
	Timeout      *int   `json:"timeout,omitempty" jsonschema:"seconds to wait for the solver (default 600)"`
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

A SolverCcxTools is created when the analysis has none. The solver runs synchronously on the FreeCAD GUI thread and blocks all other calls for its duration; do not send parallel requests.

Returns the maximum von Mises stress (MPa), maximum and minimum displacement (mm), the node count, and the working directory CalculiX wrote to. On failure it returns the prerequisite check or solver error with the working directory for triage.`

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
		InputSchema: inputSchema[femInput](mergeDefaults(screenshotDefaults, map[string]string{"timeout": "600"})),
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
	res, err := conn.RunFEMAnalysis(ctx, in.DocName, in.AnalysisName, timeout)
	if err != nil {
		return s.withNotice(failure("run FEM analysis", err, "")), nil, nil
	}
	if !succeeded(res) {
		out := render.ErrorResult(render.Error{
			Code:    codeFreeCAD,
			Message: fmt.Sprintf("FEM analysis '%s' failed: %v", in.AnalysisName, res["error"]),
			Hint:    "Check the prerequisites listed in this tool's description with get_objects; the details that follow include the working directory.",
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
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, string(in.ViewName))), nil, nil
}

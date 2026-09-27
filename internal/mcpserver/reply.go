package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/laelhalawani/freecad-mcp/internal/domain"
	"github.com/laelhalawani/freecad-mcp/internal/freecad"
	"github.com/laelhalawani/freecad-mcp/internal/xmlrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

// codeFreeCAD marks a failure FreeCAD reported for a well-formed request.
const codeFreeCAD = "freecad_error"

// toolError is an error that already carries its structured form.
type toolError struct{ e render.Error }

func (t *toolError) Error() string { return t.e.Message }

// failure builds an error result for err, which occurred while doing what.
func failure(what string, err error, hint string) *mcp.CallToolResult {
	var te *toolError
	if errors.As(err, &te) {
		return render.ErrorResult(te.e)
	}
	e := render.Error{Code: render.CodeInternal, Message: fmt.Sprintf("Failed to %s: %v", what, err), Hint: hint}
	var (
		fault  *xmlrpc.Fault
		perr   *xmlrpc.ProtocolError
		netErr net.Error
	)
	switch {
	case errors.Is(err, freecad.ErrInvalidTimeout):
		e.Code = render.CodeInvalidInput
	case errors.As(err, &fault):
		e.Code = codeFreeCAD
		if fault.MissingMethod() {
			e.Hint = "The FreeCAD addon is older than this server. Run `" + domain.BinaryName + " install-addon` and restart FreeCAD."
		}
	case errors.As(err, &perr):
		e.Code = render.CodeUnavailable
		if perr.StatusCode == 401 {
			e.Code = render.CodeAuth
		}
	case errors.Is(err, context.Canceled):
		e.Code = render.CodeUnavailable
		e.Message = fmt.Sprintf("Failed to %s: the request was cancelled", what)
		e.Hint = ""
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		e.Code = render.CodeUnavailable
		e.Hint = "FreeCAD did not answer in time. Call get_rpc_status to see whether a GUI operation is stuck; " +
			"it answers even while the GUI thread is busy."
	case errors.As(err, &netErr):
		// Connection refused or reset: FreeCAD or its RPC server stopped.
		e.Code = render.CodeUnavailable
		e.Hint = startHint
	}
	return render.ErrorResult(e)
}

// reported builds an error result for a failure FreeCAD reported in a reply.
func reported(what string, res map[string]any, hint string) *mcp.CallToolResult {
	msg, _ := res["error"].(string)
	if msg == "" {
		msg = "unknown error"
	}
	return render.ErrorResult(render.Error{
		Code:    codeFreeCAD,
		Message: fmt.Sprintf("Failed to %s: %s", what, msg),
		Hint:    hint,
	})
}

func succeeded(res map[string]any) bool {
	ok, _ := res["success"].(bool)
	return ok
}

func str(res map[string]any, key string) string {
	v, _ := res[key].(string)
	return v
}

// jsonBlock renders v as indented JSON in a fence.
func jsonBlock(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		data = []byte(fmt.Sprint(v))
	}
	return render.Fence(string(data), "json")
}

// textBlock renders free text, such as printed output, in a fence.
func textBlock(s string) string {
	return render.Fence(s, "text")
}

func imageContent(b64 string) (mcp.Content, bool) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return &mcp.ImageContent{Data: data, MIMEType: "image/png"}, true
}

// screenshot attaches a screenshot of view to res unless screenshots are off.
// A failed capture is logged and left out, as it is optional feedback.
func (s *Server) screenshot(ctx context.Context, conn *freecad.Connection, res *mcp.CallToolResult, include *bool, view string) *mcp.CallToolResult {
	if s.config.FreeCAD.OnlyTextFeedback || (include != nil && !*include) {
		return res
	}
	b64, err := conn.GetActiveScreenshot(ctx, viewOrDefault(view), nil, nil, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freecad-mcp: screenshot failed: %v\n", err)
		return res
	}
	if img, ok := imageContent(b64); ok {
		res.Content = append(res.Content, img)
	}
	return res
}

// withNotice prefixes a pending addon version warning to a tool reply.
func (s *Server) withNotice(res *mcp.CallToolResult) *mcp.CallToolResult {
	if n := s.fc.takeNotice(); n != "" {
		res.Content = append([]mcp.Content{&mcp.TextContent{Text: "Warning: " + n}}, res.Content...)
	}
	return res
}

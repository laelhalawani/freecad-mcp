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
	"unicode/utf8"

	"github.com/laelhalawani/freecad-mcp/internal/domain"
	"github.com/laelhalawani/freecad-mcp/internal/freecad"
	"github.com/laelhalawani/freecad-mcp/internal/xmlrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/mcp-wizard/render"
)

// codeFreeCAD marks a failure FreeCAD reported for a well-formed request.
const codeFreeCAD = "freecad_error"

// maxOutputBytes bounds the output one reply carries (printed output, object
// listings, tracebacks), so the reply stays under render.MaxBytes with room
// for its front matter, hints and a screenshot.
const maxOutputBytes = 512 << 10

// replyMargin is room kept in a reply for its JSON framing and notices.
const replyMargin = 32 << 10

// maxImageBytes is the largest PNG a reply without other content can carry:
// images travel base64-encoded, 4 bytes for every 3.
const maxImageBytes = (render.MaxBytes - replyMargin) / 4 * 3

// maxMessageBytes bounds an error message. The message says what failed;
// output that explains it goes in the reply body instead.
const maxMessageBytes = 2 << 10

// Hints for the error classes failure reports.
const (
	statusHint = "Call get_rpc_status to see whether FreeCAD's RPC server is healthy and whether a GUI " +
		"operation is stuck; it answers even while the GUI thread is busy."
	authHint = "Run `" + domain.BinaryName + " login --token <token>` with the token set with 'Set Auth Token' " +
		"in FreeCAD, or set " + domain.EnvToken + " in the AI client's config for this server."
	timeoutHint = "FreeCAD did not answer in time. Call get_rpc_status to see whether a GUI operation is " +
		"stuck; it answers even while the GUI thread is busy."
	invalidTimeoutHint = "Pass timeout as a positive number of seconds, or omit it for the default."
	decodeHint         = "FreeCAD's reply could not be read. Call get_rpc_status to check the addon; run `" +
		domain.BinaryName + " doctor` to compare the addon and server versions."
	cancelledHint = "The call was cancelled before FreeCAD answered; send it again to retry."
)

// toolError is an error that already carries its structured form.
type toolError struct{ e render.Error }

func (t *toolError) Error() string { return t.e.Message }

// failure builds an error result for err, which occurred while doing what.
// hint, when not empty, replaces the hint of err's class.
func failure(what string, err error, hint string) *mcp.CallToolResult {
	var te *toolError
	if errors.As(err, &te) {
		return render.ErrorResult(te.e)
	}
	e := render.Error{Code: render.CodeInternal, Message: shortMessage(fmt.Sprintf("Failed to %s: %v", what, err)), Hint: statusHint}
	var (
		fault  *xmlrpc.Fault
		perr   *xmlrpc.ProtocolError
		derr   *xmlrpc.DecodeError
		netErr net.Error
	)
	switch {
	case errors.Is(err, freecad.ErrInvalidTimeout):
		e.Code = render.CodeInvalidInput
		e.Hint = invalidTimeoutHint
	case errors.As(err, &fault):
		e.Code = codeFreeCAD
		e.Hint = "FreeCAD reported this error while handling the call. Check the arguments and retry. " + statusHint
		if fault.MissingMethod() {
			e.Hint = "The FreeCAD addon is older than this server. Run `" + domain.BinaryName + " install-addon` and restart FreeCAD."
		}
	case errors.As(err, &perr):
		e.Code = render.CodeUnavailable
		e.Hint = fmt.Sprintf("FreeCAD's RPC server answered HTTP %d. Run `%s doctor` to check the setup, then retry.",
			perr.StatusCode, domain.BinaryName)
		if perr.StatusCode == 401 {
			e.Code = render.CodeAuth
			e.Hint = authHint
		}
	case errors.Is(err, context.Canceled):
		e.Code = render.CodeUnavailable
		e.Message = fmt.Sprintf("Failed to %s: the request was cancelled", what)
		e.Hint = cancelledHint
	case isTimeout(err):
		e.Code = render.CodeUnavailable
		e.Hint = timeoutHint
	case errors.As(err, &netErr):
		// Connection refused or reset: FreeCAD or its RPC server stopped.
		e.Code = render.CodeUnavailable
		e.Hint = startHint
	case errors.As(err, &derr):
		e.Code = render.CodeInternal
		e.Hint = decodeHint
	}
	if hint != "" {
		e.Hint = hint
	}
	return render.ErrorResult(e)
}

// isTimeout reports whether err is a reply that did not arrive in time.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout()
}

// timedFailure is failure for a tool that takes a timeout: when FreeCAD did
// not answer in time, the hint also says how to allow more time (larger).
func timedFailure(what string, err error, larger string) *mcp.CallToolResult {
	if !errors.Is(err, context.Canceled) && isTimeout(err) {
		return failure(what, err, timeoutHint+" "+larger)
	}
	return failure(what, err, "")
}

// reported builds an error result for a failure FreeCAD reported in a reply.
func reported(what string, res map[string]any, hint string) *mcp.CallToolResult {
	msg, _ := res["error"].(string)
	if msg == "" {
		msg = "unknown error"
	}
	return render.ErrorResult(render.Error{
		Code:    codeFreeCAD,
		Message: shortMessage(fmt.Sprintf("Failed to %s: %s", what, msg)),
		Hint:    hint,
	})
}

// shortMessage keeps the start of an error message, which says what failed,
// within maxMessageBytes.
func shortMessage(s string) string {
	if len(s) <= maxMessageBytes {
		return s
	}
	cut := maxMessageBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " ... (message truncated)"
}

// truncateOutput keeps the last maxOutputBytes of s, the part that holds the
// result or the error, and says how much was left out.
func truncateOutput(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	tail := s[len(s)-maxOutputBytes:]
	// Start on a whole line when one begins nearby, else on a whole character.
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < 4<<10 {
		tail = tail[i+1:]
	} else {
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
	}
	return fmt.Sprintf("[output truncated: the first %d of %d bytes are left out, the last %d follow]\n%s",
		len(s)-len(tail), len(s), len(tail), tail)
}

func succeeded(res map[string]any) bool {
	ok, _ := res["success"].(bool)
	return ok
}

func str(res map[string]any, key string) string {
	v, _ := res[key].(string)
	return v
}

// jsonBlock renders v as indented JSON in a fence, truncated to its last
// maxOutputBytes.
func jsonBlock(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		data = []byte(fmt.Sprint(v))
	}
	return render.Fence(truncateOutput(string(data)), "json")
}

// textBlock renders free text, such as printed output, in a fence, truncated
// to its last maxOutputBytes.
func textBlock(s string) string {
	return render.Fence(truncateOutput(s), "text")
}

func imageContent(b64 string) (mcp.Content, bool) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return &mcp.ImageContent{Data: data, MIMEType: "image/png"}, true
}

// screenshot attaches a screenshot of view to res unless screenshots are off.
// A failed capture is logged and left out, as it is optional feedback;
// get_view reports the failure as an error. So is a screenshot that would
// take the reply past render.MaxBytes.
func (s *Server) screenshot(ctx context.Context, conn *freecad.Connection, res *mcp.CallToolResult, include *bool, view string) *mcp.CallToolResult {
	if s.config.FreeCAD.OnlyTextFeedback || (include != nil && !*include) {
		return res
	}
	b64, err := conn.GetActiveScreenshot(ctx, viewOrDefault(view), nil, nil, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freecad-mcp: screenshot failed: %v\n", err)
		return res
	}
	img, ok := imageContent(b64)
	if !ok {
		return res
	}
	text := 0
	for _, c := range res.Content {
		if tc, isText := c.(*mcp.TextContent); isText {
			text += len(tc.Text)
		}
	}
	if size := len(img.(*mcp.ImageContent).Data); size > (render.MaxBytes-replyMargin-text)/4*3 {
		fmt.Fprintf(os.Stderr, "freecad-mcp: screenshot of %d KiB left out: the reply would exceed 1 MiB\n", size>>10)
		return res
	}
	res.Content = append(res.Content, img)
	return res
}

// withNotice prefixes a pending addon version warning to a tool reply.
func (s *Server) withNotice(res *mcp.CallToolResult) *mcp.CallToolResult {
	if n := s.fc.takeNotice(); n != "" {
		res.Content = append([]mcp.Content{&mcp.TextContent{Text: "Warning: " + n}}, res.Content...)
	}
	return res
}

// invalidArguments turns the error the SDK returns for arguments that fail a
// tool's input schema, or cannot be decoded into its input, into the
// invalid_input error the tools themselves return. The tools never set such
// an error on a result, so one that carries it came from the SDK.
func invalidArguments(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if err != nil || method != "tools/call" {
			return result, err
		}
		res, ok := result.(*mcp.CallToolResult)
		if !ok || res == nil || !res.IsError || res.GetError() == nil {
			return result, err
		}
		tool := "the tool"
		if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil && call.Params.Name != "" {
			tool = call.Params.Name
		}
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: shortMessage("Invalid arguments: " + argumentProblem(res.GetError())),
			Hint: fmt.Sprintf("Call %s again with arguments that match its input schema: every required "+
				"argument, each of the listed type, and only listed values and argument names.", tool),
		}), nil
	}
}

// argumentProblem states an SDK argument error without the SDK's framing.
func argumentProblem(err error) string {
	msg := err.Error()
	for _, prefix := range []string{`validating "arguments": `, "validating root: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	msg = strings.TrimPrefix(msg, "json: ")
	return msg
}

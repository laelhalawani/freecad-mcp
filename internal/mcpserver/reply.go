package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
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
		// The same rejection connector.get, probe and start_freecad already
		// classify this way (protocolRejectedError): FreeCAD answered but
		// refused the request, so this is not a generic failure with a
		// "Failed to <what>" prefix, just the rejection itself.
		e = protocolRejectedError(perr)
	case errors.Is(err, context.Canceled):
		e.Code = render.CodeUnavailable
		e.Message = fmt.Sprintf("Failed to %s: the request was cancelled", what)
		e.Hint = cancelledHint
	case isTimeout(err):
		e.Code = render.CodeUnavailable
		e.Hint = timeoutHint
	case errors.As(err, &netErr):
		// Connection refused or reset (never a timeout: that already matched
		// the isTimeout case above): FreeCAD's RPC server stopped answering,
		// most likely after a call cached this connection while it still
		// worked. This function has no configured host to check, so it does
		// not know whether start_freecad could even start FreeCAD here (a
		// remote FREECAD_MCP_HOST would only refuse); get_rpc_status (whose
		// own probe drops this same dead connection once it also fails
		// through it) works out the host-aware next step, so point there.
		e.Code = render.CodeUnavailable
		e.Hint = statusHint
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

// boolField reads a boolean reply field, or false when it is absent or not a
// bool (the addon always sends one for the keys this reads).
func boolField(res map[string]any, key string) bool {
	b, _ := res[key].(bool)
	return b
}

// number reads a reply field that arrived as one of XML-RPC's two numeric
// kinds, rejecting a NaN or infinite float (the addon replaces those with
// None before sending them, so a genuine one never reaches here).
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	}
	return 0, false
}

// intField reads a numeric reply field as an int, or 0 when it is absent or
// not a number (a malformed reply; the addon always sends one).
func intField(res map[string]any, key string) int {
	f, _ := number(res[key])
	return int(f)
}

// intFromStatus reads an integer a status dict may hold as int64 or float64,
// or (0, false) when it is absent, not a number, or status itself is nil.
func intFromStatus(status map[string]any, key string) (int, bool) {
	if status == nil {
		return 0, false
	}
	f, ok := number(status[key])
	return int(f), ok
}

// bigIntField reads a numeric reply field that may exceed the range a
// float64 represents exactly (serialize.serialize_int falls back to a float
// or a decimal string for those), returning 0 when it is absent or cannot be
// parsed.
func bigIntField(res map[string]any, key string) int64 {
	switch v := res[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case string:
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i
		}
	}
	return 0
}

// formatMeasure renders a numeric reply value with its unit, or "unavailable"
// when the addon sent none (a NaN or infinite result, replaced by None).
func formatMeasure(v any, unit string) string {
	if f, ok := number(v); ok {
		return fmt.Sprintf("%s %s", fmt.Sprintf("%.4g", f), unit)
	}
	return fmt.Sprintf("unavailable (%s)", unit)
}

// stringItems reads a []any of strings (an addon reply list) as a []string,
// skipping anything that is not a string. v may be a raw reply field or
// already a []any; nil or any other type gives [].
func stringItems(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// jsonStrings renders a list of strings as a JSON array for a hint's
// copy-pasteable arguments.
func jsonStrings(items []any) string {
	data, err := json.Marshal(stringItems(items))
	if err != nil {
		return "[]"
	}
	return string(data)
}

// capList returns at most limit items of list, and how many were left out.
func capList(list []any, limit int) ([]any, int) {
	if len(list) <= limit {
		return list, 0
	}
	return list[:limit], len(list) - limit
}

// objRef reads an ObjRef ({"name","label","type"}) nested at key in res.
func objRef(res map[string]any, key string) (name, label, typ string) {
	obj, _ := res[key].(map[string]any)
	return str(obj, "name"), str(obj, "label"), str(obj, "type")
}

// transactionFields reads the transaction name and merged flag a mutating
// reply carries: the name of the transaction holding the changes (this
// call's own, or one it joined because a command or task panel was already
// open in FreeCAD), and whether it joined one already open.
func transactionFields(res map[string]any) (name string, merged bool) {
	return str(res, "transaction"), boolField(res, "transaction_merged")
}

// transactionNote appends the sentence explaining a merged transaction to
// body, when the call's changes joined one already open in FreeCAD instead of
// their own.
func transactionNote(body, name string, merged bool) string {
	if !merged || name == "" {
		return body
	}
	return body + fmt.Sprintf("\n\nThe changes joined FreeCAD's open '%s' transaction (a command or task panel "+
		"is active); they are undone together with it.", name)
}

// invalidObjectsCount reads how many objects are invalid: the addon's
// invalid_count when the reply carries it (the true total, even when
// invalid_objects itself was capped), else the length of invalid_objects.
func invalidObjectsCount(res map[string]any) int {
	if f, ok := number(res["invalid_count"]); ok {
		return int(f)
	}
	objs, _ := res["invalid_objects"].([]any)
	return len(objs)
}

// fixOrRemoveHint is the hint every invalid or created-but-invalid object
// gets: the copy-pasteable update_object call that could fix it, or the
// delete_object call that removes it.
func fixOrRemoveHint(docName, objName string) string {
	return fmt.Sprintf("Call update_object with {\"doc_name\": %q, \"obj_name\": %q, "+
		"\"obj_properties\": {\"<Property>\": <value>}} to fix it, or delete_object with "+
		"{\"doc_name\": %q, \"obj_name\": %q} to remove it.", docName, objName, docName, objName)
}

// invalidObjectRow renders one InvalidObj (name, label, type, state, status)
// as a bullet with a hint to fix or remove it.
func invalidObjectRow(obj map[string]any, docName string) string {
	name := str(obj, "name")
	status := str(obj, "status")
	switch {
	case status == "":
		status = "no status reported."
	case !strings.HasSuffix(status, "."):
		status += "."
	}
	return fmt.Sprintf("%s (%s): %s %s", name, str(obj, "type"), status, fixOrRemoveHint(docName, name))
}

// invalidObjectsBody renders every row of a mutating reply's invalid_objects
// into a body section, or "" when there are none. docName names the document
// they belong to, for the update_object/delete_object hints in each row. The
// addon reports the true count as invalid_count and whether the list itself
// was capped as invalid_truncated; a reply without those two fields is read
// as exactly the list's own length, not capped. A caller expecting a very
// long list of its own (import_file, whose created_objects can run to
// thousands) caps how many rows it prints itself, using invalidObjectRow and
// invalidObjectsCount directly instead of this function.
func invalidObjectsBody(res map[string]any, docName string) string {
	objs, _ := res["invalid_objects"].([]any)
	if len(objs) == 0 {
		return ""
	}
	total := invalidObjectsCount(res)
	var b strings.Builder
	if boolField(res, "invalid_truncated") {
		fmt.Fprintf(&b, "\n\n%d invalid object(s), showing %d:", total, len(objs))
	} else {
		fmt.Fprintf(&b, "\n\n%d invalid object(s):", total)
	}
	for _, item := range objs {
		if obj, ok := item.(map[string]any); ok {
			b.WriteString("\n- " + invalidObjectRow(obj, docName))
		}
	}
	return b.String()
}

// largerTimeout says how to give tool a slow call more time, up to the 1800 s
// ceiling every tool that takes a timeout shares.
func largerTimeout(tool string) string {
	return fmt.Sprintf("For slow work, call %s again with a larger timeout (at most 1800 seconds).", tool)
}

// hintAppliesTo reports whether code is one of codes, defaulting to
// not_found alone when codes is empty.
func hintAppliesTo(code string, codes []string) bool {
	if len(codes) == 0 {
		return code == render.CodeNotFound
	}
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
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

// Default hints for addon error codes, used when neither the addon nor the
// tool gives one.
var codeHints = map[string]string{
	render.CodeNotFound:     "Call list_documents with {} to see the open documents, and list_objects with {\"doc_name\": \"<name>\"} to see a document's objects.",
	render.CodeInvalidInput: "Check the arguments against the tool's input schema and description, then call it again.",
	render.CodeConflict:     "Resolve the conflict the message describes, then call the tool again.",
	render.CodeUnavailable:  statusHint,
	render.CodeInternal:     statusHint,
	codeFreeCAD:             "FreeCAD reported this error while handling the call. Check the arguments and retry. " + statusHint,
}

// reportedCode builds an error result for a failure the addon reported in a
// reply, keeping the addon's classification. The addon's code (not_found,
// invalid_input, conflict, unavailable, internal_error, freecad_error) becomes
// the error code; GUI_DISPATCH_STUCK becomes unavailable and anything else
// freecad_error. The hint is the addon's when it sent one; otherwise hint is
// used only for the codes hintCodes names (default: not_found alone, since
// most callers' hint says how to find a missing document or object by name,
// which only helps when that is the failure), and codeHints supplies the
// rest. A caller whose hint is right for another code as well, such as a
// conflict with a specific retry, names that code explicitly. The addon's
// details become the error's fields.
func reportedCode(what string, res map[string]any, hint string, hintCodes ...string) *mcp.CallToolResult {
	msg, _ := res["error"].(string)
	if msg == "" {
		msg = "unknown error"
	}
	code, _ := res["code"].(string)
	switch code {
	case render.CodeNotFound, render.CodeInvalidInput, render.CodeConflict, render.CodeUnavailable, render.CodeInternal, codeFreeCAD:
	case "GUI_DISPATCH_STUCK":
		code = render.CodeUnavailable
	default:
		code = codeFreeCAD
	}
	if h, _ := res["hint"].(string); h != "" {
		hint = h
	} else if !hintAppliesTo(code, hintCodes) {
		hint = codeHints[code]
	}
	if hint == "" {
		hint = codeHints[code]
	}
	e := render.Error{
		Code:    code,
		Message: shortMessage(fmt.Sprintf("Failed to %s: %s", what, msg)),
		Hint:    hint,
	}
	if details, ok := res["details"].(map[string]any); ok && len(details) > 0 {
		e.Fields = details
	}
	return render.ErrorResult(e)
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

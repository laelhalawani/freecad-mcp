package mcpserver

// The error log: one line per failed tool call, kept across sessions so the
// person running the agents can review what went wrong (`freecad-mcp errors`).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/headless"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
	"gopkg.in/yaml.v3"
)

const (
	errorLogName = "errors.log"
	// errorLogMaxBytes is where the log is rotated to errors.log.1, replacing
	// the earlier copy: an anti-break guard so the file cannot grow without end.
	errorLogMaxBytes = 5 << 20
	// errorLogArgChars cuts each argument value in a log line.
	errorLogArgChars = 200
)

// secretArgument matches the names of arguments whose values are never logged.
var secretArgument = regexp.MustCompile(`(?i)token|password|secret`)

// ErrorLogPath is the error log file: errors.log in freecad-mcp's cache
// directory.
func ErrorLogPath() (string, error) {
	dir, err := headless.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, errorLogName), nil
}

var errorLogMu sync.Mutex

// appendErrorLog appends line to the error log, rotating it first when it is
// over errorLogMaxBytes. A failure to write is ignored: the log never fails
// the tool call it describes.
func appendErrorLog(line string) {
	path, err := ErrorLogPath()
	if err != nil {
		return
	}
	errorLogMu.Lock()
	defer errorLogMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if info, err := os.Stat(path); err == nil && info.Size() > errorLogMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

// RecentErrors returns the last n lines of the error log, oldest first: the
// rotated copy is read before the current file.
func RecentErrors(n int) ([]string, error) {
	path, err := ErrorLogPath()
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, p := range []string{path + ".1", path} {
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		r := bufio.NewReader(f)
		for {
			line, err := r.ReadString('\n')
			if line = strings.TrimRight(line, "\r\n"); line != "" {
				lines = append(lines, line)
			}
			if err != nil {
				break
			}
		}
		f.Close()
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// logToolErrors is the receiving middleware that logs every failed tools/call:
// an error reply, an argument error the SDK refused, or a Go error. It sits
// innermost, so a call that moved to the background is logged when it ends and
// the session identity is already in its context.
func logToolErrors(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		call, ok := req.(*mcp.CallToolRequest)
		if method != "tools/call" || !ok || call.Params == nil {
			return result, err
		}
		code, message, hint := "", "", ""
		switch res, _ := result.(*mcp.CallToolResult); {
		case err != nil:
			code, message = "protocol_error", err.Error()
		case res != nil && res.IsError:
			code, message, hint = errorOfResult(res)
		default:
			return result, err
		}
		headers := xmlrpc.HeadersFrom(ctx)
		appendErrorLog(errorLogLine(time.Now(), headers[domain.HeaderSession], headers[domain.HeaderClient],
			call.Params.Name, code, message, hint, call.Params.Arguments))
		return result, err
	}
}

// errorOfResult reads the code, message and hint of an error reply: from the
// front matter render.ErrorResult wrote, or from the SDK's own error when it
// refused the call's arguments.
func errorOfResult(res *mcp.CallToolResult) (code, message, hint string) {
	if e := res.GetError(); e != nil {
		return "invalid_input", argumentProblem(e), ""
	}
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			continue
		}
		text, ok := strings.CutPrefix(tc.Text, "---\n")
		if !ok {
			return "", shortErrorText(tc.Text), ""
		}
		front, _, _ := strings.Cut(text, "\n---")
		var doc struct {
			Error struct {
				Code    string `yaml:"code"`
				Message string `yaml:"message"`
				Hint    string `yaml:"hint"`
			} `yaml:"error"`
		}
		if yaml.Unmarshal([]byte(front), &doc) == nil && doc.Error.Code != "" {
			return doc.Error.Code, doc.Error.Message, doc.Error.Hint
		}
		return "", shortErrorText(tc.Text), ""
	}
	return "", "", ""
}

func shortErrorText(s string) string { return capRunes(s, 500) }

// errorLogLine renders one log line: UTC time, session id, client label,
// tool, error code, message, hint and an argument summary. Text fields are
// quoted, so a line never spans lines.
func errorLogLine(at time.Time, session, client, tool, code, message, hint string, arguments json.RawMessage) string {
	if code == "" {
		code = "unknown"
	}
	return fmt.Sprintf("%s session=%s client=%s tool=%s code=%s message=%s hint=%s args=%s",
		at.UTC().Format("2006-01-02T15:04:05Z"), orDash(session), strconv.Quote(client), orDash(tool), code,
		strconv.Quote(message), strconv.Quote(hint), argumentSummary(arguments))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// argumentSummary lists a call's argument names with their values cut to
// errorLogArgChars characters, as one JSON object; the value of an argument
// named like a secret is left out.
func argumentSummary(arguments json.RawMessage) string {
	var args map[string]any
	if len(arguments) == 0 || json.Unmarshal(arguments, &args) != nil {
		return "{}"
	}
	summary := make(map[string]string, len(args))
	for name, value := range args {
		if secretArgument.MatchString(name) {
			summary[name] = "(hidden)"
			continue
		}
		text, ok := value.(string)
		if !ok {
			b, _ := json.Marshal(value)
			text = string(b)
		}
		summary[name] = capRunes(text, errorLogArgChars)
	}
	b, _ := json.Marshal(summary)
	return string(b)
}

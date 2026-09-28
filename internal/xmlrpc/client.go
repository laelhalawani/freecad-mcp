package xmlrpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

// ProtocolError is a non-200 HTTP reply. Its message never carries the
// request's credentials, because tool replies show it to the model.
type ProtocolError struct {
	StatusCode int
	Status     string
	URL        string
	// Listener is the X-FreeCAD-MCP-Listener header of the reply: "1" when a
	// freecad-mcp listener answered, "freecad-not-running" on its 503 while
	// FreeCAD is down, "" when the addon (or anything else) answered.
	Listener string
	// Body is the first 4 KiB of the reply's body (the listener's JSON error,
	// listenerapi.Error), with the password scrubbed; set only when the
	// listener marker is present, else "".
	Body string
	// RetryAfter is the Retry-After header in seconds (a listener's 429), or
	// 0 when it is absent or not a whole number of seconds.
	RetryAfter int
}

// maxErrorBody bounds ProtocolError.Body.
const maxErrorBody = 4 << 10

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("XML-RPC request to %s failed: HTTP %s", e.URL, e.Status)
}

type headersKey struct{}

// WithHeaders returns a context whose calls through Client.Call carry the
// given HTTP headers (the MCP session identity). Headers added again replace
// earlier values of the same name.
func WithHeaders(ctx context.Context, headers map[string]string) context.Context {
	merged := map[string]string{}
	for k, v := range HeadersFrom(ctx) {
		merged[k] = v
	}
	for k, v := range headers {
		merged[k] = v
	}
	return context.WithValue(ctx, headersKey{}, merged)
}

// HeadersFrom returns the headers WithHeaders put into ctx, or nil. Callers
// must not modify the map.
func HeadersFrom(ctx context.Context) map[string]string {
	h, _ := ctx.Value(headersKey{}).(map[string]string)
	return h
}

// Client calls one XML-RPC endpoint.
type Client struct {
	url   string
	token string
	http  *http.Client

	// OnReply, when set, sees the headers of every HTTP reply, whatever its
	// status (the addon's X-FreeCAD-MCP-Lock). Set it before the first call.
	OnReply func(http.Header)
}

// NewClient returns a client for http://host:port/RPC2. A non-empty token is
// sent as "Authorization: Bearer <token>".
func NewClient(host string, port int, token string) *Client {
	return &Client{
		url:   "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/RPC2",
		token: token,
		// The transport pools connections and opens a new one while another
		// request is blocked, so a status call never queues behind a GUI call.
		http: &http.Client{Transport: &http.Transport{
			Proxy:               nil,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
		}},
	}
}

// URL returns the endpoint without credentials.
func (c *Client) URL() string { return c.url }

// Call invokes method with params and waits at most timeout for the reply.
func (c *Client) Call(ctx context.Context, timeout time.Duration, method string, params ...any) (any, error) {
	body, err := EncodeCall(method, params...)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", method, err)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range HeadersFrom(ctx) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "text/xml")
	req.Header.Set("User-Agent", "freecad-mcp")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, scrub(err, c.token)
	}
	defer resp.Body.Close()
	if c.OnReply != nil {
		c.OnReply(resp.Header)
	}
	if resp.StatusCode != http.StatusOK {
		perr := &ProtocolError{StatusCode: resp.StatusCode, Status: resp.Status, URL: c.url,
			Listener: resp.Header.Get(domain.HeaderListener)}
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
			perr.RetryAfter = secs
		}
		if perr.Listener != "" {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
			if len(data) > 0 {
				perr.Body = Scrub(errors.New(string(data)), c.token).Error()
			}
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, perr
	}
	v, err := DecodeResponse(resp.Body)
	if err != nil {
		if _, ok := err.(*Fault); ok {
			return nil, err
		}
		return nil, &DecodeError{Method: method, Err: scrub(err, c.token)}
	}
	return v, nil
}

// DecodeError is a reply that arrived but could not be read as XML-RPC,
// such as a malformed or cut-off document.
type DecodeError struct {
	Method string
	Err    error
}

func (e *DecodeError) Error() string { return e.Method + ": " + e.Err.Error() }
func (e *DecodeError) Unwrap() error { return e.Err }

// CloseIdle closes pooled connections.
func (c *Client) CloseIdle() { c.http.CloseIdleConnections() }

type scrubbed struct {
	msg string
	err error
}

func (s *scrubbed) Error() string { return s.msg }
func (s *scrubbed) Unwrap() error { return s.err }

// Scrub removes token from an error message, keeping the error chain. Every
// error that could carry a password passes through it before it reaches a
// log, a tool reply or the screen.
func Scrub(err error, token string) error { return scrub(err, token) }

// scrub removes the token from an error message, keeping the error chain.
func scrub(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return &scrubbed{msg: strings.ReplaceAll(err.Error(), token, "***"), err: err}
}

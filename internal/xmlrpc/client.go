package xmlrpc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ProtocolError is a non-200 HTTP reply. Its message never carries the
// request's credentials, because tool replies show it to the model.
type ProtocolError struct {
	StatusCode int
	Status     string
	URL        string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("XML-RPC request to %s failed: HTTP %s", e.URL, e.Status)
}

// Client calls one XML-RPC endpoint.
type Client struct {
	url   string
	token string
	http  *http.Client
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
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &ProtocolError{StatusCode: resp.StatusCode, Status: resp.Status, URL: c.url}
	}
	v, err := DecodeResponse(resp.Body)
	if err != nil {
		if _, ok := err.(*Fault); ok {
			return nil, err
		}
		return nil, fmt.Errorf("%s: %w", method, scrub(err, c.token))
	}
	return v, nil
}

// CloseIdle closes pooled connections.
func (c *Client) CloseIdle() { c.http.CloseIdleConnections() }

type scrubbed struct {
	msg string
	err error
}

func (s *scrubbed) Error() string { return s.msg }
func (s *scrubbed) Unwrap() error { return s.err }

// scrub removes the token from an error message, keeping the error chain.
func scrub(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return &scrubbed{msg: strings.ReplaceAll(err.Error(), token, "***"), err: err}
}

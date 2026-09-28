// Package remote is the client of a freecad-mcp listener on another computer
// (or behind an SSH tunnel): detection by the listener marker, status and
// start.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
)

// Endpoint is a listener address and the password to present.
type Endpoint struct {
	Host  string
	Port  int
	Token string
}

// ErrNotListener means something answered without the listener marker
// (domain.HeaderListener): not a freecad-mcp listener, whatever the status.
var ErrNotListener = errors.New("not a freecad-mcp listener")

// ErrPasswordRequired means a listener answered 401: the password is missing
// or wrong.
var ErrPasswordRequired = errors.New("the freecad-mcp listener asks for a password")

// Error is any other refusal from a listener (marker present, status not
// 200 or 401), with the listener's error body.
type Error struct {
	StatusCode int
	Body       listenerapi.Error
	// RetryAfter is the Retry-After header in seconds (a 429), or 0 when it
	// is absent or not a whole number of seconds.
	RetryAfter int
}

// Error returns the listener's own refusal text, or a default naming its
// status code when the listener sent none (an empty listenerapi.Error.Error,
// which a caller printing this bare would otherwise show as "[fail] " with
// nothing after it).
func (e *Error) Error() string {
	if e.Body.Error != "" {
		return e.Body.Error
	}
	return fmt.Sprintf("the listener refused the request (HTTP %d)", e.StatusCode)
}

// httpClient calls listener endpoints. Its transport pools connections the
// same way internal/xmlrpc.Client does, so repeated status polling (the
// status widget, the doctor, get_rpc_status) does not open a new socket
// every time.
var httpClient = &http.Client{Transport: &http.Transport{
	Proxy:               nil,
	MaxIdleConnsPerHost: 4,
	IdleConnTimeout:     30 * time.Second,
}}

// call posts body (JSON-encoded) to path on ep and waits at most timeout.
// When withSession is true, the session headers carried in ctx (see
// xmlrpc.WithHeaders) go out too. It returns the listener marker header,
// the HTTP status, the response body and its Retry-After header in seconds
// (0 when absent or not a whole number of seconds), or a network error when
// nothing answered (connection refused, reset, closed without a response,
// or timeout): that error is never turned into ErrNotListener or
// ErrPasswordRequired, since it means unreachable, not a classified reply.
func call(ctx context.Context, ep Endpoint, path string, timeout time.Duration, body any, withSession bool) (marker string, statusCode int, respBody []byte, retryAfter int, err error) {
	data, err := json.Marshal(body)
	if err != nil {
		return "", 0, nil, 0, fmt.Errorf("encode request: %w", err)
	}
	url := "http://" + net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)) + path
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", 0, nil, 0, err
	}
	req.Header.Set("Content-Type", listenerapi.ContentTypeJSON)
	req.Header.Set("User-Agent", "freecad-mcp")
	if ep.Token != "" {
		req.Header.Set("Authorization", "Bearer "+ep.Token)
	}
	if withSession {
		for k, v := range xmlrpc.HeadersFrom(ctx) {
			req.Header.Set(k, v)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, nil, 0, xmlrpc.Scrub(err, ep.Token)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, listenerapi.MaxBodyBytes))
	if secs, aerr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); aerr == nil && secs > 0 {
		retryAfter = secs
	}
	return resp.Header.Get(domain.HeaderListener), resp.StatusCode, b, retryAfter, nil
}

// classify turns a status code, a (possibly absent) listenerapi.Error body
// and the Retry-After seconds call read into the error Probe, Status and
// Start return for a reply that carried the listener marker: nil for 200,
// ErrPasswordRequired for 401, *Error for anything else. A body that fails
// to decode still yields *Error with an empty Body, since the marker already
// proved this is a listener.
func classify(statusCode int, body []byte, retryAfter int) error {
	if statusCode == http.StatusOK {
		return nil
	}
	if statusCode == http.StatusUnauthorized {
		return ErrPasswordRequired
	}
	var e listenerapi.Error
	_ = json.Unmarshal(body, &e)
	return &Error{StatusCode: statusCode, Body: e, RetryAfter: retryAfter}
}

// Probe reports whether ep is a freecad-mcp listener. Any response carrying
// the listener marker (domain.HeaderListener) is a listener, whatever its
// status: true with a nil error for a 200, true with the refusal returned
// alongside for any other status (ErrPasswordRequired for a 401, *Error for
// a 403 while remote access is off there, a 429 after too many bad
// passwords, a 405 or 415). Something that answered without the marker
// gives false and ErrNotListener; nothing answering gives false and the
// network error. It waits at most listenerapi.ProbeTimeout.
func Probe(ctx context.Context, ep Endpoint) (bool, error) {
	marker, status, body, retryAfter, err := call(ctx, ep, listenerapi.PathStatus, listenerapi.ProbeTimeout, struct{}{}, false)
	if err != nil {
		return false, err
	}
	if marker == "" {
		return false, ErrNotListener
	}
	return true, classify(status, body, retryAfter)
}

// Status calls /listener/status with the password and the session headers
// in ctx. A refusal is ErrNotListener, ErrPasswordRequired or *Error.
func Status(ctx context.Context, ep Endpoint) (listenerapi.Status, error) {
	marker, status, body, retryAfter, err := call(ctx, ep, listenerapi.PathStatus, listenerapi.StatusTimeout, struct{}{}, true)
	if err != nil {
		return listenerapi.Status{}, err
	}
	if marker == "" {
		return listenerapi.Status{}, ErrNotListener
	}
	if cerr := classify(status, body, retryAfter); cerr != nil {
		return listenerapi.Status{}, cerr
	}
	var st listenerapi.Status
	if err := json.Unmarshal(body, &st); err != nil {
		return listenerapi.Status{}, fmt.Errorf("decode listener status: %w", err)
	}
	return st, nil
}

// Start calls /listener/start to start FreeCAD on the listener's computer,
// opening file (a path there) when it is not "". Errors as Status.
func Start(ctx context.Context, ep Endpoint, file string) (listenerapi.StartResult, error) {
	marker, status, body, retryAfter, err := call(ctx, ep, listenerapi.PathStart, listenerapi.StartTimeout,
		listenerapi.StartRequest{File: file}, true)
	if err != nil {
		return listenerapi.StartResult{}, err
	}
	if marker == "" {
		return listenerapi.StartResult{}, ErrNotListener
	}
	if cerr := classify(status, body, retryAfter); cerr != nil {
		return listenerapi.StartResult{}, cerr
	}
	var res listenerapi.StartResult
	if err := json.Unmarshal(body, &res); err != nil {
		return listenerapi.StartResult{}, fmt.Errorf("decode listener start result: %w", err)
	}
	return res, nil
}

// Package xmlrpctest serves a fake FreeCAD addon for tests.
package xmlrpctest

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/xmlrpc"
)

// Handler answers one method. Returning a *xmlrpc.Fault sends a fault.
type Handler func(params []any) (any, error)

// Call is one request the server received.
type Call struct {
	Method string
	Params []any
	Header http.Header // the request's HTTP headers
}

// Server is a fake XML-RPC server.
type Server struct {
	Host  string
	Port  int
	Token string // when set, requests need "Authorization: Bearer <Token>"

	srv      *httptest.Server
	mu       sync.Mutex
	handlers map[string]Handler
	calls    []Call
}

// New starts a server with the given handlers; it stops when the test ends.
// Unknown methods fault the way SimpleXMLRPCServer does.
func New(t testing.TB, handlers map[string]Handler) *Server {
	t.Helper()
	s := &Server{handlers: map[string]Handler{}}
	for k, v := range handlers {
		s.handlers[k] = v
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	host, port, _ := net.SplitHostPort(s.srv.Listener.Addr().String())
	s.Host = host
	s.Port, _ = strconv.Atoi(port)
	return s
}

// Close stops the server, as FreeCAD exiting would.
func (s *Server) Close() {
	s.srv.CloseClientConnections()
	s.srv.Close()
}

// Handle sets or replaces a method handler.
func (s *Server) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// Calls returns the requests received so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CallsTo returns the requests for one method.
func (s *Server) CallsTo(method string) []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	// The addon's browser guard (ip_filter.py) refuses these before dispatch;
	// enforcing it here shows the Go client passes it.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "requests from web pages are not accepted", http.StatusForbidden)
		return
	}
	if mt := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); mt != "text/xml" && mt != "application/xml" {
		http.Error(w, "XML-RPC requests must use Content-Type text/xml", http.StatusUnsupportedMediaType)
		return
	}
	if host, _, err := net.SplitHostPort(r.Host); err != nil || !(host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		http.Error(w, "the local RPC server only accepts requests addressed to localhost", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	token := s.Token
	s.mu.Unlock()
	if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, "Unauthorized: valid auth token required", http.StatusUnauthorized)
		return
	}
	method, params, err := xmlrpc.DecodeCall(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: method, Params: params, Header: r.Header.Clone()})
	h := s.handlers[method]
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml")
	if h == nil {
		w.Write(xmlrpc.EncodeFault(1, `<class 'Exception'>:method "`+method+`" is not supported`))
		return
	}
	v, err := h(params)
	if err != nil {
		if f, ok := err.(*xmlrpc.Fault); ok {
			w.Write(xmlrpc.EncodeFault(f.Code, f.String))
			return
		}
		w.Write(xmlrpc.EncodeFault(1, err.Error()))
		return
	}
	body, err := xmlrpc.EncodeResponse(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(body)
}

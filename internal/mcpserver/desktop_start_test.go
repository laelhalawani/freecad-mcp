package mcpserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/freecad-mcp/internal/remote"
)

// hiddenServer is a server whose own process is in Windows session 0 and whose
// FreeCAD is down (port 9 answers nothing), so start_freecad takes the path
// that asks the listener on this computer. desktopListener names that listener.
func hiddenServer(t *testing.T, desktopListener func(context.Context, string) remote.Endpoint) *Server {
	t.Helper()
	srv := New(Config{Version: "1.2.3", FreeCAD: domain.Settings{Host: "127.0.0.1", Port: 9, Token: "configured-token"}})
	srv.sessionOf = func(int) (uint32, error) { return 0, nil }
	srv.desktopListener = desktopListener
	return srv
}

// fakeListener answers /listener/start with reply the way a freecad-mcp
// listener does (the marker header on every response), recording the
// Authorization header and request body it saw.
func fakeListener(t *testing.T, status int, reply any) (remote.Endpoint, *[]string) {
	t.Helper()
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req listenerapi.StartRequest
		json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization")+" file="+req.File)
		w.Header().Set(domain.HeaderListener, domain.ListenerMarker)
		w.Header().Set("Content-Type", listenerapi.ContentTypeJSON)
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	port, _ := strconv.Atoi(u.Port())
	return remote.Endpoint{Host: u.Hostname(), Port: port, Token: "listener-token"}, &seen
}

func TestStartFreeCADFromASessionWithoutADesktopGoesThroughTheListener(t *testing.T) {
	elapsed := 0.5
	ep, seen := fakeListener(t, http.StatusOK, listenerapi.StartResult{
		State: "started",
		FreeCAD: listenerapi.LaunchInfo{
			State: "started", PID: os.Getpid(), PIDIsLauncher: true, Executable: `C:\FreeCAD\bin\FreeCAD.exe`,
			LogFile: `C:\Users\u\.cache\freecad-mcp\freecad-gui-1.log`, Port: 9875, ElapsedSeconds: &elapsed,
		},
	})
	srv := hiddenServer(t, func(_ context.Context, token string) remote.Endpoint {
		if token != "configured-token" {
			t.Errorf("fallback password = %q, want the configured one", token)
		}
		return ep
	})
	cs := sessionOf(t, srv)

	reply := strings.Join(texts(call(t, cs, "start_freecad", nil)), "\n")
	if got, want := strings.Join(*seen, "|"), "/listener/start Bearer listener-token file="; got != want {
		t.Errorf("listener saw %q, want %q", got, want)
	}
	for _, want := range []string{"state: started", "pid: " + strconv.Itoa(os.Getpid()), "port: 9875"} {
		if !strings.Contains(reply, want) {
			t.Errorf("start_freecad reply lacks %q:\n%s", want, reply)
		}
	}
	if got := srv.launcher.State(); got.State != "started" || got.PID != os.Getpid() {
		t.Errorf("launcher did not adopt the listener's launch: %+v", got)
	}

	// get_rpc_status follows the adopted launch instead of calling FreeCAD not running.
	status := strings.Join(texts(call(t, cs, "get_rpc_status", nil)), "\n")
	for _, want := range []string{"freecad: starting", "freecad-gui-1.log"} {
		if !strings.Contains(status, want) {
			t.Errorf("get_rpc_status lacks %q:\n%s", want, status)
		}
	}
}

func TestStartFreeCADFromASessionWithoutADesktopRefusesWithoutAListener(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	srv := hiddenServer(t, func(context.Context, string) remote.Endpoint {
		return remote.Endpoint{Host: "127.0.0.1", Port: port}
	})

	res := call(t, sessionOf(t, srv), "start_freecad", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError {
		t.Fatalf("start_freecad was not refused:\n%s", text)
	}
	for _, want := range []string{
		"code: unavailable",
		"runs outside the user's desktop session (for example over SSH), so a FreeCAD it starts would be invisible",
		"ask the user to start FreeCAD on their desktop", "freecad-mcp share --on", "execute_code_headless works without a desktop",
	} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Errorf("refusal lacks %q:\n%s", want, text)
		}
	}
	if got := srv.launcher.State(); got.State != "" {
		t.Errorf("a refused start left launch state %+v", got)
	}
}

func TestStartFreeCADFromASessionWithoutADesktopReportsAListenerRefusal(t *testing.T) {
	ep, _ := fakeListener(t, http.StatusUnauthorized, listenerapi.Error{Code: "authentication", Error: "The password was not accepted.", Reason: listenerapi.ReasonPassword})
	srv := hiddenServer(t, func(context.Context, string) remote.Endpoint { return ep })

	res := call(t, sessionOf(t, srv), "start_freecad", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(strings.ToLower(text), "password") || strings.Contains(text, "outside the user's desktop") {
		t.Fatalf("reply = %v (isError %v)", text, res.IsError)
	}
}

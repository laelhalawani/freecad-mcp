package listener

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
)

// newTestHandler is a handler fixed to settings (Options.Override, no disk
// access), with no launcher and no proxy, enough for requests that never
// reach past the password check.
func newTestHandler(settings addoninstall.RemoteSettings) *handler {
	discard := log.New(io.Discard, "", 0)
	return &handler{
		settings: newSettingsCache(addoninstall.Target{}, &settings, discard),
		log:      discard,
		rlog:     newRateLimitedLog(discard),
		backoff:  newBackoff(),
	}
}

func passwordCheckRequest(t *testing.T, auth string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, listenerapi.PathStatus, nil)
	req.Header.Set("Content-Type", listenerapi.ContentTypeJSON)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.RemoteAddr = "127.0.0.1:54321" // loopback: always allowed, whatever the allow list
	return req
}

func decodeListenerError(t *testing.T, rec *httptest.ResponseRecorder) listenerapi.Error {
	t.Helper()
	var got listenerapi.Error
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding error body: %v (%s)", err, rec.Body.String())
	}
	return got
}

// TestMissingPasswordText covers live check N1: no Authorization header at
// all gets "A password is required.", not the wrong-password wording.
func TestMissingPasswordText(t *testing.T) {
	h := newTestHandler(addoninstall.RemoteSettings{
		RemoteEnabled: true, AllowedIPs: addoninstall.DefaultAllowedIPs, AuthToken: "secret",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, passwordCheckRequest(t, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := decodeListenerError(t, rec)
	if got.Error != "A password is required." {
		t.Fatalf("message = %q", got.Error)
	}
	if got.Reason != listenerapi.ReasonPassword {
		t.Fatalf("reason = %q, want %q", got.Reason, listenerapi.ReasonPassword)
	}
}

// TestWrongPasswordText covers live check N1: a wrong Authorization header
// gets "The password was not accepted.", telling it apart from none sent.
func TestWrongPasswordText(t *testing.T) {
	h := newTestHandler(addoninstall.RemoteSettings{
		RemoteEnabled: true, AllowedIPs: addoninstall.DefaultAllowedIPs, AuthToken: "secret",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, passwordCheckRequest(t, "Bearer wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := decodeListenerError(t, rec)
	if got.Error != "The password was not accepted." {
		t.Fatalf("message = %q", got.Error)
	}
}

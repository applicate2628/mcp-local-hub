package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPing_ReturnsJSONWithPIDAndVersion(t *testing.T) {
	s := NewServer(Config{Version: "test-v1", PID: 4242})
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		OK      bool   `json:"ok"`
		PID     int    `json:"pid"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK || body.PID != 4242 || body.Version != "test-v1" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestActivateWindow_MarksSignalReceived(t *testing.T) {
	s := NewServer(Config{})
	got := make(chan struct{}, 1)
	s.OnActivateWindow(func() error { got <- struct{}{}; return nil })
	req := httptest.NewRequest(http.MethodPost, "/api/activate-window", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	select {
	case <-got:
	default:
		t.Error("activate-window did not invoke callback")
	}
}

// TestActivateWindow_HeadlessReturns503 verifies the handler maps
// ErrActivationNoTarget to 503 Service Unavailable so the second
// invocation's TryActivateIncumbent can surface a useful message
// instead of falsely claiming activation succeeded.
func TestActivateWindow_HeadlessReturns503(t *testing.T) {
	s := NewServer(Config{})
	s.OnActivateWindow(func() error { return ErrActivationNoTarget })
	req := httptest.NewRequest(http.MethodPost, "/api/activate-window", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestActivateWindow_OtherErrorReturns500 confirms unexpected callback
// errors map to 500 (not 503 — that's reserved for the documented
// "headless" sentinel).
func TestActivateWindow_OtherErrorReturns500(t *testing.T) {
	s := NewServer(Config{})
	s.OnActivateWindow(func() error { return errPingTestSentinel })
	req := httptest.NewRequest(http.MethodPost, "/api/activate-window", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

var errPingTestSentinel = errors.New("ping test sentinel")

func TestPing_WrongMethodIs405(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/ping", strings.NewReader(""))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

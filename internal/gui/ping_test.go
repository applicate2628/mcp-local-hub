package gui

import (
	"encoding/json"
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
	s.OnActivateWindow(func() { got <- struct{}{} })
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

func TestPing_WrongMethodIs405(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/ping", strings.NewReader(""))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

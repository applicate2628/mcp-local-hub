package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeDemigrater struct {
	gotServers []string
	gotClients []string
	err        error
}

func (f *fakeDemigrater) Demigrate(servers, clients []string) error {
	f.gotServers = append([]string{}, servers...)
	f.gotClients = append([]string{}, clients...)
	return f.err
}

func TestDemigrateHandler_RejectsNonPOST(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.demigrater = &fakeDemigrater{}
	req := httptest.NewRequest(http.MethodGet, "/api/demigrate", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDemigrateHandler_ForwardsServersAndClients(t *testing.T) {
	fake := &fakeDemigrater{}
	s := NewServer(Config{Port: 0})
	s.demigrater = fake
	body := bytes.NewReader([]byte(`{"servers":["memory"],"clients":["claude-code"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", w.Code, w.Body.String())
	}
	if len(fake.gotServers) != 1 || fake.gotServers[0] != "memory" {
		t.Errorf("gotServers=%v, want [memory]", fake.gotServers)
	}
	if len(fake.gotClients) != 1 || fake.gotClients[0] != "claude-code" {
		t.Errorf("gotClients=%v, want [claude-code]", fake.gotClients)
	}
}

func TestDemigrateHandler_SurfacesDemigrateError(t *testing.T) {
	fake := &fakeDemigrater{err: errStub("boom")}
	s := NewServer(Config{Port: 0})
	s.demigrater = fake
	body := bytes.NewReader([]byte(`{"servers":["memory"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	var body2 struct{ Error, Code string }
	_ = json.Unmarshal(w.Body.Bytes(), &body2)
	if !strings.Contains(body2.Error, "boom") {
		t.Errorf("error=%q, want contains boom", body2.Error)
	}
	if body2.Code != "DEMIGRATE_FAILED" {
		t.Errorf("code=%q, want DEMIGRATE_FAILED", body2.Code)
	}
}

func TestDemigrateHandler_RejectsCrossOrigin(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.demigrater = &fakeDemigrater{}
	body := bytes.NewReader([]byte(`{"servers":["memory"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

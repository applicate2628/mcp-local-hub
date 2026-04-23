package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeDismisser struct {
	got []string
	err error
}

func (f *fakeDismisser) DismissUnknown(name string) error {
	f.got = append(f.got, name)
	return f.err
}

func (f *fakeDismisser) ListDismissedUnknown() (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, n := range f.got {
		out[n] = struct{}{}
	}
	return out, f.err
}

// POST /api/dismiss tests

func TestDismissHandler_RejectsNonPOST(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.dismisser = &fakeDismisser{}
	req := httptest.NewRequest(http.MethodGet, "/api/dismiss", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestDismissHandler_ForwardsServerName(t *testing.T) {
	fake := &fakeDismisser{}
	s := NewServer(Config{Port: 0})
	s.dismisser = fake
	body := bytes.NewReader([]byte(`{"server":"fetch"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/dismiss", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", w.Code, w.Body.String())
	}
	if len(fake.got) != 1 || fake.got[0] != "fetch" {
		t.Errorf("got=%v, want [fetch]", fake.got)
	}
}

func TestDismissHandler_RejectsEmptyServer(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.dismisser = &fakeDismisser{}
	body := bytes.NewReader([]byte(`{"server":""}`))
	req := httptest.NewRequest(http.MethodPost, "/api/dismiss", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	var body2 struct{ Code string }
	_ = json.Unmarshal(w.Body.Bytes(), &body2)
	if body2.Code != "BAD_REQUEST" {
		t.Errorf("code=%q, want BAD_REQUEST", body2.Code)
	}
}

func TestDismissHandler_SurfacesBackendError(t *testing.T) {
	fake := &fakeDismisser{err: errStub("disk full")}
	s := NewServer(Config{Port: 0})
	s.dismisser = fake
	body := bytes.NewReader([]byte(`{"server":"fetch"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/dismiss", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestDismissHandler_TrimsServerNameBeforePersist(t *testing.T) {
	// PR #4 Codex R3: the handler used to validate trim(req.Server) but
	// persist the untrimmed req.Server. A request like "  serenity  "
	// passed the empty-check yet stored a whitespace-padded key that
	// never matched a scan entry during filtering. Assert the key is
	// stored trimmed.
	fake := &fakeDismisser{}
	s := NewServer(Config{Port: 0})
	s.dismisser = fake
	body := bytes.NewReader([]byte(`{"server":"  serenity  "}`))
	req := httptest.NewRequest(http.MethodPost, "/api/dismiss", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", w.Code, w.Body.String())
	}
	if len(fake.got) != 1 || fake.got[0] != "serenity" {
		t.Errorf("got=%v, want [serenity]", fake.got)
	}
}

func TestDismissHandler_RejectsCrossOrigin(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.dismisser = &fakeDismisser{}
	body := bytes.NewReader([]byte(`{"server":"fetch"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/dismiss", body)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

// GET /api/dismissed tests

func TestDismissedHandler_RejectsNonGET(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.dismisser = &fakeDismisser{}
	req := httptest.NewRequest(http.MethodPost, "/api/dismissed", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestDismissedHandler_ReturnsUnknownList(t *testing.T) {
	fake := &fakeDismisser{got: []string{"fetch", "ripgrep-mcp"}}
	s := NewServer(Config{Port: 0})
	s.dismisser = fake
	req := httptest.NewRequest(http.MethodGet, "/api/dismissed", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Unknown []string `json:"unknown"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range body.Unknown {
		got[n] = true
	}
	if !got["fetch"] || !got["ripgrep-mcp"] {
		t.Errorf("got %v, want {fetch, ripgrep-mcp}", body.Unknown)
	}
}

func TestDismissedHandler_EmptyListReturnsEmptyArray(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.dismisser = &fakeDismisser{}
	req := httptest.NewRequest(http.MethodGet, "/api/dismissed", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"unknown":[]`) {
		t.Errorf("want empty array, got body=%s", w.Body.String())
	}
}

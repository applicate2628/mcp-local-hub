package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// newPathValidateServer builds a test server with the path-validate route
// wired. NewServer (via newTestServer) now calls registerPathValidateRoutes
// in its central route block (server.go), so the test no longer registers the
// route manually — doing so would double-register and panic.
func newPathValidateServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newTestServer(t)
	return s
}

// doPathValidate issues a same-origin GET /api/path/validate?path=<p>
// against the test server's mux and returns the recorder.
func doPathValidate(t *testing.T, s *Server, p string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/path/validate?path="+url.QueryEscape(p), nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	return rr
}

func TestPathValidate_ExistingDir(t *testing.T) {
	s := newPathValidateServer(t)
	dir := t.TempDir()
	rr := doPathValidate(t, s, dir)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var resp pathValidateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Exists {
		t.Errorf("expected exists=true for %q", dir)
	}
	if !resp.IsDir {
		t.Errorf("expected is_dir=true for %q", dir)
	}
	if resp.Error != "" {
		t.Errorf("unexpected error %q", resp.Error)
	}
	if resp.Path != dir {
		t.Errorf("path echo = %q, want %q", resp.Path, dir)
	}
}

func TestPathValidate_ExistingFileIsNotDir(t *testing.T) {
	s := newPathValidateServer(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "afile.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := doPathValidate(t, s, file)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var resp pathValidateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Exists {
		t.Errorf("expected exists=true for an existing file")
	}
	if resp.IsDir {
		t.Errorf("expected is_dir=false for a regular file")
	}
}

func TestPathValidate_NonExistent(t *testing.T) {
	s := newPathValidateServer(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist-12345")
	rr := doPathValidate(t, s, missing)
	// Non-existence is a normal validation outcome, NOT an HTTP error.
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var resp pathValidateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Exists {
		t.Errorf("expected exists=false for a missing path")
	}
	if resp.IsDir {
		t.Errorf("expected is_dir=false for a missing path")
	}
	if resp.Error != "" {
		t.Errorf("a plain not-exists must not surface an error, got %q", resp.Error)
	}
}

func TestPathValidate_EmptyPath_400(t *testing.T) {
	s := newPathValidateServer(t)
	req := httptest.NewRequest("GET", "/api/path/validate", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty path, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "PATH_REQUIRED" {
		t.Errorf("code = %v, want PATH_REQUIRED", body["code"])
	}
}

func TestPathValidate_ControlChars_400(t *testing.T) {
	s := newPathValidateServer(t)
	// Embedded newline — mirrors the TypePath registry validator rejection.
	rr := doPathValidate(t, s, "/tmp/foo\nbar")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for control chars, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "PATH_INVALID" {
		t.Errorf("code = %v, want PATH_INVALID", body["code"])
	}
}

func TestPathValidate_MethodNotAllowed(t *testing.T) {
	s := newPathValidateServer(t)
	req := httptest.NewRequest("POST", "/api/path/validate?path=/tmp", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow header = %q, want GET", got)
	}
}

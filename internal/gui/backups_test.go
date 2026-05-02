package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

type fakeBackups struct {
	list       []api.BackupInfo
	listErr    error
	preview    []string
	previewErr error
	previewN   int
	cleaned    []string
	cleanErr   error
	cleanN     int
}

func (f *fakeBackups) List() ([]api.BackupInfo, error) { return f.list, f.listErr }
func (f *fakeBackups) CleanPreview(n int) ([]string, error) {
	f.previewN = n
	return f.preview, f.previewErr
}
func (f *fakeBackups) Clean(n int) ([]string, error) {
	f.cleanN = n
	return f.cleaned, f.cleanErr
}

func newBackupsTestServer(t *testing.T) (*Server, *fakeBackups) {
	t.Helper()
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	fb := &fakeBackups{}
	s.backups = fb
	return s, fb
}

func TestBackups_GET_List(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.list = []api.BackupInfo{
		{Client: "claude-code", Path: "/x.bak", Kind: "timestamped",
			ModTime: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC), SizeByte: 1234},
	}
	req := httptest.NewRequest("GET", "/api/backups", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Backups []map[string]any `json:"backups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Backups) != 1 {
		t.Fatalf("expected 1, got %d", len(resp.Backups))
	}
	row := resp.Backups[0]
	if row["client"] != "claude-code" {
		t.Errorf("client mismatch: %v", row["client"])
	}
	if row["mod_time"] != "2026-04-01T12:00:00Z" {
		t.Errorf("mod_time RFC3339 mismatch: %v", row["mod_time"])
	}
}

func TestBackups_GET_CleanPreview(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.preview = []string{"/old.bak", "/older.bak"}
	req := httptest.NewRequest("GET", "/api/backups/clean-preview?keep_n=3", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if fb.previewN != 3 {
		t.Errorf("expected keep_n=3 forwarded, got %d", fb.previewN)
	}
	var resp struct {
		WouldRemove []string `json:"would_remove"`
	}
	json.Unmarshal(rr.Body.Bytes(), &resp) //nolint:errcheck
	if len(resp.WouldRemove) != 2 {
		t.Errorf("expected 2 paths, got %v", resp.WouldRemove)
	}
}

func TestBackups_GET_CleanPreview_MissingParam_400(t *testing.T) {
	s, _ := newBackupsTestServer(t)
	req := httptest.NewRequest("GET", "/api/backups/clean-preview", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestBackups_GET_CleanPreview_NegativeKeepN_400(t *testing.T) {
	s, _ := newBackupsTestServer(t)
	req := httptest.NewRequest("GET", "/api/backups/clean-preview?keep_n=-1", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestBackups_GET_CleanPreview_NilPathsEmittedAsEmptyArray(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.preview = nil
	req := httptest.NewRequest("GET", "/api/backups/clean-preview?keep_n=99", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), `"would_remove":[]`) {
		t.Errorf("expected empty array, got %s", rr.Body.String())
	}
}

func TestBackups_AllRoutes_RejectCrossOrigin(t *testing.T) {
	// Codex r2 P2.2: read-only routes also wrapped with requireSameOrigin.
	s, _ := newBackupsTestServer(t)
	cases := []struct{ method, path string }{
		{"GET", "/api/backups"},
		{"GET", "/api/backups/clean-preview?keep_n=5"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != 403 {
			t.Errorf("%s %s: expected 403, got %d", c.method, c.path, rr.Code)
		}
	}
}

func TestBackups_GET_List_PropagatesError(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.listErr = errors.New("disk full")
	req := httptest.NewRequest("GET", "/api/backups", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 500 {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestBackups_WrongMethod_405(t *testing.T) {
	s, _ := newBackupsTestServer(t)
	cases := []struct{ method, path string }{
		{"POST", "/api/backups"},
		{"DELETE", "/api/backups/clean-preview?keep_n=1"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: expected 405, got %d", c.method, c.path, rr.Code)
		}
		if rr.Header().Get("Allow") != "GET" {
			t.Errorf("%s %s: expected Allow:GET header", c.method, c.path)
		}
	}
}

func TestBackupsClean_POST_HappyPath(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.cleaned = []string{"a.bak", "b.bak"}
	req := httptest.NewRequest("POST", "/api/backups/clean", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Cleaned int      `json:"cleaned"`
		Errors  []string `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Cleaned != 2 {
		t.Errorf("cleaned = %d, want 2", resp.Cleaned)
	}
}

func TestBackupsClean_BadMethod(t *testing.T) {
	s, _ := newBackupsTestServer(t)
	req := httptest.NewRequest("GET", "/api/backups/clean", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestBackupsClean_StorageError_500(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.cleanErr = errors.New("disk full")
	req := httptest.NewRequest("POST", "/api/backups/clean", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "BACKUPS_CLEAN_FAILED") {
		t.Errorf("body missing error code: %s", rr.Body.String())
	}
}

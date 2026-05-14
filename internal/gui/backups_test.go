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

	// Per-client (bug-bash B2 closure #21) — captures the client arg
	// + retention so tests can assert the handler forwards them.
	cleanInClient    string
	cleanInN         int
	cleanInResult    []string
	cleanInErr       error
	previewInClient  string
	previewInN       int
	previewInResult  []string
	previewInErr     error
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
func (f *fakeBackups) CleanInClient(client string, n int) ([]string, error) {
	f.cleanInClient = client
	f.cleanInN = n
	return f.cleanInResult, f.cleanInErr
}
func (f *fakeBackups) CleanPreviewInClient(client string, n int) ([]string, error) {
	f.previewInClient = client
	f.previewInN = n
	return f.previewInResult, f.previewInErr
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

// ---------------------------------------------------------------------------
// Bug-bash B2 closure (#21): per-client backup cleanup.
//
// Pre-fix, the GUI Settings.Backups screen exposed one global "Clean now"
// button that pruned every managed client's backups. Operators who wanted
// to clean only one client (e.g., "I rebuilt cursor's config; just prune
// cursor's stale backups, leave claude-code alone") had no way to scope.
//
// /api/backups/clean and /api/backups/clean-preview now accept an
// optional ?client=X query param that narrows the prune set. Empty
// param preserves the legacy "every client" semantic.
// ---------------------------------------------------------------------------

func TestBackupsClean_POST_PerClientHappyPath(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.cleanInResult = []string{
		"/home/u/.cursor/mcp.json.bak-mcp-local-hub-2026-04-30T12-00-00",
	}
	req := httptest.NewRequest("POST", "/api/backups/clean?client=cursor", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Cleaned int      `json:"cleaned"`
		Client  string   `json:"client"`
		Errors  []string `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Cleaned != 1 {
		t.Errorf("cleaned = %d, want 1", resp.Cleaned)
	}
	if resp.Client != "cursor" {
		t.Errorf("client = %q, want cursor", resp.Client)
	}
	if fb.cleanInClient != "cursor" {
		t.Errorf("CleanInClient client = %q, want cursor", fb.cleanInClient)
	}
	// keepN is read from settings (registry default 5 when unset).
	if fb.cleanInN != 5 {
		t.Errorf("CleanInClient keepN = %d, want 5 (registry default)", fb.cleanInN)
	}
	// Bulk Clean must NOT have been called (mutual-exclusivity).
	if fb.cleanN != 0 {
		t.Errorf("bulk Clean leaked through with keepN=%d", fb.cleanN)
	}
}

func TestBackupsClean_POST_PerClientUnknownClient_400(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.cleanInErr = errors.New(`unknown client "not-a-real-client" (expected claude-code | codex-cli | cursor | ...)`)
	req := httptest.NewRequest("POST", "/api/backups/clean?client=not-a-real-client", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "BACKUPS_CLEAN_UNKNOWN_CLIENT") {
		t.Errorf("body missing error code BACKUPS_CLEAN_UNKNOWN_CLIENT: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unknown client") {
		t.Errorf("body missing wrapped error message: %s", rr.Body.String())
	}
}

func TestBackupsCleanPreview_GET_PerClientHappyPath(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.previewInResult = []string{
		"/home/u/.cursor/mcp.json.bak-mcp-local-hub-2026-04-29T12-00-00",
	}
	req := httptest.NewRequest("GET", "/api/backups/clean-preview?keep_n=3&client=cursor", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		WouldRemove []string `json:"would_remove"`
		Client      string   `json:"client"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.WouldRemove) != 1 {
		t.Errorf("would_remove = %v, want 1 row", resp.WouldRemove)
	}
	if resp.Client != "cursor" {
		t.Errorf("client = %q, want cursor", resp.Client)
	}
	if fb.previewInClient != "cursor" || fb.previewInN != 3 {
		t.Errorf("CleanPreviewInClient called with (%q, %d), want (cursor, 3)", fb.previewInClient, fb.previewInN)
	}
	if fb.previewN != 0 {
		t.Errorf("bulk CleanPreview leaked through with keepN=%d", fb.previewN)
	}
}

func TestBackupsCleanPreview_GET_PerClientUnknownClient_400(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.previewInErr = errors.New(`unknown client "not-a-real-client"`)
	req := httptest.NewRequest("GET", "/api/backups/clean-preview?keep_n=3&client=not-a-real-client", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "BACKUPS_PREVIEW_UNKNOWN_CLIENT") {
		t.Errorf("body missing error code BACKUPS_PREVIEW_UNKNOWN_CLIENT: %s", rr.Body.String())
	}
}

// TestPreviewTimestampedRetention pins the pure retention helper that
// powers CleanPreviewInClient on the production adapter. Sentinels never
// appear in the result; timestamped rows are sorted newest-first, the
// first keepN are kept, the rest are returned.
func TestPreviewTimestampedRetention(t *testing.T) {
	mkTime := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return v
	}
	rows := []api.BackupInfo{
		{Path: "/a.bak-mcp-local-hub-2026-04-30T12-00-00", Kind: "timestamped", ModTime: mkTime("2026-04-30T12:00:00Z")},
		{Path: "/a.bak-mcp-local-hub-original", Kind: "original", ModTime: mkTime("2026-01-01T00:00:00Z")},
		{Path: "/a.bak-mcp-local-hub-2026-05-01T12-00-00", Kind: "timestamped", ModTime: mkTime("2026-05-01T12:00:00Z")},
		{Path: "/a.bak-mcp-local-hub-2026-04-29T12-00-00", Kind: "timestamped", ModTime: mkTime("2026-04-29T12:00:00Z")},
	}

	t.Run("keep 1 keeps newest only", func(t *testing.T) {
		got := previewTimestampedRetention(rows, 1)
		want := []string{
			"/a.bak-mcp-local-hub-2026-04-30T12-00-00",
			"/a.bak-mcp-local-hub-2026-04-29T12-00-00",
		}
		if !equalStringSlices(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("keep 5 (more than count) prunes nothing", func(t *testing.T) {
		got := previewTimestampedRetention(rows, 5)
		if len(got) != 0 {
			t.Errorf("got %v, want empty (count <= keepN)", got)
		}
	})

	t.Run("keep 0 prunes every timestamped", func(t *testing.T) {
		got := previewTimestampedRetention(rows, 0)
		if len(got) != 3 {
			t.Errorf("got %d rows, want 3 timestamped (sentinel excluded)", len(got))
		}
		// Sentinel must not appear.
		for _, p := range got {
			if strings.HasSuffix(p, "-original") {
				t.Errorf("sentinel leaked into prune set: %s", p)
			}
		}
	})

	t.Run("empty input returns empty", func(t *testing.T) {
		got := previewTimestampedRetention(nil, 0)
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

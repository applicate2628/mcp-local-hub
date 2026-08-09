package gui

import (
	"context"
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
	cleanInClient   string
	cleanInN        int
	cleanInResult   []string
	cleanInErr      error
	previewInClient string
	previewInN      int
	previewInResult []string
	previewInErr    error
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
	s := newEphemeralServer(t, Config{Port: 9125, Version: "test", PID: 1})
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

// TestBackupsClean_EmitsAuditOnDeletion covers PR #476 bot P3:
// backupsCleanHandler deleted timestamped backups with no gui-events
// audit row. A clean that actually removes files must emit a
// backup-clean operator-action row (client + count + basenames, no
// sensitive data).
func TestBackupsClean_EmitsAuditOnDeletion(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.cleanInResult = []string{"/home/u/.cursor.json.bak-mcp-local-hub-1", "/home/u/.cursor.json.bak-mcp-local-hub-2"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest("POST", "/api/backups/clean?client=cursor&keep_n=1", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	select {
	case ev := <-ch:
		if ev.Type != "operator-action" || ev.Body["action"] != "backup-clean" {
			t.Fatalf("event = %+v, want operator-action/backup-clean", ev)
		}
		if ev.Body["client"] != "cursor" {
			t.Errorf("client=%v, want cursor", ev.Body["client"])
		}
		// Subscribe delivers the live Event struct (no JSON round-trip),
		// so cleaned_count stays the original Go int from len(removed).
		if cnt, _ := ev.Body["cleaned_count"].(int); cnt != 2 {
			t.Errorf("cleaned_count=%v, want 2", ev.Body["cleaned_count"])
		}
		// Deleted entries must be BASENAMES, not full absolute paths.
		raw, _ := json.Marshal(ev.Body)
		if strings.Contains(string(raw), "/home/u/") {
			t.Errorf("audit row leaked full absolute path (should be basename only): %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no backup-clean audit event after a clean that deleted files (PR #476 P3)")
	}
}

// TestBackupsClean_NoAuditWhenNothingDeleted guards the inverse: a clean
// that pruned nothing must NOT emit an audit row (no mutation occurred).
func TestBackupsClean_NoAuditWhenNothingDeleted(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.cleaned = []string{} // nothing eligible to prune

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest("POST", "/api/backups/clean", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	select {
	case ev := <-ch:
		t.Fatalf("unexpected audit event on a no-op clean: %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected: no event
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
	// Isolate api.SettingsPath() to an empty temp dir so the no-keep_n-query
	// path reads the registry default (5), never the developer's live
	// %LOCALAPPDATA%\mcp-local-hub\gui-preferences.yaml (which persists
	// backups.keep_n=2). Same LOCALAPPDATA/XDG_DATA_HOME seam used by
	// setupGateOverrides (hub_listener_test.go) and backup_keep_test.go.
	// NOTE: MCPHUB_STATE_DIR_OVERRIDE (used by the sibling keep_n-query tests)
	// is a NO-OP for SettingsPath — it only redirects the daemon state dir.
	settingsRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", settingsRoot)
	t.Setenv("XDG_DATA_HOME", settingsRoot)
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

// ---------------------------------------------------------------------------
// Bug #2 (2026-06-15): "очистка бэкапов не работает". The GUI preview used the
// live slider DRAFT keep_n, but the clean POST sent no keep_n so the handler
// fell back to the PERSISTED setting. Dragging the slider down without Save
// showed "Clean X only (3)" but the clean then pruned nothing (persisted=5).
// Fix: an explicit ?keep_n=N query overrides the persisted setting so the
// clean is WYSIWYG with the preview. Absent → persisted; invalid → 400.
// ---------------------------------------------------------------------------

func TestBackupsClean_POST_KeepNQueryOverride_Bulk(t *testing.T) {
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())
	s, fb := newBackupsTestServer(t)
	fb.cleaned = []string{"x.bak"}
	// keep_n=2 in the query must reach the bulk Clean as 2, NOT the persisted
	// registry-default 5.
	req := httptest.NewRequest("POST", "/api/backups/clean?keep_n=2", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if fb.cleanN != 2 {
		t.Errorf("bulk Clean keepN = %d, want 2 (query override, not persisted)", fb.cleanN)
	}
}

func TestBackupsClean_POST_KeepNQueryOverride_PerClient(t *testing.T) {
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())
	s, fb := newBackupsTestServer(t)
	fb.cleanInResult = []string{"y.bak"}
	req := httptest.NewRequest("POST", "/api/backups/clean?client=cursor&keep_n=1", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if fb.cleanInClient != "cursor" {
		t.Errorf("CleanInClient client = %q, want cursor", fb.cleanInClient)
	}
	if fb.cleanInN != 1 {
		t.Errorf("CleanInClient keepN = %d, want 1 (query override)", fb.cleanInN)
	}
}

func TestBackupsClean_POST_KeepNQueryInvalid_400(t *testing.T) {
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())
	for _, q := range []string{"keep_n=-1", "keep_n=abc"} {
		s, fb := newBackupsTestServer(t)
		req := httptest.NewRequest("POST", "/api/backups/clean?"+q, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "BACKUPS_CLEAN_BAD_PARAM") {
			t.Errorf("%s: body missing error code: %s", q, rr.Body.String())
		}
		// Invalid keep_n must NEVER fall through to a destructive default clean.
		if fb.cleanN != 0 || fb.cleanInN != 0 {
			t.Errorf("%s: clean fired despite invalid keep_n (cleanN=%d cleanInN=%d)", q, fb.cleanN, fb.cleanInN)
		}
	}
}

func TestBackupsClean_POST_PerClientUnknownClient_400(t *testing.T) {
	s, _ := newBackupsTestServer(t)
	// Bot r1 P2 closure (PR #183): unknown-client validation happens
	// up-front via clients.ConfigPathForName BEFORE the cleaner runs,
	// so the handler doesn't need the fake to return an error — the
	// validation gate produces the 400. (fakeBackups.cleanInErr is
	// reserved for the I/O-failure case verified below.)
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

// TestBackupsClean_POST_PerClientIOFailure_500 pins bot r1 P2 closure
// on PR #183: a valid client id that hits a runtime/filesystem failure
// (e.g., backup directory unreadable, permission denied, disk error)
// must return 500 BACKUPS_CLEAN_FAILED, NOT 400 BACKUPS_CLEAN_UNKNOWN_
// CLIENT. Pre-fix, the handler mapped EVERY CleanInClient error to
// 400, which would have given operators a misleading "unknown client"
// diagnosis for an infrastructure failure.
func TestBackupsClean_POST_PerClientIOFailure_500(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.cleanInErr = errors.New("read .cursor/: permission denied")
	// Use a real client id so the up-front validation passes — the
	// 500 only fires when validation passes AND the fake reports an
	// I/O failure.
	req := httptest.NewRequest("POST", "/api/backups/clean?client=cursor", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "BACKUPS_CLEAN_FAILED") {
		t.Errorf("body missing error code BACKUPS_CLEAN_FAILED: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "permission denied") {
		t.Errorf("body should preserve underlying I/O message: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "UNKNOWN_CLIENT") {
		t.Errorf("body must NOT misclassify I/O failure as unknown-client: %s", rr.Body.String())
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
	s, _ := newBackupsTestServer(t)
	// Up-front validation gate produces the 400 BEFORE the fake's
	// preview is called.
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

// TestBackupsCleanPreview_GET_PerClientIOFailure_500 pins bot r1 P2
// closure on PR #183 symmetry with the clean handler: a valid client
// id that hits a runtime/filesystem failure during preview (e.g.,
// BackupsListIn's os.ReadDir errors out) must return 500
// BACKUPS_PREVIEW_FAILED, NOT 400 BACKUPS_PREVIEW_UNKNOWN_CLIENT.
func TestBackupsCleanPreview_GET_PerClientIOFailure_500(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.previewInErr = errors.New("read .cursor/: permission denied")
	req := httptest.NewRequest("GET", "/api/backups/clean-preview?keep_n=3&client=cursor", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "BACKUPS_PREVIEW_FAILED") {
		t.Errorf("body missing error code BACKUPS_PREVIEW_FAILED: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "UNKNOWN_CLIENT") {
		t.Errorf("body must NOT misclassify I/O failure as unknown-client: %s", rr.Body.String())
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

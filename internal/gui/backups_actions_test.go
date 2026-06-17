package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBackupActions records the args it was called with and returns
// canned results, so the handler tests never touch a real client config
// or the filesystem beyond the validation gate's own Lstat.
type fakeBackupActions struct {
	restoreClient string
	restorePath   string
	restoreSnap   string
	restoreErr    error

	deletePath string
	deleteErr  error
}

func (f *fakeBackupActions) Restore(client, backupPath string) (string, error) {
	f.restoreClient = client
	f.restorePath = backupPath
	return f.restoreSnap, f.restoreErr
}

func (f *fakeBackupActions) Delete(backupPath string) error {
	f.deletePath = backupPath
	return f.deleteErr
}

// seedHomeWithClaudeBackup redirects HOME/USERPROFILE to a temp dir and
// writes one real claude-code backup file there, returning the temp home
// and the absolute backup path. claude-code's config is $HOME/.claude.json,
// so its backups live in $HOME and start with `.claude.json.bak-mcp-local-hub-`.
func seedHomeWithClaudeBackup(t *testing.T) (home, backupPath string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	backupPath = filepath.Join(home, ".claude.json.bak-mcp-local-hub-20260101-000000")
	if err := os.WriteFile(backupPath, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	return home, backupPath
}

func newBackupActionsTestServer(t *testing.T) (*Server, *fakeBackupActions) {
	t.Helper()
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	fa := &fakeBackupActions{}
	s.backupActions = fa
	return s, fa
}

func postBackupAction(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestBackupsRestore_HappyPath(t *testing.T) {
	_, backupPath := seedHomeWithClaudeBackup(t)
	s, fa := newBackupActionsTestServer(t)
	fa.restoreSnap = filepath.Join(filepath.Dir(backupPath), ".claude.json.bak-mcp-local-hub-snap")

	body, _ := json.Marshal(map[string]string{"client": "claude-code", "path": backupPath})
	rec := postBackupAction(t, s, "/api/backups/restore", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fa.restoreClient != "claude-code" || filepath.Clean(fa.restorePath) != filepath.Clean(backupPath) {
		t.Fatalf("forwarded client=%q path=%q", fa.restoreClient, fa.restorePath)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["snapshot"] != fa.restoreSnap {
		t.Errorf("snapshot in response = %v, want %q", got["snapshot"], fa.restoreSnap)
	}
}

func TestBackupsDelete_HappyPath(t *testing.T) {
	_, backupPath := seedHomeWithClaudeBackup(t)
	s, fa := newBackupActionsTestServer(t)

	body, _ := json.Marshal(map[string]string{"client": "claude-code", "path": backupPath})
	rec := postBackupAction(t, s, "/api/backups/delete", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if filepath.Clean(fa.deletePath) != filepath.Clean(backupPath) {
		t.Fatalf("forwarded delete path=%q", fa.deletePath)
	}
}

// TestBackupsDelete_RejectOriginalSentinel proves the pristine one-shot
// `-original` backup (the pre-hub clean-slate capture) cannot be deleted via
// the API, and the adapter is never invoked. (PR #360.)
func TestBackupsDelete_RejectOriginalSentinel(t *testing.T) {
	home, _ := seedHomeWithClaudeBackup(t)
	s, fa := newBackupActionsTestServer(t)

	original := filepath.Join(home, ".claude.json.bak-mcp-local-hub-original")
	if err := os.WriteFile(original, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatalf("seed original sentinel: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"client": "claude-code", "path": original})
	rec := postBackupAction(t, s, "/api/backups/delete", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "BACKUPS_DELETE_ORIGINAL_FORBIDDEN") {
		t.Fatalf("body = %s, want BACKUPS_DELETE_ORIGINAL_FORBIDDEN", rec.Body.String())
	}
	if fa.deletePath != "" {
		t.Fatalf("adapter must not be called on original sentinel; got %q", fa.deletePath)
	}
}

func TestBackupsActions_RejectUnknownClient(t *testing.T) {
	_, backupPath := seedHomeWithClaudeBackup(t)
	s, fa := newBackupActionsTestServer(t)

	body, _ := json.Marshal(map[string]string{"client": "not-a-client", "path": backupPath})
	rec := postBackupAction(t, s, "/api/backups/restore", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if fa.restorePath != "" {
		t.Fatalf("adapter must not be called on unknown client; got %q", fa.restorePath)
	}
}

// TestBackupsActions_RejectTraversal proves a crafted ../ path that names
// the real backup-naming convention but resolves OUTSIDE the client's
// config dir is refused, and the adapter is never invoked.
func TestBackupsActions_RejectTraversal(t *testing.T) {
	home, _ := seedHomeWithClaudeBackup(t)
	s, fa := newBackupActionsTestServer(t)

	// A sibling-directory escape: $HOME/../evil/.claude.json.bak-mcp-local-hub-x
	evil := filepath.Join(home, "..", "evil", ".claude.json.bak-mcp-local-hub-x")
	for _, route := range []string{"/api/backups/restore", "/api/backups/delete"} {
		body, _ := json.Marshal(map[string]string{"client": "claude-code", "path": evil})
		rec := postBackupAction(t, s, route, string(body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400; body=%s", route, rec.Code, rec.Body.String())
		}
	}
	if fa.restorePath != "" || fa.deletePath != "" {
		t.Fatalf("adapter must not be called on traversal; restore=%q delete=%q", fa.restorePath, fa.deletePath)
	}
}

// TestBackupsActions_RejectNonBackupSibling proves a path inside the right
// dir but NOT matching the backup-naming prefix (e.g. the live config
// itself) is refused — you cannot delete/overwrite the live config.
func TestBackupsActions_RejectNonBackupSibling(t *testing.T) {
	home, _ := seedHomeWithClaudeBackup(t)
	s, fa := newBackupActionsTestServer(t)

	live := filepath.Join(home, ".claude.json") // the live config, not a backup
	if err := os.WriteFile(live, []byte(`{}`), 0600); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"client": "claude-code", "path": live})
	rec := postBackupAction(t, s, "/api/backups/delete", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if fa.deletePath != "" {
		t.Fatalf("adapter must not be called on live-config target; got %q", fa.deletePath)
	}
}

func TestBackupsActions_RejectMissingFile(t *testing.T) {
	home, _ := seedHomeWithClaudeBackup(t)
	s, _ := newBackupActionsTestServer(t)

	// Correctly-named + right dir, but no such file on disk.
	missing := filepath.Join(home, ".claude.json.bak-mcp-local-hub-99999999-000000")
	body, _ := json.Marshal(map[string]string{"client": "claude-code", "path": missing})
	rec := postBackupAction(t, s, "/api/backups/restore", string(body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestBackupsActions_RejectSymlink proves a symlink planted at a
// validly-named backup path is refused (Lstat, not Stat). POSIX-only:
// non-privileged symlink creation is unreliable on Windows CI.
func TestBackupsActions_RejectSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	home, _ := seedHomeWithClaudeBackup(t)
	s, fa := newBackupActionsTestServer(t)

	target := filepath.Join(home, "secret.txt")
	if err := os.WriteFile(target, []byte("secret"), 0600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(home, ".claude.json.bak-mcp-local-hub-symlink")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"client": "claude-code", "path": link})
	rec := postBackupAction(t, s, "/api/backups/restore", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if fa.restorePath != "" {
		t.Fatalf("adapter must not be called on symlink target; got %q", fa.restorePath)
	}
}

func TestBackupsActions_RejectGET(t *testing.T) {
	s, _ := newBackupActionsTestServer(t)
	for _, route := range []string{"/api/backups/restore", "/api/backups/delete"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s GET status = %d, want 405", route, rec.Code)
		}
	}
}

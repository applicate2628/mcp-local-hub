package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// seedMembershipRegistry writes a workspaces.yaml into the given directory
// with one entry for (k1, python) so handler tests that exercise the happy
// path have a valid (workspace_key, language) pair to toggle.
func seedMembershipRegistry(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "mcp-local-hub", "workspaces.yaml")
	reg := api.NewRegistry(path)
	reg.Workspaces = []api.WorkspaceEntry{
		{WorkspaceKey: "k1", Language: "python", TaskName: "tA", Port: 9100, WeeklyRefresh: true, Backend: "mcp-language-server"},
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("seedMembershipRegistry Save: %v", err)
	}
}

// newMembershipTestServer builds a Server and redirects DefaultRegistryPath
// to a temp dir. The returned cleanup dir can be seeded by the caller.
// Returns the server and the temp dir path so callers can seed before use.
func newMembershipTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_STATE_HOME", tmp)
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	s.port.Store(9125)
	return s, tmp
}

func TestMembershipHandler_HappyPath(t *testing.T) {
	srv, tmp := newMembershipTestServer(t)
	seedMembershipRegistry(t, tmp)

	body, _ := json.Marshal([]map[string]any{
		{"workspace_key": "k1", "language": "python", "enabled": false},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-refresh-membership", bytes.NewReader(body))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMembershipHandler_UnknownPair_400(t *testing.T) {
	srv, tmp := newMembershipTestServer(t)
	seedMembershipRegistry(t, tmp)

	body, _ := json.Marshal([]map[string]any{
		{"workspace_key": "kX", "language": "ruby", "enabled": true},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-refresh-membership", bytes.NewReader(body))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMembershipHandler_BadMethod(t *testing.T) {
	srv, _ := newMembershipTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/daemons/weekly-refresh-membership", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestMembershipHandler_BadJSON_400(t *testing.T) {
	srv, _ := newMembershipTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-refresh-membership",
		strings.NewReader("not json"))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

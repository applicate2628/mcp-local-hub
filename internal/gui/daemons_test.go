package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// newDaemonsTestServer is an alias for newMembershipTestServer used by
// Task 7's weekly-schedule handler tests. Same Server scaffold (NewServer
// + redirected LOCALAPPDATA/XDG_STATE_HOME), no extra setup. The
// sameOriginHeaders helper used by these tests lives in settings_test.go.
func newDaemonsTestServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := newMembershipTestServer(t)
	return srv
}

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
	// Task 11: GET is now a supported method (snapshot endpoint), so the
	// bad-method test must use a method that ISN'T in the allow list.
	// DELETE is a safe choice — neither verb is wired.
	srv, _ := newMembershipTestServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/daemons/weekly-refresh-membership", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, PUT" {
		t.Errorf("Allow header = %q, want %q", got, "GET, PUT")
	}
}

// TestMembershipSnapshotHandler_GET exercises the new GET handler that
// feeds the SectionDaemons WeeklyMembershipTable on mount (Task 11). The
// snapshot must return rows in registry order with the same field set the
// frontend's MembershipRow type expects (workspace_key, workspace_path,
// language, weekly_refresh).
func TestMembershipSnapshotHandler_GET(t *testing.T) {
	srv, tmp := newMembershipTestServer(t)
	// Seed registry with three rows spanning two workspaces / three
	// languages / mixed enrollment. Tests both registry order (k1.python
	// before k1.rust before k2.go) and the boolean is faithfully wired.
	regPath := filepath.Join(tmp, "mcp-local-hub", "workspaces.yaml")
	reg := api.NewRegistry(regPath)
	reg.Workspaces = []api.WorkspaceEntry{
		{WorkspaceKey: "k1", WorkspacePath: "D:/p1", Language: "python", Port: 9100, WeeklyRefresh: true, Backend: "mcp-language-server"},
		{WorkspaceKey: "k1", WorkspacePath: "D:/p1", Language: "rust", Port: 9101, WeeklyRefresh: false, Backend: "mcp-language-server"},
		{WorkspaceKey: "k2", WorkspacePath: "/p2", Language: "go", Port: 9102, WeeklyRefresh: true, Backend: "mcp-language-server"},
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/daemons/weekly-refresh-membership", nil)
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Rows []struct {
			WorkspaceKey  string `json:"workspace_key"`
			WorkspacePath string `json:"workspace_path"`
			Language      string `json:"language"`
			WeeklyRefresh bool   `json:"weekly_refresh"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(resp.Rows))
	}
	// Registry order — the frontend trusts the server's ordering.
	if resp.Rows[0].WorkspaceKey != "k1" || resp.Rows[0].Language != "python" {
		t.Errorf("row[0] = %+v, want k1/python", resp.Rows[0])
	}
	if resp.Rows[2].WorkspaceKey != "k2" || resp.Rows[2].Language != "go" {
		t.Errorf("row[2] = %+v, want k2/go", resp.Rows[2])
	}
	// Boolean flag is round-tripped faithfully.
	if !resp.Rows[0].WeeklyRefresh {
		t.Errorf("row[0].weekly_refresh = false, want true")
	}
	if resp.Rows[1].WeeklyRefresh {
		t.Errorf("row[1].weekly_refresh = true, want false")
	}
}

// TestMembershipSnapshotHandler_GET_EmptyRegistry covers the empty-state
// fallthrough: a missing or freshly-initialized registry must yield 200
// with an empty rows array (NOT 404, NOT a null payload). The frontend
// treats {"rows": []} as the trigger for the "No workspaces registered"
// empty state copy (memo D6).
func TestMembershipSnapshotHandler_GET_EmptyRegistry(t *testing.T) {
	srv, _ := newMembershipTestServer(t)
	// No seed — DefaultRegistryPath points at a non-existent file under
	// tmpdir, which Registry.Load tolerates as the empty-registry case.
	req := httptest.NewRequest(http.MethodGet, "/api/daemons/weekly-refresh-membership", nil)
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Rows []membershipRowDTO `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Rows) != 0 {
		t.Errorf("rows = %d, want 0 on empty registry", len(resp.Rows))
	}
	if resp.Rows == nil {
		t.Error("rows must be [] (not null) on empty registry — frontend uses array length")
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

// --- Task 7: PUT /api/daemons/weekly-schedule (memo D8) ---

func TestWeeklyScheduleHandler_ParseError_400_NoUpdatedField(t *testing.T) {
	// Memo D8: 400 carries only {error, detail, example}; NO updated, NO restore_status.
	srv := newDaemonsTestServer(t)
	body := `{"schedule": "daily 03:00"}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "parse_error" {
		t.Errorf("error = %v, want parse_error", resp["error"])
	}
	if _, has := resp["updated"]; has {
		t.Error("400 parse-error must NOT include 'updated' (memo D8)")
	}
	if _, has := resp["restore_status"]; has {
		t.Error("400 parse-error must NOT include 'restore_status' (memo D8)")
	}
	if resp["example"] != "weekly Sun 03:00" {
		t.Errorf("example = %v, want canonical 'weekly Sun 03:00'", resp["example"])
	}
}

func TestWeeklyScheduleHandler_ValidPayload_Accepted(t *testing.T) {
	srv := newDaemonsTestServer(t)
	srv.swapForRoute = func(spec *api.ScheduleSpec, priorXML []byte) (string, error) {
		return "n/a", nil
	}
	srv.exportXMLForRoute = func(name string) ([]byte, error) { return nil, nil }
	body := `{"schedule": "weekly Tue 14:30"}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["restore_status"] != "n/a" {
		t.Errorf("restore_status = %v, want n/a", resp["restore_status"])
	}
}

func TestWeeklyScheduleHandler_ExportXMLFails_Preflight500(t *testing.T) {
	srv := newDaemonsTestServer(t)
	srv.exportXMLForRoute = func(name string) ([]byte, error) {
		return nil, errors.New("scheduler down")
	}
	body := `{"schedule": "weekly Sun 03:00"}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] != "snapshot_unavailable" {
		t.Errorf("error = %v, want snapshot_unavailable", resp["error"])
	}
}

func TestWeeklyScheduleHandler_SwapFails_RollbackOK(t *testing.T) {
	srv := newDaemonsTestServer(t)
	srv.exportXMLForRoute = func(name string) ([]byte, error) { return []byte("<Task/>"), nil }
	srv.swapForRoute = func(spec *api.ScheduleSpec, priorXML []byte) (string, error) {
		return "ok", errors.New("create boom")
	}
	body := `{"schedule": "weekly Sun 03:00"}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["restore_status"] != "ok" {
		t.Errorf("restore_status = %v, want ok", resp["restore_status"])
	}
	if _, has := resp["manual_recovery"]; has {
		t.Error("manual_recovery must NOT be present when restore_status==ok")
	}
}

func TestWeeklyScheduleHandler_NormalizesInput(t *testing.T) {
	srv := newDaemonsTestServer(t)
	srv.swapForRoute = func(spec *api.ScheduleSpec, priorXML []byte) (string, error) { return "n/a", nil }
	srv.exportXMLForRoute = func(name string) ([]byte, error) { return nil, nil }
	body := `{"schedule": "  weekly mon 14:30  "}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["schedule"] != "weekly Mon 14:30" {
		t.Errorf("schedule = %v, want canonical 'weekly Mon 14:30'", resp["schedule"])
	}
	// Verify persisted value is canonical, not raw input.
	persisted, _ := api.NewAPI().SettingsGet("daemons.weekly_schedule")
	if persisted != "weekly Mon 14:30" {
		t.Errorf("persisted = %q, want canonical", persisted)
	}
}

func TestWeeklyScheduleHandler_SwapFails_DegradedRestore(t *testing.T) {
	srv := newDaemonsTestServer(t)
	srv.exportXMLForRoute = func(name string) ([]byte, error) { return []byte("<Task/>"), nil }
	srv.swapForRoute = func(spec *api.ScheduleSpec, priorXML []byte) (string, error) {
		return "degraded", errors.New("create + import boom")
	}
	body := `{"schedule": "weekly Sun 03:00"}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["restore_status"] != "degraded" {
		t.Errorf("restore_status = %v, want degraded", resp["restore_status"])
	}
	if _, has := resp["manual_recovery"]; !has {
		t.Error("manual_recovery must be present when restore_status==degraded")
	}
}

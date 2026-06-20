package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// ---- DELETE /api/install/:server (uninstall) ----
//
// These tests inject a FAKE uninstaller so the DESTRUCTIVE api.Uninstall is
// never reached against the live fleet. Each success-path test asserts the
// fake recorded the exact server arg the URL carried.

type fakeUninstaller struct {
	seenServer string
	called     bool
	report     *api.UninstallReport
	err        error
}

func (f *fakeUninstaller) Uninstall(server string) (*api.UninstallReport, error) {
	f.called = true
	f.seenServer = server
	return f.report, f.err
}

func newUninstallTestServer(u *fakeUninstaller) *Server {
	s := &Server{mux: http.NewServeMux(), uninstaller: u, installBulk: &fakeInstallBulk{}}
	registerInstallRoutes(s)
	return s
}

func newInstallTestServer(installer *fakeInstaller) *Server {
	s := &Server{mux: http.NewServeMux(), installer: installer, uninstaller: &fakeUninstaller{}, installBulk: &fakeInstallBulk{}}
	registerInstallRoutes(s)
	return s
}

// deleteReq builds a same-origin DELETE request through the mux.
func deleteReq(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestInstallHandler_OversizedBodyRejected(t *testing.T) {
	inst := &fakeInstaller{}
	s := newInstallTestServer(inst)
	body := `{"name":"` + strings.Repeat("A", int(maxControlBodyBytes)+1) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/install", strings.NewReader(body))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%q", rec.Code, rec.Body.String())
	}
	if inst.called {
		t.Fatalf("installer called with %q; oversized body must be rejected before install", inst.seenName)
	}
}

func TestUninstallHandler_RejectsNonDELETE(t *testing.T) {
	u := &fakeUninstaller{}
	s := newUninstallTestServer(u)
	req := httptest.NewRequest(http.MethodGet, "/api/install/demo", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "DELETE" {
		t.Errorf("Allow header = %q, want DELETE", got)
	}
	if u.called {
		t.Error("uninstaller must NOT be called on a method-rejected request")
	}
}

func TestUninstallHandler_RejectsCrossOrigin(t *testing.T) {
	u := &fakeUninstaller{}
	s := newUninstallTestServer(u)
	req := httptest.NewRequest(http.MethodDelete, "/api/install/demo", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if u.called {
		t.Error("uninstaller must NOT be called on a CSRF-rejected request")
	}
}

func TestUninstallHandler_EmptyServer_400(t *testing.T) {
	u := &fakeUninstaller{}
	s := newUninstallTestServer(u)
	rec := deleteReq(t, s, "/api/install/")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if u.called {
		t.Error("uninstaller must NOT be called when no server name is given")
	}
}

func TestUninstallHandler_CleanReport_200AndForwardsServer(t *testing.T) {
	u := &fakeUninstaller{report: &api.UninstallReport{
		Server:         "demo",
		TasksDeleted:   []string{"\\mcp-local-hub-demo-default"},
		ClientsUpdated: []string{"claude"},
	}}
	s := newUninstallTestServer(u)
	rec := deleteReq(t, s, "/api/install/demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	if !u.called || u.seenServer != "demo" {
		t.Fatalf("uninstaller saw server=%q called=%v, want demo/true", u.seenServer, u.called)
	}
	var body struct {
		UninstallResults uninstallResultDTO `json:"uninstall_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.UninstallResults.Server != "demo" {
		t.Errorf("report.server = %q, want demo", body.UninstallResults.Server)
	}
	if len(body.UninstallResults.TasksDeleted) != 1 || body.UninstallResults.TasksDeleted[0] != "\\mcp-local-hub-demo-default" {
		t.Errorf("tasks_deleted = %v", body.UninstallResults.TasksDeleted)
	}
	// nil slices in the report must serialize as [] not null.
	if !strings.Contains(rec.Body.String(), `"task_delete_warns":[]`) {
		t.Errorf("nil report slice should serialize as []: %q", rec.Body.String())
	}
}

func TestUninstallHandler_ReportWithWarns_207(t *testing.T) {
	u := &fakeUninstaller{report: &api.UninstallReport{
		Server:      "demo",
		ClientWarns: []string{"refusing to remove demo from cursor: not hub-managed"},
	}}
	s := newUninstallTestServer(u)
	rec := deleteReq(t, s, "/api/install/demo")
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", rec.Code)
	}
	var body struct {
		UninstallResults uninstallResultDTO `json:"uninstall_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.UninstallResults.ClientWarns) != 1 {
		t.Errorf("client_warns = %v", body.UninstallResults.ClientWarns)
	}
}

func TestUninstallHandler_BackendError_500Envelope(t *testing.T) {
	u := &fakeUninstaller{err: errors.New("load manifest demo: not found")}
	s := newUninstallTestServer(u)
	rec := deleteReq(t, s, "/api/install/demo")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "UNINSTALL_FAILED") {
		t.Errorf("body=%q missing UNINSTALL_FAILED code", body)
	}
	// writeAPIError envelope: {"error":...,"code":...}
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, `"code"`) {
		t.Errorf("body=%q not a writeAPIError envelope", body)
	}
}

// ---- POST /api/install-all ----
//
// These tests inject a FAKE installBulkAPI so the DESTRUCTIVE api.InstallAll
// is never reached against the live fleet. The success-path tests assert the
// fake recorded the servers filter parsed from ?servers=.

type fakeInstallBulk struct {
	seenServers []string
	called      bool
	results     []api.InstallResult
}

func (f *fakeInstallBulk) InstallAll(servers []string) []api.InstallResult {
	f.called = true
	f.seenServers = servers
	return f.results
}

func newInstallAllTestServer(b *fakeInstallBulk) *Server {
	s := &Server{mux: http.NewServeMux(), installBulk: b, uninstaller: &fakeUninstaller{}}
	registerInstallRoutes(s)
	return s
}

func postNoBody(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestInstallAllHandler_RejectsNonPOST(t *testing.T) {
	b := &fakeInstallBulk{}
	s := newInstallAllTestServer(b)
	req := httptest.NewRequest(http.MethodGet, "/api/install-all", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow header = %q, want POST", got)
	}
	if b.called {
		t.Error("installBulk must NOT be called on a method-rejected request")
	}
}

func TestInstallAllHandler_RejectsCrossOrigin(t *testing.T) {
	b := &fakeInstallBulk{}
	s := newInstallAllTestServer(b)
	req := httptest.NewRequest(http.MethodPost, "/api/install-all", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if b.called {
		t.Error("installBulk must NOT be called on a CSRF-rejected request")
	}
}

func TestInstallAllHandler_AllSucceed_200(t *testing.T) {
	b := &fakeInstallBulk{results: []api.InstallResult{
		{Server: "memory", Err: nil},
		{Server: "time", Err: nil},
	}}
	s := newInstallAllTestServer(b)
	rec := postNoBody(t, s, "/api/install-all")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	if !b.called {
		t.Fatal("installBulk was not called")
	}
	if b.seenServers != nil {
		t.Errorf("no ?servers filter should forward nil, got %v", b.seenServers)
	}
	var body struct {
		InstallResults []installResultDTO `json:"install_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.InstallResults) != 2 {
		t.Fatalf("install_results len = %d, want 2", len(body.InstallResults))
	}
	if body.InstallResults[0].Server != "memory" || body.InstallResults[0].Error != "" {
		t.Errorf("row[0] = %+v", body.InstallResults[0])
	}
}

func TestInstallAllHandler_PartialFailure_207(t *testing.T) {
	b := &fakeInstallBulk{results: []api.InstallResult{
		{Server: "memory", Err: nil},
		{Server: "time", Err: errors.New("port 9131 already in use")},
	}}
	s := newInstallAllTestServer(b)
	rec := postNoBody(t, s, "/api/install-all")
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207, body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		InstallResults []installResultDTO `json:"install_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.InstallResults[1].Error != "port 9131 already in use" {
		t.Errorf("row[1].error = %q, want the port error", body.InstallResults[1].Error)
	}
}

func TestInstallAllHandler_ServersFilter_ForwardedToBackend(t *testing.T) {
	b := &fakeInstallBulk{results: []api.InstallResult{{Server: "memory"}, {Server: "time"}}}
	s := newInstallAllTestServer(b)
	rec := postNoBody(t, s, "/api/install-all?servers=memory,%20time%20,,")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	// "memory, time ,," → trimmed, empties dropped → ["memory","time"].
	if len(b.seenServers) != 2 || b.seenServers[0] != "memory" || b.seenServers[1] != "time" {
		t.Errorf("backend saw servers=%v, want [memory time]", b.seenServers)
	}
}

func TestInstallAllHandler_EmptyResults_200EmptyArray(t *testing.T) {
	b := &fakeInstallBulk{results: nil}
	s := newInstallAllTestServer(b)
	rec := postNoBody(t, s, "/api/install-all")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"install_results":[]`) {
		t.Errorf("empty results must serialize as []: %q", rec.Body.String())
	}
}

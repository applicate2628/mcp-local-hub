// internal/gui/health_test.go
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// fakeHealth is the test seam for healthBackend. Captures the most
// recent opts so handler tests can assert query-parameter parsing.
//
// Phase 6 (G2): adds the DaemonStatusSnapshot path used by the
// re-sourced /api/status handler. Counts are tracked separately
// (calls vs. daemonStatusCalls) so tests can assert which surface
// the handler hit.
type fakeHealth struct {
	calls      int
	lastOpts   api.HealthOpts
	returnSnap api.HealthSnapshot
	returnErr  error

	daemonStatusCalls    int
	returnDaemonStatuses []api.DaemonStatus
	returnDaemonErr      error
}

func (f *fakeHealth) HealthSnapshot(opts api.HealthOpts) (api.HealthSnapshot, error) {
	f.calls++
	f.lastOpts = opts
	return f.returnSnap, f.returnErr
}

func (f *fakeHealth) DaemonStatusSnapshot() ([]api.DaemonStatus, error) {
	f.daemonStatusCalls++
	return f.returnDaemonStatuses, f.returnDaemonErr
}

// healthSentinelErr is a small implementation of error suitable for
// the 500-on-backend-error test. Defined inline so the test file
// stays self-contained.
type healthSentinelErr string

func (e healthSentinelErr) Error() string { return string(e) }

const errSentinel healthSentinelErr = "backend exploded"

// newHealthTestServer builds a Server with a fake healthBackend
// installed. It reuses the package's existing newTestServer helper
// (settings_test.go) so the same-origin gate, port, and other
// dependencies match what production wiring exercises.
func newHealthTestServer(t *testing.T, fake *fakeHealth) *Server {
	t.Helper()
	s, _ := newTestServer(t)
	s.health = fake
	return s
}

func TestHealthHandler_GETOnly_405OnPOST(t *testing.T) {
	fake := &fakeHealth{returnSnap: api.HealthSnapshot{SchemaVersion: "1"}}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/health", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHealthHandler_DefaultBody(t *testing.T) {
	fake := &fakeHealth{returnSnap: api.HealthSnapshot{
		SchemaVersion: "1",
		Hub:           api.HubSection{Version: "0.7.0"},
		Daemons:       api.DaemonsSection{Items: []api.DaemonRow{}, Errors: []api.SectionError{}},
	}}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got api.HealthSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SchemaVersion != "1" {
		t.Errorf("schema_version = %q, want \"1\"", got.SchemaVersion)
	}
	if got.Probes != nil || got.Capabilities != nil {
		t.Errorf("default body should omit probes/capabilities: %+v", got)
	}
	if fake.lastOpts.IncludeProbes || fake.lastOpts.IncludeCapabilities {
		t.Errorf("default opts must not request expensive sections: %+v", fake.lastOpts)
	}
}

func TestHealthHandler_IncludeProbes(t *testing.T) {
	fake := &fakeHealth{}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/health?include=probes", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if !fake.lastOpts.IncludeProbes || fake.lastOpts.IncludeCapabilities {
		t.Errorf("opts = %+v, want IncludeProbes only", fake.lastOpts)
	}
}

func TestHealthHandler_IncludeCapabilities(t *testing.T) {
	fake := &fakeHealth{}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/health?include=capabilities", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if !fake.lastOpts.IncludeCapabilities {
		t.Errorf("opts = %+v, want IncludeCapabilities=true", fake.lastOpts)
	}
	if !fake.lastOpts.IncludeProbes {
		t.Errorf("opts = %+v, IncludeCapabilities must imply IncludeProbes (handler layer)", fake.lastOpts)
	}
}

func TestHealthHandler_RefreshFlag(t *testing.T) {
	fake := &fakeHealth{}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/health?refresh=true", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if !fake.lastOpts.Refresh {
		t.Errorf("opts = %+v, want Refresh=true", fake.lastOpts)
	}
}

func TestHealthHandler_UnknownIncludeTokenIgnored(t *testing.T) {
	fake := &fakeHealth{}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet,
		"/api/health?include=probes,future-section,capabilities", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (unknown tokens silently ignored)", rec.Code)
	}
	if !fake.lastOpts.IncludeProbes || !fake.lastOpts.IncludeCapabilities {
		t.Errorf("known tokens should still be honored: %+v", fake.lastOpts)
	}
}

func TestHealthHandler_RequiresSameOrigin(t *testing.T) {
	fake := &fakeHealth{}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "http://evil.example.com:9081/api/health", nil)
	req.Header.Set("Origin", "http://evil.example.com:9081")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (cross-origin must be rejected)", rec.Code)
	}
}

func TestHealthHandler_500OnBackendError(t *testing.T) {
	fake := &fakeHealth{returnErr: errSentinel}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "HEALTH_BACKEND_FAILED") {
		t.Errorf("body = %s, want code HEALTH_BACKEND_FAILED", rec.Body.String())
	}
}

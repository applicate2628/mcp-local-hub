package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// fakeCleanupAPI is the test seam for /api/cleanup/orphans. CleanupOrphansFn
// is invoked once per request; tests assert what opts arrived.
type fakeCleanupAPI struct {
	CleanupOrphansFn func(opts api.CleanupOpts) ([]api.OrphanProcess, error)
}

func (f fakeCleanupAPI) CleanupOrphans(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
	return f.CleanupOrphansFn(opts)
}

// newCleanupTestServer wires a fake cleanup API into a Server with the
// minimum scaffolding the handler needs. Port is set so allowedHost has
// a target to compare against in the cross-origin assertion.
func newCleanupTestServer(t *testing.T, f fakeCleanupAPI) *Server {
	t.Helper()
	s := &Server{cfg: Config{Port: 9125}, mux: http.NewServeMux()}
	s.cleanup = f
	registerCleanupRoutes(s)
	return s
}

// TestCleanupOrphansHandler_DryRun_OK posts dry_run=true and asserts the
// returned orphan list comes from the API verbatim, with HTTP 200 + JSON.
func TestCleanupOrphansHandler_DryRun_OK(t *testing.T) {
	gotOpts := api.CleanupOpts{}
	want := []api.OrphanProcess{
		{PID: 1234, Server: "memory", AgeSec: 120, RAMBytes: 100 * 1024 * 1024, Cmdline: "node memory"},
	}
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupOrphansFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			gotOpts = opts
			return want, nil
		},
	})

	body := strings.NewReader(`{"dry_run": true, "min_age_sec": 60}`)
	req := httptest.NewRequest("POST", "/api/cleanup/orphans", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !gotOpts.DryRun {
		t.Errorf("CleanupOpts.DryRun = false, want true")
	}
	if gotOpts.MinAgeSec != 60 {
		t.Errorf("CleanupOpts.MinAgeSec = %d, want 60", gotOpts.MinAgeSec)
	}

	var got struct {
		Orphans []api.OrphanProcess `json:"orphans"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rr.Body.String())
	}
	if len(got.Orphans) != 1 || got.Orphans[0].PID != 1234 {
		t.Errorf("orphans = %+v, want one orphan with PID 1234", got.Orphans)
	}
}

// TestCleanupOrphansHandler_Apply_OK posts dry_run=false and asserts the
// API was invoked with DryRun=false (kill mode).
func TestCleanupOrphansHandler_Apply_OK(t *testing.T) {
	gotOpts := api.CleanupOpts{}
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupOrphansFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			gotOpts = opts
			return []api.OrphanProcess{}, nil
		},
	})

	body := strings.NewReader(`{"dry_run": false}`)
	req := httptest.NewRequest("POST", "/api/cleanup/orphans", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if gotOpts.DryRun {
		t.Errorf("CleanupOpts.DryRun = true, want false (apply mode)")
	}
}

// TestCleanupOrphansHandler_GET_405 verifies GET is rejected so destructive
// operations cannot be triggered by a simple <img> tag or browser navigation.
func TestCleanupOrphansHandler_GET_405(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupOrphansFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("CleanupOrphans must not be called on GET")
			return nil, nil
		},
	})

	req := httptest.NewRequest("GET", "/api/cleanup/orphans", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", rr.Code)
	}
}

// TestCleanupOrphansHandler_BadJSON_400 verifies malformed body returns 400.
func TestCleanupOrphansHandler_BadJSON_400(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupOrphansFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("CleanupOrphans must not be called on bad JSON")
			return nil, nil
		},
	})

	req := httptest.NewRequest("POST", "/api/cleanup/orphans", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rr.Code)
	}
}

// TestCleanupOrphansHandler_RequiresSameOrigin verifies the same-origin
// gate (CSRF defense) rejects requests from foreign origins. The shared
// requireSameOrigin wrapper handles this; this test just confirms it is
// applied to the cleanup route, not absent.
func TestCleanupOrphansHandler_RequiresSameOrigin(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupOrphansFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("CleanupOrphans must not be called on cross-origin request")
			return nil, nil
		},
	})

	body := strings.NewReader(`{"dry_run": true}`)
	req := httptest.NewRequest("POST", "/api/cleanup/orphans", body)
	req.Header.Set("Origin", "http://evil.example.com")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Errorf("cross-origin request should be rejected; got 200")
	}
}

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

// fakeCleanupAPI is the test seam for both /api/cleanup/* routes. Tests
// set whichever Fn they need; the unset one is left nil and tests must
// not exercise that route.
type fakeCleanupAPI struct {
	CleanupOrphansFn     func(opts api.CleanupOpts) ([]api.OrphanProcess, error)
	CleanupLogWatchersFn func(opts api.LogWatcherCleanupOpts) ([]api.LogWatcher, error)
}

func (f fakeCleanupAPI) CleanupOrphans(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
	return f.CleanupOrphansFn(opts)
}

func (f fakeCleanupAPI) CleanupLogWatchers(opts api.LogWatcherCleanupOpts) ([]api.LogWatcher, error) {
	return f.CleanupLogWatchersFn(opts)
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

// TestCleanupLogWatchersHandler_DryRun_OK posts dry_run=true and asserts
// the returned watcher list comes from the API verbatim.
func TestCleanupLogWatchersHandler_DryRun_OK(t *testing.T) {
	gotOpts := api.LogWatcherCleanupOpts{}
	want := []api.LogWatcher{
		{PID: 1234, ParentPID: 555, ParentAlive: false, Name: "tail.exe", AgeSec: 3600,
			Cmdline: `tail.exe -F /d/dev/.scratch/x.log`},
	}
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupLogWatchersFn: func(opts api.LogWatcherCleanupOpts) ([]api.LogWatcher, error) {
			gotOpts = opts
			return want, nil
		},
	})

	body := strings.NewReader(`{"dry_run": true, "include_live": false}`)
	req := httptest.NewRequest("POST", "/api/cleanup/log-watchers", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !gotOpts.DryRun {
		t.Errorf("LogWatcherCleanupOpts.DryRun = false, want true")
	}
	if gotOpts.IncludeLive {
		t.Errorf("LogWatcherCleanupOpts.IncludeLive = true, want false")
	}

	var got struct {
		Watchers []api.LogWatcher `json:"watchers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rr.Body.String())
	}
	if len(got.Watchers) != 1 || got.Watchers[0].PID != 1234 {
		t.Errorf("watchers = %+v, want one with PID 1234", got.Watchers)
	}
}

// TestCleanupLogWatchersHandler_Apply_SkipsLiveParent_KilledCount verifies
// the apply-mode counter logic: a watcher whose parent is still alive
// is NOT counted as killed when IncludeLive=false (it was deliberately
// skipped by the API), even when KillErr is empty.
func TestCleanupLogWatchersHandler_Apply_SkipsLiveParent_KilledCount(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupLogWatchersFn: func(opts api.LogWatcherCleanupOpts) ([]api.LogWatcher, error) {
			// API returns: one orphan (killed), one live-parent (skipped
			// per IncludeLive=false), one dead-parent with kill error.
			return []api.LogWatcher{
				{PID: 1, ParentPID: 99, ParentAlive: false, Name: "tail.exe"},
				{PID: 2, ParentPID: 100, ParentAlive: true, Name: "bash.exe"},
				{PID: 3, ParentPID: 99, ParentAlive: false, Name: "grep.exe", KillErr: "no such process"},
			}, nil
		},
	})

	body := strings.NewReader(`{"dry_run": false, "include_live": false}`)
	req := httptest.NewRequest("POST", "/api/cleanup/log-watchers", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var got struct {
		Killed  int `json:"killed"`
		Skipped int `json:"skipped"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Killed != 1 {
		t.Errorf("killed = %d, want 1 (only the dead-parent orphan)", got.Killed)
	}
	if got.Skipped != 2 {
		t.Errorf("skipped = %d, want 2 (live-parent skipped + kill-err)", got.Skipped)
	}
}

// TestCleanupLogWatchersHandler_GET_405 — same destructive-op gate as
// orphans handler.
func TestCleanupLogWatchersHandler_GET_405(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupLogWatchersFn: func(opts api.LogWatcherCleanupOpts) ([]api.LogWatcher, error) {
			t.Fatal("CleanupLogWatchers must not be called on GET")
			return nil, nil
		},
	})
	req := httptest.NewRequest("GET", "/api/cleanup/log-watchers", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", rr.Code)
	}
}

package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// fakeCleanupAPI is the test seam for both /api/cleanup/* routes. Tests
// set whichever Fn they need; the unset one is left nil and tests must
// not exercise that route. SupportedFn (default true) drives the OS
// gate — tests can flip it to simulate POSIX-unsupported runs even on
// a Windows host. Codex Cloud bot P1 on PR #131 (commit 460e7ff): the
// production gate uses runtime.GOOS, but the test seam returns true
// so non-Windows CI exercises the full handler path.
type fakeCleanupAPI struct {
	SupportedFn           func() bool
	CleanupOrphansFn      func(opts api.CleanupOpts) ([]api.OrphanProcess, error)
	CleanupLogWatchersFn  func(opts api.LogWatcherCleanupOpts) ([]api.LogWatcher, error)
	AggressiveSupportedFn func() bool
	AggressiveCleanupFn   func(opts api.CleanupOpts) ([]api.OrphanProcess, error)
}

func (f fakeCleanupAPI) CleanupOrphansSupported() bool {
	if f.SupportedFn != nil {
		return f.SupportedFn()
	}
	return true
}

func (f fakeCleanupAPI) CleanupOrphans(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
	return f.CleanupOrphansFn(opts)
}

func (f fakeCleanupAPI) CleanupLogWatchers(opts api.LogWatcherCleanupOpts) ([]api.LogWatcher, error) {
	return f.CleanupLogWatchersFn(opts)
}

// CleanupAggressiveSupported defaults to true (like CleanupOrphansSupported)
// so cross-platform CI exercises the full handler path; flip
// AggressiveSupportedFn to false to assert the 501 OS-gate branch.
func (f fakeCleanupAPI) CleanupAggressiveSupported() bool {
	if f.AggressiveSupportedFn != nil {
		return f.AggressiveSupportedFn()
	}
	return true
}

func (f fakeCleanupAPI) AggressiveCleanup(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
	return f.AggressiveCleanupFn(opts)
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

// TestCleanupOrphansHandler_DryRun_OK posts apply=false (preview /
// dry-run) and asserts the returned orphan list comes from the API
// verbatim, with HTTP 200 + JSON. Wire-shape uses `apply` so the
// zero-value path is safe (Codex bot P2 — kosyak
// 2026-05-07-destructive-endpoint-with-unsafe-zero-value-default.md).
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

	body := strings.NewReader(`{"apply": false, "min_age_sec": 60}`)
	req := httptest.NewRequest("POST", "/api/cleanup/orphans", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !gotOpts.DryRun {
		t.Errorf("CleanupOpts.DryRun = false, want true (apply=false → handler flips)")
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

// TestCleanupOrphansHandler_Apply_OK posts apply=true (explicit kill
// mode) and asserts the API was invoked with DryRun=false (kill).
func TestCleanupOrphansHandler_Apply_OK(t *testing.T) {
	gotOpts := api.CleanupOpts{}
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupOrphansFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			gotOpts = opts
			return []api.OrphanProcess{}, nil
		},
	})

	body := strings.NewReader(`{"apply": true}`)
	req := httptest.NewRequest("POST", "/api/cleanup/orphans", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if gotOpts.DryRun {
		t.Errorf("CleanupOpts.DryRun = true, want false (apply=true → kill mode)")
	}
}

// TestCleanupOrphansHandler_EmptyBody_DryRun is the regression test
// for Codex bot P2 / kosyak
// 2026-05-07-destructive-endpoint-with-unsafe-zero-value-default.md:
// `{}` (or any body that omits `apply`) MUST land on the dry-run
// path because `Apply` zero-value is false. Older / buggy clients
// must not accidentally trigger the destructive path.
func TestCleanupOrphansHandler_EmptyBody_DryRun(t *testing.T) {
	gotOpts := api.CleanupOpts{}
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupOrphansFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			gotOpts = opts
			return []api.OrphanProcess{}, nil
		},
	})

	body := strings.NewReader(`{}`)
	req := httptest.NewRequest("POST", "/api/cleanup/orphans", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !gotOpts.DryRun {
		t.Errorf("empty body must land on DRY-RUN path (CleanupOpts.DryRun=true); got DryRun=false")
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

func TestCleanupOrphansHandler_NegativeMinAge_400(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupOrphansFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("CleanupOrphans must not be called for negative min_age_sec")
			return nil, nil
		},
	})

	body := strings.NewReader(`{"apply": true, "min_age_sec": -1}`)
	req := httptest.NewRequest("POST", "/api/cleanup/orphans", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rr.Code)
	}
}

func TestCleanupOrphansHandler_UnknownServer_400(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		CleanupOrphansFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("CleanupOrphans must not be called for unknown server")
			return nil, nil
		},
	})

	body := strings.NewReader(`{"apply": true, "server": "__not_a_real_manifest__"}`)
	req := httptest.NewRequest("POST", "/api/cleanup/orphans", body)
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

	body := strings.NewReader(`{"apply": false}`)
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

	body := strings.NewReader(`{"apply": false, "include_live": false}`)
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

	body := strings.NewReader(`{"apply": true, "include_live": false}`)
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

// TestCleanupOrphansHandler_Unsupported_501 verifies the OS gate path.
// Tests inject SupportedFn returning false to simulate non-Windows
// without depending on the actual host OS — Codex Cloud bot P1 on
// PR #131 (commit 460e7ff): the platform gate must go through the
// cleanupAPI seam, not runtime.GOOS, so this test is identical on
// Windows and POSIX runners.
func TestCleanupOrphansHandler_Unsupported_501(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		SupportedFn: func() bool { return false },
		CleanupOrphansFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("CleanupOrphans must not be called when CleanupOrphansSupported returns false")
			return nil, nil
		},
	})

	body := strings.NewReader(`{"apply": false}`)
	req := httptest.NewRequest("POST", "/api/cleanup/orphans", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 501; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not_supported_on_this_os") {
		t.Errorf("body should contain not_supported_on_this_os; got %s", rr.Body.String())
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

// --- /api/cleanup/aggressive handler ---------------------------------------
//
// The aggressive sweep is the operator-confirmed override that kills the
// live-rooted MCP-stdio fan-out the default safe sweep refuses to touch.
// These tests inject fakeCleanupAPI.AggressiveCleanupFn so nothing real
// dies — the fake never runs a process snapshot or taskkill. They clone
// the orphan-handler family (dry-run/apply opts assertion, scope
// validation, method/origin/OS gates).

// TestCleanupAggressiveHandler_DryRun_OK posts apply=false (preview) with
// a valid --client scope and asserts the seam was called with DryRun=true
// and the candidate list is returned verbatim.
func TestCleanupAggressiveHandler_DryRun_OK(t *testing.T) {
	gotOpts := api.CleanupOpts{}
	want := []api.OrphanProcess{
		{PID: 4321, RAMBytes: 50 * 1024 * 1024, AgeSec: 300, CmdlineDisplay: "node.exe", MatchSource: "codex"},
	}
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			gotOpts = opts
			return want, nil
		},
	})

	body := strings.NewReader(`{"apply": false, "client": "codex", "min_age_sec": 60}`)
	req := httptest.NewRequest("POST", "/api/cleanup/aggressive", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !gotOpts.DryRun {
		t.Errorf("CleanupOpts.DryRun = false, want true (apply=false → handler flips)")
	}
	if !gotOpts.Aggressive {
		t.Errorf("CleanupOpts.Aggressive = false, want true")
	}
	if gotOpts.Client != "codex" {
		t.Errorf("CleanupOpts.Client = %q, want codex", gotOpts.Client)
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
	if len(got.Orphans) != 1 || got.Orphans[0].PID != 4321 || got.Orphans[0].MatchSource != "codex" {
		t.Errorf("orphans = %+v, want one candidate PID 4321 match_source=codex", got.Orphans)
	}
}

// TestCleanupAggressiveHandler_Apply_OK posts apply=true with a --root-pid
// scope + an opted-in danger class, and asserts the seam ran with
// DryRun=false (kill mode) and the include-class passed through.
func TestCleanupAggressiveHandler_Apply_OK(t *testing.T) {
	gotOpts := api.CleanupOpts{}
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			gotOpts = opts
			return []api.OrphanProcess{
				{PID: 11, CmdlineDisplay: "python.exe", MatchSource: "root-pid 999"},
				{PID: 22, CmdlineDisplay: "node.exe", MatchSource: "root-pid 999", KillErr: "access denied"},
			}, nil
		},
	})

	body := strings.NewReader(`{"apply": true, "root_pid": 999, "include_classes": ["chrome"]}`)
	req := httptest.NewRequest("POST", "/api/cleanup/aggressive", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if gotOpts.DryRun {
		t.Errorf("CleanupOpts.DryRun = true, want false (apply=true → kill mode)")
	}
	if gotOpts.RootPID != 999 {
		t.Errorf("CleanupOpts.RootPID = %d, want 999", gotOpts.RootPID)
	}
	if len(gotOpts.IncludeClasses) != 1 || gotOpts.IncludeClasses[0] != "chrome" {
		t.Errorf("CleanupOpts.IncludeClasses = %v, want [chrome]", gotOpts.IncludeClasses)
	}

	var got struct {
		Killed  int `json:"killed"`
		Skipped int `json:"skipped"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Killed != 1 {
		t.Errorf("killed = %d, want 1 (only the kill_err-free row)", got.Killed)
	}
	if got.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the access-denied row)", got.Skipped)
	}
}

// TestCleanupAggressiveHandler_EmptyBody_NoScope_400 verifies a `{}` body
// (no scope) is rejected 400 — exactly one of client/root_pid is
// required, and the safe zero-value still cannot trigger an implicit
// "all live-rooted" sweep. The seam must NOT be called.
func TestCleanupAggressiveHandler_NoScope_400(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("AggressiveCleanup must not be called with no scope")
			return nil, nil
		},
	})

	body := strings.NewReader(`{"apply": false}`)
	req := httptest.NewRequest("POST", "/api/cleanup/aggressive", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rr.Code)
	}
}

// TestCleanupAggressiveHandler_BothScopes_400 verifies setting BOTH
// client and root_pid is rejected 400 (mutually exclusive). Seam not
// called.
func TestCleanupAggressiveHandler_BothScopes_400(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("AggressiveCleanup must not be called with both scopes")
			return nil, nil
		},
	})

	body := strings.NewReader(`{"apply": false, "client": "codex", "root_pid": 999}`)
	req := httptest.NewRequest("POST", "/api/cleanup/aggressive", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rr.Code)
	}
}

// TestCleanupAggressiveHandler_UnknownClient_400 verifies a backend
// errAggressiveUnknownClient (surfaced through the exported sentinel) is
// mapped to 400 via errors.Is, not a generic 500. The valid-scope
// pre-check passes (exactly one scope set), so this exercises the
// backend-error classification path specifically.
func TestCleanupAggressiveHandler_UnknownClient_400(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			return nil, api.ErrAggressiveUnknownClient
		},
	})

	body := strings.NewReader(`{"apply": false, "client": "notaclient"}`)
	req := httptest.NewRequest("POST", "/api/cleanup/aggressive", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestCleanupAggressiveHandler_BackendError_500 verifies a non-scope
// backend error (e.g. a process-snapshot failure) maps to 500.
func TestCleanupAggressiveHandler_BackendError_500(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			return nil, errors.New("wmic snapshot failed")
		},
	})

	body := strings.NewReader(`{"apply": false, "client": "codex"}`)
	req := httptest.NewRequest("POST", "/api/cleanup/aggressive", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
}

// TestCleanupAggressiveHandler_NegativeMinAge_400 — min_age_sec must be >= 0.
func TestCleanupAggressiveHandler_NegativeMinAge_400(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("AggressiveCleanup must not be called for negative min_age_sec")
			return nil, nil
		},
	})

	body := strings.NewReader(`{"apply": false, "client": "codex", "min_age_sec": -1}`)
	req := httptest.NewRequest("POST", "/api/cleanup/aggressive", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rr.Code)
	}
}

// TestCleanupAggressiveHandler_GET_405 — destructive op rejected on GET.
func TestCleanupAggressiveHandler_GET_405(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("AggressiveCleanup must not be called on GET")
			return nil, nil
		},
	})

	req := httptest.NewRequest("GET", "/api/cleanup/aggressive", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", rr.Code)
	}
}

// TestCleanupAggressiveHandler_BadJSON_400 — malformed body returns 400.
func TestCleanupAggressiveHandler_BadJSON_400(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("AggressiveCleanup must not be called on bad JSON")
			return nil, nil
		},
	})

	req := httptest.NewRequest("POST", "/api/cleanup/aggressive", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rr.Code)
	}
}

// TestCleanupAggressiveHandler_RequiresSameOrigin — CSRF gate rejects a
// foreign origin (same shared wrapper as the orphan route).
func TestCleanupAggressiveHandler_RequiresSameOrigin(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("AggressiveCleanup must not be called on cross-origin request")
			return nil, nil
		},
	})

	body := strings.NewReader(`{"apply": false, "client": "codex"}`)
	req := httptest.NewRequest("POST", "/api/cleanup/aggressive", body)
	req.Header.Set("Origin", "http://evil.example.com")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Errorf("cross-origin request should be rejected; got 200")
	}
}

// TestCleanupAggressiveHandler_Unsupported_501 — OS gate through the seam
// (AggressiveSupportedFn=false) so the assertion is identical on Windows
// and POSIX runners.
func TestCleanupAggressiveHandler_Unsupported_501(t *testing.T) {
	s := newCleanupTestServer(t, fakeCleanupAPI{
		AggressiveSupportedFn: func() bool { return false },
		AggressiveCleanupFn: func(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
			t.Fatal("AggressiveCleanup must not be called when CleanupAggressiveSupported returns false")
			return nil, nil
		},
	})

	body := strings.NewReader(`{"apply": false, "client": "codex"}`)
	req := httptest.NewRequest("POST", "/api/cleanup/aggressive", body)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 501; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not_supported_on_this_os") {
		t.Errorf("body should contain not_supported_on_this_os; got %s", rr.Body.String())
	}
}

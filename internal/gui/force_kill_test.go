package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// Task 13: HTTP wrappers around C1's Probe + KillRecordedHolder.
// macOS short-circuits to 501 for both endpoints (memo D13). On
// Windows/Linux, the probe handler returns the Verdict as JSON; the
// kill handler maps Verdict.Class onto HTTP status: VerdictKillRefused
// -> 403 (identity gate), VerdictRaceLost -> 412 (lock mtime changed
// mid-flight), other failures -> 500. VerdictHealthy / VerdictKilledRecovered
// -> 200 (success or no-op).
//
// Test seams probeForRoute / killForRoute on Server let tests drive
// deterministic outcomes without touching real file locks or processes.

func TestForceKillProbe_ReturnsVerdict(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS returns 501 — covered by separate test")
	}
	srv := newDaemonsTestServer(t)
	srv.probeForRoute = func() Verdict {
		return Verdict{Class: VerdictHealthy, PIDAlive: true, PingMatch: true}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/force-kill/probe", nil)
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var v Verdict
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Class != VerdictHealthy {
		t.Errorf("Class = %v, want VerdictHealthy", v.Class)
	}
}

func TestForceKill_RequiresPOST(t *testing.T) {
	srv := newDaemonsTestServer(t)
	for _, path := range []string{"/api/force-kill", "/api/force-kill/probe"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header = sameOriginHeaders()
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s GET status = %d, want 405", path, rec.Code)
		}
	}
}

func TestForceKill_IdentityGateRefuse_403(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS returns 501")
	}
	srv := newDaemonsTestServer(t)
	srv.killForRoute = func() (Verdict, error) {
		v := Verdict{Class: VerdictKillRefused, PID: 1234, Diagnose: "image mismatch"}
		return v, errors.New("kill refused: image mismatch")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/force-kill", nil)
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestForceKill_LockChanged_412(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS returns 501")
	}
	srv := newDaemonsTestServer(t)
	srv.killForRoute = func() (Verdict, error) {
		v := Verdict{Class: VerdictRaceLost, Diagnose: "pidport changed mid-prompt"}
		return v, errors.New("pidport changed mid-prompt")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/force-kill", nil)
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}
}

func TestForceKill_KillFailed_500(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS returns 501")
	}
	srv := newDaemonsTestServer(t)
	srv.killForRoute = func() (Verdict, error) {
		v := Verdict{Class: VerdictKillFailed, PID: 1234, Diagnose: "kill PID 1234 failed: permission denied"}
		return v, errors.New("kill PID 1234 failed: permission denied")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/force-kill", nil)
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestForceKill_Recovered_200(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS returns 501")
	}
	srv := newDaemonsTestServer(t)
	srv.killForRoute = func() (Verdict, error) {
		return Verdict{Class: VerdictKilledRecovered, PID: 1234}, nil
	}
	req := httptest.NewRequest(http.MethodPost, "/api/force-kill", nil)
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var v Verdict
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Class != VerdictKilledRecovered {
		t.Errorf("Class = %v, want VerdictKilledRecovered", v.Class)
	}
}

func TestForceKill_Macos_501(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only meaningful on macOS")
	}
	srv := newDaemonsTestServer(t)
	for _, path := range []string{"/api/force-kill", "/api/force-kill/probe"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header = sameOriginHeaders()
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s status = %d, want 501", path, rec.Code)
		}
		// Body must carry the product-neutral macOS copy (memo D13).
		body := rec.Body.String()
		if !strings.Contains(body, "Lock recovery is not yet supported on macOS") {
			t.Errorf("%s body missing macOS copy: %s", path, body)
		}
		// Memo D13: copy must NOT reference CLAUDE.md.
		if strings.Contains(body, "CLAUDE.md") {
			t.Errorf("%s body must not reference CLAUDE.md (memo D13): %s", path, body)
		}
	}
}

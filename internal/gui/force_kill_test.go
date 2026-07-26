package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
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

// TestForceKill_Indeterminate_503 pins the review finding that the new
// VerdictIndeterminate class needs explicit HTTP handling. KillRecordedHolder
// SKIPS the kill on that class (its Malformed/DeadPID/Indeterminate arm), so
// falling into the default arm reported a skipped operation as 500
// `kill_failed` — implying both that a kill was attempted and that the server
// broke. Neither is true: liveness simply could not be established.
//
// MUTATION: delete the `case VerdictIndeterminate` arm in forceKillHandler —
// this test then fails with "status = 500, want 503".
func TestForceKill_Indeterminate_503(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS returns 501")
	}
	srv := newDaemonsTestServer(t)
	srv.killForRoute = func() (Verdict, error) {
		v := Verdict{
			Class:    VerdictIndeterminate,
			PID:      1234,
			Diagnose: "recorded PID 1234: liveness probe returned an ambiguous platform error",
		}
		return v, errors.New("kill skipped: Indeterminate")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/force-kill", nil)
	req.Header = sameOriginHeaders()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error   string  `json:"error"`
		Verdict Verdict `json:"verdict"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The error key is load-bearing: the frontend branches on it to say
	// "Kill skipped" rather than "Kill failed".
	if body.Error != "liveness_indeterminate" {
		t.Errorf("error key = %q, want %q", body.Error, "liveness_indeterminate")
	}
	if body.Verdict.Class != VerdictIndeterminate {
		t.Errorf("verdict class = %v, want VerdictIndeterminate", body.Verdict.Class)
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

// TestCheckIdentityGate_OwnerSIDArm is the SEC-F3 falsifying test for the GUI
// `mcphub gui --force --kill` identity gate. It builds a Verdict that passes the
// image / argv / start-time arms, then drives ONLY the owner-SID arm through the
// processOwnerSIDMatchesCurrentFn seam:
//
//   - same-SID    → gate does not refuse (kill may proceed) — single-user case.
//   - different   → gate REFUSES with the owner-SID reason. Pre-fix the gate had
//     no owner check and would authorize killing another user's
//     GUI process.
//   - unverifiable → gate REFUSES (fail closed).
//
// No real flock, kill, or process token is touched — the seam is faked.
func TestCheckIdentityGate_OwnerSIDArm(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS short-circuits the gate before the owner-SID arm")
	}

	orig := processOwnerSIDMatchesCurrentFn
	t.Cleanup(func() { processOwnerSIDMatchesCurrentFn = orig })

	// A Verdict that passes image + argv + start-time so only the SID arm decides.
	v := Verdict{
		PID:           4242,
		PIDImage:      mcphubBinaryNameForTest(),
		pidCmdlineRaw: []string{mcphubBinaryNameForTest(), "gui"},
		PIDStart:      time.Unix(1000, 0),
		Mtime:         time.Unix(2000, 0),
	}

	// Same owner SID → no refusal.
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil }
	if refused, reason := CheckIdentityGate(v); refused {
		t.Fatalf("same-owner SID must NOT refuse the gate; refused with %q", reason)
	}

	// Different owner SID → refusal naming the owner-SID gate.
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return false, nil }
	refused, reason := CheckIdentityGate(v)
	if !refused {
		t.Fatal("different-owner SID must REFUSE the gate; got refused=false")
	}
	if !strings.Contains(reason, "different user") {
		t.Fatalf("refusal reason must name the different-user owner; got %q", reason)
	}

	// Unverifiable owner SID → refusal (fail closed).
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) {
		return false, errors.New("OpenProcessToken: access denied")
	}
	if refused, _ := CheckIdentityGate(v); !refused {
		t.Fatal("unverifiable owner SID must REFUSE the gate (fail closed); got refused=false")
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

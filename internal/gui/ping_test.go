package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPing_ReturnsJSONWithPIDAndVersion(t *testing.T) {
	s := NewServer(Config{Version: "test-v1", PID: 4242})
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		OK      bool   `json:"ok"`
		PID     int    `json:"pid"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK || body.PID != 4242 || body.Version != "test-v1" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestPing_OrdinaryWireShapeRemainsByteCompatible(t *testing.T) {
	s := NewServer(Config{Version: "test-v1", PID: 4242})
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if got, want := rec.Body.String(), "{\"ok\":true,\"pid\":4242,\"version\":\"test-v1\"}\n"; got != want {
		t.Fatalf("ordinary /api/ping body = %q, want byte-compatible %q", got, want)
	}
}

func TestRestartV3_ChallengedStandbyPingBindsExactChild(t *testing.T) {
	nonce := bytes.Repeat([]byte{0x5a}, 32)
	identity := AuthenticatedReadinessIdentity{
		HandoffID:  "handoff-f",
		Generation: "generation-f",
		Sequence:   7,
		PID:        4242,
		Port:       19125,
	}
	session, err := NewAuthenticatedReadinessSession(identity, nonce)
	if err != nil {
		t.Fatalf("NewAuthenticatedReadinessSession: %v", err)
	}
	t.Cleanup(session.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/ping?challenge=parent-challenge-f", nil)
	rec := httptest.NewRecorder()
	session.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("challenged ping status = %d, body=%q", rec.Code, rec.Body.String())
	}
	raw := append([]byte(nil), rec.Body.Bytes()...)
	var proof AuthenticatedReadinessProof
	if err := json.NewDecoder(rec.Body).Decode(&proof); err != nil {
		t.Fatalf("decode challenged ping: %v", err)
	}
	if !VerifyAuthenticatedReadiness(nonce, "parent-challenge-f", proof, identity) {
		t.Fatal("challenged readiness proof did not authenticate the exact child identity")
	}

	spoof := proof
	spoof.PID++
	if VerifyAuthenticatedReadiness(nonce, "parent-challenge-f", spoof, identity) {
		t.Fatal("PID-reuse spoof authenticated after changing the MAC-bound PID")
	}
	if bytes.Contains(raw, nonce) {
		t.Fatal("challenged readiness response exposed the raw nonce")
	}
}

func TestActivateWindow_MarksSignalReceived(t *testing.T) {
	s := NewServer(Config{})
	got := make(chan struct{}, 1)
	s.OnActivateWindow(func() error { got <- struct{}{}; return nil })
	req := httptest.NewRequest(http.MethodPost, "/api/activate-window", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	select {
	case <-got:
	default:
		t.Error("activate-window did not invoke callback")
	}
}

// TestActivateWindow_HeadlessReturns503 verifies the handler maps
// ErrActivationNoTarget to 503 Service Unavailable so the second
// invocation's TryActivateIncumbent can surface a useful message
// instead of falsely claiming activation succeeded.
func TestActivateWindow_HeadlessReturns503(t *testing.T) {
	s := NewServer(Config{})
	s.OnActivateWindow(func() error {
		return &ActivationNoTargetError{Reason: ReasonHeadless}
	})
	req := httptest.NewRequest(http.MethodPost, "/api/activate-window", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get(activationNoTargetReasonHeader); got != string(ReasonHeadless) {
		t.Errorf("reason header = %q, want %q", got, ReasonHeadless)
	}
}

// TestActivateWindow_OtherErrorReturns500 confirms unexpected callback
// errors map to 500 (not 503 — that's reserved for the documented
// typed no-activation-target outcome).
func TestActivateWindow_OtherErrorReturns500(t *testing.T) {
	s := NewServer(Config{})
	s.OnActivateWindow(func() error { return errPingTestSentinel })
	req := httptest.NewRequest(http.MethodPost, "/api/activate-window", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

var errPingTestSentinel = errors.New("ping test sentinel")

func TestPing_WrongMethodIs405(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/ping", strings.NewReader(""))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

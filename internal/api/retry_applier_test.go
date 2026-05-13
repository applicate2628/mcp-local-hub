package api

import (
	"strings"
	"testing"
	"time"
)

// TestApplyRetryPolicy_KnownNames pins the A4-b PR #2 runtime
// applier contract: each known policy resolves to its documented
// MaxAttempts cap and SetMaxAttempts is invoked on the engine.
func TestApplyRetryPolicy_KnownNames(t *testing.T) {
	cases := []struct {
		policy           string
		wantMaxAttempts  int
		wantResolvedHint string
	}{
		{"none", 1, "none"},
		{"linear", 3, "linear"},
		{"exponential", 5, "exponential"},
	}
	for _, c := range cases {
		engine := newCooldownEngine(nil)
		stateRead := WatchdogStateRead{State: WatchdogStateValid, Cool: engine}
		resolved, got := ApplyRetryPolicy(&stateRead, c.policy)
		if got != c.wantMaxAttempts {
			t.Errorf("ApplyRetryPolicy(%q) maxAttempts = %d, want %d", c.policy, got, c.wantMaxAttempts)
		}
		if !strings.Contains(resolved, c.wantResolvedHint) {
			t.Errorf("ApplyRetryPolicy(%q) resolved = %q, want substring %q", c.policy, resolved, c.wantResolvedHint)
		}
		if engine.MaxAttemptsConfigured() != c.wantMaxAttempts {
			t.Errorf("ApplyRetryPolicy(%q): engine.MaxAttemptsConfigured() = %d, want %d (applier did not call SetMaxAttempts)",
				c.policy, engine.MaxAttemptsConfigured(), c.wantMaxAttempts)
		}
	}
}

// TestApplyRetryPolicy_UnknownFallsBackToExponential pins that a
// stale on-disk value (operator downgraded from a future build with
// a new policy name) falls back to "exponential" without panicking
// and the engine receives the fallback's MaxAttempts.
func TestApplyRetryPolicy_UnknownFallsBackToExponential(t *testing.T) {
	engine := newCooldownEngine(nil)
	stateRead := WatchdogStateRead{State: WatchdogStateValid, Cool: engine}
	resolved, max := ApplyRetryPolicy(&stateRead, "fibonacci-bonkers")
	if max != 5 { // exponential's MaxAttempts
		t.Errorf("unknown policy maxAttempts = %d, want 5 (exponential fallback)", max)
	}
	if !strings.Contains(resolved, "exponential") {
		t.Errorf("resolved = %q, want substring 'exponential'", resolved)
	}
	if !strings.Contains(resolved, "fibonacci-bonkers") {
		t.Errorf("resolved should mention the original unknown value for operator forensics; got %q", resolved)
	}
	if engine.MaxAttemptsConfigured() != 5 {
		t.Errorf("engine.MaxAttemptsConfigured() = %d, want 5 (applier did not apply fallback)",
			engine.MaxAttemptsConfigured())
	}
}

// TestApplyRetryPolicy_SuppressEngineNoop pins that ApplyRetryPolicy
// is a safe no-op when the WatchdogStateRead carries the fail-CLOSED
// suppressAllCooldown stub (corrupt state). The function must NOT
// panic and must return the resolved policy details for logging.
func TestApplyRetryPolicy_SuppressEngineNoop(t *testing.T) {
	stateRead := WatchdogStateRead{
		State: WatchdogStateCorrupt,
		Cool:  suppressAllCooldown{},
	}
	resolved, max := ApplyRetryPolicy(&stateRead, "linear")
	if resolved != "linear" {
		t.Errorf("resolved = %q, want \"linear\"", resolved)
	}
	if max != 3 {
		t.Errorf("maxAttempts = %d, want 3 (linear)", max)
	}
}

// TestCooldownEngine_DueRespectsConfiguredMaxAttempts pins the
// load-bearing wiring: after SetMaxAttempts(1), the engine refuses
// Due() once AttemptsInWindow reaches 1. After SetMaxAttempts(5),
// it accepts up to 5. The Due() math becomes "AttemptsInWindow <
// maxAttempts" instead of the hardcoded AttemptWindowMax.
func TestCooldownEngine_DueRespectsConfiguredMaxAttempts(t *testing.T) {
	engine := newCooldownEngine(nil)
	engine.SetMaxAttempts(1)
	// Simulate "first attempt happened 1s ago".
	now := time.Now()
	engine.entries["d1"] = CooldownEntry{
		FirstAttemptAt:   now.Add(-time.Second),
		AttemptsInWindow: 1,
	}
	if engine.Due("d1", now) {
		t.Errorf("Due returned true with maxAttempts=1 and 1 attempt already recorded; want cooldown (false)")
	}
	// Bump cap to 5 — same entry should now be Due.
	engine.SetMaxAttempts(5)
	if !engine.Due("d1", now) {
		t.Errorf("Due returned false with maxAttempts=5 and 1 attempt recorded; want Due (true)")
	}
}

// TestCooldownEngine_SetMaxAttemptsClampsBelowOne pins the contract
// that SetMaxAttempts(0) and negative values clamp to 1 (the "none"
// policy semantic — one shot then cooldown). Without the clamp, a
// stale value could fall through to "AttemptsInWindow < 0" which
// would never accept Due() even on first attempt.
func TestCooldownEngine_SetMaxAttemptsClampsBelowOne(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		engine := newCooldownEngine(nil)
		engine.SetMaxAttempts(n)
		if engine.MaxAttemptsConfigured() != 1 {
			t.Errorf("SetMaxAttempts(%d) → MaxAttemptsConfigured() = %d, want 1 (clamped)",
				n, engine.MaxAttemptsConfigured())
		}
	}
}

package cli

import (
	"testing"
	"time"
)

// The tests below cover the thundering-herd amplifier: the six serena workspace
// proxies fail within ~250 ms of each other and then re-arm on an IDENTICAL
// deterministic ladder, so every retry wave re-collides at full width and
// reproduces the contention that caused the first failure.
//
// NON-VACUITY: against the pre-fix tree armRespawnBackoffTimer armed
// computeRespawnBackoff(failures) verbatim, so TestRespawnBackoff_HerdDoesNotRearmInLockstep
// sees one distinct delay and fails. See the RED evidence in the delivery report.

// TestRespawnBackoff_HerdDoesNotRearmInLockstep is the herd regression: a fleet
// of same-server daemons that crash simultaneously must NOT all re-arm at the
// same instant. Drives the production arm path (armRespawnBackoffTimer) for a
// six-daemon fleet at the same failure count and asserts the armed delays are
// actually spread.
func TestRespawnBackoff_HerdDoesNotRearmInLockstep(t *testing.T) {
	ctrl, _, _ := armGenController(t)

	// The observed fleet: six serena workspace proxies, same failure count, so
	// the deterministic ladder hands all six an identical base.
	const fleet = 6
	const failures = 3

	base := computeRespawnBackoff(failures)
	distinct := map[time.Duration]struct{}{}
	for i := 0; i < fleet; i++ {
		d := serenaReadyBudgetDescriptor()
		// Distinct task names so the arm-generation guard treats them as six
		// independent daemons rather than superseding each other.
		d.TaskName = `\mcp-local-hub-serena-herd-` + string(rune('a'+i))
		armed := ctrl.armRespawnBackoffTimer(*d, d.TaskName, base)

		if armed < base {
			t.Fatalf("daemon %d armed %v, which is SOONER than the ladder's %v — downward jitter would accelerate the sliding-window crash count toward quarantine", i, armed, base)
		}
		distinct[armed] = struct{}{}
	}

	if len(distinct) == 1 {
		t.Fatalf("all %d daemons armed the identical delay %v — they re-enter cold start in lockstep and reproduce the contention that failed them (this is the herd the fix must break)", fleet, base)
	}
}

// TestJitteredRespawnBackoff_Bounds pins the spread's contract directly on the
// pure function, across the whole ladder including the capped plateau, without
// depending on randomness.
func TestJitteredRespawnBackoff_Bounds(t *testing.T) {
	for _, base := range []time.Duration{
		respawnBackoffStep,
		4 * time.Second,
		respawnBackoffMax,
	} {
		span := time.Duration(float64(base) * respawnBackoffJitterFraction)
		if span > respawnBackoffJitterMax {
			span = respawnBackoffJitterMax
		}
		for _, r := range []float64{0, 0.25, 0.5, 0.75, 1} {
			got := jitteredRespawnBackoff(base, r)
			// Upward-only: never sooner than the ladder.
			if got < base {
				t.Fatalf("base=%v r=%v: armed %v < base — jitter must never accelerate a retry", base, r, got)
			}
			if got > base+span {
				t.Fatalf("base=%v r=%v: armed %v exceeds base+span (%v) — the recovery-latency tail is unbounded", base, r, got, base+span)
			}
		}
	}
}

// TestJitteredRespawnBackoff_ZeroBaseUnchanged: a zero backoff means "respawn
// now" (the failures<=0 case). Delaying it would change spawn semantics.
func TestJitteredRespawnBackoff_ZeroBaseUnchanged(t *testing.T) {
	if got := jitteredRespawnBackoff(0, 1); got != 0 {
		t.Fatalf("jitteredRespawnBackoff(0, 1) = %v, want 0 — an immediate respawn must stay immediate", got)
	}
}

// TestJitteredRespawnBackoff_OutOfRangeFractionClamped keeps the spread bounded
// even if a caller supplies a fraction outside [0,1].
func TestJitteredRespawnBackoff_OutOfRangeFractionClamped(t *testing.T) {
	base := 4 * time.Second
	span := time.Duration(float64(base) * respawnBackoffJitterFraction)
	if got := jitteredRespawnBackoff(base, -5); got != base {
		t.Fatalf("negative fraction produced %v, want the bare base %v", got, base)
	}
	if got := jitteredRespawnBackoff(base, 5); got != base+span {
		t.Fatalf("over-range fraction produced %v, want the capped %v", got, base+span)
	}
}

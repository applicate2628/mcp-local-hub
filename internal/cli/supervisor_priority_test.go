package cli

import "testing"

// TestDecideSupervisorPriorityRaise pins the raise-only / never-lower /
// never-IDLE / don't-touch-unknown policy. These cases are chosen to be
// MUTATION-ADEQUATE: each documented mutant of decideSupervisorPriorityRaise
// fails at least one case (see the mutation notes on individual sub-cases).
func TestDecideSupervisorPriorityRaise(t *testing.T) {
	cases := []struct {
		name       string
		current    priorityRank
		wantTarget priorityRank
		wantRaise  bool
	}{
		{
			// The whole point: IDLE must be raised to the floor.
			// MUTANT "floor := rankIdle" → returns raise=false here → FAILS.
			// MUTANT "target := rankIdle" → target != rankBelowNormal → FAILS.
			name: "idle raises to below-normal", current: rankIdle,
			wantTarget: rankBelowNormal, wantRaise: true,
		},
		{
			// Already at the floor → no-op.
			// MUTANT ">= → >" → 1 > 1 is false → would raise at the floor → FAILS.
			name: "below-normal is at floor, no raise", current: rankBelowNormal,
			wantRaise: false,
		},
		{
			// Never LOWER a NORMAL process to the floor.
			// A raw-constant comparison (IDLE 0x40 vs BELOW_NORMAL 0x4000) would
			// mis-rank NORMAL (0x20) as below the floor and lower it → this case
			// is why the decision is on ranks, not constants.
			// MUTANT ">= → <=" → raises NORMAL to the floor → FAILS.
			name: "normal is above floor, no raise", current: rankNormal,
			wantRaise: false,
		},
		{
			name: "above-normal is above floor, no raise", current: rankAboveNormal,
			wantRaise: false,
		},
		{
			// Never lower a HIGH process.
			name: "high is above floor, no raise", current: rankHigh,
			wantRaise: false,
		},
		{
			name: "realtime is above floor, no raise", current: rankRealtime,
			wantRaise: false,
		},
		{
			// Unrecognized class → leave it alone.
			// MUTANT "drop the unknown guard" → -1 >= 1 is false → raises → FAILS.
			name: "unknown is left untouched", current: rankUnknown,
			wantRaise: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, raise := decideSupervisorPriorityRaise(tc.current)
			if raise != tc.wantRaise {
				t.Fatalf("decideSupervisorPriorityRaise(%d) raise = %v, want %v", tc.current, raise, tc.wantRaise)
			}
			if raise && target != tc.wantTarget {
				t.Fatalf("decideSupervisorPriorityRaise(%d) target = %d, want %d", tc.current, target, tc.wantTarget)
			}
		})
	}
}

// TestDecideSupervisorPriorityRaiseInvariants asserts the safety invariants
// hold for EVERY possible input rank, independently of the table above.
// This is the backstop that catches a mutant which happens to satisfy the
// table's expected values but violates a global invariant.
func TestDecideSupervisorPriorityRaiseInvariants(t *testing.T) {
	// The floor itself must never be IDLE (a mutated const would silently
	// let the supervisor "raise" itself to IDLE).
	if supervisorPriorityFloorRank <= rankIdle {
		t.Fatalf("supervisorPriorityFloorRank = %d must be strictly above rankIdle (%d)", supervisorPriorityFloorRank, rankIdle)
	}

	for current := rankUnknown; current <= rankRealtime; current++ {
		target, raise := decideSupervisorPriorityRaise(current)
		if !raise {
			continue
		}
		// NEVER-IDLE: a raise target is never IDLE.
		if target == rankIdle {
			t.Fatalf("current=%d: raise target must never be rankIdle", current)
		}
		// RAISE-ONLY: a raise never targets a rank below the current one.
		if target < current {
			t.Fatalf("current=%d: raise target %d is BELOW current (lowering)", current, target)
		}
		// The only class we ever raise FROM is one strictly below the floor.
		if current >= supervisorPriorityFloorRank {
			t.Fatalf("current=%d: raised a process already at/above the floor %d", current, supervisorPriorityFloorRank)
		}
		// And we always land exactly on the floor.
		if target != supervisorPriorityFloorRank {
			t.Fatalf("current=%d: raise target %d != floor %d", current, target, supervisorPriorityFloorRank)
		}
	}
}

package api

import (
	"errors"
	"testing"
)

// TestPruneWorkspacePhases_SetSerenaPendingRemoval_OrderingAndFailureClear
// pins the exact call ordering PruneWorkspacePhases must follow for the
// unregister-resurrects-serena-intent fix (mcphub-register-intent REVISE
// round 2, BLOCKING 1):
//
//  1. SetSerenaPendingRemoval(true) BEFORE RemoveSerenaIntent — so a
//     reconcile a live supervisor's RemoveSerenaIntent nudges INSIDE this
//     same call cannot observe the registry row without the mark set.
//  2. On a RemoveSerenaIntent failure, SetSerenaPendingRemoval(false) runs
//     to clear the mark — so a retry (or the supervisor's own startup
//     self-heal) sees a normal orphan row again, not one permanently
//     skipped.
//  3. DeleteSerenaRow runs ONLY after a successful RemoveSerenaIntent, and
//     SetSerenaPendingRemoval is NOT called again on the success path (the
//     row is about to be deleted entirely; a redundant clear would be a
//     wasted registry write, not a correctness bug, but this test also
//     documents that steady-state contract).
func TestPruneWorkspacePhases_SetSerenaPendingRemoval_OrderingAndFailureClear(t *testing.T) {
	t.Run("success path: mark(true) then RemoveSerenaIntent then DeleteSerenaRow, no clear", func(t *testing.T) {
		var calls []string
		td := PruneWorkspaceTeardown{
			SetSerenaPendingRemoval: func(pending bool) error {
				if pending {
					calls = append(calls, "mark(true)")
				} else {
					calls = append(calls, "mark(false)")
				}
				return nil
			},
			RemoveSerenaIntent: func(string) (bool, error) {
				calls = append(calls, "removeSerenaIntent")
				return true, nil
			},
			DeleteSerenaRow: func() (int, error) {
				calls = append(calls, "deleteSerenaRow")
				return 1, nil
			},
		}
		report := &PruneReport{}
		if err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, report); err != nil {
			t.Fatalf("PruneWorkspacePhases: %v", err)
		}
		want := []string{"mark(true)", "removeSerenaIntent", "deleteSerenaRow"}
		if len(calls) != len(want) {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
		for i := range want {
			if calls[i] != want[i] {
				t.Errorf("calls[%d] = %q, want %q (full: %v)", i, calls[i], want[i], calls)
			}
		}
		if report.SerenaRemoved != 1 {
			t.Errorf("SerenaRemoved = %d, want 1", report.SerenaRemoved)
		}
	})

	t.Run("failure path: mark(true) then RemoveSerenaIntent fails then mark(false), DeleteSerenaRow never called", func(t *testing.T) {
		var calls []string
		teardownErr := errors.New("simulated live-supervisor reconcile failure")
		td := PruneWorkspaceTeardown{
			SetSerenaPendingRemoval: func(pending bool) error {
				if pending {
					calls = append(calls, "mark(true)")
				} else {
					calls = append(calls, "mark(false)")
				}
				return nil
			},
			RemoveSerenaIntent: func(string) (bool, error) {
				calls = append(calls, "removeSerenaIntent")
				return false, teardownErr
			},
			DeleteSerenaRow: func() (int, error) {
				calls = append(calls, "deleteSerenaRow")
				return 0, nil
			},
		}
		report := &PruneReport{}
		err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, report)
		if err == nil {
			t.Fatal("PruneWorkspacePhases must surface the RemoveSerenaIntent failure")
		}
		if !errors.Is(err, teardownErr) {
			t.Errorf("error does not wrap the underlying teardown error: %v", err)
		}
		want := []string{"mark(true)", "removeSerenaIntent", "mark(false)"}
		if len(calls) != len(want) {
			t.Fatalf("calls = %v, want %v (DeleteSerenaRow must NEVER run after a teardown failure)", calls, want)
		}
		for i := range want {
			if calls[i] != want[i] {
				t.Errorf("calls[%d] = %q, want %q (full: %v)", i, calls[i], want[i], calls)
			}
		}
	})
}

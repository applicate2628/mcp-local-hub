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

// TestPruneWorkspacePhases_SerenaRemovalFence_WrapsTheMarkedWindow pins the
// placement of the liveness fence, which is the whole basis of the repair's
// "is this teardown alive?" answer (serena_removal_fence.go):
//
//   - ACQUIRED BEFORE SetSerenaPendingRemoval(true). Acquiring it after would
//     leave an instant where the mark is set and the fence is free — the exact
//     shape the repair reads as reclaimable crash debris.
//   - RELEASED AFTER the row delete (and, on the failure path, after the mark is
//     cleared back to false). Releasing earlier would expose the same window
//     from the other end.
func TestPruneWorkspacePhases_SerenaRemovalFence_WrapsTheMarkedWindow(t *testing.T) {
	newTD := func(calls *[]string, removeErr error) PruneWorkspaceTeardown {
		return PruneWorkspaceTeardown{
			AcquireSerenaRemovalFence: func() (func(), error) {
				*calls = append(*calls, "fence.acquire")
				return func() { *calls = append(*calls, "fence.release") }, nil
			},
			SetSerenaPendingRemoval: func(pending bool) error {
				if pending {
					*calls = append(*calls, "mark(true)")
				} else {
					*calls = append(*calls, "mark(false)")
				}
				return nil
			},
			RemoveSerenaIntent: func(string) (bool, error) {
				*calls = append(*calls, "removeSerenaIntent")
				return removeErr == nil, removeErr
			},
			DeleteSerenaRow: func() (int, error) {
				*calls = append(*calls, "deleteSerenaRow")
				return 1, nil
			},
		}
	}
	assertOrder := func(t *testing.T, calls, want []string) {
		t.Helper()
		if len(calls) != len(want) {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
		for i := range want {
			if calls[i] != want[i] {
				t.Errorf("calls[%d] = %q, want %q (full: %v)", i, calls[i], want[i], calls)
			}
		}
	}

	t.Run("success path: fence wraps mark through delete", func(t *testing.T) {
		var calls []string
		if err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, newTD(&calls, nil), &PruneReport{}); err != nil {
			t.Fatalf("PruneWorkspacePhases: %v", err)
		}
		assertOrder(t, calls, []string{
			"fence.acquire", "mark(true)", "removeSerenaIntent", "deleteSerenaRow", "fence.release",
		})
	})

	t.Run("failure path: fence outlives the mark clear", func(t *testing.T) {
		var calls []string
		err := PruneWorkspacePhases("/ws", "/ws", nil, false, true,
			newTD(&calls, errors.New("simulated teardown failure")), &PruneReport{})
		if err == nil {
			t.Fatal("PruneWorkspacePhases must surface the RemoveSerenaIntent failure")
		}
		assertOrder(t, calls, []string{
			"fence.acquire", "mark(true)", "removeSerenaIntent", "mark(false)", "fence.release",
		})
	})

	t.Run("acquire failure aborts before any mutation", func(t *testing.T) {
		fenceErr := errors.New("simulated fence acquire failure")
		var calls []string
		td := newTD(&calls, nil)
		td.AcquireSerenaRemovalFence = func() (func(), error) {
			calls = append(calls, "fence.acquire")
			return nil, fenceErr
		}
		err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, &PruneReport{})
		if err == nil || !errors.Is(err, fenceErr) {
			t.Fatalf("err = %v, want a wrap of %v — proceeding unfenced would silently reinstate the race", err, fenceErr)
		}
		assertOrder(t, calls, []string{"fence.acquire"})
	})

	t.Run("nil fence seam is tolerated", func(t *testing.T) {
		var calls []string
		td := newTD(&calls, nil)
		td.AcquireSerenaRemovalFence = nil
		if err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, &PruneReport{}); err != nil {
			t.Fatalf("PruneWorkspacePhases with a nil fence seam: %v", err)
		}
		assertOrder(t, calls, []string{"mark(true)", "removeSerenaIntent", "deleteSerenaRow"})
	})
}

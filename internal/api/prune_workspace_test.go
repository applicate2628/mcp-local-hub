package api

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestPruneWorkspace_ReleaseOnlyDisarmsZeroRowRollbackAndPreservesResults(t *testing.T) {
	releaseErr := errors.New("registry release unconfirmed")
	rollbackCalls := 0
	report := &PruneReport{}
	td := PruneWorkspaceTeardown{
		LSPUnregister: func(string, []string) (*UnregisterReport, error) {
			return &UnregisterReport{Removed: []string{"lsp"}}, nil
		},
		AcquireSerenaRemovalFence: func() (func() error, error) {
			return func() error { return nil }, nil
		},
		BeginSerenaPendingRemoval: func(string) (func() error, error) {
			return func() error { rollbackCalls++; return nil }, nil
		},
		RemoveSerenaIntent: func(string) (bool, error) { return true, nil },
		DeleteSerenaRow: func() PruneSerenaDeleteResult {
			return PruneSerenaDeleteResult{Removed: 0, ReleaseErr: releaseErr}
		},
	}

	err := PruneWorkspacePhases("/ws", "/ws", nil, true, true, td, report)
	if !errors.Is(err, releaseErr) {
		t.Fatalf("error = %v, want release failure", err)
	}
	if rollbackCalls != 0 {
		t.Fatalf("rollback calls = %d, want 0 after committed zero-row delete", rollbackCalls)
	}
	if len(report.LSPRemoved) != 1 || report.SerenaRemoved != 0 {
		t.Fatalf("committed report = %+v, want retained LSP result and zero Serena removals", report)
	}
}

func TestPruneWorkspacePhases_AbsentMarkStopsBeforeIntentTeardown(t *testing.T) {
	var releaseCalls int
	td := PruneWorkspaceTeardown{
		AcquireSerenaRemovalFence: func() (func() error, error) {
			return func() error { releaseCalls++; return nil }, nil
		},
		PublishSerenaRemovalFenceGeneration: func() (string, error) {
			return "0123456789abcdef0123456789abcdef", nil
		},
		BeginSerenaPendingRemoval: func(string) (func() error, error) {
			// Deterministically model a concurrent unregister/register winning
			// after classification but before this registry-locked mark.
			return nil, ErrSerenaPendingRemovalTargetAbsent
		},
		RemoveSerenaIntent: func(string) (bool, error) {
			t.Fatal("RemoveSerenaIntent called without a row-owned pending-removal mark")
			return false, nil
		},
		DeleteSerenaRow: func() PruneSerenaDeleteResult {
			t.Fatal("DeleteSerenaRow called after an absent mark")
			return PruneSerenaDeleteResult{}
		},
	}
	err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, &PruneReport{})
	if !errors.Is(err, ErrSerenaPendingRemovalTargetAbsent) {
		t.Fatalf("err = %v, want target-absent sentinel", err)
	}
	if releaseCalls != 1 {
		t.Fatalf("fence release calls = %d, want 1", releaseCalls)
	}
}

func TestPruneWorkspace_MutationReleaseAndRollbackFailuresAllJoin(t *testing.T) {
	mutationErr := errors.New("row mutation")
	releaseErr := errors.New("registry release")
	rollbackErr := errors.New("tuple rollback")
	report := &PruneReport{}
	td := PruneWorkspaceTeardown{
		AcquireSerenaRemovalFence: func() (func() error, error) {
			return func() error { return nil }, nil
		},
		BeginSerenaPendingRemoval: func(string) (func() error, error) {
			return func() error { return rollbackErr }, nil
		},
		RemoveSerenaIntent: func(string) (bool, error) { return true, nil },
		DeleteSerenaRow: func() PruneSerenaDeleteResult {
			return PruneSerenaDeleteResult{Removed: 1, MutationErr: mutationErr, ReleaseErr: releaseErr}
		},
	}

	err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, report)
	for _, want := range []error{mutationErr, releaseErr, rollbackErr} {
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want cause %v", err, want)
		}
	}
	if report.SerenaRemoved != 1 {
		t.Fatalf("SerenaRemoved = %d, want committed result 1", report.SerenaRemoved)
	}
}

func TestPruneWorkspace_PostCommitDeleteErrorDisarmsRollbackAndPreservesAccounting(t *testing.T) {
	postCommitErr := errors.New("post-rename verification")
	rollbackCalls := 0
	report := &PruneReport{}
	td := PruneWorkspaceTeardown{
		AcquireSerenaRemovalFence: func() (func() error, error) { return func() error { return nil }, nil },
		BeginSerenaPendingRemoval: func(string) (func() error, error) {
			return func() error { rollbackCalls++; return nil }, nil
		},
		RemoveSerenaIntent: func(string) (bool, error) { return true, nil },
		DeleteSerenaRow: func() PruneSerenaDeleteResult {
			return PruneSerenaDeleteResult{Removed: 1, PostCommitErr: postCommitErr}
		},
	}

	err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, report)
	if !errors.Is(err, postCommitErr) {
		t.Fatalf("error = %v, want post-commit cause", err)
	}
	if rollbackCalls != 0 {
		t.Fatalf("rollback calls = %d, want 0 for committed deletion", rollbackCalls)
	}
	if report.SerenaRemoved != 1 {
		t.Fatalf("SerenaRemoved = %d, want 1", report.SerenaRemoved)
	}
}

func TestPruneWorkspacePhases_PendingRemovalTransactionOrdering(t *testing.T) {
	t.Run("commit-unknown mark error invokes returned rollback", func(t *testing.T) {
		markErr := errors.New("registry writer failed reopening after rename")
		rolledBack := false
		removed := false
		td := PruneWorkspaceTeardown{
			BeginSerenaPendingRemoval: func(string) (func() error, error) {
				return func() error { rolledBack = true; return nil }, markErr
			},
			RemoveSerenaIntent: func(string) (bool, error) {
				removed = true
				return true, nil
			},
		}
		err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, &PruneReport{})
		if !errors.Is(err, markErr) || !rolledBack || removed {
			t.Fatalf("err=%v rolledBack=%t removed=%t; want commit-unknown rollback before intent removal", err, rolledBack, removed)
		}
	})

	t.Run("success path discards rollback after row delete", func(t *testing.T) {
		var calls []string
		td := PruneWorkspaceTeardown{
			BeginSerenaPendingRemoval: func(string) (func() error, error) {
				calls = append(calls, "begin")
				return func() error { calls = append(calls, "rollback"); return nil }, nil
			},
			RemoveSerenaIntent: func(string) (bool, error) {
				calls = append(calls, "removeSerenaIntent")
				return true, nil
			},
			DeleteSerenaRow: func() PruneSerenaDeleteResult {
				calls = append(calls, "deleteSerenaRow")
				return PruneSerenaDeleteResult{Removed: 1}
			},
		}
		report := &PruneReport{}
		if err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, report); err != nil {
			t.Fatalf("PruneWorkspacePhases: %v", err)
		}
		want := []string{"begin", "removeSerenaIntent", "deleteSerenaRow"}
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

	t.Run("intent failure rolls the exact attempt back before fence release", func(t *testing.T) {
		var calls []string
		teardownErr := errors.New("simulated live-supervisor reconcile failure")
		td := PruneWorkspaceTeardown{
			BeginSerenaPendingRemoval: func(string) (func() error, error) {
				calls = append(calls, "begin")
				return func() error { calls = append(calls, "rollback"); return nil }, nil
			},
			RemoveSerenaIntent: func(string) (bool, error) {
				calls = append(calls, "removeSerenaIntent")
				return false, teardownErr
			},
			DeleteSerenaRow: func() PruneSerenaDeleteResult {
				calls = append(calls, "deleteSerenaRow")
				return PruneSerenaDeleteResult{}
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
		want := []string{"begin", "removeSerenaIntent", "rollback"}
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

func TestPruneWorkspacePhases_DeleteSerenaRowFailureRollsBackPendingRemovalTuple(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	deleteErr := errors.New("registry row-delete failure")

	t.Run("successful rollback preserves row-delete error", func(t *testing.T) {
		rollbackCalls := 0
		td := PruneWorkspaceTeardown{
			PublishSerenaRemovalFenceGeneration: func() (string, error) { return generation, nil },
			BeginSerenaPendingRemoval: func(gotGeneration string) (func() error, error) {
				if gotGeneration != generation {
					t.Fatalf("generation = %q, want %q", gotGeneration, generation)
				}
				return func() error { rollbackCalls++; return nil }, nil
			},
			RemoveSerenaIntent: func(string) (bool, error) { return true, nil },
			DeleteSerenaRow:    func() PruneSerenaDeleteResult { return PruneSerenaDeleteResult{MutationErr: deleteErr} },
		}

		err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, &PruneReport{})
		if !errors.Is(err, deleteErr) {
			t.Fatalf("err = %v, want row-delete cause", err)
		}
		if rollbackCalls != 1 {
			t.Fatalf("rollback calls = %d, want 1", rollbackCalls)
		}
	})

	t.Run("rollback failure joins both causes", func(t *testing.T) {
		rollbackErr := errors.New("pending tuple rollback failure")
		rollbackCalls := 0
		td := PruneWorkspaceTeardown{
			PublishSerenaRemovalFenceGeneration: func() (string, error) { return generation, nil },
			BeginSerenaPendingRemoval: func(string) (func() error, error) {
				return func() error { rollbackCalls++; return rollbackErr }, nil
			},
			RemoveSerenaIntent: func(string) (bool, error) { return true, nil },
			DeleteSerenaRow:    func() PruneSerenaDeleteResult { return PruneSerenaDeleteResult{MutationErr: deleteErr} },
		}

		err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, &PruneReport{})
		if !errors.Is(err, deleteErr) || !errors.Is(err, rollbackErr) {
			t.Fatalf("err = %v, want both row-delete and rollback causes", err)
		}
		if rollbackCalls != 1 {
			t.Fatalf("rollback calls = %d, want 1", rollbackCalls)
		}
	})
}

// TestPruneWorkspacePhases_SerenaRemovalFence_WrapsTheMarkedWindow pins the
// placement of the liveness fence, which is the whole basis of the repair's
// "is this teardown alive?" answer (serena_removal_fence.go):
//
//   - ACQUIRED BEFORE BeginSerenaPendingRemoval. Acquiring it after would
//     leave an instant where the mark is set and the fence is free — the exact
//     shape the repair reads as reclaimable crash debris.
//   - RELEASED AFTER the row delete or returned exact rollback reaches its
//     verdict. Releasing earlier would expose the same window from the other end.
func TestPruneWorkspacePhases_SerenaRemovalFence_WrapsTheMarkedWindow(t *testing.T) {
	newTD := func(calls *[]string, removeErr error) PruneWorkspaceTeardown {
		return PruneWorkspaceTeardown{
			AcquireSerenaRemovalFence: func() (func() error, error) {
				*calls = append(*calls, "fence.acquire")
				return func() error { *calls = append(*calls, "fence.release"); return nil }, nil
			},
			BeginSerenaPendingRemoval: func(string) (func() error, error) {
				*calls = append(*calls, "begin")
				return func() error { *calls = append(*calls, "rollback"); return nil }, nil
			},
			RemoveSerenaIntent: func(string) (bool, error) {
				*calls = append(*calls, "removeSerenaIntent")
				return removeErr == nil, removeErr
			},
			DeleteSerenaRow: func() PruneSerenaDeleteResult {
				*calls = append(*calls, "deleteSerenaRow")
				return PruneSerenaDeleteResult{Removed: 1}
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
			"fence.acquire", "begin", "removeSerenaIntent", "deleteSerenaRow", "fence.release",
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
			"fence.acquire", "begin", "removeSerenaIntent", "rollback", "fence.release",
		})
	})

	t.Run("acquire failure aborts before any mutation", func(t *testing.T) {
		fenceErr := errors.New("simulated fence acquire failure")
		var calls []string
		td := newTD(&calls, nil)
		td.AcquireSerenaRemovalFence = func() (func() error, error) {
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
		assertOrder(t, calls, []string{"begin", "removeSerenaIntent", "deleteSerenaRow"})
	})
}

func TestPruneWorkspacePhases_UnconfirmedFenceReleaseDowngradesTheTeardown(t *testing.T) {
	t.Run("committed mutation remains reported", func(t *testing.T) {
		dir := t.TempDir()
		const key = "abcd1234"
		unlockErr := errors.New("simulated UnlockFileEx failure")
		failingFenceUnlock(t, unlockErr)
		calls := []string{}
		report := &PruneReport{}
		td := PruneWorkspaceTeardown{
			AcquireSerenaRemovalFence: func() (func() error, error) {
				calls = append(calls, "fence.acquire")
				return AcquireSerenaRemovalFence(dir, key)
			},
			BeginSerenaPendingRemoval: func(string) (func() error, error) { return func() error { return nil }, nil },
			RemoveSerenaIntent: func(string) (bool, error) {
				calls = append(calls, "remove")
				return true, nil
			},
			DeleteSerenaRow: func() PruneSerenaDeleteResult {
				calls = append(calls, "delete")
				return PruneSerenaDeleteResult{Removed: 1}
			},
		}
		err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, report)
		if !errors.Is(err, ErrSerenaRemovalFenceReleaseFailed) || !errors.Is(err, unlockErr) {
			t.Fatalf("err = %v, want release sentinel and unlock cause", err)
		}
		if report.SerenaRemoved != 1 {
			t.Fatalf("report.SerenaRemoved = %d, want committed row deletion retained", report.SerenaRemoved)
		}
		if fmt.Sprint(calls) != "[fence.acquire remove delete]" {
			t.Fatalf("calls = %v, want acquire then mutations", calls)
		}
	})

	t.Run("release failure joins rather than replaces mutation failure", func(t *testing.T) {
		dir := t.TempDir()
		const key = "beefcafe"
		unlockErr := errors.New("simulated UnlockFileEx failure")
		teardownErr := errors.New("simulated supervisor teardown refusal")
		failingFenceUnlock(t, unlockErr)
		td := PruneWorkspaceTeardown{
			AcquireSerenaRemovalFence: func() (func() error, error) { return AcquireSerenaRemovalFence(dir, key) },
			BeginSerenaPendingRemoval: func(string) (func() error, error) { return func() error { return nil }, nil },
			RemoveSerenaIntent:        func(string) (bool, error) { return false, teardownErr },
			DeleteSerenaRow: func() PruneSerenaDeleteResult {
				t.Fatal("DeleteSerenaRow called after teardown failure")
				return PruneSerenaDeleteResult{}
			},
		}
		err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, &PruneReport{})
		if !errors.Is(err, teardownErr) || !errors.Is(err, ErrSerenaRemovalFenceReleaseFailed) || !errors.Is(err, unlockErr) {
			t.Fatalf("err = %v, want teardown and release causes", err)
		}
	})
}

// TestPruneWorkspacePhases_RemovalGenerationTransaction pins the ownership
// transaction that prevents an old free fence leaf from being mistaken for the
// current pending-removal mark: acquire -> publish sidecar -> mark tuple ->
// intent -> clear tuple/release on failure.
func TestPruneWorkspacePhases_RemovalGenerationTransaction(t *testing.T) {
	var calls []string
	teardownErr := errors.New("simulated intent removal failure")
	td := PruneWorkspaceTeardown{
		AcquireSerenaRemovalFence: func() (func() error, error) {
			calls = append(calls, "fence.acquire")
			return func() error { calls = append(calls, "fence.release"); return nil }, nil
		},
		PublishSerenaRemovalFenceGeneration: func() (string, error) {
			calls = append(calls, "generation.publish")
			return "0123456789abcdef0123456789abcdef", nil
		},
		BeginSerenaPendingRemoval: func(generation string) (func() error, error) {
			calls = append(calls, "begin("+generation+")")
			return func() error { calls = append(calls, "rollback"); return nil }, nil
		},
		RemoveSerenaIntent: func(string) (bool, error) {
			calls = append(calls, "removeSerenaIntent")
			return false, teardownErr
		},
	}
	err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, &PruneReport{})
	if !errors.Is(err, teardownErr) {
		t.Fatalf("err = %v, want wrap of %v", err, teardownErr)
	}
	assert := []string{
		"fence.acquire", "generation.publish", "begin(0123456789abcdef0123456789abcdef)",
		"removeSerenaIntent", "rollback", "fence.release",
	}
	if len(calls) != len(assert) {
		t.Fatalf("calls = %v, want %v", calls, assert)
	}
	for i := range assert {
		if calls[i] != assert[i] {
			t.Fatalf("calls[%d] = %q, want %q (full: %v)", i, calls[i], assert[i], calls)
		}
	}
}

func TestPruneWorkspacePhases_RemovalGenerationCompensationFailurePreservesBothCauses(t *testing.T) {
	teardownErr := errors.New("intent removal failed")
	clearErr := errors.New("registry compensation failed")
	const workspaceKey = "abcd1234"
	regPath := filepath.Join(t.TempDir(), "workspaces.yaml")
	reg := NewRegistry(regPath)
	if err := reg.PutSerena(WorkspaceEntry{WorkspaceKey: workspaceKey, WorkspacePath: "c:/ws/test", Language: SerenaLanguageSentinel, Backend: "serena"}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	td := PruneWorkspaceTeardown{
		AcquireSerenaRemovalFence: func() (func() error, error) {
			return AcquireSerenaRemovalFence(filepath.Dir(regPath), workspaceKey)
		},
		PublishSerenaRemovalFenceGeneration: func() (string, error) {
			return PublishSerenaRemovalFenceGeneration(filepath.Dir(regPath), workspaceKey)
		},
		BeginSerenaPendingRemoval: func(generation string) (func() error, error) {
			rollback, err := NewRegistry(regPath).BeginSerenaPendingRemoval(workspaceKey, "", generation)
			if rollback == nil || err != nil {
				return rollback, err
			}
			return func() error { return clearErr }, nil
		},
		RemoveSerenaIntent: func(string) (bool, error) { return false, teardownErr },
	}
	err := PruneWorkspacePhases("/ws", "/ws", nil, false, true, td, &PruneReport{})
	if !errors.Is(err, teardownErr) || !errors.Is(err, clearErr) {
		t.Fatalf("err = %v, want both teardown and compensation causes", err)
	}
	observation, observeErr := observeSerenaRemovalFence(filepath.Dir(regPath), workspaceKey)
	if observeErr != nil || observation.held {
		t.Fatalf("fence after compensation failure = %+v, err=%v; want released", observation, observeErr)
	}
	loaded := NewRegistry(regPath)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load after compensation failure: %v", err)
	}
	row, ok := loaded.GetSerena(workspaceKey)
	if !ok || !row.PendingSerenaRemoval || row.PendingSerenaRemovalAt.IsZero() || !validSerenaRemovalFenceGeneration(row.PendingSerenaRemovalGeneration) {
		t.Fatalf("failed clear did not leave the complete tuple observable: %+v", row)
	}
}

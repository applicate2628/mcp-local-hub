package api

// r34 P2 — PR #288 bot round-34 fix.
//
// RemoveSerenaSupervisorIntentForWorkspace treated a reconcile
// ErrSupervisorIPCUnavailable as a SUCCESSFUL teardown. But that error also
// fires when supervisor.lock has a LIVE owner whose IPC listener is merely
// wedged: that live supervisor still tracks the now-removed descriptor in
// memory, so its reaper keeps or respawns the orphaned Serena daemon while the
// caller proceeds to delete the registry row. On retry there is no row left to
// drive the paired teardown, so the daemon is a permanent orphan.
//
// The fix mirrors the r33 Lane A demoteIPCUnavailableWhenOwnerAlive pattern:
// on ErrSupervisorIPCUnavailable, probe the flock-authoritative lock owner via
// the installSupervisorRunningProbeFn + DaemonStateDir() seam. Owner ALIVE
// (or any probe error) => restore the descriptor + return an error naming the
// wedged-supervisor recovery. NO live owner => keep the benign success (nothing
// runs to respawn the daemon, the on-disk removal is durable).
//
// All cases are hermetic: SetDaemonStateRootForTest redirects every state
// read/write to a fresh temp dir, the reconcile + probe paths go through their
// package seams (registerSupervisorReconcileFn, installSupervisorRunningProbeFn),
// and nothing touches the live host %LOCALAPPDATA%\mcp-local-hub\, the real
// scheduler, real IPC, or any real port.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

// seedSerenaTeardownIntent writes a supervisor-intent.json under the redirected
// state dir carrying exactly one Serena descriptor for workspacePath, so the
// removeSupervisorIntentDescriptorForTask inside
// RemoveSerenaSupervisorIntentForWorkspace removes a real row (returning a
// non-nil restore closure + removed=true) and the reconcile branch is reached.
// It returns the canonical Serena task name for that workspace.
func seedSerenaTeardownIntent(t *testing.T, stateDir, workspacePath string) string {
	t.Helper()
	taskName := SerenaTaskNameForWorkspace(workspacePath)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: taskName, Server: "serena", Port: 19150},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed WriteSupervisorIntent: %v", err)
	}
	return taskName
}

// serenaTeardownDescriptorPresent reports whether the Serena descriptor for
// taskName is currently on disk under the redirected state dir.
func serenaTeardownDescriptorPresent(t *testing.T, stateDir, taskName string) bool {
	t.Helper()
	got, err := ReadSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if got == nil {
		return false
	}
	for _, d := range got.Daemons {
		if d.TaskName == taskName {
			return true
		}
	}
	return false
}

// TestRemoveSerenaSupervisorIntent_OwnerAliveIPCUnavailable_RestoresAndFails is
// the r34 P2 core. Reconcile is mapped to ErrSupervisorIPCUnavailable AND the
// flock-authoritative probe reports a LIVE lock owner (a wedged-IPC supervisor).
// RemoveSerenaSupervisorIntentForWorkspace must RESTORE the descriptor and
// return an error — NOT a silent success — because the live supervisor still
// tracks the daemon in memory and would orphan it.
//
// Pre-fix falsifying property: the pre-fix code fell through to `return true,
// nil` on ErrSupervisorIPCUnavailable regardless of owner liveness, so the
// descriptor stayed REMOVED and err was nil. Asserting err != nil AND the
// descriptor is back on disk fails against the pre-fix code.
func TestRemoveSerenaSupervisorIntent_OwnerAliveIPCUnavailable_RestoresAndFails(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	ws := filepath.Join(stateDir, "ws-owner-alive")
	taskName := seedSerenaTeardownIntent(t, stateDir, ws)

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}
	t.Cleanup(func() { registerSupervisorReconcileFn = origReconcile })

	probeCalls := 0
	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(sd string) (bool, int, error) {
		probeCalls++
		if sd == "" {
			t.Fatal("serena-teardown owner probe received empty stateDir")
		}
		return true, 4242, nil // lock owner ALIVE, IPC wedged
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	removed, err := NewAPI().RemoveSerenaSupervisorIntentForWorkspace(ws)
	if probeCalls != 1 {
		t.Fatalf("live-owner probe calls = %d, want 1 (the teardown must probe the lock owner on IPC-unavailable)", probeCalls)
	}
	if err == nil {
		t.Fatal("RemoveSerenaSupervisorIntentForWorkspace returned nil error for owner-alive + IPC-unavailable; a live wedged supervisor still tracks the daemon, so the teardown must fail and the caller must retry")
	}
	if !removed {
		t.Fatalf("removed = %v, want true (the descriptor was removed before the failed reconcile)", removed)
	}
	if !serenaTeardownDescriptorPresent(t, stateDir, taskName) {
		t.Fatalf("Serena descriptor %q is gone after an owner-alive IPC-unavailable failure; it must be RESTORED so the unregister is reversible", taskName)
	}
}

// TestRemoveSerenaSupervisorIntent_NoOwnerIPCUnavailable_SucceedsRemoved is the
// r34 P2 negative control: reconcile mapped to ErrSupervisorIPCUnavailable AND
// the probe reporting NO live owner. The teardown is durable (nothing runs to
// respawn the daemon), so it returns (true, nil) and the descriptor stays
// removed. This case must keep the pre-fix benign-success behavior.
func TestRemoveSerenaSupervisorIntent_NoOwnerIPCUnavailable_SucceedsRemoved(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	ws := filepath.Join(stateDir, "ws-no-owner")
	taskName := seedSerenaTeardownIntent(t, stateDir, ws)

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}
	t.Cleanup(func() { registerSupervisorReconcileFn = origReconcile })

	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(string) (bool, int, error) {
		return false, 0, nil // NO live owner — IPC really is unavailable
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	removed, err := NewAPI().RemoveSerenaSupervisorIntentForWorkspace(ws)
	if err != nil {
		t.Fatalf("RemoveSerenaSupervisorIntentForWorkspace returned %v for no-owner + IPC-unavailable; want nil (nothing respawns the daemon, the on-disk removal is durable)", err)
	}
	if !removed {
		t.Fatalf("removed = %v, want true", removed)
	}
	if serenaTeardownDescriptorPresent(t, stateDir, taskName) {
		t.Fatalf("Serena descriptor %q is still on disk after a no-owner success; the durable removal must stand", taskName)
	}
}

// TestRemoveSerenaSupervisorIntent_ProbeError_FailsClosed asserts the
// fail-closed posture: an ErrSupervisorIPCUnavailable reconcile combined with a
// probe ERROR (owner liveness unknown) must restore the descriptor + return an
// error, because a live wedged supervisor is the dangerous case and the
// unknown must be treated as alive.
func TestRemoveSerenaSupervisorIntent_ProbeError_FailsClosed(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	ws := filepath.Join(stateDir, "ws-probe-error")
	taskName := seedSerenaTeardownIntent(t, stateDir, ws)

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}
	t.Cleanup(func() { registerSupervisorReconcileFn = origReconcile })

	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(string) (bool, int, error) {
		return false, 0, errors.New("probe boom")
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	removed, err := NewAPI().RemoveSerenaSupervisorIntentForWorkspace(ws)
	if err == nil {
		t.Fatal("RemoveSerenaSupervisorIntentForWorkspace returned nil error when the owner probe failed; unknown owner liveness must fail closed (treat as alive)")
	}
	if !removed {
		t.Fatalf("removed = %v, want true", removed)
	}
	if !serenaTeardownDescriptorPresent(t, stateDir, taskName) {
		t.Fatalf("Serena descriptor %q is gone after a probe-error fail-closed; it must be RESTORED", taskName)
	}
}

// TestRemoveSerenaSupervisorIntent_LiveSupervisorError_RestoresAndFails guards
// the UNCHANGED non-IPCUnavailable branch: a reconcile error that is NOT
// ErrSupervisorIPCUnavailable (a reachable-but-failing live supervisor) must
// still restore the descriptor + return an error WITHOUT consulting the
// lock-owner probe (the supervisor already answered, so it is provably alive).
func TestRemoveSerenaSupervisorIntent_LiveSupervisorError_RestoresAndFails(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	ws := filepath.Join(stateDir, "ws-live-error")
	taskName := seedSerenaTeardownIntent(t, stateDir, ws)

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, context.DeadlineExceeded
	}
	t.Cleanup(func() { registerSupervisorReconcileFn = origReconcile })

	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(string) (bool, int, error) {
		t.Fatal("lock-owner probe must NOT run for a non-IPCUnavailable reconcile error; the live supervisor already answered")
		return false, 0, nil
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	removed, err := NewAPI().RemoveSerenaSupervisorIntentForWorkspace(ws)
	if err == nil {
		t.Fatal("RemoveSerenaSupervisorIntentForWorkspace returned nil error for a live-supervisor reconcile failure; the teardown must fail and restore")
	}
	if !removed {
		t.Fatalf("removed = %v, want true", removed)
	}
	if !serenaTeardownDescriptorPresent(t, stateDir, taskName) {
		t.Fatalf("Serena descriptor %q is gone after a live-supervisor error; it must be RESTORED", taskName)
	}
}

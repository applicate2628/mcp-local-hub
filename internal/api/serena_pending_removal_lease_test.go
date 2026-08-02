// Package api — crash-recovery tests for the PendingSerenaRemoval LEASE
// (bot PR #590 P2, "Recover stale pending-removal markers after interruption").
//
// TestRepairSerenaIntentFromRegistry_PendingSerenaRemoval_SkippedNotReappended
// (serena_intent_repair_test.go, case 12) pins the FRESH-mark skip: a row an
// unregister is actively tearing down must not be resurrected. These tests pin
// the other half — an unregister that never reached its DeleteSerenaRow (the
// process was killed, or that delete's registry write failed) leaves the mark
// set with nobody left to clear it, and an unconditional skip stranded the row
// permanently: absent from supervisor-intent.json, still routed to by the
// serena resolver, and still rejected by `mcphub workspace register` as already
// registered.
//
// The interrupted state is constructed DIRECTLY (mark written, descriptor
// absent, registry row left in place) instead of by racing a real teardown, so
// every assertion here is deterministic.
package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRepairSerenaIntentFromRegistry_InterruptedUnregister_FreshLeaseRecovered
// proves the per-workspace fence, not a wall-clock lease, owns liveness. A
// process that dies just after writing a fresh pending-removal mark releases its
// fence immediately; a single event-driven repair pass must recover it rather
// than wait for a pass that may never occur.
func TestRepairSerenaIntentFromRegistry_InterruptedUnregister_FreshLeaseRecovered(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	healthyPath, healthyPort := liveWorkspace(t), 9150
	strandedPath, strandedPort := liveWorkspace(t), 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	strandedKey := seedSerenaRegistryRow(t, regPath, strandedPath, strandedPort)
	seedPendingRemovalMark(t, regPath, strandedKey, time.Now().UTC())
	// Model a CURRENT fence-capable writer that died: acquisition created the
	// leaf before the mark, and release models the kernel dropping its lock.
	releaseFence, err := AcquireSerenaRemovalFence(filepath.Dir(regPath), strandedKey)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFence: %v", err)
	}
	releaseFence()
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})

	result, err := NewAPI().RepairSerenaIntentFromRegistry(mustStateDir(t))
	if err != nil {
		t.Fatalf("RepairSerenaIntentFromRegistry: %v", err)
	}
	if result.Outcome != SerenaIntentRepairOutcomeCompleted || result.Repaired != 1 {
		t.Fatalf("result = %+v, want completed repair of the fresh crashed teardown", result)
	}
	if got := readIntent(t, intentPath); !got.HasSpecBearingSerenaDaemonForWorkspaceKey(strandedKey) {
		t.Fatalf("fresh stranded key %q was not materialized", strandedKey)
	}
	if row := readSerenaRowFresh(t, regPath, strandedKey); row.PendingSerenaRemoval || !row.PendingSerenaRemovalAt.IsZero() {
		t.Fatalf("fresh recovered row still carries pending-removal state: %+v", row)
	}
}

// TestRepairSerenaIntentFromRegistry_FenceProbeErrorIsIncomplete proves an
// unobservable fence remains fail-closed and is propagated to callers as a
// retryable incomplete outcome, never a completed no-drift certificate.
func TestRepairSerenaIntentFromRegistry_FenceProbeErrorIsIncomplete(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	healthyPath, healthyPort := liveWorkspace(t), 9150
	pendingPath, pendingPort := liveWorkspace(t), 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	pendingKey := seedSerenaRegistryRow(t, regPath, pendingPath, pendingPort)
	seedPendingRemovalMark(t, regPath, pendingKey, time.Now().UTC())
	seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})

	origFenceProbe := observeSerenaRemovalFenceFn
	observeSerenaRemovalFenceFn = func(string, string) (serenaRemovalFenceObservation, error) {
		return serenaRemovalFenceObservation{}, errors.New("injected fence probe failure")
	}
	t.Cleanup(func() { observeSerenaRemovalFenceFn = origFenceProbe })

	result, err := NewAPI().RepairSerenaIntentFromRegistry(mustStateDir(t))
	if err != nil {
		t.Fatalf("fence probe failure must remain a typed incomplete result, got error: %v", err)
	}
	if result.Outcome != SerenaIntentRepairOutcomeSkippedRemovalFenceProbe {
		t.Fatalf("outcome = %q, want %q", result.Outcome, SerenaIntentRepairOutcomeSkippedRemovalFenceProbe)
	}
	if result.Repaired != 0 {
		t.Fatalf("repaired = %d, want 0 while liveness is unknown", result.Repaired)
	}
	if row := readSerenaRowFresh(t, regPath, pendingKey); !row.PendingSerenaRemoval {
		t.Fatalf("probe-error row was reclaimed despite unknown liveness: %+v", row)
	}
}

// seedPendingRemovalMark stamps the serena row for key with
// PendingSerenaRemoval=true and PendingSerenaRemovalAt=stampedAt — the exact
// on-disk shape SetSerenaPendingRemoval(true) leaves behind, with the lease age
// under the test's control.
func seedPendingRemovalMark(t *testing.T, regPath, key string, stampedAt time.Time) {
	t.Helper()
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		t.Fatalf("lock registry: %v", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	e, ok := reg.GetSerena(key)
	if !ok {
		t.Fatalf("precondition: seeded row %q not found", key)
	}
	e.PendingSerenaRemoval = true
	e.PendingSerenaRemovalAt = stampedAt
	reg.Put(e)
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry with pending mark: %v", err)
	}
}

// readSerenaRowFresh re-reads one serena row from disk.
func readSerenaRowFresh(t *testing.T, regPath, key string) WorkspaceEntry {
	t.Helper()
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	e, ok := reg.GetSerena(key)
	if !ok {
		t.Fatalf("serena row %q not found", key)
	}
	return e
}

func TestRepairSerenaIntentFromRegistry_InterruptedUnregister_ExpiredLeaseRecovered(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	strandedPath, strandedPort := liveWorkspace(t), 9151

	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	strandedKey := seedSerenaRegistryRow(t, regPath, strandedPath, strandedPort)

	// The interrupted state: mark stamped well past the lease, registry row
	// still present, matching intent descriptor already gone.
	seedPendingRemovalMark(t, regPath, strandedKey,
		time.Now().UTC().Add(-serenaPendingRemovalLeaseTTL-time.Minute))
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})

	repaired, deferred, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1 (an expired pending-removal lease must be reclassified as the crash-orphan it is, not skipped forever)", repaired)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want none", deferred)
	}
	if got := readIntent(t, intentPath); !got.HasSpecBearingSerenaDaemonForWorkspaceKey(strandedKey) {
		t.Errorf("stranded key %q was not materialized; the workspace stays unusable and register keeps rejecting it as already registered", strandedKey)
	}

	// The stale mark itself is cleared, so the registry stops asserting a
	// teardown that is not happening.
	row := readSerenaRowFresh(t, regPath, strandedKey)
	if row.PendingSerenaRemoval {
		t.Errorf("PendingSerenaRemoval still true after recovery; the stale mark must be cleared")
	}
	if !row.PendingSerenaRemovalAt.IsZero() {
		t.Errorf("PendingSerenaRemovalAt = %v after recovery, want zero", row.PendingSerenaRemovalAt)
	}

	// Second pass is a clean no-op — recovery must not re-append on every boot.
	repaired2, _, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("second pass: unexpected error: %v", err)
	}
	if repaired2 != 0 {
		t.Errorf("second pass repaired = %d, want 0", repaired2)
	}
}

// A mark with NO stamp at all (an older binary's write, or a hand edit) cannot
// be aged, so it must fail toward recovery rather than toward the permanent
// skip the lease exists to end.
func TestRepairSerenaIntentFromRegistry_PendingRemovalWithoutStamp_Recovered(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	strandedPath, strandedPort := liveWorkspace(t), 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	strandedKey := seedSerenaRegistryRow(t, regPath, strandedPath, strandedPort)
	seedPendingRemovalMark(t, regPath, strandedKey, time.Time{})

	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})

	repaired, _, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1 (an unstamped mark cannot be aged and must not pin the row forever)", repaired)
	}
	if got := readIntent(t, intentPath); !got.HasSpecBearingSerenaDaemonForWorkspaceKey(strandedKey) {
		t.Errorf("stranded key %q was not materialized", strandedKey)
	}
}

// The preview must classify an expired lease EXACTLY as the commit path does
// (the two modes may never disagree on WHICH rows are orphaned) while still
// writing nothing at all — neither the intent nor the mark clear.
func TestPreviewSerenaIntentRepairFromRegistry_ExpiredLease_PreviewsWithoutClearing(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	strandedPath, strandedPort := liveWorkspace(t), 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	strandedKey := seedSerenaRegistryRow(t, regPath, strandedPath, strandedPort)
	seedPendingRemovalMark(t, regPath, strandedKey,
		time.Now().UTC().Add(-serenaPendingRemovalLeaseTTL-time.Minute))

	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent before: %v", err)
	}

	wouldRepair, _, err := previewSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wouldRepair != 1 {
		t.Errorf("wouldRepair = %d, want 1 (preview must classify the expired lease exactly as apply does)", wouldRepair)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("preview WROTE supervisor-intent.json:\nbefore=%s\nafter=%s", before, after)
	}
	if row := readSerenaRowFresh(t, regPath, strandedKey); !row.PendingSerenaRemoval {
		t.Errorf("preview cleared the pending-removal mark; a dry run must never mutate the registry")
	}
}

// A FRESH lease is still honored — the recovery must not shorten the window a
// genuinely in-flight unregister depends on.
func TestPendingSerenaRemovalLeaseFresh_Boundaries(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		stampedAt time.Time
		want      bool
	}{
		{"just stamped", now, true},
		{"mid-lease", now.Add(-serenaPendingRemovalLeaseTTL / 2), true},
		{"one second short of expiry", now.Add(-serenaPendingRemovalLeaseTTL + time.Second), true},
		{"exactly at expiry", now.Add(-serenaPendingRemovalLeaseTTL), false},
		{"long expired", now.Add(-24 * time.Hour), false},
		{"unstamped", time.Time{}, false},
		{"slightly ahead (clock skew)", now.Add(time.Minute), true},
		{"far ahead (clock stepped forward then back)", now.Add(24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pendingSerenaRemovalLeaseFresh(tc.stampedAt, now); got != tc.want {
				t.Errorf("pendingSerenaRemovalLeaseFresh(%v, %v) = %v, want %v", tc.stampedAt, now, got, tc.want)
			}
		})
	}
}

// SetSerenaPendingRemoval owns the stamp: pending=true records the lease start,
// pending=false clears BOTH the flag and the stamp so no expired lease outlives
// the mark it belonged to.
func TestRegistry_SetSerenaPendingRemoval_StampsAndClearsLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	reg := NewRegistry(path)
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "c:/ws/foo",
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9500,
		TaskName:      "mcp-local-hub-serena-abcd1234",
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	before := time.Now().UTC().Add(-time.Second)
	if err := reg.SetSerenaPendingRemoval("abcd1234", "", true); err != nil {
		t.Fatalf("SetSerenaPendingRemoval(true): %v", err)
	}
	got := readSerenaRowFresh(t, path, "abcd1234")
	if !got.PendingSerenaRemoval {
		t.Fatal("PendingSerenaRemoval = false after set")
	}
	if got.PendingSerenaRemovalAt.Before(before) {
		t.Errorf("PendingSerenaRemovalAt = %v, want a stamp at or after %v", got.PendingSerenaRemovalAt, before)
	}

	if err := reg.SetSerenaPendingRemoval("abcd1234", "", false); err != nil {
		t.Fatalf("SetSerenaPendingRemoval(false): %v", err)
	}
	cleared := readSerenaRowFresh(t, path, "abcd1234")
	if cleared.PendingSerenaRemoval {
		t.Error("PendingSerenaRemoval = true after clear")
	}
	if !cleared.PendingSerenaRemovalAt.IsZero() {
		t.Errorf("PendingSerenaRemovalAt = %v after clear, want zero (a stamp outliving its mark would age into a phantom expiry)", cleared.PendingSerenaRemovalAt)
	}
}

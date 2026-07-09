package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

// ---------------------------------------------------------------------------
// Phase 4-E2 ORDERED DELETION: the merge deletes daemon-intent.json ONLY after
// the sub-block carries its active stops, and is idempotent.
// ---------------------------------------------------------------------------

// After the merge, daemon-intent.json (+ its .lock) is deleted, the active
// stop survives in the sub-block, and a re-enabled/expired stop is dropped.
func TestRunDaemonIntentCollapse_E2_DeletesFileAfterMerge(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	// Seed a legacy daemon-intent.json (via the legacy writer) with one active
	// stop + one expired stop.
	seedDaemonIntent(t, `\mcp-local-hub-paper-search-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-1 * time.Hour),
	})
	seedDaemonIntent(t, `\mcp-local-hub-expired-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-48 * time.Hour),
	})

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.Wrote {
		t.Fatalf("expected the merge to write (active stop present); res=%+v", res)
	}
	if !res.DeletedLegacyFile {
		t.Fatalf("expected daemon-intent.json deleted after merge; res=%+v", res)
	}

	// daemon-intent.json is gone.
	if _, statErr := os.Stat(filepath.Join(stateDir, intentFileLeaf)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-intent.json should be deleted (err=%v)", statErr)
	}

	// The active stop survives in the sub-block; the expired one was dropped.
	stops := readSupervisorStopsFromDisk(t, stateDir)
	if _, ok := stops[`\mcp-local-hub-paper-search-default`]; !ok {
		t.Fatalf("active stop lost from sub-block: %+v", stops)
	}
	if _, ok := stops[`\mcp-local-hub-expired-default`]; ok {
		t.Fatalf("expired stop should not be carried: %+v", stops)
	}
}

// The deletion is idempotent: a second run finds the file gone and is a no-op.
func TestRunDaemonIntentCollapse_E2_DeletionIdempotent(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	seedDaemonIntent(t, `\mcp-local-hub-time-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(-1 * time.Hour),
	})

	first, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !first.DeletedLegacyFile {
		t.Fatalf("first run should delete the file; res=%+v", first)
	}

	// Second run: file already gone → no-op delete, no write.
	second, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.DeletedLegacyFile {
		t.Fatalf("second run must NOT report a delete (file already gone); res=%+v", second)
	}
	if second.Wrote {
		t.Fatalf("second run must NOT write (idempotent no-op); res=%+v", second)
	}
	// Stop still preserved in the sub-block across the idempotent re-run.
	if _, ok := readSupervisorStopsFromDisk(t, stateDir)[`\mcp-local-hub-time-default`]; !ok {
		t.Fatalf("stop lost across idempotent re-run")
	}
}

// Ordered: a daemon-intent.json with an active stop that the sub-block ALREADY
// carries (E1 already merged it) is deleted on the next boot. The present
// sub-block stop is the durable watermark; collapse must not write a redundant
// legacy-stop watermark before the delete.
func TestRunDaemonIntentCollapse_E2_DeletesWhenSubBlockAlreadyMerged(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-paper-search-default`
	di := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Hour)}

	// Pre-seed the sub-block with the stop already present (simulate the E1
	// merge having run), AND a leftover daemon-intent.json with the same stop.
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Stops:   map[string]DaemonIntent{task: di},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	seedDaemonIntent(t, task, di)

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if res.Wrote {
		t.Fatalf("expected no write when present sub-block stop already accounts for legacy stop; res=%+v", res)
	}
	if !res.DeletedLegacyFile {
		t.Fatalf("expected delete even on no-delta path (crash-recovery); res=%+v", res)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, intentFileLeaf)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-intent.json should be deleted on no-delta path (err=%v)", statErr)
	}
	assertNoLegacyStopWatermarkForTask(t, stateDir, task)
}

func TestRunDaemonIntentCollapse_E2_NewerActiveLegacyStopUpdatesSubBlockAndDeletes(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-paper-search-default`
	oldStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-2 * time.Hour)}
	newStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(-time.Minute)}

	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{
		Version: 1,
		Stops:   map[string]DaemonIntent{task: oldStop},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	seedDaemonIntent(t, task, newStop)

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.Changed || !res.Wrote || !res.DeletedLegacyFile {
		t.Fatalf("newer active legacy stop should update, write, and delete legacy file; res=%+v", res)
	}
	if len(res.Entries) != 1 || res.Entries[0].TaskName != task || res.Entries[0].Action != MergeStopUpdated {
		t.Fatalf("collapse entries = %+v, want one update for %s", res.Entries, task)
	}
	if got := readSupervisorStopsFromDisk(t, stateDir)[task]; got.Desired != newStop.Desired || got.Reason != newStop.Reason || !got.UpdatedAt.Equal(newStop.UpdatedAt) {
		t.Fatalf("updated sub-block stop = %+v, want %+v", got, newStop)
	}
	assertNoLegacyStopWatermarkForTask(t, stateDir, task)
	if _, statErr := os.Stat(filepath.Join(stateDir, intentFileLeaf)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-intent.json should be deleted after exact updated record is persisted (err=%v)", statErr)
	}
}

// REFUSE-DELETE safety: deleteLegacyDaemonIntentIfMerged must NOT delete when
// an active stop in daemon-intent.json is NOT present in the sub-block (a stop
// would be lost). This is the "never delete before the stops persist" gate.
func TestDeleteLegacyDaemonIntentIfMerged_RefusesWhenActiveStopMissingFromSubBlock(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	supPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	daemonPath := filepath.Join(stateDir, intentFileLeaf)

	// Sub-block is EMPTY (the merge has NOT yet captured the stop).
	if err := WriteSupervisorIntent(supPath, &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	// daemon-intent.json has an ACTIVE stop not yet in the sub-block.
	daemonIntent := &DaemonIntentFile{Tasks: map[string]DaemonIntent{
		`\mcp-local-hub-paper-search-default`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Hour)},
	}}
	if err := os.WriteFile(daemonPath, []byte(`{"version":1,"tasks":{"\\mcp-local-hub-paper-search-default":{"desired":"stopped","reason":"user-stop","updated_at":"2026-06-11T11:00:00Z"}}}`), 0o600); err != nil {
		t.Fatalf("seed daemon-intent.json: %v", err)
	}

	deleted, err := deleteLegacyDaemonIntentIfMerged(stateDir, supPath, daemonPath, daemonIntent, now)
	if err != nil {
		t.Fatalf("deleteLegacyDaemonIntentIfMerged: %v", err)
	}
	if deleted {
		t.Fatalf("must REFUSE delete: active stop not yet in sub-block")
	}
	if _, statErr := os.Stat(daemonPath); statErr != nil {
		t.Fatalf("daemon-intent.json must be retained for retry (err=%v)", statErr)
	}
}

func TestDeleteLegacyDaemonIntentIfMerged_AllowsMatchingLegacyStopWatermark(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)
	supPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	daemonPath := filepath.Join(stateDir, intentFileLeaf)
	task := `\mcp-local-hub-paper-search-default`
	stop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Minute)}

	writeRawSupervisorIntentForCollapseTest(t, stateDir, nil, map[string]DaemonIntent{task: stop})
	daemonIntent := &DaemonIntentFile{Tasks: map[string]DaemonIntent{task: stop}}
	seedDaemonIntent(t, task, stop)

	deleted, err := deleteLegacyDaemonIntentIfMerged(stateDir, supPath, daemonPath, daemonIntent, now)
	if err != nil {
		t.Fatalf("deleteLegacyDaemonIntentIfMerged: %v", err)
	}
	if !deleted {
		t.Fatalf("matching legacy-stop watermark should permit deleting the accounted stale legacy file")
	}
	if _, statErr := os.Stat(daemonPath); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-intent.json should be deleted when active stop is accounted by watermark (err=%v)", statErr)
	}
}

// A CORRUPT daemon-intent.json fails the merge CLOSED — the file is NOT deleted
// (forensic state preserved) and no stop is silently lost.
func TestRunDaemonIntentCollapse_E2_CorruptDaemonIntentFailsClosedNoDelete(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	supPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(supPath, &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	daemonPath := filepath.Join(stateDir, intentFileLeaf)
	if err := os.WriteFile(daemonPath, []byte(`{ not valid json `), 0o600); err != nil {
		t.Fatalf("seed corrupt daemon-intent.json: %v", err)
	}

	_, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: time.Now().UTC()})
	if err == nil {
		t.Fatalf("expected fail-closed error on corrupt daemon-intent.json, got nil")
	}
	// The corrupt file must NOT have been deleted by the merge owner (the
	// fail-closed path returns before the delete). It may be quarantined by a
	// reader, but the merge owner itself must not have removed it without
	// migrating.
	if _, statErr := os.Stat(daemonPath); os.IsNotExist(statErr) {
		t.Fatalf("corrupt daemon-intent.json should NOT be deleted by the merge owner")
	}
}

// DryRun (--check) NEVER deletes.
func TestRunDaemonIntentCollapse_E2_DryRunDoesNotDelete(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	seedDaemonIntent(t, `\mcp-local-hub-x`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Hour),
	})

	res, err := CheckDaemonIntentCollapse(stateDir, now)
	if err != nil {
		t.Fatalf("CheckDaemonIntentCollapse: %v", err)
	}
	if res.DeletedLegacyFile || res.Wrote {
		t.Fatalf("dry-run must not write or delete; res=%+v", res)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, intentFileLeaf)); statErr != nil {
		t.Fatalf("dry-run must leave daemon-intent.json intact (err=%v)", statErr)
	}
}

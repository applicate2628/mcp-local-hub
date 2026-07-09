package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

// readSupervisorIntentFromDisk loads the supervisor-intent.json under the test
// state dir for assertions.
func readSupervisorIntentFromDisk(t *testing.T, stateDir string) *SupervisorIntentFile {
	t.Helper()
	got, err := ReadSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor-intent.json: %v", err)
	}
	return got
}

// ---------------------------------------------------------------------------
// E2 WRITE PATH: the stop lands in the supervisor-intent.json stops sub-block,
// NOT daemon-intent.json.
// ---------------------------------------------------------------------------

func TestWriteStopIntent_LandsInSubBlock_NotDaemonIntentFile(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	task := `\mcp-local-hub-paper-search-default`
	if err := NewAPI().WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	}, "tester"); err != nil {
		t.Fatalf("WriteStopIntent: %v", err)
	}

	// The stop must be in the sub-block.
	sup := readSupervisorIntentFromDisk(t, stateDir)
	di, ok := sup.Stops[task]
	if !ok {
		t.Fatalf("stop not found in supervisor-intent.json stops sub-block; stops=%+v", sup.Stops)
	}
	if di.Desired != IntentDesiredStopped || di.Reason != IntentReasonUserStop || !di.UpdatedAt.Equal(now) {
		t.Fatalf("sub-block stop mismatch: got %+v", di)
	}

	// daemon-intent.json must NOT have been written by the sub-block writer.
	if _, err := os.Stat(filepath.Join(stateDir, intentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("daemon-intent.json should not exist after WriteStopIntent (err=%v)", err)
	}
}

// A bare task name is normalized to the canonical leading-backslash key, same
// as WriteDaemonIntent.
func TestWriteStopIntent_NormalizesLeadingBackslash(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	if err := NewAPI().WriteStopIntent("mcp-local-hub-bare", DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("WriteStopIntent (bare): %v", err)
	}
	sup := readSupervisorIntentFromDisk(t, stateDir)
	if _, ok := sup.Stops[`\mcp-local-hub-bare`]; !ok {
		t.Fatalf("expected canonical key \\mcp-local-hub-bare in sub-block, got keys %v", keysOf(sup.Stops))
	}
}

// A Desired=running write (re-enable) DROPS a prior stop from the sub-block.
func TestWriteStopIntent_RunningDropsPriorStop(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-time-default`
	now := time.Now().UTC()
	stop := DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	}
	if err := NewAPI().WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	}, "tester"); err != nil {
		t.Fatalf("seed stop: %v", err)
	}
	if _, ok := readSupervisorIntentFromDisk(t, stateDir).Stops[task]; !ok {
		t.Fatalf("precondition: stop should be set")
	}

	// Re-enable via Desired=running → must remove the entry.
	if err := NewAPI().WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredRunning, Reason: IntentReasonInstall, UpdatedAt: now.Add(time.Minute),
	}, "tester"); err != nil {
		t.Fatalf("re-enable WriteStopIntent: %v", err)
	}
	sup := readSupervisorIntentFromDisk(t, stateDir)
	if _, ok := sup.Stops[task]; ok {
		t.Fatalf("expected stop dropped after Desired=running write, still present: %+v", sup.Stops)
	}
	// An emptied sub-block is omitted from JSON (omitempty / nil). len of a nil
	// map is 0, so the explicit nil check is redundant.
	if len(sup.Stops) != 0 {
		t.Fatalf("expected empty/nil sub-block, got %+v", sup.Stops)
	}
	assertDaemonIntentEqual(t, sup.LegacyStopWatermarks[task], stop)
}

func TestWriteStopIntent_ActiveStopPrunesLegacyStopWatermark(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-paper-search-default`
	oldStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC().Add(-time.Hour)}
	newStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: time.Now().UTC()}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{
		Version:              1,
		LegacyStopWatermarks: map[string]DaemonIntent{task: oldStop},
	}); err != nil {
		t.Fatalf("seed supervisor intent watermark: %v", err)
	}

	if err := NewAPI().WriteStopIntent(task, newStop, "tester"); err != nil {
		t.Fatalf("WriteStopIntent active: %v", err)
	}
	sup := readSupervisorIntentFromDisk(t, stateDir)
	assertDaemonIntentEqual(t, sup.Stops[task], newStop)
	if _, ok := sup.LegacyStopWatermarks[task]; ok {
		t.Fatalf("active WriteStopIntent left redundant watermark behind: %+v", sup.LegacyStopWatermarks)
	}
}

func TestWriteStopIntent_IdempotentActivePrunesWatermarkWithoutAudit(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	var captured []IntentAuditEntry
	installTestAuditFn(t, &captured, nil)

	task := `\mcp-local-hub-paper-search-default`
	stop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	writeRawSupervisorIntentForCollapseTest(t, stateDir,
		map[string]DaemonIntent{task: stop},
		map[string]DaemonIntent{task: stop},
	)

	if err := NewAPI().WriteStopIntent(task, stop, "tester"); err != nil {
		t.Fatalf("idempotent active WriteStopIntent: %v", err)
	}
	sup := readSupervisorIntentFromDisk(t, stateDir)
	assertDaemonIntentEqual(t, sup.Stops[task], stop)
	if _, ok := sup.LegacyStopWatermarks[task]; ok {
		t.Fatalf("idempotent active write did not prune redundant watermark: %+v", sup.LegacyStopWatermarks)
	}
	if len(captured) != 0 {
		t.Fatalf("watermark-only normalization emitted stop audit entries: %+v", captured)
	}
}

func TestWriteStopIntent_UnrelatedWriteNormalizesRedundantLegacyStopWatermark(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	activeTask := `\mcp-local-hub-demo-active`
	writeTask := `\mcp-local-hub-demo-written`
	clearedTask := `\mcp-local-hub-demo-cleared`
	activeStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now}
	writeStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(time.Minute)}
	clearedStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Minute)}
	writeRawSupervisorIntentForCollapseTest(t, stateDir,
		map[string]DaemonIntent{activeTask: activeStop},
		map[string]DaemonIntent{activeTask: activeStop, clearedTask: clearedStop},
	)

	if err := NewAPI().WriteStopIntent(writeTask, writeStop, "tester"); err != nil {
		t.Fatalf("unrelated WriteStopIntent: %v", err)
	}

	sup := readSupervisorIntentFromDisk(t, stateDir)
	assertDaemonIntentEqual(t, sup.Stops[activeTask], activeStop)
	assertDaemonIntentEqual(t, sup.Stops[writeTask], writeStop)
	if _, ok := sup.LegacyStopWatermarks[activeTask]; ok {
		t.Fatalf("unrelated stop write preserved redundant active-task watermark: %+v", sup.LegacyStopWatermarks)
	}
	assertDaemonIntentEqual(t, sup.LegacyStopWatermarks[clearedTask], clearedStop)
}

// An expired user-stop write DROPS rather than carries (matches the merge
// owner's active/inactive semantic).
func TestWriteStopIntent_ExpiredUserStopDropped(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-expired-default`
	// A user-stop dated 48h ago is past the 24h TTL → inactive → not stored.
	// The write is a no-op (nothing to store), so supervisor-intent.json may
	// legitimately not exist at all — either way the stop must be absent.
	if err := NewAPI().WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC().Add(-48 * time.Hour),
	}, "tester"); err != nil {
		t.Fatalf("WriteStopIntent: %v", err)
	}
	got, err := ReadSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return // no file written for an expired no-op stop — correct.
		}
		t.Fatalf("read supervisor-intent.json: %v", err)
	}
	if _, ok := got.Stops[task]; ok {
		t.Fatalf("expired user-stop should NOT be stored as active stop, got %+v", got.Stops)
	}
}

func TestClearStopIntent_SnapshotsDepartingStopWatermark(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-clear-default`
	stop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	if err := NewAPI().WriteStopIntent(task, stop, "tester"); err != nil {
		t.Fatalf("seed stop: %v", err)
	}

	if err := NewAPI().ClearStopIntent(task, "tester"); err != nil {
		t.Fatalf("ClearStopIntent: %v", err)
	}
	sup := readSupervisorIntentFromDisk(t, stateDir)
	if _, ok := sup.Stops[task]; ok {
		t.Fatalf("ClearStopIntent left stop behind: %+v", sup.Stops)
	}
	assertDaemonIntentEqual(t, sup.LegacyStopWatermarks[task], stop)
}

func TestClearStopIntentIfReason_WatermarkLifecycle(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-idle-default`
	siblingTask := `\mcp-local-hub-cleared-sibling`
	now := time.Now().UTC()
	operatorStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now}
	siblingWatermark := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(-time.Minute)}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{
		Version:              1,
		Stops:                map[string]DaemonIntent{task: operatorStop},
		LegacyStopWatermarks: map[string]DaemonIntent{siblingTask: siblingWatermark},
	}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}

	clearAllowed, err := NewAPI().ClearStopIntentIfReason(task, IntentReasonIdle, "wake")
	if err != nil {
		t.Fatalf("refused ClearStopIntentIfReason: %v", err)
	}
	if clearAllowed {
		t.Fatal("ClearStopIntentIfReason returned clearAllowed=true for mismatched reason")
	}
	sup := readSupervisorIntentFromDisk(t, stateDir)
	assertDaemonIntentEqual(t, sup.Stops[task], operatorStop)
	if _, ok := sup.LegacyStopWatermarks[task]; ok {
		t.Fatalf("refused clear created same-task watermark: %+v", sup.LegacyStopWatermarks)
	}
	assertDaemonIntentEqual(t, sup.LegacyStopWatermarks[siblingTask], siblingWatermark)

	idleStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonIdle, UpdatedAt: now.Add(time.Minute)}
	if err := NewAPI().WriteStopIntent(task, idleStop, "tester"); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}
	clearAllowed, err = NewAPI().ClearStopIntentIfReason(task, IntentReasonIdle, "wake")
	if err != nil {
		t.Fatalf("matching ClearStopIntentIfReason: %v", err)
	}
	if !clearAllowed {
		t.Fatal("ClearStopIntentIfReason returned clearAllowed=false for matching idle stop")
	}
	sup = readSupervisorIntentFromDisk(t, stateDir)
	if _, ok := sup.Stops[task]; ok {
		t.Fatalf("matching clear left stop behind: %+v", sup.Stops)
	}
	assertDaemonIntentEqual(t, sup.LegacyStopWatermarks[task], idleStop)
	assertDaemonIntentEqual(t, sup.LegacyStopWatermarks[siblingTask], siblingWatermark)
}

func TestClearStopIntent_IdempotentAbsentPreservesWatermarkWithoutAudit(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	var captured []IntentAuditEntry
	installTestAuditFn(t, &captured, nil)

	task := `\mcp-local-hub-cleared-default`
	watermark := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{
		Version:              1,
		LegacyStopWatermarks: map[string]DaemonIntent{task: watermark},
	}); err != nil {
		t.Fatalf("seed absent watermark: %v", err)
	}

	if err := NewAPI().ClearStopIntent(task, "tester"); err != nil {
		t.Fatalf("idempotent ClearStopIntent: %v", err)
	}
	sup := readSupervisorIntentFromDisk(t, stateDir)
	if _, ok := sup.Stops[task]; ok {
		t.Fatalf("idempotent clear created stop: %+v", sup.Stops)
	}
	assertDaemonIntentEqual(t, sup.LegacyStopWatermarks[task], watermark)
	if len(captured) != 0 {
		t.Fatalf("idempotent clear emitted audit entries: %+v", captured)
	}
}

func TestClearStopIntent_BareKeyStopClearsWithoutResurrection(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-bare-clear`
	bareTask := strings.TrimPrefix(task, `\`)
	stop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	writeRawSupervisorIntentForCollapseTest(t, stateDir, map[string]DaemonIntent{bareTask: stop}, nil)

	if err := NewAPI().ClearStopIntent(task, "tester"); err != nil {
		t.Fatalf("ClearStopIntent: %v", err)
	}

	sup := readSupervisorIntentFromDisk(t, stateDir)
	if _, ok := sup.Stops[task]; ok {
		t.Fatalf("bare-key stop resurrected as canonical active stop after clear: %+v", sup.Stops)
	}
	if _, ok := sup.Stops[bareTask]; ok {
		t.Fatalf("bare-key stop survived clear: %+v", sup.Stops)
	}
	assertDaemonIntentEqual(t, sup.LegacyStopWatermarks[task], stop)
}

func TestWriteStopIntentIdleGuarded_BareKeyOperatorStopRefusesIdleOverwrite(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-bare-idle`
	bareTask := strings.TrimPrefix(task, `\`)
	now := time.Now().UTC()
	operatorStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now}
	idleStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonIdle, UpdatedAt: now.Add(time.Minute)}
	writeRawSupervisorIntentForCollapseTest(t, stateDir, map[string]DaemonIntent{bareTask: operatorStop}, nil)

	wrote, err := NewAPI().WriteStopIntentIdleGuardedResult(task, idleStop, "idle-sweeper", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("WriteStopIntentIdleGuardedResult: %v", err)
	}
	if wrote {
		t.Fatal("idle guarded write reported a write despite an active bare-key operator stop")
	}

	sup := readSupervisorIntentFromDisk(t, stateDir)
	got, ok := sup.Stops[task]
	if !ok {
		t.Fatalf("operator stop missing after idle guarded write; stops=%+v", sup.Stops)
	}
	assertDaemonIntentEqual(t, got, operatorStop)
}

func TestWriteStopIntent_BareStopAndBareWatermarkCompactWithoutStopAudit(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	var captured []IntentAuditEntry
	installTestAuditFn(t, &captured, nil)

	task := `\mcp-local-hub-bare-watermark`
	bareTask := strings.TrimPrefix(task, `\`)
	stop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	writeRawSupervisorIntentForCollapseTest(t, stateDir,
		map[string]DaemonIntent{bareTask: stop},
		map[string]DaemonIntent{bareTask: stop},
	)

	if err := NewAPI().WriteStopIntent(task, stop, "tester"); err != nil {
		t.Fatalf("WriteStopIntent: %v", err)
	}
	if len(captured) != 0 {
		t.Fatalf("canonicalization-only/write-normalization-only persistence emitted stop audit entries: %+v", captured)
	}

	raw := readRawSupervisorIntentFileForTest(t, stateDir)
	if len(raw.Stops) != 1 {
		t.Fatalf("raw stops count = %d, want exactly one canonical entry: %+v", len(raw.Stops), raw.Stops)
	}
	assertDaemonIntentEqual(t, raw.Stops[task], stop)
	if _, ok := raw.Stops[bareTask]; ok {
		t.Fatalf("bare stop key survived compaction: %+v", raw.Stops)
	}
	if len(raw.LegacyStopWatermarks) != 0 {
		t.Fatalf("redundant same-task watermark survived compaction: %+v", raw.LegacyStopWatermarks)
	}
}

// WriteStopIntent preserves every non-Stops field of a pre-existing
// supervisor-intent.json (the lost-update guard).
func TestWriteStopIntent_PreservesNonStopsFields(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	seed := &SupervisorIntentFile{
		Version:    1,
		StrictMode: true,
		Daemons: []SupervisorDaemon{{
			TaskName: `\mcp-local-hub-serena-abc`,
			Server:   "serena",
			Daemon:   "abc",
			Port:     9121,
		}},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	if err := NewAPI().WriteStopIntent(`\mcp-local-hub-paper-search-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("WriteStopIntent: %v", err)
	}

	sup := readSupervisorIntentFromDisk(t, stateDir)
	if !sup.StrictMode {
		t.Fatalf("StrictMode clobbered by stop write")
	}
	if len(sup.Daemons) != 1 || sup.Daemons[0].TaskName != `\mcp-local-hub-serena-abc` || sup.Daemons[0].Port != 9121 {
		t.Fatalf("Daemons slice clobbered by stop write: %+v", sup.Daemons)
	}
	if _, ok := sup.Stops[`\mcp-local-hub-paper-search-default`]; !ok {
		t.Fatalf("stop not recorded in sub-block")
	}
}

// The over-size guards fire on both `who` and the canonical key, same as
// WriteDaemonIntent.
func TestWriteStopIntent_OversizeGuards(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	huge := make([]byte, IdentityFieldByteCap+1)
	for i := range huge {
		huge[i] = 'a'
	}
	if err := NewAPI().WriteStopIntent("\\mcp-local-hub-ok", DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC(),
	}, string(huge)); err != ErrEntryOversize {
		t.Fatalf("want ErrEntryOversize on oversize who, got %v", err)
	}
	// A bare name at exactly the cap becomes cap+1 canonical → reject.
	bareAtCap := "m" + string(make([]byte, IdentityFieldByteCap-1))
	if err := NewAPI().WriteStopIntent(bareAtCap, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC(),
	}, "tester"); err != ErrEntryOversize {
		t.Fatalf("want ErrEntryOversize on canonical-recheck, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// AUDIT emission on the sub-block write path matches WriteDaemonIntent shape.
// ---------------------------------------------------------------------------

func TestWriteStopIntent_EmitsSetIntentAndClearIntentAudit(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	var captured []IntentAuditEntry
	installTestAuditFn(t, &captured, nil)

	task := `\mcp-local-hub-x`
	now := time.Now().UTC()
	if err := NewAPI().WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	}, "tester"); err != nil {
		t.Fatalf("WriteStopIntent (set): %v", err)
	}
	if len(captured) != 1 || captured[0].Action != "set-intent" || captured[0].Task != task {
		t.Fatalf("expected one set-intent audit entry, got %+v", captured)
	}
	if captured[0].After == nil || captured[0].After.Desired != IntentDesiredStopped {
		t.Fatalf("set-intent After snapshot missing/wrong: %+v", captured[0].After)
	}

	// Re-enable → clear-intent with Before.
	captured = nil
	if err := NewAPI().WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredRunning, Reason: IntentReasonInstall, UpdatedAt: now.Add(time.Minute),
	}, "tester"); err != nil {
		t.Fatalf("WriteStopIntent (re-enable): %v", err)
	}
	if len(captured) != 1 || captured[0].Action != "clear-intent" || captured[0].Before == nil {
		t.Fatalf("expected one clear-intent audit entry with Before, got %+v", captured)
	}
}

func TestWriteStopIntentIdleGuarded_AuditsRefusalWithoutSetIntent(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	var captured []IntentAuditEntry
	installTestAuditFn(t, &captured, nil)

	a := NewAPI()
	now := time.Now().UTC()
	operatorTask := `\mcp-local-hub-operator-default`
	if err := a.WriteStopIntent(operatorTask, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}, "operator"); err != nil {
		t.Fatalf("seed operator stop: %v", err)
	}

	captured = nil
	if err := a.WriteStopIntentIdleGuarded(operatorTask, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonIdle,
		UpdatedAt: now.Add(time.Minute),
	}, "idle-sweeper", now.Add(time.Minute)); err != nil {
		t.Fatalf("idle guarded refusal: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("idle guarded refusal audit entries = %+v, want one distinct refusal event", captured)
	}
	if captured[0].Action == "set-intent" {
		t.Fatalf("idle guarded refusal emitted set-intent: %+v", captured[0])
	}
	if captured[0].Action != "idle-stop-refused-operator-stop-active" {
		t.Fatalf("idle guarded refusal action = %q, want idle-stop-refused-operator-stop-active", captured[0].Action)
	}
	if captured[0].Who != "idle-sweeper" || captured[0].Task != operatorTask {
		t.Fatalf("idle guarded refusal identity mismatch: %+v", captured[0])
	}
	if captured[0].Priority != "" {
		t.Fatalf("idle guarded refusal priority = %q, want default info-level empty priority", captured[0].Priority)
	}
	if captured[0].Before == nil || captured[0].Before.Reason != IntentReasonUserStop {
		t.Fatalf("idle guarded refusal Before = %+v, want operator stop snapshot", captured[0].Before)
	}
	got := readSupervisorIntentFromDisk(t, stateDir).Stops[operatorTask]
	if got.Reason != IntentReasonUserStop {
		t.Fatalf("idle guarded refusal changed stored stop reason to %q, want %q", got.Reason, IntentReasonUserStop)
	}

	idleTask := `\mcp-local-hub-idle-default`
	captured = nil
	if err := a.WriteStopIntentIdleGuarded(idleTask, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonIdle,
		UpdatedAt: now,
	}, "idle-sweeper", now); err != nil {
		t.Fatalf("idle guarded genuine write: %v", err)
	}
	if len(captured) != 1 || captured[0].Action != "set-intent" || captured[0].After == nil {
		t.Fatalf("genuine idle write audit = %+v, want one set-intent with After", captured)
	}
	if captured[0].After.Reason != IntentReasonIdle {
		t.Fatalf("genuine idle write After.Reason = %q, want %q", captured[0].After.Reason, IntentReasonIdle)
	}

	captured = nil
	clearAllowed, err := a.ClearStopIntentIfReason(idleTask, IntentReasonIdle, "wake")
	if err != nil {
		t.Fatalf("ClearStopIntentIfReason: %v", err)
	}
	if !clearAllowed {
		t.Fatal("ClearStopIntentIfReason returned false for matching idle stop")
	}
	if len(captured) != 1 || captured[0].Action != "clear-intent" || captured[0].Before == nil {
		t.Fatalf("matching clear audit = %+v, want one clear-intent with Before", captured)
	}
}

// ---------------------------------------------------------------------------
// ROUND-TRIP: a stop written via WriteStopIntent survives a read through the
// sub-block ALONE (daemon-intent.json absent), via UnifiedStopsFile + the five
// readers' shared helpers.
// ---------------------------------------------------------------------------

func TestWriteStopIntent_RoundTripsThroughSubBlockAlone(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	task := `\mcp-local-hub-paper-search-default`
	if err := NewAPI().WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	}, "tester"); err != nil {
		t.Fatalf("WriteStopIntent: %v", err)
	}

	// daemon-intent.json is absent. UnifiedStopsFile(sub-block, nil) must yield
	// the stop.
	sup := readSupervisorIntentFromDisk(t, stateDir)
	unified := UnifiedStopsFile(sup, nil)
	di, ok := unified.Tasks[task]
	if !ok {
		t.Fatalf("stop lost through UnifiedStopsFile with daemon-intent.json absent; tasks=%+v", unified.Tasks)
	}
	active, _ := di.IsActiveStop(now)
	if !active {
		t.Fatalf("stop should be active at write time")
	}

	// Reader #5 surface: IntentStillRunning must report the daemon NOT running
	// (suppressed) sourced purely from the sub-block.
	if NewAPI().IntentStillRunning(task, now) {
		t.Fatalf("IntentStillRunning should be false (stop active) via sub-block alone")
	}

	// Tray-side reader: TryReadUnifiedStops must surface the stop from the
	// sub-block.
	res := NewAPI().TryReadUnifiedStops(0)
	if res.State != IntentStateValid {
		t.Fatalf("TryReadUnifiedStops state = %s, want valid", res.State)
	}
	if _, ok := res.File.Tasks[task]; !ok {
		t.Fatalf("TryReadUnifiedStops dropped the sub-block stop: %+v", res.File.Tasks)
	}
}

// ---------------------------------------------------------------------------
// PRECEDENCE FLIP: the sub-block is authoritative; a STALE daemon-intent.json
// must NOT override it (E1→E2 inversion).
// ---------------------------------------------------------------------------

func TestUnifiedStopsFile_SubBlockIsAuthoritative_StaleDaemonIntentIgnored(t *testing.T) {
	now := time.Now().UTC()
	sub := &SupervisorIntentFile{
		Stops: map[string]DaemonIntent{
			`\mcp-local-hub-a`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
		},
	}
	// A stale daemon-intent.json with a DIFFERENT (extra) stop that the
	// sub-block does NOT have. After E2 it must be ignored entirely.
	stale := &DaemonIntentFile{
		Tasks: map[string]DaemonIntent{
			`\mcp-local-hub-b`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
		},
	}
	got := UnifiedStopsFile(sub, stale)
	if _, ok := got.Tasks[`\mcp-local-hub-a`]; !ok {
		t.Fatalf("sub-block stop A missing — sub-block must be authoritative")
	}
	if _, ok := got.Tasks[`\mcp-local-hub-b`]; ok {
		t.Fatalf("stale daemon-intent.json stop B leaked through — must be ignored after E2")
	}
}

func TestUnifiedStopsFile_NilSupervisorIntent_EmptyNonNil(t *testing.T) {
	got := UnifiedStopsFile(nil, nil)
	if got == nil || got.Tasks == nil {
		t.Fatalf("UnifiedStopsFile(nil,nil) must return non-nil file with non-nil Tasks; got %+v", got)
	}
	if len(got.Tasks) != 0 {
		t.Fatalf("expected empty Tasks, got %+v", got.Tasks)
	}
}

// keysOf returns the keys of a stops map for diagnostics.
func keysOf(m map[string]DaemonIntent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// marshalRoundTripStops asserts the on-disk JSON shape carries the stops key
// (defensive that the sub-block actually persists, not just in-memory).
func TestWriteStopIntent_OnDiskJSONCarriesStopsKey(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	if err := NewAPI().WriteStopIntent(`\mcp-local-hub-y`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: time.Now().UTC(),
	}, "tester"); err != nil {
		t.Fatalf("WriteStopIntent: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	var probe struct {
		Stops map[string]json.RawMessage `json:"stops"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := probe.Stops[`\mcp-local-hub-y`]; !ok {
		t.Fatalf("on-disk JSON missing stops[\\mcp-local-hub-y]; raw=%s", raw)
	}
}

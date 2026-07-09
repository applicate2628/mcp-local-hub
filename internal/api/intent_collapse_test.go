package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"mcp-local-hub/internal/api/apitest"
)

// seedDaemonIntent writes daemon-intent.json under the test state dir via the
// public WriteDaemonIntent path (so the key normalization + atomic write match
// production exactly).
func seedDaemonIntent(t *testing.T, task string, di DaemonIntent) {
	t.Helper()
	if err := NewAPI().WriteDaemonIntent(task, di, "test"); err != nil {
		t.Fatalf("seed daemon-intent.json (%s): %v", task, err)
	}
}

func readSupervisorStopsFromDisk(t *testing.T, stateDir string) map[string]DaemonIntent {
	t.Helper()
	got, err := ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read supervisor-intent.json: %v", err)
	}
	if got.Stops == nil {
		return map[string]DaemonIntent{}
	}
	return got.Stops
}

func writeRawSupervisorIntentForCollapseTest(t *testing.T, stateDir string, stops, watermarks map[string]DaemonIntent) {
	t.Helper()
	payload := struct {
		Version              int                     `json:"version"`
		Stops                map[string]DaemonIntent `json:"stops,omitempty"`
		LegacyStopWatermarks map[string]DaemonIntent `json:"legacy_stop_watermarks,omitempty"`
	}{
		Version:              1,
		Stops:                stops,
		LegacyStopWatermarks: watermarks,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal raw supervisor-intent fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, supervisorIntentFileLeaf), raw, 0o600); err != nil {
		t.Fatalf("write raw supervisor-intent fixture: %v", err)
	}
}

func readSupervisorLegacyStopWatermarksFromDisk(t *testing.T, stateDir string) map[string]DaemonIntent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("read raw supervisor-intent.json: %v", err)
	}
	var payload struct {
		LegacyStopWatermarks map[string]DaemonIntent `json:"legacy_stop_watermarks"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal raw supervisor-intent.json: %v", err)
	}
	if payload.LegacyStopWatermarks == nil {
		return map[string]DaemonIntent{}
	}
	return payload.LegacyStopWatermarks
}

func assertDaemonIntentEqual(t *testing.T, got, want DaemonIntent) {
	t.Helper()
	if got.Desired != want.Desired || got.Reason != want.Reason || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("daemon intent = %+v, want %+v", got, want)
	}
}

func TestReadDaemonIntentForMerge_LargeLegacyDaemonIntentAboveHubStateCap(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	tasks := make(map[string]DaemonIntent, 12000)
	for i := 0; i < 12000; i++ {
		tasks[fmt.Sprintf("\\mcp-local-hub-large-%05d", i)] = DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now,
		}
	}
	raw, err := json.Marshal(DaemonIntentFile{Tasks: tasks})
	if err != nil {
		t.Fatalf("marshal large daemon-intent: %v", err)
	}
	if len(raw) <= maxStateFileBytes {
		t.Fatalf("test fixture is only %d bytes; want above hub-state cap %d", len(raw), maxStateFileBytes)
	}

	path := filepath.Join(stateDir, intentFileLeaf)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write large daemon-intent: %v", err)
	}

	got, err := readDaemonIntentForMerge(path)
	if err != nil {
		t.Fatalf("readDaemonIntentForMerge over hub-state cap returned error: %v", err)
	}
	if got == nil || len(got.Tasks) != len(tasks) {
		t.Fatalf("readDaemonIntentForMerge tasks = %d, want %d", len(got.Tasks), len(tasks))
	}
}

func TestWritePreCollapseBackup_LargeLegacyDaemonIntentAboveHubStateCap(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	tasks := make(map[string]DaemonIntent, 12000)
	for i := 0; i < 12000; i++ {
		tasks[fmt.Sprintf("\\mcp-local-hub-backup-large-%05d", i)] = DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now,
		}
	}
	daemonRaw, err := json.Marshal(DaemonIntentFile{Tasks: tasks})
	if err != nil {
		t.Fatalf("marshal large daemon-intent: %v", err)
	}
	if len(daemonRaw) <= maxStateFileBytes {
		t.Fatalf("test fixture is only %d bytes; want above hub-state cap %d", len(daemonRaw), maxStateFileBytes)
	}

	supervisorPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := os.WriteFile(supervisorPath, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatalf("write supervisor-intent: %v", err)
	}
	daemonPath := filepath.Join(stateDir, intentFileLeaf)
	if err := os.WriteFile(daemonPath, daemonRaw, 0o600); err != nil {
		t.Fatalf("write daemon-intent: %v", err)
	}

	backupDir, err := writePreCollapseBackup(stateDir, supervisorPath, daemonPath, now)
	if err != nil {
		t.Fatalf("writePreCollapseBackup over hub-state cap returned error: %v", err)
	}
	gotRaw, err := os.ReadFile(filepath.Join(backupDir, intentFileLeaf))
	if err != nil {
		t.Fatalf("read daemon-intent backup: %v", err)
	}
	if string(gotRaw) != string(daemonRaw) {
		t.Fatalf("daemon-intent backup bytes changed: got %d bytes, want %d", len(gotRaw), len(daemonRaw))
	}
}

func TestWritePreCollapseBackup_LargeSupervisorIntentAboveHubStateCap(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	supervisorRaw := append([]byte(`{"version":1,"padding":"`), bytes.Repeat([]byte("x"), int(maxStateFileBytes)+1)...)
	supervisorRaw = append(supervisorRaw, []byte(`"}`)...)
	if len(supervisorRaw) <= int(maxStateFileBytes) {
		t.Fatalf("test fixture is only %d bytes; want above hub-state cap %d", len(supervisorRaw), maxStateFileBytes)
	}
	if int64(len(supervisorRaw)) > maxIntentFileBytes {
		t.Fatalf("test fixture grew past supervisor intent cap: %d > %d", len(supervisorRaw), maxIntentFileBytes)
	}

	supervisorPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := os.WriteFile(supervisorPath, supervisorRaw, 0o600); err != nil {
		t.Fatalf("write supervisor-intent: %v", err)
	}
	daemonPath := filepath.Join(stateDir, intentFileLeaf)

	backupDir, err := writePreCollapseBackup(stateDir, supervisorPath, daemonPath, now)
	if err != nil {
		t.Fatalf("writePreCollapseBackup rejected large supervisor-intent source: %v", err)
	}
	gotRaw, err := os.ReadFile(filepath.Join(backupDir, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor-intent backup: %v", err)
	}
	if !bytes.Equal(gotRaw, supervisorRaw) {
		t.Fatalf("supervisor-intent backup bytes changed: got %d bytes, want %d", len(gotRaw), len(supervisorRaw))
	}
}

// ---------------------------------------------------------------------------
// Merge preserves ALL stop semantics (TTL / clock-skew / reason).
// ---------------------------------------------------------------------------

func TestRunDaemonIntentCollapse_PreservesActiveStopsAndDropsExpired(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	// A running serena pool descriptor in supervisor-intent.json (must be
	// preserved untouched by the merge).
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: `\mcp-local-hub-serena-abc`,
			Server:   "serena",
			Daemon:   "abc",
			Port:     9121,
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	// Four daemon-intent.json entries exercising every IsActiveStop branch:
	//   - paper-search: fresh user-stop (ACTIVE — carry, reason preserved)
	//   - disabled: user-disabled (ACTIVE forever — carry)
	//   - expired: user-stop older than TTL (INACTIVE — drop)
	//   - skew: stop dated far in the future (ACTIVE via clock-skew fail-closed)
	seedDaemonIntent(t, `\mcp-local-hub-paper-search-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-1 * time.Hour),
	})
	seedDaemonIntent(t, `\mcp-local-hub-disabled-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(-100 * 24 * time.Hour),
	})
	seedDaemonIntent(t, `\mcp-local-hub-expired-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-48 * time.Hour),
	})
	seedDaemonIntent(t, `\mcp-local-hub-skew-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonChronicFailure, UpdatedAt: now.Add(48 * time.Hour),
	})

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.Wrote {
		t.Fatalf("expected the merge to write (active stops present); res=%+v", res)
	}

	stops := readSupervisorStopsFromDisk(t, stateDir)

	// paper-search: active user-stop preserved with its exact reason+timestamp.
	ps, ok := stops[`\mcp-local-hub-paper-search-default`]
	if !ok {
		t.Fatalf("paper-search stop missing from unified stops: %+v", stops)
	}
	if ps.Reason != IntentReasonUserStop || !ps.UpdatedAt.Equal(now.Add(-1*time.Hour)) {
		t.Fatalf("paper-search stop mangled: %+v", ps)
	}
	// disabled: permanent stop preserved.
	if _, ok := stops[`\mcp-local-hub-disabled-default`]; !ok {
		t.Fatalf("user-disabled stop dropped; want carried")
	}
	// skew: clock-skew-future is fail-closed ACTIVE → carried.
	if _, ok := stops[`\mcp-local-hub-skew-default`]; !ok {
		t.Fatalf("clock-skew-future stop dropped; want carried (fail-closed active)")
	}
	// expired: TTL-expired stop NOT carried.
	if _, ok := stops[`\mcp-local-hub-expired-default`]; ok {
		t.Fatalf("expired user-stop carried; want dropped")
	}

	// Verify IsActiveStop semantics survive the round-trip: the carried
	// paper-search record must still evaluate active at `now`.
	if active, _ := ps.IsActiveStop(now); !active {
		t.Fatalf("carried paper-search stop no longer evaluates active")
	}

	// The supervisor daemon descriptor must be untouched (merge touches Stops only).
	got, _ := ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if len(got.Daemons) != 1 || got.Daemons[0].TaskName != `\mcp-local-hub-serena-abc` || got.Daemons[0].Port != 9121 {
		t.Fatalf("merge mutated the daemon descriptors: %+v", got.Daemons)
	}
}

func TestRunDaemonIntentCollapse_MintsSupervisorIntentForLegacyOnlyActiveStop(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-paper-search-default`
	stop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Minute)}
	supPath := filepath.Join(stateDir, supervisorIntentFileLeaf)

	if _, err := os.Stat(supPath); !os.IsNotExist(err) {
		t.Fatalf("test precondition: supervisor-intent.json must be absent, stat err=%v", err)
	}
	seedDaemonIntent(t, task, stop)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("legacy-only collapse panicked instead of minting supervisor-intent.json: %v", r)
		}
	}()
	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.Wrote {
		t.Fatalf("legacy-only active stop should write a freshly minted supervisor intent; res=%+v", res)
	}
	got, err := ReadSupervisorIntent(supPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("minted supervisor intent Version = %d, want 1", got.Version)
	}
	if gotStop, ok := got.Stops[task]; !ok || gotStop.Desired != stop.Desired || gotStop.Reason != stop.Reason || !gotStop.UpdatedAt.Equal(stop.UpdatedAt) {
		t.Fatalf("minted supervisor intent stops[%s] = %+v, ok=%v; want %+v", task, gotStop, ok, stop)
	}
}

func TestRunDaemonIntentCollapse_LegacyStopWatermarkBlocksStaleReplayAfterClear(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-paper-search-default`
	oldStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Minute)}

	writeRawSupervisorIntentForCollapseTest(t, stateDir, nil, map[string]DaemonIntent{task: oldStop})
	seedDaemonIntent(t, task, oldStop)

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.DeletedLegacyFile {
		t.Fatalf("matching legacy-stop watermark should permit deleting the stale legacy file; res=%+v", res)
	}
	if _, ok := readSupervisorStopsFromDisk(t, stateDir)[task]; ok {
		t.Fatalf("stale legacy stop was resurrected after the stop had been cleared")
	}
	if !NewAPI().IntentStillRunning(task, now) {
		t.Fatalf("watermark must not be a stop source; task should be permitted to run")
	}
	assertDaemonIntentEqual(t, readSupervisorLegacyStopWatermarksFromDisk(t, stateDir)[task], oldStop)
}

func TestRunDaemonIntentCollapse_LegacyStopWatermarkSelfHealsExistingSubBlockStop(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-paper-search-default`
	stop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Minute)}

	writeRawSupervisorIntentForCollapseTest(t, stateDir, map[string]DaemonIntent{task: stop}, nil)
	seedDaemonIntent(t, task, stop)

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.Wrote || !res.DeletedLegacyFile {
		t.Fatalf("collapse should write the missing watermark and delete the accounted legacy file; res=%+v", res)
	}
	assertDaemonIntentEqual(t, readSupervisorStopsFromDisk(t, stateDir)[task], stop)
	assertDaemonIntentEqual(t, readSupervisorLegacyStopWatermarksFromDisk(t, stateDir)[task], stop)
}

func TestRunDaemonIntentCollapse_LegacyStopWatermarkLossFailsTowardRespectingStop(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-paper-search-default`
	stop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Minute)}

	writeRawSupervisorIntentForCollapseTest(t, stateDir, nil, map[string]DaemonIntent{task: stop})
	raw, err := os.ReadFile(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor-intent before simulated old rewrite: %v", err)
	}
	var oldReaderShape struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &oldReaderShape); err != nil {
		t.Fatalf("supported old reader shape should ignore watermark: %v", err)
	}
	oldRaw, err := json.Marshal(oldReaderShape)
	if err != nil {
		t.Fatalf("marshal simulated old writer shape: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, supervisorIntentFileLeaf), oldRaw, 0o600); err != nil {
		t.Fatalf("write simulated old supervisor-intent without watermark: %v", err)
	}
	seedDaemonIntent(t, task, stop)

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.Wrote || !res.DeletedLegacyFile {
		t.Fatalf("missing/lost watermark should be treated as a real legacy stop and persisted; res=%+v", res)
	}
	assertDaemonIntentEqual(t, readSupervisorStopsFromDisk(t, stateDir)[task], stop)
	assertDaemonIntentEqual(t, readSupervisorLegacyStopWatermarksFromDisk(t, stateDir)[task], stop)
	if NewAPI().IntentStillRunning(task, now) {
		t.Fatalf("watermark loss must fail toward respecting the legacy stop, not silently starting")
	}
}

func TestRunDaemonIntentCollapse_LegitimateRestopAfterClearAddsDifferentLegacyRecord(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-paper-search-default`
	oldStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-2 * time.Hour)}
	newStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(-time.Minute)}

	writeRawSupervisorIntentForCollapseTest(t, stateDir, nil, map[string]DaemonIntent{task: oldStop})
	seedDaemonIntent(t, task, newStop)

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.Wrote || !res.DeletedLegacyFile {
		t.Fatalf("different legacy stop should be treated as a real re-stop and persisted; res=%+v", res)
	}
	assertDaemonIntentEqual(t, readSupervisorStopsFromDisk(t, stateDir)[task], newStop)
	assertDaemonIntentEqual(t, readSupervisorLegacyStopWatermarksFromDisk(t, stateDir)[task], newStop)
}

// Phase 4-E2 (was E1 "DoesNotDelete"): daemon-intent.json is now DELETED after
// the merge migrates its active stops into the sub-block. This is the inverted
// E2 contract — the file no longer remains on disk.
func TestRunDaemonIntentCollapse_E2_DeletesDaemonIntentAfterMerge(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()
	seedDaemonIntent(t, `\mcp-local-hub-paper-search-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	})
	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.DeletedLegacyFile {
		t.Fatalf("E2 contract: expected daemon-intent.json deleted; res=%+v", res)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "daemon-intent.json")); !os.IsNotExist(err) {
		t.Fatalf("E2 contract violated: daemon-intent.json must be deleted (err=%v)", err)
	}
	// The stop must survive in the sub-block.
	if _, ok := readSupervisorStopsFromDisk(t, stateDir)[`\mcp-local-hub-paper-search-default`]; !ok {
		t.Fatalf("stop lost from sub-block after E2 delete")
	}
}

// Code-baked pre-merge backup snapshots BOTH files before writing.
func TestRunDaemonIntentCollapse_TakesPreMergeBackup(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"),
		&SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	seedDaemonIntent(t, `\mcp-local-hub-paper-search-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	})
	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if res.BackupDir == "" {
		t.Fatalf("expected a pre-merge backup dir; got empty")
	}
	for _, leaf := range []string{"supervisor-intent.json", "daemon-intent.json"} {
		if _, err := os.Stat(filepath.Join(res.BackupDir, leaf)); err != nil {
			t.Fatalf("backup missing %s: %v", leaf, err)
		}
	}
}

// Idempotent: a second run with no stop delta writes nothing.
func TestRunDaemonIntentCollapse_IdempotentSecondRun(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()
	seedDaemonIntent(t, `\mcp-local-hub-paper-search-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	})
	first, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("first RunDaemonIntentCollapse: %v", err)
	}
	if !first.Wrote {
		t.Fatalf("first run should have written")
	}
	second, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("second RunDaemonIntentCollapse: %v", err)
	}
	if second.Changed || second.Wrote {
		t.Fatalf("second run must be a no-op (idempotent): %+v", second)
	}
	if second.BackupDir != "" {
		t.Fatalf("idempotent no-op must not take a backup: %q", second.BackupDir)
	}
}

// ---------------------------------------------------------------------------
// --check / dry-run is READ-ONLY.
// ---------------------------------------------------------------------------

func TestCheckDaemonIntentCollapse_IsReadOnly(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()

	// supervisor-intent.json with NO stops sub-block; capture its exact bytes.
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"),
		&SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	supPath := filepath.Join(stateDir, "supervisor-intent.json")
	before, err := os.ReadFile(supPath)
	if err != nil {
		t.Fatalf("read supervisor-intent.json: %v", err)
	}

	seedDaemonIntent(t, `\mcp-local-hub-paper-search-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	})

	res, err := CheckDaemonIntentCollapse(stateDir, now)
	if err != nil {
		t.Fatalf("CheckDaemonIntentCollapse: %v", err)
	}
	// The dry-run computes the SAME merge result the write path would persist.
	if _, ok := res.MergedStops[`\mcp-local-hub-paper-search-default`]; !ok {
		t.Fatalf("--check did not report the paper-search stop in MergedStops: %+v", res.MergedStops)
	}
	if !res.Changed {
		t.Fatalf("--check should report Changed=true (a stop would be merged)")
	}
	if res.Wrote {
		t.Fatalf("--check must NEVER write")
	}
	if res.BackupDir != "" {
		t.Fatalf("--check must NOT take a backup")
	}

	// supervisor-intent.json must be byte-identical (no write, no stops sub-block).
	after, err := os.ReadFile(supPath)
	if err != nil {
		t.Fatalf("re-read supervisor-intent.json: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("--check mutated supervisor-intent.json on disk")
	}
	// And carries no stops on disk.
	if got := readSupervisorStopsFromDisk(t, stateDir); len(got) != 0 {
		t.Fatalf("--check persisted stops to disk: %+v", got)
	}
}

func TestCheckDaemonIntentCollapse_DoesNotCreateDaemonIntentLock(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	now := time.Date(2026, 6, 12, 11, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-paper-search-default`
	supPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	daemonPath := filepath.Join(stateDir, intentFileLeaf)
	lockPath := filepath.Join(stateDir, intentLockLeaf)

	if err := WriteSupervisorIntent(supPath, &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	raw := []byte(`{"tasks":{"\\mcp-local-hub-paper-search-default":{"desired":"stopped","reason":"user-stop","updated_at":"2026-06-12T10:59:00Z"}}}`)
	if err := os.WriteFile(daemonPath, raw, 0o600); err != nil {
		t.Fatalf("seed daemon-intent.json: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("test precondition: daemon-intent lock must be absent, stat err=%v", err)
	}

	res, err := CheckDaemonIntentCollapse(stateDir, now)
	if err != nil {
		t.Fatalf("CheckDaemonIntentCollapse: %v", err)
	}
	if got, ok := res.MergedStops[task]; !ok || got.Desired != IntentDesiredStopped {
		t.Fatalf("--check merge verdict missing seeded active stop: got=%+v ok=%v res=%+v", got, ok, res)
	}
	if !res.Changed {
		t.Fatalf("--check should report Changed=true for seeded active stop; res=%+v", res)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("--check created daemon-intent lock %s; stat err=%v", lockPath, err)
	}
}

func TestCheckDaemonIntentCollapse_ReportsDroppedInactiveLegacyStops(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Date(2026, 6, 12, 11, 0, 0, 0, time.UTC)
	runningTask := `\mcp-local-hub-running-default`
	expiredTask := `\mcp-local-hub-expired-default`

	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	seedDaemonIntent(t, runningTask, DaemonIntent{
		Desired:   IntentDesiredRunning,
		Reason:    IntentReasonInstall,
		UpdatedAt: now.Add(-time.Hour),
	})
	seedDaemonIntent(t, expiredTask, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now.Add(-48 * time.Hour),
	})

	res, err := CheckDaemonIntentCollapse(stateDir, now)
	if err != nil {
		t.Fatalf("CheckDaemonIntentCollapse: %v", err)
	}
	if res.Changed || res.Wrote {
		t.Fatalf("inactive legacy entries should not mutate the sub-block in --check; res=%+v", res)
	}
	want := []MergeStopsEntry{
		{TaskName: expiredTask, Action: MergeStopDroppedExpired, Reason: IntentReasonUserStop},
		{TaskName: runningTask, Action: MergeStopDroppedExpired, Reason: IntentReasonInstall},
	}
	if len(res.Entries) != len(want) {
		t.Fatalf("--check entries = %+v, want %+v", res.Entries, want)
	}
	for i := range want {
		if res.Entries[i] != want[i] {
			t.Fatalf("--check entries[%d] = %+v, want %+v (all entries=%+v)", i, res.Entries[i], want[i], res.Entries)
		}
	}
}

// ---------------------------------------------------------------------------
// The merge owner holds the daemon-intent flock across the WHOLE op.
// ---------------------------------------------------------------------------

func TestRunDaemonIntentCollapse_HoldsDaemonIntentFlockAcrossWholeOp(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()
	seedDaemonIntent(t, `\mcp-local-hub-paper-search-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	})

	// A concurrent WriteDaemonIntent acquires the SAME flock; if the merge
	// holds it across read→merge→backup→write, this writer cannot interleave
	// inside the critical section. We detect "held" by trying the flock
	// directly: while the merge runs, an external TryLock on the daemon-intent
	// lock must FAIL at least once. Drive the merge on a goroutine and poll.
	lockPath := filepath.Join(stateDir, intentLockLeaf)

	var (
		mu         sync.Mutex
		sawHeld    bool
		mergeDone  = make(chan struct{})
		mergeErr   error
		mergeWrote bool
	)
	go func() {
		res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
		mu.Lock()
		mergeErr = err
		mergeWrote = res.Wrote
		mu.Unlock()
		close(mergeDone)
	}()

	// Poll the external flock until the merge finishes; record if we ever see
	// it held (TryLock returns locked=false).
	for {
		select {
		case <-mergeDone:
			goto done
		default:
		}
		fl := flock.New(lockPath)
		locked, _ := fl.TryLock()
		if !locked {
			mu.Lock()
			sawHeld = true
			mu.Unlock()
		} else {
			_ = fl.Unlock()
		}
	}
done:
	<-mergeDone
	mu.Lock()
	defer mu.Unlock()
	if mergeErr != nil {
		t.Fatalf("merge errored: %v", mergeErr)
	}
	if !mergeWrote {
		t.Fatalf("merge should have written (active stop present)")
	}
	if !sawHeld {
		t.Fatalf("never observed the daemon-intent flock held during the merge; the owner must hold it across the whole op")
	}
}

// Re-read-under-lock: a stop that lands BETWEEN the first read and the write
// is captured. We simulate the delta by pre-staging an extra entry on disk and
// asserting the merge picks it up (the re-read sees the latest file). Because
// the flock blocks concurrent writers, we instead assert the contract that the
// merge always re-reads the LATEST on-disk daemon-intent.json before writing:
// we mutate the file (adding a second stop) directly under a held lock window
// the merge cannot see is impossible — so we verify the simpler, deterministic
// invariant: the merged result reflects the FULL current file content, not a
// stale subset.
func TestRunDaemonIntentCollapse_MergesFullCurrentFileContent(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()
	seedDaemonIntent(t, `\mcp-local-hub-a-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	})
	seedDaemonIntent(t, `\mcp-local-hub-b-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now,
	})
	if _, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now}); err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	stops := readSupervisorStopsFromDisk(t, stateDir)
	if _, ok := stops[`\mcp-local-hub-a-default`]; !ok {
		t.Fatalf("stop a missing from merged result: %+v", stops)
	}
	if _, ok := stops[`\mcp-local-hub-b-default`]; !ok {
		t.Fatalf("stop b missing from merged result: %+v", stops)
	}
}

// A stale legacy daemon-intent.json can only add missing active stops after E2.
// The supervisor-intent stops sub-block is authoritative for tasks it already
// names, so an older running legacy entry must not delete the sub-block stop.
func TestRunDaemonIntentCollapse_StaleLegacyRunningDoesNotDropSubBlockStop(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()
	stoppedTask := `\mcp-local-hub-paper-search-default`
	absentTask := `\mcp-local-hub-memory-default`
	subBlockStop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}

	intent := &SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			stoppedTask: subBlockStop,
		},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	seedDaemonIntent(t, stoppedTask, DaemonIntent{
		Desired: IntentDesiredRunning, Reason: IntentReasonInstall, UpdatedAt: now.Add(-time.Hour),
	})
	seedDaemonIntent(t, absentTask, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now.Add(-time.Minute),
	})

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.Changed {
		t.Fatalf("expected a change for the absent active legacy stop")
	}
	stops := readSupervisorStopsFromDisk(t, stateDir)
	if got, ok := stops[stoppedTask]; !ok || got != subBlockStop {
		t.Fatalf("authoritative sub-block stop = (%+v,%v), want (%+v,true)", got, ok, subBlockStop)
	}
	if got, ok := stops[absentTask]; !ok || got.Desired != IntentDesiredStopped || got.Reason != IntentReasonUserDisabled {
		t.Fatalf("absent active legacy stop = (%+v,%v), want added disabled stop", got, ok)
	}
	wantEntries := []MergeStopsEntry{
		{TaskName: absentTask, Action: MergeStopAdded, Reason: IntentReasonUserDisabled},
		{TaskName: stoppedTask, Action: MergeStopDroppedExpired, Reason: IntentReasonInstall},
	}
	if len(res.Entries) != len(wantEntries) {
		t.Fatalf("collapse entries = %+v, want %+v", res.Entries, wantEntries)
	}
	for i := range wantEntries {
		if res.Entries[i] != wantEntries[i] {
			t.Fatalf("collapse entries[%d] = %+v, want %+v (all entries=%+v)", i, res.Entries[i], wantEntries[i], res.Entries)
		}
	}
}

func TestRunDaemonIntentCollapse_StaleLegacyRunningOnlyIsNoOp(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()
	stoppedTask := `\mcp-local-hub-paper-search-default`
	subBlockStop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now,
	}

	intent := &SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			stoppedTask: subBlockStop,
		},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	seedDaemonIntent(t, stoppedTask, DaemonIntent{
		Desired:   IntentDesiredRunning,
		Reason:    IntentReasonInstall,
		UpdatedAt: now.Add(-time.Hour),
	})

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if res.Changed || res.Wrote || len(res.Entries) != 1 {
		t.Fatalf("stale legacy running entry should only report a drop decision; res=%+v", res)
	}
	if got := res.Entries[0]; got.TaskName != stoppedTask || got.Action != MergeStopDroppedExpired || got.Reason != IntentReasonInstall {
		t.Fatalf("stale legacy running entry = %+v, want drop-expired for %s", got, stoppedTask)
	}
	stops := readSupervisorStopsFromDisk(t, stateDir)
	if got, ok := stops[stoppedTask]; !ok || got != subBlockStop {
		t.Fatalf("authoritative sub-block stop = (%+v,%v), want (%+v,true)", got, ok, subBlockStop)
	}
}

// ---------------------------------------------------------------------------
// Corrupt daemon-intent.json fails CLOSED (no merge-to-no-stops).
// ---------------------------------------------------------------------------

func TestRunDaemonIntentCollapse_CorruptDaemonIntentFailsClosed(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	// Write garbage to daemon-intent.json.
	if err := os.WriteFile(filepath.Join(stateDir, "daemon-intent.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt daemon-intent.json: %v", err)
	}
	if _, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: time.Now().UTC()}); err == nil {
		t.Fatalf("expected fail-closed error on corrupt daemon-intent.json; got nil")
	}
}

// Missing daemon-intent.json → no stops, no error, no write.
func TestRunDaemonIntentCollapse_MissingDaemonIntentNoOp(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"),
		&SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if res.Wrote || res.Changed {
		t.Fatalf("missing daemon-intent.json must be a no-op: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Unified precedence helpers (the shape the 5 readers consume).
// ---------------------------------------------------------------------------

// Phase 4-E2 precedence FLIP: the sub-block is authoritative; a present (even
// empty) daemon-intent.json no longer overrides it. (Was
// TestUnifiedStopsFile_LiveDaemonIntentWinsWhenPresent under E1.)
func TestUnifiedStopsFile_E2_SubBlockWinsOverPresentDaemonIntent(t *testing.T) {
	sup := &SupervisorIntentFile{Stops: map[string]DaemonIntent{
		`\mcp-local-hub-x`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()},
	}}
	// A present-but-empty daemon-intent.json must be IGNORED after E2 → the
	// sub-block stop for x STAYS.
	live := &DaemonIntentFile{Tasks: map[string]DaemonIntent{}}
	got := UnifiedStopsFile(sup, live)
	if _, ok := got.Tasks[`\mcp-local-hub-x`]; !ok {
		t.Fatalf("E2: sub-block stop must survive a present (empty) daemon-intent.json")
	}
}

func TestUnifiedStopsFile_FallsBackToSubBlockWhenDaemonIntentAbsent(t *testing.T) {
	sup := &SupervisorIntentFile{Stops: map[string]DaemonIntent{
		`\mcp-local-hub-x`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()},
	}}
	got := UnifiedStopsFile(sup, nil)
	if _, ok := got.Tasks[`\mcp-local-hub-x`]; !ok {
		t.Fatalf("absent daemon-intent.json should fall back to the stops sub-block")
	}
}

func TestUnifiedStopsFile_NeverNil(t *testing.T) {
	if got := UnifiedStopsFile(nil, nil); got == nil || got.Tasks == nil {
		t.Fatalf("UnifiedStopsFile must return a non-nil file with a non-nil Tasks map")
	}
}

// StopsAsDaemonIntentFile aliases the sub-block (read-only view).
func TestStopsAsDaemonIntentFile_ViewsSubBlock(t *testing.T) {
	sup := &SupervisorIntentFile{Stops: map[string]DaemonIntent{
		`\mcp-local-hub-x`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()},
	}}
	got := sup.StopsAsDaemonIntentFile()
	if _, ok := got.Tasks[`\mcp-local-hub-x`]; !ok {
		t.Fatalf("StopsAsDaemonIntentFile lost the sub-block entry")
	}
	if sup.StopsAsDaemonIntentFile() == nil {
		t.Fatalf("nil result")
	}
	if (&SupervisorIntentFile{}).StopsAsDaemonIntentFile().Tasks == nil {
		t.Fatalf("empty intent must still yield a non-nil Tasks map")
	}
}

// ---------------------------------------------------------------------------
// TryReadUnifiedStops — the tray/GUI-side reader source (readers #4 + #5).
// ---------------------------------------------------------------------------

// Phase 4-E2: TryReadUnifiedStops reads ONLY the sub-block. A (stale)
// daemon-intent.json is IGNORED. (Was TestTryReadUnifiedStops_LiveDaemonIntentWins
// under E1.)
func TestTryReadUnifiedStops_E2_SubBlockIsSoleSource(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()

	// supervisor-intent stops sub-block has a stop for x...
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"),
		&SupervisorIntentFile{Version: 1, Stops: map[string]DaemonIntent{
			`\mcp-local-hub-x`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
		}}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	// ...and a STALE leftover daemon-intent.json has a stop for a DIFFERENT
	// task y. After E2 it must be ignored.
	seedDaemonIntent(t, `\mcp-local-hub-y`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	})

	res := NewAPI().TryReadUnifiedStops(250 * time.Millisecond)
	if res.State != IntentStateValid {
		t.Fatalf("want valid state, got %q (err=%v)", res.State, res.Err)
	}
	// Sub-block is authoritative → x is a stop; the stale daemon-intent.json y
	// is ignored.
	if _, ok := res.File.Tasks[`\mcp-local-hub-x`]; !ok {
		t.Fatalf("sub-block stop x missing: %+v", res.File.Tasks)
	}
	if _, ok := res.File.Tasks[`\mcp-local-hub-y`]; ok {
		t.Fatalf("stale daemon-intent.json stop y leaked through — must be ignored after E2")
	}
}

func TestTryReadUnifiedStops_FallsBackToSubBlockWhenNoDaemonIntent(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()
	// No daemon-intent.json on disk; supervisor stops sub-block carries x.
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"),
		&SupervisorIntentFile{Version: 1, Stops: map[string]DaemonIntent{
			`\mcp-local-hub-x`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
		}}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	res := NewAPI().TryReadUnifiedStops(250 * time.Millisecond)
	if res.State != IntentStateValid {
		t.Fatalf("want valid state (sub-block has a stop), got %q", res.State)
	}
	if _, ok := res.File.Tasks[`\mcp-local-hub-x`]; !ok {
		t.Fatalf("fallback sub-block stop x missing: %+v", res.File.Tasks)
	}
}

// Round-trip: a pre-E1 supervisor-intent.json (no stops field) decodes with a
// nil Stops map, and re-encodes WITHOUT a stops key (omitempty) — additive.
func TestSupervisorIntent_StopsOmitemptyRoundTrip(t *testing.T) {
	raw := `{"version":1,"updated_at":"","daemons":[],"strict_mode":false}`
	var f SupervisorIntentFile
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("decode legacy intent: %v", err)
	}
	if f.Stops != nil {
		t.Fatalf("legacy file should decode with nil Stops; got %+v", f.Stops)
	}
	out, err := json.Marshal(&f)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(out) != `{"version":1,"updated_at":"","daemons":[],"strict_mode":false}` {
		t.Fatalf("omitempty stops key leaked into output: %s", out)
	}
}

// TestRunDaemonIntentCollapse_PreservesConcurrentSupervisorIntentEdit is the
// P2-A lost-update regression test. A concurrent supervisor-intent.json writer
// (InstallParsedManifest / serena_intent_repair / register_supervisor / the
// autostart shim) lands BETWEEN the collapse's top-of-pass supervisor-intent
// read and its write. Before the fix, the collapse wrote back its STALE
// whole-struct snapshot, silently reverting the concurrent writer's
// Daemons / StrictMode / MaintenanceTimers edits. The fix re-reads
// supervisor-intent.json FRESH under the supervisor-intent flock and applies
// ONLY the recomputed Stops sub-block, so the concurrent non-Stops edit
// survives.
//
// The concurrent writer is simulated deterministically via the
// collapseAfterFirstSupervisorReadHook test seam, which fires exactly in the
// vulnerable window.
func TestRunDaemonIntentCollapse_PreservesConcurrentSupervisorIntentEdit(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()
	supPath := filepath.Join(stateDir, "supervisor-intent.json")

	// Seed: supervisor-intent.json with the OLD descriptor set + StrictMode off.
	if err := WriteSupervisorIntent(supPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: `\mcp-local-hub-old-default`, Server: "old", Daemon: "default", Port: 9201,
		}},
		StrictMode: false,
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	// Seed an active stop so the merge actually writes.
	seedDaemonIntent(t, `\mcp-local-hub-paper-search-default`, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	})

	// Concurrent writer: after the collapse's first read, REPLACE the
	// supervisor-intent.json with a NEW descriptor set + StrictMode on (an edit
	// the collapse must preserve). Fires exactly once.
	var hookFired bool
	collapseAfterFirstSupervisorReadHook = func() {
		hookFired = true
		if err := WriteSupervisorIntent(supPath, &SupervisorIntentFile{
			Version: 1,
			Daemons: []SupervisorDaemon{{
				TaskName: `\mcp-local-hub-new-default`, Server: "new", Daemon: "default", Port: 9202,
			}},
			StrictMode: true,
		}); err != nil {
			t.Errorf("concurrent supervisor-intent write: %v", err)
		}
	}
	defer func() { collapseAfterFirstSupervisorReadHook = nil }()

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !hookFired {
		t.Fatalf("concurrent-writer hook never fired — the test seam regressed")
	}
	if !res.Wrote {
		t.Fatalf("expected the merge to write (active stop present); res=%+v", res)
	}

	// The committed file must carry the CONCURRENT writer's edit (new descriptor
	// + StrictMode on), NOT the stale snapshot the collapse first read.
	got, err := ReadSupervisorIntent(supPath)
	if err != nil {
		t.Fatalf("read supervisor-intent.json after merge: %v", err)
	}
	if len(got.Daemons) != 1 || got.Daemons[0].TaskName != `\mcp-local-hub-new-default` || got.Daemons[0].Port != 9202 {
		t.Fatalf("P2-A lost update: collapse clobbered concurrent Daemons edit; got %+v", got.Daemons)
	}
	if !got.StrictMode {
		t.Fatalf("P2-A lost update: collapse clobbered concurrent StrictMode=true edit; got StrictMode=%v", got.StrictMode)
	}
	// AND the merge's own job (the stop) must still be applied onto the fresh struct.
	if _, ok := got.Stops[`\mcp-local-hub-paper-search-default`]; !ok {
		t.Fatalf("merge did not apply the stop onto the fresh struct: %+v", got.Stops)
	}
}

func TestRunDaemonIntentCollapse_DeletesLegacyIntentWhenFreshRereadAlreadyMerged(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()
	supPath := filepath.Join(stateDir, "supervisor-intent.json")
	daemonPath := filepath.Join(stateDir, "daemon-intent.json")
	task := `\mcp-local-hub-paper-search-default`
	stop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now}

	if err := WriteSupervisorIntent(supPath, &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	seedDaemonIntent(t, task, stop)

	var hookFired bool
	collapseAfterFirstSupervisorReadHook = func() {
		hookFired = true
		if err := WriteSupervisorIntent(supPath, &SupervisorIntentFile{
			Version: 1,
			Stops: map[string]DaemonIntent{
				task: stop,
			},
		}); err != nil {
			t.Errorf("concurrent supervisor-intent write: %v", err)
		}
	}
	defer func() { collapseAfterFirstSupervisorReadHook = nil }()

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !hookFired {
		t.Fatalf("concurrent-writer hook never fired")
	}
	if !res.Changed || !res.Wrote {
		t.Fatalf("fresh reread already had the merged stop, but collapse must write the missing watermark; res=%+v", res)
	}
	if !res.DeletedLegacyFile {
		t.Fatalf("fresh no-op merge must still delete merged legacy daemon-intent.json; res=%+v", res)
	}
	if _, err := os.Stat(daemonPath); !os.IsNotExist(err) {
		t.Fatalf("daemon-intent.json survived fresh no-op merge; stat err=%v", err)
	}
	if got := readSupervisorStopsFromDisk(t, stateDir)[task]; got.Reason != IntentReasonUserStop || !got.UpdatedAt.Equal(now) {
		t.Fatalf("merged stop missing or mutated after cleanup: %+v", got)
	}
	assertDaemonIntentEqual(t, readSupervisorLegacyStopWatermarksFromDisk(t, stateDir)[task], stop)
}

func TestRunDaemonIntentCollapse_DeletesLegacyIntentWhenSubBlockStopIsNewer(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Date(2026, 6, 12, 11, 0, 0, 0, time.UTC)
	supPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	daemonPath := filepath.Join(stateDir, intentFileLeaf)
	task := `\mcp-local-hub-paper-search-default`
	legacyStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Hour)}
	newerStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now}

	if err := WriteSupervisorIntent(supPath, &SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			task: newerStop,
		},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	seedDaemonIntent(t, task, legacyStop)

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.Changed || !res.Wrote {
		t.Fatalf("newer sub-block stop should win and collapse should write the accounted legacy watermark; res=%+v", res)
	}
	if !res.DeletedLegacyFile {
		t.Fatalf("older superseded daemon-intent.json should be deleted; res=%+v", res)
	}
	if _, err := os.Stat(daemonPath); !os.IsNotExist(err) {
		t.Fatalf("daemon-intent.json survived newer sub-block cleanup; stat err=%v", err)
	}
	if got := readSupervisorStopsFromDisk(t, stateDir)[task]; !daemonIntentRecordsEqual(got, newerStop) {
		t.Fatalf("newer sub-block stop = %+v, want %+v", got, newerStop)
	}
	assertDaemonIntentEqual(t, readSupervisorLegacyStopWatermarksFromDisk(t, stateDir)[task], legacyStop)
}

func TestDeleteLegacyDaemonIntentIfMerged_RefusesWhenLegacyStopIsNewerThanSubBlock(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Date(2026, 6, 12, 11, 0, 0, 0, time.UTC)
	supPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	daemonPath := filepath.Join(stateDir, intentFileLeaf)
	task := `\mcp-local-hub-paper-search-default`
	subBlockStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now.Add(-time.Hour)}
	legacyStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now}

	if err := WriteSupervisorIntent(supPath, &SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			task: subBlockStop,
		},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	seedDaemonIntent(t, task, legacyStop)

	deleted, err := deleteLegacyDaemonIntentIfMerged(stateDir, supPath, daemonPath, &DaemonIntentFile{
		Tasks: map[string]DaemonIntent{task: legacyStop},
	}, now)
	if err != nil {
		t.Fatalf("deleteLegacyDaemonIntentIfMerged: %v", err)
	}
	if deleted {
		t.Fatal("deleteLegacyDaemonIntentIfMerged deleted a newer legacy stop before merge")
	}
	if _, err := os.Stat(daemonPath); err != nil {
		t.Fatalf("daemon-intent.json should remain when legacy stop is newer; stat err=%v", err)
	}
}

// TestPruneOldPreCollapseBackups_KeepsNewestN is the P3-2 retention test:
// pruneOldPreCollapseBackups must keep exactly the newest preCollapseBackupRetention
// directories (by lexicographic timestamp suffix, which is chronological) and
// os.RemoveAll the rest. Drives the helper directly with synthetic backup dirs
// so the test does not depend on the merge's ~5 MB copy cost.
func TestPruneOldPreCollapseBackups_KeepsNewestN(t *testing.T) {
	stateDir := t.TempDir()
	// 8 synthetic backup dirs with ascending, fixed-width, colon-free
	// timestamp suffixes (same layout quarantineSuffixLayout produces).
	var all []string
	for i := 0; i < 8; i++ {
		// 2026-06-10T00-00-0Ni-style suffix: monotonic + fixed width.
		name := preCollapseBackupPrefix + "2026-06-10T00-00-0" + string(rune('0'+i)) + "Z"
		dir := filepath.Join(stateDir, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		// Drop a file inside so RemoveAll has real content to clear.
		if err := os.WriteFile(filepath.Join(dir, "supervisor-intent.json"), []byte("{}"), 0o600); err != nil {
			t.Fatalf("seed file in %s: %v", name, err)
		}
		all = append(all, dir)
	}
	// A stray FILE sharing the prefix must NOT be touched (only dirs are pruned).
	strayFile := filepath.Join(stateDir, preCollapseBackupPrefix+"2026-06-10T00-00-099Z-stray.txt")
	if err := os.WriteFile(strayFile, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("seed stray file: %v", err)
	}

	pruneOldPreCollapseBackups(stateDir, preCollapseBackupRetention)

	// The newest 5 (indices 3..7) survive; the oldest 3 (indices 0..2) are gone.
	for i, dir := range all {
		_, err := os.Stat(dir)
		shouldExist := i >= len(all)-preCollapseBackupRetention
		if shouldExist && err != nil {
			t.Errorf("backup %d (%s) should have survived, but is gone: %v", i, filepath.Base(dir), err)
		}
		if !shouldExist && err == nil {
			t.Errorf("backup %d (%s) should have been pruned, but survives", i, filepath.Base(dir))
		}
	}
	// Stray file untouched.
	if _, err := os.Stat(strayFile); err != nil {
		t.Errorf("stray prefix-sharing file was pruned; want untouched: %v", err)
	}
}

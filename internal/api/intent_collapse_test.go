package api

import (
	"encoding/json"
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

// A re-enabled daemon (stop cleared from daemon-intent.json) drops the stale
// baseline so it is not left suppressed by a prior merge.
func TestRunDaemonIntentCollapse_ClearedStopDropsBaseline(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()
	now := time.Now().UTC()

	// Pre-stage a supervisor-intent.json that ALREADY has a baseline stop for
	// paper-search (as if a prior merge ran).
	intent := &SupervisorIntentFile{
		Version: 1,
		Stops: map[string]DaemonIntent{
			`\mcp-local-hub-paper-search-default`: {
				Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
			},
		},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	// daemon-intent.json now says the daemon is RUNNING (operator re-enabled).
	seedDaemonIntent(t, `\mcp-local-hub-paper-search-default`, DaemonIntent{
		Desired: IntentDesiredRunning, Reason: IntentReasonInstall, UpdatedAt: now.Add(time.Minute),
	})

	res, err := RunDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{Now: now})
	if err != nil {
		t.Fatalf("RunDaemonIntentCollapse: %v", err)
	}
	if !res.Changed {
		t.Fatalf("expected a change (stale baseline stop should be dropped)")
	}
	stops := readSupervisorStopsFromDisk(t, stateDir)
	if _, ok := stops[`\mcp-local-hub-paper-search-default`]; ok {
		t.Fatalf("stale baseline stop not dropped after re-enable: %+v", stops)
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

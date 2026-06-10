package api

// Spec §4 Phase A.1 (STOP supervisor-aware) — tests for the
// stopSupervisorOwnedDaemons pass and its wiring into Stop / StopAll.
// Same hermetic seams the restart_supervisor tests use:
// SetDaemonStateRootForTest for state reads/writes, LOCALAPPDATA /
// XDG_STATE_HOME redirect so DefaultRegistryPath (workspaceTasksByName)
// never touches the real registry, stopSchedulerFactory for the OS
// scheduler, killByPortFn for the kill path, and the
// supervisorReconcileApplyFn seam for the IPC reconcile.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/scheduler"
)

const stopSupervisorTestTask = `\mcp-local-hub-time-default`

// stopSupervisorTestSetup builds the hermetic Stop fixture: hardened
// per-test state dir (supervisor-intent read passes the parent-dir
// gate), audit recording seam, counted no-op kill seam, and a fake
// scheduler behind stopSchedulerFactory. Returns the kill counter and
// the fake scheduler for assertions.
func stopSupervisorTestSetup(t *testing.T, intent *SupervisorIntentFile, schedTasks []scheduler.TaskStatus) (*int32, *restartAllFakeScheduler) {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	restore := SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	if intent != nil {
		if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
			t.Fatalf("seed supervisor-intent.json: %v", err)
		}
	}
	installRecordingAudit(t, &recordingAuditWriter{})
	kills := stopFakeKillCounter(t)
	fake := &restartAllFakeScheduler{tasks: schedTasks}
	origFactory := stopSchedulerFactory
	stopSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	t.Cleanup(func() { stopSchedulerFactory = origFactory })
	return kills, fake
}

func stopSupervisorTestIntent() *SupervisorIntentFile {
	return &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: stopSupervisorTestTask,
			Server:   "time",
			Daemon:   "default",
			Port:     9128,
		}},
	}
}

// TestStopUsesSupervisorReconcileAndSkipsKill: Stop on a server with a
// matching supervisor-intent row must (1) write Desired=stopped intent
// BEFORE dialing the reconcile (the reconcile reads it from disk), (2)
// dial exactly one reconcile with apply=true, and (3) NOT taskkill the
// handled task even when a scheduler row with the same name exists —
// taskkill is the non-clean exit the supervisor reaper respawns.
//
// The reconcile stub returns REALISTIC per-target drift (#279 opus gate):
// the regular `time-default` global daemon classifies needs_manual_review
// on the terminate direction (it is NOT a proxy-shaped descriptor, so the
// supervisor posts NO EvIntentUpdate and it converges only via the ~60s
// IntentWatcher), while a proxy-shaped `time-proxy` descriptor classifies
// post_ev_intent_update (truly dispatched). The result rows must reflect
// that split: the regular target carries Code=DeferredToIntentWatcherCode
// with EMPTY Err (durable, not a failure), and the proxy target is a plain
// success row.
func TestStopUsesSupervisorReconcileAndSkipsKill(t *testing.T) {
	const proxyTask = `\mcp-local-hub-time-proxy`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: stopSupervisorTestTask, Server: "time", Daemon: "default", Port: 9128},
			// Proxy-shaped supervisor-owned descriptor (same server so the
			// Stop("time", "") scope selects both). Its argv is what the
			// cli-side classifier keys proxy-ownership on; the api side does
			// not inspect Args, so the drift stub below drives the action.
			{TaskName: proxyTask, Server: "time", Daemon: "proxy", Port: 9129,
				Args: []string{"daemon", "serena-proxy"}},
		},
	}
	kills, fake := stopSupervisorTestSetup(t, intent,
		[]scheduler.TaskStatus{{Name: stopSupervisorTestTask}})

	var reconcileCalls int32
	var gotApply bool
	var intentDesiredAtReconcile string
	restore := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		atomic.AddInt32(&reconcileCalls, 1)
		gotApply = apply
		// Read-back assertion: the stop intent must already be on disk
		// when the reconcile fires, otherwise the supervisor would see
		// desired=running and stop nothing.
		res := NewAPI().ReadDaemonIntent()
		intentDesiredAtReconcile = res.File.Tasks[stopSupervisorTestTask].Desired
		// Realistic drift: the regular daemon got nothing posted
		// (needs_manual_review); only the proxy was dispatched.
		return ReconcileResponse{
			AppliedCount: 1,
			Drift: []DriftEntry{
				{TaskName: stopSupervisorTestTask, Action: ReconcileActionNeedsManualReview},
				{TaskName: proxyTask, Action: ReconcileActionPostEvIntentUpdate},
			},
		}, nil
	})
	defer restore()

	results, err := NewAPI().Stop("time", "")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := atomic.LoadInt32(&reconcileCalls); got != 1 {
		t.Fatalf("reconcile calls = %d, want 1", got)
	}
	if !gotApply {
		t.Fatal("reconcile dialed with apply=false, want apply=true")
	}
	if intentDesiredAtReconcile != IntentDesiredStopped {
		t.Fatalf("daemon-intent at reconcile time = %q, want %q (intent must be on disk BEFORE the reconcile)",
			intentDesiredAtReconcile, IntentDesiredStopped)
	}
	if got := atomic.LoadInt32(kills); got != 0 {
		t.Fatalf("killByPortFn calls = %d, want 0 (supervisor-handled task must not be taskkilled)", got)
	}
	if len(fake.stopNames) != 0 {
		t.Fatalf("scheduler Stop calls = %v, want none for the handled task", fake.stopNames)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want two supervisor rows (regular + proxy)", results)
	}
	byTask := map[string]RestartResult{}
	for _, r := range results {
		byTask[r.TaskName] = r
	}
	regular, ok := byTask[stopSupervisorTestTask]
	if !ok {
		t.Fatalf("missing regular-daemon row in %+v", results)
	}
	if regular.Err != "" {
		t.Fatalf("regular row Err = %q, want empty (deferred is not a failure)", regular.Err)
	}
	if regular.Code != DeferredToIntentWatcherCode {
		t.Fatalf("regular row Code = %q, want %q (no EvIntentUpdate posted; IntentWatcher converges)",
			regular.Code, DeferredToIntentWatcherCode)
	}
	proxy, ok := byTask[proxyTask]
	if !ok {
		t.Fatalf("missing proxy-daemon row in %+v", results)
	}
	if proxy.Err != "" || proxy.Code != "" {
		t.Fatalf("proxy row = %+v, want plain success (empty Err + empty Code; truly dispatched)", proxy)
	}
}

// TestStopFallsBackToKillPathWhenSupervisorIPCUnavailable: a dead
// supervisor (ErrSupervisorIPCUnavailable) means nothing will respawn a
// killed daemon, so the legacy kill path is correct AND curing — it must
// run.
func TestStopFallsBackToKillPathWhenSupervisorIPCUnavailable(t *testing.T) {
	kills, fake := stopSupervisorTestSetup(t, stopSupervisorTestIntent(),
		[]scheduler.TaskStatus{{Name: stopSupervisorTestTask}})

	restore := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: dial: %w", ErrSupervisorIPCUnavailable)
	})
	defer restore()

	results, err := NewAPI().Stop("time", "")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := atomic.LoadInt32(kills); got != 1 {
		t.Fatalf("killByPortFn calls = %d, want 1 (legacy kill path must run when the supervisor is down)", got)
	}
	if len(fake.stopNames) != 1 || fake.stopNames[0] != stopSupervisorTestTask {
		t.Fatalf("scheduler Stop calls = %v, want [%s]", fake.stopNames, stopSupervisorTestTask)
	}
	if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask || results[0].Err != "" {
		t.Fatalf("results = %+v, want one legacy kill success row", results)
	}
}

// TestStopReconcileFailureKeepsSupervisorOwnedUnkilled: when the
// supervisor is ALIVE but the reconcile fails, falling through to
// taskkill would hand the reaper a non-clean exit to respawn — the exact
// churn this fix kills. The handled tasks must surface as error rows
// instead, with zero kills.
func TestStopReconcileFailureKeepsSupervisorOwnedUnkilled(t *testing.T) {
	kills, fake := stopSupervisorTestSetup(t, stopSupervisorTestIntent(),
		[]scheduler.TaskStatus{{Name: stopSupervisorTestTask}})

	restore := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, errors.New("reconcile handler exploded")
	})
	defer restore()

	results, err := NewAPI().Stop("time", "")
	if err != nil {
		t.Fatalf("Stop: %v (reconcile failure must surface as per-row errors, not a hard error)", err)
	}
	if got := atomic.LoadInt32(kills); got != 0 {
		t.Fatalf("killByPortFn calls = %d, want 0 (a live supervisor would respawn a taskkilled daemon)", got)
	}
	if len(fake.stopNames) != 0 {
		t.Fatalf("scheduler Stop calls = %v, want none", fake.stopNames)
	}
	if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask {
		t.Fatalf("results = %+v, want one error row for the supervisor-owned task", results)
	}
	if !strings.Contains(results[0].Err, "reconcile handler exploded") {
		t.Fatalf("results[0].Err = %q, want the reconcile failure surfaced", results[0].Err)
	}
}

// TestStopAllRecordsIntentThenReconciles: StopAll historically recorded
// NO stop intent — the supervisor pass must write Desired=stopped for
// its targets FIRST (asserted via read-back inside the reconcile stub),
// then reconcile, then run the legacy loop skipping handled names.
func TestStopAllRecordsIntentThenReconciles(t *testing.T) {
	const legacyTask = `\mcp-local-hub-memory-default`
	kills, fake := stopSupervisorTestSetup(t, stopSupervisorTestIntent(),
		[]scheduler.TaskStatus{
			{Name: stopSupervisorTestTask},
			{Name: legacyTask},
		})

	var intentDesiredAtReconcile string
	var gotApply bool
	restore := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		gotApply = apply
		res := NewAPI().ReadDaemonIntent()
		intentDesiredAtReconcile = res.File.Tasks[stopSupervisorTestTask].Desired
		// Realistic drift: the lone regular daemon converges via the
		// IntentWatcher (no EvIntentUpdate posted).
		return ReconcileResponse{
			Drift: []DriftEntry{
				{TaskName: stopSupervisorTestTask, Action: ReconcileActionNeedsManualReview},
			},
		}, nil
	})
	defer restore()

	results, err := NewAPI().StopAll()
	if err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if !gotApply {
		t.Fatal("reconcile dialed with apply=false, want apply=true")
	}
	if intentDesiredAtReconcile != IntentDesiredStopped {
		t.Fatalf("daemon-intent at reconcile time = %q, want %q (StopAll must record stop intent BEFORE reconciling)",
			intentDesiredAtReconcile, IntentDesiredStopped)
	}
	// Legacy task killed; supervisor-handled task skipped.
	if got := atomic.LoadInt32(kills); got != 1 {
		t.Fatalf("killByPortFn calls = %d, want 1 (legacy task only)", got)
	}
	if len(fake.stopNames) != 1 || fake.stopNames[0] != legacyTask {
		t.Fatalf("scheduler Stop calls = %v, want [%s]", fake.stopNames, legacyTask)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want supervisor row + legacy row", results)
	}
	if results[0].TaskName != stopSupervisorTestTask || results[0].Err != "" {
		t.Fatalf("results[0] = %+v, want supervisor row (empty Err)", results[0])
	}
	// The lone regular daemon converges via the IntentWatcher, so its row
	// is success-but-deferred (#279 opus gate), not a plain success.
	if results[0].Code != DeferredToIntentWatcherCode {
		t.Fatalf("results[0].Code = %q, want %q (regular daemon → watcher-deferred)",
			results[0].Code, DeferredToIntentWatcherCode)
	}
	// The legacy task goes through the kill loop (not supervisor-owned), so
	// it stays a plain success row with no deferred Code.
	if results[1].TaskName != legacyTask || results[1].Err != "" || results[1].Code != "" {
		t.Fatalf("results[1] = %+v, want legacy plain-success row", results[1])
	}
}

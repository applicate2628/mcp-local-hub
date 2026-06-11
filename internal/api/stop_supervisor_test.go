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
	"time"

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
// Under the no-legacy ownership model (spec §0.2) EVERY supervisor-intent
// row is supervisor-owned, so the reconcile's drift classifier posts
// EvIntentUpdate for the regular `time-default` global daemon exactly as it
// does for a proxy-shaped `time-proxy` descriptor — both classify
// post_ev_intent_update (truly dispatched). The result rows must therefore
// BOTH be plain success rows (empty Err + empty Code): the synchronous SM
// dispatch happened for the whole fleet, not just the proxies.
func TestStopUsesSupervisorReconcileAndSkipsKill(t *testing.T) {
	const proxyTask = `\mcp-local-hub-time-proxy`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: stopSupervisorTestTask, Server: "time", Daemon: "default", Port: 9128},
			// Second supervisor-intent descriptor (same server so the
			// Stop("time", "") scope selects both). The api side does not
			// inspect Args; the drift stub below drives the per-target action.
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
		// desired=running and stop nothing. Phase 4-E2: the stop lives in
		// the supervisor-intent.json `stops` sub-block (the sole source).
		if di, ok := lookupSupervisorStop(stopSupervisorTestTask); ok {
			intentDesiredAtReconcile = di.Desired
		}
		// No-legacy drift: every supervisor-intent row is dispatched through
		// the SM (post_ev_intent_update) — regular daemon included.
		return ReconcileResponse{
			AppliedCount: 2,
			Drift: []DriftEntry{
				{TaskName: stopSupervisorTestTask, Action: ReconcileActionPostEvIntentUpdate},
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
	if regular.Err != "" || regular.Code != "" {
		t.Fatalf("regular row = %+v, want plain success (empty Err + empty Code; truly dispatched under no-legacy ownership)", regular)
	}
	proxy, ok := byTask[proxyTask]
	if !ok {
		t.Fatalf("missing proxy-daemon row in %+v", results)
	}
	if proxy.Err != "" || proxy.Code != "" {
		t.Fatalf("proxy row = %+v, want plain success (empty Err + empty Code; truly dispatched)", proxy)
	}
}

func TestStopSerenaSupervisorTargetRecordsStopIntentForSentinelRow(t *testing.T) {
	const serenaTask = `\mcp-local-hub-serena-deadbeef`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName:  serenaTask,
			Server:    "serena",
			Daemon:    "default",
			Workspace: `C:\work\alpha`,
			Port:      9500,
		}},
	}
	stopSupervisorTestSetup(t, intent, nil)

	var stopActiveAtReconcile bool
	restore := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		if di, ok := lookupSupervisorStop(serenaTask); ok {
			stopActiveAtReconcile, _ = di.IsActiveStop(time.Now().UTC())
		}
		return ReconcileResponse{
			AppliedCount: 1,
			Drift: []DriftEntry{
				{TaskName: serenaTask, Action: ReconcileActionPostEvIntentUpdate},
			},
		}, nil
	})
	defer restore()

	results, err := NewAPI().Stop("serena", "")
	if err != nil {
		t.Fatalf("Stop serena: %v", err)
	}
	if !stopActiveAtReconcile {
		t.Fatalf("serena sentinel stop intent was not active before supervisor reconcile")
	}
	if got, ok := lookupSupervisorStop(serenaTask); !ok {
		t.Fatalf("supervisor-intent stops sub-block missing serena sentinel row %s", serenaTask)
	} else if active, reason := got.IsActiveStop(time.Now().UTC()); !active || reason != IntentReasonUserStop {
		t.Fatalf("serena stop IsActiveStop=(%v,%q), want active user-stop; intent=%+v", active, reason, got)
	}
	if len(results) != 1 || results[0].TaskName != serenaTask || results[0].Err != "" {
		t.Fatalf("results = %+v, want one successful serena supervisor row", results)
	}
}

// TestStopKillsSupervisorDescriptorWhenSupervisorIPCUnavailable: a dead
// supervisor (ErrSupervisorIPCUnavailable) cannot reap supervisor-owned v0.6
// daemons, and fresh supervisor-owned daemons have no scheduler rows. The stop
// path must therefore kill the descriptor directly and mark the supervisor pass
// handled instead of silently falling through to the legacy scheduler loop.
func TestStopKillsSupervisorDescriptorWhenSupervisorIPCUnavailable(t *testing.T) {
	kills, fake := stopSupervisorTestSetup(t, stopSupervisorTestIntent(),
		[]scheduler.TaskStatus{{Name: stopSupervisorTestTask}})

	var forceKillPorts []int
	origForceKill := forceKillByPortFn
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		forceKillPorts = append(forceKillPorts, port)
		return portKillKilled, nil
	}
	t.Cleanup(func() { forceKillByPortFn = origForceKill })

	restore := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: dial: %w", ErrSupervisorIPCUnavailable)
	})
	defer restore()

	results, err := NewAPI().Stop("time", "")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := atomic.LoadInt32(kills); got != 0 {
		t.Fatalf("legacy killByPortFn calls = %d, want 0 (descriptor force-kill handles supervisor-owned IPC-down target)", got)
	}
	if len(forceKillPorts) != 1 || forceKillPorts[0] != 9128 {
		t.Fatalf("forceKillByPortFn ports = %v, want [9128]", forceKillPorts)
	}
	if len(fake.stopNames) != 0 {
		t.Fatalf("scheduler Stop calls = %v, want none for supervisor-handled IPC-down target", fake.stopNames)
	}
	if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask || results[0].Err != "" {
		t.Fatalf("results = %+v, want one descriptor kill success row", results)
	}
}

func TestStopSupervisorOwnedDaemons_IPCUnavailableKillsLoadedTargetsAndHandles(t *testing.T) {
	stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)

	restore := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: dial: %w", ErrSupervisorIPCUnavailable)
	})
	defer restore()

	var forceKillPorts []int
	origForceKill := forceKillByPortFn
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		forceKillPorts = append(forceKillPorts, port)
		return portKillKilled, nil
	}
	t.Cleanup(func() { forceKillByPortFn = origForceKill })

	origPID := stopForceKillPIDFn
	stopForceKillPIDFn = func(pid int) error {
		t.Fatalf("IPC-down stop must pass nil pidByTask and avoid PID fallback; got pid=%d", pid)
		return nil
	}
	t.Cleanup(func() { stopForceKillPIDFn = origPID })

	results, handled, err := stopSupervisorOwnedDaemons(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("stopSupervisorOwnedDaemons: %v", err)
	}
	if !handled {
		t.Fatal("handled=false, want true so the legacy scheduler fallback is not taken")
	}
	if len(forceKillPorts) != 1 || forceKillPorts[0] != 9128 {
		t.Fatalf("forceKillByPortFn ports = %v, want [9128]", forceKillPorts)
	}
	if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask || results[0].Err != "" {
		t.Fatalf("results = %+v, want one descriptor kill success row", results)
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
//
// This test also pins the DeferredToIntentWatcherCode HONESTY BACKSTOP
// under the no-legacy ownership model: while a live supervisor-owned daemon
// now classifies post_ev_intent_update (truly dispatched), an
// ALREADY-IDLE/settled daemon's terminate classifies no_op (nothing live to
// terminate), so its reconcile drift entry is NOT a post_ev_intent_update.
// The api-side honesty seam must still surface that as a success-but-deferred
// row (empty Err + Code=DeferredToIntentWatcherCode) rather than a false
// synchronous-dispatch claim. The drift stub below fabricates that no_op edge.
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
		// Phase 4-E2: the stop lives in the supervisor-intent.json stops
		// sub-block (the sole source), read here via lookupSupervisorStop.
		if di, ok := lookupSupervisorStop(stopSupervisorTestTask); ok {
			intentDesiredAtReconcile = di.Desired
		}
		// Honesty backstop: the lone daemon is already idle/settled, so its
		// terminate classifies no_op (nothing live to terminate) — NOT a
		// post_ev_intent_update. The api side must report it as deferred.
		return ReconcileResponse{
			Drift: []DriftEntry{
				{TaskName: stopSupervisorTestTask, Action: ReconcileActionNoOp},
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
	// The already-idle daemon's terminate is no_op, so its row is
	// success-but-deferred (the honesty backstop), not a plain success.
	if results[0].Code != DeferredToIntentWatcherCode {
		t.Fatalf("results[0].Code = %q, want %q (no_op edge → watcher-deferred)",
			results[0].Code, DeferredToIntentWatcherCode)
	}
	// The legacy task goes through the kill loop (not supervisor-owned), so
	// it stays a plain success row with no deferred Code.
	if results[1].TaskName != legacyTask || results[1].Err != "" || results[1].Code != "" {
		t.Fatalf("results[1] = %+v, want legacy plain-success row", results[1])
	}
}

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

func stopBatchResultForTest(command StopBatchCommandV1, settlements []StoppedSettlement) StopBatchResultV1 {
	return StopBatchResultV1{
		ProtocolVersion: command.ProtocolVersion,
		BatchID:         command.BatchID,
		Targets:         append([]StopBatchTargetV1(nil), command.Targets...),
		Settlements:     settlements,
	}
}

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

func builtinRouteIntentWithExtraRouteRow() *SupervisorIntentFile {
	return &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{
				TaskName: BuiltinRouteTaskName,
				Server:   BuiltinRouteServer,
				Daemon:   BuiltinRouteDaemonName,
				Port:     9137,
			},
			// This is malformed durable state: the reserved route server has one
			// valid daemon only. A targeted route stop must never widen to it.
			{
				TaskName: `\mcp-local-hub-route-other`,
				Server:   BuiltinRouteServer,
				Daemon:   "other",
				Port:     9138,
			},
		},
	}
}

func TestStopIntentTaskNamesForServer_BuiltinRouteExcludesExtraRouteRow(t *testing.T) {
	for _, daemonFilter := range []string{"", BuiltinRouteDaemonName} {
		t.Run("daemon="+daemonFilter, func(t *testing.T) {
			stopSupervisorTestSetup(t, builtinRouteIntentWithExtraRouteRow(), nil)

			got, err := stopIntentTaskNamesForServer(BuiltinRouteServer, daemonFilter)
			if err != nil {
				t.Fatalf("stopIntentTaskNamesForServer: %v", err)
			}
			want := strings.TrimPrefix(BuiltinRouteTaskName, `\`)
			if len(got) != 1 || got[0] != want {
				t.Fatalf("task names = %v, want exactly [%q]", got, want)
			}
		})
	}
}

func TestStopBuiltinRouteSupervisorOnlyExcludesExtraRowAndLegacyFallback(t *testing.T) {
	for _, daemonFilter := range []string{"", BuiltinRouteDaemonName} {
		t.Run("daemon="+daemonFilter, func(t *testing.T) {
			kills, fake := stopSupervisorTestSetup(t, builtinRouteIntentWithExtraRouteRow(), nil)

			// A successful supervisor route stop is complete ownership; the legacy
			// path would re-enter manifest resolution where "route" is reserved.
			origFactory := stopSchedulerFactory
			var schedulerFactoryCalls int32
			stopSchedulerFactory = func() (scheduler.Scheduler, error) {
				atomic.AddInt32(&schedulerFactoryCalls, 1)
				return fake, nil
			}
			t.Cleanup(func() { stopSchedulerFactory = origFactory })

			var gotTargets []StopBatchTargetV1
			restore := setSupervisorStopBatchHookForTest(func(_ context.Context, command StopBatchCommandV1) (StopBatchResultV1, error) {
				gotTargets = append([]StopBatchTargetV1(nil), command.Targets...)
				return stopBatchResultForTest(command, []StoppedSettlement{{
					TaskName: BuiltinRouteTaskName,
					State:    StoppedSettlementStopped,
					Reason:   StoppedSettlementReasonStopped,
				}}), nil
			})
			defer restore()

			results, err := NewAPI().Stop(BuiltinRouteServer, daemonFilter)
			if err != nil {
				t.Fatalf("Stop(route): %v", err)
			}
			if len(gotTargets) != 1 || gotTargets[0].TaskName != BuiltinRouteTaskName {
				t.Fatalf("stop_batch targets = %+v, want only %q", gotTargets, BuiltinRouteTaskName)
			}
			if got := atomic.LoadInt32(&schedulerFactoryCalls); got != 0 {
				t.Fatalf("legacy scheduler factory calls = %d, want 0 for supervisor-owned route", got)
			}
			if got := atomic.LoadInt32(kills); got != 0 {
				t.Fatalf("killByPortFn calls = %d, want 0 for supervisor-owned route", got)
			}
			if len(fake.stopNames) != 0 {
				t.Fatalf("legacy scheduler Stop calls = %v, want none", fake.stopNames)
			}
			if len(results) != 1 || results[0].TaskName != BuiltinRouteTaskName || results[0].Err != "" {
				t.Fatalf("results = %+v, want one successful built-in route result", results)
			}
		})
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

	var stopBatchCalls int32
	var gotCommand StopBatchCommandV1
	var intentDesiredAtReconcile string
	restore := setSupervisorStopBatchHookForTest(func(ctx context.Context, command StopBatchCommandV1) (StopBatchResultV1, error) {
		atomic.AddInt32(&stopBatchCalls, 1)
		gotCommand = command
		// Read-back assertion: the stop intent must already be on disk
		// when the reconcile fires, otherwise the supervisor would see
		// desired=running and stop nothing. Phase 4-E2: the stop lives in
		// the supervisor-intent.json `stops` sub-block (the sole source).
		if di, ok := lookupSupervisorStop(stopSupervisorTestTask); ok {
			intentDesiredAtReconcile = di.Desired
		}
		return stopBatchResultForTest(command, []StoppedSettlement{
			{TaskName: stopSupervisorTestTask, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped},
			{TaskName: proxyTask, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped},
		}), nil
	})
	defer restore()

	results, err := NewAPI().Stop("time", "")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := atomic.LoadInt32(&stopBatchCalls); got != 1 {
		t.Fatalf("stop_batch calls = %d, want 1", got)
	}
	if gotCommand.ProtocolVersion != 1 || len(gotCommand.Targets) != 2 {
		t.Fatalf("stop_batch command = %+v, want v1/two targets", gotCommand)
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
			Workspace: `<workspace-alpha>`,
			Port:      9500,
		}},
	}
	stopSupervisorTestSetup(t, intent, nil)

	var stopActiveAtReconcile bool
	restore := setSupervisorStopBatchHookForTest(func(ctx context.Context, command StopBatchCommandV1) (StopBatchResultV1, error) {
		if di, ok := lookupSupervisorStop(serenaTask); ok {
			stopActiveAtReconcile, _ = di.IsActiveStop(time.Now().UTC())
		}
		return stopBatchResultForTest(command, []StoppedSettlement{
			{TaskName: serenaTask, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped},
		}), nil
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

	restore := setSupervisorStopBatchHookForTest(func(context.Context, StopBatchCommandV1) (StopBatchResultV1, error) {
		return StopBatchResultV1{}, fmt.Errorf("supervisor IPC stop-batch: dial: %w", ErrSupervisorIPCUnavailable)
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

	restore := setSupervisorStopBatchHookForTest(func(context.Context, StopBatchCommandV1) (StopBatchResultV1, error) {
		return StopBatchResultV1{}, fmt.Errorf("supervisor IPC stop-batch: dial: %w", ErrSupervisorIPCUnavailable)
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

	restore := setSupervisorStopBatchHookForTest(func(context.Context, StopBatchCommandV1) (StopBatchResultV1, error) {
		return StopBatchResultV1{}, errors.New("reconcile handler exploded")
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
	var gotCommand StopBatchCommandV1
	restore := setSupervisorStopBatchHookForTest(func(ctx context.Context, command StopBatchCommandV1) (StopBatchResultV1, error) {
		gotCommand = command
		// Phase 4-E2: the stop lives in the supervisor-intent.json stops
		// sub-block (the sole source), read here via lookupSupervisorStop.
		if di, ok := lookupSupervisorStop(stopSupervisorTestTask); ok {
			intentDesiredAtReconcile = di.Desired
		}
		return stopBatchResultForTest(command, []StoppedSettlement{
			{TaskName: stopSupervisorTestTask, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped},
		}), nil
	})
	defer restore()

	results, err := NewAPI().StopAll()
	if err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if gotCommand.ProtocolVersion != 1 || len(gotCommand.Targets) != 1 {
		t.Fatalf("stop_batch command = %+v, want v1/one target", gotCommand)
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
	// The typed settlement proves stopped, so an already-idle daemon is a plain
	// success rather than a watcher-deferred row.
	if results[0].Code != "" {
		t.Fatalf("results[0].Code = %q, want empty code after terminal settlement", results[0].Code)
	}
	// The legacy task goes through the kill loop (not supervisor-owned), so
	// it stays a plain success row with no deferred Code.
	if results[1].TaskName != legacyTask || results[1].Err != "" || results[1].Code != "" {
		t.Fatalf("results[1] = %+v, want legacy plain-success row", results[1])
	}
}

// TestStopSupervisorHandledToleratesTypedSchedulerUnavailable pins the N1
// contract: once a supervisor stop batch durably settles the requested row,
// a typed legacy-scheduler outage cannot replace that committed result or
// trigger the legacy kill path. The four subtests cover Stop/StopAll at both
// factory and List fallback boundaries.
func TestStopSupervisorHandledToleratesTypedSchedulerUnavailable(t *testing.T) {
	typedUnavailable := fmt.Errorf("scheduler bridge: %w: protocol", scheduler.ErrUnavailable)

	for _, tc := range []struct {
		name    string
		stop    func(*API) ([]RestartResult, error)
		factory func() (func() (scheduler.Scheduler, error), *restartAllFakeScheduler)
	}{
		{
			name: "targeted_factory",
			stop: func(a *API) ([]RestartResult, error) { return a.Stop("time", "") },
			factory: func() (func() (scheduler.Scheduler, error), *restartAllFakeScheduler) {
				return func() (scheduler.Scheduler, error) { return nil, typedUnavailable }, nil
			},
		},
		{
			name: "targeted_list",
			stop: func(a *API) ([]RestartResult, error) { return a.Stop("time", "") },
			factory: func() (func() (scheduler.Scheduler, error), *restartAllFakeScheduler) {
				legacyScheduler := &restartAllFakeScheduler{listErr: typedUnavailable}
				return func() (scheduler.Scheduler, error) { return legacyScheduler, nil }, legacyScheduler
			},
		},
		{
			name: "all_factory",
			stop: func(a *API) ([]RestartResult, error) { return a.StopAll() },
			factory: func() (func() (scheduler.Scheduler, error), *restartAllFakeScheduler) {
				return func() (scheduler.Scheduler, error) { return nil, typedUnavailable }, nil
			},
		},
		{
			name: "all_list",
			stop: func(a *API) ([]RestartResult, error) { return a.StopAll() },
			factory: func() (func() (scheduler.Scheduler, error), *restartAllFakeScheduler) {
				legacyScheduler := &restartAllFakeScheduler{listErr: typedUnavailable}
				return func() (scheduler.Scheduler, error) { return legacyScheduler, nil }, legacyScheduler
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kills, _ := stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)
			restoreBatch := setSupervisorStopBatchHookForTest(func(_ context.Context, command StopBatchCommandV1) (StopBatchResultV1, error) {
				return stopBatchResultForTest(command, []StoppedSettlement{{
					TaskName: stopSupervisorTestTask,
					State:    StoppedSettlementStopped,
					Reason:   StoppedSettlementReasonStopped,
				}}), nil
			})
			t.Cleanup(restoreBatch)
			previousFactory := stopSchedulerFactory
			factory, legacyScheduler := tc.factory()
			stopSchedulerFactory = factory
			t.Cleanup(func() { stopSchedulerFactory = previousFactory })

			results, err := tc.stop(NewAPI())
			if err != nil {
				t.Fatalf("stop: %v", err)
			}
			if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask || results[0].Err != "" || results[0].Code != "" {
				t.Fatalf("results = %+v, want preserved durable supervisor success", results)
			}
			if got := atomic.LoadInt32(kills); got != 0 {
				t.Fatalf("killByPortFn calls = %d, want 0 after accepted supervisor result", got)
			}
			if legacyScheduler != nil && len(legacyScheduler.stopNames) != 0 {
				t.Fatalf("scheduler Stop calls = %v, want none after accepted supervisor result", legacyScheduler.stopNames)
			}
		})
	}
}

// TestStopSchedulerUnavailableRemainsFatalWithoutTypedSupervisorFallback
// guards both limits of the stop-scoped tolerance: textual lookalikes are not
// scheduler.ErrUnavailable, and a real typed outage remains fatal if the
// supervisor did not settle a target first.
func TestStopSchedulerUnavailableRemainsFatalWithoutTypedSupervisorFallback(t *testing.T) {
	typedUnavailable := fmt.Errorf("scheduler bridge: %w: protocol", scheduler.ErrUnavailable)

	for _, tc := range []struct {
		name       string
		seedIntent bool
		err        error
		stop       func(*API) ([]RestartResult, error)
	}{
		{
			name:       "targeted_untyped_same_text",
			seedIntent: true,
			err:        errors.New(typedUnavailable.Error()),
			stop:       func(a *API) ([]RestartResult, error) { return a.Stop("time", "") },
		},
		{
			name:       "all_untyped_same_text",
			seedIntent: true,
			err:        errors.New(typedUnavailable.Error()),
			stop:       func(a *API) ([]RestartResult, error) { return a.StopAll() },
		},
		{
			name: "targeted_no_supervisor_result",
			err:  typedUnavailable,
			stop: func(a *API) ([]RestartResult, error) { return a.Stop("time", "") },
		},
		{
			name: "all_no_supervisor_result",
			err:  typedUnavailable,
			stop: func(a *API) ([]RestartResult, error) { return a.StopAll() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var intent *SupervisorIntentFile
			if tc.seedIntent {
				intent = stopSupervisorTestIntent()
			}
			stopSupervisorTestSetup(t, intent, nil)
			if tc.seedIntent {
				restoreBatch := setSupervisorStopBatchHookForTest(func(_ context.Context, command StopBatchCommandV1) (StopBatchResultV1, error) {
					return stopBatchResultForTest(command, []StoppedSettlement{{
						TaskName: stopSupervisorTestTask,
						State:    StoppedSettlementStopped,
						Reason:   StoppedSettlementReasonStopped,
					}}), nil
				})
				t.Cleanup(restoreBatch)
			}
			previousFactory := stopSchedulerFactory
			stopSchedulerFactory = func() (scheduler.Scheduler, error) { return nil, tc.err }
			t.Cleanup(func() { stopSchedulerFactory = previousFactory })

			results, err := tc.stop(NewAPI())
			if err == nil {
				t.Fatalf("stop results = %+v, want scheduler error", results)
			}
			if errors.Is(tc.err, scheduler.ErrUnavailable) {
				if !errors.Is(err, scheduler.ErrUnavailable) {
					t.Fatalf("stop error = %v, want typed scheduler.ErrUnavailable", err)
				}
			} else if !errors.Is(err, tc.err) {
				t.Fatalf("stop error = %v, want original untyped scheduler error %v", err, tc.err)
			}
		})
	}
}

// A transport-level reconcile success is not a completed stop. The supervisor
// must return controller-owned terminal settlement for every selected target;
// otherwise the caller must fail loud instead of publishing Stopped while an
// owned listener may still be alive.
func TestStopSupervisorDispatchWithoutTerminalSettlementFailsLoud(t *testing.T) {
	stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)

	restore := setSupervisorStopBatchHookForTest(func(context.Context, StopBatchCommandV1) (StopBatchResultV1, error) {
		return StopBatchResultV1{}, nil
	})
	defer restore()

	results, handled, err := stopSupervisorOwnedDaemons(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("stopSupervisorOwnedDaemons: %v", err)
	}
	if !handled {
		t.Fatal("handled=false, want supervisor ownership retained")
	}
	if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask {
		t.Fatalf("results = %+v, want one target row", results)
	}
	if results[0].Err == "" {
		t.Fatalf("dispatch-only reconcile returned success without terminal stop settlement: %+v", results[0])
	}
	for _, want := range []string{"settlement", "incomplete"} {
		if !strings.Contains(strings.ToLower(results[0].Err), want) {
			t.Fatalf("settlement error = %q, want substring %q", results[0].Err, want)
		}
	}
}

func TestStopSupervisorStoppedSettlementPartialFailureIsPerTarget(t *testing.T) {
	const secondTask = `\mcp-local-hub-time-proxy`
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{
		{TaskName: stopSupervisorTestTask, Server: "time", Daemon: "default", Port: 9128},
		{TaskName: secondTask, Server: "time", Daemon: "proxy", Port: 9129},
	}}
	stopSupervisorTestSetup(t, intent, nil)

	restore := setSupervisorStopBatchHookForTest(func(_ context.Context, command StopBatchCommandV1) (StopBatchResultV1, error) {
		return stopBatchResultForTest(command, []StoppedSettlement{
			{TaskName: stopSupervisorTestTask, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped},
			{TaskName: secondTask, State: StoppedSettlementFailed, Reason: StoppedSettlementReasonListenerAlive, Error: "port still bound"},
		}), nil
	})
	defer restore()

	results, handled, err := stopSupervisorOwnedDaemons(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("stopSupervisorOwnedDaemons: %v", err)
	}
	if !handled || len(results) != 2 {
		t.Fatalf("handled/results = %v/%+v, want true/two target rows", handled, results)
	}
	byTask := map[string]RestartResult{}
	for _, result := range results {
		byTask[result.TaskName] = result
	}
	if got := byTask[stopSupervisorTestTask]; got.Err != "" {
		t.Fatalf("successful sibling = %+v, want empty Err", got)
	}
	if got := byTask[secondTask]; !strings.Contains(got.Err, "failed") || !strings.Contains(got.Err, "listener_alive") {
		t.Fatalf("failed sibling = %+v, want typed settlement failure", got)
	}
}

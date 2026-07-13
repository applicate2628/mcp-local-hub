package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// reallocSyncController builds a controller with a NON-running loop for
// deterministic synchronous handleLoopEvent drives (the counter / cap /
// fall-through / dwell logic). reallocFn + the worker are left UNwired, so a
// reallocation only records its counter + holds the daemon; no async port move
// happens (that end-to-end path is exercised by reallocRunningController).
func reallocSyncController(t *testing.T, daemons ...api.SupervisorDaemon) (*supervisorController, string) {
	t.Helper()
	tmp := apitest.HardenedTempDir(t)
	statePath := filepath.Join(tmp, "supervisor-state.json")
	eventsPath := filepath.Join(tmp, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		eventLoop:           api.NewEventLoop(64), // present but NOT run
		tracker:             NewDaemonRuntimeTracker(),
		events:              events,
		graceful:            &gracefulCounter{},
		daemonIntent:        newDaemonIntentCache(),
		spawn:               func(api.SupervisorDaemon) error { return nil },
		terminate:           func(api.SupervisorDaemon) error { return nil },
		statePath:           statePath,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: daemons})
	ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{})
	return ctrl, eventsPath
}

// driveBindRefusedExit resets the task to StRunning (where a freshly-spawned
// daemon lands right before its external bind fails) and synchronously feeds a
// bind-refused EvChildExit through handleLoopEvent.
func driveBindRefusedExit(ctrl *supervisorController, task string) {
	ctrl.smStates.Store(canonicalSupervisorTaskName(task), api.StRunning)
	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: task,
		Body:     map[string]any{"exit_code": exitBindRefused},
	})
}

// driveReallocCompletion simulates the off-loop worker posting a COMPLETED
// (Reallocated) outcome for d, moved to newPort, WITHOUT a carried whole-intent
// snapshot (the read-miss shape → FIX-3b targeted cache patch). FIX-6 records the cap
// slot ONLY on this completed outcome, so the sync-controller counting tests drive it
// explicitly (driveBindRefusedExit alone holds + dispatches but records nothing). The
// daemon is left StBackoffWaiting where maybeHandleBindRefusedExit's hold parked it.
func driveReallocCompletion(ctrl *supervisorController, d api.SupervisorDaemon, newPort int) {
	task := canonicalSupervisorTaskName(d.TaskName)
	ctrl.smStates.Store(task, api.StBackoffWaiting)
	ctrl.handleReallocApplied(api.LoopEvent{
		Kind:     evReallocApplied,
		TaskName: d.TaskName,
		Body: map[string]any{
			reallocResultOutcomeBodyKey: reallocOutcomeReallocated,
			reallocResultNewPortBodyKey: newPort,
		},
	})
}

// driveNonBindCrashFromRunning transitions the task StRunning → (non-Running) through
// the REAL SM via a non-bind exit-1 crash. Used to exercise the FIX-5b transition-side
// dwell reset (the SM-transition hook, not the sampling tick).
func driveNonBindCrashFromRunning(ctrl *supervisorController, task string) {
	ctrl.smStates.Store(canonicalSupervisorTaskName(task), api.StRunning)
	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: task,
		Body:     map[string]any{"exit_code": 1},
	})
}

func reallocCount(ctrl *supervisorController, task string) int {
	return ctrl.tracker.ReallocationCountInWindow(task, time.Now().UTC(), respawnFailureWindow)
}
func crashCount(ctrl *supervisorController, task string) int {
	return ctrl.tracker.CrashCountInWindow(task, time.Now().UTC(), respawnFailureWindow)
}

// TestRealloc_WithinCap_RecordsReallocationNoCrashIncrement: the first
// reallocationCap COMPLETED reallocations of a dynamic-pool proxy each record a
// reallocation and DO NOT touch the crash window, and hold the daemon in backoff.
// FIX-6: the bind-refused exit HOLDS the daemon but records no slot; the slot is
// recorded only when the worker posts the completed (Reallocated) outcome.
func TestRealloc_WithinCap_RecordsReallocationNoCrashIncrement(t *testing.T) {
	d := serenaProxyDescriptor()
	ctrl, _ := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	for i := 1; i <= reallocationCap; i++ {
		driveBindRefusedExit(ctrl, d.TaskName)
		// FIX-6: the hold alone records nothing; the cap slot lands on completion.
		if got := reallocCount(ctrl, task); got != i-1 {
			t.Fatalf("after the %d-th hold (pre-completion): reallocation count = %d, want %d (only a COMPLETED reallocation records a slot)", i, got, i-1)
		}
		if st, _ := ctrl.getSMStateCanonical(task); st != api.StBackoffWaiting {
			t.Fatalf("after the %d-th bind-refused hold: state = %v, want StBackoffWaiting (held)", i, st)
		}
		driveReallocCompletion(ctrl, d, 9160+i)
		if got := reallocCount(ctrl, task); got != i {
			t.Fatalf("after %d completed reallocations: reallocation count = %d, want %d", i, got, i)
		}
		if got := crashCount(ctrl, task); got != 0 {
			t.Fatalf("after %d completed reallocations: crash count = %d, want 0 (a within-cap reallocation must NOT fuel quarantine)", i, got)
		}
	}
}

// TestRealloc_CapExhausted_FallsThroughToCrashCounting: the (cap+1)-th
// bind-refused exit STOPS reallocating (counter frozen at cap) and falls through
// to the normal crash path (crash window increments), and a further march
// reaches quarantine at the existing 10-in-30-min threshold.
func TestRealloc_CapExhausted_FallsThroughToCrashCounting(t *testing.T) {
	d := serenaProxyDescriptor()
	ctrl, eventsPath := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)
	// FIX-9.1 pin: the on-loop cap-exhausted terminal emit MUST pass
	// foreignHolderPort=0 (FIX-1), so the foreign-holder resolver is never called on
	// the loop. Wire a PANICKING resolver so a regression that re-passes port>0 blows
	// up this test instead of silently stalling the loop.
	ctrl.reallocForeignHolderFn = func(int) (int, string) {
		panic("reallocForeignHolderFn must not be called on the loop from the cap-exhausted terminal emit (FIX-1)")
	}

	// Exhaust the reallocation budget via cap COMPLETED reallocations (FIX-6: the
	// slot is recorded on completion, not on the bind-refused hold).
	for i := 0; i < reallocationCap; i++ {
		driveBindRefusedExit(ctrl, d.TaskName)
		driveReallocCompletion(ctrl, d, 9160+i)
	}
	if reallocCount(ctrl, task) != reallocationCap || crashCount(ctrl, task) != 0 {
		t.Fatalf("after cap reallocations: realloc=%d crash=%d, want %d/0", reallocCount(ctrl, task), crashCount(ctrl, task), reallocationCap)
	}

	// The next bind-refused exit falls through to crash counting.
	driveBindRefusedExit(ctrl, d.TaskName)
	if got := reallocCount(ctrl, task); got != reallocationCap {
		t.Fatalf("after cap-exhausted exit: reallocation count = %d, want frozen at %d", got, reallocationCap)
	}
	if got := crashCount(ctrl, task); got != 1 {
		t.Fatalf("after cap-exhausted exit: crash count = %d, want 1 (fell through to normal crash counting)", got)
	}
	assertEventInLog(t, eventsPath, "quarantined-realloc-cap-exhausted")

	// Continue the crash march to quarantine (threshold total crashes).
	for crashCount(ctrl, task) < respawnQuarantineThreshold {
		driveBindRefusedExit(ctrl, d.TaskName)
	}
	if st, _ := ctrl.getSMStateCanonical(task); st != api.StQuarantined {
		t.Fatalf("after %d crashes: state = %v, want StQuarantined", respawnQuarantineThreshold, st)
	}
	// The reallocation budget never grew past the cap.
	if got := reallocCount(ctrl, task); got != reallocationCap {
		t.Fatalf("reallocation count = %d after quarantine, want frozen at %d", got, reallocationCap)
	}
}

// TestRealloc_DwellGate_TransientStRunningDoesNotReset is the KEY subtlety: a
// bind-refused daemon reaches StRunning between reallocations, but a dwell tick
// run while it is NOT dwelling (or below the dwell) must NOT reset the counter —
// otherwise the cap never engages (forever-flap). Only a FULL stable dwell resets.
func TestRealloc_DwellGate_TransientStRunningDoesNotReset(t *testing.T) {
	d := serenaProxyDescriptor()
	ctrl, _ := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	// Two COMPLETED reallocations, each transiting StRunning (driveBindRefusedExit
	// sets StRunning before the exit; FIX-6 records the slot on completion).
	driveBindRefusedExit(ctrl, d.TaskName)
	driveReallocCompletion(ctrl, d, 9160)
	driveBindRefusedExit(ctrl, d.TaskName)
	driveReallocCompletion(ctrl, d, 9161)
	if reallocCount(ctrl, task) != 2 {
		t.Fatalf("reallocation count = %d, want 2", reallocCount(ctrl, task))
	}

	now := time.Now().UTC()
	// The daemon is momentarily StRunning (as it would be between a spawn and its
	// bind fail). A dwell tick BELOW the stabilize dwell must not reset.
	ctrl.smStates.Store(task, api.StRunning)
	ctrl.runReallocDwellTick(now)                                               // arms healthySince=now
	ctrl.runReallocDwellTick(now.Add(reallocationStabilizeDwell - time.Second)) // still below the dwell
	if got := reallocCount(ctrl, task); got != 2 {
		t.Fatalf("reallocation count = %d after sub-dwell ticks, want 2 (transient StRunning must NOT reset — forever-flap guard)", got)
	}

	// A daemon that leaves StRunning (as a bind-refused one does when it exits)
	// resets the dwell clock, so the accrued dwell is discarded.
	ctrl.smStates.Store(task, api.StBackoffWaiting)
	ctrl.runReallocDwellTick(now.Add(reallocationStabilizeDwell + time.Second))
	if got := reallocCount(ctrl, task); got != 2 {
		t.Fatalf("reallocation count = %d after leaving StRunning, want 2 (dwell must restart from scratch)", got)
	}

	// Only a CONTINUOUS StRunning dwell past the threshold resets both windows.
	ctrl.smStates.Store(task, api.StRunning)
	base := now.Add(time.Hour)
	ctrl.runReallocDwellTick(base)                                               // healthySince=base
	ctrl.runReallocDwellTick(base.Add(reallocationStabilizeDwell + time.Second)) // dwell satisfied → reset
	if got := reallocCount(ctrl, task); got != 0 {
		t.Fatalf("reallocation count = %d after a full stable dwell, want 0 (recovered daemon resets its budget)", got)
	}
}

// TestRealloc_FixedGlobal_NotReallocated: a fixed-port global daemon is NEVER
// moved (its port is baked into gate-OFF client URLs). It emits the
// run-host-remedy L3 event once and falls through to the normal crash path.
func TestRealloc_FixedGlobal_NotReallocated(t *testing.T) {
	d := globalDaemonDescriptor()
	ctrl, eventsPath := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)
	// FIX-9.1 pin: the fixed-global run-host-remedy terminal emit MUST pass
	// foreignHolderPort=0 (FIX-1) — a PANICKING resolver catches any regression that
	// re-passes port>0 and fires the blocking probe on the loop.
	ctrl.reallocForeignHolderFn = func(int) (int, string) {
		panic("reallocForeignHolderFn must not be called on the loop from the run-host-remedy terminal emit (FIX-1)")
	}

	driveBindRefusedExit(ctrl, d.TaskName)
	if got := reallocCount(ctrl, task); got != 0 {
		t.Fatalf("global daemon reallocation count = %d, want 0 (never reallocated)", got)
	}
	if got := crashCount(ctrl, task); got != 1 {
		t.Fatalf("global daemon crash count = %d, want 1 (falls through to normal crash path)", got)
	}
	assertEventInLog(t, eventsPath, "quarantined-run-host-remedy")
}

// TestRealloc_NonBindExit_NoReallocation: an ordinary non-bind crash (exit 1)
// takes the normal crash/backoff/quarantine path — no reallocation, no L3 event.
func TestRealloc_NonBindExit_NoReallocation(t *testing.T) {
	d := serenaProxyDescriptor()
	ctrl, eventsPath := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	ctrl.smStates.Store(task, api.StRunning)
	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: d.TaskName,
		Body:     map[string]any{"exit_code": 1}, // generic crash, NOT exitBindRefused
	})
	if got := reallocCount(ctrl, task); got != 0 {
		t.Fatalf("reallocation count = %d after a non-bind crash, want 0", got)
	}
	if got := crashCount(ctrl, task); got != 1 {
		t.Fatalf("crash count = %d after a non-bind crash, want 1 (normal path)", got)
	}
	assertEventNotInLog(t, eventsPath, "daemon-bind-access-denied")
}

// --- End-to-end (running loop + worker) ---------------------------------------

// reallocRunningController wires the FULL self-heal (reallocCh + worker + a fake
// reallocFn) with the loop + worker running, so a bind-refused exit drives a real
// reallocation round-trip.
func reallocRunningController(t *testing.T, reallocFn func(api.SupervisorDaemon) (int, error), foreignHolderFn func(int) (int, string), daemons ...api.SupervisorDaemon) (*supervisorController, *api.EventLoop, string) {
	return reallocRunningControllerCfg(t, reallocFn, foreignHolderFn, nil, daemons...)
}

// reallocRunningControllerCfg is reallocRunningController with a `configure` hook
// applied AFTER the controller is built but BEFORE the loop + worker goroutines
// start, so a test can wire additional fields (reapIntentReader, a spawn capture)
// without racing the running goroutines.
func reallocRunningControllerCfg(t *testing.T, reallocFn func(api.SupervisorDaemon) (int, error), foreignHolderFn func(int) (int, string), configure func(*supervisorController), daemons ...api.SupervisorDaemon) (*supervisorController, *api.EventLoop, string) {
	t.Helper()
	tmp := apitest.HardenedTempDir(t)
	statePath := filepath.Join(tmp, "supervisor-state.json")
	eventsPath := filepath.Join(tmp, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })
	loop := api.NewEventLoop(64)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctrl := &supervisorController{
		intentCache:            newIntentCache(),
		eventLoop:              loop,
		tracker:                NewDaemonRuntimeTracker(),
		events:                 events,
		graceful:               &gracefulCounter{},
		daemonIntent:           newDaemonIntentCache(),
		spawn:                  func(api.SupervisorDaemon) error { return nil },
		terminate:              func(api.SupervisorDaemon) error { return nil },
		statePath:              statePath,
		ctx:                    ctx,
		failureWindow:          respawnFailureWindow,
		quarantineThreshold:    respawnQuarantineThreshold,
		reallocFn:              reallocFn,
		reallocForeignHolderFn: foreignHolderFn,
		reallocCh:              make(chan reallocReq, reallocChCapacity),
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: daemons})
	ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{})
	if configure != nil {
		configure(ctrl)
	}
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctx)
	go ctrl.runReallocWorker(ctx)
	return ctrl, loop, eventsPath
}

// TestRealloc_EndToEnd_ReallocatesAndEmitsRedactedEvent: a bind-refused exit
// drives the worker's reallocFn exactly once, keeps the crash window at 0, and
// emits a `reallocated` L3 event whose foreign_holder is REDACTED to PID +
// basename only.
func TestRealloc_EndToEnd_ReallocatesAndEmitsRedactedEvent(t *testing.T) {
	d := serenaProxyDescriptor()
	var reallocCalls atomic.Int32
	reallocFn := func(got api.SupervisorDaemon) (int, error) {
		reallocCalls.Add(1)
		return 9160, nil
	}
	foreignHolderFn := func(int) (int, string) { return 18180, "NTKDaemon.exe" }
	ctrl, loop, eventsPath := reallocRunningController(t, reallocFn, foreignHolderFn, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	ctrl.smStates.Store(task, api.StRunning)
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName, Body: map[string]any{"exit_code": exitBindRefused}})

	waitForCount(t, func() int32 { return reallocCalls.Load() }, 1, "reallocFn calls")
	assertEventInLog(t, eventsPath, `"action":"reallocated"`)
	assertEventInLog(t, eventsPath, `"new_port":9160`)
	// Redaction: foreign_holder carries pid + basename only.
	assertEventInLog(t, eventsPath, `"NTKDaemon.exe"`)
	assertEventInLog(t, eventsPath, `18180`)

	if got := crashCount(ctrl, task); got != 0 {
		t.Fatalf("crash count = %d after a successful reallocation, want 0", got)
	}
}

// TestRealloc_PoolExhausted_DistinctQuarantine: reallocFn returning
// ErrPortPoolExhausted quarantines the daemon with the pool-exhausted L3 event,
// and does NOT loop retrying.
func TestRealloc_PoolExhausted_DistinctQuarantine(t *testing.T) {
	d := lspWorkspaceProxyDescriptor()
	reallocFn := func(api.SupervisorDaemon) (int, error) {
		return 0, api.ErrPortPoolExhausted
	}
	ctrl, loop, eventsPath := reallocRunningController(t, reallocFn, nil, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	ctrl.smStates.Store(task, api.StRunning)
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName, Body: map[string]any{"exit_code": exitBindRefused}})

	assertEventInLog(t, eventsPath, "quarantined-pool-exhausted")
	// The daemon lands in quarantine and STAYS there (no retry loop).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := ctrl.getSMStateCanonical(task); st == api.StQuarantined {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st, _ := ctrl.getSMStateCanonical(task); st != api.StQuarantined {
		t.Fatalf("state = %v, want StQuarantined after pool exhaustion", st)
	}
	if got := crashCount(ctrl, task); got != 0 {
		t.Fatalf("crash count = %d, want 0 (pool exhaustion is not a crash march)", got)
	}
}

// TestRealloc_LoopPath_RespawnsOnNewPort exercises the P1-1 reallocated LOOP path
// end-to-end via the WHOLE-snapshot apply (FIX-4b reallocSnapshotIncomingNewer): a
// bind-refused exit drives the worker's reallocation, the worker reads the fresh
// (new-port) intent OFF the loop with a STRICTLY-NEWER UpdatedAt, the loop applies it
// INLINE (no blocking self-Post) and re-arms the respawn, and the daemon respawns
// with `--port <newPort>` resolved from the refreshed cache.
func TestRealloc_LoopPath_RespawnsOnNewPort(t *testing.T) {
	d := lspWorkspaceProxyDescriptor() // oldPort 9401, no RuntimeSpec (spawnable)
	const newPort = 9450

	// The fresh on-disk snapshot the worker's reapIntentReader returns: same task,
	// moved to newPort (Port field + --port argv consistent, so the field↔argv
	// fail-closed guard resolves newPort). UpdatedAt is STRICTLY NEWER than the cache's
	// so reallocSnapshotOrder classifies it IncomingNewer → the whole-snapshot apply.
	newLSP := d
	newLSP.Port = newPort
	newLSP.Args = append([]string(nil), d.Args...)
	for i := 0; i+1 < len(newLSP.Args); i++ {
		if newLSP.Args[i] == "--port" {
			newLSP.Args[i+1] = "9450"
		}
	}
	freshIntent := &api.SupervisorIntentFile{Version: 1, UpdatedAt: "2026-07-13T10:00:01Z", Daemons: []api.SupervisorDaemon{newLSP}}

	reallocFn := func(api.SupervisorDaemon) (int, error) { return newPort, nil }
	var spawnedPort atomic.Int32
	configure := func(c *supervisorController) {
		// Seed the cache with a parseable, OLDER UpdatedAt so the carried snapshot is
		// strictly newer (IncomingNewer). Without a parseable current timestamp the
		// order would be Unorderable → the FIX-3b targeted patch, not this path.
		c.intentCache.Refresh(&api.SupervisorIntentFile{Version: 1, UpdatedAt: "2026-07-13T10:00:00Z", Daemons: []api.SupervisorDaemon{d}})
		c.reapIntentReader = func() (*api.SupervisorIntentFile, error) { return freshIntent, nil }
		c.spawn = func(got api.SupervisorDaemon) error {
			if p, ok := api.EffectiveDaemonPort(got); ok {
				spawnedPort.Store(int32(p))
			}
			return nil
		}
	}

	ctrl, loop, _ := reallocRunningControllerCfg(t, reallocFn, nil, configure, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	ctrl.smStates.Store(task, api.StRunning)
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName, Body: map[string]any{"exit_code": exitBindRefused}})

	// The respawn fires respawnBackoffStep (~1s) after the reallocated outcome; the
	// spawned descriptor MUST carry the refreshed new port, proving the loop applied
	// the fresh intent to the cache before re-driving the respawn.
	waitForCount(t, func() int32 { return spawnedPort.Load() }, newPort, "respawn --port")
	if got := crashCount(ctrl, task); got != 0 {
		t.Fatalf("crash count = %d after a within-cap reallocation, want 0", got)
	}
}

// TestRealloc_ReadMiss_TargetedPatchRespawnsOnNewPort (FIX-3b): a successful realloc
// whose worker intent-read FAILS (no carried whole-intent snapshot) STILL respawns on
// newPort — the loop patches JUST this descriptor's port in the current cache from the
// worker's returned newPort (targeted patch), rather than respawning the stale
// old-port cache descriptor and bouncing on exit-1/exit-3 for up to the ~60s
// IntentWatcher window.
func TestRealloc_ReadMiss_TargetedPatchRespawnsOnNewPort(t *testing.T) {
	d := lspWorkspaceProxyDescriptor() // oldPort 9401, has --port argv
	const newPort = 9455

	reallocFn := func(api.SupervisorDaemon) (int, error) { return newPort, nil }
	var spawnedPort atomic.Int32
	configure := func(c *supervisorController) {
		// Read MISS: reapIntentReader errors, so no whole-intent snapshot is carried —
		// the loop must fall back to the FIX-3b targeted port patch.
		c.reapIntentReader = func() (*api.SupervisorIntentFile, error) {
			return nil, errors.New("intent read failed")
		}
		c.spawn = func(got api.SupervisorDaemon) error {
			if p, ok := api.EffectiveDaemonPort(got); ok {
				spawnedPort.Store(int32(p))
			}
			return nil
		}
	}

	ctrl, loop, _ := reallocRunningControllerCfg(t, reallocFn, nil, configure, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	ctrl.smStates.Store(task, api.StRunning)
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName, Body: map[string]any{"exit_code": exitBindRefused}})

	waitForCount(t, func() int32 { return spawnedPort.Load() }, newPort, "respawn --port after read-miss targeted patch")
	if got := crashCount(ctrl, task); got != 0 {
		t.Fatalf("crash count = %d after a within-cap reallocation, want 0", got)
	}
}

// TestRealloc_EqualTimestamp_CommonPath_RespawnsOnNewPort is the round-4b regression
// test for fable's P1: the PRODUCTION-NORMAL timeline. ReallocateDynamicPoolPort's
// step-4 intent write does NOT stamp UpdatedAt, so after a SUCCESSFUL realloc + SUCCESSFUL
// intent read the worker's carried snapshot has the SAME UpdatedAt as the cache. That
// EQUAL timestamp classifies IncomingStale — and FIX-4b's stale branch used to SKIP both
// the whole-apply AND the targeted patch, so the respawn went out on the OLD stolen port
// (bounded ≤60s by the mtime IntentWatcher, but burning the full cap on the common path).
// The fix: the stale branch STILL targeted-patches this descriptor to newPort. Here the
// cache holds the old-port descriptor at UpdatedAt=T and the worker reads back a new-port
// snapshot with the IDENTICAL UpdatedAt=T; the respawn must resolve --port == newPort.
func TestRealloc_EqualTimestamp_CommonPath_RespawnsOnNewPort(t *testing.T) {
	d := lspWorkspaceProxyDescriptor() // oldPort 9401, has --port argv, no RuntimeSpec
	const newPort = 9470
	const ts = "2026-07-13T10:00:00Z" // step-4 leaves UpdatedAt unchanged → EQUAL on both sides

	// The fresh on-disk snapshot the worker's reapIntentReader returns: same task moved to
	// newPort (Port + --port argv consistent), but with the IDENTICAL UpdatedAt as the
	// cache — so reallocSnapshotOrder classifies it EQUAL == IncomingStale (NOT
	// IncomingNewer). This is the shape production always produces.
	newLSP := d
	newLSP.Port = newPort
	newLSP.Args = append([]string(nil), d.Args...)
	for i := 0; i+1 < len(newLSP.Args); i++ {
		if newLSP.Args[i] == "--port" {
			newLSP.Args[i+1] = "9470"
		}
	}
	freshIntent := &api.SupervisorIntentFile{Version: 1, UpdatedAt: ts, Daemons: []api.SupervisorDaemon{newLSP}}

	reallocFn := func(api.SupervisorDaemon) (int, error) { return newPort, nil }
	var spawnedPort atomic.Int32
	var spawned atomic.Pointer[api.SupervisorDaemon]
	configure := func(c *supervisorController) {
		// Cache carries the OLD-port descriptor at the SAME UpdatedAt the worker will read
		// back (the common path). A parseable timestamp on BOTH sides is required so the
		// order is EQUAL (IncomingStale), not Unorderable.
		c.intentCache.Refresh(&api.SupervisorIntentFile{Version: 1, UpdatedAt: ts, Daemons: []api.SupervisorDaemon{d}})
		c.reapIntentReader = func() (*api.SupervisorIntentFile, error) { return freshIntent, nil }
		c.spawn = func(got api.SupervisorDaemon) error {
			g := got
			spawned.Store(&g)
			if p, ok := api.EffectiveDaemonPort(got); ok {
				spawnedPort.Store(int32(p))
			}
			return nil
		}
	}

	ctrl, loop, eventsPath := reallocRunningControllerCfg(t, reallocFn, nil, configure, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	ctrl.smStates.Store(task, api.StRunning)
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName, Body: map[string]any{"exit_code": exitBindRefused}})

	// The equal-timestamp snapshot is treated STALE (no whole-apply), but the targeted
	// patch moves THIS descriptor to newPort, so the respawn resolves argv=newPort. Before
	// the fix (stale branch skipped the patch) the respawn went out on the OLD port 9401.
	waitForCount(t, func() int32 { return spawnedPort.Load() }, newPort, "respawn --port on the equal-timestamp common path")
	// The spawned descriptor's --port argv MUST also be newPort — a moved Port field with a
	// stale argv would still bind the wrong port (Sol P2). Safe to read after the
	// waitForCount observes spawnedPort (the spawn hook stored `spawned` before spawnedPort).
	if sp := spawned.Load(); sp == nil {
		t.Fatalf("no spawned descriptor captured")
	} else if got := argPortOf(*sp); got != newPort {
		t.Fatalf("respawn --port argv = %d, want %d (stale argv on the common path)", got, newPort)
	}
	assertEventInLog(t, eventsPath, "realloc-stale-snapshot-skipped")
	if got := crashCount(ctrl, task); got != 0 {
		t.Fatalf("crash count = %d after a within-cap reallocation, want 0", got)
	}
}

// TestRealloc_PoolExhausted_OperatorStopNotOverwritten is the P2-3 guard: an
// operator stop that lands in the reallocation window (driving the daemon to
// StIdle) must NOT be overwritten by a racing pool-exhausted outcome stamping
// StQuarantined. FIX-7: the quarantined-pool-exhausted TERMINAL event must ALSO NOT
// fire (it is now emitted on the loop AFTER the guard, so a raced stop that skips the
// quarantine never produces a lying terminal event).
func TestRealloc_PoolExhausted_OperatorStopNotOverwritten(t *testing.T) {
	d := lspWorkspaceProxyDescriptor()
	ctrl, eventsPath := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	// Operator stop already drove the daemon to StIdle before the worker's outcome
	// arrives.
	ctrl.smStates.Store(task, api.StIdle)
	ctrl.handleReallocApplied(api.LoopEvent{
		Kind:     evReallocApplied,
		TaskName: d.TaskName,
		Body: map[string]any{
			reallocResultOutcomeBodyKey: reallocOutcomePoolExhausted,
			reallocResultOldPortBodyKey: 9401,
			reallocResultErrBodyKey:     "pool exhausted",
		},
	})
	if st, _ := ctrl.getSMStateCanonical(task); st != api.StIdle {
		t.Fatalf("operator stop overwritten: state = %v, want StIdle (P2-3 StBackoffWaiting guard missing)", st)
	}
	// FIX-7: no false terminal event claiming a quarantine that never happened.
	assertEventNotInLog(t, eventsPath, "quarantined-pool-exhausted")
	assertEventNotInLog(t, eventsPath, "daemon-quarantined")
}

// TestRealloc_FailedOutcome_HeldNotQuarantined: a transient (non-pool-exhausted)
// reallocation error yields a Failed outcome — the daemon stays HELD in backoff
// (re-armed fallback) and is NOT quarantined nor crash-counted.
func TestRealloc_FailedOutcome_HeldNotQuarantined(t *testing.T) {
	d := lspWorkspaceProxyDescriptor()
	var reallocCalls atomic.Int32
	reallocFn := func(api.SupervisorDaemon) (int, error) {
		reallocCalls.Add(1)
		return 0, errors.New("transient registry write error")
	}
	ctrl, loop, _ := reallocRunningController(t, reallocFn, nil, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	ctrl.smStates.Store(task, api.StRunning)
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName, Body: map[string]any{"exit_code": exitBindRefused}})

	waitForCount(t, func() int32 { return reallocCalls.Load() }, 1, "reallocFn calls")
	// Give the Failed outcome time to be applied on the loop.
	time.Sleep(100 * time.Millisecond)
	if st, _ := ctrl.getSMStateCanonical(task); st != api.StBackoffWaiting {
		t.Fatalf("state = %v after a Failed reallocation, want StBackoffWaiting (held, re-armed fallback)", st)
	}
	if got := crashCount(ctrl, task); got != 0 {
		t.Fatalf("crash count = %d after a Failed reallocation, want 0 (a transient realloc error must NOT crash-count)", got)
	}
	// FIX-6: a Failed outcome must NOT consume a cap slot (only a COMPLETED
	// reallocation records one).
	if got := reallocCount(ctrl, task); got != 0 {
		t.Fatalf("reallocation count = %d after a Failed reallocation, want 0 (a failed outcome must NOT consume a cap slot)", got)
	}
}

// TestRealloc_DroppedDispatch_DoesNotConsumeCap (FIX-6): a bind-refused exit whose
// worker dispatch is dropped (the sync controller's unwired reallocCh, mirroring a
// full channel) HOLDS the daemon but records NO cap slot — only a COMPLETED
// reallocation records one. Repeated dropped dispatches therefore never exhaust the
// cap nor march the daemon to a false quarantine.
func TestRealloc_DroppedDispatch_DoesNotConsumeCap(t *testing.T) {
	d := serenaProxyDescriptor()
	ctrl, _ := reallocSyncController(t, d) // reallocCh nil → every dispatch drops
	task := canonicalSupervisorTaskName(d.TaskName)

	for i := 0; i < reallocationCap+2; i++ {
		driveBindRefusedExit(ctrl, d.TaskName)
	}
	if got := reallocCount(ctrl, task); got != 0 {
		t.Fatalf("dropped dispatches consumed cap slots (%d), want 0 (FIX-6: only a completed reallocation records a slot)", got)
	}
	if got := crashCount(ctrl, task); got != 0 {
		t.Fatalf("dropped dispatches crash-counted (%d), want 0 (held in backoff, not a crash)", got)
	}
	if st, _ := ctrl.getSMStateCanonical(task); st != api.StBackoffWaiting {
		t.Fatalf("state = %v after dropped dispatches, want StBackoffWaiting (held, not quarantined)", st)
	}
}

// TestRealloc_DwellReset_OnTransitionOutOfRunning (FIX-5b): a daemon that leaves
// StRunning via an SM transition and RE-ENTERS before the next dwell tick must NOT
// accrue continuous dwell — the transition itself resets the dwell clock, so the
// reallocation window is not cleared early. This exercises the SM-transition reset
// (not the sampling-tick reset the pre-existing DwellGate test already covers).
func TestRealloc_DwellReset_OnTransitionOutOfRunning(t *testing.T) {
	d := serenaProxyDescriptor()
	ctrl, _ := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	// One completed reallocation → realloc window = 1, dwell entry armed.
	driveBindRefusedExit(ctrl, d.TaskName)
	driveReallocCompletion(ctrl, d, 9160)
	if got := reallocCount(ctrl, task); got != 1 {
		t.Fatalf("reallocation count = %d, want 1", got)
	}

	// The daemon reaches StRunning and a tick arms healthySince at t0.
	t0 := time.Now().UTC()
	ctrl.smStates.Store(task, api.StRunning)
	ctrl.runReallocDwellTick(t0)

	// It LEAVES StRunning via an SM transition (a non-bind crash) that NO dwell tick
	// observes, then RE-ENTERS StRunning before the next tick — the flap FIX-5b
	// targets. The transition must reset the dwell clock.
	driveNonBindCrashFromRunning(ctrl, d.TaskName)
	ctrl.smStates.Store(task, api.StRunning) // re-enter before any tick observes the departure

	// A tick a FULL dwell after t0 must NOT satisfy the dwell (healthySince was reset
	// on the transition, so it re-arms to this tick's time), so the reallocation
	// window is NOT cleared early.
	ctrl.runReallocDwellTick(t0.Add(reallocationStabilizeDwell + time.Second))
	if got := reallocCount(ctrl, task); got != 1 {
		t.Fatalf("reallocation window cleared early after a leave+reenter-between-ticks flap (count=%d), want 1 (FIX-5b: the transition out of StRunning must reset the dwell)", got)
	}
}

// TestRealloc_Exit3AtNonRunning_FallsThrough: a bind-refused exit-3 observed at a
// non-StRunning state (StExiting / StBackoffWaiting — a controller-driven exit or
// backoff, NOT a fresh bind attempt) is NOT self-healed; it falls through to the
// normal SM path and records no reallocation nor L3 event.
func TestRealloc_Exit3AtNonRunning_FallsThrough(t *testing.T) {
	for _, st := range []api.SMState{api.StExiting, api.StBackoffWaiting} {
		t.Run(string(st), func(t *testing.T) {
			d := serenaProxyDescriptor()
			ctrl, eventsPath := reallocSyncController(t, d)
			task := canonicalSupervisorTaskName(d.TaskName)

			ctrl.smStates.Store(task, st)
			ctrl.handleLoopEvent(api.LoopEvent{
				Kind:     api.EvChildExit,
				TaskName: d.TaskName,
				Body:     map[string]any{"exit_code": exitBindRefused},
			})
			if got := reallocCount(ctrl, task); got != 0 {
				t.Fatalf("reallocation count = %d for exit-3 at %v, want 0 (only StRunning self-heals)", got, st)
			}
			assertEventNotInLog(t, eventsPath, "daemon-bind-access-denied")
		})
	}
}

// driveFailedReallocOutcome feeds a Failed reallocation outcome through
// handleReallocApplied with the daemon held in StBackoffWaiting (the reallocation
// backoff), the state a real Failed outcome lands in.
func driveFailedReallocOutcome(ctrl *supervisorController, d api.SupervisorDaemon, errMsg string) {
	task := canonicalSupervisorTaskName(d.TaskName)
	ctrl.smStates.Store(task, api.StBackoffWaiting)
	ctrl.handleReallocApplied(api.LoopEvent{
		Kind:     evReallocApplied,
		TaskName: d.TaskName,
		Body: map[string]any{
			reallocResultOutcomeBodyKey: reallocOutcomeFailed,
			reallocResultOldPortBodyKey: 9401,
			reallocResultAttemptBodyKey: 1,
			reallocResultErrBodyKey:     errMsg,
		},
	})
}

// TestRealloc_FailedOutcome_BoundedEscalationToQuarantine (F2): a PERSISTENT
// reallocation failure escalates to a parole-eligible quarantine after
// reallocFailedEscalationBound consecutive Failed outcomes, instead of looping
// StBackoffWaiting forever. Under the bound each Failed outcome re-arms the transient
// hold (no crash-count, no quarantine); the (bound+1)-th escalates + emits the
// distinct terminal L3 reason naming the allocator error.
//
// NON-VACUITY: with an unbounded Failed branch (remove the incrReallocFailCount >
// bound escalation) the daemon NEVER quarantines and quarantined-realloc-failing is
// never emitted, so both post-escalation assertions fail.
func TestRealloc_FailedOutcome_BoundedEscalationToQuarantine(t *testing.T) {
	d := lspWorkspaceProxyDescriptor()
	ctrl, eventsPath := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)
	const allocErr = "persistent registry write denied"

	// Under the bound: each Failed outcome re-arms the transient hold — NOT quarantine,
	// NOT crash-count.
	for i := 0; i < reallocFailedEscalationBound; i++ {
		driveFailedReallocOutcome(ctrl, d, allocErr)
		if st, _ := ctrl.getSMStateCanonical(task); st != api.StBackoffWaiting {
			t.Fatalf("Failed #%d: state = %v, want StBackoffWaiting (under bound = transient hold)", i+1, st)
		}
		if got := crashCount(ctrl, task); got != 0 {
			t.Fatalf("Failed #%d: crash count = %d, want 0 (a transient realloc failure must NOT crash-count)", i+1, got)
		}
	}
	assertEventNotInLog(t, eventsPath, "quarantined-realloc-failing")

	// The (bound+1)-th consecutive Failed outcome ESCALATES to a parole-eligible
	// quarantine with the distinct terminal L3 reason naming the allocator error.
	driveFailedReallocOutcome(ctrl, d, allocErr)
	if st, _ := ctrl.getSMStateCanonical(task); st != api.StQuarantined {
		t.Fatalf("after %d consecutive Failed outcomes: state = %v, want StQuarantined (bounded escalation)", reallocFailedEscalationBound+1, st)
	}
	// Straight to quarantine (parole-eligible), NOT a crash march.
	if got := crashCount(ctrl, task); got != 0 {
		t.Fatalf("escalation crash count = %d, want 0 (a persistent-realloc-failure quarantine is parole-eligible, not a crash march)", got)
	}
	assertEventInLog(t, eventsPath, "quarantined-realloc-failing")
	assertEventInLog(t, eventsPath, allocErr)
}

// TestRealloc_FailedOutcome_SuccessResetsEscalationCounter (F2): a COMPLETED
// reallocation between failures resets the consecutive-Failed counter, so the
// escalation counts CONSECUTIVE failures (not lifetime ones).
//
// NON-VACUITY: without the reset-on-success (reallocFailCount.Delete in the
// Reallocated branch), the second run of `bound` failures pushes the cumulative
// count past the bound and the FIRST post-reset failure escalates → the loop assert
// fires with StQuarantined.
func TestRealloc_FailedOutcome_SuccessResetsEscalationCounter(t *testing.T) {
	d := lspWorkspaceProxyDescriptor()
	ctrl, eventsPath := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	// Accrue exactly `bound` consecutive failures (bound is not yet EXCEEDED → no
	// escalation).
	for i := 0; i < reallocFailedEscalationBound; i++ {
		driveFailedReallocOutcome(ctrl, d, "transient write error")
	}
	if st, _ := ctrl.getSMStateCanonical(task); st != api.StBackoffWaiting {
		t.Fatalf("state = %v after %d failures, want StBackoffWaiting (bound not yet exceeded)", st, reallocFailedEscalationBound)
	}

	// A COMPLETED reallocation resets the consecutive-Failed counter.
	driveReallocCompletion(ctrl, d, 9460)

	// After the reset, another full run of `bound` failures must NOT escalate (the
	// failures are no longer consecutive across the successful reallocation).
	for i := 0; i < reallocFailedEscalationBound; i++ {
		driveFailedReallocOutcome(ctrl, d, "transient write error")
		if st, _ := ctrl.getSMStateCanonical(task); st != api.StBackoffWaiting {
			t.Fatalf("post-reset Failed #%d: state = %v, want StBackoffWaiting (a successful reallocation reset the escalation counter)", i+1, st)
		}
	}
	assertEventNotInLog(t, eventsPath, "quarantined-realloc-failing")
}

// TestPreSpawnRealloc_DynamicPoolReallocatesNotParks (F3): a pre-spawn FOREIGN holder
// on a dynamic-pool proxy (routed here by the port-gate worker via evPreSpawnRealloc)
// reallocates OUR daemon to a fresh pool port instead of parking on the stolen port.
//
// NON-VACUITY: if handlePreSpawnRealloc did not dispatch a reallocation (the pre-fix
// behavior — the pre-spawn gate only parked), reallocCh stays empty and the select's
// default fails.
func TestPreSpawnRealloc_DynamicPoolReallocatesNotParks(t *testing.T) {
	d := serenaProxyDescriptor() // dynamic-pool proxy
	ctrl, _ := reallocSyncController(t, d)
	ctrl.reallocCh = make(chan reallocReq, reallocChCapacity) // wire so the dispatch lands
	task := canonicalSupervisorTaskName(d.TaskName)
	// The pre-spawn port gate has already parked the daemon in StBackoffWaiting because
	// a foreign process holds its port.
	ctrl.smStates.Store(task, api.StBackoffWaiting)

	ctrl.handlePreSpawnRealloc(api.LoopEvent{Kind: evPreSpawnRealloc, TaskName: d.TaskName})

	select {
	case req := <-ctrl.reallocCh:
		if canonicalSupervisorTaskName(req.d.TaskName) != task {
			t.Fatalf("dispatched reallocation for %q, want %q", req.d.TaskName, task)
		}
	default:
		t.Fatal("no reallocation dispatched — a pre-spawn FOREIGN holder on a dynamic-pool proxy must reallocate, not park on the old port")
	}
	if st, _ := ctrl.getSMStateCanonical(task); st != api.StBackoffWaiting {
		t.Fatalf("state = %v, want StBackoffWaiting (held pending the reallocation)", st)
	}
	if got := crashCount(ctrl, task); got != 0 {
		t.Fatalf("crash count = %d, want 0 (a pre-spawn reallocation must not crash-count)", got)
	}
}

// TestPreSpawnRealloc_FixedGlobalNeverReallocated (F3): a fixed-global daemon is NEVER
// reallocated even if an evPreSpawnRealloc reaches its handler (its port is baked into
// gate-OFF client URLs). handlePreSpawnRealloc's defense-in-depth dynamic-pool
// re-check parks it.
//
// NON-VACUITY: remove the isDynamicPoolProxyDescriptor guard in handlePreSpawnRealloc
// and a fixed-global gets reallocated → reallocCh len 1.
func TestPreSpawnRealloc_FixedGlobalNeverReallocated(t *testing.T) {
	d := globalDaemonDescriptor() // fixed-global — port baked into client URLs
	ctrl, _ := reallocSyncController(t, d)
	ctrl.reallocCh = make(chan reallocReq, reallocChCapacity)
	task := canonicalSupervisorTaskName(d.TaskName)
	ctrl.smStates.Store(task, api.StBackoffWaiting)

	ctrl.handlePreSpawnRealloc(api.LoopEvent{Kind: evPreSpawnRealloc, TaskName: d.TaskName})

	if got := len(ctrl.reallocCh); got != 0 {
		t.Fatalf("dispatched %d reallocations for a FIXED-global daemon, want 0 (its port is baked into gate-OFF client URLs — never reallocate)", got)
	}
}

// TestPreSpawnRealloc_OperatorStopNotResurrected (F3): an operator stop that drove the
// daemon out of StBackoffWaiting between the worker's classify and this event must not
// be resurrected into a reallocation.
func TestPreSpawnRealloc_OperatorStopNotResurrected(t *testing.T) {
	d := serenaProxyDescriptor()
	ctrl, _ := reallocSyncController(t, d)
	ctrl.reallocCh = make(chan reallocReq, reallocChCapacity)
	task := canonicalSupervisorTaskName(d.TaskName)
	// An operator stop already drove the daemon to StIdle.
	ctrl.smStates.Store(task, api.StIdle)

	ctrl.handlePreSpawnRealloc(api.LoopEvent{Kind: evPreSpawnRealloc, TaskName: d.TaskName})

	if got := len(ctrl.reallocCh); got != 0 {
		t.Fatalf("dispatched %d reallocations for a stopped daemon, want 0 (StBackoffWaiting guard must not resurrect a raced stop)", got)
	}
	if st, _ := ctrl.getSMStateCanonical(task); st != api.StIdle {
		t.Fatalf("state = %v, want StIdle (the raced operator stop must be preserved)", st)
	}
}

// assertEventInLog / assertEventNotInLog read the supervisor-events.log and
// assert a substring is present / absent (with a short poll for the async paths).
func assertEventInLog(t *testing.T, path, substr string) {
	t.Helper()
	waitForEventInLog(t, path, substr)
}

func assertEventNotInLog(t *testing.T, path, substr string) {
	t.Helper()
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), substr) {
		t.Fatalf("event %q unexpectedly present in supervisor-events.log:\n%s", substr, raw)
	}
}

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

// --- F6.1: the spawn stamps a KERNEL-sourced StartedAt (not wall-clock) so a
// subsequent identity check passes for the live child. -------------------------

// TestSupervisorSpawn_StampsKernelSourcedStartedAt drives the REAL production
// spawn closure against a live helper child and asserts the recorded StartedAt
// is the kernel creation time (the SAME source the liveness identity check
// observes), so process.VerifyPIDIdentity PASSES for the supervisor's own live
// child. The complement asserts a wall-clock-DRIFTED recorded time (loop lag
// past the 2s tolerance) FAILS — which is exactly the false pid_identity_mismatch
// the kernel-sourcing eliminates (bug ...lost-child-quarantine-class F6.1).
func TestSupervisorSpawn_StampsKernelSourcedStartedAt(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	spawnFn := makeProductionSpawnFn(events, tracker)
	descriptor := api.SupervisorDaemon{
		TaskName: reconcileWiringTestTaskName,
		Server:   "memory",
		Daemon:   "default",
		Command:  os.Args[0],
		Args:     []string{"-test.run=TestProductionTerminateFn_HelperSleep"},
		Env:      map[string]string{"MCPHUB_PRODUCTION_TERMINATE_HELPER": "1"},
	}

	if err := spawnFn(descriptor); err != nil {
		t.Fatalf("spawn fn failed on helper command: %v", err)
	}
	entry, ok := tracker.Get(reconcileWiringTestTaskName)
	if !ok || entry.CurrentPID <= 0 || entry.StartedAt.IsZero() {
		t.Fatalf("tracker entry after spawn = %+v, want running pid>0 started_at", entry)
	}
	pid := entry.CurrentPID
	// Best-effort backstop in case an assertion below fails early. The happy
	// path below terminates the child AND drains the spawn's wait goroutine so
	// the temp-dir cleanup does not race an inherited/held event-log handle.
	t.Cleanup(func() { _ = process.TerminatePID(pid) })
	defer func() {
		_ = process.TerminatePID(pid)
		// Drain: the wait goroutine emits daemon-exited AFTER cmd.Wait returns
		// (i.e. after the child is dead and any inherited handle is released), so
		// polling for it makes the child teardown deterministic before the
		// deferred events.Close() + temp-dir RemoveAll run.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if raw, rerr := os.ReadFile(eventsPath); rerr == nil && strings.Contains(string(raw), `"event":"daemon-exited"`) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	kernelStart, ok := process.ProcessStartTime(pid)
	if !ok {
		t.Skipf("kernel ProcessStartTime unavailable on %s (spawn falls back to wall-clock by design)", runtime.GOOS)
	}
	// F6.1: the recorded StartedAt is the kernel creation time, NOT a wall-clock
	// time.Now() captured after cmd.Start.
	if !entry.StartedAt.Equal(kernelStart) {
		t.Fatalf("recorded StartedAt = %s, want kernel creation time %s (F6.1 wall-clock regression)",
			entry.StartedAt.Format(time.RFC3339Nano), kernelStart.Format(time.RFC3339Nano))
	}

	// The identity proof the liveness sweep builds must PASS for this live child.
	proof := process.PIDIdentityProof{
		PID:            pid,
		ExecutablePath: daemonExpectedIdentityExe(descriptor.Command),
		StartedAt:      entry.StartedAt.UTC().Format(time.RFC3339Nano),
	}
	if err := process.VerifyPIDIdentity(proof); err != nil {
		t.Fatalf("identity check FAILED for the supervisor's own live child (kernel-sourced StartedAt): %v", err)
	}

	// Complement: a 3s-drifted recorded time (the pre-F6.1 loop-lag class) pierces
	// the 2s tolerance and would falsely disown the live child.
	drifted := proof
	drifted.StartedAt = kernelStart.Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	if err := process.VerifyPIDIdentity(drifted); err == nil {
		t.Fatal("expected a 3s-drifted StartedAt to fail identity — proves kernel-sourcing is load-bearing")
	}
}

// --- F6.2 + F6.3: two-strike disown + observability. --------------------------

const lostChildIdentityDetail = "PID 22036 started_at mismatch recorded=2026-01-01T00:00:00Z observed=2026-01-01T00:00:03Z"

func lostChildMismatchProbe(mismatch *atomic.Bool) supervisorLivenessProbe {
	return supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(process.PIDIdentityProof) error {
			if mismatch.Load() {
				return fmt.Errorf("%w: %s", process.ErrProcessIdentityMismatch, lostChildIdentityDetail)
			}
			return nil
		},
		// The tracked PID owns the port, so the identity mismatch (checked BEFORE
		// the port) is the sole not-live reason under test.
		PortOwnerPID: func(port int) (int, bool, error) { return 22036, true, nil },
	}
}

func lostChildLivenessFixture(t *testing.T) (stateDir string, intent *api.SupervisorIntentFile, tracker *DaemonRuntimeTracker, loop *api.EventLoop, events <-chan api.LoopEvent, auditLog *api.SupervisorEventLog, eventsPath string) {
	t.Helper()
	stateDir = apitest.HardenedTempDir(t)
	eventsPath = filepath.Join(stateDir, "supervisor-events.log")
	log, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	taskName := `\mcp-local-hub-memory-default`
	intent = &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{TaskName: taskName, Server: "memory", Daemon: "default", Port: 9123}},
	}
	tracker = NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	loop = api.NewEventLoop(16)
	ch := make(chan api.LoopEvent, 8)
	loop.RegisterHandler(func(e api.LoopEvent) { ch <- e })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go loop.Run(ctx)
	return stateDir, intent, tracker, loop, ch, log, eventsPath
}

// TestSupervisorLivenessSweep_TwoStrike_DefersFirstMismatchThenDisowns proves
// F6.2: a pid_identity_mismatch on a LIVE pid does NOT disown on the FIRST sweep
// (it records a pending strike), and disowns only on a SECOND consecutive sweep
// of the SAME generation+pid. F6.3: the pending + stale events carry the
// identity_detail (recorded=… observed=…).
func TestSupervisorLivenessSweep_TwoStrike_DefersFirstMismatchThenDisowns(t *testing.T) {
	stateDir, intent, tracker, loop, events, auditLog, eventsPath := lostChildLivenessFixture(t)
	var mismatch atomic.Bool
	mismatch.Store(true)
	restore := setSupervisorLivenessProbeForTest(lostChildMismatchProbe(&mismatch))
	defer restore()

	mismatchLatch := map[string]identityMismatchStrike{}

	// FIRST sweep: two-strike defers → NO disown.
	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, auditLog, map[string]int{}, nil, mismatchLatch)
	select {
	case ev := <-events:
		t.Fatalf("first-sweep mismatch disowned immediately (two-strike broken): %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
	log := readFileString(t, eventsPath)
	if !strings.Contains(log, `"event":"daemon-identity-mismatch-pending"`) {
		t.Fatalf("expected daemon-identity-mismatch-pending on the first strike:\n%s", log)
	}
	if !strings.Contains(log, "recorded=2026-01-01T00:00:00Z observed=2026-01-01T00:00:03Z") {
		t.Fatalf("pending event did not carry identity_detail (F6.3):\n%s", log)
	}

	// SECOND consecutive sweep (same gen+pid): disown fires.
	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, auditLog, map[string]int{}, nil, mismatchLatch)
	select {
	case ev := <-events:
		if ev.Kind != api.EvChildExit {
			t.Fatalf("second-strike event = %+v, want EvChildExit", ev)
		}
		if ev.Body["reason"] != "pid_identity_mismatch" {
			t.Fatalf("second-strike reason = %v, want pid_identity_mismatch", ev.Body["reason"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second consecutive mismatch did not disown")
	}
	log = readFileString(t, eventsPath)
	if !strings.Contains(log, `"event":"daemon-running-state-stale"`) || !strings.Contains(log, `"identity_detail"`) {
		t.Fatalf("stale disown event missing identity_detail (F6.3):\n%s", log)
	}
}

// TestSupervisorLivenessSweep_TwoStrike_ClearsOnRecovery proves the streak
// resets: a first-sweep mismatch followed by a live second sweep never disowns
// and clears the latch, so the child is not manufactured into a lost orphan.
func TestSupervisorLivenessSweep_TwoStrike_ClearsOnRecovery(t *testing.T) {
	stateDir, intent, tracker, loop, events, auditLog, _ := lostChildLivenessFixture(t)
	var mismatch atomic.Bool
	mismatch.Store(true)
	restore := setSupervisorLivenessProbeForTest(lostChildMismatchProbe(&mismatch))
	defer restore()

	mismatchLatch := map[string]identityMismatchStrike{}

	// First sweep: mismatch → pending, no disown.
	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, auditLog, map[string]int{}, nil, mismatchLatch)
	if len(mismatchLatch) != 1 {
		t.Fatalf("first-strike latch len = %d, want 1", len(mismatchLatch))
	}

	// The (false) mismatch resolves — the next sweep sees a live child.
	mismatch.Store(false)
	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, auditLog, map[string]int{}, nil, mismatchLatch)
	select {
	case ev := <-events:
		t.Fatalf("a resolved mismatch must NOT disown the recovered child: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
	if len(mismatchLatch) != 0 {
		t.Fatalf("mismatch latch not pruned after recovery (streak must reset): %+v", mismatchLatch)
	}
}

// TestSupervisorLivenessSweep_NilMismatchLatchDisownsOnFirstSweep pins the
// nil-latch contract: direct-call tests (and any caller that opts out of the
// two-strike gate) keep the pre-F6.2 first-sweep disown.
func TestSupervisorLivenessSweep_NilMismatchLatchDisownsOnFirstSweep(t *testing.T) {
	stateDir, intent, tracker, loop, events, auditLog, _ := lostChildLivenessFixture(t)
	var mismatch atomic.Bool
	mismatch.Store(true)
	restore := setSupervisorLivenessProbeForTest(lostChildMismatchProbe(&mismatch))
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, auditLog, map[string]int{}, nil, nil)
	select {
	case ev := <-events:
		if ev.Kind != api.EvChildExit || ev.Body["reason"] != "pid_identity_mismatch" {
			t.Fatalf("nil-latch event = %+v, want EvChildExit pid_identity_mismatch", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nil mismatchLatch must disown on the first sweep (pre-F6.2 behavior)")
	}
}

// --- F2: quarantine parole. ---------------------------------------------------

func lostChildParoleController(t *testing.T, threshold int) (*supervisorController, *api.EventLoop, string) {
	t.Helper()
	tmpHome := apitest.HardenedTempDir(t)
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	events, err := api.OpenSupervisorEventLog(filepath.Join(tmpHome, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })

	loop := api.NewEventLoop(16)
	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		eventLoop:           loop,
		tracker:             NewDaemonRuntimeTracker(),
		events:              events,
		graceful:            &gracefulCounter{},
		daemonIntent:        newDaemonIntentCache(),
		spawn:               func(api.SupervisorDaemon) error { return nil },
		terminate:           func(api.SupervisorDaemon) error { return nil },
		statePath:           statePath,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: threshold,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctrl.ctx = ctx
	return ctrl, loop, statePath
}

func lostChildWaitForSMState(t *testing.T, ctrl *supervisorController, task string, want api.SMState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := ctrl.GetSMState(task); ok && st == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := ctrl.GetSMState(task)
	t.Fatalf("SM state for %s = %s, want %s", task, st, want)
}

// TestSupervisorController_QuarantineParole_FiresAfterCooldownAndClears drives a
// daemon through the REAL threshold-quarantine path, then proves: (1) the
// threshold quarantine records parole eligibility, (2) no parole before the
// cooldown, (3) parole after the cooldown re-runs the daemon, (4) the ladder is
// reset once it stabilizes, and (5) the parole state is IN-MEMORY only (never
// written to supervisor-state.json).
func TestSupervisorController_QuarantineParole_FiresAfterCooldownAndClears(t *testing.T) {
	ctrl, loop, statePath := lostChildParoleController(t, 1)
	task := `\mcp-local-hub-test-default`
	descriptor := api.SupervisorDaemon{TaskName: task, Server: "test", Daemon: "default"}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{descriptor}})
	ctrl.smStates.Store(task, api.StRunning)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctrl.ctx)

	// Drive the REAL threshold path: one non-clean child exit at StRunning →
	// StBackoffWaiting → handleBackoffWaiting → failures(1) >= threshold(1) →
	// StQuarantined (+ recordQuarantineParoleEligible).
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: task})
	lostChildWaitForSMState(t, ctrl, task, api.StQuarantined)
	// smStates.Store(StQuarantined) precedes recordQuarantineParoleEligible in the
	// SAME handler, so the poll above can win the race to StQuarantined before the
	// record lands. Drain the loop so the record is observable before the Load.
	paroleBarrierSync(t, loop)

	if _, ok := ctrl.quarantineParole.Load(canonicalSupervisorTaskName(task)); !ok {
		t.Fatal("threshold quarantine did not record parole eligibility")
	}

	// In-memory only: parole state is never persisted.
	if raw, err := os.ReadFile(statePath); err == nil && strings.Contains(strings.ToLower(string(raw)), "parole") {
		t.Fatalf("parole state leaked into supervisor-state.json (must be in-memory only):\n%s", raw)
	}

	// Before the cooldown: the tick posts nothing; the daemon stays quarantined.
	ctrl.runQuarantineParoleTick(time.Now().UTC())
	if st, _ := ctrl.GetSMState(task); st != api.StQuarantined {
		t.Fatalf("daemon paroled before the cooldown elapsed: state = %s", st)
	}
	if v, ok := ctrl.quarantineParole.Load(canonicalSupervisorTaskName(task)); ok {
		if v.(*quarantineParoleEntry).attempts != 0 {
			t.Fatalf("parole attempts advanced before the cooldown: %d", v.(*quarantineParoleEntry).attempts)
		}
	}

	// After the cooldown: parole posts EvManualRestart → StSpawning → StRunning.
	ctrl.runQuarantineParoleTick(time.Now().UTC().Add(quarantineParoleBaseDelay + time.Minute))
	lostChildWaitForSMState(t, ctrl, task, api.StRunning)

	// Stabilize: after dwelling in StRunning, the parole ladder resets.
	base := time.Now().UTC().Add(time.Hour)
	ctrl.runQuarantineParoleTick(base)
	ctrl.runQuarantineParoleTick(base.Add(quarantineParoleStabilizeDwell + time.Minute))
	if _, ok := ctrl.quarantineParole.Load(canonicalSupervisorTaskName(task)); ok {
		t.Fatal("parole ladder not reset after the daemon stabilized in Running")
	}
}

// TestSupervisorController_QuarantineParole_ExponentialAndBounded proves the
// cooldown grows exponentially across successive paroles of a daemon that keeps
// re-failing (modeled by pinning it in StQuarantined) and that the ladder is
// preserved across re-quarantine rather than tight-looping at the base delay.
func TestSupervisorController_QuarantineParole_ExponentialAndBounded(t *testing.T) {
	ctrl, loop, _ := lostChildParoleController(t, 1)
	task := `\mcp-local-hub-test-default`
	key := canonicalSupervisorTaskName(task)
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: task, Server: "test", Daemon: "default"}}})

	// A recording handler keeps the daemon pinned in StQuarantined (each parole
	// re-fails and immediately re-quarantines in this model), so the ladder must
	// advance exponentially instead of resetting.
	restarts := make(chan api.LoopEvent, 8)
	loop.RegisterHandler(func(e api.LoopEvent) {
		if e.Kind == api.EvManualRestart {
			restarts <- e
		}
	})
	go loop.Run(ctrl.ctx)

	ctrl.smStates.Store(key, api.StQuarantined)
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	ctrl.recordQuarantineParoleEligible(task, t0)

	assertNoParole := func(when time.Time, msg string) {
		t.Helper()
		ctrl.runQuarantineParoleTick(when)
		select {
		case ev := <-restarts:
			t.Fatalf("%s: unexpected parole EvManualRestart at %s: %+v", msg, when.Format(time.RFC3339), ev)
		case <-time.After(150 * time.Millisecond):
		}
	}
	assertParole := func(when time.Time, wantAttempts int, msg string) {
		t.Helper()
		ctrl.runQuarantineParoleTick(when)
		select {
		case <-restarts:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: expected a parole EvManualRestart at %s", msg, when.Format(time.RFC3339))
		}
		v, ok := ctrl.quarantineParole.Load(key)
		if !ok {
			t.Fatalf("%s: parole entry vanished", msg)
		}
		if got := v.(*quarantineParoleEntry).attempts; got != wantAttempts {
			t.Fatalf("%s: attempts = %d, want %d", msg, got, wantAttempts)
		}
	}

	// Base cooldown (15m) not yet elapsed → no parole.
	assertNoParole(t0.Add(quarantineParoleBaseDelay-time.Minute), "before base cooldown")
	// First parole at base cooldown.
	firstParole := t0.Add(quarantineParoleBaseDelay + time.Second)
	assertParole(firstParole, 1, "first parole")
	// The second cooldown is 2× the base (exponential): 20m in is NOT enough.
	assertNoParole(firstParole.Add(quarantineParoleDelay(0)+time.Minute), "before second (exponential) cooldown")
	// After the 30m second-step cooldown, the second parole fires.
	assertParole(firstParole.Add(quarantineParoleDelay(1)+time.Second), 2, "second parole")

	// Bounded: the cooldown never exceeds the ceiling.
	if quarantineParoleDelay(100) != quarantineParoleMaxDelay {
		t.Fatalf("parole delay not capped at the ceiling: %s", quarantineParoleDelay(100))
	}
	if quarantineParoleDelay(1) <= quarantineParoleDelay(0) {
		t.Fatalf("parole delay must grow: delay(1)=%s <= delay(0)=%s", quarantineParoleDelay(1), quarantineParoleDelay(0))
	}
}

// TestSupervisorController_QuarantineParole_RespectsStoppedIntent proves an
// operator-stopped quarantined daemon is never auto-respawned by parole.
func TestSupervisorController_QuarantineParole_RespectsStoppedIntent(t *testing.T) {
	ctrl, loop, _ := lostChildParoleController(t, 1)
	task := `\mcp-local-hub-test-default`
	key := canonicalSupervisorTaskName(task)
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: task, Server: "test", Daemon: "default"}}})
	ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			task: {Desired: api.IntentDesiredStopped, Reason: "user-stop", UpdatedAt: time.Now().UTC()},
		},
	})

	restarts := make(chan api.LoopEvent, 4)
	loop.RegisterHandler(func(e api.LoopEvent) {
		if e.Kind == api.EvManualRestart {
			restarts <- e
		}
	})
	go loop.Run(ctrl.ctx)

	ctrl.smStates.Store(key, api.StQuarantined)
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	ctrl.recordQuarantineParoleEligible(task, t0)

	// Well past the cooldown — parole must still refuse for a stopped daemon.
	ctrl.runQuarantineParoleTick(t0.Add(quarantineParoleMaxDelay + time.Hour))
	select {
	case ev := <-restarts:
		t.Fatalf("parole respawned an operator-stopped daemon: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// paroleBarrierSync posts an evReapBarrier and blocks until the loop has drained
// every prior event, giving the test goroutine a happens-before edge so it may
// read controller/parole state race-free (mirrors the reap harness's sync()).
func paroleBarrierSync(t *testing.T, loop *api.EventLoop) {
	t.Helper()
	done := make(chan struct{})
	loop.Post(api.LoopEvent{Kind: evReapBarrier, Body: map[string]any{reapBarrierResultBodyKey: done}})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parole barrier sync timed out (loop wedged?)")
	}
}

// TestSupervisorController_ParoleTick_OnLoopDispatchFires proves the Fix A wiring:
// posting evParoleTick onto the event loop invokes runQuarantineParoleTick ON the
// loop goroutine (via handleLoopEvent's top-switch interception), which paroles an
// eligible, cooled-down quarantined daemon. This is the on-loop replacement for the
// off-loop monitor tick — the monitor now only TryPosts evParoleTick.
func TestSupervisorController_ParoleTick_OnLoopDispatchFires(t *testing.T) {
	ctrl, loop, _ := lostChildParoleController(t, 1)
	task := `\mcp-local-hub-test-default`
	key := canonicalSupervisorTaskName(task)
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: task, Server: "test", Daemon: "default"}}})
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctrl.ctx)

	// A quarantined daemon whose cooldown has already elapsed (nextAttemptAt in the
	// past), running intent (default). Seeding the map from the test goroutine BEFORE
	// the post gives a happens-before edge into the loop's read.
	ctrl.smStates.Store(key, api.StQuarantined)
	ctrl.quarantineParole.Store(key, &quarantineParoleEntry{attempts: 0, nextAttemptAt: time.Now().UTC().Add(-time.Hour)})

	// Dispatch the parole scan through the loop (NOT a direct runQuarantineParoleTick
	// call): handleLoopEvent must intercept evParoleTick and run the tick on-loop.
	loop.Post(api.LoopEvent{Kind: evParoleTick})

	// The tick posts EvManualRestart → StQuarantined+EvManualRestart → StSpawning →
	// StRunning. Reaching StRunning proves evParoleTick drove the tick and the parole
	// fired.
	lostChildWaitForSMState(t, ctrl, task, api.StRunning)
}

// TestSupervisorController_ParoleGraduationVsThreshold_NoWedge is the Fix A
// regression: it drives the exact interleaving that USED to wedge when the tick ran
// off the loop — a graduation Delete racing an on-loop threshold record. A paroled
// daemon dwelling in StRunning (graduation-eligible) re-fails; a graduation
// evParoleTick and the threshold EvChildExit are both dispatched onto the FIFO loop.
// Pre-fix the off-loop tick's Delete could land AFTER the on-loop
// recordQuarantineParoleEligible's LoadOrStore no-op, erasing the just-recorded
// ladder → StQuarantined with NO parole entry → permanent pre-F2 wedge. On-loop the
// two are serialized, so the entry always survives and the daemon can still parole.
func TestSupervisorController_ParoleGraduationVsThreshold_NoWedge(t *testing.T) {
	ctrl, loop, _ := lostChildParoleController(t, 1)
	task := `\mcp-local-hub-test-default`
	key := canonicalSupervisorTaskName(task)
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: task, Server: "test", Daemon: "default"}}})
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctrl.ctx)

	// A daemon on the parole ladder (attempts=1) that has dwelled in StRunning past
	// the stabilize dwell, so a parole tick would GRADUATE it (Delete the ladder).
	ctrl.smStates.Store(key, api.StRunning)
	ctrl.quarantineParole.Store(key, &quarantineParoleEntry{
		attempts:      1,
		nextAttemptAt: time.Now().UTC().Add(time.Hour),
		healthySince:  time.Now().UTC().Add(-time.Hour),
	})

	// Interleave on the FIFO loop: the graduation tick (Delete) THEN the re-fail that
	// re-quarantines + records eligibility. Serialized on-loop, either order leaves a
	// live entry; pre-fix the off-loop Delete could erase the record.
	loop.Post(api.LoopEvent{Kind: evParoleTick})
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: task})
	lostChildWaitForSMState(t, ctrl, task, api.StQuarantined)
	paroleBarrierSync(t, loop)

	// No-wedge invariant: a StQuarantined daemon MUST retain a parole ladder entry.
	entryAny, ok := ctrl.quarantineParole.Load(key)
	if !ok {
		t.Fatal("WEDGE: daemon is StQuarantined with NO parole entry (graduation Delete erased the threshold record)")
	}

	// Recovery proof: the daemon is not wedged — making it eligible and dispatching
	// another evParoleTick paroles it back to running. The barrier above established
	// happens-before, and this write happens-before the loop's read via the post
	// below, so mutating the entry here is race-free.
	entryAny.(*quarantineParoleEntry).nextAttemptAt = time.Now().UTC().Add(-time.Hour)
	loop.Post(api.LoopEvent{Kind: evParoleTick})
	lostChildWaitForSMState(t, ctrl, task, api.StRunning)
}

// TestSupervisorController_ParolePostRoutesThroughQuarantined documents the Fix B
// refutation (finding-1, FALSE). The bot claimed the parole tick's canonical-key
// EvManualRestart post misses a legacy bare-key smStates row and misroutes to
// StIdle+EvManualRestart. That is wrong: smStates is a single canonical key space
// (ev.TaskName is canonicalized once at the SM ingestion boundary), so a canonical
// EvManualRestart — exactly what the tick posts — hits the canonical StQuarantined
// row and takes the StQuarantined+EvManualRestart "reset failures" spawn. This test
// pins that routing: a StQuarantined row stored under the CANONICAL key + a canonical
// EvManualRestart reaches StRunning (StIdle+EvManualRestart on a stopped-less daemon
// would instead refuse to reach StRunning through this path). No behavior changed.
func TestSupervisorController_ParolePostRoutesThroughQuarantined(t *testing.T) {
	ctrl, loop, _ := lostChildParoleController(t, 1)
	task := `\mcp-local-hub-test-default`
	key := canonicalSupervisorTaskName(task)
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: task, Server: "test", Daemon: "default"}}})
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctrl.ctx)

	// The ONLY smStates row is canonical — production stores every StQuarantined row
	// canonical (all 8 smStates.Store sites key canonical post-r8). No bare-key row
	// exists for the canonical post to "miss".
	ctrl.smStates.Store(key, api.StQuarantined)

	// The tick posts EvManualRestart with the canonical TaskName; drive that directly.
	loop.Post(api.LoopEvent{Kind: api.EvManualRestart, TaskName: task})
	lostChildWaitForSMState(t, ctrl, task, api.StRunning)
}

// TestSupervisorController_ParoleQueuedStop_PostSelfHonorsStop is the P1/P2
// regression: the parole restart MUST go through PostSelf (priority selfCh), not
// the main-channel TryPost, now that the tick runs INSIDE the evParoleTick handler.
// It reproduces the queued-stop interleaving a handler-context TryPost mishandles:
// a parole tick reads running intent and posts EvManualRestart, then a stop swaps
// the intent cache and an EvIntentUpdate(stopped) is queued BEHIND the tick.
//
//   - With TryPost the EvManualRestart lands at the main-channel TAIL, so
//     EvIntentUpdate(stopped) is consumed FIRST at StQuarantined (an absorbing
//     no-op that records no stop), then EvManualRestart spawns cleanly → the daemon
//     runs against an already-applied stop that no later delta reaps (StRunning,
//     forever — the IntentWatcher is delta-only).
//   - With PostSelf the EvManualRestart priority-drains FIRST
//     (StQuarantined→StSpawning→StRunning), then the queued EvIntentUpdate(stopped)
//     lands at StRunning and drives StExiting (terminate) → the stop is honored.
//
// The three events are pre-filled into the loop buffer BEFORE Run starts so their
// FIFO order is deterministic regardless of scheduling; an on-loop sentinel flips
// the intent cache running→stopped between the tick and the EvIntentUpdate
// (mirroring the evReapScan stop-swap). Asserting the daemon reaches StExiting
// (own-spawned, so it stays there under the fake fixture) PASSES on PostSelf and
// TIMES OUT on a TryPost revert (daemon stuck StRunning).
func TestSupervisorController_ParoleQueuedStop_PostSelfHonorsStop(t *testing.T) {
	ctrl, loop, _ := lostChildParoleController(t, 1)
	task := `\mcp-local-hub-test-default`
	key := canonicalSupervisorTaskName(task)
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: task, Server: "test", Daemon: "default"}}})

	// Eligible cooled-down quarantined daemon; intent RUNNING at gate time (the empty
	// daemonIntent cache defaults to running).
	ctrl.smStates.Store(key, api.StQuarantined)
	ctrl.quarantineParole.Store(key, &quarantineParoleEntry{attempts: 0, nextAttemptAt: time.Now().UTC().Add(-time.Hour)})

	const flipStopsSentinel = api.SMEvent("test-flip-stops-to-stopped")
	stoppedIntent := &api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		task: {Desired: api.IntentDesiredStopped, Reason: "user-stop", UpdatedAt: time.Now().UTC()},
	}}
	loop.RegisterHandler(func(e api.LoopEvent) {
		if e.Kind == flipStopsSentinel {
			// On-loop stop-cache swap (mirrors the evReapScan stop-swap) between the
			// tick's gate read and the EvIntentUpdate below.
			ctrl.daemonIntent.Refresh(stoppedIntent)
			return
		}
		ctrl.handleLoopEvent(e)
	})

	// Pre-fill the FIFO buffer BEFORE Run so the order is deterministic:
	// [evParoleTick, flip→stopped, EvIntentUpdate(stopped)]. loop.Post does not block
	// (buffer >= 16, nothing consuming yet), so all three land before any dispatch.
	loop.Post(api.LoopEvent{Kind: evParoleTick})
	loop.Post(api.LoopEvent{Kind: flipStopsSentinel})
	loop.Post(api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: task})
	go loop.Run(ctrl.ctx)

	// PostSelf: the restart drains first, the daemon spawns, then the queued stop
	// drives it to StExiting. A TryPost revert leaves it stuck at StRunning → this
	// wait times out and fails, which is the regression guard.
	lostChildWaitForSMState(t, ctrl, task, api.StExiting)
}

// --- Commission fixes: P1 (generation-tagged disown) + P2 (absorbing-leak). ----

// TestSupervisorLivenessSweep_DisownEvChildExitCarriesGeneration proves the
// commission-P1 fix: the liveness assume-dead disown EvChildExit carries the
// sweep-time pid_generation so the controller's P1a guard can drop it when a
// respawn has superseded the child it observed.
func TestSupervisorLivenessSweep_DisownEvChildExitCarriesGeneration(t *testing.T) {
	stateDir, intent, tracker, loop, events, auditLog, _ := lostChildLivenessFixture(t)
	var mismatch atomic.Bool
	mismatch.Store(true)
	restore := setSupervisorLivenessProbeForTest(lostChildMismatchProbe(&mismatch))
	defer restore()

	// nil mismatchLatch → first-sweep disown (no two-strike deferral).
	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, auditLog, map[string]int{}, nil, nil)
	select {
	case ev := <-events:
		if ev.Kind != api.EvChildExit {
			t.Fatalf("event = %+v, want EvChildExit", ev)
		}
		gen, ok := ev.Body["pid_generation"].(int)
		if !ok || gen != 1 {
			t.Fatalf("disown EvChildExit pid_generation = %v (ok=%v), want 1 (the sweep-time generation; commission P1)", ev.Body["pid_generation"], ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no disown posted")
	}
}

// TestSupervisorController_StaleLivenessDisownDroppedByGeneration proves the
// downstream half of commission-P1: a generation-tagged liveness disown that is
// SUPERSEDED (older generation than the tracker's current child) is dropped by
// P1a — it does NOT clear the fresh live child — while a CURRENT-generation
// disown transitions off StRunning as intended.
func TestSupervisorController_StaleLivenessDisownDroppedByGeneration(t *testing.T) {
	ctrl, loop, _ := lostChildParoleController(t, respawnQuarantineThreshold)
	task := `\mcp-local-hub-test-default`
	eventsPath := filepath.Dir(ctrl.statePath)
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: task, Server: "test", Daemon: "default"}}})
	// Tracker at generation 2 (two spawns); smStates says the current child runs.
	ctrl.tracker.MarkSpawned(task, 100, time.Now().UTC())
	ctrl.tracker.MarkSpawned(task, 200, time.Now().UTC())
	ctrl.smStates.Store(task, api.StRunning)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctrl.ctx)

	// Stale disown (generation 1 < current 2): P1a drops it; the child survives.
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: task, Body: map[string]any{"pid": 100, "pid_generation": 1, "reason": "pid_identity_mismatch"}})
	// Wait for the stale-drop to be logged, then assert the SM never moved.
	deadline := time.Now().Add(2 * time.Second)
	logPath := filepath.Join(eventsPath, "supervisor-events.log")
	for time.Now().Before(deadline) {
		if strings.Contains(readFileStringIfExists(t, logPath), `"event":"daemon-stale-exit-ignored"`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st, _ := ctrl.GetSMState(task); st != api.StRunning {
		t.Fatalf("superseded liveness disown was NOT dropped; state = %s, want %s (the fresh child would be forgotten)", st, api.StRunning)
	}

	// Current disown (generation 2): NOT dropped — the SM leaves StRunning.
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: task, Body: map[string]any{"pid": 200, "pid_generation": 2, "reason": "pid_identity_mismatch"}})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := ctrl.GetSMState(task); st != api.StRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("current-generation liveness disown did not transition the daemon off StRunning")
}

// TestSupervisorController_QuarantineParole_AbsorbingLegacySerena_NoParole proves
// commission-P2-1: a legacy-serena-nil-spec (absorbing) quarantine that later
// takes a stopped self-loop must NOT leak into the parole ladder.
func TestSupervisorController_QuarantineParole_AbsorbingLegacySerena_NoParole(t *testing.T) {
	ctrl, _, _ := lostChildParoleController(t, respawnQuarantineThreshold)
	task := `\mcp-local-hub-serena-deadbeef`
	descriptor := api.SupervisorDaemon{
		TaskName:    task,
		Server:      "serena",
		Daemon:      "deadbeef",
		Args:        []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", `C:\work\alpha`, "--port", "9121"},
		RuntimeSpec: nil,
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{descriptor}})
	ctrl.smStates.Store(task, api.StIdle)

	// Absorbing quarantine (no parole eligibility).
	ctrl.handleLoopEvent(api.LoopEvent{Kind: api.EvStart, TaskName: task})
	if st, _ := ctrl.GetSMState(task); st != api.StQuarantined {
		t.Fatalf("legacy-serena nil-spec did not quarantine; state = %s", st)
	}
	if _, ok := ctrl.quarantineParole.Load(canonicalSupervisorTaskName(task)); ok {
		t.Fatal("absorbing legacy-serena quarantine must NOT be parole-eligible on entry")
	}

	// Stopped self-loop: StQuarantined + EvIntentUpdate(stopped) → StQuarantined.
	ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		task: {Desired: api.IntentDesiredStopped, Reason: "user-stop", UpdatedAt: time.Now().UTC()},
	}})
	ctrl.handleLoopEvent(api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: task})
	if _, ok := ctrl.quarantineParole.Load(canonicalSupervisorTaskName(task)); ok {
		t.Fatal("a stopped self-loop leaked an absorbing quarantine into the parole ladder (commission P2-1)")
	}
}

// TestSupervisorController_QuarantineParole_AbsorbingStrictJob_NoParole proves
// commission-P2-1 for the strict-job-protection absorbing quarantine: a
// stopped/graceful self-loop must NOT create a parole entry.
func TestSupervisorController_QuarantineParole_AbsorbingStrictJob_NoParole(t *testing.T) {
	ctrl, _, _ := lostChildParoleController(t, respawnQuarantineThreshold)
	ctrl.spawn = func(api.SupervisorDaemon) error { return errSpawnJobProtectionRefused }
	task := `\mcp-local-hub-strictjob-default`
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: task, Server: "strictjob", Daemon: "default"}}})
	ctrl.smStates.Store(task, api.StIdle)

	// Fail-closed absorbing quarantine (no parole eligibility).
	ctrl.handleLoopEvent(api.LoopEvent{Kind: api.EvStart, TaskName: task})
	if st, _ := ctrl.GetSMState(task); st != api.StQuarantined {
		t.Fatalf("strict-job refusal did not quarantine; state = %s", st)
	}
	if _, ok := ctrl.quarantineParole.Load(canonicalSupervisorTaskName(task)); ok {
		t.Fatal("absorbing strict-job quarantine must NOT be parole-eligible on entry")
	}

	// Graceful-request self-loop: StQuarantined + EvRequestGraceful → StQuarantined.
	ctrl.handleLoopEvent(api.LoopEvent{Kind: api.EvRequestGraceful, TaskName: task})
	if _, ok := ctrl.quarantineParole.Load(canonicalSupervisorTaskName(task)); ok {
		t.Fatal("a graceful self-loop leaked an absorbing strict-job quarantine into the parole ladder (commission P2-1)")
	}
}

// TestSupervisorController_QuarantineParole_ClassFlipToJobRefusedClearsLadder
// proves commission-P2-2: a THRESHOLD-quarantined daemon that is paroled and
// whose respawn then hits an absorbing job-protection refusal has its ladder
// CLEARED (class-flip), so it is not paroled into a permanent spawn-refuse loop.
func TestSupervisorController_QuarantineParole_ClassFlipToJobRefusedClearsLadder(t *testing.T) {
	ctrl, loop, _ := lostChildParoleController(t, 1)
	task := `\mcp-local-hub-test-default`
	key := canonicalSupervisorTaskName(task)
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: task, Server: "test", Daemon: "default"}}})

	var refuseJob atomic.Bool // starts false: the first spawn succeeds → threshold quarantine records eligibility
	ctrl.spawn = func(api.SupervisorDaemon) error {
		if refuseJob.Load() {
			return errSpawnJobProtectionRefused
		}
		return nil
	}
	ctrl.smStates.Store(task, api.StRunning)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctrl.ctx)

	// Threshold quarantine via the real path (threshold=1) → eligibility recorded.
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: task})
	lostChildWaitForSMState(t, ctrl, task, api.StQuarantined)
	// Drain the loop so recordQuarantineParoleEligible (which follows the
	// StQuarantined store in the same handler) is observable before the Load.
	paroleBarrierSync(t, loop)
	if _, ok := ctrl.quarantineParole.Load(key); !ok {
		t.Fatal("threshold quarantine did not record parole eligibility")
	}

	// The daemon's spawn now hits the absorbing job-protection refusal.
	refuseJob.Store(true)
	// Parole after the cooldown → EvManualRestart → StSpawning → job-refused →
	// absorbing quarantine → clearQuarantineParole.
	ctrl.runQuarantineParoleTick(time.Now().UTC().Add(quarantineParoleBaseDelay + time.Minute))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := ctrl.quarantineParole.Load(key); !ok {
			return // ladder cleared by the class-flip
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("parole ladder not cleared after the daemon's class flipped to absorbing job-refused (commission P2-2)")
}

// TestSupervisorController_QuarantineParole_BareTaskNameKeyParoled proves
// commission-P2-3: a legacy row whose smStates key is BARE (no leading
// backslash) is still paroled — the tick resolves its state via
// getSMStateCanonical (both key forms), not a strict canonical-only lookup that
// would silently un-parole it.
func TestSupervisorController_QuarantineParole_BareTaskNameKeyParoled(t *testing.T) {
	ctrl, loop, _ := lostChildParoleController(t, 1)
	bare := "mcp-local-hub-legacy-default" // NO leading backslash (legacy hand-written row)
	canonical := canonicalSupervisorTaskName(bare)
	if canonical == bare {
		t.Fatalf("canonicalSupervisorTaskName did not add a leading backslash: %q", canonical)
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: bare, Server: "legacy", Daemon: "default"}}})
	// smStates stored under the BARE key to simulate a LEGACY pre-r8 remnant.
	// (Post-r8 handleBackoffWaiting receives the CANONICALIZED descriptor copy and
	// stores canonical — the SM ingestion boundary prevents new bare-key rows — so
	// this bare row is only a legacy leftover the getSMStateCanonical both-form probe
	// still resolves.) The parole entry is keyed CANONICAL (recordQuarantineParoleEligible).
	ctrl.smStates.Store(bare, api.StQuarantined)
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	ctrl.recordQuarantineParoleEligible(bare, t0)

	restarts := make(chan api.LoopEvent, 4)
	loop.RegisterHandler(func(e api.LoopEvent) {
		if e.Kind == api.EvManualRestart {
			restarts <- e
		}
	})
	go loop.Run(ctrl.ctx)

	ctrl.runQuarantineParoleTick(t0.Add(quarantineParoleBaseDelay + time.Minute))
	select {
	case <-restarts:
	case <-time.After(2 * time.Second):
		t.Fatal("bare-TaskName quarantined row was not paroled (getSMStateCanonical regression, commission P2-3)")
	}
	if _, ok := ctrl.quarantineParole.Load(canonical); !ok {
		t.Fatal("parole entry was deleted for a bare-keyed row (tick used strict GetSMState instead of getSMStateCanonical)")
	}
}

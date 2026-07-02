package cli

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// newStaleExitController builds an in-memory controller (no real process, no
// loop) for the direct-handleLoopEvent P1a controller-guard tests. spawn is a
// recording fake; the tracker + smStates are in-memory; statePath points into a
// hardened temp dir so the persist seam is real but isolated.
func newStaleExitController(t *testing.T, taskName string) (*supervisorController, *DaemonRuntimeTracker, *atomic.Int32) {
	t.Helper()
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	descriptor := api.SupervisorDaemon{TaskName: taskName, Server: "serena", Daemon: "default"}
	ctrl := &supervisorController{
		intentCache:  newIntentCache(),
		tracker:      tracker,
		events:       events,
		daemonIntent: newDaemonIntentCache(),
		spawn: func(d api.SupervisorDaemon) error {
			spawnCalls.Add(1)
			return nil
		},
		statePath:           statePath,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{descriptor}})
	return ctrl, tracker, &spawnCalls
}

// TestHandleLoopEvent_StaleGenerationExitDropped is spec test 5: a controller
// seeded at gen3/StRunning receives an EvChildExit stamped with gen2 (an older
// child's late exit). The guard must drop it: smStates stays StRunning, no
// failure is recorded (no backoff), and reaperOutstanding is NOT cleared.
func TestHandleLoopEvent_StaleGenerationExitDropped(t *testing.T) {
	taskName := `\mcp-local-hub-serena-default`
	ctrl, tracker, _ := newStaleExitController(t, taskName)

	// Seed generation 3 (three spawns), StRunning, and a live reaper marker.
	tracker.MarkSpawned(taskName, 100, time.Now().UTC())
	tracker.MarkSpawned(taskName, 200, time.Now().UTC())
	tracker.MarkSpawned(taskName, 300, time.Now().UTC())
	if e, _ := tracker.Get(taskName); e.PIDGeneration != 3 {
		t.Fatalf("seed generation = %d, want 3", e.PIDGeneration)
	}
	ctrl.smStates.Store(taskName, api.StRunning)
	ctrl.reaperOutstanding.Store(taskName, true)

	crashesBefore := tracker.CrashCountInWindow(taskName, time.Now().UTC(), respawnFailureWindow)

	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: taskName,
		Body:     map[string]any{"pid": 200, "pid_generation": 2, "exit_code": 1},
	})

	// SM state unchanged — the stale exit did not drive StRunning→StBackoffWaiting.
	if st, _ := ctrl.GetSMState(taskName); st != api.StRunning {
		t.Fatalf("SM state = %s after stale exit, want StRunning (dropped)", st)
	}
	// No crash recorded (no backoff-window inflation).
	if got := tracker.CrashCountInWindow(taskName, time.Now().UTC(), respawnFailureWindow); got != crashesBefore {
		t.Fatalf("crash count = %d after stale exit, want unchanged %d (no phantom failure)", got, crashesBefore)
	}
	// reaperOutstanding for the CURRENT child must NOT have been cleared.
	if _, ok := ctrl.reaperOutstanding.Load(taskName); !ok {
		t.Fatalf("reaperOutstanding cleared by a stale exit; the current child's reaper marker must survive")
	}
	// Current tracking untouched (still gen3, pid 300, running).
	if e, _ := tracker.Get(taskName); e.PIDGeneration != 3 || e.CurrentPID != 300 || e.State != daemonRuntimeStateRunning {
		t.Fatalf("tracker mutated by stale exit: %+v, want running pid=300 generation=3", e)
	}
}

// TestHandleLoopEvent_StaleCleanExitDoesNotIdleRunningDaemon is spec test 7: a
// stale CLEAN exit (clean_exit:true, older generation) at StRunning must be
// dropped by the stale guard BEFORE the clean-exit-at-StRunning handler runs.
// Without the guard-ordering it would store StIdle while the current child is
// alive (the second latent instance of this bug class).
func TestHandleLoopEvent_StaleCleanExitDoesNotIdleRunningDaemon(t *testing.T) {
	taskName := `\mcp-local-hub-serena-default`
	ctrl, tracker, _ := newStaleExitController(t, taskName)

	tracker.MarkSpawned(taskName, 100, time.Now().UTC())
	tracker.MarkSpawned(taskName, 200, time.Now().UTC()) // gen2 current
	ctrl.smStates.Store(taskName, api.StRunning)

	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: taskName,
		Body: map[string]any{
			"pid":                      100,
			"pid_generation":           1, // stale
			"exit_code":                0,
			supervisorCleanExitBodyKey: true,
		},
	})

	// The stale guard must fire before the clean-exit-at-StRunning handler, so
	// the SM stays StRunning (NOT flipped to StIdle).
	if st, _ := ctrl.GetSMState(taskName); st != api.StRunning {
		t.Fatalf("SM state = %s after stale clean exit, want StRunning (stale guard precedes clean-exit drop)", st)
	}
	if e, _ := tracker.Get(taskName); e.CurrentPID != 200 || e.PIDGeneration != 2 {
		t.Fatalf("stale clean exit mutated current tracking: %+v", e)
	}
}

// TestHandleLoopEvent_GenerationlessExitUnchanged is spec test 8: an EvChildExit
// with NO pid_generation (the synthetic/foreign/liveness parity case) must route
// exactly as before the guard existed — at StRunning, a NON-clean generationless
// exit still drives StRunning→StBackoffWaiting. This proves the guard treats
// gen-less exits as current (passes through).
func TestHandleLoopEvent_GenerationlessExitUnchanged(t *testing.T) {
	taskName := `\mcp-local-hub-serena-default`
	ctrl, tracker, _ := newStaleExitController(t, taskName)

	tracker.MarkSpawned(taskName, 100, time.Now().UTC())
	ctrl.smStates.Store(taskName, api.StRunning)

	// No pid_generation key at all — the pre-child synthetic / foreign /
	// liveness EvChildExit shape.
	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: taskName,
		Body:     map[string]any{"exit_code": 1},
	})

	// Routes normally: StRunning + (non-clean) EvChildExit → StBackoffWaiting.
	if st, _ := ctrl.GetSMState(taskName); st != api.StBackoffWaiting {
		t.Fatalf("SM state = %s after generationless exit, want StBackoffWaiting (guard must pass gen-less exits)", st)
	}
	if got := tracker.CrashCountInWindow(taskName, time.Now().UTC(), respawnFailureWindow); got != 1 {
		t.Fatalf("crash count = %d, want 1 (generationless exit must record a real crash)", got)
	}
}

// TestHandleLoopEvent_GenerationlessExitAtZeroGenPasses guards the boundary: a
// body carrying pid_generation:0 (a not-yet-spawned / synthetic value) is
// treated as current (the guard only drops gen>0 that is < current), so it
// routes like a generationless exit.
func TestHandleLoopEvent_GenerationlessExitAtZeroGenPasses(t *testing.T) {
	taskName := `\mcp-local-hub-serena-default`
	ctrl, tracker, _ := newStaleExitController(t, taskName)
	tracker.MarkSpawned(taskName, 100, time.Now().UTC())
	tracker.MarkSpawned(taskName, 200, time.Now().UTC()) // gen2 current
	ctrl.smStates.Store(taskName, api.StRunning)

	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: taskName,
		Body:     map[string]any{"pid": 200, "pid_generation": 0, "exit_code": 1},
	})
	if st, _ := ctrl.GetSMState(taskName); st != api.StBackoffWaiting {
		t.Fatalf("SM state = %s, want StBackoffWaiting (pid_generation:0 must pass as current)", st)
	}
}

// loopStaleExitHarness runs a REAL event loop with handleLoopEvent registered so
// spawn side effects (PostSelf EvHealthOK) and the StExiting queued-respawn flow
// actually execute. The fake spawn calls MarkSpawned (bumping the generation)
// and increments spawnCalls, mirroring the real generation semantics P1a depends
// on. A barrier event drains the loop so the test reads state race-free.
type loopStaleExitHarness struct {
	ctrl       *supervisorController
	tracker    *DaemonRuntimeTracker
	loop       *api.EventLoop
	spawnCalls *atomic.Int32
	spawnPID   *atomic.Int32
	taskName   string
}

func newLoopStaleExitHarness(t *testing.T, taskName string) *loopStaleExitHarness {
	t.Helper()
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	var spawnPID atomic.Int32
	spawnPID.Store(50000)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	loop := api.NewEventLoop(64)
	descriptor := api.SupervisorDaemon{TaskName: taskName, Server: "serena", Daemon: "default"}
	h := &loopStaleExitHarness{
		tracker:    tracker,
		loop:       loop,
		spawnCalls: &spawnCalls,
		spawnPID:   &spawnPID,
		taskName:   taskName,
	}
	h.ctrl = &supervisorController{
		intentCache:  newIntentCache(),
		eventLoop:    loop,
		tracker:      tracker,
		events:       events,
		daemonIntent: newDaemonIntentCache(),
		spawn: func(d api.SupervisorDaemon) error {
			// Real generation semantics: each spawn bumps the tracker generation
			// via MarkSpawned, exactly as the production spawn closure does.
			pid := int(spawnPID.Add(1))
			tracker.MarkSpawned(d.TaskName, pid, time.Now().UTC())
			spawnCalls.Add(1)
			return nil
		},
		terminate: func(d api.SupervisorDaemon) error {
			// Own-spawned terminate: the real cmd.Wait exit drives the next
			// transition, so the fake just marks the tracker exited (mirroring the
			// production terminate's nil-return-means-gone contract). The test
			// posts the follow-up EvChildExit explicitly.
			tracker.MarkExited(d.TaskName)
			return nil
		},
		statePath:           statePath,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	h.ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{descriptor}})
	loop.RegisterHandler(h.ctrl.handleLoopEvent)
	go loop.Run(ctx)
	return h
}

// sync drains the loop via a barrier so the test reads state after every prior
// event has been processed.
func (h *loopStaleExitHarness) sync() {
	done := make(chan struct{})
	h.loop.Post(api.LoopEvent{
		Kind: evReapBarrier,
		Body: map[string]any{reapBarrierResultBodyKey: done},
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		panic("loopStaleExitHarness sync timed out (loop wedged?)")
	}
}

func (h *loopStaleExitHarness) post(ev api.LoopEvent) {
	h.loop.Post(ev)
	h.sync()
}

// TestHandleLoopEvent_CurrentGenerationExitAtStExitingDrivesQueuedRespawn is
// spec test 6: the deliberate-kill regression guard. Seed StExiting with a
// queued respawn and a current-generation child; the current-generation
// EvChildExit (which terminate produces after clearing CurrentPID) must drive
// StExiting → StSpawning (respawn), NOT be dropped by the stale guard. A PID
// guard would deadlock here because terminate zeroed CurrentPID before the exit.
func TestHandleLoopEvent_CurrentGenerationExitAtStExitingDrivesQueuedRespawn(t *testing.T) {
	taskName := `\mcp-local-hub-serena-default`
	h := newLoopStaleExitHarness(t, taskName)

	// Seed a running gen1 child, then drive EvManualRestart → StExiting (queued
	// respawn) + terminate. The terminate fake clears the tracker (CurrentPID=0)
	// but the generation stays 1 (terminate does not bump it).
	h.tracker.MarkSpawned(taskName, 40000, time.Now().UTC())
	h.ctrl.smStates.Store(taskName, api.StRunning)
	h.ctrl.ownSpawned.Store(taskName, true)
	h.post(api.LoopEvent{Kind: api.EvManualRestart, TaskName: taskName})

	if st, _ := h.ctrl.GetSMState(taskName); st != api.StExiting {
		t.Fatalf("after EvManualRestart SM = %s, want StExiting", st)
	}
	spawnsBefore := h.spawnCalls.Load()

	// The deliberately-killed child's real exit carries the CURRENT generation
	// (1) — terminate did not bump it. It must pass the stale guard and drive the
	// queued respawn.
	h.post(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: taskName,
		Body:     map[string]any{"pid": 40000, "pid_generation": 1, "exit_code": 0},
	})

	if got := h.spawnCalls.Load() - spawnsBefore; got != 1 {
		t.Fatalf("queued respawn fired %d spawns, want 1 (current-gen exit must drive StExiting→StSpawning)", got)
	}
	// After respawn the daemon is running again at a bumped generation.
	if e, _ := h.tracker.Get(taskName); e.State != daemonRuntimeStateRunning || e.PIDGeneration != 2 {
		t.Fatalf("after queued respawn tracker = %+v, want running generation=2", e)
	}
	if st, _ := h.ctrl.GetSMState(taskName); st != api.StRunning {
		t.Fatalf("after queued respawn SM = %s, want StRunning", st)
	}
}

// TestIncidentReplay_LateExitAfterRespawnNoDuplicateSpawn is spec test 9: the
// 2026-07-01 incident, compressed. spawn(gen1) → liveness EvManualRestart
// (port_unbound, live PID) → StExiting → current exit(gen1) → respawn(gen2) →
// inject a LATE duplicate EvChildExit stamped gen1. The late duplicate must be
// dropped by the stale guard so NO third spawn fires and the gen2 tracking is
// intact. Pre-fix, the late gen1 exit would have driven a duplicate spawn
// (manufacturing the lost child).
func TestIncidentReplay_LateExitAfterRespawnNoDuplicateSpawn(t *testing.T) {
	taskName := `\mcp-local-hub-serena-default`
	h := newLoopStaleExitHarness(t, taskName)

	// spawn(gen1): a running child, StRunning, own-spawned.
	h.tracker.MarkSpawned(taskName, 40000, time.Now().UTC())
	h.ctrl.smStates.Store(taskName, api.StRunning)
	h.ctrl.ownSpawned.Store(taskName, true)

	// Liveness EvManualRestart (a live-PID reason like port_unbound: the sweep
	// keeps the PID for the terminate-first handoff, so runtime_pid_cleared is
	// NOT set) → StExiting + queued respawn + terminate.
	h.post(api.LoopEvent{
		Kind:     api.EvManualRestart,
		TaskName: taskName,
		Body:     map[string]any{"reason": supervisorLivenessReasonPortUnbound, "pid": 40000, "port": 9151},
	})
	if st, _ := h.ctrl.GetSMState(taskName); st != api.StExiting {
		t.Fatalf("after liveness restart SM = %s, want StExiting", st)
	}

	// The current child's real exit (gen1) drives the queued respawn → gen2.
	h.post(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: taskName,
		Body:     map[string]any{"pid": 40000, "pid_generation": 1, "exit_code": 0},
	})
	spawnsAfterRespawn := h.spawnCalls.Load()
	if spawnsAfterRespawn != 1 {
		t.Fatalf("after respawn spawnCalls = %d, want exactly 1", spawnsAfterRespawn)
	}
	if e, _ := h.tracker.Get(taskName); e.PIDGeneration != 2 || e.State != daemonRuntimeStateRunning {
		t.Fatalf("after respawn tracker = %+v, want running generation=2", e)
	}

	// THE INCIDENT: a LATE duplicate exit of the OLD gen1 child arrives now (the
	// forgotten wait goroutine finally fired). It must be dropped — no third
	// spawn, gen2 intact.
	h.post(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: taskName,
		Body:     map[string]any{"pid": 40000, "pid_generation": 1, "exit_code": 1},
	})

	// Only the queued respawn incremented spawnCalls (the initial seed used
	// tracker.MarkSpawned directly, not the fake spawn). So the total must STILL
	// be 1 after the late duplicate — the duplicate fired NO spawn.
	if got := h.spawnCalls.Load(); got != 1 {
		t.Fatalf("late duplicate gen1 exit drove extra spawn(s): spawnCalls = %d, want 1 (duplicate must be dropped)", got)
	}
	if e, _ := h.tracker.Get(taskName); e.PIDGeneration != 2 || e.State != daemonRuntimeStateRunning {
		t.Fatalf("late duplicate mutated tracking: %+v, want running generation=2 intact", e)
	}
	if st, _ := h.ctrl.GetSMState(taskName); st != api.StRunning {
		t.Fatalf("late duplicate changed SM = %s, want StRunning", st)
	}
}

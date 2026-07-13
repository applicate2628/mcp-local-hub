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

// armGenController builds a minimal controller whose armRespawnBackoffTimer fires
// through the spawn FALLBACK (eventLoop left nil), so a stacked/coincident set of
// arms is observable as a spawn count. The generation guard sits BEFORE the
// eventLoop-Post / spawn-fallback branch, so this path exercises exactly the same
// drop logic the production EvTimerDue path uses.
func armGenController(t *testing.T) (*supervisorController, *atomic.Int32, string) {
	t.Helper()
	tmp := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmp, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var spawns atomic.Int32
	ctrl := &supervisorController{
		events: events,
		ctx:    ctx,
		// eventLoop deliberately nil → armRespawnBackoffTimer fires via the spawn
		// fallback, so each NON-superseded timer bumps the counter exactly once.
		spawn: func(api.SupervisorDaemon) error { spawns.Add(1); return nil },
	}
	return ctrl, &spawns, eventsPath
}

// TestRespawnArmGen_StackedTimersCollapseToOne: two coincident arms (both stored
// before either timer fires) collapse to exactly ONE effective respawn — the
// superseded (older-generation) timer is dropped by the generation guard. This is
// the round-6 fix for Sol's redundant coincident respawn timer.
//
// NON-VACUITY (verified manually, reported in the round-6 artifact): commenting
// out the fire-time respawnArmGen guard makes BOTH timers fire → spawns == 2, so
// this assertion genuinely exercises the guard rather than passing trivially.
func TestRespawnArmGen_StackedTimersCollapseToOne(t *testing.T) {
	ctrl, spawns, eventsPath := armGenController(t)

	// Canonical TaskName (raw == canonical) so the SM-state re-check keyed on the
	// passed taskName finds the StBackoffWaiting we store below.
	d := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-armgen`}
	task := canonicalSupervisorTaskName(d.TaskName)
	ctrl.smStates.Store(task, api.StBackoffWaiting) // state re-check must NOT drop; isolate the gen guard

	const backoff = 50 * time.Millisecond
	// Two arms BEFORE either fires: nextRespawnArmGen runs synchronously inside each
	// call (gen 1, then gen 2) before its timer goroutine starts, so by the time
	// either 50ms timer fires the current generation is already 2. Timer#1 (captured
	// gen 1) is superseded and drops; timer#2 (gen 2) fires.
	ctrl.armRespawnBackoffTimer(d, d.TaskName, backoff)
	ctrl.armRespawnBackoffTimer(d, d.TaskName, backoff)

	// Wait well past the backoff so BOTH timers have definitely fired (if the guard
	// were absent, the count would settle at 2).
	time.Sleep(6 * backoff)

	if got := spawns.Load(); got != 1 {
		t.Fatalf("stacked coincident arms produced %d spawns, want 1 (the superseded first timer must be dropped by the respawnArmGen guard)", got)
	}
	assertEventInLog(t, eventsPath, "daemon-respawn-timer-superseded")
}

// TestRespawnArmGen_NormalBackoffFiresEachArm: the generation guard must NOT break
// the normal exponential-backoff cadence, where each daemon has ONE active respawn
// timer that fires and drives the next transition BEFORE the next arm. Each arm is
// the latest at its own fire time, so every arm must fire.
func TestRespawnArmGen_NormalBackoffFiresEachArm(t *testing.T) {
	ctrl, spawns, _ := armGenController(t)

	d := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-armgen-normal`}
	task := canonicalSupervisorTaskName(d.TaskName)
	ctrl.smStates.Store(task, api.StBackoffWaiting)

	const backoff = 50 * time.Millisecond

	// Arm → fire → re-arm → fire. The first timer fully fires (and would drive the
	// transition) before the second arm, so the second arm is the latest at its own
	// fire time. Neither is superseded.
	ctrl.armRespawnBackoffTimer(d, d.TaskName, backoff)
	time.Sleep(4 * backoff)
	if got := spawns.Load(); got != 1 {
		t.Fatalf("first arm did not fire: %d spawns, want 1", got)
	}

	ctrl.armRespawnBackoffTimer(d, d.TaskName, backoff)
	time.Sleep(4 * backoff)
	if got := spawns.Load(); got != 2 {
		t.Fatalf("normal backoff re-arm was wrongly dropped by the generation guard: %d spawns, want 2 (the guard must only drop genuinely-superseded coincident timers)", got)
	}
}

// TestRespawnArmGen_RemoveReregisterNoABA: a stale respawn timer armed BEFORE a task
// is removed must NOT fire against a same-name RE-REGISTRATION. Round-6's per-task
// arm counter reintroduced an ABA: clearRemovedTaskRuntime Deletes the task's arm-gen
// entry, so the re-registered task's first arm got generation 1 AGAIN (deleted →
// Load-miss → 0 → +1 = 1) — which the stale pre-removal timer (also captured
// generation 1) MATCHED at the fire-time guard → it fired against the NEW incarnation
// and shortened its backoff. The round-7 GLOBAL monotonic epoch (respawnArmEpoch,
// never reset/reused) hands the re-registered arm a FRESH high epoch, so the stale
// timer's captured epoch can never match — it is dropped. Both reviewers (Sol P2 +
// Terra HIGH-BROKEN) flagged the ABA and asked for this test.
//
// NON-VACUITY (verified manually, reported in the round-7 artifact): temporarily
// reverting nextRespawnArmGen to the per-task-reset counter makes the stale gen-1
// timer match the re-registered gen-1 arm → both the "epoch reused" and the "stale
// timer fired" assertions below trip (epoch == 1, spawns == 1), so this test
// genuinely exercises the global-epoch fix rather than passing trivially.
func TestRespawnArmGen_RemoveReregisterNoABA(t *testing.T) {
	ctrl, spawns, eventsPath := armGenController(t)

	d := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-armgen-aba`}
	task := canonicalSupervisorTaskName(d.TaskName)

	// Backoff long enough that the stale timer stays pending across the whole
	// synchronous remove → re-register sequence below (microseconds), so by the time
	// it fires the new incarnation's arm epoch is already recorded — otherwise a
	// mid-window fire would drop on a map-miss and mask the ABA in the neuter run.
	const backoff = 80 * time.Millisecond

	// (1) Arm the STALE timer for T (first arm → epoch e1). T must be in backoff so
	// the timer's fire-time SM-state re-check passes and it reaches the epoch guard.
	ctrl.smStates.Store(task, api.StBackoffWaiting)
	ctrl.armRespawnBackoffTimer(d, d.TaskName, backoff)

	// (2) Remove T: clearRemovedTaskRuntime Deletes the arm-gen entry (and smStates).
	ctrl.clearRemovedTaskRuntime(d.TaskName)

	// (3) Re-register T under the SAME canonical name: a new arm takes a fresh global
	// epoch e2 (> the stale timer's e1) with the round-7 counter; with the reverted
	// per-task-reset counter it would be 1 AGAIN — the ABA. Put T back in backoff so
	// the stale timer's SM-state re-check passes and the epoch guard is the ONLY thing
	// that can distinguish the stale timer from a legitimate one. No competing firing
	// timer is armed for e2 — we only record its epoch — so any spawn observed below
	// is the stale timer wrongly firing.
	e2 := ctrl.nextRespawnArmGen(task)
	ctrl.smStates.Store(task, api.StBackoffWaiting)
	if e2 <= 1 {
		// Root-cause signal: the re-registered arm reused a low generation instead of
		// a fresh high global epoch. t.Errorf (not Fatalf) so the stale-fire symptom
		// below is still observed in the same neuter run.
		t.Errorf("re-registered arm epoch = %d, want a fresh high global epoch > 1 (a per-task-reset counter hands back 1 — the reused generation is the ABA root cause)", e2)
	}

	// (4) Wait past the backoff so the stale e1 timer has definitely fired.
	time.Sleep(6 * backoff)

	// The stale timer must be DROPPED — it must NOT fire against the re-registered task.
	if got := spawns.Load(); got != 0 {
		t.Fatalf("stale pre-removal timer fired against the re-registered task: %d spawns, want 0 (the global-epoch guard must drop the stale timer whose captured epoch no longer matches the re-registered arm — ABA regression)", got)
	}
	assertEventInLog(t, eventsPath, "daemon-respawn-timer-superseded")
}

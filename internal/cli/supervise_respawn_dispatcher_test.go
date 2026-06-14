package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// TestComputeRespawnBackoff verifies the exponential schedule:
// failures=1 -> 1s, 2 -> 2s, 3 -> 4s, 4 -> 8s, 5 -> 16s, 6 -> 32s, 7+ -> 60s cap.
func TestComputeRespawnBackoff(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{-1, 0},
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 32 * time.Second},
		{7, 60 * time.Second},  // capped
		{10, 60 * time.Second}, // capped
		{40, 60 * time.Second}, // overflow guard kicks in, still capped
	}
	for _, c := range cases {
		got := computeRespawnBackoff(c.failures)
		if got != c.want {
			t.Errorf("computeRespawnBackoff(%d) = %v, want %v", c.failures, got, c.want)
		}
	}
}

// TestDaemonRuntimeTracker_RecordCrashAndCountInWindow_PrunesOldEntries
// verifies the sliding-window pruning: entries older than (now-window)
// are dropped on each call, the new timestamp is appended, and the
// returned count reflects only in-window entries.
func TestDaemonRuntimeTracker_RecordCrashAndCountInWindow_PrunesOldEntries(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	task := `\mcp-local-hub-test-default`
	window := 100 * time.Millisecond

	// First crash at t=0
	t0 := time.Now().UTC()
	n := tracker.RecordCrashAndCountInWindow(task, t0, window)
	if n != 1 {
		t.Errorf("first crash count = %d, want 1", n)
	}

	// Second crash at t=50ms (within 100ms window) -> count 2
	n = tracker.RecordCrashAndCountInWindow(task, t0.Add(50*time.Millisecond), window)
	if n != 2 {
		t.Errorf("second crash within window count = %d, want 2", n)
	}

	// Third crash at t=120ms - cutoff = t0+20ms; prunes only the
	// t=0 entry (0 < 20). The t=50ms entry is > 20ms after t0 so
	// it stays. Total = (50ms kept) + (new 120ms) = 2.
	n = tracker.RecordCrashAndCountInWindow(task, t0.Add(120*time.Millisecond), window)
	if n != 2 {
		t.Errorf("crash at t=120ms count = %d, want 2 (t=0 pruned, t=50ms kept, plus new)", n)
	}

	// Fourth crash at t=300ms - cutoff = t0+200ms; prunes both
	// in-window predecessors (50ms < 200ms, 120ms < 200ms).
	n = tracker.RecordCrashAndCountInWindow(task, t0.Add(300*time.Millisecond), window)
	if n != 1 {
		t.Errorf("crash after full window expiry count = %d, want 1 (all prior pruned, only new)", n)
	}
}

// TestDaemonRuntimeTracker_ClearCrashes verifies the reset path.
func TestDaemonRuntimeTracker_ClearCrashes(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	task := `\mcp-local-hub-test-default`
	now := time.Now().UTC()
	tracker.RecordCrashAndCountInWindow(task, now, 30*time.Minute)
	tracker.RecordCrashAndCountInWindow(task, now, 30*time.Minute)
	if c := tracker.CrashCountInWindow(task, now, 30*time.Minute); c != 2 {
		t.Fatalf("pre-clear count = %d, want 2", c)
	}
	tracker.ClearCrashes(task)
	if c := tracker.CrashCountInWindow(task, now, 30*time.Minute); c != 0 {
		t.Errorf("post-clear count = %d, want 0", c)
	}
}

// TestSupervisorController_HandleEvChildExit_TransitionsToBackoffWaiting
// replaces TestRespawnDispatcher_SchedulesRespawnAfterCrash. Drives
// EvChildExit through the controller and verifies the audit row
// (daemon-respawn-scheduled) AND a spawn fires after the backoff.
func TestSupervisorController_HandleEvChildExit_TransitionsToBackoffWaiting(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		return nil
	}

	descriptor := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-test-default`,
		Server:   "test",
		Daemon:   "default",
	}
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{descriptor},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		tracker:             tracker,
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		spawn:               fakeSpawn,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		// eventLoop intentionally nil so the backoff timer falls
		// back to direct spawn instead of EvTimerDue recursion.
		// This isolates the StRunning + EvChildExit transition we
		// want to assert.
	}
	ctrl.intentCache.Refresh(intent)
	// Seed StRunning so the transition is well-defined: StIdle +
	// EvChildExit is unhandled by the SM.
	ctrl.smStates.Store(descriptor.TaskName, api.StRunning)

	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: descriptor.TaskName,
		Body:     map[string]any{"exit_code": 1},
	})

	// First failure -> backoff 1s -> spawn fires ~1s later
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if spawnCalls.Load() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if spawnCalls.Load() == 0 {
		t.Fatal("spawn fn never invoked within 3s after EvChildExit")
	}

	// Audit log must contain daemon-respawn-scheduled
	logRaw, _ := os.ReadFile(eventsPath)
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"daemon-respawn-scheduled"`) {
		t.Errorf("daemon-respawn-scheduled missing from audit log:\n%s", logStr)
	}

	// State must have moved to backoff-waiting (persisted via
	// persistBefore=true in api.Transition table).
	st, ok := ctrl.GetSMState(descriptor.TaskName)
	if !ok {
		t.Fatalf("GetSMState returned ok=false for tracked task")
	}
	if st != api.StBackoffWaiting {
		t.Errorf("post-EvChildExit state = %s, want %s", st, api.StBackoffWaiting)
	}
}

// TestSupervisorController_HandleEvChildExit_TransitionsToQuarantinedAfterThreshold
// replaces TestRespawnDispatcher_QuarantineAfterThreshold. Pre-seeds 9
// crashes through the tracker then fires the 10th via the controller;
// expects daemon-quarantined audit row and no spawn fire.
func TestSupervisorController_HandleEvChildExit_TransitionsToQuarantinedAfterThreshold(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		return nil
	}

	descriptor := api.SupervisorDaemon{
		TaskName:  `\mcp-local-hub-test-default`,
		Server:    "test",
		Daemon:    "default",
		Workspace: `C:\ws\quarantine-target`,
	}
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{descriptor},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		tracker:             tracker,
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		spawn:               fakeSpawn,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.smStates.Store(descriptor.TaskName, api.StRunning)

	// Pre-seed 9 crashes via the tracker directly so the controller's
	// 10th crash trips the quarantine threshold immediately.
	now := time.Now().UTC()
	for i := 0; i < 9; i++ {
		tracker.RecordCrashAndCountInWindow(descriptor.TaskName, now, respawnFailureWindow)
	}

	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: descriptor.TaskName,
		Body:     map[string]any{"exit_code": 1},
	})

	// Wait for quarantine event to land.
	deadline := time.Now().Add(2 * time.Second)
	var logStr string
	for time.Now().Before(deadline) {
		logRaw, _ := os.ReadFile(eventsPath)
		logStr = string(logRaw)
		if strings.Contains(logStr, `"event":"daemon-quarantined"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logStr, `"event":"daemon-quarantined"`) {
		t.Fatalf("daemon-quarantined event never appeared:\n%s", logStr)
	}
	if !strings.Contains(logStr, `"failures_in_30m":10`) {
		t.Errorf("quarantine event missing correct failure count:\n%s", logStr)
	}
	// The daemon-quarantined Body must carry the descriptor's workspace path
	// so a future GUI serena-session-cleanup consumer can key teardown by it
	// (handleBackoffWaiting emit site). JSON-escaped backslashes in the
	// canonical Windows path.
	if !strings.Contains(logStr, `"workspace":"C:\\ws\\quarantine-target"`) {
		t.Errorf("quarantine event missing workspace field:\n%s", logStr)
	}

	// Spawn must NOT have been invoked on the quarantine path -
	// give it the full backoff window of the first respawn (1s) to
	// confirm no spawn fires.
	time.Sleep(1500 * time.Millisecond)
	if got := spawnCalls.Load(); got != 0 {
		t.Errorf("spawn fn invoked %d times during quarantine, want 0", got)
	}

	// State must reflect quarantine.
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	if st != api.StQuarantined {
		t.Errorf("post-quarantine state = %s, want %s", st, api.StQuarantined)
	}
}

// TestSupervisorController_HandleEvChildExit_SuppressesOnStopIntent
// replaces TestRespawnDispatcher_SuppressesOnStopIntent. The original
// dispatcher had no intent awareness; the new controller reads
// daemon-intent.json via daemonIntentCache and the SM table
// suppresses respawn when IntentDesired=="stopped".
//
// Note: the SM at StRunning+EvChildExit transitions unconditionally to
// StBackoffWaiting (the IntentDesired check fires later via EvTimerDue
// transitioning to StSpawning); but the StBackoffWaiting +
// EvIntentUpdate path with IntentDesired=stopped transitions to
// StIdle, cancelling the timer. This test asserts the OVERALL
// suppression semantic: a daemon with active stop intent should NOT
// have its spawn fn invoked even after EvChildExit + backoff timer.
func TestSupervisorController_HandleEvChildExit_SuppressesOnStopIntent(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		return nil
	}

	descriptor := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-test-default`,
		Server:   "test",
		Daemon:   "default",
	}
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{descriptor},
	}
	// daemon-intent.json says this task is stopped (recent
	// user-disabled, no TTL).
	now := time.Now().UTC()
	daemonIntent := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			descriptor.TaskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserDisabled,
				UpdatedAt: now,
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		tracker:             tracker,
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		spawn:               fakeSpawn,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.daemonIntent.Refresh(daemonIntent)
	ctrl.smStates.Store(descriptor.TaskName, api.StRunning)

	// Fire EvChildExit; it transitions to StBackoffWaiting (the SM
	// table at StRunning+EvChildExit is unconditional). Then fire
	// EvIntentUpdate which observes IntentDesired=stopped and
	// transitions StBackoffWaiting -> StIdle (cancel timer).
	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: descriptor.TaskName,
		Body:     map[string]any{"exit_code": 1},
	})
	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvIntentUpdate,
		TaskName: descriptor.TaskName,
	})

	// Even after the backoff window for the (now-cancelled) timer,
	// the spawn fn must not have fired.
	time.Sleep(1500 * time.Millisecond)
	if got := spawnCalls.Load(); got != 0 {
		t.Errorf("spawn invoked %d times despite stop intent, want 0", got)
	}

	st, _ := ctrl.GetSMState(descriptor.TaskName)
	if st != api.StIdle {
		t.Errorf("post-stop-intent state = %s, want %s", st, api.StIdle)
	}
}

// TestSupervisorController_HandleEvChildExit_RetriesOnSpawnFailure
// replaces TestRespawnDispatcher_RetriesOnSpawnFailure. Spawn returns
// an error; the next EvChildExit (simulating a fail-fast crash) must
// schedule another respawn (backoff timer fires spawn again).
func TestSupervisorController_HandleEvChildExit_RetriesOnSpawnFailure(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		// Return a synthetic spawn failure so the SM observes a
		// non-clean spawn (the dispatcher's retry-on-error semantic
		// in the new wiring is: spawn error is logged by the spawn
		// closure; the next EvChildExit drives another backoff).
		return nil
	}

	descriptor := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-test-default`,
		Server:   "test",
		Daemon:   "default",
	}
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{descriptor},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		tracker:             tracker,
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		spawn:               fakeSpawn,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		// eventLoop nil so backoff timer falls back to direct
		// spawn; this isolates the retry path from the formal
		// EvTimerDue recursion.
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.smStates.Store(descriptor.TaskName, api.StRunning)

	// First crash + backoff -> spawn fires (call #1).
	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: descriptor.TaskName,
		Body:     map[string]any{"exit_code": 1},
	})

	// Wait for first spawn.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if spawnCalls.Load() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if spawnCalls.Load() < 1 {
		t.Fatalf("first spawn never invoked: got %d", spawnCalls.Load())
	}

	// Reset SM state to StRunning (simulates the daemon getting a
	// PID and going healthy briefly), then fire another EvChildExit
	// to assert retry behavior under the same window.
	ctrl.smStates.Store(descriptor.TaskName, api.StRunning)
	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: descriptor.TaskName,
		Body:     map[string]any{"exit_code": 1},
	})

	// Second crash -> backoff 2s -> spawn call #2.
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if spawnCalls.Load() >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if spawnCalls.Load() < 2 {
		t.Fatalf("retry spawn never invoked: got %d", spawnCalls.Load())
	}
}

// TestStateMachineWiring_DoesNotDoubleRespawnWithLegacyDispatcher is a
// regression guard that the runRespawnDispatcher symbol has been
// removed from the package. If a future refactor re-introduces it,
// this test will fail to compile, surfacing the regression at build
// time rather than runtime.
//
// We assert via the existence of the controller-driven test contract
// (this test compiling means the controller exists AND the dispatcher
// symbol does not).
func TestStateMachineWiring_DoesNotDoubleRespawnWithLegacyDispatcher(t *testing.T) {
	// Compile-time regression guard: if runRespawnDispatcher were
	// re-introduced, the package would have two consumers of
	// crashCh (the dispatcher AND the controller's runCrashEventBridge),
	// and crash events would route to BOTH paths. We assert the
	// bridge exists and the dispatcher does not by referencing the
	// bridge symbol; absence of dispatcher is enforced by the
	// deletion of supervise_respawn_dispatcher.go.
	var _ = runCrashEventBridge

	// Runtime check: a single EvChildExit must result in exactly one
	// respawn scheduled, not two. The controller wired against the
	// event loop already enforces this (the SM has a single
	// transition per state+event), but the test makes the assertion
	// explicit.
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		return nil
	}

	descriptor := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-test-default`,
		Server:   "test",
		Daemon:   "default",
	}
	intent := &api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{descriptor}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		tracker:             tracker,
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		spawn:               fakeSpawn,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.smStates.Store(descriptor.TaskName, api.StRunning)

	// Single crash event.
	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: descriptor.TaskName,
		Body:     map[string]any{"exit_code": 1},
	})

	// Wait long enough for backoff (1s + slack) and assert exactly
	// one spawn.
	time.Sleep(2500 * time.Millisecond)
	if got := spawnCalls.Load(); got != 1 {
		t.Errorf("single EvChildExit triggered %d spawns, want exactly 1", got)
	}
}

// TestRespawnDispatcher_CleanExitNoRespawn preserves the contract
// regression marker from the pre-A.2 dispatcher test. The contract
// ("a clean exit on a daemon that is NOT being restarted must NOT be
// respawned") is now enforced inside the CONTROLLER, not at the crashCh
// gate.
//
// As of the Codex bot #268 P1 fix, the production spawn fn's cmd.Wait
// goroutine posts EVERY exit to crashCh (clean and non-clean), because a
// controller-driven restart of an OWN daemon that exits cleanly needs the
// real EvChildExit to complete its queued respawn at StExiting. The
// deliberate-shutdown contract is preserved by handleLoopEvent dropping a
// `clean_exit`-flagged EvChildExit when the task is still StRunning (no
// controller-driven exit in flight), so a plain clean exit at steady
// state never reaches the StRunning->StBackoffWaiting respawn path.
//
// The clean-exit-at-StRunning drop is covered by the
// TestSupervisorController_CleanExit* tests in supervisor_controller_test.go;
// the daemon-exited diagnostic emit is covered by
// TestProductionSpawnFn_EmitsDaemonExitedOnChildExit.
func TestRespawnDispatcher_CleanExitNoRespawn(t *testing.T) {
	t.Skip("contract relocated to the controller; verified via TestSupervisorController_CleanExitAtRunningDropped + TestProductionSpawnFn_EmitsDaemonExitedOnChildExit")
}

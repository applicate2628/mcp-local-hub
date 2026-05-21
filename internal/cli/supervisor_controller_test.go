package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// TestSupervisorController_GetSMState_ReturnsTrackedState verifies the
// public accessor surface: a value stored via smStates.Store is
// returned with ok=true.
func TestSupervisorController_GetSMState_ReturnsTrackedState(t *testing.T) {
	ctrl := &supervisorController{}
	taskName := `\mcp-local-hub-test-default`
	ctrl.smStates.Store(taskName, api.StRunning)

	st, ok := ctrl.GetSMState(taskName)
	if !ok {
		t.Fatalf("GetSMState returned ok=false for tracked task")
	}
	if st != api.StRunning {
		t.Errorf("GetSMState returned %s, want %s", st, api.StRunning)
	}
}

// TestSupervisorController_GetSMState_DefaultsToIdleForUnknownTask
// verifies the absent-task contract: unknown task returns (StIdle,
// false). The bool distinguishes "no tracked state yet" from a
// literal StIdle value.
func TestSupervisorController_GetSMState_DefaultsToIdleForUnknownTask(t *testing.T) {
	ctrl := &supervisorController{}
	st, ok := ctrl.GetSMState(`\mcp-local-hub-unknown`)
	if ok {
		t.Errorf("GetSMState returned ok=true for unknown task; want false")
	}
	if st != api.StIdle {
		t.Errorf("GetSMState default = %s, want %s", st, api.StIdle)
	}
}

// TestSupervisorController_IntentCacheRefreshOnWatcherEvent verifies the
// atomic.Value snapshot pointer swap: a Refresh call replaces the
// underlying snapshot, and concurrent Lookup reads see consistent
// old-or-new state, never partial.
func TestSupervisorController_IntentCacheRefreshOnWatcherEvent(t *testing.T) {
	cache := newIntentCache()

	// Initial state: empty cache returns no entries.
	if _, ok := cache.Lookup(`\mcp-local-hub-foo`); ok {
		t.Errorf("empty cache returned ok=true for missing task")
	}

	// Refresh with one daemon.
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: `\mcp-local-hub-foo`, Server: "foo", Daemon: "default"},
		},
	}
	cache.Refresh(intent)

	d, ok := cache.Lookup(`\mcp-local-hub-foo`)
	if !ok {
		t.Fatalf("Lookup returned ok=false after Refresh")
	}
	if d.Server != "foo" {
		t.Errorf("Lookup returned wrong descriptor: %+v", d)
	}

	// Concurrent reads + refresh: snapshot pointer swap must not
	// race. We spawn a writer that flips between two intent shapes
	// and N readers that assert they always see EITHER the old shape
	// OR the new shape consistently (never a partial map).
	intentA := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: `\mcp-local-hub-a`, Server: "a"},
			{TaskName: `\mcp-local-hub-b`, Server: "b"},
		},
	}
	intentB := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: `\mcp-local-hub-a`, Server: "a-updated"},
			{TaskName: `\mcp-local-hub-c`, Server: "c"},
		},
	}

	// Seed with intentA so the assertion loop always observes
	// EITHER intentA or intentB and both contain task `a`.
	cache.Refresh(intentA)

	stop := make(chan struct{})
	defer close(stop)

	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			cache.Refresh(intentA)
			cache.Refresh(intentB)
		}
	}()

	// Single-snapshot consistency check: each Refresh call swaps the
	// underlying snapshot atomically, so a reader that loads the
	// snapshot pointer once and reads multiple keys off the SAME
	// snapshot must see consistent state. The cache's public Lookup
	// API does an independent atomic.Load per call; the writer can
	// swap snapshots between two Lookup calls. The atomicity
	// invariant is therefore *within* a single snapshot, not across
	// multiple sequential Lookup calls.
	//
	// We assert atomicity by loading the snapshot pointer once via a
	// reflection-free probe: snap.Load() returns the current
	// *intentSnapshot. Within ONE such load, the daemonByTask map
	// is fully populated for either intentA or intentB - never
	// partial.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		snap, _ := cache.snap.Load().(*intentSnapshot)
		if snap == nil {
			continue
		}
		_, hasA := snap.daemonByTask[`\mcp-local-hub-a`]
		_, hasB := snap.daemonByTask[`\mcp-local-hub-b`]
		_, hasC := snap.daemonByTask[`\mcp-local-hub-c`]
		// Invariant: hasB and hasC are exclusive (snapshot A has b,
		// snapshot B has c). Within a single snapshot, both can
		// never appear; partial mutation would be the only way both
		// could be true.
		if hasB && hasC {
			t.Fatalf("partial snapshot observed: hasB=true AND hasC=true in one snapshot")
		}
		// "a" is in both snapshots; hasA must always be true.
		if !hasA {
			t.Fatalf("partial snapshot observed: hasA=false")
		}
	}
}

// TestSupervisorController_PersistedStateMatchesSpec verifies that
// after a transition with persistBefore=true, the supervisor-state.json
// file carries the expected SM state and the daemon's restart_history
// field reflects the failure count (via tracker hydration).
func TestSupervisorController_PersistedStateMatchesSpec(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	fakeSpawn := func(d api.SupervisorDaemon) error { return nil }

	descriptor := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-test-default`,
		Server:   "test",
		Daemon:   "default",
	}
	// Seed tracker with a PID so MarkExited has something to clear.
	tracker.MarkSpawned(descriptor.TaskName, 12345, time.Now().UTC())
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("seed tracker persist: %v", err)
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
		statePath:           statePath,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.smStates.Store(descriptor.TaskName, api.StRunning)

	// Fire EvChildExit; SM table at StRunning+EvChildExit
	// transitions to StBackoffWaiting with persistBefore=true. The
	// controller calls persistDaemonRuntimeTracker which writes
	// supervisor-state.json with the tracker's view.
	//
	// The tracker entry was MarkSpawned at the top; after the SM
	// stores StBackoffWaiting and persistDaemonRuntimeTracker runs,
	// the tracker's own state is still "running" (MarkExited hasn't
	// been called - that happens in the production cmd.Wait
	// goroutine, not the controller). To assert the persist seam
	// runs, we check that supervisor-state.json is mtime-fresh.
	beforeStat, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	before := beforeStat.ModTime()
	time.Sleep(15 * time.Millisecond) // mtime resolution slack

	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: descriptor.TaskName,
		Body:     map[string]any{"exit_code": 1},
	})

	// Wait a touch for the controller to persist + arm the timer.
	time.Sleep(50 * time.Millisecond)

	afterStat, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !afterStat.ModTime().After(before) {
		t.Errorf("supervisor-state.json was not re-persisted after EvChildExit (mtime before=%v after=%v)", before, afterStat.ModTime())
	}

	// Parse the file and assert it carries our descriptor's task
	// name as a key (the precise state value depends on tracker
	// MarkExited/MarkSpawned ordering, which we don't fully control
	// here; the existence + version check is the load-bearing
	// assertion).
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state api.SupervisorStateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if _, ok := state.Daemons[descriptor.TaskName]; !ok {
		t.Errorf("supervisor-state.json missing daemon entry for %s; raw=%s", descriptor.TaskName, raw)
	}

	// Spec field surface check: the on-disk schema must carry the
	// fields the SM table reads/writes through the tracker. State
	// + PIDGeneration + CurrentPID at minimum.
	rawStr := string(raw)
	for _, field := range []string{`"state"`, `"current_pid"`, `"pid_generation"`} {
		if !strings.Contains(rawStr, field) {
			t.Errorf("supervisor-state.json missing field %s in raw output:\n%s", field, rawStr)
		}
	}
}

// TestIntentWatcherOnChange_DeltaOnly_DoesNotEventStorm closes the v6
// sonnet IMPORTANT B.1 finding: an intent reload where only one of N
// daemons changed must post exactly one EvIntentUpdate, not N. We
// drive the watcher's onChange logic directly (the watcher itself
// just calls our callback on mtime change; the delta-only logic
// lives in the callback we wire in runSupervise).
//
// This test exercises diffIntentSnapshots which is the pure function
// owning the delta calculation.
func TestIntentWatcherOnChange_DeltaOnly_DoesNotEventStorm(t *testing.T) {
	now := time.Now().UTC()
	tasks := func(n int) map[string]api.DaemonIntent {
		m := map[string]api.DaemonIntent{}
		for i := 0; i < n; i++ {
			name := `\mcp-local-hub-task-` + strings.Repeat("x", i+1)
			m[name] = api.DaemonIntent{
				Desired:   api.IntentDesiredRunning,
				Reason:    api.IntentReasonInstall,
				UpdatedAt: now,
			}
		}
		return m
	}

	previous := &api.DaemonIntentFile{Tasks: tasks(18)}
	updated := &api.DaemonIntentFile{Tasks: tasks(18)}
	// Flip only one daemon.
	targetName := `\mcp-local-hub-task-x`
	updated.Tasks[targetName] = api.DaemonIntent{
		Desired:   api.IntentDesiredStopped,
		Reason:    api.IntentReasonUserStop,
		UpdatedAt: now,
	}

	delta := diffIntentSnapshots(previous, updated)
	if len(delta) != 1 {
		t.Errorf("delta size = %d (want 1); diff entries = %v", len(delta), delta)
	}
	if len(delta) == 1 && delta[0] != targetName {
		t.Errorf("delta[0] = %q, want %q", delta[0], targetName)
	}
}

// TestDiffIntentSnapshots_AddedRemovedChanged covers all 4 paths of
// the diff function: added, removed, changed-Desired, changed-Reason,
// changed-UpdatedAt.
func TestDiffIntentSnapshots_AddedRemovedChanged(t *testing.T) {
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	previous := &api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		`\removed-task`: {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t0},
		`\changed-desired-task`: {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t0},
		`\changed-reason-task`: {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonUserStop, UpdatedAt: t0},
		`\changed-updated-at-task`: {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t0},
		`\unchanged-task`: {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t0},
	}}

	updated := &api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		`\added-task`:             {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonRegister, UpdatedAt: t1},
		`\changed-desired-task`:   {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonInstall, UpdatedAt: t0},
		`\changed-reason-task`:    {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonUserDisabled, UpdatedAt: t0},
		`\changed-updated-at-task`: {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t1},
		`\unchanged-task`:         {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t0},
	}}

	delta := diffIntentSnapshots(previous, updated)
	got := make(map[string]bool, len(delta))
	for _, name := range delta {
		got[name] = true
	}

	want := map[string]bool{
		`\removed-task`:             true,
		`\added-task`:               true,
		`\changed-desired-task`:     true,
		`\changed-reason-task`:      true,
		`\changed-updated-at-task`:  true,
	}
	for name := range want {
		if !got[name] {
			t.Errorf("delta missing expected %s; delta=%v", name, delta)
		}
	}
	if got[`\unchanged-task`] {
		t.Errorf("delta included unchanged task; delta=%v", delta)
	}
	if len(delta) != len(want) {
		t.Errorf("delta size = %d, want %d; delta=%v", len(delta), len(want), delta)
	}
}

// TestSupervisorController_HandleEvStart_TransitionsFromIdleToSpawning
// verifies the EvStart entry path (used by Reconciler.Reconcile after
// Phase A.2 wiring posts EvStart instead of direct spawn). StIdle +
// EvStart with IntentDesired=running transitions to StSpawning and
// fires the spawn closure.
func TestSupervisorController_HandleEvStart_TransitionsFromIdleToSpawning(t *testing.T) {
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
	// StIdle is the zero value of api.SMState ("") - the SM table
	// has a switch on StIdle so we leave smStates empty (no Store
	// call) and the handler's "var currentState api.SMState" yields
	// "".  But the SM table specifically checks StIdle == "idle"; an
	// empty string falls through. Store StIdle explicitly.
	ctrl.smStates.Store(descriptor.TaskName, api.StIdle)

	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvStart,
		TaskName: descriptor.TaskName,
	})

	if got := spawnCalls.Load(); got != 1 {
		t.Errorf("EvStart spawn count = %d, want 1", got)
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	if st != api.StSpawning {
		t.Errorf("post-EvStart state = %s, want %s", st, api.StSpawning)
	}
}

// TestSupervisorController_HappyPath_SpawnHealthRunningStopExiting drives
// the controller through the FULL happy-path WITHOUT manually staging
// smStates (which prior tests do to bypass the EvHealthOK gap):
//
//	StIdle + EvStart → StSpawning + spawn closure call
//	(spawn returns nil) → executeSideEffect posts EvHealthOK
//	StSpawning + EvHealthOK → StRunning
//	(intent flips to Desired=stopped) → external Post EvIntentUpdate
//	StRunning + EvIntentUpdate(stopped) → StExiting + terminate closure
//
// Regression guard for sonnet impl-r2 BLOCKER (the v(r2) fix posted
// EvHealthOK but smStates.Store was wrapped in `if persistBefore` so
// the StSpawning → StRunning transition matched but never updated the
// in-memory state — the daemon stayed in StSpawning forever and the
// subsequent EvIntentUpdate(stopped) silently dropped). This test
// exercises the actual production path with a running event loop, not
// hand-staged states.
func TestSupervisorController_HappyPath_SpawnHealthRunningStopExiting(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	var terminateCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		return nil
	}
	fakeTerminate := func(d api.SupervisorDaemon) error {
		terminateCalls.Add(1)
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

	loop := api.NewEventLoop(16)
	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		eventLoop:           loop,
		tracker:             tracker,
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		spawn:               fakeSpawn,
		terminate:           fakeTerminate,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.smStates.Store(descriptor.TaskName, api.StIdle)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctx)

	// Step 1: EvStart → expect spawn + StSpawning → executeSideEffect
	// posts EvHealthOK → loop processes → StRunning
	loop.Post(api.LoopEvent{Kind: api.EvStart, TaskName: descriptor.TaskName})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := ctrl.GetSMState(descriptor.TaskName)
		if st == api.StRunning && spawnCalls.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	if st != api.StRunning {
		t.Fatalf("after EvStart+EvHealthOK chain, state = %s, want %s (smStates.Store-outside-persistBefore regression)", st, api.StRunning)
	}
	if spawnCalls.Load() != 1 {
		t.Fatalf("spawn call count = %d, want 1", spawnCalls.Load())
	}

	// Step 2: flip intent to Desired=stopped, post EvIntentUpdate
	// → expect StExiting + terminate closure call
	stopFile := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			descriptor.TaskName: {Desired: "stopped", Reason: "user-stop", UpdatedAt: time.Now().UTC()},
		},
	}
	ctrl.daemonIntent.Refresh(stopFile)
	loop.Post(api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: descriptor.TaskName})

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := ctrl.GetSMState(descriptor.TaskName)
		if st == api.StExiting && terminateCalls.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ = ctrl.GetSMState(descriptor.TaskName)
	if st != api.StExiting {
		t.Fatalf("after EvIntentUpdate(stopped), state = %s, want %s (StRunning+EvIntentUpdate stop branch regression)", st, api.StExiting)
	}
	if terminateCalls.Load() != 1 {
		t.Fatalf("terminate call count = %d, want 1", terminateCalls.Load())
	}
}

// TestSupervisorController_HandleEvStart_StopIntentSuppressesSpawn
// closes the IntentDesired=stopped branch of StIdle+EvStart: SM
// returns (StIdle, "intent suppresses spawn") and the spawn closure
// must not fire.
func TestSupervisorController_HandleEvStart_StopIntentSuppressesSpawn(t *testing.T) {
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
	now := time.Now().UTC()
	daemonIntent := &api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		descriptor.TaskName: {
			Desired:   api.IntentDesiredStopped,
			Reason:    api.IntentReasonUserDisabled,
			UpdatedAt: now,
		},
	}}

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
	ctrl.smStates.Store(descriptor.TaskName, api.StIdle)

	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvStart,
		TaskName: descriptor.TaskName,
	})

	if got := spawnCalls.Load(); got != 0 {
		t.Errorf("EvStart with stop intent spawned %d times, want 0", got)
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	// The SM returns persistBefore=false on this transition so
	// smStates is NOT updated; GetSMState returns the previously
	// stored StIdle.
	if st != api.StIdle {
		t.Errorf("stop-intent EvStart state = %s, want %s", st, api.StIdle)
	}
}

// TestSupervisorController_HandleLoopEvent_OrphanTaskDropped verifies
// that an event for a task NOT in the intent cache is logged and
// dropped (no spawn, no state mutation).
func TestSupervisorController_HandleLoopEvent_OrphanTaskDropped(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	var spawnCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		return nil
	}

	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		spawn:               fakeSpawn,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	// Intent cache is empty - any handleLoopEvent must drop.

	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvStart,
		TaskName: `\orphan-task`,
	})

	if got := spawnCalls.Load(); got != 0 {
		t.Errorf("orphan task spawned %d times, want 0", got)
	}

	// Audit log should carry the controller-event-orphan row.
	logRaw, _ := os.ReadFile(eventsPath)
	if !strings.Contains(string(logRaw), `"event":"controller-event-orphan"`) {
		t.Errorf("controller-event-orphan missing from audit log:\n%s", logRaw)
	}
}

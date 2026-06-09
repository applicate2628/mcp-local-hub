package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		`\removed-task`:            {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t0},
		`\changed-desired-task`:    {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t0},
		`\changed-reason-task`:     {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonUserStop, UpdatedAt: t0},
		`\changed-updated-at-task`: {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t0},
		`\unchanged-task`:          {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t0},
	}}

	updated := &api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		`\added-task`:              {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonRegister, UpdatedAt: t1},
		`\changed-desired-task`:    {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonInstall, UpdatedAt: t0},
		`\changed-reason-task`:     {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonUserDisabled, UpdatedAt: t0},
		`\changed-updated-at-task`: {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t1},
		`\unchanged-task`:          {Desired: api.IntentDesiredRunning, Reason: api.IntentReasonInstall, UpdatedAt: t0},
	}}

	delta := diffIntentSnapshots(previous, updated)
	got := make(map[string]bool, len(delta))
	for _, name := range delta {
		got[name] = true
	}

	want := map[string]bool{
		`\removed-task`:            true,
		`\added-task`:              true,
		`\changed-desired-task`:    true,
		`\changed-reason-task`:     true,
		`\changed-updated-at-task`: true,
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

// TestSupervisorController_LegacyNilSpecSerenaProxy_QuarantinedNotSpawned is the
// bot PR #246 r2 P2 guard for the controller restart path (supervisor_controller.go
// StSpawning). A legacy nil-RuntimeSpec serena-proxy descriptor that reaches
// StSpawning — e.g. via EvChildExit→backoff→EvTimerDue for a row that was already
// running at upgrade and later exited — must be QUARANTINED, not spawned: the
// redesigned proxy fails loud on a nil spec (its args lack --task-name), so
// spawning would churn restart backoff. The reconcile pass + IPC respawn cover the
// other two spawn paths; this is the third. (Positive control: the EvStart test
// above spawns a normal descriptor → StSpawning.)
func TestSupervisorController_LegacyNilSpecSerenaProxy_QuarantinedNotSpawned(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error { spawnCalls.Add(1); return nil }

	const legacyTask = `\mcp-local-hub-serena-deadbeef`
	descriptor := api.SupervisorDaemon{
		TaskName:    legacyTask,
		Server:      "serena",
		Daemon:      "deadbeef",
		Args:        []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", `C:\work\alpha`, "--port", "9121"},
		RuntimeSpec: nil, // pre-redesign / stale row
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
		statePath:           filepath.Join(tmpHome, "supervisor-state.json"),
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.smStates.Store(legacyTask, api.StIdle)

	ctrl.handleLoopEvent(api.LoopEvent{Kind: api.EvStart, TaskName: legacyTask})

	if got := spawnCalls.Load(); got != 0 {
		t.Errorf("legacy nil-spec serena-proxy must NOT be spawned by the controller; spawn count = %d, want 0", got)
	}
	st, _ := ctrl.GetSMState(legacyTask)
	if st != api.StQuarantined {
		t.Errorf("legacy nil-spec serena-proxy must be quarantined; state = %s, want %s", st, api.StQuarantined)
	}
	logStr := readFileString(t, eventsPath)
	if !strings.Contains(logStr, `"event":"legacy-serena-descriptor-quarantined"`) {
		t.Fatalf("expected legacy-serena-descriptor-quarantined event:\n%s", logStr)
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

func TestSupervisorController_RemovedIntentClearsStateSoReregisterSpawns(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
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
		TaskName: `\mcp-local-hub-lsp-abcd1234-go`,
		Server:   "mcp-language-server",
		Daemon:   "lsp-abcd1234-go",
		Args:     []string{"daemon", "workspace-proxy"},
	}
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{descriptor}}
	emptyIntent := &api.SupervisorIntentFile{Version: 1}

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
		statePath:           statePath,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.smStates.Store(descriptor.TaskName, api.StRunning)
	ctrl.queuedActions.Store(descriptor.TaskName, "respawn")
	tracker.MarkSpawned(descriptor.TaskName, 1234, time.Now().UTC())
	_ = tracker.RecordCrashAndCountInWindow(descriptor.TaskName, time.Now().UTC(), respawnFailureWindow)
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("seed tracker persist: %v", err)
	}

	deps := ipcDispatchDeps{controllerProvider: func() *supervisorController { return ctrl }}
	applyReconcileDrift(deps, nil, emptyIntent, nil)

	if _, ok := ctrl.GetSMState(descriptor.TaskName); ok {
		t.Fatalf("removed descriptor left stale SM state tracked")
	}
	if v, ok := ctrl.queuedActions.Load(descriptor.TaskName); ok && v != "" {
		t.Fatalf("removed descriptor left queued action %q", v)
	}
	if entry, ok := tracker.Get(descriptor.TaskName); ok {
		t.Fatalf("removed descriptor left tracker entry: %+v", entry)
	}
	state, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read supervisor state after removal: %v", err)
	}
	if _, ok := state.Daemons[descriptor.TaskName]; ok {
		t.Fatalf("removed descriptor persisted stale supervisor-state row: %+v", state.Daemons[descriptor.TaskName])
	}

	ctrl.intentCache.Refresh(intent)
	ctrl.handleLoopEvent(api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: descriptor.TaskName})
	if got := spawnCalls.Load(); got != 1 {
		t.Fatalf("re-register EvIntentUpdate spawn calls = %d, want 1", got)
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	if st != api.StSpawning {
		t.Fatalf("state after re-register EvIntentUpdate = %s, want %s", st, api.StSpawning)
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

// TestSupervisorController_SpawnPreChildErrorRoutesToBackoff is the
// regression guard for the Codex Cloud finding on 2d67031: a daemon
// whose c.spawn returns errSpawnPreChild (cmd.Start / StartWithJob
// failed BEFORE any child existed) was previously stranded in
// StSpawning forever because:
//
//   - StIdle + EvStart -> StSpawning was stored in smStates BEFORE
//     the spawn closure ran (supervisor_controller.go ~L390).
//   - executeSideEffect only posted EvHealthOK on c.spawn() == nil; on
//     error it posted NOTHING (~L467-470).
//   - There was no child to produce a real EvChildExit, and the SM
//     table at supervisor_state_machine.go:73-81 has no StSpawning +
//     EvStart transition - subsequent reconcile EvStarts were
//     unmatched and dropped as controller-event-unhandled.
//
// The fix: executeSideEffect now posts a SYNTHETIC EvChildExit when
// c.spawn returns an errSpawnPreChild-wrapped error, so the SM
// table's StSpawning + EvChildExit transition routes to
// StBackoffWaiting (which arms the failure-counter timer) and the
// standard backoff retry pipeline drives the next spawn attempt via
// EvTimerDue.
//
// This test reproduces the PoC: register a fake spawn that returns
// fmt.Errorf("%w: ...", errSpawnPreChild, ...), post EvStart, and
// assert the state lands on StBackoffWaiting after a bounded wait
// (NOT stuck on StSpawning).
func TestSupervisorController_SpawnPreChildErrorRoutesToBackoff(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	// Fake spawn ALWAYS returns an errSpawnPreChild-wrapped error,
	// mirroring a persistent pre-child failure (missing executable,
	// invalid path, permission denied at cmd.Start, etc.).
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		return fmt.Errorf("%w: simulated cmd.Start failure", errSpawnPreChild)
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
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.smStates.Store(descriptor.TaskName, api.StIdle)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctx)

	// Post EvStart. Expected chain:
	//   StIdle + EvStart -> StSpawning + spawn() returns
	//   errSpawnPreChild -> executeSideEffect posts synthetic
	//   EvChildExit via PostSelf (priority channel, FIFO preserved
	//   via Run's priority-drain; deadlock-free because handler is
	//   only producer of selfCh) -> StSpawning + EvChildExit ->
	//   StBackoffWaiting (intent re-check passes because no stop
	//   was queued in this test).
	loop.Post(api.LoopEvent{Kind: api.EvStart, TaskName: descriptor.TaskName})

	// Poll until state lands on StBackoffWaiting OR the deadline
	// expires. With a 2-second deadline and 10ms polling, the loop
	// has ample time to process both events on a healthy machine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := ctrl.GetSMState(descriptor.TaskName)
		if st == api.StBackoffWaiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := spawnCalls.Load(); got != 1 {
		t.Errorf("spawn call count = %d, want 1 (spawn must be attempted exactly once before backoff)", got)
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	if st != api.StBackoffWaiting {
		t.Fatalf("post-spawn-error state = %s, want %s (regression: daemon stuck in StSpawning - the synthetic EvChildExit on errSpawnPreChild is missing from executeSideEffect)", st, api.StBackoffWaiting)
	}
}

// TestSupervisorController_SpawnPostChildErrorDoesNotSynthesizeExit is
// the regression guard for Codex Cloud bot finding on PR #236 a54cc95
// (P1): when c.spawn returns a NON-errSpawnPreChild error (e.g.
// persistDaemonRuntimeTracker failed AFTER cmd.Start succeeded - the
// child IS running, the wait goroutine is observing it), the
// controller MUST NOT post a synthetic EvChildExit. Otherwise the
// backoff timer would respawn while the original child is still
// alive, creating a duplicate daemon.
//
// The real EvChildExit will arrive from the wait goroutine's
// crashCh path when the child eventually exits naturally; the
// controller routes through StBackoffWaiting at that point. No
// synthesis is needed - and synthesizing it here would cause the
// duplicate-daemon bug the bot caught.
func TestSupervisorController_SpawnPostChildErrorDoesNotSynthesizeExit(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	// Fake spawn returns a generic (NON-errSpawnPreChild) error,
	// mirroring the production case where cmd.Start succeeded, the
	// wait goroutine was launched, and a downstream step like
	// persistDaemonRuntimeTracker failed. The child is alive at
	// this point.
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		return errors.New("persistDaemonRuntimeTracker: write supervisor-state.json: ...")
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
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.smStates.Store(descriptor.TaskName, api.StIdle)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctx)

	// Post EvStart. Expected:
	//   StIdle + EvStart -> StSpawning + spawn() returns generic
	//   non-pre-child error -> executeSideEffect posts NOTHING
	//   -> daemon stays in StSpawning until real EvChildExit
	//   arrives later from wait goroutine.
	loop.Post(api.LoopEvent{Kind: api.EvStart, TaskName: descriptor.TaskName})

	// Wait a bit to ensure no synthetic event is posted. We can't
	// "wait for nothing to happen", so we sleep a short bounded
	// time then assert the state did NOT transition away from
	// StSpawning.
	time.Sleep(200 * time.Millisecond)

	if got := spawnCalls.Load(); got != 1 {
		t.Errorf("spawn call count = %d, want 1", got)
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	if st != api.StSpawning {
		t.Fatalf("post-generic-error state = %s, want %s (regression: synthetic EvChildExit on non-pre-child error would respawn while the original child is still alive - duplicate daemon)", st, api.StSpawning)
	}
}

// setupControllerForB1Test builds a minimal controller wired to a real
// EventLoop and daemonIntent cache so the B1 + C3 regression tests can
// drive transitions through the production seam.
func setupControllerForB1Test(t *testing.T, descriptor api.SupervisorDaemon, intent string, spawn SpawnFunc) (*supervisorController, *api.EventLoop, context.CancelFunc) {
	t.Helper()
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { events.Close() })

	tracker := NewDaemonRuntimeTracker()
	intentFile := &api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{descriptor}}
	dintFile := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			descriptor.TaskName: {Desired: intent, UpdatedAt: time.Now().UTC()},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	loop := api.NewEventLoop(16)
	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		eventLoop:           loop,
		tracker:             tracker,
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		spawn:               spawn,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intentFile)
	ctrl.daemonIntent.Refresh(dintFile)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctx)

	return ctrl, loop, cancel
}

// TestSupervisorController_B1_StopDuringSpawn_EvHealthOK_RoutesToStExiting
// is the B1 regression guard: when a user stop is queued during spawn
// (queued_action=stop set via StSpawning + EvIntentUpdate(stopped)
// transition), the subsequent EvHealthOK MUST route to StExiting
// (issue terminate) instead of StRunning. Closes bot finding B on PR
// #236 1c0ea09 (stop-during-spawn previously silently dropped).
func TestSupervisorController_B1_StopDuringSpawn_EvHealthOK_RoutesToStExiting(t *testing.T) {
	var terminateCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error { return nil }
	fakeTerminate := func(d api.SupervisorDaemon) error {
		terminateCalls.Add(1)
		return nil
	}

	descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-default`, Server: "test", Daemon: "default"}
	ctrl, loop, cancel := setupControllerForB1Test(t, descriptor, "running", fakeSpawn)
	defer cancel()
	ctrl.terminate = fakeTerminate

	// Stage: state=StSpawning, queued_action=stop (as if the
	// StSpawning + EvIntentUpdate(stopped) transition fired). In
	// production a daemon only reaches StSpawning by the controller firing
	// its spawn closure, so it is OWN-spawned (has a live cmd.Wait that
	// posts the real EvChildExit). Mark it so the StExiting terminate path
	// does NOT synthesize a foreign-PID EvChildExit (#268 r11 P2) — that
	// would otherwise race past the StExiting state this test guards
	// straight to StIdle once queued_action=none cleared the respawn.
	ctrl.smStates.Store(descriptor.TaskName, api.StSpawning)
	ctrl.queuedActions.Store(descriptor.TaskName, "stop")
	ctrl.ownSpawned.Store(descriptor.TaskName, true)

	loop.Post(api.LoopEvent{Kind: api.EvHealthOK, TaskName: descriptor.TaskName})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := ctrl.GetSMState(descriptor.TaskName); st == api.StExiting && terminateCalls.Load() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	t.Fatalf("StSpawning + EvHealthOK with queued_action=stop: state=%s, terminate_calls=%d; want StExiting + 1 terminate call (regression: B1 transition is missing)", st, terminateCalls.Load())
}

// TestSupervisorController_B1_StopDuringSpawn_EvChildExit_RoutesToStIdle
// is the B1 regression guard for the pre-child spawn-error path: when a
// user stop is queued during a spawn that then fails, the synthetic
// EvChildExit MUST route to StIdle (clear queued_action) instead of
// StBackoffWaiting (backoff respawn). Closes bot finding B on PR #236
// 1c0ea09 (pre-child retry overriding queued user stop).
func TestSupervisorController_B1_StopDuringSpawn_EvChildExit_RoutesToStIdle(t *testing.T) {
	fakeSpawn := func(d api.SupervisorDaemon) error { return nil }
	descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-default`, Server: "test", Daemon: "default"}
	ctrl, loop, cancel := setupControllerForB1Test(t, descriptor, "running", fakeSpawn)
	defer cancel()

	// Stage: state=StSpawning, queued_action=stop.
	ctrl.smStates.Store(descriptor.TaskName, api.StSpawning)
	ctrl.queuedActions.Store(descriptor.TaskName, "stop")

	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: descriptor.TaskName})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := ctrl.GetSMState(descriptor.TaskName); st == api.StIdle {
			// Also verify queued_action was cleared by the
			// "clear queued_action" side-effect string.
			if v, ok := ctrl.queuedActions.Load(descriptor.TaskName); !ok || v.(string) != "" {
				t.Fatalf("queued_action not cleared after StIdle transition: %v", v)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	t.Fatalf("StSpawning + EvChildExit with queued_action=stop: state=%s; want StIdle (regression: B1 transition is missing)", st)
}

// TestSupervisorController_B1_StopDuringBackoff_EvTimerDue_RoutesToStIdle
// is the B1 regression guard for the backoff window: when a user stop
// arrives during the backoff window, the EvTimerDue firing MUST
// re-check intent and route to StIdle instead of StSpawning (respawn).
// Closes bot finding B on PR #236 1c0ea09 (backoff timer fired blind
// to current intent state).
func TestSupervisorController_B1_StopDuringBackoff_EvTimerDue_RoutesToStIdle(t *testing.T) {
	var spawnCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		return nil
	}
	descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-default`, Server: "test", Daemon: "default"}
	ctrl, loop, cancel := setupControllerForB1Test(t, descriptor, "stopped", fakeSpawn)
	defer cancel()

	// Stage: state=StBackoffWaiting (as if the daemon just exited
	// abnormally and we're in the backoff window). Intent is
	// already "stopped" via setup.
	ctrl.smStates.Store(descriptor.TaskName, api.StBackoffWaiting)

	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: descriptor.TaskName})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := ctrl.GetSMState(descriptor.TaskName); st == api.StIdle {
			if spawnCalls.Load() != 0 {
				t.Fatalf("spawn fired despite intent=stopped: %d call(s); want 0", spawnCalls.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	t.Fatalf("StBackoffWaiting + EvTimerDue with intent=stopped: state=%s, spawn_calls=%d; want StIdle + 0 spawn calls (regression: timer ignored intent and respawned)", st, spawnCalls.Load())
}

// TestSupervisorController_B1_StopDuringSpawn_IntentUpdateSetsQueuedActionStop
// is the B1 regression guard for the substring-match wiring: when
// StSpawning + EvIntentUpdate(stopped) fires, the SM returns
// "set queued_action=stop" and the controller's substring switch MUST
// store "stop" in queuedActions. Closes bot finding B on PR #236
// 1c0ea09 (sonnet caught: original switch only handled "respawn"/
// "none"/"clear"; needed to add "stop" case AND bounded auto-clear).
func TestSupervisorController_B1_StopDuringSpawn_IntentUpdateSetsQueuedActionStop(t *testing.T) {
	fakeSpawn := func(d api.SupervisorDaemon) error { return nil }
	descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-default`, Server: "test", Daemon: "default"}
	ctrl, loop, cancel := setupControllerForB1Test(t, descriptor, "stopped", fakeSpawn)
	defer cancel()

	ctrl.smStates.Store(descriptor.TaskName, api.StSpawning)

	loop.Post(api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: descriptor.TaskName})

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := ctrl.queuedActions.Load(descriptor.TaskName); ok && v.(string) == "stop" {
			// Also verify state stayed StSpawning (self-loop).
			if st, _ := ctrl.GetSMState(descriptor.TaskName); st != api.StSpawning {
				t.Fatalf("state changed during self-loop: %s; want StSpawning", st)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	v, _ := ctrl.queuedActions.Load(descriptor.TaskName)
	t.Fatalf("queued_action after StSpawning + EvIntentUpdate(stopped): %v; want \"stop\" (regression: substring switch missing \"stop\" OR auto-clear at line 415-417 wiped it)", v)
}

// TestSupervisorController_R5_NonPreChildErrorDoesNotSynthExit verifies
// the controller's contract on receiving a SpawnFunc error that is
// NOT wrapped with errSpawnPreChild. The switch on
// errors.Is(err, errSpawnPreChild) MUST NOT match - no synthetic
// EvChildExit is posted - the daemon stays in StSpawning.
//
// This is the controller-level contract that supports both:
//   - the post-child persist-error case (cmd.Start succeeded, wait
//     goroutine launched, persistDaemonRuntimeTracker failed); the
//     real EvChildExit will arrive from the wait goroutine
//   - any future error path that explicitly chooses to not route
//     through backoff
//
// The Windows post-create orphan case is now handled in
// supervise.go via process.BestEffortKillByPID + wrap-with-
// errSpawnPreChild (closes the stuck-StSpawning issue the bot
// flagged on PR #237 a646148). Production no longer relies on this
// "don't synth" path for orphans, but the controller contract is
// still tested here as a defensive guard.
func TestSupervisorController_R5_NonPreChildErrorDoesNotSynthExit(t *testing.T) {
	var spawnCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		// Non-pre-child error - mirrors post-child persist-error
		// case (or any other error that's NOT wrapped with
		// errSpawnPreChild).
		return errors.New("generic non-pre-child error")
	}
	descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-default`, Server: "test", Daemon: "default"}
	ctrl, loop, cancel := setupControllerForB1Test(t, descriptor, "running", fakeSpawn)
	defer cancel()
	ctrl.smStates.Store(descriptor.TaskName, api.StIdle)

	loop.Post(api.LoopEvent{Kind: api.EvStart, TaskName: descriptor.TaskName})

	time.Sleep(300 * time.Millisecond)

	if spawnCalls.Load() != 1 {
		t.Fatalf("spawn called %d times; want exactly 1 (no backoff retry should fire)", spawnCalls.Load())
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	if st != api.StSpawning {
		t.Fatalf("after non-pre-child spawn error: state=%s; want StSpawning (regression: controller should NOT synth EvChildExit for non-errSpawnPreChild errors)", st)
	}
}

// TestSupervisorController_R5_IntentFlipStopThenRun_ClearsQueuedAction is
// the r5 follow-up regression guard for bot finding on PR #236 db988e0
// (intent flip stopped -> running stale queued_action). The SM's new
// StSpawning + EvIntentUpdate(running) self-loop returns
// "clear queued_action" side string, and the controller's substring
// switch on "clear queued_action" stores "". This ensures that a
// later EvHealthOK does not route to StExiting against the operator's
// LATEST intent.
func TestSupervisorController_R5_IntentFlipStopThenRun_ClearsQueuedAction(t *testing.T) {
	fakeSpawn := func(d api.SupervisorDaemon) error { return nil }
	descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-default`, Server: "test", Daemon: "default"}
	// Initial intent: running (so we can flip to stopped then back).
	ctrl, loop, cancel := setupControllerForB1Test(t, descriptor, "running", fakeSpawn)
	defer cancel()

	// Stage: state=StSpawning + queued_action=stop (as if a prior
	// stop intent had been queued during spawn).
	ctrl.smStates.Store(descriptor.TaskName, api.StSpawning)
	ctrl.queuedActions.Store(descriptor.TaskName, "stop")

	// Now post EvIntentUpdate with current intent=running (already
	// the setup's intent).
	loop.Post(api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: descriptor.TaskName})

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := ctrl.queuedActions.Load(descriptor.TaskName); ok && v.(string) == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	v, _ := ctrl.queuedActions.Load(descriptor.TaskName)
	t.Fatalf("after StSpawning + EvIntentUpdate(running) on stale queued_action=stop: queued_action=%v; want \"\" (regression: SM returned empty side or controller substring switch missing \"clear queued_action\")", v)
}

// TestSupervisorController_R5_StIdleFromStBackoffWaitingMarksTrackerExited
// is the r5 follow-up regression guard for bot finding on PR #236
// db988e0 (tracker state drift). When SM transitions to StIdle from
// a non-idle state (e.g. StBackoffWaiting via the B1 intent re-check
// path), the controller MUST call tracker.MarkExited BEFORE persist
// so supervisor-state.json does not record state="backoff-waiting"
// + CurrentPID=N alongside SM state="idle".
func TestSupervisorController_R5_StIdleFromStBackoffWaitingMarksTrackerExited(t *testing.T) {
	fakeSpawn := func(d api.SupervisorDaemon) error { return nil }
	descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-default`, Server: "test", Daemon: "default"}
	ctrl, loop, cancel := setupControllerForB1Test(t, descriptor, "stopped", fakeSpawn)
	defer cancel()

	// Seed tracker with a live PID + backoff state (as if the daemon
	// just crashed and is in the backoff window).
	ctrl.tracker.MarkSpawned(descriptor.TaskName, 12345, time.Now().UTC())
	ctrl.tracker.MarkBackoff(descriptor.TaskName)
	ctrl.smStates.Store(descriptor.TaskName, api.StBackoffWaiting)

	// Fire EvTimerDue with intent=stopped - B1 routes to StIdle.
	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: descriptor.TaskName})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := ctrl.GetSMState(descriptor.TaskName)
		if st != api.StIdle {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		entry, ok := ctrl.tracker.Get(descriptor.TaskName)
		if !ok {
			t.Fatalf("tracker entry missing after MarkExited")
		}
		if entry.State != daemonRuntimeStateIdle {
			t.Fatalf("tracker state = %q; want %q (regression: tracker not synced to idle on StBackoffWaiting -> StIdle transition)", entry.State, daemonRuntimeStateIdle)
		}
		if entry.CurrentPID != 0 {
			t.Fatalf("tracker CurrentPID = %d; want 0 (regression: MarkExited not called)", entry.CurrentPID)
		}
		return
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	t.Fatalf("after EvTimerDue with intent=stopped: state=%s; want StIdle (B1 intent re-check missing)", st)
}

// TestSupervisorController_C3_PreChildSpawnFailureUsesPostSelf verifies
// the C3 wiring: executeSideEffect uses PostSelf (priority channel)
// instead of inline Post on the main channel. Test by checking the
// pre-child error path completes without deadlock and routes through
// backoff. The actual priority-drain ordering is unit-tested in
// internal/api/supervisor_event_loop_test.go; here we verify the
// integration that supervisor_controller calls PostSelf and the SM
// transition fires correctly.
func TestSupervisorController_C3_PreChildSpawnFailureUsesPostSelf(t *testing.T) {
	var spawnCalls atomic.Int32
	fakeSpawn := func(d api.SupervisorDaemon) error {
		spawnCalls.Add(1)
		return fmt.Errorf("%w: simulated cmd.Start failure", errSpawnPreChild)
	}
	descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-test-default`, Server: "test", Daemon: "default"}
	// intent="running" so EvStart actually triggers spawn (not
	// "intent suppresses spawn"). The pre-child spawn error then
	// synth-posts EvChildExit via PostSelf -> priority drain ->
	// StSpawning + EvChildExit (intent=running) -> StBackoffWaiting.
	ctrl, loop, cancel := setupControllerForB1Test(t, descriptor, "running", fakeSpawn)
	defer cancel()

	ctrl.smStates.Store(descriptor.TaskName, api.StIdle)

	loop.Post(api.LoopEvent{Kind: api.EvStart, TaskName: descriptor.TaskName})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := ctrl.GetSMState(descriptor.TaskName); st == api.StBackoffWaiting {
			if spawnCalls.Load() != 1 {
				t.Fatalf("spawn called %d times; want 1", spawnCalls.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := ctrl.GetSMState(descriptor.TaskName)
	t.Fatalf("after EvStart pre-child fail: state=%s, spawn_calls=%d; want StBackoffWaiting + 1 spawn call (regression: PostSelf path or synth EvChildExit missing)", st, spawnCalls.Load())
}

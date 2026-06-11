package cli

// Spec §4 Phase A.1 (STOP supervisor-aware) — classifier + handleReconcile
// coverage for the terminate direction on supervisor-owned descriptors.
// Sibling of supervise_reconcile_ipc_test.go; uses the same fixture.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// TestClassifyDriftAction_TerminateDirectionSupervisorOwned covers the
// classifier matrix under the no-legacy ownership model (spec §0.2): EVERY
// supervisor-intent row is supervisor-owned, so the supervisorOwned
// dimension is gone from the signature. A sched=missing row is now always a
// supervisor-intent descriptor with no scheduler row by design:
//
//   - terminate direction (intent=stopped against a live SM) →
//     post_ev_intent_update; the scheduler can never witness these daemons
//     running, and without this row `mcphub stop` falls back to taskkill,
//     whose non-clean exit the reaper respawns (stop→respawn churn →
//     quarantine). Dead/settled SM states stay no_op.
//   - spawn direction (intent=running) → post_ev_intent_update (the
//     supervisor spawns directly from supervisor-intent.json); the old
//     needs_manual_review "legacy scheduler-owned" row is dead.
//
// hasSched=true rows are unchanged (real scheduler rows still reconcile
// against scheduler state until Phase F removes them).
func TestClassifyDriftAction_TerminateDirectionSupervisorOwned(t *testing.T) {
	const (
		missing = api.ReconcileSchedulerStateMissing
		running = api.ReconcileIntentDesiredRunning
		stopped = api.ReconcileIntentDesiredStopped
	)
	cases := []struct {
		name          string
		schedState    string
		hasSched      bool
		intentDesired string
		smState       api.SMState
		want          string
	}{
		// Terminate direction: stopped intent + live SM (no scheduler row).
		{"stopped spawning", missing, false, stopped, api.StSpawning, api.ReconcileActionPostEvIntentUpdate},
		{"stopped running", missing, false, stopped, api.StRunning, api.ReconcileActionPostEvIntentUpdate},
		{"stopped exiting", missing, false, stopped, api.StExiting, api.ReconcileActionPostEvIntentUpdate},
		{"stopped backoff", missing, false, stopped, api.StBackoffWaiting, api.ReconcileActionPostEvIntentUpdate},
		// Settled SM states: nothing live to terminate → no_op.
		{"stopped idle", missing, false, stopped, api.StIdle, api.ReconcileActionNoOp},
		{"stopped quarantined", missing, false, stopped, api.StQuarantined, api.ReconcileActionNoOp},
		// Spawn direction: running intent + missing scheduler row → spawn
		// directly from intent (the dead legacy needs_manual_review row).
		{"running missing idle", missing, false, running, api.StIdle, api.ReconcileActionPostEvIntentUpdate},
		// Spawn direction across in-flight SM states → still spawn (these are
		// NOT settled, so a running-intent reconcile drives them). StRunning is
		// already at the desired state and must be no_op.
		{"running spawning", missing, false, running, api.StSpawning, api.ReconcileActionPostEvIntentUpdate},
		{"running running", missing, false, running, api.StRunning, api.ReconcileActionNoOp},
		{"running exiting", missing, false, running, api.StExiting, api.ReconcileActionPostEvIntentUpdate},
		{"running backoff", missing, false, running, api.StBackoffWaiting, api.ReconcileActionNoOp},
		// Spawn-direction quarantine-respect: a quarantined bystander whose
		// intent is running must NOT be revived (the SM row
		// StQuarantined+EvIntentUpdate(running) resets the failure window).
		// needs_manual_review surfaces it as drift the operator must force/reset
		// rather than pretending steady-state — the fix for fleet-wide bystander
		// revival on every stop/restart (#279).
		{"running quarantined", missing, false, running, api.StQuarantined, api.ReconcileActionNeedsManualReview},
		// hasSched rows unchanged.
		{"sched running intent stopped", api.ReconcileSchedulerStateRunning, true, stopped, api.StIdle, api.ReconcileActionPostEvIntentUpdate},
		{"sched stopped intent running", api.ReconcileSchedulerStateStopped, true, running, api.StIdle, api.ReconcileActionPostEvIntentUpdate},
		{"sched stopped intent running backoff", api.ReconcileSchedulerStateStopped, true, running, api.StBackoffWaiting, api.ReconcileActionNoOp},
		{"sched stopped intent stopped", api.ReconcileSchedulerStateStopped, true, stopped, api.StRunning, api.ReconcileActionNoOp},
		// Spawn-direction quarantine-respect ALSO holds on the hasSched
		// scheduler-stopped arm: a daemon that quarantined while still carrying
		// a stale/residual scheduler-stopped row must NOT revive on every
		// reconcile (#279 fable r5 F-1 — same defect class as the !hasSched
		// quarantine gate, all-return-paths).
		{"sched stopped intent running quarantined", api.ReconcileSchedulerStateStopped, true, running, api.StQuarantined, api.ReconcileActionNeedsManualReview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDriftAction(tc.schedState, tc.hasSched, tc.intentDesired, tc.smState)
			if got != tc.want {
				t.Fatalf("classifyDriftAction(%q, %v, %q, sm=%q) = %q, want %q",
					tc.schedState, tc.hasSched, tc.intentDesired, tc.smState, got, tc.want)
			}
		})
	}
}

// seedStoppedDaemonIntentForReconcileTest records a Desired=stopped stop for
// taskName the way `mcphub stop` does. Phase 4-E2: the stop lives in the
// supervisor-intent.json `stops` sub-block (the sole stop source — the legacy
// daemon-intent.json the apply-mode reconcile used to read is deleted), so this
// delegates to the sub-block seeder. The name is retained so the many callers
// stay unchanged.
func seedStoppedDaemonIntentForReconcileTest(t *testing.T, stateDir, taskName string) {
	t.Helper()
	seedStopsSubBlockOnSupervisorIntent(t, stateDir, taskName)
}

// supervisorOwnedTimeIntentForReconcileTest builds a one-daemon intent
// whose descriptor carries a proxy-shaped argv. Under the no-legacy
// ownership model (spec §0.2) every supervisor-intent row is
// supervisor-owned regardless of argv, so the classifier treats it the
// same as a regular daemon — the proxy shape here is just incidental.
func supervisorOwnedTimeIntentForReconcileTest(taskName string) *api.SupervisorIntentFile {
	return &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName: taskName,
				Server:   "time",
				Daemon:   "default",
				Command:  "mcphub",
				Args:     []string{"daemon", "serena-proxy"},
				Port:     9242,
			},
		},
	}
}

// TestReconcileIPC_SupervisorOwnedStoppedIntentTerminatesLiveDaemon is the
// handleReconcile-level proof of the spec §4 Phase A.1 stop propagation:
// a supervisor-owned descriptor (no scheduler row), a Desired=stopped
// daemon-intent entry, and a live controller SM state must produce one
// post_ev_intent_update drift entry AND, in apply mode, one EvIntentUpdate
// post onto the controller's event loop — the event whose SM transition
// (StRunning→StExiting) stops the daemon without a respawnable non-clean
// exit.
func TestReconcileIPC_SupervisorOwnedStoppedIntentTerminatesLiveDaemon(t *testing.T) {
	taskName := `\mcp-local-hub-time-default`
	fx := newReconcileTestFixture(t, supervisorOwnedTimeIntentForReconcileTest(taskName))
	seedStoppedDaemonIntentForReconcileTest(t, fx.deps.stateDir, taskName)

	// Live SM state for the task; no scheduler rows (supervisor-owned
	// descriptors never have one).
	fx.ctrl.smStates.Store(taskName, api.StRunning)
	installSchedulerListFake(t, nil)

	req := api.IPCRequest{
		ID:   42,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 1 || len(body.Drift) != 1 {
		t.Fatalf("DriftCount=%d drift=%+v, want one terminate-direction row", body.DriftCount, body.Drift)
	}
	entry := body.Drift[0]
	if entry.Action != api.ReconcileActionPostEvIntentUpdate {
		t.Errorf("Action = %q, want %q (live supervisor-owned daemon + stopped intent)",
			entry.Action, api.ReconcileActionPostEvIntentUpdate)
	}
	if entry.IntentDesired != api.ReconcileIntentDesiredStopped {
		t.Errorf("IntentDesired = %q, want %q", entry.IntentDesired, api.ReconcileIntentDesiredStopped)
	}
	if entry.SMState != api.StRunning {
		t.Errorf("SMState = %q, want %q", entry.SMState, api.StRunning)
	}
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1", body.AppliedCount)
	}
	select {
	case ev := <-fx.postedCh:
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != taskName {
			t.Fatalf("posted event = %+v, want EvIntentUpdate for %s", ev, taskName)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("expected EvIntentUpdate post for the terminate direction")
	}
}

// TestReconcileIPC_SupervisorOwnedStoppedIntentQuarantinedStaysNoOp is the
// settled-SM counterpart: a quarantined daemon with stopped intent must
// NOT be classified as drift (quarantine-respect, same as the startup
// reconciler's gate) and apply mode must post nothing.
func TestReconcileIPC_SupervisorOwnedStoppedIntentQuarantinedStaysNoOp(t *testing.T) {
	taskName := `\mcp-local-hub-time-default`
	fx := newReconcileTestFixture(t, supervisorOwnedTimeIntentForReconcileTest(taskName))
	seedStoppedDaemonIntentForReconcileTest(t, fx.deps.stateDir, taskName)

	fx.ctrl.smStates.Store(taskName, api.StQuarantined)
	installSchedulerListFake(t, nil)

	req := api.IPCRequest{
		ID:   43,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.AppliedCount != 0 {
		t.Fatalf("AppliedCount = %d, want 0 (quarantined daemon must not be poked)", body.AppliedCount)
	}
	if len(body.Drift) != 1 || body.Drift[0].Action != api.ReconcileActionNoOp {
		t.Fatalf("drift = %+v, want one no_op row", body.Drift)
	}
	select {
	case ev := <-fx.postedCh:
		t.Fatalf("unexpected event posted for quarantined daemon: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// twoRowIntentTargetPlusQuarantinedBystander builds a two-daemon intent: a
// stop/restart TARGET (the daemon the operator acted on) plus a BYSTANDER that
// is genuinely quarantined. Both are supervisor-owned with no scheduler row.
func twoRowIntentTargetPlusQuarantinedBystander(target, bystander string) *api.SupervisorIntentFile {
	return &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: target, Server: "target", Daemon: "default", Command: "mcphub",
				Args: []string{"daemon", "--server", "target", "--daemon", "default"}, Port: 9242},
			{TaskName: bystander, Server: "bystander", Daemon: "default", Command: "mcphub",
				Args: []string{"daemon", "--server", "bystander", "--daemon", "default"}, Port: 9243},
		},
	}
}

// TestReconcileIPC_QuarantinedBystanderNotRevivedOnStop is the
// handleReconcile-level proof of the #279 spawn-direction quarantine-respect
// fix. `mcphub stop` of the TARGET fires `reconcile --apply`, which walks ALL
// supervisor-intent rows. The target (stopped intent + live SM) must terminate
// via one EvIntentUpdate; the quarantined BYSTANDER (no daemon-intent override
// → computeIntentDesired defaults running, StQuarantined SM) must NOT be poked
// — its drift entry carries needs_manual_review and apply mode posts NOTHING
// for it. Net: exactly ONE EvIntentUpdate (the target), AppliedCount=1. Before
// the fix the bystander classified post_ev_intent_update and the SM row
// StQuarantined+EvIntentUpdate(running) reset its failure window, reviving it.
func TestReconcileIPC_QuarantinedBystanderNotRevivedOnStop(t *testing.T) {
	target := `\mcp-local-hub-target-default`
	bystander := `\mcp-local-hub-bystander-default`
	fx := newReconcileTestFixture(t, twoRowIntentTargetPlusQuarantinedBystander(target, bystander))
	// Operator stopped the TARGET only: daemon-intent marks the target
	// stopped; the bystander has no override (defaults running).
	seedStoppedDaemonIntentForReconcileTest(t, fx.deps.stateDir, target)

	// Live target SM (terminate-eligible); quarantined bystander SM.
	fx.ctrl.smStates.Store(target, api.StRunning)
	fx.ctrl.smStates.Store(bystander, api.StQuarantined)
	installSchedulerListFake(t, nil)

	req := api.IPCRequest{ID: 77, Cmd: "reconcile", Args: map[string]any{"apply": true}}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 2 || len(body.Drift) != 2 {
		t.Fatalf("DriftCount=%d drift=%+v, want two rows (target + bystander)", body.DriftCount, body.Drift)
	}

	driftByTask := map[string]api.DriftEntry{}
	for _, e := range body.Drift {
		driftByTask[e.TaskName] = e
	}
	tgt, ok := driftByTask[target]
	if !ok {
		t.Fatalf("no drift entry for target %s; drift=%+v", target, body.Drift)
	}
	if tgt.Action != api.ReconcileActionPostEvIntentUpdate {
		t.Errorf("target Action = %q, want %q (stopped intent + live SM → terminate)",
			tgt.Action, api.ReconcileActionPostEvIntentUpdate)
	}
	bys, ok := driftByTask[bystander]
	if !ok {
		t.Fatalf("no drift entry for bystander %s; drift=%+v", bystander, body.Drift)
	}
	if bys.Action != api.ReconcileActionNeedsManualReview {
		t.Errorf("bystander Action = %q, want %q (quarantined + running intent must not be revived)",
			bys.Action, api.ReconcileActionNeedsManualReview)
	}
	if bys.SMState != api.StQuarantined {
		t.Errorf("bystander SMState = %q, want %q", bys.SMState, api.StQuarantined)
	}
	if bys.IntentDesired != api.ReconcileIntentDesiredRunning {
		t.Errorf("bystander IntentDesired = %q, want %q (absent override defaults running)",
			bys.IntentDesired, api.ReconcileIntentDesiredRunning)
	}

	// Exactly ONE EvIntentUpdate posted — the target.
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1 (only the target is dispatched; bystander must not be poked)", body.AppliedCount)
	}
	select {
	case ev := <-fx.postedCh:
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != target {
			t.Fatalf("posted event = %+v, want EvIntentUpdate for the target %s", ev, target)
		}
	case <-time.After(time.Second):
		t.Fatal("expected EvIntentUpdate post for the target terminate direction")
	}
	// No second event for the bystander.
	select {
	case ev := <-fx.postedCh:
		t.Fatalf("unexpected second event posted (bystander must not be revived): %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReconcileIPC_BackoffBystanderNotRespawnedOnApply(t *testing.T) {
	idle := `\mcp-local-hub-idle-default`
	backoff := `\mcp-local-hub-backoff-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: idle, Server: "idle", Daemon: "default", Command: "mcphub",
				Args: []string{"daemon", "--server", "idle", "--daemon", "default"}, Port: 9242},
			{TaskName: backoff, Server: "backoff", Daemon: "default", Command: "mcphub",
				Args: []string{"daemon", "--server", "backoff", "--daemon", "default"}, Port: 9243},
		},
	}
	fx := newReconcileTestFixture(t, intent)
	fx.ctrl.smStates.Store(idle, api.StIdle)
	fx.ctrl.smStates.Store(backoff, api.StBackoffWaiting)
	installSchedulerListFake(t, nil)

	req := api.IPCRequest{ID: 78, Cmd: "reconcile", Args: map[string]any{"apply": true}}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 2 || len(body.Drift) != 2 {
		t.Fatalf("DriftCount=%d drift=%+v, want two rows (idle + backoff)", body.DriftCount, body.Drift)
	}

	driftByTask := map[string]api.DriftEntry{}
	for _, e := range body.Drift {
		driftByTask[e.TaskName] = e
	}
	if got := driftByTask[idle].Action; got != api.ReconcileActionPostEvIntentUpdate {
		t.Fatalf("idle Action = %q, want %q (missing idle row still gets spawn intent)",
			got, api.ReconcileActionPostEvIntentUpdate)
	}
	if got := driftByTask[backoff].Action; got != api.ReconcileActionNoOp {
		t.Fatalf("backoff Action = %q, want %q (apply must not reset the SM-owned backoff timer)",
			got, api.ReconcileActionNoOp)
	}
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1 (only the idle row is dispatched)", body.AppliedCount)
	}
	select {
	case ev := <-fx.postedCh:
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != idle {
			t.Fatalf("posted event = %+v, want EvIntentUpdate for idle row %s", ev, idle)
		}
	case <-time.After(time.Second):
		t.Fatal("expected EvIntentUpdate post for the idle row")
	}
	select {
	case ev := <-fx.postedCh:
		t.Fatalf("unexpected second event posted (backoff timer must not be preempted): %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReconcileIPC_RunningBystanderNotRepostedOnApply(t *testing.T) {
	target := `\mcp-local-hub-target-default`
	bystander := `\mcp-local-hub-bystander-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: target, Server: "target", Daemon: "default", Command: "mcphub",
				Args: []string{"daemon", "--server", "target", "--daemon", "default"}, Port: 9244},
			{TaskName: bystander, Server: "bystander", Daemon: "default", Command: "mcphub",
				Args: []string{"daemon", "--server", "bystander", "--daemon", "default"}, Port: 9245},
		},
	}
	fx := newReconcileTestFixture(t, intent)
	fx.ctrl.smStates.Store(target, api.StIdle)
	fx.ctrl.smStates.Store(bystander, api.StRunning)
	installSchedulerListFake(t, nil)

	req := api.IPCRequest{ID: 79, Cmd: "reconcile", Args: map[string]any{"apply": true}}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 2 || len(body.Drift) != 2 {
		t.Fatalf("DriftCount=%d drift=%+v, want two rows (target + bystander)", body.DriftCount, body.Drift)
	}

	driftByTask := map[string]api.DriftEntry{}
	for _, e := range body.Drift {
		driftByTask[e.TaskName] = e
	}
	if got := driftByTask[target].Action; got != api.ReconcileActionPostEvIntentUpdate {
		t.Fatalf("target Action = %q, want %q (idle missing row still gets spawn intent)",
			got, api.ReconcileActionPostEvIntentUpdate)
	}
	if got := driftByTask[bystander].Action; got != api.ReconcileActionNoOp {
		t.Fatalf("bystander Action = %q, want %q (running bystander is already at desired=running)",
			got, api.ReconcileActionNoOp)
	}
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1 (only the idle target is dispatched)", body.AppliedCount)
	}
	select {
	case ev := <-fx.postedCh:
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != target {
			t.Fatalf("posted event = %+v, want EvIntentUpdate for target row %s", ev, target)
		}
	case <-time.After(time.Second):
		t.Fatal("expected EvIntentUpdate post for the idle target")
	}
	select {
	case ev := <-fx.postedCh:
		t.Fatalf("unexpected second event posted (running bystander must not receive EvIntentUpdate): %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// seedStopsSubBlockOnSupervisorIntent rewrites supervisor-intent.json's stops
// sub-block (the Phase 4-E1 unified stop home) so a reconcile with NO
// daemon-intent.json on disk still reads the stop. It preserves the existing
// daemons[] rows (the merge touches only Stops).
func seedStopsSubBlockOnSupervisorIntent(t *testing.T, stateDir, taskName string) {
	t.Helper()
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	got, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("read supervisor-intent.json: %v", err)
	}
	if got.Stops == nil {
		got.Stops = map[string]api.DaemonIntent{}
	}
	got.Stops[taskName] = api.DaemonIntent{
		Desired:   api.IntentDesiredStopped,
		Reason:    api.IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}
	if err := api.WriteSupervisorIntent(intentPath, got); err != nil {
		t.Fatalf("write supervisor-intent.json stops sub-block: %v", err)
	}
}

// TestReconcileIPC_ReadsStopFromSupervisorIntentSubBlock is the Phase 4-E1
// reader-repoint proof for reader #2 (supervise_reconcile.go IsActiveStop) on
// the apply-mode IPC path: with NO daemon-intent.json on disk, a stop recorded
// ONLY in supervisor-intent.json's stops sub-block must still terminate a live
// supervisor-owned daemon. Before E1 the reconcile read daemon-intent.json
// only, so a sub-block-only stop would be invisible (the daemon would not be
// stopped). This asserts the new canonical path is wired.
func TestReconcileIPC_ReadsStopFromSupervisorIntentSubBlock(t *testing.T) {
	taskName := `\mcp-local-hub-time-default`
	fx := newReconcileTestFixture(t, supervisorOwnedTimeIntentForReconcileTest(taskName))
	// Deliberately DO NOT seed daemon-intent.json — the stop lives only in the
	// supervisor-intent stops sub-block (the E1 recovery-baseline / new home).
	if _, err := os.Stat(filepath.Join(fx.deps.stateDir, "daemon-intent.json")); err == nil {
		t.Fatalf("test precondition: daemon-intent.json must be absent")
	}
	seedStopsSubBlockOnSupervisorIntent(t, fx.deps.stateDir, taskName)

	fx.ctrl.smStates.Store(taskName, api.StRunning)
	installSchedulerListFake(t, nil)

	req := api.IPCRequest{ID: 142, Cmd: "reconcile", Args: map[string]any{"apply": true}}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if len(body.Drift) != 1 {
		t.Fatalf("drift = %+v, want exactly one row", body.Drift)
	}
	entry := body.Drift[0]
	if entry.IntentDesired != api.ReconcileIntentDesiredStopped {
		t.Fatalf("IntentDesired = %q, want stopped (read from supervisor-intent stops sub-block)", entry.IntentDesired)
	}
	if entry.Action != api.ReconcileActionPostEvIntentUpdate {
		t.Fatalf("Action = %q, want post_ev_intent_update (stop from sub-block must terminate live daemon)", entry.Action)
	}
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1", body.AppliedCount)
	}
	select {
	case ev := <-fx.postedCh:
		if ev.Kind != api.EvIntentUpdate || ev.TaskName != taskName {
			t.Fatalf("posted event = %+v, want EvIntentUpdate for %s", ev, taskName)
		}
	case <-time.After(time.Second):
		t.Fatal("expected EvIntentUpdate post for the sub-block-sourced stop")
	}
}

// regularGlobalDaemonIntentForReconcileTest builds a one-daemon intent whose
// descriptor is a REGULAR global daemon (`daemon --server foo --daemon
// default`) — the shape memory / paper-search / time daemons carry. Under
// the no-legacy ownership model (spec §0.2) this row is supervisor-owned
// exactly like a proxy descriptor, so the reconcile classifier DOES post
// EvIntentUpdate for it on both directions.
func regularGlobalDaemonIntentForReconcileTest(taskName string) *api.SupervisorIntentFile {
	return &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName: taskName,
				Server:   "foo",
				Daemon:   "default",
				Command:  "mcphub",
				Args:     []string{"daemon", "--server", "foo", "--daemon", "default"},
				Port:     9242,
			},
		},
	}
}

// TestReconcileIPC_RegularGlobalDaemonDispatchedThroughSM pins the no-legacy
// ownership model (spec §0.2): a REGULAR global daemon descriptor is now
// supervisor-owned exactly like a proxy descriptor, so the drift classifier
// posts EvIntentUpdate for it on BOTH directions (the api-side stop/restart
// paths therefore report it as a plain synchronous SM dispatch, not as
// DeferredToIntentWatcherCode):
//
//   - terminate direction (stopped intent + live SM) → post_ev_intent_update,
//     AppliedCount=1, EvIntentUpdate observed. This is the SM dispatch that
//     drives StRunning→StExiting→StIdle (deliberate stop, no reaper respawn).
//   - spawn direction (running intent + missing scheduler row) →
//     post_ev_intent_update, AppliedCount=1, EvIntentUpdate observed (the
//     supervisor spawns the daemon directly from supervisor-intent.json; the
//     dead legacy needs_manual_review row is gone).
//
// This inverts the prior parked-boundary guard
// (TestReconcileIPC_RegularGlobalDaemonNotDispatchedThroughSM), which
// asserted no_op / needs_manual_review and AppliedCount=0.
func TestReconcileIPC_RegularGlobalDaemonDispatchedThroughSM(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`

	t.Run("terminate direction stopped intent live SM posts EvIntentUpdate", func(t *testing.T) {
		fx := newReconcileTestFixture(t, regularGlobalDaemonIntentForReconcileTest(taskName))
		seedStoppedDaemonIntentForReconcileTest(t, fx.deps.stateDir, taskName)
		// Live SM state; no scheduler row (mirrors a regular daemon the
		// supervisor is actively running but which has no scheduler task).
		fx.ctrl.smStates.Store(taskName, api.StRunning)
		installSchedulerListFake(t, nil)

		req := api.IPCRequest{ID: 91, Cmd: "reconcile", Args: map[string]any{"apply": true}}
		conn := newFakeIPCConn()
		if err := handleReconcile(conn, req, fx.deps); err != nil {
			t.Fatalf("handleReconcile: %v", err)
		}
		_, body := decodeReconcileResponse(t, conn)
		if len(body.Drift) != 1 {
			t.Fatalf("drift = %+v, want exactly one row", body.Drift)
		}
		entry := body.Drift[0]
		if entry.Action != api.ReconcileActionPostEvIntentUpdate {
			t.Errorf("Action = %q, want %q (regular daemon is supervisor-owned: terminate posts EvIntentUpdate)",
				entry.Action, api.ReconcileActionPostEvIntentUpdate)
		}
		if entry.IntentDesired != api.ReconcileIntentDesiredStopped {
			t.Errorf("IntentDesired = %q, want %q", entry.IntentDesired, api.ReconcileIntentDesiredStopped)
		}
		if body.AppliedCount != 1 {
			t.Fatalf("AppliedCount = %d, want 1 (regular daemon terminate is dispatched through the SM)", body.AppliedCount)
		}
		select {
		case ev := <-fx.postedCh:
			if ev.Kind != api.EvIntentUpdate || ev.TaskName != taskName {
				t.Fatalf("posted event = %+v, want EvIntentUpdate for %s", ev, taskName)
			}
		case <-time.After(time.Second):
			t.Fatal("expected EvIntentUpdate post for the regular-daemon terminate direction")
		}
	})

	t.Run("spawn direction running intent missing scheduler posts EvIntentUpdate", func(t *testing.T) {
		fx := newReconcileTestFixture(t, regularGlobalDaemonIntentForReconcileTest(taskName))
		// No daemon-intent override → computeIntentDesired defaults to
		// running (the mixed-bootstrap default). No scheduler row → missing.
		installSchedulerListFake(t, nil)

		req := api.IPCRequest{ID: 92, Cmd: "reconcile", Args: map[string]any{"apply": true}}
		conn := newFakeIPCConn()
		if err := handleReconcile(conn, req, fx.deps); err != nil {
			t.Fatalf("handleReconcile: %v", err)
		}
		_, body := decodeReconcileResponse(t, conn)
		if len(body.Drift) != 1 {
			t.Fatalf("drift = %+v, want exactly one row", body.Drift)
		}
		entry := body.Drift[0]
		if entry.SchedulerState != api.ReconcileSchedulerStateMissing {
			t.Errorf("SchedulerState = %q, want %q", entry.SchedulerState, api.ReconcileSchedulerStateMissing)
		}
		if entry.IntentDesired != api.ReconcileIntentDesiredRunning {
			t.Errorf("IntentDesired = %q, want %q", entry.IntentDesired, api.ReconcileIntentDesiredRunning)
		}
		if entry.Action != api.ReconcileActionPostEvIntentUpdate {
			t.Errorf("Action = %q, want %q (regular daemon spawn: no-legacy → spawn directly from intent)",
				entry.Action, api.ReconcileActionPostEvIntentUpdate)
		}
		if body.AppliedCount != 1 {
			t.Fatalf("AppliedCount = %d, want 1 (regular daemon spawn is dispatched through the SM)", body.AppliedCount)
		}
		select {
		case ev := <-fx.postedCh:
			if ev.Kind != api.EvIntentUpdate || ev.TaskName != taskName {
				t.Fatalf("posted event = %+v, want EvIntentUpdate for %s", ev, taskName)
			}
		case <-time.After(time.Second):
			t.Fatal("expected EvIntentUpdate post for the regular-daemon spawn direction")
		}
	})
}

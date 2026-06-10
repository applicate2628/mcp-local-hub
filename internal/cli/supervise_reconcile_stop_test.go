package cli

// Spec §4 Phase A.1 (STOP supervisor-aware) — classifier + handleReconcile
// coverage for the terminate direction on supervisor-owned descriptors.
// Sibling of supervise_reconcile_ipc_test.go; uses the same fixture.

import (
	"encoding/json"
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
		{"running missing", missing, false, running, api.StIdle, api.ReconcileActionPostEvIntentUpdate},
		// hasSched rows unchanged.
		{"sched running intent stopped", api.ReconcileSchedulerStateRunning, true, stopped, api.StIdle, api.ReconcileActionPostEvIntentUpdate},
		{"sched stopped intent running", api.ReconcileSchedulerStateStopped, true, running, api.StIdle, api.ReconcileActionPostEvIntentUpdate},
		{"sched stopped intent stopped", api.ReconcileSchedulerStateStopped, true, stopped, api.StRunning, api.ReconcileActionNoOp},
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

// seedStoppedDaemonIntentForReconcileTest writes a daemon-intent.json with
// Desired=stopped for taskName into the fixture's state dir — the write
// `mcphub stop` performs via recordStopIntent before dialing the reconcile.
func seedStoppedDaemonIntentForReconcileTest(t *testing.T, stateDir, taskName string) {
	t.Helper()
	di := api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: time.Now().UTC(),
			},
		},
	}
	diRaw, err := json.Marshal(di)
	if err != nil {
		t.Fatalf("marshal daemon-intent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "daemon-intent.json"), diRaw, 0o600); err != nil {
		t.Fatalf("seed daemon-intent.json: %v", err)
	}
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

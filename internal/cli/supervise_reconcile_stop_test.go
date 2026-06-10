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
// spec §4 Phase A.1 classifier change: supervisor-owned descriptors have
// no scheduler row by design, so the terminate direction (intent=stopped
// against a live SM state) must classify as post_ev_intent_update — the
// scheduler can never witness these daemons running, and without this row
// `mcphub stop` falls back to taskkill, whose non-clean exit the reaper
// respawns (stop→respawn churn → quarantine). Dead/settled SM states and
// non-supervisor-owned rows stay no_op; the spawn direction is unchanged.
func TestClassifyDriftAction_TerminateDirectionSupervisorOwned(t *testing.T) {
	const (
		missing = api.ReconcileSchedulerStateMissing
		running = api.ReconcileIntentDesiredRunning
		stopped = api.ReconcileIntentDesiredStopped
	)
	cases := []struct {
		name            string
		schedState      string
		hasSched        bool
		intentDesired   string
		supervisorOwned bool
		smState         api.SMState
		want            string
	}{
		// Terminate direction: supervisor-owned + stopped intent + live SM.
		{"owned stopped spawning", missing, false, stopped, true, api.StSpawning, api.ReconcileActionPostEvIntentUpdate},
		{"owned stopped running", missing, false, stopped, true, api.StRunning, api.ReconcileActionPostEvIntentUpdate},
		{"owned stopped exiting", missing, false, stopped, true, api.StExiting, api.ReconcileActionPostEvIntentUpdate},
		{"owned stopped backoff", missing, false, stopped, true, api.StBackoffWaiting, api.ReconcileActionPostEvIntentUpdate},
		// Settled SM states: nothing live to terminate → no_op.
		{"owned stopped idle", missing, false, stopped, true, api.StIdle, api.ReconcileActionNoOp},
		{"owned stopped quarantined", missing, false, stopped, true, api.StQuarantined, api.ReconcileActionNoOp},
		// Non-supervisor-owned stopped rows stay no_op even with a live SM.
		{"legacy stopped running-sm", missing, false, stopped, false, api.StRunning, api.ReconcileActionNoOp},
		// Spawn direction unchanged by the smState parameter.
		{"owned running missing", missing, false, running, true, api.StIdle, api.ReconcileActionPostEvIntentUpdate},
		{"legacy running missing", missing, false, running, false, api.StIdle, api.ReconcileActionNeedsManualReview},
		// hasSched rows unchanged.
		{"sched running intent stopped", api.ReconcileSchedulerStateRunning, true, stopped, false, api.StIdle, api.ReconcileActionPostEvIntentUpdate},
		{"sched stopped intent running", api.ReconcileSchedulerStateStopped, true, running, false, api.StIdle, api.ReconcileActionPostEvIntentUpdate},
		{"sched stopped intent stopped", api.ReconcileSchedulerStateStopped, true, stopped, false, api.StRunning, api.ReconcileActionNoOp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDriftAction(tc.schedState, tc.hasSched, tc.intentDesired, tc.supervisorOwned, tc.smState)
			if got != tc.want {
				t.Fatalf("classifyDriftAction(%q, %v, %q, owned=%v, sm=%q) = %q, want %q",
					tc.schedState, tc.hasSched, tc.intentDesired, tc.supervisorOwned, tc.smState, got, tc.want)
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
// whose descriptor is supervisor-owned (Args[0]=="daemon" + proxy verb)
// so isSupervisorOwnedDescriptorForReconcile matches it.
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

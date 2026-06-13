package cli

// r38-2 P2 — running-descriptor-drift restart coverage.
//
// Bug: classifyDriftAction classified a StRunning daemon as steady-state
// no_op even when a global reinstall/upgrade REWROTE the same task name's
// descriptor (changed port / command / args / runtime_spec). The post-install
// `reconcile --apply` then posted NO event, so the old child kept serving the
// OLD descriptor (old port/command) while install had already written the NEW
// descriptor + client configs — clients pointed at a port/command that was
// never started. The fix drives a RESTART (post_ev_manual_restart ->
// EvManualRestart) for a StRunning daemon whose spawned descriptor drifted from
// the freshly-read intent, respawning the child with the NEW descriptor.
//
// These tests are the falsifying regression: the changed-descriptor cases FAIL
// pre-fix (they returned no_op / posted no restart). Negative controls assert
// no restart churn on an identical descriptor, on StBackoffWaiting (the timer
// owns retry), and on StQuarantined (needs_manual_review, no silent
// un-quarantine).

import (
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// dp is a tiny constructor for a plain global daemon descriptor (NOT an LSP /
// serena proxy, so the reconcile loop routes straight through
// classifyDriftAction rather than the orphaned-proxy guard).
func dpGlobalDescriptor(taskName, command string, port int, args []string) *api.SupervisorDaemon {
	return &api.SupervisorDaemon{
		TaskName: taskName,
		Server:   "memory",
		Daemon:   "default",
		Command:  command,
		Args:     args,
		Port:     port,
	}
}

// TestClassifyDriftAction_RunningDescriptorDrift is the pure-classifier
// falsifying test. Each row exercises the StRunning / StBackoffWaiting /
// StQuarantined classification under the no-scheduler-row (supervisor-owned)
// arm with a CHANGED vs IDENTICAL descriptor.
func TestClassifyDriftAction_RunningDescriptorDrift(t *testing.T) {
	const (
		missing = api.ReconcileSchedulerStateMissing
		running = api.ReconcileIntentDesiredRunning
	)
	taskName := `\mcp-local-hub-memory-default`

	oldDesc := dpGlobalDescriptor(taskName, "mcphub", 9128, []string{"daemon", "--server", "memory", "--daemon", "default"})

	// Drift variants (each changes exactly ONE spawn-affecting field vs oldDesc).
	driftPort := dpGlobalDescriptor(taskName, "mcphub", 9200, []string{"daemon", "--server", "memory", "--daemon", "default"})
	driftCommand := dpGlobalDescriptor(taskName, "mcphub-v2", 9128, []string{"daemon", "--server", "memory", "--daemon", "default"})
	driftArgs := dpGlobalDescriptor(taskName, "mcphub", 9128, []string{"daemon", "--server", "memory", "--daemon", "renamed"})
	identical := dpGlobalDescriptor(taskName, "mcphub", 9128, []string{"daemon", "--server", "memory", "--daemon", "default"})

	// Cosmetic-only change: ManifestHash differs but no spawn-affecting field
	// does → must NOT restart (no churn on a manifest-hash-only refresh).
	cosmetic := dpGlobalDescriptor(taskName, "mcphub", 9128, []string{"daemon", "--server", "memory", "--daemon", "default"})
	cosmetic.ManifestHash = "deadbeef"

	cases := []struct {
		name          string
		smState       api.SMState
		oldDescriptor *api.SupervisorDaemon
		newDescriptor *api.SupervisorDaemon
		want          string
	}{
		// THE BUG: StRunning + drifted descriptor → restart (was no_op pre-fix).
		{"running drift port", api.StRunning, oldDesc, driftPort, reconcileActionPostEvManualRestart},
		{"running drift command", api.StRunning, oldDesc, driftCommand, reconcileActionPostEvManualRestart},
		{"running drift args", api.StRunning, oldDesc, driftArgs, reconcileActionPostEvManualRestart},
		// Negative: StRunning + identical descriptor → steady-state no_op (no
		// churn on every apply).
		{"running identical descriptor", api.StRunning, oldDesc, identical, api.ReconcileActionNoOp},
		// Negative: cosmetic-only (manifest_hash) change → no_op.
		{"running cosmetic-only change", api.StRunning, oldDesc, cosmetic, api.ReconcileActionNoOp},
		// Negative: no cached descriptor (cache miss) → conservative no_op
		// (nothing to prove drift against).
		{"running nil cached descriptor", api.StRunning, nil, driftPort, api.ReconcileActionNoOp},
		// Negative: StBackoffWaiting → no_op even with drift (the backoff timer
		// owns the retry and already respawns from the refreshed cache;
		// preempting it would collapse the crash-loop delay).
		{"backoff drift port", api.StBackoffWaiting, oldDesc, driftPort, api.ReconcileActionNoOp},
		// Negative: StQuarantined → needs_manual_review even with drift (a
		// descriptor change must not silently un-quarantine).
		{"quarantined drift port", api.StQuarantined, oldDesc, driftPort, api.ReconcileActionNeedsManualReview},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDriftAction(missing, false, running, tc.smState, tc.oldDescriptor, tc.newDescriptor)
			if got != tc.want {
				t.Fatalf("classifyDriftAction(running, sm=%q, drift) = %q, want %q",
					tc.smState, got, tc.want)
			}
		})
	}
}

// TestClassifyDriftAction_RunningDescriptorDrift_RuntimeSpec covers the
// runtime_spec (materialized proxy launch spec) drift dimension — a proxy
// descriptor re-reads NONE of the manifest at spawn, so a proxy rewrite is
// captured entirely by the runtime_spec fields.
func TestClassifyDriftAction_RunningDescriptorDrift_RuntimeSpec(t *testing.T) {
	const (
		missing = api.ReconcileSchedulerStateMissing
		running = api.ReconcileIntentDesiredRunning
	)
	taskName := `\mcp-local-hub-serena-myws`

	base := func() *api.SupervisorDaemon {
		return &api.SupervisorDaemon{
			TaskName: taskName,
			Server:   "serena",
			Command:  "mcphub",
			Args:     []string{"daemon", "serena-proxy", "--task-name", taskName},
			Port:     9121,
			RuntimeSpec: &api.DaemonRuntimeSpec{
				SpecVersion:   api.DaemonRuntimeSpecVersion,
				ChildCommand:  "uvx",
				ChildArgs:     []string{"--from", "git+serena", "serena", "--context", "ide"},
				UpstreamPort:  19121,
				ExternalPort:  9121,
				WorkspacePath: `C:\ws`,
			},
		}
	}

	oldDesc := base()
	identical := base()

	driftUpstream := base()
	driftUpstream.RuntimeSpec.UpstreamPort = 19200

	driftChildArgs := base()
	driftChildArgs.RuntimeSpec.ChildArgs = []string{"--from", "git+serena", "serena", "--context", "desktop"}

	gainedSpec := base()
	lostSpec := base()
	lostSpec.RuntimeSpec = nil
	plainOld := base()
	plainOld.RuntimeSpec = nil

	cases := []struct {
		name          string
		oldDescriptor *api.SupervisorDaemon
		newDescriptor *api.SupervisorDaemon
		want          string
	}{
		{"running spec upstream-port drift", oldDesc, driftUpstream, reconcileActionPostEvManualRestart},
		{"running spec child-args drift", oldDesc, driftChildArgs, reconcileActionPostEvManualRestart},
		{"running spec lost (nil new)", oldDesc, lostSpec, reconcileActionPostEvManualRestart},
		{"running spec gained (nil old)", plainOld, gainedSpec, reconcileActionPostEvManualRestart},
		{"running spec identical", oldDesc, identical, api.ReconcileActionNoOp},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDriftAction(missing, false, running, api.StRunning, tc.oldDescriptor, tc.newDescriptor)
			if got != tc.want {
				t.Fatalf("classifyDriftAction(running, StRunning, spec-drift) = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReconcileApply_RunningDescriptorDrift_PostsManualRestart is the apply-path
// integration falsifying test: a StRunning global daemon whose on-disk intent
// was rewritten with a CHANGED port (the controller cache still holds the OLD
// port the child was spawned with) must classify post_ev_manual_restart and
// apply must post EvManualRestart (NOT EvIntentUpdate, NOT no_op).
func TestReconcileApply_RunningDescriptorDrift_PostsManualRestart(t *testing.T) {
	taskName := `\mcp-local-hub-memory-default`
	args := []string{"daemon", "--server", "memory", "--daemon", "default"}

	// NEW intent (on disk) — port rewritten 9128 -> 9200 by a global reinstall.
	newIntent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{*dpGlobalDescriptor(taskName, "mcphub", 9200, args)},
	}
	fx := newReconcileTestFixture(t, newIntent)

	// Controller cache holds the OLD descriptor (port 9128) — what the live
	// child was actually spawned with. This is the pre-refresh state the
	// drift loop reads before applyReconcileDrift refreshes it to NEW.
	oldIntent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{*dpGlobalDescriptor(taskName, "mcphub", 9128, args)},
	}
	fx.ctrl.intentCache.Refresh(oldIntent)
	fx.ctrl.smStates.Store(taskName, api.StRunning)

	// Supervisor-owned global row: no scheduler registration.
	installSchedulerListFake(t, []scheduler.TaskStatus{})

	req := api.IPCRequest{
		ID:   100,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 1 || len(body.Drift) != 1 {
		t.Fatalf("DriftCount=%d drift=%+v, want exactly one drift entry", body.DriftCount, body.Drift)
	}
	entry := body.Drift[0]
	if entry.TaskName != taskName {
		t.Errorf("TaskName = %q, want %q", entry.TaskName, taskName)
	}
	if entry.SMState != api.StRunning {
		t.Errorf("SMState = %q, want %q", entry.SMState, api.StRunning)
	}
	if entry.Action != reconcileActionPostEvManualRestart {
		t.Fatalf("Action = %q, want %q (StRunning + rewritten port must drive a restart)",
			entry.Action, reconcileActionPostEvManualRestart)
	}
	if body.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1", body.AppliedCount)
	}

	select {
	case ev := <-fx.postedCh:
		if ev.Kind != api.EvManualRestart {
			t.Fatalf("posted event kind = %q, want %q (EvManualRestart respawns with the NEW descriptor)",
				ev.Kind, api.EvManualRestart)
		}
		if ev.TaskName != taskName {
			t.Fatalf("posted event task_name = %q, want %q", ev.TaskName, taskName)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("expected an EvManualRestart post for the drifted StRunning daemon; got none")
	}
}

// TestReconcileApply_RunningIdenticalDescriptor_NoRestartChurn is the apply-path
// negative control: a StRunning daemon whose on-disk descriptor is IDENTICAL to
// the cached spawned descriptor must classify no_op and post NOTHING (no
// restart churn on a steady-state apply).
func TestReconcileApply_RunningIdenticalDescriptor_NoRestartChurn(t *testing.T) {
	taskName := `\mcp-local-hub-memory-default`
	args := []string{"daemon", "--server", "memory", "--daemon", "default"}

	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{*dpGlobalDescriptor(taskName, "mcphub", 9128, args)},
	}
	fx := newReconcileTestFixture(t, intent)
	// Cache already holds the SAME descriptor (fixture seeded it); make the
	// identity explicit so the test is robust to fixture changes.
	fx.ctrl.intentCache.Refresh(&api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{*dpGlobalDescriptor(taskName, "mcphub", 9128, args)},
	})
	fx.ctrl.smStates.Store(taskName, api.StRunning)

	installSchedulerListFake(t, []scheduler.TaskStatus{})

	req := api.IPCRequest{
		ID:   101,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.DriftCount != 1 || len(body.Drift) != 1 {
		t.Fatalf("DriftCount=%d drift=%+v, want one entry", body.DriftCount, body.Drift)
	}
	if got := body.Drift[0].Action; got != api.ReconcileActionNoOp {
		t.Fatalf("Action = %q, want %q (identical descriptor must be steady-state)", got, api.ReconcileActionNoOp)
	}
	if body.AppliedCount != 0 {
		t.Fatalf("AppliedCount = %d, want 0 (no restart on identical descriptor)", body.AppliedCount)
	}

	// Settle window: no event must be posted.
	time.Sleep(75 * time.Millisecond)
	if got := fx.postedCount.Load(); got != 0 {
		t.Fatalf("identical-descriptor apply posted %d events; want 0", got)
	}
}

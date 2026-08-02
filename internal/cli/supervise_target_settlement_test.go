package cli

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

type targetSettlementHarness struct {
	ctrl       *supervisorController
	target     api.ReconcileTarget
	row        api.WorkspaceEntry
	registry   string
	loopCancel context.CancelFunc
	loopDone   chan struct{}
}

func newTargetSettlementHarness(t *testing.T) *targetSettlementHarness {
	t.Helper()
	workspace := t.TempDir()
	registryPath := filepath.Join(t.TempDir(), "workspaces.yaml")
	registeredAt := time.Date(2026, 8, 2, 10, 11, 12, 123456789, time.UTC)
	row := api.WorkspaceEntry{
		WorkspaceKey:  "settle123",
		WorkspacePath: workspace,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9311,
		TaskName:      "mcp-local-hub-serena-settle123",
		RegisteredAt:  registeredAt,
	}
	target := api.ReconcileTarget{
		WorkspaceKey:  row.WorkspaceKey,
		WorkspacePath: row.WorkspacePath,
		TaskName:      row.TaskName,
		RegisteredAt:  registeredAt.Format(time.RFC3339Nano),
		ExpectedPort:  row.Port,
	}
	writeTargetSettlementRegistryRow(t, registryPath, row)

	loop := api.NewEventLoop(16)
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(target.TaskName, 4812, registeredAt.Add(time.Second))
	intentCache := newIntentCache()
	intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{
		TaskName:  canonicalSupervisorTaskName(target.TaskName),
		Workspace: target.WorkspacePath,
		Port:      target.ExpectedPort,
		Command:   "test-daemon",
	}}})
	ctrl := &supervisorController{
		intentCache: intentCache,
		eventLoop:   loop,
		tracker:     tracker,
		targetRegistryPath: func() (string, error) {
			return registryPath, nil
		},
		targetLivenessProbe: func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) supervisorLivenessVerdict {
			return supervisorLivenessVerdict{Live: true, PortBound: true, OwnershipProof: supervisorLivenessProofPortOwnerPID}
		},
	}
	loop.RegisterHandler(ctrl.handleLoopEvent)
	loopCtx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		loop.Run(loopCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-loopDone
	})
	return &targetSettlementHarness{
		ctrl:       ctrl,
		target:     target,
		row:        row,
		registry:   registryPath,
		loopCancel: cancel,
		loopDone:   loopDone,
	}
}

func writeTargetSettlementRegistryRow(t *testing.T, path string, row api.WorkspaceEntry) {
	t.Helper()
	registry := api.NewRegistry(path)
	unlock, err := registry.Lock()
	if err != nil {
		t.Fatalf("lock registry: %v", err)
	}
	defer unlock()
	if err := registry.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := registry.PutSerena(row); err != nil {
		t.Fatalf("put serena row: %v", err)
	}
	if err := registry.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
}

func TestSettleReconcileTarget_ReadyRequiresProcessedBarrierAndExactOwner(t *testing.T) {
	h := newTargetSettlementHarness(t)
	priorProcessed := make(chan struct{})
	h.ctrl.eventLoop.Post(api.LoopEvent{
		Kind: evControllerBarrier,
		Body: map[string]any{controllerBarrierResultBodyKey: priorProcessed},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := h.ctrl.settleReconcileTarget(ctx, h.target)
	if got.State != api.ReconcileTargetSettlementReady || got.Reason != api.ReconcileTargetReasonReady {
		t.Fatalf("settlement = %+v, want ready", got)
	}
	select {
	case <-priorProcessed:
	default:
		t.Fatal("ready returned before the controller processed the preceding event")
	}
	if got.CurrentPID != 4812 || got.PIDGeneration != 1 {
		t.Fatalf("runtime identity = pid %d generation %d, want 4812/1", got.CurrentPID, got.PIDGeneration)
	}
}

func TestSettleReconcileTarget_EnqueuedBarrierWithoutProcessingTimesOut(t *testing.T) {
	loop := api.NewEventLoop(16) // deliberately not run
	ctrl := &supervisorController{eventLoop: loop}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got := ctrl.settleReconcileTarget(ctx, api.ReconcileTarget{})
	if got.State != api.ReconcileTargetSettlementIncomplete || got.Reason != api.ReconcileTargetReasonSettlementTimeout {
		t.Fatalf("settlement = %+v, want incomplete/settlement_timeout", got)
	}
}

func TestSettleReconcileTarget_FullLoopCancellationIsTyped(t *testing.T) {
	loop := api.NewEventLoop(16) // deliberately not run
	for loop.TryPost(api.LoopEvent{Kind: evControllerBarrier}) {
	}
	ctrl := &supervisorController{eventLoop: loop}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := ctrl.settleReconcileTarget(ctx, api.ReconcileTarget{})
	if got.State != api.ReconcileTargetSettlementIncomplete || got.Reason != api.ReconcileTargetReasonSettlementCancelled {
		t.Fatalf("settlement = %+v, want incomplete/settlement_cancelled", got)
	}
}

func TestSettleReconcileTarget_BindGraceIsNotReady(t *testing.T) {
	h := newTargetSettlementHarness(t)
	h.ctrl.targetLivenessProbe = func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) supervisorLivenessVerdict {
		return supervisorLivenessVerdict{Live: true}
	}
	h.ctrl.targetSettlementWait = func(context.Context) error { return context.DeadlineExceeded }
	got := h.ctrl.settleReconcileTarget(context.Background(), h.target)
	if got.State != api.ReconcileTargetSettlementIncomplete || got.Reason != api.ReconcileTargetReasonSettlementTimeout {
		t.Fatalf("settlement = %+v, bind-grace live=true/bound=false must not be ready", got)
	}
}

func TestSettleReconcileTarget_TCPOnlyUnsupportedIdentityNeverReady(t *testing.T) {
	h := newTargetSettlementHarness(t)
	var observed supervisorLivenessVerdict
	h.ctrl.targetLivenessProbe = func(d api.SupervisorDaemon, entry DaemonRuntimeEntry, now time.Time) supervisorLivenessVerdict {
		observed = supervisorDaemonLivenessVerdictWithProbe(d, entry, now, supervisorLivenessProbe{
			PIDAlive: func(int) bool { return true },
			PIDIdentity: func(process.PIDIdentityProof) error {
				return process.ErrProcessIdentityUnsupported
			},
			PortOwnerPID: nil,
			PortLive:     func(int) bool { return true }, // foreign/unknown listener
		}, supervisorStartupBindDeadline(d))
		return observed
	}
	got := h.ctrl.settleReconcileTarget(context.Background(), h.target)
	if !observed.Live || !observed.PortBound || observed.OwnershipProof != supervisorLivenessProofTCPOnly {
		t.Fatalf("canonical operational verdict = %+v, want live TCP-only provenance", observed)
	}
	if got.State != api.ReconcileTargetSettlementIncomplete || got.Reason != api.ReconcileTargetReasonLivenessUnverified {
		t.Fatalf("settlement = %+v, unsupported identity + TCP-only listener must never be ready", got)
	}
}

func TestSettleReconcileTarget_VerifiedIdentityAndTCPIsAcceptedEquivalentProof(t *testing.T) {
	h := newTargetSettlementHarness(t)
	h.ctrl.targetLivenessProbe = func(d api.SupervisorDaemon, entry DaemonRuntimeEntry, now time.Time) supervisorLivenessVerdict {
		return supervisorDaemonLivenessVerdictWithProbe(d, entry, now, supervisorLivenessProbe{
			PIDAlive:    func(int) bool { return true },
			PIDIdentity: func(process.PIDIdentityProof) error { return nil },
			PortLive:    func(int) bool { return true },
		}, supervisorStartupBindDeadline(d))
	}

	got := h.ctrl.settleReconcileTarget(context.Background(), h.target)
	if got.State != api.ReconcileTargetSettlementReady || got.Reason != api.ReconcileTargetReasonReady {
		t.Fatalf("settlement = %+v, verified PID identity + TCP is the accepted POSIX equivalent proof", got)
	}
}

func TestSettleReconcileTarget_PortOwnerMismatchFails(t *testing.T) {
	h := newTargetSettlementHarness(t)
	h.ctrl.targetLivenessProbe = func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) supervisorLivenessVerdict {
		return supervisorLivenessVerdict{Reason: supervisorLivenessReasonPortOwnerMismatch, IdentityDetail: "owner pid 9912"}
	}
	got := h.ctrl.settleReconcileTarget(context.Background(), h.target)
	if got.State != api.ReconcileTargetSettlementFailed || got.Reason != api.ReconcileTargetReasonPortOwnerMismatch || got.Error != "owner pid 9912" {
		t.Fatalf("settlement = %+v, want failed/port_owner_mismatch with detail", got)
	}
}

func TestSettleReconcileTarget_PortUnboundIsExplicitIncomplete(t *testing.T) {
	h := newTargetSettlementHarness(t)
	h.ctrl.targetLivenessProbe = func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) supervisorLivenessVerdict {
		return supervisorLivenessVerdict{Reason: supervisorLivenessReasonPortUnbound}
	}
	got := h.ctrl.settleReconcileTarget(context.Background(), h.target)
	if got.State != api.ReconcileTargetSettlementIncomplete || got.Reason != api.ReconcileTargetReasonPortUnbound {
		t.Fatalf("settlement = %+v, want incomplete/port_unbound", got)
	}
}

func TestSettleReconcileTarget_SpawnFailurePreservesCause(t *testing.T) {
	h := newTargetSettlementHarness(t)
	h.ctrl.tracker.MarkSpawnFailed(h.target.TaskName, errors.New("ScriptEnv launch failed"))
	got := h.ctrl.settleReconcileTarget(context.Background(), h.target)
	if got.State != api.ReconcileTargetSettlementFailed || got.Reason != api.ReconcileTargetReasonBackoff || got.Error != "ScriptEnv launch failed" {
		t.Fatalf("settlement = %+v, want failed/backoff with spawn cause", got)
	}
}

func TestSettleReconcileTarget_QuarantineAndMissingIntentAreTerminal(t *testing.T) {
	t.Run("quarantined", func(t *testing.T) {
		h := newTargetSettlementHarness(t)
		h.ctrl.tracker.MarkQuarantined(h.target.TaskName)
		got := h.ctrl.settleReconcileTarget(context.Background(), h.target)
		if got.State != api.ReconcileTargetSettlementFailed || got.Reason != api.ReconcileTargetReasonQuarantined {
			t.Fatalf("settlement = %+v, want failed/quarantined", got)
		}
	})
	t.Run("intent missing", func(t *testing.T) {
		h := newTargetSettlementHarness(t)
		h.ctrl.intentCache.Refresh(&api.SupervisorIntentFile{})
		got := h.ctrl.settleReconcileTarget(context.Background(), h.target)
		if got.State != api.ReconcileTargetSettlementFailed || got.Reason != api.ReconcileTargetReasonIntentMissing {
			t.Fatalf("settlement = %+v, want failed/intent_missing", got)
		}
	})
}

func TestSettleReconcileTarget_RegistryReplacementDuringProbeCannotReady(t *testing.T) {
	h := newTargetSettlementHarness(t)
	h.ctrl.targetLivenessProbe = func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) supervisorLivenessVerdict {
		replaced := h.row
		replaced.RegisteredAt = replaced.RegisteredAt.Add(time.Nanosecond)
		writeTargetSettlementRegistryRow(t, h.registry, replaced)
		return supervisorLivenessVerdict{Live: true, PortBound: true, OwnershipProof: supervisorLivenessProofPortOwnerPID}
	}
	got := h.ctrl.settleReconcileTarget(context.Background(), h.target)
	if got.State != api.ReconcileTargetSettlementFailed || got.Reason != api.ReconcileTargetReasonTargetGenerationReplaced {
		t.Fatalf("settlement = %+v, want failed/target_generation_replaced", got)
	}
}

func TestSettleReconcileTarget_PIDGenerationReplacementDuringProbeCannotReady(t *testing.T) {
	h := newTargetSettlementHarness(t)
	h.ctrl.targetLivenessProbe = func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) supervisorLivenessVerdict {
		h.ctrl.tracker.MarkSpawned(h.target.TaskName, 5923, h.row.RegisteredAt.Add(2*time.Second))
		return supervisorLivenessVerdict{Live: true, PortBound: true, OwnershipProof: supervisorLivenessProofPortOwnerPID}
	}
	h.ctrl.targetSettlementWait = func(context.Context) error { return context.DeadlineExceeded }
	got := h.ctrl.settleReconcileTarget(context.Background(), h.target)
	if got.State == api.ReconcileTargetSettlementReady || got.Reason != api.ReconcileTargetReasonSettlementTimeout {
		t.Fatalf("settlement = %+v, changed PID generation must not be ready", got)
	}
	if got.CurrentPID != 5923 || got.PIDGeneration != 2 {
		t.Fatalf("settlement retained stale runtime identity: %+v", got)
	}
}

func TestParseReconcileArgs_SettleTargetValidation(t *testing.T) {
	valid := map[string]any{
		"apply": true,
		"settle_target": map[string]any{
			"workspace_key":  "settle123",
			"workspace_path": "testdata/workspace",
			"task_name":      "mcp-local-hub-serena-settle123",
			"registered_at":  "2026-08-02T10:11:12.123456789Z",
			"expected_port":  9311,
		},
	}
	args, err := parseReconcileArgs(valid)
	if err != nil || args.SettleTarget == nil {
		t.Fatalf("valid settle target rejected: args=%+v err=%v", args, err)
	}
	valid["apply"] = false
	if _, err := parseReconcileArgs(valid); err == nil {
		t.Fatal("settle_target with apply=false accepted")
	}
}

func TestReconcileIPC_TargetSettlementIsReturnedForExactGeneration(t *testing.T) {
	workspace := t.TempDir()
	registeredAt := time.Date(2026, 8, 2, 13, 14, 15, 987654321, time.UTC)
	target := api.ReconcileTarget{
		WorkspaceKey:  "handler123",
		WorkspacePath: workspace,
		TaskName:      "mcp-local-hub-serena-handler123",
		RegisteredAt:  registeredAt.Format(time.RFC3339Nano),
		ExpectedPort:  9322,
	}
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{{
		TaskName:  canonicalSupervisorTaskName(target.TaskName),
		Workspace: workspace,
		Port:      target.ExpectedPort,
	}}}
	fx := newReconcileTestFixture(t, intent)
	registryPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	writeTargetSettlementRegistryRow(t, registryPath, api.WorkspaceEntry{
		WorkspaceKey:  target.WorkspaceKey,
		WorkspacePath: target.WorkspacePath,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          target.ExpectedPort,
		TaskName:      target.TaskName,
		RegisteredAt:  registeredAt,
	})
	fx.ctrl.tracker = fx.deps.runtimeTracker
	fx.ctrl.targetRegistryPath = func() (string, error) { return registryPath, nil }
	fx.ctrl.targetLivenessProbe = func(api.SupervisorDaemon, DaemonRuntimeEntry, time.Time) supervisorLivenessVerdict {
		return supervisorLivenessVerdict{Live: true, PortBound: true, OwnershipProof: supervisorLivenessProofPortOwnerPID}
	}
	fx.ctrl.tracker.MarkSpawned(target.TaskName, 6042, registeredAt.Add(time.Second))
	installSchedulerListFake(t, nil)

	conn := newFakeIPCConn()
	req := api.IPCRequest{
		ID:  771,
		Cmd: "reconcile",
		Args: map[string]any{
			"apply":         true,
			"settle_target": target,
		},
	}
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if body.TargetSettlement == nil {
		t.Fatal("TargetSettlement absent for requested target")
	}
	if body.TargetSettlement.State != api.ReconcileTargetSettlementReady || body.TargetSettlement.Target != target {
		t.Fatalf("TargetSettlement = %+v, want ready exact target", body.TargetSettlement)
	}
}

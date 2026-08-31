package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func TestStopBatchSettlementRequiresControllerIdleAndFreeListener(t *testing.T) {
	const taskName = `\mcp-local-hub-time-default`
	const port = 1

	loop := api.NewEventLoop(8)
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	tracker.MarkExited(taskName)
	ctrl := &supervisorController{
		intentCache:  newIntentCache(),
		daemonIntent: newDaemonIntentCache(),
		eventLoop:    loop,
		tracker:      tracker,
		stoppedSettlementPortOwnersSnapshot: func(context.Context) (map[int]int, error) {
			return map[int]int{}, nil
		},
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{
		TaskName: taskName,
		Port:     port,
	}}})
	ctrl.smStates.Store(taskName, api.StIdle)
	loop.RegisterHandler(func(event api.LoopEvent) {
		if event.Kind != evControllerBarrier {
			t.Fatalf("unexpected loop event: %+v", event)
		}
		close(event.Body[controllerBarrierResultBodyKey].(chan struct{}))
	})
	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(loopCtx)

	got := ctrl.settleStopBatchTargets(context.Background(), []api.StopBatchTargetV1{{TaskName: taskName, ExpectedPort: port}})[0]
	if got.State != api.StoppedSettlementStopped || got.Reason != api.StoppedSettlementReasonStopped {
		t.Fatalf("settlement = %+v, want stopped/free", got)
	}
	if got.CurrentPID != 0 || got.PIDGeneration != 1 {
		t.Fatalf("terminal runtime identity = pid %d generation %d, want 0/1", got.CurrentPID, got.PIDGeneration)
	}
}

func TestStopBatchSettlementRejectsIdleStateWithLiveListener(t *testing.T) {
	const taskName = `\mcp-local-hub-time-default`
	const port = 9128

	loop := api.NewEventLoop(8)
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	tracker.MarkExited(taskName)
	ctrl := &supervisorController{
		intentCache: newIntentCache(),
		eventLoop:   loop,
		tracker:     tracker,
		stoppedSettlementPortOwnersSnapshot: func(context.Context) (map[int]int, error) {
			return map[int]int{port: 4812}, nil
		},
		stoppedSettlementWait: func(context.Context) error {
			return context.DeadlineExceeded
		},
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{
		TaskName: taskName,
		Port:     port,
	}}})
	ctrl.smStates.Store(taskName, api.StIdle)
	loop.RegisterHandler(func(event api.LoopEvent) {
		if event.Kind != evControllerBarrier {
			t.Fatalf("unexpected loop event: %+v", event)
		}
		close(event.Body[controllerBarrierResultBodyKey].(chan struct{}))
	})
	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(loopCtx)

	got := ctrl.settleStopBatchTargets(context.Background(), []api.StopBatchTargetV1{{TaskName: taskName, ExpectedPort: port}})[0]
	if got.State != api.StoppedSettlementFailed || got.Reason != api.StoppedSettlementReasonListenerAlive {
		t.Fatalf("settlement = %+v, want failed/listener_alive", got)
	}
	if got.CurrentPID != 4812 || got.PIDGeneration != 1 {
		t.Fatalf("live listener identity = pid %d generation %d, want 4812/1", got.CurrentPID, got.PIDGeneration)
	}
}

func TestStopBatchSettlementCommitsDurableReceiptLastAfterExactExitAndFreePort(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	const port = 9128

	loop := api.NewEventLoop(8)
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	receipt := beginStopSettlementForTest(t, tracker, statePath, taskName)
	if _, err := tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhaseExitObserved, "", ""); err != nil {
		t.Fatalf("record exact exit observation: %v", err)
	}
	tracker.MarkExited(taskName)
	ctrl := &supervisorController{
		intentCache: newIntentCache(),
		eventLoop:   loop,
		tracker:     tracker,
		statePath:   statePath,
		stoppedSettlementPortOwnersSnapshot: func(context.Context) (map[int]int, error) {
			return map[int]int{}, nil
		},
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{
		TaskName: taskName,
		Port:     port,
	}}})
	ctrl.smStates.Store(taskName, api.StIdle)
	loop.RegisterHandler(func(event api.LoopEvent) {
		if event.Kind == evControllerBarrier {
			close(event.Body[controllerBarrierResultBodyKey].(chan struct{}))
		}
	})
	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(loopCtx)

	got := ctrl.settleStopBatchTargets(context.Background(), []api.StopBatchTargetV1{{TaskName: taskName, ExpectedPort: port}})[0]
	if got.State != api.StoppedSettlementStopped || got.Reason != api.StoppedSettlementReasonStopped {
		t.Fatalf("settlement = %+v, want stopped/free", got)
	}
	if receipt, present := tracker.StopSettlementReceipt(taskName); present {
		t.Fatalf("terminal receipt remained in runtime tracker: %+v", receipt)
	}
	state, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if _, present := state.StopSettlements[taskName]; present {
		t.Fatalf("terminal receipt remained durable: %+v", state.StopSettlements[taskName])
	}
}

func TestRecoverPendingStopReceiptsOwnsOneBoundedRecoverySweep(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	const port = 1
	loop := api.NewEventLoop(8)
	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	receipt := beginStopSettlementForTest(t, tracker, statePath, taskName)
	var err error
	receipt, err = tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhaseExitObserved, "", "")
	if err != nil {
		t.Fatalf("record exact exit: %v", err)
	}
	tracker.MarkExited(taskName)
	ctrl := &supervisorController{
		ctx:          loopCtx,
		intentCache:  newIntentCache(),
		daemonIntent: newDaemonIntentCache(),
		eventLoop:    loop,
		tracker:      tracker,
		statePath:    statePath,
		stoppedSettlementPortOwnersSnapshot: func(context.Context) (map[int]int, error) {
			return map[int]int{}, nil
		},
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: taskName, Port: port}}})
	ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{taskName: {Desired: api.IntentDesiredStopped, UpdatedAt: time.Now().UTC()}}})
	ctrl.smStates.Store(taskName, api.StIdle)
	entry, present := tracker.Get(taskName)
	if !present || !stopReceiptRecoveryIdentityMatches(receipt, receipt.Phase, entry) {
		t.Fatalf("exit-observed recovery identity mismatch: receipt=%+v entry=%+v present=%v", receipt, entry, present)
	}
	recoveryDelivered := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(event api.LoopEvent) {
		if event.Kind == evStopSettlementRecovery {
			recoveryDelivered <- event
		}
		ctrl.handleLoopEvent(event)
	})
	go loop.Run(loopCtx)

	if err := ctrl.enqueueStopSettlementRecovery(context.Background(), taskName); err != nil {
		t.Fatalf("enqueue recovery: %v", err)
	}
	select {
	case event := <-recoveryDelivered:
		if event.TaskName != taskName {
			t.Fatalf("recovery event task = %q, want %q", event.TaskName, taskName)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO recovery event was not delivered")
	}
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		if _, pending := tracker.StopSettlementReceipt(taskName); !pending {
			break
		}
		select {
		case <-timeout.C:
			current, _ := tracker.StopSettlementReceipt(taskName)
			currentEntry, currentPresent := tracker.Get(taskName)
			_, armed := ctrl.stopSettlementRecoveryArmed.Load(taskName)
			t.Fatalf("FIFO recovery left receipt pending: phase=%s mode=%s failure=%s resume=%s receipt=%+v entry=%+v entry_present=%v armed=%v", current.Phase, current.Mode, current.FailureClass, current.ResumePhase, current, currentEntry, currentPresent, armed)
		default:
			runtime.Gosched()
		}
	}
}

func TestControllerRealChildExitAdvancesOnlyMatchingStopReceipt(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	receipt := beginStopSettlementForTest(t, tracker, statePath, taskName)
	ctrl := &supervisorController{
		intentCache:  newIntentCache(),
		daemonIntent: newDaemonIntentCache(),
		tracker:      tracker,
		statePath:    statePath,
		terminate:    func(api.SupervisorDaemon) error { return nil },
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: taskName, Port: 9128}}})
	ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		taskName: {Desired: api.IntentDesiredStopped},
	}})
	ctrl.smStates.Store(taskName, api.StExiting)

	ctrl.handleLoopEvent(api.LoopEvent{Kind: evOwnedChildWaitExit, TaskName: taskName, Body: map[string]any{
		"pid":            4812,
		"pid_generation": 1,
		"started_at":     "2026-08-31T12:00:00Z",
		"clean_exit":     true,
	}})
	got, present := tracker.StopSettlementReceipt(taskName)
	if !present || got.Phase != api.StopSettlementPhaseExitObserved || got.Epoch != receipt.Epoch || got.PID != receipt.PID || got.PIDGeneration != receipt.PIDGeneration {
		t.Fatalf("receipt after exact real exit = %+v present=%v, want matching exit-observed receipt", got, present)
	}
}

func TestSupervisorStatusDaemons_ProjectsPendingStopReceiptAsStopping(t *testing.T) {
	stateDir := t.TempDir()
	const taskName = `\mcp-local-hub-time-default`
	const port = 9128
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{{
		TaskName: taskName,
		Port:     port,
	}}}); err != nil {
		t.Fatalf("write intent: %v", err)
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	_ = beginStopSettlementForTest(t, tracker, filepath.Join(stateDir, "supervisor-state.json"), taskName)
	oldSnapshot := loopbackPortOwnersSnapshotFn
	loopbackPortOwnersSnapshotFn = func() (map[int]int, error) { return map[int]int{port: 4812}, nil }
	t.Cleanup(func() { loopbackPortOwnersSnapshotFn = oldSnapshot })

	rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(rows) != 1 || rows[0]["state"] != "Stopping" {
		t.Fatalf("status rows = %+v, want one Stopping row", rows)
	}
	diagnostic, ok := rows[0]["stop_settlement"].(map[string]any)
	if !ok || diagnostic["phase"] != string(api.StopSettlementPhaseStopRequested) || diagnostic["observed_port_owner_pid"] != 4812 {
		t.Fatalf("stop settlement diagnostic = %#v, want stop-requested receipt and owner pid", rows[0]["stop_settlement"])
	}
}

func TestStopBatchAdmissionDurablyMirrorsWholeBatchBeforeTransitions(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const first = `\mcp-local-hub-time-default`
	const second = `\mcp-local-hub-fetch-default`
	tracker := NewDaemonRuntimeTracker()
	started := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tracker.MarkSpawned(first, 4812, started)
	tracker.MarkSpawned(second, 4813, started)
	stops := map[string]api.DaemonIntent{
		first:  {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonUserStop, UpdatedAt: time.Now().UTC()},
		second: {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonUserStop, UpdatedAt: time.Now().UTC()},
	}
	intent := &api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: first, Port: 9242}, {TaskName: second, Port: 9243}}, Stops: stops}
	ctrl := &supervisorController{
		intentCache:  newIntentCache(),
		daemonIntent: newDaemonIntentCache(),
		tracker:      tracker,
		statePath:    statePath,
		terminate:    func(api.SupervisorDaemon) error { return nil },
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.daemonIntent.Refresh(api.UnifiedStopsFile(intent, nil))
	ctrl.smStates.Store(first, api.StRunning)
	ctrl.smStates.Store(second, api.StRunning)
	intent.IntentGeneration = 1
	command := api.StopBatchCommandV1{ProtocolVersion: 1, BatchID: "admission-whole-batch", Targets: []api.StopBatchTargetV1{{TaskName: first, ExpectedPort: 9242}, {TaskName: second, ExpectedPort: 9243}}, IntentGeneration: 1, SupervisorIntent: intent, UnifiedStops: api.UnifiedStopsFile(intent, nil)}
	reply := make(chan stopBatchLoopResult, 1)
	ctrl.handleStopBatchOnLoop(stopBatchLoopRequest{command: command, reply: reply})
	if outcome := <-reply; outcome.err != nil {
		t.Fatalf("batch admission: %v", outcome.err)
	}
	state, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read batch state: %v", err)
	}
	if len(state.StopSettlements) != 2 {
		t.Fatalf("state receipts = %+v, want full batch", state.StopSettlements)
	}
	for _, taskName := range []string{first, second} {
		receipt, ok := tracker.StopSettlementReceipt(taskName)
		if !ok || receipt.BatchID != command.BatchID || receipt.Phase != api.StopSettlementPhaseStopRequested || state.StopSettlements[taskName] != receipt {
			t.Fatalf("receipt %s memory=%+v durable=%+v, want exact stop_requested mirror", taskName, receipt, state.StopSettlements[taskName])
		}
	}
}

func TestStopBatchMixedTrackedAndIdlePortFenceAdmitsInOrder(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const tracked = `\mcp-local-hub-time-default`
	const fenced = `\mcp-local-hub-fetch-default`
	tracker := NewDaemonRuntimeTracker()
	started := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tracker.MarkSpawned(tracked, 4812, started)
	tracker.MarkSpawned(fenced, 4813, started)
	tracker.MarkExited(fenced)
	intent := &api.SupervisorIntentFile{IntentGeneration: 1, Daemons: []api.SupervisorDaemon{{TaskName: tracked, Port: 9242}, {TaskName: fenced, Port: 9243}}, Stops: map[string]api.DaemonIntent{
		tracked: {Desired: api.IntentDesiredStopped, UpdatedAt: started},
		fenced:  {Desired: api.IntentDesiredStopped, UpdatedAt: started},
	}}
	terminated := 0
	ctrl := &supervisorController{intentCache: newIntentCache(), daemonIntent: newDaemonIntentCache(), tracker: tracker, statePath: statePath, terminate: func(api.SupervisorDaemon) error { terminated++; return nil }}
	ctrl.intentCache.Refresh(intent)
	ctrl.daemonIntent.Refresh(api.UnifiedStopsFile(intent, nil))
	ctrl.smStates.Store(tracked, api.StRunning)
	ctrl.smStates.Store(fenced, api.StIdle)
	command := api.StopBatchCommandV1{ProtocolVersion: 1, BatchID: "mixed-port-fence", Targets: []api.StopBatchTargetV1{{TaskName: tracked, ExpectedPort: 9242}, {TaskName: fenced, ExpectedPort: 9243}}, IntentGeneration: 1, SupervisorIntent: intent, UnifiedStops: api.UnifiedStopsFile(intent, nil)}
	reply := make(chan stopBatchLoopResult, 1)
	ctrl.handleStopBatchOnLoop(stopBatchLoopRequest{command: command, reply: reply})
	if result := <-reply; result.err != nil {
		t.Fatalf("admit mixed batch: %v", result.err)
	}
	if terminated != 1 {
		t.Fatalf("terminate calls = %d, want one tracked target only", terminated)
	}
	trackedReceipt, trackedOK := tracker.StopSettlementReceipt(tracked)
	fencedReceipt, fencedOK := tracker.StopSettlementReceipt(fenced)
	if !trackedOK || trackedReceipt.Phase != api.StopSettlementPhaseStopRequested || trackedReceipt.Mode != "stop" {
		t.Fatalf("tracked receipt = %+v present=%v", trackedReceipt, trackedOK)
	}
	if !fencedOK || fencedReceipt.Phase != api.StopSettlementPhaseExitObserved || fencedReceipt.Mode != "port_fence" || fencedReceipt.PID != 0 {
		t.Fatalf("port fence receipt = %+v present=%v", fencedReceipt, fencedOK)
	}
}

func TestStopBatchApplicationFailureContinuesRemainingTargets(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const failed = `\mcp-local-hub-time-default`
	const continued = `\mcp-local-hub-fetch-default`
	started := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(failed, 4812, started)
	tracker.MarkSpawned(continued, 4813, started)
	intent := &api.SupervisorIntentFile{IntentGeneration: 1, Daemons: []api.SupervisorDaemon{{TaskName: failed, Port: 9242}, {TaskName: continued, Port: 9243}}, Stops: map[string]api.DaemonIntent{failed: {Desired: api.IntentDesiredStopped, UpdatedAt: started}, continued: {Desired: api.IntentDesiredStopped, UpdatedAt: started}}}
	terminated := make([]string, 0, 2)
	ctrl := &supervisorController{intentCache: newIntentCache(), daemonIntent: newDaemonIntentCache(), tracker: tracker, statePath: statePath, terminate: func(d api.SupervisorDaemon) error {
		terminated = append(terminated, d.TaskName)
		if d.TaskName == failed {
			return errors.New("terminate failed")
		}
		return nil
	}}
	ctrl.intentCache.Refresh(intent)
	ctrl.daemonIntent.Refresh(api.UnifiedStopsFile(intent, nil))
	ctrl.smStates.Store(failed, api.StRunning)
	ctrl.smStates.Store(continued, api.StRunning)
	command := api.StopBatchCommandV1{ProtocolVersion: 1, BatchID: "continue-after-application-failure", Targets: []api.StopBatchTargetV1{{TaskName: failed, ExpectedPort: 9242}, {TaskName: continued, ExpectedPort: 9243}}, IntentGeneration: 1, SupervisorIntent: intent, UnifiedStops: api.UnifiedStopsFile(intent, nil)}
	reply := make(chan stopBatchLoopResult, 1)
	ctrl.handleStopBatchOnLoop(stopBatchLoopRequest{command: command, reply: reply})
	result := <-reply
	if result.err != nil {
		t.Fatalf("admitted batch returned transport error: %v", result.err)
	}
	if len(terminated) != 2 || terminated[1] != continued {
		t.Fatalf("application did not continue in order: %v", terminated)
	}
	if result.applicationErrors[failed] == "" {
		t.Fatalf("missing causal application error: %+v", result.applicationErrors)
	}
	if receipt, ok := tracker.StopSettlementReceipt(failed); !ok || receipt.Phase != api.StopSettlementPhaseFailed {
		t.Fatalf("failed target receipt = %+v present=%v", receipt, ok)
	}
	if receipt, ok := tracker.StopSettlementReceipt(continued); !ok || receipt.Phase != api.StopSettlementPhaseStopRequested {
		t.Fatalf("continued target receipt = %+v present=%v", receipt, ok)
	}
}

func TestStopBatchPreflightRejectsInvalidLaterTargetWithoutMutatingIntentOrReceipts(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const first = `\mcp-local-hub-time-default`
	const missing = `\mcp-local-hub-missing-default`
	started := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(first, 4812, started)
	tracker.MarkSpawned(missing, 4813, started)
	oldIntent := &api.SupervisorIntentFile{IntentGeneration: 1, Daemons: []api.SupervisorDaemon{{TaskName: first, Port: 9242}}}
	commandIntent := &api.SupervisorIntentFile{
		IntentGeneration: 2,
		Daemons:          []api.SupervisorDaemon{{TaskName: first, Port: 9242}},
		Stops: map[string]api.DaemonIntent{
			first:   {Desired: api.IntentDesiredStopped, UpdatedAt: started},
			missing: {Desired: api.IntentDesiredStopped, UpdatedAt: started},
		},
	}
	ctrl := &supervisorController{
		intentCache:  newIntentCache(),
		daemonIntent: newDaemonIntentCache(),
		tracker:      tracker,
		statePath:    statePath,
	}
	ctrl.intentCache.Refresh(oldIntent)
	ctrl.daemonIntent.Refresh(api.UnifiedStopsFile(oldIntent, nil))
	ctrl.smStates.Store(first, api.StRunning)
	ctrl.smStates.Store(missing, api.StRunning)
	command := api.StopBatchCommandV1{
		ProtocolVersion:  1,
		BatchID:          "reject-later-target-without-mutation",
		Targets:          []api.StopBatchTargetV1{{TaskName: first, ExpectedPort: 9242}, {TaskName: missing, ExpectedPort: 9243}},
		IntentGeneration: commandIntent.IntentGeneration,
		SupervisorIntent: commandIntent,
		UnifiedStops:     api.UnifiedStopsFile(commandIntent, nil),
	}
	reply := make(chan stopBatchLoopResult, 1)
	ctrl.handleStopBatchOnLoop(stopBatchLoopRequest{command: command, reply: reply})
	if outcome := <-reply; outcome.err == nil {
		t.Fatal("invalid later target admitted stop batch")
	}
	if got := ctrl.intentCache.Snapshot(); got == nil || got.IntentGeneration != oldIntent.IntentGeneration {
		t.Fatalf("preflight changed intent cache to %+v, want generation %d", got, oldIntent.IntentGeneration)
	}
	if _, pending := tracker.StopSettlementReceipt(first); pending {
		t.Fatal("preflight failure wrote receipt for valid earlier target")
	}
	if _, pending := tracker.StopSettlementReceipt(missing); pending {
		t.Fatal("preflight failure wrote receipt for invalid later target")
	}
}

func TestHandleRespawnIdleDoesNotSpawnBeforeStoppedSettlement(t *testing.T) {
	const taskName = `\mcp-local-hub-time-default`
	const port = 9242
	intent := &api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{
		TaskName: taskName,
		Server:   "time",
		Daemon:   "default",
		Port:     port,
	}}}
	deps, spawnCalls, _ := newRespawnTestDeps(t, intent)

	loop := api.NewEventLoop(8)
	ctrl := &supervisorController{
		intentCache: newIntentCache(),
		eventLoop:   loop,
		tracker:     deps.runtimeTracker,
		stoppedSettlementPortOwnersSnapshot: func(context.Context) (map[int]int, error) {
			return map[int]int{port: 4812}, nil
		},
		stoppedSettlementWait: func(context.Context) error {
			return context.DeadlineExceeded
		},
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	ctrl.tracker.MarkExited(taskName)
	ctrl.smStates.Store(taskName, api.StIdle)
	loop.RegisterHandler(func(event api.LoopEvent) {
		if event.Kind != evControllerBarrier {
			return
		}
		close(event.Body[controllerBarrierResultBodyKey].(chan struct{}))
	})
	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(loopCtx)
	deps.controllerProvider = func() *supervisorController { return ctrl }

	conn := newFakeIPCConn()
	if err := handleRespawn(conn, api.IPCRequest{
		ID:   908,
		Cmd:  "respawn",
		Args: map[string]any{"task_name": taskName, "force": false},
	}, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}
	resp := conn.lastResponse(t)
	if resp.Error == nil || resp.Error.Code != "RESPAWN_STOP_SETTLEMENT_INCOMPLETE" {
		t.Fatalf("respawn response = %+v, want settlement refusal", resp)
	}
	if spawnCalls.Load() != 0 {
		t.Fatalf("spawn calls = %d, want 0 while prior listener remains bound", spawnCalls.Load())
	}
}

func TestAdmitCreateProcessRejectsEveryPendingReceiptUntilCommitRemoval(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	receipt := beginStopSettlementForTest(t, tracker, statePath, taskName)
	ctrl := &supervisorController{tracker: tracker}
	for _, origin := range []string{"idle", "backoff", "quarantine", "exiting"} {
		if err := ctrl.admitCreateProcess(taskName); err == nil {
			t.Fatalf("%s spawn origin admitted while receipt %s remains", origin, receipt.Phase)
		}
	}
	exited, err := tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhaseExitObserved, "", "")
	if err != nil {
		t.Fatalf("exit observed: %v", err)
	}
	released, err := tracker.AdvanceStopSettlement(statePath, exited, api.StopSettlementPhasePortReleased, "", "")
	if err != nil {
		t.Fatalf("port released: %v", err)
	}
	if err := ctrl.admitCreateProcess(taskName); err == nil {
		t.Fatal("spawn admitted before commit-last removal")
	}
	if err := tracker.RemoveStopSettlement(statePath, released); err != nil {
		t.Fatalf("commit removal: %v", err)
	}
	if err := ctrl.admitCreateProcess(taskName); err != nil {
		t.Fatalf("spawn remained fenced after committed removal: %v", err)
	}
}

// TestStopSettlementPortFencePersistsExitReleaseAndRemoval proves that a
// stopped-but-still-bound listener has its own complete receipt lifecycle.  A
// port fence has no PID exit to wait for, but it must still durably pass
// exit_observed -> port_released before the commit-last removal.
func TestStopSettlementPortFencePersistsExitReleaseAndRemoval(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	tracker.MarkExited(taskName)
	receipt := beginStopSettlementForTest(t, tracker, statePath, taskName)
	if receipt.Mode != "port_fence" || receipt.Phase != api.StopSettlementPhaseExitObserved {
		t.Fatalf("port-fence admission = %+v, want exit_observed port_fence", receipt)
	}

	state, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read durable port-fence receipt: %v", err)
	}
	hydrated := NewDaemonRuntimeTracker()
	hydrated.HydrateFromState(state)
	if got, ok := hydrated.StopSettlementReceipt(taskName); !ok || got != receipt || hydrated.StopSettlementIntegrityError() != nil {
		t.Fatalf("hydrated port-fence receipt = %+v present=%v integrity=%v, want exact valid receipt", got, ok, hydrated.StopSettlementIntegrityError())
	}

	released, err := tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhasePortReleased, "", "")
	if err != nil {
		t.Fatalf("persist port release after port-fence exit observation: %v", err)
	}
	if err := tracker.RemoveStopSettlement(statePath, released); err != nil {
		t.Fatalf("commit port-fence receipt removal: %v", err)
	}
	if _, pending := tracker.StopSettlementReceipt(taskName); pending {
		t.Fatal("port-fence receipt remained pending after commit-last removal")
	}
	state, err = api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read terminal port-fence state: %v", err)
	}
	if _, pending := state.StopSettlements[taskName]; pending {
		t.Fatalf("durable port-fence receipt remained after removal: %+v", state.StopSettlements[taskName])
	}
}

func TestStopSettlementPortFenceLiveListenerDoesNoProcessWorkThenRecovers(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	const port = 9242
	started := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, started)
	tracker.MarkExited(taskName)
	intent := &api.SupervisorIntentFile{IntentGeneration: 1, Daemons: []api.SupervisorDaemon{{TaskName: taskName, Port: port}}, Stops: map[string]api.DaemonIntent{
		taskName: {Desired: api.IntentDesiredStopped, UpdatedAt: started},
	}}
	loop := api.NewEventLoop(8)
	ctrlCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctrlCtx)
	terminateCalls := 0
	spawnCalls := 0
	listenerLive := true
	ctrl := &supervisorController{
		ctx:          ctrlCtx,
		intentCache:  newIntentCache(),
		daemonIntent: newDaemonIntentCache(),
		eventLoop:    loop,
		tracker:      tracker,
		statePath:    statePath,
		terminate: func(api.SupervisorDaemon) error {
			terminateCalls++
			return nil
		},
		spawn: func(api.SupervisorDaemon) error {
			spawnCalls++
			return nil
		},
		stoppedSettlementPortOwnersSnapshot: func(context.Context) (map[int]int, error) {
			if listenerLive {
				return map[int]int{port: 7777}, nil
			}
			return map[int]int{}, nil
		},
		stoppedSettlementWait: func(context.Context) error { return context.DeadlineExceeded },
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.daemonIntent.Refresh(api.UnifiedStopsFile(intent, nil))
	ctrl.smStates.Store(taskName, api.StIdle)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	command := api.StopBatchCommandV1{ProtocolVersion: 1, BatchID: "port-fence-live-listener", Targets: []api.StopBatchTargetV1{{TaskName: taskName, ExpectedPort: port}}, IntentGeneration: intent.IntentGeneration, SupervisorIntent: intent, UnifiedStops: api.UnifiedStopsFile(intent, nil)}
	reply := make(chan stopBatchLoopResult, 1)
	ctrl.handleStopBatchOnLoop(stopBatchLoopRequest{command: command, reply: reply})
	if outcome := <-reply; outcome.err != nil {
		t.Fatalf("admit port fence: %v", outcome.err)
	}

	first := ctrl.settleStopBatchTargets(context.Background(), command.Targets)[0]
	if first.State != api.StoppedSettlementFailed || first.Reason != api.StoppedSettlementReasonListenerAlive {
		t.Fatalf("live listener settlement = %+v, want failed/listener_alive", first)
	}
	if terminateCalls != 0 || spawnCalls != 0 {
		t.Fatalf("port fence process operations: terminate=%d spawn=%d, want zero/zero", terminateCalls, spawnCalls)
	}
	failed, pending := tracker.StopSettlementReceipt(taskName)
	if !pending || failed.Mode != "port_fence" || failed.Phase != api.StopSettlementPhaseFailed || failed.FailureClass != api.StopSettlementFailureListenerAlive {
		t.Fatalf("live-listener receipt = %+v pending=%v, want retained typed port-fence failure", failed, pending)
	}
	if _, armed := ctrl.stopSettlementRecoveryArmed.Load(taskName); !armed {
		t.Fatal("live listener failure did not arm one recovery sweep")
	}

	listenerLive = false
	if err := ctrl.enqueueStopSettlementRecovery(context.Background(), taskName); err != nil {
		t.Fatalf("enqueue free-port recovery: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, pending := tracker.StopSettlementReceipt(taskName); !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("free-port FIFO recovery retained receipt")
		}
		time.Sleep(time.Millisecond)
	}
	if terminateCalls != 0 || spawnCalls != 0 {
		t.Fatalf("port-fence recovery process operations: terminate=%d spawn=%d, want zero/zero", terminateCalls, spawnCalls)
	}
}

func TestStopSettlementPersistenceFailureRetainsReceiptAndArmsRecovery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prepare    func(*testing.T, *DaemonRuntimeTracker, string, string) api.StopSettlementReceiptV1
		wantPrefix string
	}{
		{
			name: "advance_port_release",
			prepare: func(t *testing.T, tracker *DaemonRuntimeTracker, statePath, taskName string) api.StopSettlementReceiptV1 {
				t.Helper()
				receipt := beginStopSettlementForTest(t, tracker, statePath, taskName)
				exited, err := tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhaseExitObserved, "", "")
				if err != nil {
					t.Fatalf("record exit: %v", err)
				}
				return exited
			},
			wantPrefix: "persist released stop listener:",
		},
		{
			name: "remove_committed_port_release",
			prepare: func(t *testing.T, tracker *DaemonRuntimeTracker, statePath, taskName string) api.StopSettlementReceiptV1 {
				t.Helper()
				receipt := beginStopSettlementForTest(t, tracker, statePath, taskName)
				exited, err := tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhaseExitObserved, "", "")
				if err != nil {
					t.Fatalf("record exit: %v", err)
				}
				released, err := tracker.AdvanceStopSettlement(statePath, exited, api.StopSettlementPhasePortReleased, "", "")
				if err != nil {
					t.Fatalf("record release: %v", err)
				}
				return released
			},
			wantPrefix: "commit recovered stop settlement:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
			const taskName = `\mcp-local-hub-time-default`
			const port = 9242
			tracker := NewDaemonRuntimeTracker()
			tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
			receipt := tc.prepare(t, tracker, statePath, taskName)
			tracker.MarkExited(taskName)
			if err := os.Remove(statePath); err != nil {
				t.Fatalf("remove state file before persistence failure: %v", err)
			}
			if err := os.Mkdir(statePath, 0o700); err != nil {
				t.Fatalf("replace state file with directory: %v", err)
			}

			ctrlCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			loop := api.NewEventLoop(1)
			loop.RegisterHandler(stopSettlementTestBarrierHandler(t))
			go loop.Run(ctrlCtx)
			ctrl := &supervisorController{
				ctx:                                 ctrlCtx,
				intentCache:                         newIntentCache(),
				eventLoop:                           loop,
				tracker:                             tracker,
				statePath:                           statePath,
				stoppedSettlementPortOwnersSnapshot: func(context.Context) (map[int]int, error) { return map[int]int{}, nil },
			}
			ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{{TaskName: taskName, Port: port}}})
			ctrl.smStates.Store(taskName, api.StIdle)

			got := ctrl.settleStopBatchTargets(context.Background(), []api.StopBatchTargetV1{{TaskName: taskName, ExpectedPort: port}})[0]
			if got.State != api.StoppedSettlementIncomplete || got.Reason != api.StoppedSettlementReasonIdentityUnverified || !strings.HasPrefix(got.Error, tc.wantPrefix) {
				t.Fatalf("persistence failure row = %+v, want incomplete/identity_unverified with %q", got, tc.wantPrefix)
			}
			if current, pending := tracker.StopSettlementReceipt(taskName); !pending || current != receipt {
				t.Fatalf("persistence failure changed in-memory receipt: current=%+v pending=%v want=%+v", current, pending, receipt)
			}
			if _, armed := ctrl.stopSettlementRecoveryArmed.Load(taskName); !armed {
				t.Fatal("persistence failure did not arm recovery")
			}
		})
	}
}

func stopSettlementTestBarrierHandler(t *testing.T) func(api.LoopEvent) {
	t.Helper()
	return func(event api.LoopEvent) {
		if event.Kind == evStopSettlementRecovery {
			return
		}
		if event.Kind != evControllerBarrier {
			t.Errorf("unexpected test event loop event: %+v", event)
			return
		}
		barrier, ok := event.Body[controllerBarrierResultBodyKey].(chan struct{})
		if !ok {
			t.Errorf("invalid test barrier body: %+v", event.Body)
			return
		}
		close(barrier)
	}
}

// TestEnqueueStopSettlementRecoveryBlocksOnFullFIFOAndRecoversOnce locks the
// recovery trigger to the EventLoop's minimum-capacity boundary.  A full FIFO
// must retain its single armed token and block the producer; once drained, it
// delivers exactly one recovery command rather than silently dropping it.
func TestEnqueueStopSettlementRecoveryBlocksOnFullFIFOAndRecoversOnce(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	const port = 1 // beginStopSettlementForTest's canonical descriptor port.
	started := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, started)
	receipt := beginStopSettlementForTest(t, tracker, statePath, taskName)
	if _, err := tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhaseFailed, api.StopSettlementFailureProcessAlive, "first observed process still alive"); err != nil {
		t.Fatalf("seed retryable receipt: %v", err)
	}
	intent := &api.SupervisorIntentFile{IntentGeneration: 1, Daemons: []api.SupervisorDaemon{{TaskName: taskName, Port: port}}, Stops: map[string]api.DaemonIntent{
		taskName: {Desired: api.IntentDesiredStopped, UpdatedAt: started},
	}}
	loop := api.NewEventLoop(1) // EventLoop clamps this to its minimum FIFO capacity.
	loopCtx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	defer func() {
		cancel()
		<-loopDone
	}()
	var recoveryEvents atomic.Int32
	var terminateCalls atomic.Int32
	var spawnCalls atomic.Int32
	recoveryDelivered := make(chan struct{})
	terminated := make(chan struct{})
	ctrl := &supervisorController{
		ctx:          loopCtx,
		intentCache:  newIntentCache(),
		daemonIntent: newDaemonIntentCache(),
		eventLoop:    loop,
		tracker:      tracker,
		statePath:    statePath,
		terminateOutcome: func(_ api.SupervisorDaemon, expected terminationExpectedTuple) terminationOutcome {
			if got := terminateCalls.Add(1); got != 1 {
				t.Errorf("recovery terminate calls = %d, want exactly one", got)
			}
			tracker.MarkExitedIfCurrent(taskName, expected.PIDGeneration)
			close(terminated)
			return terminationOutcome{Kind: terminationOutcomeTerminated, Expected: expected}
		},
		spawn: func(api.SupervisorDaemon) error {
			spawnCalls.Add(1)
			return nil
		},
		stoppedSettlementPortOwnersSnapshot: func(context.Context) (map[int]int, error) {
			return map[int]int{}, nil
		},
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.daemonIntent.Refresh(api.UnifiedStopsFile(intent, nil))
	ctrl.smStates.Store(taskName, api.StRunning)
	loop.RegisterHandler(func(event api.LoopEvent) {
		if event.Kind == evStopSettlementRecovery {
			recoveryEvents.Add(1)
			close(recoveryDelivered)
		}
		ctrl.handleLoopEvent(event)
	})

	for i := 0; i < 16; i++ { // NewEventLoop's documented minimum capacity.
		if !loop.TryPost(api.LoopEvent{Kind: api.SMEvent("test-fifo-fill")}) {
			t.Fatalf("minimum-capacity FIFO filled early at %d", i)
		}
	}
	if loop.TryPost(api.LoopEvent{Kind: api.SMEvent("test-fifo-overflow")}) {
		t.Fatal("minimum-capacity FIFO accepted an overflow event")
	}

	enqueueStarted := make(chan struct{})
	enqueueDone := make(chan error, 1)
	go func() {
		close(enqueueStarted)
		enqueueDone <- ctrl.enqueueStopSettlementRecovery(loopCtx, taskName)
	}()
	<-enqueueStarted
	awaitStopSettlementPredicate(t, "armed recovery token while FIFO is full", func() bool {
		_, armed := ctrl.stopSettlementRecoveryArmed.Load(taskName)
		return armed
	})
	select {
	case err := <-enqueueDone:
		t.Fatalf("full FIFO enqueue returned before drain: %v", err)
	default:
	}
	if err := ctrl.enqueueStopSettlementRecovery(loopCtx, taskName); err != nil {
		t.Fatalf("duplicate enqueue while token armed: %v", err)
	}

	go func() {
		defer close(loopDone)
		loop.Run(loopCtx)
	}()
	awaitStopSettlementSignal(t, "recovery FIFO delivery after drain", recoveryDelivered)
	awaitStopSettlementSignal(t, "recovery termination after FIFO drain", terminated)
	if err := <-enqueueDone; err != nil {
		t.Fatalf("drained FIFO enqueue: %v", err)
	}
	if _, armed := ctrl.stopSettlementRecoveryArmed.Load(taskName); armed {
		t.Fatal("recovery handler did not clear its armed token")
	}
	if got := recoveryEvents.Load(); got != 1 {
		t.Fatalf("recovery FIFO events = %d, want exactly one", got)
	}
	if got := terminateCalls.Load(); got != 1 {
		t.Fatalf("recovery terminate calls = %d, want exactly one", got)
	}
	if got := spawnCalls.Load(); got != 0 {
		t.Fatalf("recovery spawn calls = %d, want zero", got)
	}
}

// TestStopSettlementRecoveryFIFORevalidatesAndRetriesTrackedTerminationOnce
// proves a failed tracked termination cannot devolve into a process_alive
// observation loop: the one listener observation is reached only after the
// FIFO recovery has revalidated the stopped intent and retried termination.
func TestStopSettlementRecoveryFIFORevalidatesAndRetriesTrackedTerminationOnce(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	const port = 9242
	started := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, started)
	intent := &api.SupervisorIntentFile{IntentGeneration: 1, Daemons: []api.SupervisorDaemon{{TaskName: taskName, Port: port}}, Stops: map[string]api.DaemonIntent{
		taskName: {Desired: api.IntentDesiredStopped, UpdatedAt: started},
	}}
	loop := api.NewEventLoop(16)
	loopCtx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	defer func() {
		cancel()
		<-loopDone
	}()
	var terminateCalls atomic.Int32
	var spawnCalls atomic.Int32
	var observations atomic.Int32
	secondTermination := make(chan struct{})
	ctrl := &supervisorController{
		// Keep the initial failure free of a timer; this test drives the
		// production FIFO helper explicitly after establishing the receipt.
		intentCache:  newIntentCache(),
		daemonIntent: newDaemonIntentCache(),
		eventLoop:    loop,
		tracker:      tracker,
		statePath:    statePath,
		terminateOutcome: func(_ api.SupervisorDaemon, expected terminationExpectedTuple) terminationOutcome {
			switch got := terminateCalls.Add(1); got {
			case 1:
				return terminationOutcome{Kind: terminationOutcomeFailed, Expected: expected, Cause: errors.New("first terminate failed")}
			case 2:
				tracker.MarkExitedIfCurrent(taskName, expected.PIDGeneration)
				close(secondTermination)
				return terminationOutcome{Kind: terminationOutcomeTerminated, Expected: expected}
			default:
				t.Errorf("tracked terminate calls = %d, want exactly two", got)
				return terminationOutcome{Kind: terminationOutcomeFailed, Expected: expected, Cause: errors.New("unexpected retry")}
			}
		},
		spawn: func(api.SupervisorDaemon) error {
			spawnCalls.Add(1)
			return nil
		},
		stoppedSettlementPortOwnersSnapshot: func(context.Context) (map[int]int, error) {
			if got := terminateCalls.Load(); got != 2 {
				t.Errorf("listener observation began after %d termination attempts, want retry attempt 2", got)
			}
			observations.Add(1)
			return map[int]int{}, nil
		},
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.daemonIntent.Refresh(api.UnifiedStopsFile(intent, nil))
	ctrl.smStates.Store(taskName, api.StRunning)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	go func() {
		defer close(loopDone)
		loop.Run(loopCtx)
	}()

	command := api.StopBatchCommandV1{ProtocolVersion: 1, BatchID: "tracked-termination-retry", Targets: []api.StopBatchTargetV1{{TaskName: taskName, ExpectedPort: port}}, IntentGeneration: intent.IntentGeneration, SupervisorIntent: intent, UnifiedStops: api.UnifiedStopsFile(intent, nil)}
	reply := make(chan stopBatchLoopResult, 1)
	ctrl.handleStopBatchOnLoop(stopBatchLoopRequest{command: command, reply: reply})
	if result := <-reply; result.err != nil || result.applicationErrors[taskName] == "" {
		t.Fatalf("first tracked termination result = %+v, want admitted causal failure", result)
	}
	failed, pending := tracker.StopSettlementReceipt(taskName)
	if !pending || failed.Phase != api.StopSettlementPhaseFailed || failed.FailureClass != api.StopSettlementFailureTerminationFailed {
		t.Fatalf("first termination receipt = %+v pending=%v, want typed termination_failed", failed, pending)
	}
	if got := terminateCalls.Load(); got != 1 {
		t.Fatalf("initial terminate calls = %d, want one", got)
	}

	ctrl.ctx = loopCtx
	if err := ctrl.enqueueStopSettlementRecovery(loopCtx, taskName); err != nil {
		t.Fatalf("enqueue tracked retry: %v", err)
	}
	awaitStopSettlementSignal(t, "tracked termination retry", secondTermination)
	awaitStopSettlementPredicate(t, "exact exit, free-port settlement, and receipt removal", func() bool {
		_, pending := tracker.StopSettlementReceipt(taskName)
		return !pending
	})
	if got := terminateCalls.Load(); got != 2 {
		t.Fatalf("tracked terminate calls after recovery = %d, want exactly two", got)
	}
	if got := observations.Load(); got != 1 {
		t.Fatalf("process/listener observations = %d, want exactly one only after retry", got)
	}
	if got := spawnCalls.Load(); got != 0 {
		t.Fatalf("tracked recovery spawn calls = %d, want zero", got)
	}
	state, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read converged state: %v", err)
	}
	if _, pending := state.StopSettlements[taskName]; pending {
		t.Fatalf("durable receipt remained after exact exit and free port: %+v", state.StopSettlements[taskName])
	}
}

func awaitStopSettlementSignal(t *testing.T, name string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func awaitStopSettlementPredicate(t *testing.T, name string, predicate func() bool) {
	t.Helper()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for !predicate() {
		select {
		case <-timeout.C:
			t.Fatalf("timed out waiting for %s", name)
		default:
			runtime.Gosched()
		}
	}
}

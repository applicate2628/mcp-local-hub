//go:build windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

func forceStopControllerFixture() (*supervisorController, *daemonCreatorOperation, api.ForceStopCommandV1) {
	started := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-force-fixture-default`, Server: "force-fixture", Daemon: "default"}
	tracker := NewDaemonRuntimeTracker()
	generation := tracker.MarkSpawned(descriptor.TaskName, 4242, started)
	operation := &daemonCreatorOperation{task: descriptor.TaskName, pid: 4242, generation: generation, startedAt: started, owner: &process.ProvisionalProcess{}}
	controller := &supervisorController{tracker: tracker, intentCache: newIntentCache(), daemonIntent: newDaemonIntentCache(), daemonCreators: &daemonCreatorRecords{operations: []*daemonCreatorOperation{operation}}}
	controller.intentCache.Refresh(&api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{descriptor}})
	controller.smStates.Store(descriptor.TaskName, api.StRunning)
	command := api.ForceStopCommandV1{ProtocolVersion: 1, Target: api.ForceStopTargetV1{TaskName: descriptor.TaskName, PID: 4242, StartedAt: started.Format(time.RFC3339Nano), PIDGeneration: generation}}
	return controller, operation, command
}

// Model-only refusal: a zero process owner cannot create native closure proof.
func TestForceStopControllerRefusesBeforeAction(t *testing.T) {
	for _, mode := range []string{"stale-generation", "lost-owner", "unsupported-owner", "already-exiting", "stopped-intent", "other-termination", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			controller, operation, command := forceStopControllerFixture()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			want := api.ForceStopOwnerUnavailable
			switch mode {
			case "stale-generation":
				command.Target.PIDGeneration++
				want = api.ForceStopGenerationChanged
			case "lost-owner":
				operation.owner = nil
			case "unsupported-owner":
				operation.unsupported = true
				want = api.ForceStopUnsupported
			case "already-exiting":
				controller.smStates.Store(command.Target.TaskName, api.StExiting)
				want = api.ForceStopNotRunning
			case "stopped-intent":
				controller.daemonIntent.Refresh(&api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{command.Target.TaskName: {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonUserDisabled, UpdatedAt: time.Now().UTC()}}})
				want = api.ForceStopIntentStopped
			case "other-termination":
				operation.otherTerminationSelected = true
				want = api.ForceStopControllerBusy
			case "canceled":
				cancel()
				want = api.ForceStopControllerBusy
			}
			result := controller.forceStopOnLoop(ctx, command)
			if result.State != api.ForceStopRefused || result.Reason != want || operation.forceStopRequested || operation.terminalDone != nil {
				t.Fatalf("refusal entered native terminal operation: result=%+v operation=%+v", result, operation)
			}
		})
	}
}

func TestForceStopDuplicateRetainsIncompleteOriginalAfterSuccessor(t *testing.T) {
	controller, operation, command := forceStopControllerFixture()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first := controller.forceStopOnLoop(ctx, command)
	if first.State != api.ForceStopIncomplete || first.Error == "" || operation.terminalDone == nil {
		t.Fatalf("zero owner fabricated closure: %+v", first)
	}
	done := operation.terminalDone
	controller.tracker.MarkSpawned(command.Target.TaskName, command.Target.PID, time.Now().UTC())
	second := controller.forceStopOnLoop(ctx, command)
	if first != second || operation.terminalDone != done {
		t.Fatalf("retry changed the original result or retargeted successor: first=%+v second=%+v", first, second)
	}
}

// Native acceptance, to be run only in the admitted existing Windows fixture.
// It reuses the production creator and the existing before-exit barrier. A
// native receipt must exist before the sole real waiter publishes; duplicate
// force commands return that same generation and never create a second exit.
func TestProductionForceStopOriginalCreatorClosesBeforeSingleExit(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	events, err := api.OpenSupervisorEventLog(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	ctx, cancel := context.WithCancel(context.Background())
	workers := &fixtureSpawnWorkers{}
	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var publications atomic.Int32
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	workers.beforeEmit = func() {
		if publications.Add(1) == 1 {
			close(held)
		}
		<-release
	}
	defer func() { unblock(); settleFixtureSpawnWorkers(t, workers, cancel) }()
	records, tracker := &daemonCreatorRecords{}, NewDaemonRuntimeTracker()
	crashCh := make(chan crashEvent, 1)
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := api.SupervisorDaemon{TaskName: `\mcp-local-hub-force-fixture-default`, Server: "force-fixture", Daemon: "default", Command: command,
		// Reuse the existing admitted stateless sleeper. Its TestMain dispatch
		// precedes package-root allocation, so forced exit cannot leak a root.
		Args: []string{"-test.run=^TestProductionTerminateFn_HelperSleep$"}, Env: map[string]string{"MCPHUB_PRODUCTION_TERMINATE_HELPER": "1"}}
	spawn := makeProductionSpawnFnWithCreatorRecords(events, tracker, "", nil, "", crashCh, ctx, nil, false, workers, records)
	if err := spawn(descriptor); err != nil {
		t.Fatal(err)
	}
	entry, found := tracker.Get(descriptor.TaskName)
	if !found || entry.CurrentPID <= 0 {
		t.Fatal("fixture original creator not running")
	}
	controller := &supervisorController{tracker: tracker, intentCache: newIntentCache(), daemonIntent: newDaemonIntentCache(), daemonCreators: records}
	controller.intentCache.Refresh(&api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{descriptor}})
	controller.smStates.Store(descriptor.TaskName, api.StRunning)
	request := api.ForceStopCommandV1{ProtocolVersion: 1, Target: api.ForceStopTargetV1{TaskName: descriptor.TaskName, PID: entry.CurrentPID, StartedAt: entry.StartedAt.UTC().Format(time.RFC3339Nano), PIDGeneration: entry.PIDGeneration}}
	forceCtx, stop := context.WithTimeout(ctx, daemonCreatorTerminalObservationBudget)
	defer stop()
	result := controller.forceStopOnLoop(forceCtx, request)
	if result.State != api.ForceStopClosed || !result.RootExited || !result.TreeClosed || !result.HandlesClosed || result.Action != "request-succeeded" {
		t.Fatalf("native original closure not established: %+v", result)
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("original waiter did not reach post-closure barrier")
	}
	select {
	case event := <-crashCh:
		t.Fatalf("exit published before barrier release: %+v", event)
	default:
	}
	if repeated := controller.forceStopOnLoop(forceCtx, request); repeated != result {
		t.Fatalf("duplicate changed receipt: %+v", repeated)
	}
	unblock()
	select {
	case event := <-crashCh:
		if event.PID != request.Target.PID || event.PIDGeneration != request.Target.PIDGeneration || (event.ExitCode == 0 && event.WaitErr == nil) {
			t.Fatalf("force-stop lost its original non-clean exit: %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("original force-stop exit was lost")
	}
	settleFixtureSpawnWorkers(t, workers, cancel)
	if publications.Load() != 1 {
		t.Fatalf("original exit publication attempts=%d, want one", publications.Load())
	}
	select {
	case event := <-crashCh:
		t.Fatalf("duplicate original exit: %+v", event)
	default:
	}
}

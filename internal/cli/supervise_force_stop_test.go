package cli

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// No fake can mint a native closure. This barrier checks the orchestration
// counterexample: a natural waiter must join the admitted force operation,
// retain its actual failure, and never enter a second native terminal call.
func TestDaemonCreatorForceCompletionJoinsWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		records := &daemonCreatorRecords{}
		operation, err := records.begin("force-completion-fixture")
		if err != nil {
			t.Fatal(err)
		}
		owner := &process.ProvisionalProcess{}
		if err := records.retain(operation, owner); err != nil {
			t.Fatal(err)
		}
		records.endStart(operation, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		forceDone, waiterDone := make(chan error, 1), make(chan error, 1)
		cause := errors.New("original terminal failure")
		var actions atomic.Int32
		go func() {
			_, err := records.completeTerminal(ctx, operation, func(context.Context) (*process.CreatorTerminalResult, error) {
				actions.Add(1)
				close(entered)
				<-release
				return nil, cause
			})
			forceDone <- err
		}()
		<-entered
		go func() {
			_, err := records.completeTerminal(ctx, operation, func(context.Context) (*process.CreatorTerminalResult, error) {
				actions.Add(1)
				return nil, errors.New("duplicate terminal action")
			})
			waiterDone <- err
		}()
		synctest.Wait()
		select {
		case err := <-waiterDone:
			t.Fatalf("waiter bypassed force completion: %v", err)
		default:
		}
		unblock()
		if !errors.Is(<-forceDone, cause) || !errors.Is(<-waiterDone, cause) || actions.Load() != 1 {
			t.Fatal("original result was lost or native terminal action repeated")
		}
		if operation.owner != owner || operation.terminalErr != cause || operation.complete {
			t.Fatal("incomplete force action lost custody or became complete")
		}
		if err := records.publishAfterCreatorClosure(operation, func() { t.Error("incomplete force action published terminal exit") }); !errors.Is(err, cause) {
			t.Fatalf("publication gate lost cause: %v", err)
		}
	})
}

func TestDaemonCreatorForceCanceledJoinKeepsOriginalResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		records := &daemonCreatorRecords{}
		operation := &daemonCreatorOperation{owner: &process.ProvisionalProcess{}}
		entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		cause := errors.New("original close error")
		go func() {
			defer close(finished)
			records.completeTerminal(context.Background(), operation, func(context.Context) (*process.CreatorTerminalResult, error) {
				close(entered)
				<-release
				return nil, cause
			})
		}()
		<-entered
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := records.completeTerminal(ctx, operation, func(context.Context) (*process.CreatorTerminalResult, error) {
			t.Fatal("canceled join replayed native action")
			return nil, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		unblock()
		<-finished
		_, err = records.completeTerminal(context.Background(), operation, func(context.Context) (*process.CreatorTerminalResult, error) {
			t.Fatal("completed operation replayed")
			return nil, nil
		})
		if !errors.Is(err, cause) || operation.owner == nil {
			t.Fatal("cancellation overwrote the owner/result")
		}
	})
}

func TestForceStopGuardSelectsOneTerminationPath(t *testing.T) {
	started := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	target := api.ForceStopTargetV1{TaskName: `\mcp-local-hub-force-fixture-default`, PID: 4242, PIDGeneration: 7, StartedAt: started.Format(time.RFC3339Nano)}
	for _, forceFirst := range []bool{false, true} {
		operation := &daemonCreatorOperation{task: target.TaskName, pid: target.PID, generation: target.PIDGeneration, startedAt: started, forceStopRequested: forceFirst}
		records := &daemonCreatorRecords{operations: []*daemonCreatorOperation{operation}}
		calls := 0
		guard := guardForceStopTermination(records, func(api.SupervisorDaemon, terminationExpectedTuple) terminationOutcome {
			calls++
			return terminationOutcome{}
		})
		expected := terminationExpectedTuple{CanonicalTaskName: target.TaskName, PID: target.PID, PIDGeneration: target.PIDGeneration, StartedAt: target.StartedAt, Valid: true}
		outcome := guard(api.SupervisorDaemon{TaskName: target.TaskName}, expected)
		if forceFirst {
			if calls != 0 || outcome.Kind != terminationOutcomeFailed || outcome.Cause == nil {
				t.Fatalf("duplicate termination: calls=%d result=%+v", calls, outcome)
			}
			// Tracker retirement must not expose the old startup-PID fallback.
			expected.Valid = false
			guard(api.SupervisorDaemon{TaskName: target.TaskName}, expected)
			if calls != 0 {
				t.Fatal("retired tracker re-enabled numerical fallback")
			}
		} else if calls != 1 || !operation.otherTerminationSelected {
			t.Fatal("ordinary termination did not reserve its selection")
		}
	}
}

func TestForceStopSelectionNeverSubstitutesReusedPID(t *testing.T) {
	started := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	old := &daemonCreatorOperation{task: `\mcp-local-hub-force-fixture-default`, pid: 4242, generation: 7, startedAt: started, forceStopRequested: true}
	next := &daemonCreatorOperation{task: old.task, pid: old.pid, generation: 8, startedAt: started.Add(time.Second)}
	records := &daemonCreatorRecords{operations: []*daemonCreatorOperation{old, next}}
	target := api.ForceStopTargetV1{TaskName: old.task, PID: old.pid, PIDGeneration: old.generation, StartedAt: old.startedAt.Format(time.RFC3339Nano)}
	if got, repeated := records.forceStopOperation(target); got != old || !repeated {
		t.Fatal("retry selected replacement")
	}
	target.PIDGeneration = next.generation
	if got, _ := records.forceStopOperation(target); got != nil {
		t.Fatal("mixed generation tuple selected an owner")
	}
}

func TestForceStopUnsupportedPlatformRefusesBeforeOwnerAction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unsupported-platform contract")
	}
	command := api.ForceStopCommandV1{ProtocolVersion: 1, Target: api.ForceStopTargetV1{TaskName: `\mcp-local-hub-force-fixture-default`, PID: 4242, PIDGeneration: 7, StartedAt: "2026-11-24T00:00:00Z"}}
	result := (&supervisorController{}).forceStopOnLoop(context.Background(), command)
	if result.State != api.ForceStopRefused || result.Reason != api.ForceStopUnsupported || result.RootExited || result.TreeClosed {
		t.Fatalf("unsupported platform fabricated a terminal result: %+v", result)
	}
}

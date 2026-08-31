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

// Test-only compatibility names keep existing table fixtures readable while
// production termination contracts remain package-private.
type TerminationExpectedTuple = terminationExpectedTuple
type TerminationOutcome = terminationOutcome

type fakeHeldPIDGeneration struct {
	pid       int
	verifyFn  func(process.PIDIdentityProof) error
	terminate func() (bool, error)
	closeErr  error
}

func (h *fakeHeldPIDGeneration) PID() int { return h.pid }
func (h *fakeHeldPIDGeneration) VerifyIdentity(proof process.PIDIdentityProof) error {
	if h.verifyFn == nil {
		return nil
	}
	return h.verifyFn(proof)
}
func (h *fakeHeldPIDGeneration) Terminate() (bool, error) {
	if h.terminate == nil {
		return true, nil
	}
	return h.terminate()
}
func (h *fakeHeldPIDGeneration) Close() error { return h.closeErr }

func TestTerminationOutcomeAllowsSyntheticOnlyForVerifiedTerminalOutcome(t *testing.T) {
	valid := TerminationExpectedTuple{CanonicalTaskName: `\mcp-local-hub-time-default`, PID: 4812, StartedAt: "2026-08-31T12:00:00Z", PIDGeneration: 1, Valid: true}
	for _, tc := range []struct {
		name string
		out  TerminationOutcome
		want bool
	}{
		{"already exited exact", TerminationOutcome{Kind: terminationOutcomeAlreadyExited, Expected: valid}, true},
		{"terminated exact", TerminationOutcome{Kind: terminationOutcomeTerminated, Expected: valid}, true},
		{"committed without observed exit", TerminationOutcome{Kind: terminationOutcomeCommitted, Expected: valid}, false},
		{"identity mismatch", TerminationOutcome{Kind: terminationOutcomeIdentityMismatch, Expected: valid}, false},
		{"terminal kind without tuple", TerminationOutcome{Kind: terminationOutcomeTerminated}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.out.AllowsSynthetic(); got != tc.want {
				t.Fatalf("AllowsSynthetic() = %v, want %v for %+v", got, tc.want, tc.out)
			}
		})
	}
}

func TestLegacyTerminationNilIsUncertainAndCannotSynthesize(t *testing.T) {
	expected := TerminationExpectedTuple{CanonicalTaskName: `\mcp-local-hub-time-default`, PID: 4812, StartedAt: "2026-08-31T12:00:00Z", PIDGeneration: 1, Valid: true}
	got := terminationOutcomeFromLegacy(expected, nil)
	if got.Kind != terminationOutcomeUncertain || got.Expected.Valid {
		t.Fatalf("legacy nil outcome = %+v, want uncertain with invalid exact tuple", got)
	}
	if got.AllowsSynthetic() {
		t.Fatal("legacy nil outcome authorized a synthetic exit")
	}
}

func TestControllerSynthesizesExactForeignExitOnlyAfterReceiptExitObserved(t *testing.T) {
	const taskName = `\mcp-local-hub-time-default`
	startedAt := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	events, err := api.OpenSupervisorEventLog(filepath.Join(filepath.Dir(statePath), "supervisor-events.log"))
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer events.Close()
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, startedAt)
	_ = beginStopSettlementForTest(t, tracker, statePath, taskName)

	loop := api.NewEventLoop(8)
	posted := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(ev api.LoopEvent) { posted <- ev })
	ctrl := &supervisorController{
		eventLoop: loop,
		tracker:   tracker,
		statePath: statePath,
		terminateOutcome: func(_ api.SupervisorDaemon, expected TerminationExpectedTuple) TerminationOutcome {
			return TerminationOutcome{Kind: terminationOutcomeTerminated, Expected: expected}
		},
	}

	if err := ctrl.executeSideEffect("issue terminate", api.StExiting, &api.SupervisorDaemon{TaskName: taskName}, api.LoopEvent{Kind: api.EvManualRestart, TaskName: taskName}); err != nil {
		t.Fatalf("terminate side effect: %v", err)
	}
	advanced, present := tracker.StopSettlementReceipt(taskName)
	if !present || advanced.Phase != api.StopSettlementPhaseStopRequested {
		t.Fatalf("receipt before typed terminal event = %+v present=%v, want stop_requested", advanced, present)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)
	select {
	case ev := <-posted:
		if ev.Kind != evForeignTerminationExit || ev.TaskName != taskName {
			t.Fatalf("synthetic event = %+v, want exact foreign terminal event", ev)
		}
		if ev.Body["pid"] != 4812 || ev.Body["pid_generation"] != 1 || ev.Body["started_at"] != startedAt.Format(time.RFC3339Nano) {
			t.Fatalf("synthetic event tuple = %#v, want exact receipt tuple", ev.Body)
		}
		ctrl.handleLoopEvent(ev)
	case <-time.After(time.Second):
		t.Fatal("controller did not post the verified foreign synthetic exit")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		advanced, present = tracker.StopSettlementReceipt(taskName)
		if present && advanced.Phase == api.StopSettlementPhaseExitObserved {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !present || advanced.Phase != api.StopSettlementPhaseExitObserved {
		t.Fatalf("receipt after typed foreign terminal event = %+v present=%v, want exit_observed", advanced, present)
	}
}

func TestWindowsHeldTerminationKeepsCloseFailureAsCleanupEvidence(t *testing.T) {
	const taskName = `\mcp-local-hub-time-default`
	statePath := filepath.Join(t.TempDir(), "supervisor-state.json")
	events, err := api.OpenSupervisorEventLog(filepath.Join(filepath.Dir(statePath), "supervisor-events.log"))
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer events.Close()
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	expected := terminationExpectedTupleForTask(tracker, taskName)
	previousHold := productionHoldPIDForTerminationFn
	productionHoldPIDForTerminationFn = func(pid int) (process.HeldPIDGeneration, error) {
		return &fakeHeldPIDGeneration{pid: pid, terminate: func() (bool, error) { return true, nil }, closeErr: errors.New("close evidence")}, nil
	}
	t.Cleanup(func() { productionHoldPIDForTerminationFn = previousHold })

	outcome := terminateProductionWindows(events, tracker, statePath, api.SupervisorDaemon{TaskName: taskName, Command: "mcphub.exe"}, expected, process.PIDIdentityProof{PID: 4812, ExecutablePath: "mcphub.exe", StartedAt: expected.StartedAt})
	if outcome.Kind != terminationOutcomeTerminated {
		t.Fatalf("outcome = %+v, want terminated despite handle close failure", outcome)
	}
	if outcome.CleanupError == nil || outcome.CleanupError.Error() != "close evidence" {
		t.Fatalf("cleanup evidence = %v, want close error", outcome.CleanupError)
	}
}

func TestWindowsHeldTerminationWaitFailureIsCommittedNotSynthetic(t *testing.T) {
	const taskName = `\mcp-local-hub-time-default`
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	events, err := api.OpenSupervisorEventLog(filepath.Join(t.TempDir(), "supervisor-events.log"))
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer events.Close()
	expected := terminationExpectedTupleForTask(tracker, taskName)
	previousHold := productionHoldPIDForTerminationFn
	productionHoldPIDForTerminationFn = func(pid int) (process.HeldPIDGeneration, error) {
		return &fakeHeldPIDGeneration{pid: pid, terminate: func() (bool, error) { return true, errors.New("wait timeout") }}, nil
	}
	t.Cleanup(func() { productionHoldPIDForTerminationFn = previousHold })

	outcome := terminateProductionWindows(events, tracker, "", api.SupervisorDaemon{TaskName: taskName, Command: "mcphub.exe"}, expected, process.PIDIdentityProof{PID: 4812, ExecutablePath: "mcphub.exe", StartedAt: expected.StartedAt})
	if outcome.Kind != terminationOutcomeCommitted || outcome.AllowsSynthetic() {
		t.Fatalf("outcome = %+v, want committed with no synthetic authorization", outcome)
	}
}

// internal/api/supervisor_state_machine_test.go
package api

import (
	"math/rand"
	"testing"
)

// State + Event values mirror spec §"Restart policy state machine" Q4 v13.
type SMState string

const (
	StIdle           SMState = "idle"
	StSpawning       SMState = "spawning"
	StRunning        SMState = "running"
	StExiting        SMState = "exiting"
	StBackoffWaiting SMState = "backoff-waiting"
	StQuarantined    SMState = "quarantined"
)

type SMEvent string

const (
	EvStart             SMEvent = "start"
	EvHealthOK          SMEvent = "health-ok"
	EvChildExit         SMEvent = "child-exit"
	EvTimerDue          SMEvent = "timer-due"
	EvIntentUpdate      SMEvent = "intent-update"
	EvManualRestart     SMEvent = "manual-restart"
	EvRequestGraceful   SMEvent = "request-graceful-exit"
	EvQuiesceComplete   SMEvent = "quiesce-complete"
	EvSupervisorRestart SMEvent = "supervisor-restart"
)

// SMContext is the per-daemon context the transition function reads.
type SMContext struct {
	IntentDesired      string // "running" | "stopped"
	IntentIsActiveStop bool   // IsActiveStop(now)
	Failures           int    // count in 30-min sliding window
	QueuedAction       string // "" | "respawn" | "none"
	GracefulInProgress bool   // supervisor-wide flag
}

// transition is the pure function the engine will implement. Returns
// (newState, sideEffect, persistBefore). For the property test, we
// drive transition against the spec table directly.
// Stub returns ok=false until Task 4.1 implements Transition().
func transition(state SMState, ev SMEvent, ctx SMContext) (newState SMState, side string, persistBefore bool, ok bool) {
	return state, "", false, false
}

// TestStateMachineInvariants_GracefulExitTerminates verifies that once
// graceful_exit_in_progress is set + request-graceful-exit fanned out,
// every reachable per-daemon state eventually reaches idle (or
// quarantined for daemons already there). No daemon stays in spawning,
// running, exiting, or backoff-waiting indefinitely.
func TestStateMachineInvariants_GracefulExitTerminates(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 1000; trial++ {
		initial := randomState(rng)
		ctx := SMContext{
			IntentDesired:      randomIntent(rng),
			IntentIsActiveStop: rng.Intn(2) == 0,
			Failures:           rng.Intn(15),
			QueuedAction:       randomQueuedAction(rng),
			GracefulInProgress: true,
		}
		final := simulateGracefulExit(initial, ctx)
		switch final {
		case StIdle, StQuarantined:
			// OK — terminal under graceful exit.
		default:
			t.Fatalf("trial %d: initial=%s final=%s — daemon stuck non-terminal under graceful exit", trial, initial, final)
		}
	}
}

// simulateGracefulExit runs request-graceful-exit + drives the state
// machine forward until quiescence. Stub implementation; will pass
// after Task 4.1 implements Transition().
func simulateGracefulExit(s SMState, ctx SMContext) SMState {
	// Fire request-graceful-exit once.
	s, _, _, _ = transition(s, EvRequestGraceful, ctx)
	// Drain child-exit until terminal (idle/quarantined).
	for i := 0; i < 100; i++ {
		switch s {
		case StIdle, StQuarantined:
			return s
		case StExiting:
			s, _, _, _ = transition(s, EvChildExit, ctx)
		case StSpawning, StRunning:
			return s // surface as failure to the test
		case StBackoffWaiting:
			s, _, _, _ = transition(s, EvTimerDue, ctx)
		default:
			return s
		}
	}
	return s
}

// TestStateMachineInvariants_NoDeadlockUnderIntentRace verifies that
// intent_update{running} arriving AFTER request-graceful-exit fan-out
// does NOT re-arm queued_action=respawn (the v11 P2 fix verifies
// graceful_exit_in_progress flag suppresses the respawn re-arm row).
func TestStateMachineInvariants_NoDeadlockUnderIntentRace(t *testing.T) {
	ctx := SMContext{
		IntentDesired:      "running",
		IntentIsActiveStop: false,
		GracefulInProgress: true,
		QueuedAction:       "none",
	}
	// Start in exiting (drain in progress, no queued action).
	s := StExiting
	// intent_update{running} during drain — must NOT mutate queued_action.
	s2, side, _, _ := transition(s, EvIntentUpdate, ctx)
	if side == "set queued_action=respawn" {
		t.Fatalf("intent_update{running} during graceful exit must NOT set queued_action=respawn; got side=%q", side)
	}
	if s2 != StExiting {
		t.Fatalf("intent_update{running} during graceful exit must keep state=exiting; got %s", s2)
	}
}

func randomState(rng *rand.Rand) SMState {
	all := []SMState{StIdle, StSpawning, StRunning, StExiting, StBackoffWaiting, StQuarantined}
	return all[rng.Intn(len(all))]
}

func randomIntent(rng *rand.Rand) string {
	if rng.Intn(2) == 0 {
		return "running"
	}
	return "stopped"
}

func randomQueuedAction(rng *rand.Rand) string {
	all := []string{"", "respawn", "none"}
	return all[rng.Intn(len(all))]
}

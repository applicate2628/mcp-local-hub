// internal/api/supervisor_state_machine_test.go
package api

import (
	"math/rand"
	"testing"
)

// SMState, SMEvent, SMContext and the St*/Ev* constants live in
// supervisor_state_machine.go (production file). The test file only
// holds the property-test scaffolding plus this thin delegator so the
// tests stay agnostic of whether the implementation is exported or
// not.

// transition is the package-local helper used by the property tests.
// Delegates to the exported Transition function (Task 4.1 impl).
func transition(state SMState, ev SMEvent, ctx SMContext) (SMState, string, bool, bool) {
	return Transition(state, ev, ctx)
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

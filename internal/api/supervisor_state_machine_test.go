// internal/api/supervisor_state_machine_test.go
package api

import (
	"math/rand"
	"strings"
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

// TestStateMachineInvariant_TimerDueSpawnsOnlyFromBackoffWaiting is the Conc-F4
// (PR #268 deep-sec P3) guard. The supervisor's backoff timer goroutine posts
// EvTimerDue from OFF the event loop; the loop's api.Transition is the single
// authoritative gate. The invariant — EvTimerDue may issue a spawn (a
// create-process side effect) ONLY from StBackoffWaiting — is what makes a stale
// timer fire harmless from every other state (the goroutine's own best-effort
// smStates re-check is a perf early-out + observability emit, NOT the safety
// property). This test goes RED the instant a future SM edit adds an EvTimerDue
// spawn row to any other state — the exact change that would turn the latent
// race live. Per the PR #268 principle: off-loop posters detect and post; the
// loop's Transition is the only gate.
//
// The spawn signal is the create-process side effect, NOT newState==StSpawning:
// a state with no EvTimerDue row falls through to the unmatched return (state,
// "", false, false), so a StSpawning start trivially "stays" StSpawning via a
// no-op drop — which is not a spawn.
func TestStateMachineInvariant_TimerDueSpawnsOnlyFromBackoffWaiting(t *testing.T) {
	spawns := func(side string, matched bool) bool {
		return matched && strings.Contains(side, "create-process")
	}
	// A context that WOULD spawn if the state allowed it (running intent, no
	// queued stop, well under the quarantine threshold). Proving non-spawn under
	// THIS context shows the STATE blocks the spawn, not the context.
	spawnEligible := SMContext{
		IntentDesired:      "running",
		IntentIsActiveStop: false,
		Failures:           0,
		QueuedAction:       "",
		GracefulInProgress: false,
	}
	for _, st := range []SMState{StIdle, StSpawning, StRunning, StExiting, StQuarantined} {
		_, side, _, matched := transition(st, EvTimerDue, spawnEligible)
		if spawns(side, matched) {
			t.Errorf("EvTimerDue from %s issued a spawn (side=%q) — only StBackoffWaiting may spawn on a timer; a stale off-loop timer post must never spawn from %s", st, side, st)
		}
	}
	// StBackoffWaiting is the ONE state that may spawn on EvTimerDue.
	if _, side, _, matched := transition(StBackoffWaiting, EvTimerDue, spawnEligible); !spawns(side, matched) {
		t.Errorf("EvTimerDue from StBackoffWaiting (spawn-eligible) did not spawn (side=%q matched=%v); want a create-process transition", side, matched)
	}
	// Even from StBackoffWaiting the intent re-check still suppresses the spawn
	// when a stop is queued / intent is stopped, or failures hit the quarantine
	// threshold — so "spawn from backoff" is itself conditional, never blind.
	stopQueued := spawnEligible
	stopQueued.QueuedAction = "stop"
	if _, side, _, matched := transition(StBackoffWaiting, EvTimerDue, stopQueued); spawns(side, matched) {
		t.Errorf("EvTimerDue from StBackoffWaiting with queued stop spawned (side=%q); want suppressed (StIdle)", side)
	}
	atThreshold := spawnEligible
	atThreshold.Failures = 10
	if _, side, _, matched := transition(StBackoffWaiting, EvTimerDue, atThreshold); spawns(side, matched) {
		t.Errorf("EvTimerDue from StBackoffWaiting at Failures=10 spawned (side=%q); want StQuarantined", side)
	}
}

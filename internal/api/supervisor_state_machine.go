// internal/api/supervisor_state_machine.go
package api

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

// Transition implements the spec §"Restart policy state machine"
// transition table (v13, ~36 rows). Pure function — caller is
// responsible for persisting supervisor-state.json BEFORE applying
// side effect per the "Persist when" column.
//
// Returns: newState, side-effect description (for logging), whether
// the caller must persist before performing the side effect, whether
// the transition matched any row (false = drop event with log).
//
// INVARIANT (Conc-F4, PR #268 deep-sec): EvTimerDue may issue a spawn
// (create-process) ONLY from StBackoffWaiting. This is the single gate that
// makes a stale post from the off-loop backoff-timer goroutine harmless from
// every other state. Off-loop posters DETECT and POST; this table is the only
// authoritative gate — they must not pre-judge SM state. Adding an EvTimerDue
// spawn row to any other state would turn that latent race live; the guard is
// TestStateMachineInvariant_TimerDueSpawnsOnlyFromBackoffWaiting, which goes RED
// on exactly that edit.
func Transition(state SMState, ev SMEvent, ctx SMContext) (newState SMState, side string, persistBefore bool, matched bool) {
	switch state {
	case StIdle:
		switch ev {
		case EvStart:
			if ctx.IntentDesired == "running" && !ctx.IntentIsActiveStop {
				return StSpawning, "bump pid_generation, create-process", true, true
			}
			return StIdle, "intent suppresses spawn", false, true
		case EvIntentUpdate:
			if ctx.IntentDesired == "stopped" {
				return StIdle, "", false, true
			}
			if ctx.IntentDesired == "running" && !ctx.IntentIsActiveStop {
				return StSpawning, "bump pid_generation, create-process", true, true
			}
		case EvManualRestart:
			if ctx.IntentDesired == "running" && !ctx.IntentIsActiveStop {
				return StSpawning, "bump pid_generation, create-process", true, true
			}
			return StIdle, "RESTART_REFUSED_INTENT_STOPPED", false, true
		case EvRequestGraceful:
			return StIdle, "no-op (already drained)", false, true
		case EvQuiesceComplete:
			return StIdle, "clear quiesce_in_progress", false, true
		}
	case StSpawning:
		switch ev {
		case EvHealthOK:
			// If a user stop was queued during spawn (via the
			// StSpawning + EvIntentUpdate(stopped) transition below),
			// route directly to StExiting now that we have a live
			// child to terminate. Closes bot finding B on PR #236
			// 1c0ea09: previously a stop request arriving while a
			// daemon was spawning was silently dropped because
			// StSpawning had no EvIntentUpdate transition.
			if ctx.QueuedAction == "stop" {
				return StExiting, "issue terminate, queued_action=none", true, true
			}
			return StRunning, "", false, true
		case EvChildExit:
			// If user stop was queued during spawn OR current intent
			// is stopped, route to StIdle instead of backoff. The
			// synthetic EvChildExit from a pre-child spawn error also
			// flows through here; if the user queued a stop while the
			// failed spawn was in flight, the daemon stays stopped
			// instead of entering the backoff retry loop. Closes bot
			// finding B on PR #236 1c0ea09 (pre-child retry overriding
			// queued user stop).
			if ctx.QueuedAction == "stop" || ctx.IntentDesired == "stopped" {
				return StIdle, "clear queued_action", true, true
			}
			return StBackoffWaiting, "arm timer at backoff(failures)", true, true
		case EvIntentUpdate:
			// Record stop-pending in queued_action so EvHealthOK /
			// EvChildExit transitions above can honor it. Self-loop
			// (newState == StSpawning) is intentional: the daemon is
			// still spawning, no SM-level transition is appropriate
			// yet. The controller's queued_action auto-clear is
			// bounded to "transition OUT of StExiting" so this set is
			// preserved across the self-loop. Closes bot finding B
			// on PR #236 1c0ea09.
			//
			// If intent flips back to "running" before spawn completes,
			// CLEAR any previously-queued stop so the next EvHealthOK
			// does not route to StExiting against the operator's
			// latest intent. Closes bot finding on PR #236 db988e0
			// (intent flip stopped -> running during StSpawning left
			// queued_action=stop stale).
			if ctx.IntentDesired == "stopped" {
				return StSpawning, "set queued_action=stop", false, true
			}
			return StSpawning, "clear queued_action", false, true
		case EvRequestGraceful:
			return StExiting, "issue terminate, queued_action=none", true, true
		}
	case StRunning:
		switch ev {
		case EvChildExit:
			return StBackoffWaiting, "arm timer", true, true
		case EvIntentUpdate:
			if ctx.IntentDesired == "stopped" {
				return StExiting, "issue terminate, queued_action=none", true, true
			}
		case EvManualRestart:
			return StExiting, "issue terminate, queued_action=respawn", true, true
		case EvRequestGraceful:
			return StExiting, "issue terminate, queued_action=none", true, true
		}
	case StExiting:
		switch ev {
		case EvChildExit:
			// graceful_exit_in_progress suppresses any respawn from a stale
			// queued_action — drain wins. Spec §"queued_action preservation
			// across supervisor exit" v13.
			if ctx.GracefulInProgress {
				return StIdle, "drain wins (graceful-exit suppresses respawn)", false, true
			}
			if ctx.QueuedAction == "respawn" {
				return StSpawning, "bump pid_generation, create-process", true, true
			}
			return StIdle, "", false, true
		case EvManualRestart:
			// graceful_exit_in_progress suppresses re-arm of queued_action.
			if ctx.GracefulInProgress {
				return StExiting, "deferred via graceful-exit", false, true
			}
			return StExiting, "coalesce — set queued_action=respawn", true, true
		case EvIntentUpdate:
			if ctx.GracefulInProgress && ctx.IntentDesired == "running" {
				return StExiting, "deferred via graceful-exit", false, true
			}
			if ctx.IntentDesired == "stopped" {
				return StExiting, "clear queued_action", true, true
			}
			if ctx.IntentDesired == "running" {
				return StExiting, "set queued_action=respawn", true, true
			}
		case EvRequestGraceful:
			return StExiting, "coalesce, set queued_action=none (drain wins)", true, true
		case EvTimerDue:
			return StExiting, "log + ignore", false, true
		}
	case StBackoffWaiting:
		switch ev {
		case EvTimerDue:
			// Intent re-check before respawn. If user stop was queued
			// during spawn or current intent is stopped, do NOT
			// respawn - route to StIdle. Closes bot finding B on PR
			// #236 1c0ea09 (timer fired blind to current intent state,
			// could respawn even after user stop was queued during
			// spawn or arrived during the backoff window).
			if ctx.QueuedAction == "stop" || ctx.IntentDesired == "stopped" {
				return StIdle, "cancel timer, clear queued_action", true, true
			}
			if ctx.Failures < 10 {
				return StSpawning, "bump pid_generation, create-process", true, true
			}
			return StQuarantined, "clear timer", true, true
		case EvIntentUpdate:
			if ctx.IntentDesired == "stopped" {
				return StIdle, "cancel timer", true, true
			}
			if ctx.IntentDesired == "running" {
				return StSpawning, "cancel timer, bump pid_generation, create-process", true, true
			}
		case EvManualRestart:
			return StSpawning, "cancel timer, bump pid_generation, create-process", true, true
		case EvRequestGraceful:
			return StIdle, "cancel timer", true, true
		}
	case StQuarantined:
		switch ev {
		case EvIntentUpdate:
			if ctx.IntentDesired == "stopped" {
				return StQuarantined, "", false, true
			}
			if ctx.IntentDesired == "running" {
				return StSpawning, "reset failures, bump pid_generation, create-process", true, true
			}
		case EvManualRestart:
			return StSpawning, "reset failures, bump pid_generation, create-process", true, true
		case EvRequestGraceful:
			return StQuarantined, "no-op (already not running)", false, true
		}
	}
	return state, "", false, false
}

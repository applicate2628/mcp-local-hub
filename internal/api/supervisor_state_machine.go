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
			return StRunning, "", false, true
		case EvChildExit:
			return StBackoffWaiting, "arm timer at backoff(failures)", true, true
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

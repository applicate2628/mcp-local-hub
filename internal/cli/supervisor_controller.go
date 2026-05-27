// Package cli — Phase A.2 supervisorController.
//
// Spec: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md §A.2.
//
// supervisorController is the long-lived runtime owner that promotes the
// runRespawnDispatcher subset of the formal state machine into a single
// dispatch path routing every spawn/respawn through api.Transition().
//
// Responsibilities owned here:
//   - intentCache (atomic.Value) for descriptor lookup on EvStart/EvChildExit
//   - daemonIntentCache (atomic.Value) for per-task desired/stop lookup
//   - per-TaskName SM state map (api.SMState values), mirrored to
//     supervisor-state.json via the existing tracker persist seam
//   - sliding-window quarantine + exponential backoff (absorbed from the
//     deleted runRespawnDispatcher)
//
// What stays outside the controller:
//   - the FIFO event loop (api.EventLoop) is constructed in runSupervise
//     and shared with both the legacy audit-only handler and this
//     controller's handleLoopEvent
//   - DaemonRuntimeTracker is constructed in runSupervise; the controller
//     is the SOLE consumer of crash-counting methods
//   - SpawnFunc / TerminateFunc closures are constructed in runSupervise
//     (they reference job + events + tracker + statePath); the controller
//     receives them via the spawn field
//
// The PR #230 runRespawnDispatcher is REMOVED. Its responsibilities
// (sliding-window check, backoff timer arm, spawn fire, quarantine
// audit) are absorbed into executeSideEffect.
package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
)

// supervisorController is the long-lived runtime owner.
type supervisorController struct {
	intentCache  *IntentCache
	eventLoop    *api.EventLoop
	tracker      *DaemonRuntimeTracker
	smStates     sync.Map // taskName -> api.SMState
	// queuedActions tracks per-task queued action ("" | "respawn" | "none")
	// preserved across StExiting transitions per SM spec §"queued_action
	// preservation across supervisor exit" (api/supervisor_state_machine.go:99).
	// Closes bot PR#222 P1-3: previously the controller hardcoded
	// SMContext.QueuedAction="" so EvManualRestart → StExiting then
	// EvChildExit always went to StIdle instead of bouncing back to
	// StSpawning for the queued respawn. handleLoopEvent reads this map
	// into SMContext.QueuedAction and writes it back based on the SM's
	// side-effect string (which encodes set/clear queued_action directives).
	queuedActions sync.Map // taskName -> string
	events       *api.SupervisorEventLog
	graceful     *gracefulCounter
	daemonIntent *daemonIntentCache

	// spawn is the production SpawnFunc closure constructed in
	// runSupervise. executeSideEffect calls it when the SM transition
	// requests a "create-process" side effect. The controller does NOT
	// own this closure's lifetime - runSupervise constructs it once
	// (with job + events + tracker + statePath + overlay + crashCh)
	// and passes the same closure to both the Reconciler and the
	// controller so spawn semantics stay identical on both paths.
	spawn SpawnFunc

	// terminate is the production TerminateFunc closure constructed in
	// runSupervise. executeSideEffect calls it when the SM transition
	// requests an "issue terminate" side effect (StRunning/StSpawning +
	// EvIntentUpdate{stopped} | EvRequestGraceful | EvManualRestart).
	terminate TerminateFunc

	// statePath points at <state-dir>/supervisor-state.json. After every
	// persistBefore=true transition the controller asks the tracker to
	// flush its in-memory state to this file.
	statePath string

	// ctx is the lifetime context for backoff timer goroutines. The
	// controller observes ctx.Done() in executeSideEffect's timer
	// goroutine so a graceful exit cancels pending respawns instead of
	// firing them against a torn-down event loop.
	ctx context.Context

	// failureWindow + quarantineThreshold mirror the deleted
	// runRespawnDispatcher constants. Kept as struct fields so tests
	// can shrink the window without touching the package-level
	// respawnFailureWindow constant (the constant remains the package
	// default for production wiring).
	failureWindow       time.Duration
	quarantineThreshold int
}

// daemonIntentCache is the per-task DaemonIntent snapshot owned by
// the controller. Same atomic.Value pattern as IntentCache; readers
// see a coherent snapshot pointer regardless of concurrent writer
// refresh.
//
// Lookup semantic mirrors the mixed-bootstrap default at
// daemon_intent.go:230: an absent task returns the zero DaemonIntent,
// which handleLoopEvent reinterprets as "default-running" before
// passing to api.Transition.
type daemonIntentCache struct {
	snap atomic.Value // *daemonIntentSnapshot
}

type daemonIntentSnapshot struct {
	file  *api.DaemonIntentFile
	tasks map[string]api.DaemonIntent
}

func newDaemonIntentCache() *daemonIntentCache {
	c := &daemonIntentCache{}
	// Seed with empty snapshot so Load() never returns nil typed-as
	// pointer (atomic.Value typed nil-vs-untyped-nil edge case).
	c.snap.Store(&daemonIntentSnapshot{})
	return c
}

// Lookup returns the per-task DaemonIntent. Absent task returns the
// zero DaemonIntent (Desired=="" -> default-running via
// IsActiveStop semantics at daemon_intent.go:308).
func (c *daemonIntentCache) Lookup(taskName string) api.DaemonIntent {
	if c == nil {
		return api.DaemonIntent{}
	}
	s, ok := c.snap.Load().(*daemonIntentSnapshot)
	if !ok || s == nil {
		return api.DaemonIntent{}
	}
	if s.tasks == nil {
		return api.DaemonIntent{}
	}
	return s.tasks[taskName]
}

// Refresh atomically swaps the cached snapshot. Wired into
// watcher.onChange alongside IntentCache.Refresh.
func (c *daemonIntentCache) Refresh(file *api.DaemonIntentFile) {
	if c == nil {
		return
	}
	snap := &daemonIntentSnapshot{file: file}
	if file != nil {
		snap.tasks = file.Tasks
	}
	c.snap.Store(snap)
}

// IntentCache is the supervisor-intent.json descriptor cache. atomic.Value
// snapshot pointer; refreshed on IntentWatcher.onChange. The plan's v5
// design names this `IntentCache` (exported) so future phases can
// reference the type from outside cli/.
type IntentCache struct {
	snap atomic.Value // *intentSnapshot
}

type intentSnapshot struct {
	intent       *api.SupervisorIntentFile
	daemonByTask map[string]*api.SupervisorDaemon
}

func newIntentCache() *IntentCache {
	c := &IntentCache{}
	c.snap.Store(&intentSnapshot{daemonByTask: map[string]*api.SupervisorDaemon{}})
	return c
}

// Lookup returns the descriptor for taskName + a boolean for the
// "present in current intent" check. The bool distinguishes absent
// (handleLoopEvent should drop the event) from present (the SM
// runs).
func (c *IntentCache) Lookup(taskName string) (*api.SupervisorDaemon, bool) {
	if c == nil {
		return nil, false
	}
	s, ok := c.snap.Load().(*intentSnapshot)
	if !ok || s == nil {
		return nil, false
	}
	d, ok := s.daemonByTask[taskName]
	return d, ok
}

// Refresh atomically swaps the cached snapshot. Wired into
// IntentWatcher.onChange.
func (c *IntentCache) Refresh(intent *api.SupervisorIntentFile) {
	if c == nil {
		return
	}
	snap := &intentSnapshot{
		intent:       intent,
		daemonByTask: map[string]*api.SupervisorDaemon{},
	}
	if intent != nil {
		// Build a stable per-task pointer map. The intent.Daemons slice
		// is owned by the caller; we capture pointers into it because
		// the snapshot is short-lived (replaced on the next watcher
		// fire) and readers only need read-only access.
		for i := range intent.Daemons {
			d := &intent.Daemons[i]
			snap.daemonByTask[d.TaskName] = d
		}
	}
	c.snap.Store(snap)
}

// diffIntentSnapshots returns the slice of task names whose intent
// state CHANGED between previous and updated. "Changed" is defined as
// one of:
//   - task is in updated but not in previous (added)
//   - task is in previous but not in updated (removed; an EvIntentUpdate
//     for a removed task drives the SM to recognize the absence -
//     typically a transition through StExiting or StIdle)
//   - task is in both AND any of (Desired, Reason, UpdatedAt) differs
//
// Pure function; no I/O. Used by the watcher onChange to avoid the
// v6 storm (posting one EvIntentUpdate per known task on every
// mtime bump even if nothing actually changed).
func diffIntentSnapshots(previous, updated *api.DaemonIntentFile) []string {
	prev := mapOrEmpty(previous)
	next := mapOrEmpty(updated)
	var delta []string
	seen := make(map[string]struct{}, len(prev)+len(next))
	for taskName, p := range prev {
		seen[taskName] = struct{}{}
		n, ok := next[taskName]
		if !ok || p.Desired != n.Desired || p.Reason != n.Reason || !p.UpdatedAt.Equal(n.UpdatedAt) {
			delta = append(delta, taskName)
		}
	}
	for taskName := range next {
		if _, already := seen[taskName]; already {
			continue
		}
		delta = append(delta, taskName) // added
	}
	return delta
}

func mapOrEmpty(f *api.DaemonIntentFile) map[string]api.DaemonIntent {
	if f == nil {
		return nil
	}
	return f.Tasks
}

// GetSMState exposes the controller's per-task SM state so OTHER
// subsystems (notably F.3's single-workspace-shortcut health gate)
// can read the policy state without touching the unexported sync.Map
// directly. Returns (StIdle, false) when no state is tracked for the
// given task. The bool distinguishes "no tracked state yet" from a
// literal StIdle value.
func (c *supervisorController) GetSMState(taskName string) (api.SMState, bool) {
	if c == nil {
		return api.StIdle, false
	}
	v, ok := c.smStates.Load(taskName)
	if !ok {
		return api.StIdle, false
	}
	s, ok := v.(api.SMState)
	if !ok {
		return api.StIdle, false
	}
	return s, true
}

// handleLoopEvent is the single dispatch path for spawn/exit events.
// Replaces both the direct r.spawn(d) call in supervise_reconcile.go:118
// AND the runRespawnDispatcher goroutine in
// supervise_respawn_dispatcher.go:77.
//
// Resolves the descriptor from intentCache, reads the current SM state
// from smStates, builds an SMContext from the cached daemon-intent
// + tracker + graceful flag, calls api.Transition, persists the new
// state (when persistBefore=true), then executes the side effect.
func (c *supervisorController) handleLoopEvent(ev api.LoopEvent) {
	if c == nil {
		return
	}
	d, ok := c.intentCache.Lookup(ev.TaskName)
	if !ok {
		// Daemon dropped from intent OR not yet known (controller
		// initial state, or a stale event posted by the cmd.Wait
		// goroutine after the descriptor was removed). Audit-log
		// and drop.
		if c.events != nil {
			_ = c.events.Emit(api.SupervisorEvent{
				Severity: "debug",
				Source:   "lifecycle",
				Event:    "controller-event-orphan",
				TaskName: ev.TaskName,
				Body: map[string]any{
					"kind": string(ev.Kind),
				},
			})
		}
		return
	}

	// Default to StIdle when the smStates map has no entry. The
	// api.SMState type is a string and its zero value is the empty
	// string ""; the SM transition table only matches named states
	// (StIdle == "idle"), so an empty zero value would always drop
	// the event as "no row matched". The bootstrap state is StIdle
	// per the SM design - a daemon not yet observed is idle.
	currentState := api.StIdle
	if v, ok := c.smStates.Load(ev.TaskName); ok {
		if s, ok := v.(api.SMState); ok {
			currentState = s
		}
	}

	now := time.Now().UTC()

	// Resolve the intent's active-stop predicate via the REAL method
	// form. There is no api.IsActiveStop(d, now) free function; the
	// predicate is the method on DaemonIntent (daemon_intent.go:308).
	di := c.daemonIntent.Lookup(ev.TaskName)
	activeStop, _ := di.IsActiveStop(now)
	intentDesired := di.Desired
	if intentDesired == "" {
		// Mixed-bootstrap default per daemon_intent.go:230. Absent
		// entries are treated as "default-running" by the SM.
		intentDesired = api.IntentDesiredRunning
	}

	failures := 0
	if c.tracker != nil {
		failures = c.tracker.CrashCountInWindow(ev.TaskName, now, c.failureWindow)
	}

	gracefulInProgress := false
	if c.graceful != nil {
		gracefulInProgress = c.graceful.InProgress()
	}

	// Read the per-task queued action (preserved across StExiting per SM
	// spec). Closes bot PR#222 P1-3: previously hardcoded "" — see
	// supervisor_controller.go queuedActions field docstring.
	queuedAction := ""
	if v, ok := c.queuedActions.Load(ev.TaskName); ok {
		if qa, ok := v.(string); ok {
			queuedAction = qa
		}
	}

	smCtx := api.SMContext{
		IntentDesired:      intentDesired,
		IntentIsActiveStop: activeStop,
		Failures:           failures,
		QueuedAction:       queuedAction,
		GracefulInProgress: gracefulInProgress,
	}

	newState, side, persistBefore, matched := api.Transition(currentState, ev.Kind, smCtx)
	if !matched {
		if c.events != nil {
			_ = c.events.Emit(api.SupervisorEvent{
				Severity: "debug",
				Source:   "lifecycle",
				Event:    "controller-event-unhandled",
				TaskName: ev.TaskName,
				Body: map[string]any{
					"kind":  string(ev.Kind),
					"state": string(currentState),
				},
			})
		}
		return
	}

	// Always update in-memory smStates when transition matches, regardless
	// of persistBefore (closes sonnet impl-r2 BLOCKER: previous code wrapped
	// the Store inside `if persistBefore`, so persistBefore=false transitions
	// like StSpawning + EvHealthOK → StRunning would match the SM but never
	// update smStates. The daemon then stayed in StSpawning in-memory and
	// subsequent EvIntentUpdate(stopped) silently dropped since StSpawning
	// has no EvIntentUpdate case in the SM table).
	//
	// persistBefore semantically controls DISK-FLUSH TIMING (whether the
	// caller must persist the new state to supervisor-state.json BEFORE
	// performing the side effect), NOT whether the in-memory transition
	// took effect. The SM transition matched (matched=true), so smStates
	// reflects the new state immediately; persistence is a separate axis.
	c.smStates.Store(ev.TaskName, newState)

	// Apply queued_action updates encoded in the SM's `side` string per
	// spec §"queued_action preservation". Closes bot PR#222 P1-3:
	// without this write-back, the queuedAction field stays "" forever
	// and the EvManualRestart → StExiting → EvChildExit → StSpawning
	// path is unreachable.
	//
	// Substring matches mirror the side-string vocabulary defined in
	// api/supervisor_state_machine.go (StRunning+EvManualRestart returns
	// "issue terminate, queued_action=respawn"; StExiting+EvIntentUpdate
	// returns "set queued_action=respawn" or "clear queued_action"; etc.).
	// "queued_action=none" implies clearing; "queued_action=respawn"
	// implies setting respawn.
	switch {
	case strings.Contains(side, "set queued_action=stop"):
		// Closes bot finding B on PR #236 1c0ea09: StSpawning +
		// EvIntentUpdate(stopped) records stop-pending so the
		// subsequent EvHealthOK / EvChildExit transition can honor it
		// (see api/supervisor_state_machine.go StSpawning case).
		c.queuedActions.Store(ev.TaskName, "stop")
	case strings.Contains(side, "queued_action=respawn"):
		c.queuedActions.Store(ev.TaskName, "respawn")
	case strings.Contains(side, "queued_action=none"), strings.Contains(side, "clear queued_action"):
		c.queuedActions.Store(ev.TaskName, "")
	}
	// Bounded auto-clear of queued_action: ONLY clear when transitioning
	// OUT of StExiting back to StIdle / StSpawning. The original intent
	// of this clear (per spec §"queued_action preservation across
	// supervisor exit") was "consumed by the StExiting handler". A
	// self-loop StSpawning -> StSpawning from EvIntentUpdate(stopped)
	// must preserve queued_action=stop so the next EvHealthOK /
	// EvChildExit transition (which checks ctx.QueuedAction) sees it.
	// Closes the queued_action self-clear bug that sonnet r3 caught in
	// the original B1 fix-design (without the currentState check, the
	// "set queued_action=stop" store at line above would be immediately
	// wiped by the auto-clear when newState == StSpawning).
	if currentState == api.StExiting && (newState == api.StIdle || newState == api.StSpawning) {
		c.queuedActions.Store(ev.TaskName, "")
	}
	if persistBefore {
		// Best-effort persist - audit-log on failure but do NOT block
		// the side effect. The tracker mirrors the SM state into
		// supervisor-state.json via supervisorStateFromRuntimeState
		// (which understands "backoff-waiting" and "quarantined"
		// directly per supervisor_runtime_tracker.go:314-325).
		if c.tracker != nil && c.statePath != "" {
			_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, ev.TaskName)
		}
	}

	c.executeSideEffect(side, newState, d, ev)
}

// executeSideEffect absorbs the dispatcher's responsibilities
// (sliding-window check, backoff timer, spawn fire, quarantine audit)
// PLUS the formal terminate side effects from api.Transition.
//
// side is the human-readable side-effect description returned by
// api.Transition (e.g. "bump pid_generation, create-process",
// "arm timer at backoff(failures)", "issue terminate, ..."). The
// controller dispatches on the new state primarily; the side string
// is captured for future audit-row extensions.
func (c *supervisorController) executeSideEffect(
	side string,
	newState api.SMState,
	d *api.SupervisorDaemon,
	ev api.LoopEvent,
) {
	if c == nil || d == nil {
		return
	}
	_ = side // captured for future audit-row extensions

	switch newState {
	case api.StSpawning:
		// Gate spawn-fire on the side string carrying "create-process".
		// The SM table can return newState=StSpawning on a self-loop
		// (e.g. StSpawning + EvIntentUpdate(stopped) returns
		// "set queued_action=stop" without re-spawning) - we must NOT
		// re-fire spawn for those. Only fresh-entry transitions
		// (StIdle/StBackoffWaiting/StQuarantined/StExiting -> StSpawning)
		// include "create-process" in the side string.
		//
		// Closes B1 self-loop spawn duplication bug discovered during
		// r4 implementation: without this gate, every
		// EvIntentUpdate(stopped) arriving while spawning would
		// re-fire spawn, and the subsequent EvHealthOK would clear
		// queued_action=stop via the "issue terminate, queued_action=
		// none" side string from the StExiting transition.
		if !strings.Contains(side, "create-process") {
			return
		}

		// "bump pid_generation, create-process" - fire the spawn
		// closure.
		//
		// On success, post EvHealthOK to drive StSpawning → StRunning
		// (closes codex r1 BLOCKER-1: without this transition, daemons
		// stuck in StSpawning never become eligible for EvIntentUpdate
		// stop handling, which only StRunning processes — Desired=stopped
		// would be silently dropped). The "health" here is process-start
		// success; a proper health probe (port-bind / HTTP /health) is
		// a follow-up. Mirrors the pre-A.2 dispatcher's "started =
		// considered healthy" semantic.
		//
		// On failure, the response depends on whether a child process
		// ever existed:
		//
		//   - PRE-child (cmd.Start / StartWithJob returned error;
		//     no PID; no wait goroutine launched). errors.Is(err,
		//     errSpawnPreChild) discriminates this case. We post a
		//     SYNTHETIC EvChildExit so the SM routes StSpawning →
		//     StBackoffWaiting and the backoff timer drives retry
		//     through the standard failure-counter pipeline. Closes
		//     Codex Cloud finding on 2d67031 (daemon stuck in
		//     StSpawning forever after pre-child spawn error).
		//
		//   - POST-child (cmd.Start succeeded, wait goroutine
		//     observing child, but a downstream step like
		//     persistDaemonRuntimeTracker failed). Child IS alive
		//     and will eventually produce a real EvChildExit via
		//     the wait goroutine's crashCh path. We do NOTHING here:
		//     posting a synthetic EvChildExit would race the real
		//     one and the backoff timer could spawn a duplicate
		//     daemon while the original child is still running.
		//     Closes Codex Cloud bot finding on PR #236 a54cc95 (P1).
		//
		// Self-posts use PostSelf (priority channel) instead of inline
		// Post on the main channel. This closes TWO bot findings on
		// PR #236 1c0ea09:
		//
		//   - P2 deadlock: inline Post into the same channel the
		//     handler drains can deadlock when external producers
		//     have filled the buffer. PostSelf goes to a separate
		//     selfCh whose only producer is the handler, so it
		//     cannot collide with external producers.
		//
		//   - P2 FIFO race: a pre-queued EvIntentUpdate(stopped)
		//     behind the original EvStart would land in the SM
		//     against StSpawning -> no transition -> drop. PostSelf
		//     goes to the priority channel; Run priority-drains
		//     selfCh before reading from ch on the next iteration,
		//     so the synthetic EvChildExit / EvHealthOK transitions
		//     the daemon OUT of StSpawning BEFORE the pre-queued
		//     EvIntentUpdate is processed.
		//
		// The B1 fix in supervisor_state_machine.go also adds
		// StSpawning + EvIntentUpdate -> set queued_action=stop so
		// even if the priority-drain order is wrong somewhere, the
		// stop request is preserved across the StSpawning self-loop
		// and consumed by the next EvHealthOK / EvChildExit.
		//
		// If PostSelf returns false (selfCh full - should never
		// happen at production cap=1024), we emit an audit error
		// and let the next reconcile-driven EvStart re-attempt;
		// falling back to blocking Post would reintroduce the
		// deadlock we are avoiding.
		if c.spawn != nil {
			err := c.spawn(*d)
			if c.eventLoop != nil {
				switch {
				case err == nil:
					if !c.eventLoop.PostSelf(api.LoopEvent{Kind: api.EvHealthOK, TaskName: d.TaskName}) {
						c.emitSelfChannelSaturated(d.TaskName, "EvHealthOK")
					}
				case errors.Is(err, errSpawnPreChild):
					if !c.eventLoop.PostSelf(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName}) {
						c.emitSelfChannelSaturated(d.TaskName, "EvChildExit")
					}
				}
			}
		}

	case api.StBackoffWaiting:
		c.handleBackoffWaiting(d, ev)

	case api.StQuarantined:
		// Reached via EvTimerDue while at threshold. Emit the
		// quarantine audit row if it has not already been emitted
		// from the backoff path. The SM transition table sets
		// persistBefore=true here so the state is already mirrored.
		if c.events != nil {
			_ = c.events.Emit(api.SupervisorEvent{
				Severity: "error",
				Source:   "lifecycle",
				Event:    "daemon-quarantined",
				TaskName: d.TaskName,
				Body: map[string]any{
					"reason": "EvTimerDue at or beyond quarantine threshold",
				},
			})
		}

	case api.StExiting:
		// "issue terminate, queued_action=*" - fire the terminate
		// closure. Errors are swallowed: the terminate fn itself
		// emits per-daemon audit rows and the SM owns retry via
		// EvChildExit when the child actually exits.
		if c.terminate != nil {
			_ = c.terminate(*d)
		}

	case api.StRunning, api.StIdle:
		// Steady / no-op states. No side effect required.
		// StRunning is reached on EvHealthOK (clears the spawning
		// gate). StIdle is reached on EvChildExit while exiting,
		// graceful drain, or initial reconcile of a stopped intent.
		return
	}
}

// emitSelfChannelSaturated logs an audit row when PostSelf on the
// priority channel returns false. This should never happen at
// production cap=1024 (handler-self-posts are bounded at ~1-2 per
// handler call); if it does, the daemon is left in StSpawning until
// the next reconcile cycle re-attempts (the alternative of falling
// back to blocking Post reintroduces the deadlock we are avoiding -
// see PostSelf doc in supervisor_event_loop.go).
//
// Closes bot finding on PR #236 1c0ea09 (P2 self-post deadlock): the
// audit-fallback is the explicit drop policy that PostSelf's contract
// requires.
func (c *supervisorController) emitSelfChannelSaturated(taskName, kind string) {
	if c.events == nil {
		return
	}
	_ = c.events.Emit(api.SupervisorEvent{
		Severity: "error",
		Source:   "lifecycle",
		Event:    "supervisor-self-channel-saturated",
		TaskName: taskName,
		Body: map[string]any{
			"event_kind": kind,
			"note":       "PostSelf returned false; daemon left in StSpawning until next reconcile (fallback to blocking Post would reintroduce the deadlock we are avoiding)",
		},
	})
}

// handleBackoffWaiting is the absorbed responsibility from the deleted
// runRespawnDispatcher: record the failure, check the quarantine
// threshold, emit the audit row, arm the backoff timer.
func (c *supervisorController) handleBackoffWaiting(d *api.SupervisorDaemon, ev api.LoopEvent) {
	now := time.Now().UTC()
	failures := 0
	if c.tracker != nil {
		failures = c.tracker.RecordCrashAndCountInWindow(d.TaskName, now, c.failureWindow)
	}

	// Capture exit_code for the audit row if available on the crash
	// event. EvChildExit carries it via the optional Body payload
	// (parity with the deleted dispatcher's daemon-respawn-scheduled
	// audit row).
	exitCode := 0
	if ev.Body != nil {
		if v, ok := ev.Body["exit_code"].(int); ok {
			exitCode = v
		}
	}

	if failures >= c.quarantineThreshold {
		// Quarantine transition - promote state, audit-log, and stop
		// scheduling respawns. The SM itself moves to StQuarantined
		// via the EvTimerDue path in production; we anticipate it
		// here so an operator looking at the audit log sees the
		// quarantine row immediately rather than after the backoff
		// timer would have fired.
		c.smStates.Store(d.TaskName, api.StQuarantined)
		// Mirror SM state into the tracker so IPC status snapshots
		// + the respawn IPC guard see "quarantined", not stale "idle"
		// (closes codex r1 BLOCKER-2: smStates and tracker were
		// out-of-sync; supervise_respawn.go:132 reads tracker state
		// for the respawn refusal guard).
		if c.tracker != nil {
			c.tracker.MarkQuarantined(d.TaskName)
		}
		if c.tracker != nil && c.statePath != "" {
			_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, d.TaskName)
		}
		if c.events != nil {
			_ = c.events.Emit(api.SupervisorEvent{
				Severity: "error",
				Source:   "lifecycle",
				Event:    "daemon-quarantined",
				TaskName: d.TaskName,
				Body: map[string]any{
					"failures_in_30m": failures,
					"reason":          "10+ failures in 30-min sliding window; respawn attempts suspended until supervisor restart",
					"exit_code":       exitCode,
				},
			})
		}
		return
	}

	// Mirror SM state into the tracker so IPC status snapshots and the
	// respawn IPC guard see "backoff", not stale "idle" (closes codex
	// r1 BLOCKER-2 second half; symmetric with MarkQuarantined above).
	if c.tracker != nil {
		c.tracker.MarkBackoff(d.TaskName)
	}
	if c.tracker != nil && c.statePath != "" {
		_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, d.TaskName)
	}

	backoff := computeRespawnBackoff(failures)
	if c.events != nil {
		_ = c.events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "daemon-respawn-scheduled",
			TaskName: d.TaskName,
			Body: map[string]any{
				"failures_in_30m": failures,
				"backoff_seconds": int(backoff / time.Second),
				"exit_code":       exitCode,
			},
		})
	}

	// Arm the backoff timer in a goroutine so the event loop stays
	// responsive. When the timer fires, post EvTimerDue so the SM
	// moves StBackoffWaiting -> StSpawning (or StQuarantined per the
	// transition table) via the same handleLoopEvent path. Cancel-
	// respecting timer ensures graceful shutdown doesn't block on a
	// pending backoff.
	//
	// Cancel-on-state-change: when an EvIntentUpdate or
	// EvManualRestart transitions the SM off StBackoffWaiting BEFORE
	// the timer fires, the timer should be a no-op. We re-check the
	// SM state at fire time and drop the EvTimerDue/spawn if the
	// state has already moved. This honors the SM's "cancel timer"
	// side effect without needing a per-task timer registry.
	descriptor := *d
	taskName := d.TaskName
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		t := time.NewTimer(backoff)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// Re-check SM state: if the daemon transitioned off
		// StBackoffWaiting (e.g. via EvIntentUpdate(stopped) ->
		// StIdle, EvManualRestart -> StSpawning, EvRequestGraceful
		// -> StIdle), the timer is stale. Drop it. The SM table
		// already moved state with persistBefore=true so the
		// supervisor-state.json view is authoritative.
		if v, ok := c.smStates.Load(taskName); ok {
			if s, ok := v.(api.SMState); ok && s != api.StBackoffWaiting {
				if c.events != nil {
					_ = c.events.Emit(api.SupervisorEvent{
						Severity: "debug",
						Source:   "lifecycle",
						Event:    "daemon-respawn-timer-stale",
						TaskName: taskName,
						Body: map[string]any{
							"current_state": string(s),
						},
					})
				}
				return
			}
		}
		if c.events != nil {
			_ = c.events.Emit(api.SupervisorEvent{
				Severity: "debug",
				Source:   "lifecycle",
				Event:    "daemon-respawn-fired",
				TaskName: taskName,
			})
		}
		// Drive the next transition through the formal SM via
		// EvTimerDue. The SM at StBackoffWaiting+EvTimerDue
		// transitions to StSpawning (when failures < threshold) or
		// StQuarantined (when threshold reached); both outcomes are
		// handled by recursing through handleLoopEvent which
		// evaluates a fresh SMContext.
		if c.eventLoop != nil {
			c.eventLoop.Post(api.LoopEvent{
				Kind:     api.EvTimerDue,
				TaskName: taskName,
			})
		} else {
			// Fallback for tests that don't wire an event loop into
			// the controller and just want to assert the spawn fires
			// after backoff: directly invoke spawn.
			if c.spawn != nil {
				_ = c.spawn(descriptor)
			}
		}
	}()
}

// computeRespawnBackoff returns the wait duration before the
// `failures`-th respawn attempt. failures=1 -> 1s, failures=2 -> 2s,
// failures=3 -> 4s, ..., capped at respawnBackoffMax. failures<=0
// returns 0 (no backoff).
//
// Moved here from the deleted supervise_respawn_dispatcher.go so the
// controller is the SOLE owner of the backoff schedule. The
// constants (respawnFailureWindow, respawnQuarantineThreshold,
// respawnBackoffStep, respawnBackoffMax) live alongside this file as
// package-level constants because tests + the tracker reference them.
func computeRespawnBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	// Exponential: 2^(failures-1) * step.
	// failures=1 -> 2^0 = 1; failures=7 -> 2^6 = 64; ...
	n := failures - 1
	if n > 30 {
		n = 30 // guard against overflow in the bit-shift below
	}
	d := time.Duration(int64(1)<<uint(n)) * respawnBackoffStep
	if d > respawnBackoffMax || d <= 0 {
		return respawnBackoffMax
	}
	return d
}

// respawnFailureWindow is the sliding window inside which crashes
// count toward the quarantine threshold. 30 minutes is the v0.5.0
// spec default (supervisor_state_machine.go references the same
// window for the legacy SM design).
const respawnFailureWindow = 30 * time.Minute

// respawnQuarantineThreshold is the count of failures within
// respawnFailureWindow that triggers quarantine. After this many
// failures the controller stops respawning the daemon until the
// supervisor cold-restarts (which clears the in-memory window).
const respawnQuarantineThreshold = 10

// respawnBackoffStep is the base unit for the exponential backoff
// schedule: 1s, 2s, 4s, 8s, 16s, 32s, then capped at respawnBackoffMax.
const respawnBackoffStep = 1 * time.Second

// respawnBackoffMax caps the exponential backoff so long-running
// degraded states still get a respawn attempt at least once a minute.
const respawnBackoffMax = 60 * time.Second

// runCrashEventBridge consumes crashEvent values from crashCh and
// posts api.LoopEvent{Kind: EvChildExit, ...} onto the supervisor's
// FIFO event loop so the controller's handleLoopEvent can drive the
// next SM transition. Replaces the deleted runRespawnDispatcher
// while keeping the existing crashCh wiring in the spawn fn's
// cmd.Wait goroutine intact (no changes needed to the production
// spawn closure).
//
// exit_code is propagated through LoopEvent.Body so the controller's
// audit row (daemon-respawn-scheduled / daemon-quarantined) carries
// the same observable diagnostic field the deleted dispatcher
// emitted.
//
// Exits when ctx is canceled (supervisor graceful shutdown) or when
// crashCh is closed. Errors from loop.Post are not possible -
// EventLoop.Post is non-blocking against a full channel only at the
// channel-cap level (cap=1024 from the production wiring at
// supervise.go:416), and a full channel here would block the
// bridge but not lose events.
func runCrashEventBridge(
	ctx context.Context,
	crashCh <-chan crashEvent,
	loop *api.EventLoop,
	events *api.SupervisorEventLog,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-crashCh:
			if !ok {
				return
			}
			if loop == nil {
				// Defensive: a missing event loop means the
				// controller wiring was incomplete. Audit-log so
				// operators see the dropped crash event.
				if events != nil {
					_ = events.Emit(api.SupervisorEvent{
						Severity: "warn",
						Source:   "lifecycle",
						Event:    "crash-event-dropped-no-loop",
						TaskName: ev.Daemon.TaskName,
						Body: map[string]any{
							"exit_code": ev.ExitCode,
						},
					})
				}
				continue
			}
			body := map[string]any{
				"exit_code": ev.ExitCode,
			}
			if ev.WaitErr != nil {
				body["wait_err"] = ev.WaitErr.Error()
			}
			loop.Post(api.LoopEvent{
				Kind:     api.EvChildExit,
				TaskName: ev.Daemon.TaskName,
				Body:     body,
			})
		}
	}
}

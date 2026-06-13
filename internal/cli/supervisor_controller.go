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
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
)

// supervisorController is the long-lived runtime owner.
type supervisorController struct {
	intentCache *IntentCache
	eventLoop   *api.EventLoop
	tracker     *DaemonRuntimeTracker
	smStates    sync.Map // taskName -> api.SMState
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
	// ownSpawned records every task whose `spawn` closure this controller
	// fired SUCCESSFULLY during the CURRENT supervisor process lifetime.
	// It is the discriminator between own-spawned children (which have a
	// live cmd.Wait/reaper goroutine in this process that posts the real
	// EvChildExit on exit) and FOREIGN warm-start PIDs hydrated from
	// supervisor-state.json by a previous supervisor (which have NO
	// cmd.Wait goroutine here). The StExiting terminate path uses this map
	// to decide whether to SYNTHESIZE the follow-up EvChildExit: a foreign
	// PID's terminate produces no real exit event, so without a synthetic
	// one the SM stays wedged in StExiting with queued_action=respawn never
	// consumed (Codex bot #268 r11 P2, supervise_liveness.go:179). Marked
	// true on spawn-success (after which a cmd.Wait DOES exist), so the
	// SECOND restart of a previously-foreign daemon correctly relies on the
	// real exit event and never double-posts.
	ownSpawned sync.Map // taskName -> bool (own-spawned this process lifetime)
	// reaperOutstanding records, per task, whether a real own cmd.Wait /
	// reaper goroutine the controller launched is still expected to post a
	// real EvChildExit. It is the race-safe complement to ownSpawned:
	// ownSpawned tracks INTENT membership (and is dropped by
	// clearRemovedTaskRuntime on re-registration so a later genuinely-
	// foreign PID under the same name can still be synthesized), but a
	// re-registration that drops ownSpawned does NOT make the previous
	// own child's reaper disappear — that reaper is still live and will
	// post the real EvChildExit when the child finally dies. Synthesizing
	// a foreign EvChildExit while that real one is still pending would
	// double-post for a single exit and double-spawn (Codex deep-sec PR
	// #268 Conc-F3). So the StExiting synthesize is gated on
	// reaperOutstanding being absent in ADDITION to !ownSpawned: a real
	// reaper outstanding suppresses the synthesize, and the real exit
	// drives the single respawn. Set true on spawn-success (a fresh reaper
	// is now live); cleared when the controller observes the task's real
	// EvChildExit (any EvChildExit reaching handleLoopEvent is a real
	// exit observation — the synthetic one fires only for foreign tasks
	// that never had an entry here, so clearing it for them is a no-op).
	reaperOutstanding sync.Map // taskName -> bool (real own reaper expected to post EvChildExit)
	// pendingReap records, per canonical task name, that the task DISAPPEARED
	// from the freshly-read intent on the PREVIOUS refresh while its SM state
	// was LIVE, but has not yet been confirmed-absent across the verification
	// window. It is the transient-absence guard for the orphan-reap: a
	// descriptor can be momentarily absent across two refresh ticks during an
	// operator/install REPLACE-IN-PLACE (remove + re-add in separate intent
	// writes). The single os.ReadFile + atomic temp+rename writer discipline
	// (api.ReadSupervisorIntent / WriteStateFileAtomic under the
	// supervisor-intent flock) guarantees each read sees a CONSISTENT complete
	// snapshot — never a half-written file — so mid-write tearing is NOT the
	// risk; the replace-in-place blip across ticks is. The reap therefore
	// DETECTS an absence on tick N (marks pendingReap, captures the OLD
	// descriptor needed by the SM-driven terminate), and only TERMINATES on
	// tick N+1 if the task is STILL absent. A re-appearance between ticks drops
	// the mark with no terminate, absorbing the blip. The stored value is the
	// last-seen descriptor (*api.SupervisorDaemon) so the SM-aware terminate
	// has the TaskName + Command it needs even though the cache no longer
	// carries the row.
	pendingReap  sync.Map // taskName -> *api.SupervisorDaemon (absent-once, awaiting still-absent confirmation)
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

const idleRespawnResultBodyKey = "idle_respawn_result"

// supervisorCleanExitBodyKey flags an EvChildExit that corresponds to a
// CLEAN child exit (exit code 0, no Wait error). runCrashEventBridge sets
// it from the crashEvent fields; handleLoopEvent reads it to preserve the
// deliberate-shutdown contract (a clean exit observed while the task is
// still StRunning — i.e. NO controller-driven exit in flight — is dropped
// instead of routed through StRunning->StBackoffWaiting respawn). During a
// controller-driven restart the task is in StExiting when the clean exit
// arrives, so the flag does NOT suppress the queued respawn there.
const supervisorCleanExitBodyKey = "clean_exit"

// supervisorEventIsCleanExit reports whether a LoopEvent carries the
// clean-exit flag set by runCrashEventBridge. A missing flag is treated
// as NOT clean (the conservative default: synthetic pre-child / foreign
// EvChildExit events and any event without the flag route through the
// normal crash/backoff path).
func supervisorEventIsCleanExit(ev api.LoopEvent) bool {
	if ev.Body == nil {
		return false
	}
	clean, _ := ev.Body[supervisorCleanExitBodyKey].(bool)
	return clean
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

func (c *IntentCache) TaskNames() map[string]struct{} {
	out := map[string]struct{}{}
	if c == nil {
		return out
	}
	s, ok := c.snap.Load().(*intentSnapshot)
	if !ok || s == nil {
		return out
	}
	for taskName := range s.daemonByTask {
		out[canonicalSupervisorTaskName(taskName)] = struct{}{}
	}
	return out
}

// refreshSupervisorIntent atomically swaps the intent-descriptor cache to
// `updated` and reconciles the controller's in-memory bookkeeping for any
// task that DISAPPEARED from intent. It is the single owner of the
// descriptor-disappearance ORPHAN REAP — the durable supervisor-side
// backstop for the synchronous install/uninstall-time kill
// (api.killRemovedSupervisorTargetsAfter*), which is conditional on the
// reconcile-nudge succeeding and is skipped on nudge-failure or an install
// process that crashes between the intent-write and the kill. When that
// synchronous kill does not run, the running child is orphaned: it keeps
// holding its TCP port until a full supervisor cold-restart or manual
// taskkill (the prior design — supervise_reconcile.go's "Orphans are
// intentionally NOT terminated here" — left this gap because the pure
// Reconciler has no descriptor to fan a terminate through; here the
// controller still holds the OLD descriptor in its pre-refresh cache, so it
// CAN terminate).
//
// Only TWO call sites drive refreshSupervisorIntent — the 60s IntentWatcher
// onChange (supervise.go) and the apply-mode reconcile IPC
// (applyReconcileDrift) — and both already read the intent through the
// flock-atomic api.ReadSupervisorIntent (single os.ReadFile against an
// atomic temp+rename writer), so each `updated` is a CONSISTENT complete
// snapshot, never a half-written file. Both call sites also pass a NON-nil
// `updated` only on a successful read (the watcher nils it on read error and
// skips the call; the reconcile handler fails closed upstream on a corrupt
// read). So a disappearance observed here is a REAL on-disk removal, not a
// torn read.
//
// TRANSIENT-ABSENCE GUARD (the safety that prevents reaping a live daemon):
// a descriptor can still be MOMENTARILY absent across two refresh ticks when
// an operator/install REPLACES it in place (remove + re-add landing in
// separate intent writes). Reaping on the first observed absence would kill
// a daemon the operator is merely re-writing. So the reap uses a one-tick
// verification window: the FIRST refresh that observes a live task absent
// only MARKS it pendingReap (capturing the OLD descriptor for the later
// SM-driven terminate) and does NOT terminate. The terminate fires on the
// NEXT refresh, and ONLY if the task is STILL absent AND STILL live. A task
// that re-appears between ticks has its pendingReap mark dropped with no
// terminate — the blip is absorbed.
func (c *supervisorController) refreshSupervisorIntent(updated *api.SupervisorIntentFile) {
	if c == nil || c.intentCache == nil {
		return
	}
	previous := c.intentCache.TaskNames()

	// Resolve confirmed reaps BEFORE swapping the cache: a task that was
	// pendingReap from the PREVIOUS refresh and is STILL absent in `updated`
	// has cleared the verification window. We must read its descriptor +
	// terminate it while the OLD cache snapshot is still live (the captured
	// pendingReap descriptor is the authoritative source, but reading under
	// the old cache keeps the lookup paths identical). nextNames is computed
	// from `updated` directly so the still-absent check does not depend on
	// the cache having been swapped yet.
	nextNames := taskNameSetFromIntent(updated)
	var confirmedReaps []*api.SupervisorDaemon
	c.pendingReap.Range(func(key, value any) bool {
		taskName, _ := key.(string)
		if taskName == "" {
			return true
		}
		if _, present := nextNames[taskName]; present {
			// Re-appeared (replace-in-place completed) — drop the mark, no
			// terminate. The verification window absorbed the blip.
			c.pendingReap.Delete(taskName)
			c.emitReapEvent("debug", "orphan-reap-candidate-reappeared", taskName,
				"removed descriptor re-appeared within the verification window (replace-in-place); reap canceled")
			return true
		}
		// Still absent across the window. Re-check the SM state is still LIVE
		// (a self-driven StExiting/StIdle transition since the mark — e.g. the
		// child already exited and was reaped by its own cmd.Wait — means
		// there is nothing left to terminate).
		if !c.smStateIsReapable(taskName) {
			c.pendingReap.Delete(taskName)
			c.emitReapEvent("debug", "orphan-reap-candidate-settled", taskName,
				"removed descriptor settled to a non-live SM state within the verification window; no terminate needed")
			return true
		}
		if d, ok := value.(*api.SupervisorDaemon); ok && d != nil {
			confirmedReaps = append(confirmedReaps, d)
		}
		c.pendingReap.Delete(taskName)
		return true
	})

	// Capture the OLD descriptor for any task disappearing in THIS refresh
	// that is currently LIVE and not already pendingReap — it becomes a NEW
	// pendingReap candidate (tick 1 of the window). Captured BEFORE the cache
	// swap because IntentCache.Lookup returns pointers into the soon-to-be-
	// replaced snapshot slice; we copy the descriptor value so the captured
	// pointer outlives the swap.
	for taskName := range previous {
		if _, stillPresent := nextNames[taskName]; stillPresent {
			continue
		}
		if _, already := c.pendingReap.Load(taskName); already {
			// Handled above (either confirmed or re-appeared); skip.
			continue
		}
		if !c.smStateIsReapable(taskName) {
			// Not a live daemon (StIdle/StQuarantined or untracked) — nothing
			// to terminate. Just clear bookkeeping as before.
			c.clearRemovedTaskRuntime(taskName)
			continue
		}
		if d, ok := c.intentCache.Lookup(taskName); ok && d != nil {
			captured := *d // copy out of the pre-refresh snapshot slice
			c.pendingReap.Store(taskName, &captured)
			c.emitReapEvent("info", "orphan-reap-candidate-marked", taskName,
				"descriptor disappeared from intent while the daemon is live; awaiting still-absent confirmation across one refresh window before terminating (transient-absence/replace-in-place guard)")
		} else {
			// No descriptor available to terminate through (cache miss). Fall
			// back to the prior bookkeeping-only clear; the synchronous
			// install-time kill or a cold-restart reaper remains the backstop.
			c.clearRemovedTaskRuntime(taskName)
		}
	}

	// Swap the cache now that all pre-refresh descriptor reads are done.
	c.intentCache.Refresh(updated)

	// Drive the confirmed reaps through the SM-aware terminate, then clear
	// their bookkeeping. Done AFTER the cache swap so a terminate cannot
	// observe a stale descriptor for a DIFFERENT (still-present) task.
	for _, d := range confirmedReaps {
		c.reapRemovedDaemon(d)
		c.clearRemovedTaskRuntime(d.TaskName)
	}
}

// taskNameSetFromIntent returns the canonical task-name set declared by an
// intent file, mirroring IntentCache.TaskNames but reading the raw file so
// the still-absent check does not depend on the cache having been swapped.
func taskNameSetFromIntent(intent *api.SupervisorIntentFile) map[string]struct{} {
	out := map[string]struct{}{}
	if intent == nil {
		return out
	}
	for i := range intent.Daemons {
		out[canonicalSupervisorTaskName(intent.Daemons[i].TaskName)] = struct{}{}
	}
	return out
}

// smStateIsReapable reports whether the controller's current SM state for the
// task names a LIVE daemon the reap should terminate — StSpawning, StRunning,
// StBackoffWaiting, or StExiting. StIdle and StQuarantined are settled (no
// live child to terminate), and an untracked task (GetSMState ok=false) is
// likewise not reapable. A task already at StExiting is included so a reap
// confirmed against a still-terminating descriptor is idempotent, but
// reapRemovedDaemon's own re-check below avoids double-driving a terminate.
func (c *supervisorController) smStateIsReapable(taskName string) bool {
	st, ok := c.GetSMState(taskName)
	if !ok {
		return false
	}
	switch st {
	case api.StSpawning, api.StRunning, api.StBackoffWaiting, api.StExiting:
		return true
	}
	return false
}

// reapRemovedDaemon drives the SM-aware terminate for a descriptor that
// disappeared from intent and stayed absent across the verification window.
// It reuses the EXISTING terminate side-effect path — the same
// api.Transition row + executeSideEffect StExiting dispatch the operator-stop
// (EvIntentUpdate stopped) path uses — rather than a raw kill, so the
// audit trail (daemon-terminate-requested / daemon-terminated) and the
// Job-Object teardown stay identical to an operator-initiated stop.
//
// The standard handleLoopEvent path cannot be reused here because the
// descriptor has already been removed from intentCache, so handleLoopEvent's
// intent-lookup would drop the event as an orphan. Instead we run the SM
// transition explicitly against the CAPTURED OLD descriptor and dispatch its
// side effect. The descriptor carries the TaskName + Command the production
// TerminateFunc needs; the live PID it kills comes from the runtime tracker
// keyed by TaskName (makeProductionTerminateFnWithStatePath), so the captured
// (pre-removal) descriptor terminates the right child.
func (c *supervisorController) reapRemovedDaemon(d *api.SupervisorDaemon) {
	if c == nil || d == nil {
		return
	}
	taskName := canonicalSupervisorTaskName(d.TaskName)

	currentState, ok := c.GetSMState(taskName)
	if !ok {
		return
	}
	// Already terminating (StExiting) — a terminate is in flight; do not
	// double-drive it. The deliberate-stop / manual-restart path already owns
	// this child's exit, and the subsequent EvChildExit drives it to StIdle.
	if currentState == api.StExiting {
		return
	}
	if !c.smStateIsReapable(taskName) {
		return
	}

	// Build the stopped-intent SMContext so api.Transition takes the
	// terminate row for the current live state:
	//   StRunning        + EvIntentUpdate(stopped) -> StExiting (issue terminate)
	//   StSpawning       + EvIntentUpdate(stopped) -> StSpawning (queued_action=stop) then terminate on EvHealthOK/EvChildExit
	//   StBackoffWaiting + EvIntentUpdate(stopped) -> StIdle (cancel pending respawn)
	// A removed descriptor is, by definition, a stop request: the operator
	// deleted it from intent.
	smCtx := api.SMContext{
		IntentDesired:      api.IntentDesiredStopped,
		IntentIsActiveStop: true,
	}
	if c.graceful != nil {
		smCtx.GracefulInProgress = c.graceful.InProgress()
	}
	if v, ok := c.queuedActions.Load(taskName); ok {
		if qa, ok := v.(string); ok {
			smCtx.QueuedAction = qa
		}
	}

	newState, side, _, matched := api.Transition(currentState, api.EvIntentUpdate, smCtx)
	if !matched {
		// No terminate row for this state under stopped intent (e.g.
		// StQuarantined was excluded by smStateIsReapable already, so this is
		// defensive). Leave bookkeeping to the caller's clearRemovedTaskRuntime.
		return
	}

	c.emitReapEvent("info", "orphan-reap-terminate", taskName,
		"descriptor confirmed removed from intent across the verification window; driving SM-aware terminate so the orphaned child releases its port (supervisor-side backstop for a skipped install-time kill)")

	// Mirror handleLoopEvent's queued-action write-back for the new side
	// string so a StSpawning self-loop's queued_action=stop is preserved for
	// the subsequent EvHealthOK/EvChildExit (consistency with the operator
	// stop path).
	switch {
	case strings.Contains(side, "set queued_action=stop"):
		c.queuedActions.Store(taskName, "stop")
	case strings.Contains(side, "queued_action=respawn"):
		c.queuedActions.Store(taskName, "respawn")
	case strings.Contains(side, "queued_action=none"), strings.Contains(side, "clear queued_action"):
		c.queuedActions.Store(taskName, "")
	}
	c.smStates.Store(taskName, newState)
	if newState == api.StIdle && currentState != api.StIdle && c.tracker != nil {
		c.tracker.MarkExited(taskName)
	}
	if c.tracker != nil && c.statePath != "" {
		_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, taskName)
	}

	// Dispatch the side effect (the StExiting terminate fires c.terminate;
	// the StIdle/StSpawning rows are no-ops at executeSideEffect for a reap).
	_ = c.executeSideEffect(side, newState, d, api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: taskName})
}

// emitReapEvent logs a reconcile-source supervisor event for the orphan reap.
// Best-effort; emit failures are swallowed (the reap proceeds regardless).
func (c *supervisorController) emitReapEvent(severity, event, taskName, note string) {
	if c == nil || c.events == nil {
		return
	}
	_ = c.events.Emit(api.SupervisorEvent{
		Severity: severity,
		Source:   "reconcile",
		Event:    event,
		TaskName: taskName,
		Body:     map[string]any{"note": note},
	})
}

func (c *supervisorController) clearRemovedTaskRuntime(taskName string) {
	taskName = canonicalSupervisorTaskName(taskName)
	c.smStates.Delete(taskName)
	c.queuedActions.Delete(taskName)
	// Drop any pending orphan-reap candidate: a removal that reaches the
	// bookkeeping clear (either after a confirmed reap drove the terminate, or
	// because the task was never reapable) must not leave a stale pendingReap
	// entry that a later, unrelated refresh would re-evaluate. A re-registration
	// under the same task name starts the reap window fresh from a present
	// descriptor.
	c.pendingReap.Delete(taskName)
	// Drop the own-spawned marker: a re-registered task with the same name
	// must be reclassified from scratch so a stale "owned" entry does not
	// suppress a later genuinely-foreign-PID synthesize.
	//
	// Do NOT drop reaperOutstanding here. The premise "its old child is
	// gone" is FALSE during a race: the previous own child's cmd.Wait
	// reaper may still be live and will post the real EvChildExit. Dropping
	// ownSpawned flips the task to "foreign" immediately, but the
	// reaperOutstanding marker survives and keeps the StExiting synthesize
	// suppressed until that real exit is observed — without it, a terminate
	// in the re-registration window would synthesize a foreign EvChildExit
	// that races the still-pending real one and double-spawns (Codex
	// deep-sec PR #268 Conc-F3). The marker is cleared when handleLoopEvent
	// sees the real EvChildExit (including for a now-removed task, via the
	// early clear before the orphan-drop), so genuine removal does not leak
	// it and a subsequent foreign re-registration is still synthesizable.
	c.ownSpawned.Delete(taskName)
	if c.tracker != nil {
		c.tracker.Remove(taskName)
		if c.statePath != "" {
			_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, taskName)
		}
	}
	if c.events != nil {
		_ = c.events.Emit(api.SupervisorEvent{
			Severity: "debug",
			Source:   "reconcile",
			Event:    "controller-removed-intent-state-cleared",
			TaskName: taskName,
		})
	}
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

func (c *supervisorController) postIdleRespawnAndWait(taskName string, timeout time.Duration) error {
	if c == nil || c.eventLoop == nil {
		return errors.New("controller event loop unavailable")
	}
	resultCh := make(chan error, 1)
	c.eventLoop.Post(api.LoopEvent{
		Kind:     api.EvManualRestart,
		TaskName: taskName,
		Body: map[string]any{
			idleRespawnResultBodyKey: resultCh,
		},
	})

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case err := <-resultCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("timed out waiting for idle respawn state-machine transition")
	}
}

func idleRespawnResultChannel(ev api.LoopEvent) chan error {
	if ev.Body == nil {
		return nil
	}
	ch, _ := ev.Body[idleRespawnResultBodyKey].(chan error)
	return ch
}

func completeIdleRespawnEvent(ev api.LoopEvent, err error) {
	ch := idleRespawnResultChannel(ev)
	if ch == nil {
		return
	}
	select {
	case ch <- err:
	default:
	}
}

// postManualRestartAndWaitRunning drives a restart of a RUNNING daemon
// through the controller's state machine and waits until the respawn has
// re-fired (the SM is back at StRunning with a bumped PIDGeneration) or
// the timeout elapses. It is the controller-routed replacement for the
// IPC respawn handler's old direct terminate+spawn of a running daemon
// (Codex bot #268 P1, supervise_respawn.go:308).
//
// Posting EvManualRestart at StRunning drives StRunning -> StExiting
// (issue terminate, queued_action=respawn). The terminate fires the SAME
// production TerminateFunc the handler would have called directly; the
// child's real EvChildExit (clean exits now flow through crashCh too, per
// the wait-goroutine change for #268 P1) — or, for a foreign warm-start
// PID, the StExiting synthesize — then drives StExiting -> StSpawning
// (the respawn) via the queued action, all serialized on the single FIFO
// loop goroutine. Serializing terminate -> observe-exit -> respawn is
// exactly what closes the "old child's late exit drives backoff over the
// fresh PID" race the direct path had: the controller cannot start the
// new PID until it has consumed the old child's exit.
//
// A running-daemon restart both STARTS and ENDS at StRunning
// (StRunning -> StExiting -> StSpawning -> StRunning), so waiting for
// "state == StRunning" alone would match the INITIAL state and return
// before the restart even began — a real race, not just a test artifact,
// because the poll can win against the loop processing EvManualRestart.
// The completion signal must therefore detect that a NEW spawn actually
// fired, not merely that the daemon is running.
//
// We anchor on the tracker's PIDGeneration, which MarkSpawned increments
// on every (re)spawn and which terminate does NOT change. Capture the
// generation BEFORE posting EvManualRestart; success is "state ==
// StRunning AND PIDGeneration > the captured value" — true only after the
// terminate -> respawn cycle re-fired the spawn closure (which bumps the
// generation) and the controller PostSelf'd EvHealthOK to reach StRunning
// ("started == healthy" per the StSpawning success branch). A synchronous
// OK to the IPC caller then genuinely means the NEW PID is up. A spawn
// failure routes StSpawning -> StBackoffWaiting and never reaches
// StRunning, so the wait falls through to the timeout and the caller
// surfaces RESPAWN_FAILED — the honest outcome for a failed restart
// rather than a false success.
func (c *supervisorController) postManualRestartAndWaitRunning(taskName string, timeout time.Duration) error {
	if c == nil || c.eventLoop == nil {
		return errors.New("controller event loop unavailable")
	}
	startGen := c.trackerPIDGeneration(taskName)
	c.eventLoop.Post(api.LoopEvent{
		Kind:     api.EvManualRestart,
		TaskName: taskName,
	})

	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(timeout)
	for {
		if st, ok := c.GetSMState(taskName); ok && st == api.StRunning &&
			c.trackerPIDGeneration(taskName) > startGen {
			return nil
		}
		if !time.Now().Before(deadline) {
			st, _ := c.GetSMState(taskName)
			return fmt.Errorf("timed out waiting for running-daemon restart to respawn (state %s)", st)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// trackerPIDGeneration returns the tracker's recorded PIDGeneration for
// the task, or 0 when the tracker or the entry is absent. Monotonic per
// (re)spawn (MarkSpawned increments it; terminate / exit do not), so a
// strictly-greater value is an unambiguous "a new spawn fired" signal.
func (c *supervisorController) trackerPIDGeneration(taskName string) int {
	if c == nil || c.tracker == nil {
		return 0
	}
	if entry, ok := c.tracker.Get(taskName); ok {
		return entry.PIDGeneration
	}
	return 0
}

// hydrateSMStateFromTrackerIfMissing seeds the controller's smStates from
// the runtime tracker's persisted state when the controller has NO SM
// state for the task yet (GetSMState ok=false). This happens after a
// supervisor cold restart: the tracker is hydrated from
// supervisor-state.json (so it can report quarantine / backoff) before
// the controller has observed any event for the task, leaving smStates
// empty.
//
// Without this seeding, a forced respawn of a quarantined (or backoff)
// task would route the EvManualRestart through the StIdle transition
// (GetSMState defaults a missing entry to StIdle), which does NOT reset
// the failure window. The daemon could then immediately re-quarantine
// off the stale crash window even though the operator used the
// force-restart recovery path (Codex bot #268 P2, supervise_respawn.go:75).
// Seeding StQuarantined / StBackoffWaiting makes the SM take the
// "reset failures, ..." transition for a forced restart.
//
// Returns the effective SM state (the existing one when present, or the
// freshly-seeded one). Running tracker state is NOT seeded here — a
// running daemon is restarted via the terminate-first manual-restart
// path, not the non-running respawn router.
func (c *supervisorController) hydrateSMStateFromTrackerIfMissing(taskName string) api.SMState {
	if c == nil {
		return api.StIdle
	}
	if st, ok := c.GetSMState(taskName); ok {
		return st
	}
	seeded := api.StIdle
	if c.tracker != nil {
		if entry, ok := c.tracker.Get(taskName); ok {
			switch entry.State {
			case daemonRuntimeStateQuarantine:
				seeded = api.StQuarantined
			case daemonRuntimeStateBackoff:
				seeded = api.StBackoffWaiting
			}
		}
	}
	c.smStates.Store(taskName, seeded)
	return seeded
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
	// Any EvChildExit reaching the controller is an observation that the
	// task's real own reaper (if it had one) has fired its exit — clear
	// the reaperOutstanding marker so a subsequent terminate is free to
	// synthesize a foreign exit if the task is genuinely foreign now. Done
	// BEFORE the intent lookup so a real exit for a task already removed
	// from intent (orphan-drop path below) still clears the marker rather
	// than leaking it (Codex deep-sec PR #268 Conc-F3). A re-spawn driven
	// by this same EvChildExit re-sets reaperOutstanding in
	// executeSideEffect's spawn-success branch, so this clear cannot strand
	// a freshly-respawned child. Harmless for synthetic / foreign exits:
	// those tasks have no reaperOutstanding entry, so Delete is a no-op.
	if ev.Kind == api.EvChildExit {
		c.reaperOutstanding.Delete(ev.TaskName)
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
		completeIdleRespawnEvent(ev, fmt.Errorf("task %s not found in controller intent cache", ev.TaskName))
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
	// Deliberate-shutdown contract: a CLEAN child exit (exit 0, no wait
	// error) observed while the task is still StRunning means the daemon
	// shut down on its own with NO controller-driven exit in flight
	// (e.g. `mcphub stop` already drove the SM to StExiting before the
	// exit; an external clean kill at steady state did not). Per the
	// long-standing contract (supervise.go wait goroutine) such a clean
	// exit must NOT be respawned. The wait goroutine now posts clean
	// exits too (so a controller-driven restart's queued respawn can
	// complete at StExiting — Codex bot #268 P1), so the drop has to
	// happen HERE rather than at the channel gate. Only StRunning is
	// dropped: at StExiting (controller-driven restart) the clean exit
	// MUST drive the queued respawn, and at every other state the SM
	// already routes EvChildExit correctly. reaperOutstanding was
	// already cleared above (the reaper genuinely fired), so dropping the
	// event here does not strand the marker.
	//
	// We suppress the RESPAWN (no auto-restart of a deliberately-stopped
	// daemon) but STILL drive the SM state to StIdle before returning. The
	// reaper already marked the runtime tracker idle / current_pid=0 BEFORE
	// posting this event (supervise.go cmd.Wait: MarkExited+persist precede
	// the crashCh post), so leaving smStates at StRunning would desync the
	// SM from the tracker. A later /api/daemon/respawn then takes the
	// non-running path, but shouldRouteNonRunningRespawnThroughController
	// rejects a stale StRunning as "not spawnable without a live PID" — the
	// operator could not restart the daemon until the supervisor restarted.
	// Storing StIdle (which matches the idle tracker) makes that non-running
	// respawn route through the idle transition normally (Codex bot #268 P2,
	// supervisor_controller.go:664).
	if ev.Kind == api.EvChildExit && currentState == api.StRunning && supervisorEventIsCleanExit(ev) {
		c.smStates.Store(ev.TaskName, api.StIdle)
		if c.events != nil {
			_ = c.events.Emit(api.SupervisorEvent{
				Severity: "debug",
				Source:   "lifecycle",
				Event:    "controller-clean-exit-ignored-running",
				TaskName: ev.TaskName,
				Body: map[string]any{
					"note": "clean child exit at StRunning with no controller-driven exit in flight; deliberate-shutdown contract suppresses respawn, SM driven to idle to match the already-idle tracker so a later manual respawn can route",
				},
			})
		}
		return
	}
	// Liveness restarts for a dead-PID reason carry a clear instruction
	// (supervisorLivenessRuntimeClearedBodyKey). The actual MarkExited +
	// persist now happens HERE on the event-loop goroutine instead of in
	// the sweep goroutine, so the tracker mutation + supervisor-state.json
	// write for the task stay single-writer and cannot race a concurrent
	// handler MarkSpawned/MarkExited+persist (Codex deep-sec PR #268
	// Conc-F2). The clear is idempotent — MarkExited on an already-idle
	// entry is a no-op-equivalent — so a sweep that pre-cleared (older
	// callers / tests) still lands on the same state. After clearing, the
	// SM treats the verified-idle tracker state as spawnable so the
	// EvManualRestart goes through the SM without firing the terminate
	// side effect against a dead or recycled PID.
	if supervisorLivenessRestartClearedRuntime(ev) && c.tracker != nil {
		c.tracker.MarkExited(ev.TaskName)
		if c.statePath != "" {
			_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, ev.TaskName)
		}
		if currentState == api.StRunning {
			if entry, ok := c.tracker.Get(ev.TaskName); ok && entry.State == daemonRuntimeStateIdle && entry.CurrentPID == 0 {
				currentState = api.StIdle
			}
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
		completeIdleRespawnEvent(ev, fmt.Errorf("no state-machine transition for %s from %s", ev.Kind, currentState))
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
	if strings.Contains(side, "reset failures") && c.tracker != nil {
		c.tracker.ClearCrashes(ev.TaskName)
	}
	// Sync tracker runtime state when SM transitions to StIdle from a
	// non-idle state. Without this, persistDaemonRuntimeTracker below
	// would write supervisor-state.json with stale tracker fields
	// (e.g. state="backoff-waiting" + CurrentPID=N) while the SM
	// itself reports state="idle". The mismatch is operator-visible
	// in mcphub status and the GUI Dashboard. Closes bot finding on
	// PR #236 db988e0 (StBackoffWaiting + EvTimerDue intent re-check
	// path persists tracker before tracker is synced to idle).
	if newState == api.StIdle && currentState != api.StIdle && c.tracker != nil {
		c.tracker.MarkExited(ev.TaskName)
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

	sideEffectErr := c.executeSideEffect(side, newState, d, ev)
	if idleRespawnResultChannel(ev) != nil {
		if sideEffectErr == nil && !strings.Contains(side, "create-process") {
			// The SM refused to spawn. When the refusal is specifically
			// the stopped-intent gate (StIdle+EvManualRestart with
			// IntentDesired=stopped → side "RESTART_REFUSED_INTENT_STOPPED"),
			// complete with the typed sentinel so handleRespawn surfaces the
			// DISTINCT RESPAWN_REFUSED_INTENT_STOPPED code rather than the
			// generic RESPAWN_FAILED. The distinct code lets the restart
			// caller recover (write Desired=running, retry once) without
			// bypassing the QUARANTINED force-gate (#279 fable N1).
			if side == "RESTART_REFUSED_INTENT_STOPPED" {
				sideEffectErr = errIdleRespawnRefusedIntentStopped
			} else {
				sideEffectErr = fmt.Errorf("idle respawn did not spawn: %s", side)
			}
		}
		completeIdleRespawnEvent(ev, sideEffectErr)
	}
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
) error {
	if c == nil || d == nil {
		return nil
	}

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
			return nil
		}

		// bot PR #246 r2 P2: refuse to fire spawn for a legacy nil-RuntimeSpec
		// serena-proxy descriptor. Such a row reaches StSpawning via the
		// EvChildExit -> backoff -> EvTimerDue restart path when a row that was
		// ALREADY RUNNING at upgrade later exits - the THIRD spawn path (the
		// reconcile pass excludes not-running rows; supervise_respawn.go refuses
		// IPC respawns). The redesigned proxy fails loud on a nil spec (its args
		// lack --task-name), so firing spawn would churn restart backoff on a row
		// that can never start. Quarantine directly (mirrors handleBackoffWaiting's
		// promotion) so the restart loop stops. Spec-bearing serena-proxy rows and
		// global daemons are unaffected. StExiting (terminate) is a separate case,
		// so an operator stop of a running legacy row is still honored.
		if isSerenaProxyDescriptor(*d) && d.RuntimeSpec == nil {
			c.smStates.Store(ev.TaskName, api.StQuarantined)
			if c.tracker != nil {
				c.tracker.MarkQuarantined(ev.TaskName)
			}
			if c.tracker != nil && c.statePath != "" {
				_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, ev.TaskName)
			}
			if c.events != nil {
				_ = c.events.Emit(api.SupervisorEvent{
					Severity: "warn",
					Source:   "lifecycle",
					Event:    "legacy-serena-descriptor-quarantined",
					TaskName: ev.TaskName,
					Body: map[string]any{
						"server": d.Server,
						"reason": "serena-proxy descriptor carries no runtime_spec (pre-redesign / stale row) and cannot be spawned; quarantined to stop the restart loop instead of churning backoff",
						"action": "run the serena dynamic-pool re-install/migrate to re-materialize this descriptor with a runtime_spec",
					},
				})
			}
			return errors.New("legacy serena-proxy descriptor carries no runtime_spec")
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
		if c.spawn == nil {
			return errors.New("spawn function unavailable")
		}
		err := c.spawn(*d)
		if err == nil {
			// This controller now owns a live child for the task: the
			// production spawn closure launched it AND its cmd.Wait/reaper
			// goroutine, which posts the real EvChildExit on exit. Record
			// own-spawned so the StExiting terminate path does NOT
			// synthesize a duplicate EvChildExit for this task (Codex bot
			// #268 r11 P2). A previously-foreign warm-start PID flips to
			// own-spawned here after its first terminate-then-respawn, so
			// its SECOND restart relies on the real exit event.
			c.ownSpawned.Store(d.TaskName, true)
			// A fresh real reaper is now live and WILL post a real
			// EvChildExit when this child exits. Mark it outstanding so the
			// synthesize gate suppresses a foreign EvChildExit even if a
			// concurrent intent re-registration drops ownSpawned before the
			// real exit arrives (Codex deep-sec PR #268 Conc-F3). Cleared
			// when handleLoopEvent observes the real EvChildExit.
			c.reaperOutstanding.Store(d.TaskName, true)
		}
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
		return err

	case api.StBackoffWaiting:
		c.handleBackoffWaiting(d, ev)
		return nil

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
		return nil

	case api.StExiting:
		// "issue terminate, queued_action=*" - fire the terminate
		// closure. For OWN-spawned children the terminate fn's audit
		// rows + the cmd.Wait goroutine's real EvChildExit drive the
		// next transition, so the SM owns retry when the child actually
		// exits and we do nothing more here.
		//
		// For a FOREIGN warm-start PID (one this supervisor never
		// spawned — typically the live-but-port-stale handoff from a
		// previous supervisor, supervise_liveness.go:179) there is NO
		// cmd.Wait goroutine in this process, so a successful terminate
		// produces no follow-up EvChildExit and the SM would wedge in
		// StExiting with queued_action=respawn never consumed. Synthesize
		// the EvChildExit so StExiting -> consume queued respawn ->
		// StSpawning -> single respawn completes (Codex bot #268 r11 P2).
		if c.terminate == nil {
			return nil
		}
		terminateDescriptor := descriptorForTerminateSideEffect(d, ev)
		termErr := c.terminate(*terminateDescriptor)
		// Synthesize ONLY when ALL of:
		//   (a) the task is foreign (not own-spawned, no own cmd.Wait — else
		//       we double-emit against the real exit event), AND
		//   (b) no real own reaper is still outstanding for this task — a
		//       concurrent intent re-registration can drop ownSpawned
		//       (clearRemovedTaskRuntime) WHILE the previous own child's
		//       reaper is still live and will post the real EvChildExit;
		//       synthesizing here would race that real exit and double-spawn
		//       (Codex deep-sec PR #268 Conc-F3). reaperOutstanding survives
		//       the ownSpawned drop and is cleared only when the real exit is
		//       observed, so it closes the re-registration window that the
		//       ownSpawned boolean alone cannot, AND
		//   (c) terminate returned nil — the production TerminateFunc returns
		//       nil only on paths where the targeted PID is GONE (already-
		//       dead, identity-mismatch/reuse, or confirmed-terminated),
		//       never while the live daemon is still running. On a terminate
		//       failure we leave the entry for the next liveness sweep /
		//       retry rather than respawning over a possibly-live process.
		_, owned := c.ownSpawned.Load(d.TaskName)
		_, reaperPending := c.reaperOutstanding.Load(d.TaskName)
		if termErr == nil && !owned && !reaperPending {
			c.synthesizeForeignChildExit(d, ev)
		}
		return nil

	case api.StRunning, api.StIdle:
		// Steady / no-op states. No side effect required.
		// StRunning is reached on EvHealthOK (clears the spawning
		// gate). StIdle is reached on EvChildExit while exiting,
		// graceful drain, or initial reconcile of a stopped intent.
		return nil
	}
	return nil
}

func descriptorForTerminateSideEffect(d *api.SupervisorDaemon, ev api.LoopEvent) *api.SupervisorDaemon {
	if d == nil || ev.Kind != api.EvManualRestart || ev.Body == nil {
		return d
	}
	if oldDescriptor, ok := ev.Body[reconcileManualRestartTerminateDescriptorBodyKey].(*api.SupervisorDaemon); ok && oldDescriptor != nil {
		return oldDescriptor
	}
	return d
}

// synthesizeForeignChildExit posts the follow-up EvChildExit for a
// FOREIGN warm-start PID after its StExiting terminate succeeded. Such a
// PID was inherited from a previous supervisor (hydrated into smState=
// StRunning by hydrateControllerRunningStates) and has NO cmd.Wait/reaper
// goroutine in this process, so nothing else will ever post the
// EvChildExit that StExiting needs to consume queued_action=respawn. The
// synthetic event drives StExiting -> StSpawning -> single respawn,
// completing the warm-start restart (Codex bot #268 r11 P2).
//
// Caller contract (enforced at the StExiting call site): invoked ONLY
// when terminate returned nil (the targeted PID is gone — no race against
// a live process or a late real exit) AND the task is NOT own-spawned AND
// no real own reaper is outstanding for the task (reaperOutstanding
// absent). The own-spawned + reaperOutstanding pair together guarantee
// there is no real EvChildExit still pending to double up against, even
// across an intent re-registration that dropped ownSpawned mid-flight
// (Codex deep-sec PR #268 Conc-F3).
//
// Uses PostSelf, not Post: this runs inside handleLoopEvent (a registered
// loop handler), and an inline Post from a handler can deadlock on a full
// buffer / lose FIFO priority to pre-queued external events. PostSelf
// lands on the priority channel and is drained before the next external
// event — the same discipline the StSpawning success branch uses for its
// EvHealthOK / pre-child EvChildExit self-posts. On selfCh saturation
// (should never happen at production cap) the saturated-audit row fires
// and the next liveness sweep re-drives the restart.
func (c *supervisorController) synthesizeForeignChildExit(d *api.SupervisorDaemon, ev api.LoopEvent) {
	if c == nil || d == nil || c.eventLoop == nil {
		return
	}
	if c.events != nil {
		reason, _ := ev.Body["reason"].(string)
		_ = c.events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "daemon-foreign-exit-synthesized",
			TaskName: d.TaskName,
			Body: map[string]any{
				"reason": reason,
				"note":   "foreign warm-start PID terminated with no cmd.Wait in this supervisor; synthesizing EvChildExit so the queued respawn completes",
			},
		})
	}
	if !c.eventLoop.PostSelf(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName}) {
		c.emitSelfChannelSaturated(d.TaskName, "EvChildExit")
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
			// clean_exit lets handleLoopEvent distinguish a deliberate
			// clean shutdown (exit 0, no wait error) from a crash. The
			// controller honors a clean exit ONLY when a controller-driven
			// exit is in flight (state != StRunning); at StRunning it is
			// dropped to preserve the deliberate-shutdown contract (Codex
			// bot #268 P1, supervise.go wait goroutine). Non-clean exits
			// leave the flag false and route through the crash/backoff
			// path unchanged.
			body[supervisorCleanExitBodyKey] = ev.ExitCode == 0 && ev.WaitErr == nil
			loop.Post(api.LoopEvent{
				Kind:     api.EvChildExit,
				TaskName: ev.Daemon.TaskName,
				Body:     body,
			})
		}
	}
}

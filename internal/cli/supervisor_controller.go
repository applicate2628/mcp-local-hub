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
	pendingReap sync.Map // taskName -> *api.SupervisorDaemon (absent-once, awaiting still-absent confirmation)
	// reapShadow keeps the captured descriptor of a task that is CURRENTLY in
	// the pendingReap window AVAILABLE to handleLoopEvent even though it has
	// been removed from intentCache. It is the "reaping shadow" that closes the
	// orphan-drop common root: refreshSupervisorIntent swaps intentCache to the
	// post-removal snapshot, so without a shadow every in-flight event for the
	// removed task (EvChildExit from the child's own reaper, EvTimerDue from an
	// armed backoff timer, the EvHealthOK/EvChildExit completing a queued stop
	// on a StSpawning task) would miss intentCache.Lookup and be orphan-dropped
	// — leaving the SM wedged (stale StRunning over a dead child, a backoff that
	// never re-arms, a queued stop deleted before the spawn completes). With the
	// shadow, handleLoopEvent.Lookup falls back to it so those events ROUTE
	// NORMALLY through the SM. An entry lives for exactly the pendingReap window:
	// stored when a live task is first marked pendingReap, deleted when the reap
	// is confirmed-and-terminated, when the descriptor re-appears, or when the
	// task settles to a non-live SM state. Subsumes findings 3/4/5.
	reapShadow sync.Map // taskName -> *api.SupervisorDaemon (descriptor kept routable during the pendingReap window)
	// reapFollowupArmed records, per task, the GENERATION of the bounded follow-up
	// timer currently armed for the task's pendingReap resolution. A second mark
	// within the same window does not arm a duplicate timer (the entry is already
	// present), and the recorded generation lets a fired EvReapFollowup event
	// detect that it is STALE — its generation no longer matches the live
	// pendingReap generation, e.g. after a remove→reappear→remove-again for the
	// same task armed a fresh timer (Codex pr302 r3 finding C). Mutated ONLY on
	// the event-loop goroutine (markPendingReap / arm / disarm all run inside loop
	// handlers now), so no lock is needed beyond the sync.Map's own.
	reapFollowupArmed sync.Map // taskName -> int (generation of the armed timer)
	// reapTimers holds the live timer handle per task so disarmReapFollowup can
	// Stop() it — a bare armed-flag clear left the underlying time.AfterFunc
	// running, and a stale fire during a SECOND verification window for the same
	// task could confirm/terminate the second removal early (Codex pr302 r3
	// finding C). Stopping the timer on disarm AND generation-tagging the fired
	// event together close the stale-timer window. Mutated ONLY on the event loop.
	reapTimers sync.Map // taskName -> reapTimer
	// reapGeneration is the monotonic per-task generation assigned to each NEW
	// pendingReap window. Bumped (on the event loop) every time a task is freshly
	// marked pendingReap; the value is copied into reapFollowupArmed when the
	// timer arms and into the posted EvReapFollowup body. A fired follow-up whose
	// body generation != the live pendingReap generation is a stale no-op (finding
	// C). Mutated ONLY on the event loop.
	reapGeneration sync.Map // taskName -> int
	// reapDeferredTimerDue records, per task, that a StBackoffWaiting daemon's
	// armed backoff timer FIRED an EvTimerDue while the task was pendingReap and
	// the controller SUPPRESSED the respawn it would otherwise have driven (Codex
	// pr302 r3 finding E: respawning a removed backoff daemon during the
	// verification window binds a port the user just removed — a transient orphan
	// / port-collision). The marker lets the controller REPLAY that single dropped
	// EvTimerDue if the removal turns out to be a replace-in-place BLIP (the
	// descriptor reappears), so a blip'd backoff daemon is NOT stranded (the
	// anti-stranding invariant of the original finding 5). It is consumed (replay
	// + delete) on reappear and dropped on confirmed removal / clear. Mutated ONLY
	// on the event loop.
	reapDeferredTimerDue sync.Map // taskName -> bool
	// reapDeferredStop records, per task, that the orphan reap drove a StSpawning
	// daemon to a deferred queued_action=stop (the terminate is applied later on
	// the spawn-completion event). If the descriptor then REAPPEARS before the
	// spawn completes (replace-in-place), the cancel path must also clear that
	// reap-originated queued stop — otherwise the next EvHealthOK terminates the
	// just-re-added daemon the operator wants RUNNING (Codex pr302 r3 finding D).
	// The marker distinguishes a REAP-set stop (safe to clear on reappear) from an
	// operator-set stop (must be honored), so the cancel never silently drops a
	// genuine operator stop. Set in reapRemovedDaemon's StSpawning branch; consumed
	// (clear stop + delete marker) on reappear; dropped on confirmed clear. Mutated
	// ONLY on the event loop.
	reapDeferredStop sync.Map // taskName -> bool
	events           *api.SupervisorEventLog
	graceful         *gracefulCounter
	daemonIntent     *daemonIntentCache

	// reapFollowupDelay is the bounded delay before a marked-but-unconfirmed
	// pendingReap is forcibly re-evaluated against a FRESH on-disk intent read,
	// independent of any further intent-file mtime change. The two
	// refreshSupervisorIntent call sites (the 60s IntentWatcher onChange and
	// applyReconcileDrift) only fire on an intent CHANGE; in the common
	// uninstall/remove case intent then stays STABLE, so the confirming second
	// refresh would NEVER arrive and the orphan would never be terminated
	// (finding 1). When a pendingReap is marked, the controller arms a timer of
	// this duration that POSTS an EvReapFollowup event onto the loop, where the
	// handler re-reads intent FROM DISK and resolves only that task's reap, so the
	// terminate fires within a bounded window even if intent mtime never changes
	// again. Defaults to reapFollowupDefaultDelay; tests shrink it (or inject
	// reapAfterFunc) for deterministic, real-clock-free assertions.
	reapFollowupDelay time.Duration
	// reapAfterFunc is the injectable timer seam used to arm the follow-up
	// resolution. Defaults to time.AfterFunc (production). Tests install a
	// controllable fake so the follow-up tick fires synchronously under a fake
	// clock instead of waiting on the wall clock. The callback the controller
	// passes does NOT mutate controller state directly anymore — it only POSTS an
	// EvReapFollowup event onto the event loop, so the actual resolution runs
	// serialized in handleLoopEvent (Codex pr302 r3 findings A/B/C/H/I).
	reapAfterFunc func(d time.Duration, f func()) reapTimer
	// reapIntentReader is the injectable fresh-from-disk intent reader the
	// follow-up handler uses to re-confirm a pendingReap's absence against the
	// CURRENT on-disk supervisor-intent.json rather than the possibly-stale
	// intentCache snapshot (Codex pr302 r3 finding A: the cache only refreshes on
	// the 60s IntentWatcher poll, so after a remove→re-add-on-disk within the
	// window the cache stays empty up to 60s and a cache-only follow-up would
	// terminate a live re-declared daemon). Defaults to reading
	// <dir(statePath)>/supervisor-intent.json via api.ReadSupervisorIntent; tests
	// inject a fake. A nil reader (or a read error) means the follow-up falls back
	// to the intentCache snapshot — strictly no worse than the pre-fix behavior.
	reapIntentReader func() (*api.SupervisorIntentFile, error)

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

	// F1 pre-spawn port-owner gate (decision D-A) — split across the loop and a
	// dedicated off-loop worker so the loop NEVER runs the blocking classify/reap
	// (codex-P1: WMI identity lookup + 5s terminate-wait on the event loop would
	// freeze the whole fleet). Before a StBackoffWaiting+EvTimerDue respawn or a
	// force-respawn (EvManualRestart) fires create-process:
	//   - the LOOP runs ONLY the fast, deadline-bounded owner probe (portOwnerFn).
	//     Port free / probe error → spawn as today. Port owned → hold the daemon
	//     in backoff (no crash increment) and hand the request to the worker.
	//   - the WORKER (runPortGateWorker, exactly one goroutine) runs the identity
	//     classify + identity-gated reap and maps the outcome back to the loop via
	//     an EvManualRestart post (+ the gateCleared one-shot flag for the
	//     unverified→proceed contract).
	//
	// portOwnerFn is the per-port owner probe. Production wires it to a
	// short-deadline api.LoopbackPortOwnerPIDContext closure so a wedged netstat
	// cannot hang the loop; nil DISABLES the gate (spawn as today) for the
	// direct-construction controller tests. squatterLimiter is F1's rate-limit
	// budget, owned SOLELY by the worker goroutine (the loop must never touch it),
	// so it stays single-goroutine-owned + lock-free; it is a SEPARATE instance
	// from the liveness sweep's limiter (P2a + F3). portGateCh carries requests
	// loop→worker (buffered; a full channel drops the dispatch and the 30s backoff
	// timer re-probes). gateCleared is the worker→loop one-shot short-circuit that
	// lets an unverified-owned re-probe spawn WITHOUT re-running the gate. All are
	// wired only in the production runSupervise path (reconcileSpawnFn==nil).
	squatterLimiter *squatterReapLimiter
	portOwnerFn     func(port int) (pid int, ok bool, err error)
	portGateCh      chan portGateReq
	gateCleared     sync.Map // canonical taskName -> struct{} (one-shot worker→loop proceed flag)
	// portGateInFlight dedupes worker requests per task: at most one dispatch for a
	// given task is queued/processing at a time, so a delayed worker + a re-firing
	// 30s backoff timer cannot pile up duplicate requests whose stale results would
	// restart an already-recovered daemon or leave a stale gateCleared bypass
	// (Codex PR-3 round-2 P2). Set on a successful dispatch, cleared when the worker
	// finishes. Combined with the worker's port re-probe staleness guard.
	portGateInFlight sync.Map // canonical taskName -> struct{}

	// --- Ephemeral-collision self-heal (L1) wiring ---------------------------
	// reallocCh carries loop→worker port-REALLOCATION requests (a dynamic-pool
	// proxy that exited exitBindRefused because a foreign process stole its
	// port). Mirrors portGateCh: buffered, drained by a single off-loop worker
	// (runReallocWorker) that runs the blocking AllocatePort + atomic registry/
	// intent re-persist (reallocFn) OFF the loop and posts the outcome back via
	// evReallocApplied. Wired only in the production runSupervise path
	// (reconcileSpawnFn==nil); nil DISABLES the self-heal (spawn-as-today).
	reallocCh chan reallocReq
	// reallocInFlight dedupes reallocation requests per task (mirrors
	// portGateInFlight): at most one reallocation is queued/processing per task
	// at a time, so the armed fallback timer + a re-firing bind-refused exit
	// cannot pile up duplicate reallocations.
	reallocInFlight sync.Map // canonical taskName -> struct{}
	// reallocFn performs the OFF-LOOP atomic port move for one descriptor:
	// resolve the pool PER descriptor, then under the registry flock
	// AllocatePort → write the registry row (new port) → write the
	// supervisor-intent descriptor (Port + --port argv + serena
	// RuntimeSpec.External/UpstreamPort) as ONE atomic temp+rename, and return
	// the new port. It returns api.ErrPortPoolExhausted (wrapped) when the pool
	// is full. Wired in runSupervise; nil in direct-construction tests (the
	// worker then no-ops and the fallback timer re-drives the retry).
	reallocFn func(d api.SupervisorDaemon) (newPort int, err error)
	// reallocForeignHolderFn best-effort resolves (pid, basename) of the foreign
	// process holding a stolen port, for the L3 event's REDACTED foreign_holder
	// (PID + basename ONLY — never a command line/secrets). Runs on the off-loop
	// worker (it may spawn a WMI/PowerShell identity probe). nil / pid<=0 → the
	// event omits foreign_holder.
	reallocForeignHolderFn func(port int) (pid int, basename string)
	// ephemeralRangeContainsFn best-effort reports whether a port falls inside
	// the OS TCP ephemeral (dynamic) range, for the L3 event's
	// inside_ephemeral_range field. Production wires a CACHED netsh probe so it
	// is cheap after warmup; nil → the field is omitted.
	ephemeralRangeContainsFn func(port int) (inRange bool, known bool)
	// reallocDwell tracks, per task, the stabilize-dwell clock used to reset the
	// reallocation window + crash window ONLY after a reallocated daemon has
	// dwelt continuously in StRunning past reallocationStabilizeDwell — the
	// DWELL-GATE that stops a bind-refused daemon (which transits StRunning
	// BEFORE it exits on the bind fail) from forever-resetting its own counter.
	// Loop-owned (created on interception, advanced/cleared by the dwell tick on
	// the loop goroutine).
	reallocDwell sync.Map // canonical taskName -> *reallocDwellEntry
	// bindAccessDeniedTerminalEmitted dedupes the terminal (give-up) L3 events
	// (quarantined-realloc-cap-exhausted for proxies, quarantined-run-host-remedy
	// for fixed globals) to ONE per episode. Cleared when the daemon stabilizes
	// (dwell reset) or leaves quarantine (ClearCrashes site), so a later episode
	// re-emits. Loop-owned.
	bindAccessDeniedTerminalEmitted sync.Map // canonical taskName -> struct{}

	// quarantineParole is the F2 in-memory parole ladder for threshold-quarantine
	// daemons (map[canonicalTaskName]*quarantineParoleEntry). A daemon that hits
	// the failure threshold used to stay quarantined "until supervisor restart" —
	// an external kill-storm (or the false-mismatch bursts F6 fixes) then wedged
	// it forever with no self-recovery. Parole gives a quarantined daemon an
	// automatic EvManualRestart after a bounded cooldown (15m, then exponential to
	// a 2h ceiling), clearing the quarantine if it stabilizes and re-quarantining
	// on the preserved ladder if it re-fails.
	//
	// Ownership discipline (entirely loop-owned): the parole map AND every entry
	// field are read and mutated ONLY on the event-loop goroutine, so nothing ever
	// races. Entries are CREATED (create-if-absent, ladder-preserving) via
	// recordQuarantineParoleEligible, DELETED at the absorbing-quarantine store
	// sites (clearQuarantineParole, class-flip) and by the parole tick (stabilize /
	// recovery / prune), and their fields advanced by the tick — every one of those
	// on the loop. The tick used to run on the parole monitor goroutine, which split
	// its map Delete + field mutation OFF the loop from the on-loop record/clear and
	// left a real (low-probability) TOCTOU: a graduation Delete could erase a just-
	// recorded threshold eligibility, wedging the daemon at StQuarantined with no
	// ladder. The monitor now only POSTS evParoleTick and the tick runs on the loop
	// (runQuarantineParoleTick, dispatched from handleLoopEvent), so record/clear and
	// Delete/field-advance are serialized by construction. The tick reads smStates +
	// daemonIntent (both already loop-consistent) and the ONLY supervisor-state
	// mutation it drives is the EvManualRestart it self-posts onto the loop —
	// preserving Conc-F2/Conc-F4 (it posts EvManualRestart, never EvTimerDue, and
	// adds no SM row). In-memory only: never persisted (no supervisor-state.json
	// schema change), resets on cold restart by design.
	quarantineParole sync.Map // canonicalTaskName -> *quarantineParoleEntry
}

// quarantineParoleEntry is the per-task F2 parole ladder state (in-memory only).
type quarantineParoleEntry struct {
	// attempts counts parole EvManualRestart posts fired so far; it drives the
	// exponential cooldown and is PRESERVED across a re-quarantine so a repeatedly-
	// failing daemon backs off rather than tight-looping at the base delay.
	attempts int
	// nextAttemptAt is the earliest wall-clock time the next parole may fire.
	nextAttemptAt time.Time
	// healthySince is the time the daemon was first observed continuously in
	// StRunning since its last parole; once it dwells past
	// quarantineParoleStabilizeDwell the ladder is reset (entry deleted). Zeroed
	// whenever the daemon is observed outside StRunning.
	healthySince time.Time
}

const (
	// quarantineParoleBaseDelay is the cooldown before the FIRST parole attempt
	// after a daemon enters threshold-quarantine.
	quarantineParoleBaseDelay = 15 * time.Minute
	// quarantineParoleMaxDelay caps the exponential parole cooldown ceiling so a
	// persistently-failing daemon retries at most this often (bounded, not a tight
	// loop). Within the report's 2-4h band.
	quarantineParoleMaxDelay = 2 * time.Hour
	// quarantineParoleStabilizeDwell is how long a paroled daemon must stay
	// continuously in StRunning before its parole ladder is reset (treated as
	// recovered). A daemon that reaches StRunning only briefly before re-failing
	// does NOT reset the ladder, so re-quarantine stays on the exponential
	// schedule.
	quarantineParoleStabilizeDwell = 2 * time.Minute
	// quarantineParoleTickInterval is the parole monitor's scan cadence. It is far
	// shorter than the base delay, so the ~cooldown granularity is bounded by this
	// tick, not by the tick itself firing paroles.
	quarantineParoleTickInterval = 30 * time.Second
)

// quarantineParoleDelay returns the parole cooldown for the given prior-attempt
// count: base * 2^attempts, capped at quarantineParoleMaxDelay. attempts<=0
// yields the base delay.
func quarantineParoleDelay(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	d := quarantineParoleBaseDelay
	for i := 0; i < attempts && d < quarantineParoleMaxDelay; i++ {
		d *= 2
	}
	if d > quarantineParoleMaxDelay {
		d = quarantineParoleMaxDelay
	}
	return d
}

// recordQuarantineParoleEligible marks a task as parole-eligible when it enters
// THRESHOLD quarantine (called from the two threshold-quarantine sites only —
// NOT the strict-job-protection or legacy-serena-nil-spec quarantines, which are
// deliberately absorbing per their own contracts). Create-if-absent: an existing
// entry (a task re-quarantining while still on the ladder) is left untouched so
// the exponential cooldown is preserved across the re-quarantine. Called on the
// event-loop goroutine.
func (c *supervisorController) recordQuarantineParoleEligible(taskName string, now time.Time) {
	if c == nil {
		return
	}
	key := canonicalSupervisorTaskName(taskName)
	c.quarantineParole.LoadOrStore(key, &quarantineParoleEntry{
		attempts:      0,
		nextAttemptAt: now.Add(quarantineParoleDelay(0)),
	})
}

// clearQuarantineParole drops any parole ladder entry for a task. Called at the
// ABSORBING quarantine store sites (strict-job-protection / legacy-serena-nil-
// spec) so a class-flip — a threshold-quarantined daemon whose later parole
// respawn hits an absorbing refusal — removes the now-stale threshold entry and
// stops being paroled into a permanent spawn-refuse loop (commission P2-2). Runs
// on the event-loop goroutine (the absorbing store sites are handleLoopEvent
// side-effects), the SAME goroutine as recordQuarantineParoleEligible and the
// parole tick, so nothing here is concurrent with them — the map is entirely
// loop-owned (see the quarantineParole ownership doc).
func (c *supervisorController) clearQuarantineParole(taskName string) {
	if c == nil {
		return
	}
	c.quarantineParole.Delete(canonicalSupervisorTaskName(taskName))
}

// runQuarantineParoleMonitor drives the parole scan on a fixed cadence until ctx
// is canceled. Production wiring starts it once per supervisor (runSupervise); it
// is the F2 counterpart of the liveness monitor goroutine. It does NOT run the
// scan itself — it only POSTS evParoleTick onto the event loop, so the scan (and
// all parole map mutation it performs) runs ON the loop goroutine, serialized
// against recordQuarantineParoleEligible / clearQuarantineParole. The post is
// best-effort non-blocking: a dropped post on a momentarily-full buffer just
// delays that scan by one interval (parole is not latency-critical), which is
// strictly safer than blocking the monitor on a wedged loop.
func (c *supervisorController) runQuarantineParoleMonitor(ctx context.Context) {
	if c == nil || c.eventLoop == nil {
		return
	}
	ticker := time.NewTicker(quarantineParoleTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.eventLoop.TryPost(api.LoopEvent{Kind: evParoleTick})
		}
	}
}

// runQuarantineParoleTick evaluates every parole-eligible task once. `now` is
// injected so tests drive the ladder deterministically without wall-clock waits.
// It POSTS EvManualRestart (the legal StQuarantined->StSpawning "reset failures"
// transition — the SAME one `mcphub daemon recover` / force-respawn use) for a
// quarantined, running-intent daemon whose cooldown has elapsed; it mutates ONLY
// its own in-memory ladder, never smStates/tracker directly.
func (c *supervisorController) runQuarantineParoleTick(now time.Time) {
	if c == nil || c.eventLoop == nil {
		return
	}
	// Never parole while the supervisor is gracefully draining — respawning a
	// daemon into a shutting-down supervisor is pointless churn.
	if c.graceful != nil && c.graceful.InProgress() {
		return
	}
	c.quarantineParole.Range(func(k, v any) bool {
		taskName, _ := k.(string)
		entry, _ := v.(*quarantineParoleEntry)
		if entry == nil {
			c.quarantineParole.Delete(k)
			return true
		}
		// getSMStateCanonical (NOT GetSMState) probes BOTH key forms as post-r8
		// belt-and-suspenders. Since the SM ingestion boundary canonicalizes
		// ev.TaskName once, smStates is a single canonical key space (see
		// getSMStateCanonical), so this parole entry's canonical "\foo" key and the
		// smStates row line up. The toggled probe is cheap insurance for a legacy
		// bare-keyed remnant the boundary structurally prevents; a strict
		// GetSMState(canonical) miss on such a remnant would delete the ladder as "no
		// longer tracked" and silently un-parole the daemon (commission P2-3).
		state, ok := c.getSMStateCanonical(taskName)
		if !ok {
			// Task no longer tracked (removed from intent). Drop the ladder.
			c.quarantineParole.Delete(k)
			return true
		}
		if state != api.StQuarantined {
			switch state {
			case api.StRunning:
				// A parole respawn reached healthy StRunning. Reset the ladder
				// only after it DWELLS — a daemon that reaches StRunning briefly
				// then re-fails must keep the exponential schedule.
				if entry.healthySince.IsZero() {
					entry.healthySince = now
				}
				if now.Sub(entry.healthySince) >= quarantineParoleStabilizeDwell {
					c.quarantineParole.Delete(k)
					c.emitQuarantineParoleEvent(taskName, "daemon-quarantine-parole-cleared", map[string]any{
						"attempts": entry.attempts,
						"note":     "paroled daemon stabilized in Running past the dwell; quarantine parole ladder reset",
					})
				}
			case api.StIdle:
				// Settled idle (operator stop drained it, or it reached a
				// non-restart terminal). Drop the ladder.
				c.quarantineParole.Delete(k)
			default:
				// StSpawning / StBackoffWaiting / StExiting: a parole respawn is
				// in flight (or re-failing). Keep the ladder; reset the dwell so a
				// later StRunning must accrue the full dwell afresh.
				entry.healthySince = time.Time{}
			}
			return true
		}
		// state == StQuarantined
		entry.healthySince = time.Time{}
		if now.Before(entry.nextAttemptAt) {
			return true // cooldown not yet elapsed
		}
		// Respect intent: never auto-respawn a daemon the operator has stopped.
		// The SM's StQuarantined + EvManualRestart transition spawns
		// UNCONDITIONALLY (unlike StIdle + EvManualRestart, which refuses on
		// stopped intent), so the stop gate MUST live here (commission P3-4).
		//
		// Stop-read + post are loop-serialized AND priority-drained: this tick runs
		// on the event-loop goroutine — the SAME goroutine that applies EvIntentUpdate
		// and swaps the stops cache (handleReapScan) — so no loop event interleaves
		// between the snapshot read below and the PostSelf, and any operator stop
		// already applied on the loop is observed here. For a stop that lands AFTER
		// this tick's snapshot (queued behind this tick as evReapScan stop-swap +
		// EvIntentUpdate(stopped)), the PostSelf restart priority-drains via selfCh
		// BEFORE those queued external events: EvManualRestart transitions
		// StQuarantined→StSpawning first, then the queued EvIntentUpdate(stopped)
		// lands at StSpawning and records queued_action=stop, so the just-spawned
		// child is terminated by the stop's OWN event — no reliance on a later delta.
		// The result is at most a brief spawn that the stop itself reaps; the stop is
		// honored. The durable fix (intent-gating the StQuarantined + EvManualRestart
		// SM row itself) also changes `mcphub daemon recover`-on-stopped semantics and
		// stays a separate SM-contract decision.
		di := c.daemonIntent.Lookup(taskName)
		activeStop, _ := di.IsActiveStop(now)
		desired := di.Desired
		if desired == "" {
			desired = api.IntentDesiredRunning
		}
		if desired != api.IntentDesiredRunning || activeStop {
			return true // keep the entry; do not parole a stopped daemon
		}
		// Cooldown elapsed and intent still running → parole. This tick runs INSIDE
		// the evParoleTick handler (on the loop goroutine), so the restart MUST go
		// through PostSelf, not the main-channel TryPost: PostSelf is the only
		// contract-safe handler-context post (supervisor_event_loop.go), and the
		// loop priority-drains selfCh BEFORE any pre-queued external event. That
		// ordering is load-bearing for the stop gate — a stop queued behind this
		// tick (evReapScan stop-swap + EvIntentUpdate(stopped)) would, via a
		// main-channel TryPost to the TAIL, let EvIntentUpdate(stopped) hit
		// StQuarantined as an absorbing no-op FIRST and then this EvManualRestart
		// spawn cleanly against an already-applied stop that no later delta reaps.
		// With PostSelf the EvManualRestart drains first (StQuarantined→StSpawning)
		// and the queued EvIntentUpdate(stopped) then lands at StSpawning, recording
		// queued_action=stop so the stop is honored. On a full selfCh, leave the
		// ladder unchanged so the next tick retries rather than burning a step.
		// P2-ii: carry the require_running_intent flag for one uniform rule (every
		// automatic EvManualRestart is stop-gated on the loop). This parole tick
		// already re-checked the stop intent on the loop above, but PostSelf lands on
		// the NEXT loop iteration, so the flag closes the residual window where a stop
		// is queued between this tick and the posted restart being processed.
		if !c.eventLoop.PostSelf(api.LoopEvent{
			Kind:     api.EvManualRestart,
			TaskName: taskName,
			Body:     map[string]any{autoRestartRequireRunningIntentBodyKey: true},
		}) {
			c.emitQuarantineParoleEvent(taskName, "daemon-quarantine-parole-deferred", map[string]any{
				"note": "event loop self-channel full; parole retry deferred to the next tick",
			})
			return true
		}
		entry.attempts++
		entry.nextAttemptAt = now.Add(quarantineParoleDelay(entry.attempts))
		c.emitQuarantineParoleEvent(taskName, "daemon-quarantine-parole-retry", map[string]any{
			"attempt":            entry.attempts,
			"next_retry_seconds": int(quarantineParoleDelay(entry.attempts) / time.Second),
			"note":               "posted EvManualRestart to give a quarantined daemon an automatic recovery attempt (F2 parole) without a supervisor restart",
		})
		return true
	})
}

func (c *supervisorController) emitQuarantineParoleEvent(taskName, event string, body map[string]any) {
	if c == nil || c.events == nil {
		return
	}
	_ = c.events.Emit(api.SupervisorEvent{
		Severity: "info",
		Source:   "restart-policy",
		Event:    event,
		TaskName: canonicalSupervisorTaskName(taskName),
		Body:     body,
	})
}

const idleRespawnResultBodyKey = "idle_respawn_result"

// Controller-internal reap lifecycle events. These are api.SMEvent VALUES (so
// they fit LoopEvent.Kind) but are NOT rows in api.Transition's table — they are
// intercepted at the top of handleLoopEvent and dispatched to the reap handlers,
// never passed to api.Transition. Routing the whole reap lifecycle through the
// FIFO event loop is the class fix for Codex pr302 r3 findings A/B/C/H/I: the
// off-loop IntentWatcher / IPC-apply / follow-up-timer goroutines only DETECT a
// change and POST one of these events; ALL mutation of smStates / reapShadow /
// pendingReap / queuedActions / tracker and the terminate decision then run on
// the single loop goroutine, where they are naturally serialized against the
// EvChildExit / EvHealthOK / EvTimerDue handlers they used to race.
const (
	// evReapScan is posted by refreshSupervisorIntent (off the loop) AFTER it has
	// atomically swapped the intent-descriptor cache. The on-loop handler diffs
	// the previous-vs-next task-name sets carried in the event body, marks newly-
	// disappeared live tasks pendingReap (tick 1), and resolves any pendingReap
	// that is now confirmed-absent across the verification window (tick 2). This
	// replaces the body of the old off-loop refreshSupervisorIntent reap.
	evReapScan api.SMEvent = "reap-scan"
	// evReapFollowup is posted by the bounded follow-up timer (finding 1). The
	// on-loop handler re-reads intent FROM DISK (finding A), scopes the resolution
	// to ONLY the event's own task (finding B), and no-ops when the event's
	// generation no longer matches the live pendingReap generation (finding C).
	evReapFollowup api.SMEvent = "reap-followup"
	// evReapBarrier is a TEST-ONLY synchronization event. handleLoopEvent signals
	// the result channel in its body and returns; a test posts it after driving
	// reap events and blocks on the channel, so when the channel fires every
	// previously-posted event (FIFO) has been fully processed on the loop
	// goroutine. Production never posts it. It is the deterministic barrier that
	// lets the orphan-reap tests run against a REAL concurrent loop (so `-race`
	// exercises the off-loop→on-loop serialization) while keeping assertions
	// race-free: the loop's writes happen-before the barrier signal, which
	// happens-before the test goroutine's post-barrier reads.
	evReapBarrier api.SMEvent = "reap-barrier"
)

// evParoleTick is the F2 quarantine-parole scan, run as a controller-internal
// loop event. Like the evReap* events above it is an api.SMEvent VALUE (so it
// fits LoopEvent.Kind) but is NOT a row in api.Transition's table — it is
// intercepted at the top of handleLoopEvent and dispatched to
// runQuarantineParoleTick, never passed to api.Transition. The parole monitor
// goroutine (runQuarantineParoleMonitor) now only DETECTS the tick cadence and
// POSTS this event; ALL parole map mutation (create / clear / Delete / field
// advance) and the EvManualRestart post then run on the single loop goroutine,
// serialized against recordQuarantineParoleEligible / clearQuarantineParole
// (which already run there). That removes the pre-fix cross-goroutine TOCTOU
// between the off-loop tick's Delete and an on-loop threshold record.
const evParoleTick api.SMEvent = "parole-tick"

// reapBarrierResultBodyKey carries the chan struct{} a test waits on for the
// evReapBarrier synchronization event.
const reapBarrierResultBodyKey = "reap_barrier_result"

// Reap event body keys.
const (
	// reapScanIntentBodyKey carries the FRESH *api.SupervisorIntentFile snapshot the
	// off-loop detector read. handleReapScan applies the WHOLE snapshot atomically
	// on the loop: it (re)computes next-names + captured descriptors from THIS fresh
	// intent against the CURRENT cache at handling time (so a descriptor re-added
	// before the scan is processed is re-evaluated, #614), installs reap shadows
	// BEFORE swapping the cache (#564/#535), then swaps both the descriptor cache
	// and the stops cache from the SAME snapshot (#826/#829). A nil value means the
	// off-loop read failed and the swap is skipped (the cache is preserved).
	reapScanIntentBodyKey = "reap_scan_intent"
	// reapScanStopsBodyKey carries the FRESH *api.DaemonIntentFile (unified stops)
	// resolved alongside the intent snapshot, so handleReapScan swaps the stops
	// cache from the same fresh snapshot as the descriptor cache (a re-add with
	// Desired=stopped must update the stops cache so a replayed EvTimerDue/EvHealthOK
	// does not treat it as default-running — #826/#829). A nil value means "preserve
	// the prior stops cache" (the off-loop caller passed no fresh stops, e.g. an
	// apply with a physically-absent source).
	reapScanStopsBodyKey = "reap_scan_stops"
	// reapScanPreviousNamesBodyKey carries the task-name set the cache held at
	// POST time (read off-loop). It is used ONLY as the universe of tasks whose
	// FIRST-removal must be considered this scan; the authoritative next/captured
	// diff is recomputed on-loop against the CURRENT cache so a re-add landing
	// between post and handling is honored (#614).
	reapScanPreviousNamesBodyKey  = "reap_previous_names"
	reapFollowupGenerationBodyKey = "reap_followup_generation"
	// reapScanDoneBodyKey carries an OPTIONAL chan struct{} the on-loop evReapScan
	// handler closes AFTER handleReapScan has fully applied the snapshot (cache
	// swap + reap reconcile). It is the synchronous BARRIER the reconcile-apply
	// path (refreshSupervisorIntentSync) blocks on so the descriptor + stops cache
	// swap is OBSERVABLE before the IPC apply response is sent (Codex pr302 r6
	// finding 1: the r5 reconcile-apply path routed its cache refresh through the
	// ASYNC evReapScan and returned before the loop had run daemonIntent.Refresh,
	// so an immediate observer / back-to-back reconcile/status read the stale
	// default-running cache and the existing reconcile-apply orphaned-LSP tests
	// could read "" right after a successful apply). A nil/absent value means the
	// caller does NOT need the barrier (the IntentWatcher onChange path stays
	// async — no synchronous observer waits on it). Closing happens on the loop
	// goroutine; the off-loop apply caller blocks on the channel (with a ctx +
	// timeout guard) so the swap happens-before the apply returns.
	reapScanDoneBodyKey = "reap_scan_done"
)

// reapScanBarrierTimeout bounds how long refreshSupervisorIntentSync blocks
// waiting for the on-loop evReapScan handler to signal completion. It is a
// safety valve against a wedged loop: the cache swap that the reconcile-apply
// caller needs has either run (the channel closes well within this bound on a
// healthy loop) or the loop is stuck, in which case blocking forever would
// freeze the IPC handler. On timeout the apply proceeds with the swap NOT yet
// observed — strictly no worse than the r5 async behavior — and the deferred
// EvIntentUpdate/EvManualRestart events still drain behind the queued scan in
// FIFO order, so the eventual swap is correct; only the synchronous read
// guarantee is forfeited. The bound mirrors the reap test harness's own 5s
// sync() panic budget.
const reapScanBarrierTimeout = 5 * time.Second

// supervisorCleanExitBodyKey flags an EvChildExit that corresponds to a
// CLEAN child exit (exit code 0, no Wait error). runCrashEventBridge sets
// it from the crashEvent fields; handleLoopEvent reads it to preserve the
// deliberate-shutdown contract (a clean exit observed while the task is
// still StRunning — i.e. NO controller-driven exit in flight — is dropped
// instead of routed through StRunning->StBackoffWaiting respawn). During a
// controller-driven restart the task is in StExiting when the clean exit
// arrives, so the flag does NOT suppress the queued respawn there.
const supervisorCleanExitBodyKey = "clean_exit"

// reapFollowupDefaultDelay is the production bound on how long an orphaned
// daemon can linger after its descriptor is removed from intent before the
// self-driven follow-up tick forces the still-absent confirmation + terminate.
// It must be LONGER than the IntentWatcher poll interval's replace-in-place
// window (so a genuine operator REPLACE-IN-PLACE re-add still lands first and
// cancels the reap) yet short enough that an orphaned port is reclaimed within
// a few seconds. 5s mirrors the supervisor liveness sweep cadence — the same
// "reclaim a wedged daemon promptly" budget — and comfortably exceeds the
// sub-second gap between the remove-write and any re-add-write of a real
// replace-in-place.
const reapFollowupDefaultDelay = 5 * time.Second

// reapTimer is the minimal surface the follow-up resolution timer needs:
// Stop so a confirmed/canceled reap can disarm a still-pending timer. Both
// the production *time.Timer and the test fake satisfy it.
type reapTimer interface {
	Stop() bool
}

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

// LookupCanonical resolves the descriptor for a task by BOTH the canonical
// leading-backslash key AND the raw bare key (Codex pr302 r6 finding 4). The
// descriptor map (daemonByTask) is keyed by the RAW SupervisorDaemon.TaskName
// exactly as written in supervisor-intent.json, but TaskNames() canonicalizes
// (prepends "\"). For LEGACY / hand-written intent rows whose TaskName LACKS the
// leading backslash, a reap removal-candidate name comes from TaskNames() (canonical
// "\foo") while the descriptor was stored under the raw bare key ("foo"), so a plain
// Lookup("\foo") MISSES it — the orphan-reap capture then falls into the captured-miss
// path and only clears bookkeeping instead of driving the terminate backstop, leaving
// exactly the orphan the reap exists to kill. LookupCanonical tries the requested key
// first (the common canonical case), then the toggled form (strip-or-add the leading
// "\"), so a canonical query resolves a bare-keyed legacy descriptor and vice versa.
// Lookup itself keeps strict exact-key semantics for callers that depend on it.
func (c *IntentCache) LookupCanonical(taskName string) (*api.SupervisorDaemon, bool) {
	if c == nil {
		return nil, false
	}
	s, ok := c.snap.Load().(*intentSnapshot)
	if !ok || s == nil {
		return nil, false
	}
	if d, ok := s.daemonByTask[taskName]; ok && d != nil {
		return d, true
	}
	// Toggle the leading backslash: a canonical "\foo" query also probes the raw
	// "foo" key, and a bare "foo" query also probes the canonical "\foo" key.
	var alt string
	if strings.HasPrefix(taskName, `\`) {
		alt = strings.TrimPrefix(taskName, `\`)
	} else {
		alt = `\` + taskName
	}
	if alt != taskName {
		if d, ok := s.daemonByTask[alt]; ok && d != nil {
			return d, true
		}
	}
	return nil, false
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

// CurrentIntent returns the supervisor-intent snapshot currently applied to the
// cache (nil before the first Refresh, or when a Refresh applied a nil file). The
// returned pointer is read-only — callers must not mutate it (the cache still
// references it). Used by the reallocation stale-snapshot guard (FIX-4) to compare
// the worker's carried snapshot's UpdatedAt against what is already applied.
func (c *IntentCache) CurrentIntent() *api.SupervisorIntentFile {
	if c == nil {
		return nil
	}
	s, ok := c.snap.Load().(*intentSnapshot)
	if !ok || s == nil {
		return nil
	}
	return s.intent
}

// refreshSupervisorIntent is the OFF-LOOP detect-and-post-only entry point for a
// fresh supervisor-intent.json snapshot (descriptors `updated` + the resolved
// unified `stops`). It performs NO cache mutation itself: it reads the current
// cache (atomic.Value snapshot — safe off the loop) only to decide whether
// anything CHANGED, then posts ONE evReapScan carrying the WHOLE fresh snapshot.
// The on-loop handleReapScan then applies the snapshot ATOMICALLY (Codex pr302 r4
// root fix for #564/#535/#614/#826/#856): it installs reap shadows BEFORE swapping
// the descriptor cache, swaps BOTH the descriptor cache and the stops cache from
// the SAME snapshot, and re-diffs previous-vs-fresh against the CURRENT cache at
// handling time.
//
// Why the swap MUST move on-loop (the bug the pre-r4 design left): the old body
// swapped intentCache OFF the loop and only THEN posted evReapScan. Because Post
// appends BEHIND events already queued, an EvChildExit / EvHealthOK / EvTimerDue
// for the removed task that was already in the FIFO ran against the now-EMPTY
// cache with NO shadow installed yet → orphan-dropped → the SM wedged at stale
// StSpawning/StRunning/StBackoffWaiting, and the later confirmed reap recorded a
// queued stop that deferred to a spawn-completion event already lost → orphan
// forever. Routing the cache swap THROUGH the same FIFO as those events, with the
// shadow installed before the swap, makes the application atomic with respect to
// them: a queued event drains first (cache still holds the descriptor, routes
// normally) or the evReapScan drains first (shadow installed before the swap, so
// the queued event still routes via the shadow). Either order is correct.
//
// It is the single owner of the descriptor-disappearance ORPHAN REAP — the durable
// supervisor-side backstop for the synchronous install/uninstall-time kill
// (api.killRemovedSupervisorTargetsAfter*), which is conditional on the
// reconcile-nudge succeeding and is skipped on nudge-failure or an install process
// that crashes between the intent-write and the kill.
//
// Only TWO call sites drive refreshSupervisorIntent — the 60s IntentWatcher
// onChange (supervise.go) and the apply-mode reconcile IPC (applyReconcileDrift) —
// and both read the intent through the flock-atomic api.ReadSupervisorIntent
// (single os.ReadFile against an atomic temp+rename writer), so each `updated` is a
// CONSISTENT complete snapshot. `stops` is the unified stops resolved alongside it;
// a nil `stops` means "preserve the prior stops cache" (the caller had no fresh
// stops source). A nil `updated` is a no-op (the caller nils it on read error).
//
// TRANSIENT-ABSENCE GUARD (prevents reaping a live daemon): a descriptor can still
// be MOMENTARILY absent across two scans when an operator/install REPLACES it in
// place. The reap uses a one-tick verification window — the first scan that
// observes a live task absent only MARKS it pendingReap; the terminate fires only
// on the next scan / the bounded follow-up tick if it is STILL absent.
func (c *supervisorController) refreshSupervisorIntent(updated *api.SupervisorIntentFile, stops *api.DaemonIntentFile) {
	if c == nil || c.intentCache == nil {
		return
	}
	if updated == nil {
		// No fresh descriptor snapshot to apply (the caller read failed and nilled
		// it). Preserve the cache; a nil updated must NOT be interpreted as
		// "everything removed". The off-loop callers already skip the call on a hard
		// read error, but guard here too.
		return
	}
	// Capture the post-time cache name-set (read-only, off the loop) only to WIDEN the
	// universe of tasks the on-loop diff considers; the authoritative diff is
	// recomputed on the loop against the CURRENT cache (#614). We do NOT make a
	// post-or-skip DECISION from a cache comparison here: the off-loop cache reflects
	// only ALREADY-APPLIED scans, not scans still queued ahead of this one, so a
	// "snapshot == cache → skip" optimization would silently DROP a second rapid scan
	// (e.g. two watcher mtime bumps before the first is applied) whose cache view is
	// stale. We always post when updated != nil; handleReapScan (which sees the true
	// current cache on the loop) is a cheap no-op when nothing actually changed — and
	// since evReapScan is an internal cache-apply event, not an SM event, it never
	// disturbs the apply-mode "first SM event is EvIntentUpdate" ordering (FIFO keeps
	// the scan ahead of the drift events) nor the "no SM event on a no-op" assertions.
	previous := c.intentCache.TaskNames()

	// POST (onto the loop): handleReapScan applies the WHOLE snapshot atomically —
	// shadow-install BEFORE the cache swap, swap BOTH caches from the same snapshot,
	// re-diff against the CURRENT cache at handling time, mark/resolve reaps, queue
	// re-add respawns. When the event loop is absent (unit-test fixtures that drive
	// the controller synchronously and never start Run) we fall back to running the
	// scan inline so those tests keep their deterministic single-goroutine semantics.
	if c.eventLoop == nil {
		c.handleReapScan(previous, updated, stops)
		return
	}
	c.eventLoop.Post(api.LoopEvent{
		Kind: evReapScan,
		Body: map[string]any{
			reapScanPreviousNamesBodyKey: previous,
			reapScanIntentBodyKey:        updated,
			reapScanStopsBodyKey:         stops,
		},
	})
}

// refreshSupervisorIntentSync is the SYNCHRONOUS variant of refreshSupervisorIntent
// for the reconcile-apply path (Codex pr302 r6 finding 1). It posts the SAME
// evReapScan onto the loop — so the cache swap still runs ON the loop goroutine,
// serialized against handleLoopEvent (the on-loop atomic-apply invariant the r4
// fix established is preserved) — but it ALSO carries a done-channel barrier and
// BLOCKS the (off-loop) caller until the on-loop handler has finished applying the
// snapshot. By the time this returns, the descriptor cache AND the stops cache
// have been swapped, so the reconcile-apply IPC handler's synchronous read of
// daemonIntent.Lookup (and any immediate back-to-back reconcile/status) observes
// the NEW stops instead of the stale default-running cache.
//
// Why the apply path needs the barrier but the IntentWatcher path does NOT: the
// IntentWatcher onChange (supervise.go) posts evReapScan then immediately posts
// the delta EvIntentUpdate events behind it in the SAME FIFO — there is no
// synchronous observer waiting on the cache, and FIFO ordering already guarantees
// the swap precedes the deltas, so async is correct there. The reconcile-apply
// handler, by contrast, RETURNS to its IPC caller right after applyReconcileDrift,
// and the existing tests (and a real back-to-back `mcphub status`) read the cache
// synchronously on the return path; that read must see the applied swap.
//
// The barrier is the ONLY change vs the async path: the same on-loop handleReapScan
// runs, the same FIFO ordering keeps the scan ahead of the drift EvIntentUpdate /
// EvManualRestart events applyReconcileDrift posts AFTER this returns. Blocking the
// off-loop caller cannot deadlock the loop: the caller is NOT the loop goroutine,
// and the close happens on the loop, so the loop keeps draining while the caller
// waits. A wedged loop (or a torn-down ctx) is bounded by reapScanBarrierTimeout /
// ctx.Done() — on timeout the apply proceeds exactly as the r5 async path did.
//
// Falls back to the inline synchronous run when the event loop is absent (the
// unit-test fixtures that never start Run), where the swap is already synchronous.
func (c *supervisorController) refreshSupervisorIntentSync(updated *api.SupervisorIntentFile, stops *api.DaemonIntentFile) {
	if c == nil || c.intentCache == nil {
		return
	}
	if updated == nil {
		// Same nil-updated guard as the async path: a nil snapshot is a no-op
		// (the caller read failed and nilled it); never interpreted as "everything
		// removed".
		return
	}
	if c.eventLoop == nil {
		// No loop to post onto: run the swap inline (already synchronous). Mirrors
		// the async path's eventLoop==nil fallback.
		previous := c.intentCache.TaskNames()
		c.handleReapScan(previous, updated, stops)
		return
	}
	previous := c.intentCache.TaskNames()
	done := make(chan struct{})
	parent := c.ctx
	if parent == nil {
		parent = context.Background()
	}
	// pr302 r7 finding 3: bound BOTH the enqueue AND the done-wait with ONE
	// deadline context. The r6 barrier started its timeout only AFTER
	// eventLoop.Post returned, but Post is a BLOCKING send (api.EventLoop.Post:
	// `l.ch <- e`). If reconcile --apply runs while the external buffer is FULL,
	// or the loop stopped draining during shutdown, the plain Post would block
	// HERE forever — the IPC handler never reached the timeout select below. The
	// deadline context makes the enqueue itself bounded: PostCtx selects on
	// {l.ch <- e, barrierCtx.Done()} so a full/stopped loop returns
	// DeadlineExceeded (or Canceled on shutdown) instead of hanging. The same
	// barrierCtx then bounds the done-wait, so the documented timeout caps the
	// WHOLE path.
	barrierCtx, cancel := context.WithTimeout(parent, reapScanBarrierTimeout)
	defer cancel()
	if err := c.eventLoop.PostCtx(barrierCtx, api.LoopEvent{
		Kind: evReapScan,
		Body: map[string]any{
			reapScanPreviousNamesBodyKey: previous,
			reapScanIntentBodyKey:        updated,
			reapScanStopsBodyKey:         stops,
			reapScanDoneBodyKey:          done,
		},
	}); err != nil {
		// The event could NOT be enqueued within the bound: the loop's buffer was
		// full and stayed full (wedged loop) or the supervisor is shutting down.
		// The scan did NOT run, so the cache swap did NOT happen — strictly no
		// worse than a torn-down/wedged loop dropping the scan; the next 60s
		// IntentWatcher poll re-applies the snapshot. Proceed with the apply; the
		// synchronous read guarantee is forfeited this apply. CRITICAL: returning
		// here cannot hang the IPC handler (the whole point of the bound).
		c.emitReapEvent("warn", "reconcile-apply-cache-barrier-enqueue-timeout", "",
			"the reconcile-apply cache-swap barrier could not be enqueued within the bound (the event loop buffer is full / the loop stopped draining); proceeding with the apply WITHOUT the on-loop swap this tick (the next intent-watcher poll re-applies the snapshot); the synchronous read guarantee is forfeited this apply")
		return
	}
	select {
	case <-done:
		// The on-loop handler finished applying the snapshot — both caches are
		// swapped and observable now (SUCCESS path: the cache-swap-before-return
		// guarantee the r6 barrier established is preserved).
	case <-barrierCtx.Done():
		// Either the supervisor is shutting down (parent canceled) or the loop did
		// not signal within the bound (wedged loop). Blocking past this would freeze
		// the IPC handler against a torn-down / wedged loop. Proceed; the
		// FIFO-ordered drift events behind the scan still drain correctly when/if
		// the loop resumes. The scan WAS enqueued (PostCtx returned nil), so the
		// swap is queued ahead of the drift events in FIFO order and still applies
		// — only the synchronous read guarantee is forfeited this apply.
		c.emitReapEvent("warn", "reconcile-apply-cache-barrier-timeout", "",
			"the reconcile-apply cache-swap barrier did not signal within the bound; proceeding with the apply (the cache swap is queued ahead of the drift events in FIFO, so it still applies, but the synchronous read guarantee is forfeited this apply)")
	}
}

// handleReapScan is the ON-LOOP atomic snapshot-applier + reap reconciler for the
// SCAN path (the evReapScan posted by a real intent CHANGE — the 60s IntentWatcher
// onChange and the reconcile-apply IPC). It runs inside handleLoopEvent (single loop
// goroutine), so every cache swap, every mutation, and every terminate it drives is
// serialized against the other SM event handlers (Codex pr302 r4 root fix + r3
// findings A/B/C/H/I).
//
// pr302 r6 finding 2: handleReapScan is now a THIN wrapper that (1) calls the shared
// applyReapSnapshot — which performs steps 1-5 (the always-safe snapshot APPLY:
// diff, capture+shadow, swap both caches, reappear/settle handling, mark new
// disappearances) and RETURNS the still-absent-and-live confirmed-reap candidates —
// then (2) CONFIRMS+TERMINATES every returned candidate. The multi-confirm is
// correct for the SCAN path: a real intent change that removes N daemons at once
// SHOULD confirm all N that are still absent across the window. What must NOT
// multi-confirm is the FOLLOW-UP TIMER path: a per-task timer that observes its own
// task re-added (and routes the fresh snapshot through applyReapSnapshot for the
// #946/#942/#748 benefits) must confirm ONLY its own task, never a sibling that is
// merely in its OWN verification window. handleReapFollowup therefore calls
// applyReapSnapshot then confirms ONLY ownTask — see confirmReapForTask.
func (c *supervisorController) handleReapScan(previous map[string]struct{}, updated *api.SupervisorIntentFile, stops *api.DaemonIntentFile) {
	confirmedReaps := c.applyReapSnapshot(previous, updated, stops)
	// Drive EVERY confirmed reap through the SM-aware terminate. Correct for the
	// scan path: a real intent change confirms all still-absent removals at once.
	for _, d := range confirmedReaps {
		c.resolveConfirmedReap(d)
	}
}

// applyReapSnapshot is the ON-LOOP atomic snapshot APPLY (steps 1-5) shared by both
// the scan path (handleReapScan) and the follow-up reappear path (handleReapFollowup).
// It is the always-SAFE half: it mutates the caches + reap bookkeeping but drives NO
// confirm-terminate itself. Instead it RETURNS the still-absent-and-live candidates so
// the CALLER decides the confirm scope (finding 2 — separate snapshot-APPLY from
// reap-CONFIRM):
//
//   - the scan path confirms ALL returned candidates (a real intent change SHOULD
//     resolve every still-absent removal);
//   - the follow-up timer path confirms ONLY its own (taskName, generation), so A's
//     timer can never confirm+terminate a sibling B that is merely in its own
//     verification window (the old #662/#B sibling-early-terminate bug, reintroduced
//     by r5 routing the follow-up reappear through the full handleReapScan).
//
// It applies the WHOLE fresh snapshot ATOMICALLY in this order:
//
//  1. Compute the prev-vs-next diff from the CURRENT cache (read at handling time)
//     vs the FRESH snapshot — NOT a diff snapshotted at post time. A descriptor
//     re-added BETWEEN post and handling is therefore re-evaluated as present and
//     is NOT terminated (#614). `previous` (the post-time name set) is used only to
//     widen the universe of names considered, never to override the current diff.
//  2. Capture the OLD descriptor for every newly-ABSENT task from the CURRENT cache
//     and INSTALL its reapShadow BEFORE swapping the cache (#564/#535): a queued
//     in-flight event that drains after the swap then still routes via the shadow.
//  3. Swap BOTH the descriptor cache AND the stops cache from the SAME fresh
//     snapshot (#826/#829): a re-add with Desired=stopped updates the stops cache so
//     a replayed EvTimerDue/EvHealthOK does not treat it as default-running.
//  4. Mark new disappearances pendingReap (tick 1) + arm the generation-tagged
//     follow-up timer.
//  5. For newly-PRESENT (re-added) descriptors, cancel their pendingReap/shadow,
//     refresh stops (done by the swap above), replay any deferred backoff timer,
//     clear any reap-originated queued stop, AND queue a respawn where the re-added
//     own-spawned daemon was being terminated (#625).
//
// It returns the still-absent-and-live pendingReap descriptors (reappear handled in
// step 5; settled → cleared in-line; still-absent-and-live → returned for the caller
// to confirm). A single snapshot that simultaneously re-adds A and first-removes B
// installs B's shadow+pendingReap here (#826-rescan: the reappear path must not
// blind-replace the cache and lose a sibling's first removal).
func (c *supervisorController) applyReapSnapshot(previous map[string]struct{}, updated *api.SupervisorIntentFile, stops *api.DaemonIntentFile) []*api.SupervisorDaemon {
	if c == nil || c.intentCache == nil {
		return nil
	}

	// Step 1: re-diff against the CURRENT cache at handling time (#614). `next` is
	// the authoritative present-set from the fresh snapshot; `current` is what the
	// cache holds RIGHT NOW (which may already reflect a re-add a prior on-loop event
	// applied). The union of `previous` ∪ `current` is the universe of names whose
	// first-removal we must consider.
	next := taskNameSetFromIntent(updated)
	current := c.intentCache.TaskNames()
	universe := map[string]struct{}{}
	for name := range previous {
		universe[name] = struct{}{}
	}
	for name := range current {
		universe[name] = struct{}{}
	}

	// Step 2: capture the OLD descriptor for every newly-ABSENT task from the CURRENT
	// cache and install its reapShadow BEFORE the cache swap. A task is newly-absent
	// when it is gone from `next` but is NOT already pendingReap (those keep their
	// own already-captured shadow). Installing the shadow before the swap is the
	// #564/#535 fix: a queued in-flight event draining after the swap routes via it.
	captured := map[string]*api.SupervisorDaemon{}
	for taskName := range universe {
		if _, stillPresent := next[taskName]; stillPresent {
			continue
		}
		if _, already := c.pendingReap.Load(taskName); already {
			continue // Arm 1 already holds this task's captured descriptor + shadow
		}
		// pr302 r6 finding 4: capture the descriptor under BOTH the canonical key AND
		// the raw bare key. `taskName` here is canonical (from TaskNames()), but a
		// LEGACY / hand-written intent row stores the descriptor under its raw bare
		// TaskName; a plain Lookup(canonical) would MISS it and fall into the
		// captured-miss bookkeeping-only clear, leaving the orphan unterminated.
		if d, ok := c.intentCache.LookupCanonical(taskName); ok && d != nil {
			cp := *d // copy out of the pre-swap snapshot slice
			captured[taskName] = &cp
			// Pre-install the shadow so an in-flight event that drains AFTER the swap
			// still routes. markPendingReap (step 4) re-stores it; an idempotent set.
			if c.smStateIsReapable(taskName) {
				c.reapShadow.Store(taskName, &cp)
			}
		}
	}

	// Step 3: swap BOTH caches from the SAME fresh snapshot. The descriptor cache
	// swap makes re-added rows routable for replayed timers; the stops swap keeps a
	// re-add-with-Desired=stopped suppressing correctly (#826/#829). A nil `stops`
	// means "no fresh stops source" → preserve the prior stops cache.
	c.intentCache.Refresh(updated)
	if stops != nil && c.daemonIntent != nil {
		c.daemonIntent.Refresh(stops)
	}

	// Arm 1 (steps 5 + 6-collect): resolve the SAFE half of every existing pendingReap
	// against the fresh present-set (reappear → cancel; settled → clear) and COLLECT —
	// but do NOT terminate — the still-absent-and-live candidates. The caller confirms.
	var confirmedReaps []*api.SupervisorDaemon
	c.pendingReap.Range(func(key, value any) bool {
		taskName, _ := key.(string)
		if taskName == "" {
			return true
		}
		if _, present := next[taskName]; present {
			// Step 5: re-appeared (replace-in-place completed). The caches were
			// already swapped above, so the re-added descriptor is routable. Queue a
			// respawn for a re-added own-spawned daemon whose in-flight reap was
			// terminating it (#625), replay any deferred backoff timer (finding E),
			// clear any reap-originated queued stop (finding D), then drop the
			// mark + shadow.
			c.queueRespawnOnReapCancelIfNeeded(taskName)
			c.replayDeferredBackoffTimerIfPending(taskName)
			c.clearReapDeferredStopIfPending(taskName)
			c.cancelPendingReap(taskName)
			c.emitReapEvent("debug", "orphan-reap-candidate-reappeared", taskName,
				"removed descriptor re-appeared within the verification window (replace-in-place); reap canceled, in-flight events already routed via the reaping shadow")
			return true
		}
		// Step 6: still absent. If the SM settled to a non-live state since the mark
		// (its own cmd.Wait reaped it, or a shadow-routed in-flight event drove it to
		// idle), there is nothing left to terminate — clear bookkeeping.
		if !c.smStateIsReapable(taskName) {
			c.clearRemovedTaskRuntime(taskName)
			c.emitReapEvent("debug", "orphan-reap-candidate-settled", taskName,
				"removed descriptor settled to a non-live SM state within the verification window (in-flight event routed via the shadow); stale runtime bookkeeping cleared, no terminate needed")
			return true
		}
		if d, ok := value.(*api.SupervisorDaemon); ok && d != nil {
			confirmedReaps = append(confirmedReaps, d)
		}
		return true
	})

	// Arm 2 (step 4): mark new disappearances (tick 1 of the verification window).
	for taskName := range universe {
		if _, stillPresent := next[taskName]; stillPresent {
			continue
		}
		if _, already := c.pendingReap.Load(taskName); already {
			continue // handled by Arm 1
		}
		if !c.smStateIsReapable(taskName) {
			// Not a live daemon (StIdle/StQuarantined or untracked) — nothing to
			// terminate. Clear bookkeeping as before. A speculative shadow installed
			// in step 2 only fires for reapable states, so there is none to drop here.
			c.clearRemovedTaskRuntime(taskName)
			continue
		}
		if d, ok := captured[taskName]; ok && d != nil {
			c.markPendingReap(taskName, d)
			c.emitReapEvent("info", "orphan-reap-candidate-marked", taskName,
				"descriptor disappeared from intent while the daemon is live; awaiting still-absent confirmation across one refresh window before terminating (transient-absence/replace-in-place guard); descriptor kept routable via the reaping shadow + bounded follow-up tick armed")
		} else {
			// No descriptor captured (cache miss at detect time). Fall back to the
			// prior bookkeeping-only clear; the synchronous install-time kill or a
			// cold-restart reaper remains the backstop.
			c.clearRemovedTaskRuntime(taskName)
		}
	}

	return confirmedReaps
}

// confirmReapForTask drives the SM-aware terminate for EXACTLY ONE task out of an
// applyReapSnapshot result set (finding 2). The follow-up timer path uses it to
// confirm ONLY its own task: A's timer applies the fresh snapshot (so a re-added A
// is canceled, a sibling B first-removed in the same snapshot gets its shadow +
// pendingReap, the stops cache is refreshed) WITHOUT terminating a sibling B that is
// merely partway through its OWN verification window. Each pending reap is confirmed
// only by ITS OWN scoped follow-up timer (or a real intent-change scan), never by a
// sibling's timer.
func (c *supervisorController) confirmReapForTask(taskName string, candidates []*api.SupervisorDaemon) {
	taskName = canonicalSupervisorTaskName(taskName)
	for _, d := range candidates {
		if d == nil {
			continue
		}
		if canonicalSupervisorTaskName(d.TaskName) == taskName {
			c.resolveConfirmedReap(d)
			return
		}
	}
}

// queueRespawnOnReapCancelIfNeeded ensures a re-declared daemon comes back RUNNING
// when its in-flight reap kill is canceled. It handles the TWO post-reap states a
// re-added own-spawned daemon can be in when the reappear is observed:
//
//   - StExiting (#625): the terminate has fired but the real cmd.Wait exit has NOT
//     yet landed. reapRemovedDaemon moved StRunning → StExiting (queued_action=
//     none); without intervention the EvChildExit drives StExiting → StIdle
//     (consuming NO respawn — the watcher posts no EvStart for a descriptor
//     ADDITION, only Desired-flip deltas). So set queued_action=respawn and the
//     EvChildExit drives StExiting → StSpawning (the respawn).
//
//   - StIdle (#748): the reap killed the own-spawned child and its REAL EvChildExit
//     was already processed (StExiting → StIdle) BEFORE the descriptor re-appeared,
//     so the task is idle but pendingReap is still set (the deferred-clear follow-up
//     had not yet run). queued_action is meaningless at StIdle — the SM consumes no
//     queued respawn there — and the watcher posts no EvStart for the addition, so
//     the re-declared daemon would stay stopped until a manual restart. Post an
//     EvStart so StIdle + EvStart drives StIdle → StSpawning. The reap-cancel path
//     is the ONLY deliverer of this EvStart (the IntentWatcher does not emit one
//     for a pure descriptor re-add).
//
// In both cases the re-added descriptor must be declared RUNNING for a respawn to be
// correct; a re-add that is Desired=stopped should stay stopped. Read the
// just-swapped stops cache (handleReapScan swapped BOTH caches before this runs).
// MUST run on the event loop.
func (c *supervisorController) queueRespawnOnReapCancelIfNeeded(taskName string) {
	taskName = canonicalSupervisorTaskName(taskName)
	st, ok := c.GetSMState(taskName)
	if !ok || (st != api.StExiting && st != api.StIdle) {
		// Only a mid-terminate (StExiting) or already-exited (StIdle) own-spawned
		// re-add needs a respawn nudge to come back; StRunning / StBackoffWaiting /
		// StSpawning re-adds are handled by the shadow + replay / deferred-stop clear
		// paths (the daemon was never driven toward exit, or its own
		// EvChildExit/EvTimerDue will route normally).
		return
	}
	// The re-added descriptor must be declared RUNNING for a respawn to be correct;
	// a re-add that is Desired=stopped should stay stopped. Read the just-swapped
	// stops cache (handleReapScan swapped it before this runs).
	di := c.daemonIntent.Lookup(taskName)
	if stop, _ := di.IsActiveStop(time.Now().UTC()); stop {
		return
	}
	if st == api.StExiting {
		// Mid-terminate: queue the respawn so the awaited EvChildExit drives
		// StExiting → StSpawning (#625).
		c.queuedActions.Store(taskName, "respawn")
		c.emitReapEvent("info", "orphan-reap-cancel-respawn-queued", taskName,
			"an own-spawned daemon being terminated by an in-flight reap was re-declared running before its exit; queuing a respawn (queued_action=respawn) so the EvChildExit drives StExiting -> StSpawning and the re-declared daemon comes back running (#625)")
		return
	}
	// StIdle (#748): the killed own-spawned child's EvChildExit already settled the
	// task to idle before the re-add was observed. queued_action does nothing at
	// StIdle and the watcher emits no EvStart for a descriptor addition, so POST one
	// here — StIdle + EvStart (intent running) → StSpawning. Use PostSelf so the
	// EvStart lands on the loop ahead of pre-queued external events; fall back to an
	// inline dispatch in the synchronous (no event loop) unit-test fixtures.
	if c.eventLoop == nil {
		c.emitReapEvent("info", "orphan-reap-cancel-idle-respawn-posted", taskName,
			"an own-spawned daemon whose reap-kill exit had already settled it to StIdle was re-declared running; posting EvStart so StIdle -> StSpawning and the re-declared daemon comes back running (the watcher posts no EvStart for a descriptor re-add) (#748)")
		c.handleLoopEvent(api.LoopEvent{Kind: api.EvStart, TaskName: taskName})
		return
	}
	if !c.eventLoop.PostSelf(api.LoopEvent{Kind: api.EvStart, TaskName: taskName}) {
		c.emitSelfChannelSaturated(taskName, "EvStart")
		return
	}
	c.emitReapEvent("info", "orphan-reap-cancel-idle-respawn-posted", taskName,
		"an own-spawned daemon whose reap-kill exit had already settled it to StIdle was re-declared running; posting EvStart so StIdle -> StSpawning and the re-declared daemon comes back running (the watcher posts no EvStart for a descriptor re-add) (#748)")
}

// markPendingReap records a newly-detected orphan-reap candidate: it stores
// the captured descriptor in BOTH the pendingReap map (the still-absent-
// confirmation marker) and the reapShadow (so handleLoopEvent can keep routing
// the task's in-flight events while its descriptor is gone from intentCache),
// then arms the bounded follow-up tick that guarantees the reap resolves even
// if no further intent-file change ever arrives (finding 1). Idempotent: a
// second mark within the same window does not re-arm a duplicate timer.
//
// MUST run on the event loop (called only from handleReapScan / the reap
// handlers): it bumps the per-task reap generation and arms a generation-tagged
// follow-up timer, both of which assume single-writer access (findings A/B/C).
func (c *supervisorController) markPendingReap(taskName string, captured *api.SupervisorDaemon) {
	taskName = canonicalSupervisorTaskName(taskName)
	c.pendingReap.Store(taskName, captured)
	c.reapShadow.Store(taskName, captured)
	// Open a FRESH reap-generation window for this disappearance, then Stop any
	// stale timer still registered from a previous window for the same task
	// before arming a new generation-tagged timer. This is what lets a
	// remove→reappear→remove-again sequence for the same task NOT have the first
	// removal's timer confirm/terminate the second removal early (finding C).
	c.bumpReapGeneration(taskName)
	c.stopReapTimer(taskName)
	c.reapFollowupArmed.Delete(taskName) // force a fresh arm at the new generation
	c.armReapFollowup(taskName)
}

// cancelPendingReap drops a pendingReap candidate WITHOUT terminating: the
// descriptor re-appeared or the task settled to a non-live state. It removes
// the pendingReap marker + the reaping shadow and disarms the follow-up tick.
// Does NOT touch the SM state or tracker — the task keeps running (reappear)
// or already settled on its own (settle).
func (c *supervisorController) cancelPendingReap(taskName string) {
	taskName = canonicalSupervisorTaskName(taskName)
	c.pendingReap.Delete(taskName)
	c.reapShadow.Delete(taskName)
	c.disarmReapFollowup(taskName)
}

// resolveConfirmedReap drives the SM-aware terminate for a confirmed orphan and
// reconciles bookkeeping based on the terminate OUTCOME:
//
//   - confirmed-dead (the targeted PID is gone — terminate returned nil, OR
//     terminate failed but the failure proves the process is already gone:
//     no-running-PID / post-kill-persist-failure — see reapRemovedDaemon's
//     terminate-outcome classification for finding F): clear the SM/tracker
//     entry + pendingReap + shadow + follow-up timer. The orphan is gone.
//
//   - terminate FAILED while the process may still be ALIVE (PID query /
//     permission / escalation error): PRESERVE the SM/tracker entry, the
//     pendingReap marker, and the shadow, and RE-ARM the follow-up tick so a
//     later tick retries the terminate. Clearing here would lose the recorded
//     PID, and since the descriptor is already gone from intent the liveness
//     sweep (which only considers tracker rows that still have a descriptor)
//     could never retry — the orphan would run forever with no supervisor
//     handle (finding 2).
//
// A StSpawning task whose reap left a queued_action=stop is NOT cleared here
// either: reapRemovedDaemon returns reapDeferred for it (the terminate fires
// later on the spawn-completion event), so its entry + queued stop + shadow
// survive until that event applies the stop (finding 4). The shadow keeps the
// spawn-completion event routable.
//
// MUST run on the event loop (called from handleReapScan / handleReapFollowup).
func (c *supervisorController) resolveConfirmedReap(d *api.SupervisorDaemon) {
	if d == nil {
		return
	}
	taskName := canonicalSupervisorTaskName(d.TaskName)
	outcome := c.reapRemovedDaemon(d)
	switch outcome {
	case reapTerminatedDead:
		c.pendingReap.Delete(taskName)
		c.reapShadow.Delete(taskName)
		c.disarmReapFollowup(taskName)
		c.clearRemovedTaskRuntime(taskName)
	case reapDeferred:
		// A queued stop is pending against an in-flight spawn (StSpawning) OR a
		// terminate is already in flight (StExiting). Keep the entry + shadow so
		// the spawn-completion / exit event applies the stop and terminates the
		// child; the follow-up tick stays armed so the reap is re-confirmed once
		// the task settles. Do NOT clear runtime state (finding 4).
		c.armReapFollowup(taskName)
	case reapTerminateFailed:
		// The targeted PID may still be alive. Preserve everything and retry on
		// the next tick rather than losing the supervisor handle (finding 2).
		c.armReapFollowup(taskName)
		c.emitReapEvent("warn", "orphan-reap-terminate-failed", taskName,
			"SM-aware terminate of the orphaned child FAILED (process may still be alive); preserving SM/tracker state + pendingReap so a later follow-up tick retries (clearing here would orphan a possibly-live process with no supervisor handle)")
	}
}

// handleReapFollowup is the ON-LOOP follow-up resolver for a SINGLE task's
// pendingReap. It is posted by the bounded follow-up timer (finding 1) and runs
// serialized inside handleLoopEvent, so it never races the SM event handlers
// (findings H/I). It closes three follow-up-specific findings:
//
//   - Finding A: it re-reads intent FROM DISK (reapIntentReader), not the
//     possibly-stale intentCache. The cache only refreshes on the 60s
//     IntentWatcher poll, so after a remove→re-add-on-disk within the window the
//     cache stays empty for up to 60s; a cache-only follow-up would terminate a
//     live re-declared daemon. The fresh disk read sees the re-add and cancels.
//
//   - Finding B: it resolves ONLY `taskName`, never sibling pendingReap entries.
//     A task A's timer can no longer confirm+terminate a task B that is merely
//     momentarily absent without B's own verification window.
//
//   - Finding C: it no-ops when `generation` no longer matches the task's live
//     reap generation — a stale timer from a previous remove→reappear→remove
//     cycle for the same task cannot confirm/terminate the SECOND removal early.
func (c *supervisorController) handleReapFollowup(taskName string, generation int) {
	if c == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)

	// Finding C: stale-generation guard. If the live reap generation for this
	// task no longer matches the generation this timer was armed for, a newer
	// pendingReap window (or no window) owns the task now — this fire is stale.
	curGen, hasGen := c.reapGeneration.Load(taskName)
	if !hasGen {
		return // already resolved/cleared; nothing to do
	}
	if g, ok := curGen.(int); !ok || g != generation {
		c.emitReapEvent("debug", "orphan-reap-followup-stale", taskName,
			"follow-up timer fired with a generation that no longer matches the live pendingReap window (remove→reappear→remove or already-resolved); ignoring so it cannot confirm a later removal early")
		return
	}

	// #966/#967: this timer GENUINELY fired (its generation is still the live one).
	// Clear the armed record + drop the fired timer handle NOW, BEFORE resolution
	// runs. Without this, reapFollowupArmed still holds THIS generation, so when
	// resolveConfirmedReap below hits reapTerminateFailed or reapDeferred and calls
	// armReapFollowup to schedule a RETRY, armReapFollowup sees "a timer for the
	// current generation is already pending" and returns WITHOUT scheduling a new one
	// — the transient terminate failure is then never retried, and an own-spawned
	// deferred reap leaks pendingReap/shadow until an unrelated intent refresh. The
	// generation is deliberately NOT bumped: a retry stays in the SAME verification
	// window (only a fresh disappearance via markPendingReap opens a new generation),
	// so the stale-generation guard above still neutralizes any other late timer.
	c.reapFollowupArmed.Delete(taskName)
	c.stopReapTimer(taskName)

	// Still the owning generation. Re-load the captured descriptor.
	v, still := c.pendingReap.Load(taskName)
	if !still {
		return // resolved between arm and fire (a real intent-change scan handled it)
	}
	d, _ := v.(*api.SupervisorDaemon)
	if d == nil {
		return
	}

	// Finding A: confirm absence against a FRESH on-disk read, not the cache.
	fresh, freshOK := c.freshIntentForReap()
	// #856: on a fresh-read FAILURE, do NOT fall back to the (already-emptied-by-the-
	// removal-scan) intentCache to confirm absence — that would terminate a daemon the
	// operator may have re-declared on disk in a write we simply could not read this
	// tick. A transient read glitch must NOT confirm a reap. KEEP the pendingReap +
	// shadow and RE-ARM the follow-up so a later tick re-reads disk and resolves
	// correctly. The bounded delay caps how long the orphan lingers; a persistent read
	// failure keeps retrying rather than mis-terminating a live re-declared daemon.
	if !freshOK {
		c.armReapFollowup(taskName)
		c.emitReapEvent("warn", "orphan-reap-followup-fresh-read-failed", taskName,
			"the follow-up tick could not re-read supervisor-intent.json from disk; preserving pendingReap + re-arming the tick rather than confirming absence against the emptied cache (a transient read glitch must not terminate a possibly-re-declared daemon — #856)")
		return
	}
	present := c.taskPresentInFreshIntent(taskName, fresh, freshOK)
	if present {
		// Re-appeared on disk since the mark — apply the WHOLE fresh snapshot through
		// the SAME on-loop atomic applier applyReapSnapshot instead of a raw
		// intentCache.Refresh + ad-hoc cancel (#946/#942/#748: the follow-up must NOT
		// duplicate a WEAKER version of the snapshot applier). applyReapSnapshot, fed
		// the fresh descriptor set AND its unified stops sub-block:
		//
		//   - installs a reap shadow + pendingReap for any SIBLING task that was ALSO
		//     removed on disk in this same fresh snapshot (relative to the current
		//     cache) — a raw Refresh(fresh) would silently drop such a sibling from the
		//     cache with NO shadow, so the later watcher pass would diff from a cache
		//     already missing it and never mark/terminate it → orphaned daemon (#942);
		//
		//   - swaps BOTH the descriptor cache AND the stops cache from the same fresh
		//     snapshot, so a re-add carrying Desired=stopped suppresses a replayed
		//     EvTimerDue/EvHealthOK correctly (#946 stops half);
		//
		//   - resolves THIS task's pendingReap via its Arm-1 reappear path, which
		//     queues a respawn for an own-spawned daemon mid-terminate (StExiting,
		//     #625) OR already-settled-to-idle (StIdle, #748 — its kill exit landed
		//     before the re-add), replays any deferred backoff timer (finding E),
		//     clears any reap-originated queued stop (finding D), then cancels the
		//     reap. The pre-#946 follow-up branch called none of queueRespawn / the
		//     stops swap / the sibling-shadow install, so it MISSED all three.
		//
		// pr302 r6 finding 2: the follow-up calls applyReapSnapshot (the always-safe
		// snapshot APPLY) then confirms ONLY ITS OWN task via confirmReapForTask —
		// NOT the full handleReapScan, which would also CONFIRM+TERMINATE every other
		// still-absent sibling B that is merely partway through ITS OWN verification
		// window. Routing the r5 follow-up reappear through the full handleReapScan
		// reintroduced exactly that sibling-early-terminate bug (#662/#B): A reappears,
		// A's timer fires, and B (still absent, its own window unelapsed) was confirmed
		// + terminated on A's timer. With the APPLY/CONFIRM split, A's snapshot apply
		// still installs B's shadow + pendingReap (so B is not orphaned), but B's
		// terminate waits for B's OWN scoped follow-up timer. For THIS task: since it
		// re-appeared, it is NOT in the returned candidates, so confirmReapForTask is a
		// no-op for it (its pendingReap was already canceled by the reappear path inside
		// applyReapSnapshot) — correct.
		//
		// `previous` is the current cache name-set: applyReapSnapshot unions it with the
		// live cache to widen the universe of first-removals it considers; the
		// authoritative diff is recomputed against the fresh snapshot inside.
		previous := map[string]struct{}{}
		if c.intentCache != nil {
			previous = c.intentCache.TaskNames()
		}
		candidates := c.applyReapSnapshot(previous, fresh, fresh.StopsAsDaemonIntentFile())
		c.confirmReapForTask(taskName, candidates)
		c.emitReapEvent("debug", "orphan-reap-candidate-reappeared", taskName,
			"removed descriptor re-appeared on disk by the follow-up tick (fresh read); applied the whole fresh snapshot through applyReapSnapshot (single atomic applier) so the reap is canceled with respawn-on-reappear, the stops cache is refreshed, and any sibling removed in the same snapshot gets its own shadow + pendingReap (confirm scoped to THIS task only — a sibling's reap waits for its OWN follow-up timer, not this one — finding 2)")
		return
	}
	// Still absent on disk. If the SM settled on its own since the mark, clear.
	if !c.smStateIsReapable(taskName) {
		c.clearRemovedTaskRuntime(taskName)
		c.emitReapEvent("debug", "orphan-reap-candidate-settled", taskName,
			"removed descriptor settled to a non-live SM state by the follow-up tick (in-flight event routed via the shadow); stale runtime bookkeeping cleared, no terminate needed")
		return
	}
	// Confirmed orphan — drive the SM-aware terminate for THIS task only.
	c.resolveConfirmedReap(d)
}

// freshIntentForReap performs the injectable fresh on-disk supervisor-intent.json
// read (finding A). Returns (intent, true) on a successful read, (nil, false)
// when no reader is wired or the read errored — callers fall back to the
// intentCache snapshot in that case.
func (c *supervisorController) freshIntentForReap() (*api.SupervisorIntentFile, bool) {
	if c.reapIntentReader == nil {
		return nil, false
	}
	intent, err := c.reapIntentReader()
	if err != nil {
		return nil, false
	}
	return intent, true
}

// taskPresentInFreshIntent reports whether `taskName` is declared in the FRESH
// on-disk intent read (finding A). The sole caller (handleReapFollowup) now guards
// the freshOK=false case BEFORE this call (#856: a fresh-read failure preserves the
// reap + re-arms rather than confirming absence), so the freshOK=true branch is the
// live path. The freshOK=false fallback to the intentCache is retained as a
// defensive no-worse-than-cache-only behavior for any future caller, but is
// unreachable from handleReapFollowup.
func (c *supervisorController) taskPresentInFreshIntent(taskName string, fresh *api.SupervisorIntentFile, freshOK bool) bool {
	taskName = canonicalSupervisorTaskName(taskName)
	if freshOK {
		names := taskNameSetFromIntent(fresh)
		_, present := names[taskName]
		return present
	}
	if c.intentCache != nil {
		// LookupCanonical (not strict Lookup) so a legacy bare-keyed cache
		// descriptor is found by the canonical query — taskName was canonicalized
		// at the top of this function, so a strict Lookup(canonical) would MISS a
		// bare-keyed remnant, the exact split-key-space class r8 closed elsewhere.
		// This freshOK=false branch is unreachable from the sole caller
		// (handleReapFollowup guards it before the call), so this changes no live
		// behavior; it only keeps the single-key-space invariant consistent for any
		// future caller (pr302 r9 sweep).
		if _, ok := c.intentCache.LookupCanonical(taskName); ok {
			return true
		}
	}
	return false
}

// replayDeferredBackoffTimerIfPending re-posts the single EvTimerDue that was
// SUPPRESSED for a removed StBackoffWaiting daemon (finding E) when its removal
// turns out to be a replace-in-place BLIP. Without the replay the one backoff
// timer that already fired during the window is lost and the reappeared daemon
// is stranded in backoff forever (the IntentWatcher posts no EvIntentUpdate for
// an unchanged-Desired re-add, and does not run Reconcile). The descriptor MUST
// be routable (in intentCache or the still-present shadow) when this runs — the
// callers refresh the cache / call this before cancelPendingReap drops the
// shadow. Uses PostSelf so the replayed EvTimerDue lands on the loop ahead of
// pre-queued external events. MUST run on the event loop.
func (c *supervisorController) replayDeferredBackoffTimerIfPending(taskName string) {
	taskName = canonicalSupervisorTaskName(taskName)
	if _, deferred := c.reapDeferredTimerDue.LoadAndDelete(taskName); !deferred {
		return
	}
	if c.eventLoop == nil {
		// Synchronous test fixtures: drive it inline.
		c.handleLoopEvent(api.LoopEvent{Kind: api.EvTimerDue, TaskName: taskName})
		return
	}
	if !c.eventLoop.PostSelf(api.LoopEvent{Kind: api.EvTimerDue, TaskName: taskName}) {
		c.emitSelfChannelSaturated(taskName, "EvTimerDue")
	}
	c.emitReapEvent("debug", "orphan-reap-backoff-timer-replayed", taskName,
		"replace-in-place reappear: replaying the backoff timer that was suppressed during the reap window so the reappeared daemon is not stranded (finding E anti-stranding)")
}

// resolvePendingReapsAgainstCurrentIntent re-evaluates EVERY pendingReap
// candidate against the CURRENT (already-swapped) intent cache. It is retained
// for the rare callers that want a full cache-based sweep on the loop (it is no
// longer the follow-up timer's path — that is handleReapFollowup, scoped to one
// task + a fresh disk read). MUST run on the event loop.
func (c *supervisorController) resolvePendingReapsAgainstCurrentIntent() {
	if c == nil || c.intentCache == nil {
		return
	}
	present := c.intentCache.TaskNames()
	var confirmedReaps []*api.SupervisorDaemon
	c.pendingReap.Range(func(key, value any) bool {
		taskName, _ := key.(string)
		if taskName == "" {
			return true
		}
		if _, here := present[taskName]; here {
			c.replayDeferredBackoffTimerIfPending(taskName)
			c.clearReapDeferredStopIfPending(taskName)
			c.cancelPendingReap(taskName)
			c.emitReapEvent("debug", "orphan-reap-candidate-reappeared", taskName,
				"removed descriptor re-appeared by the cache sweep; reap canceled")
			return true
		}
		if !c.smStateIsReapable(taskName) {
			c.clearRemovedTaskRuntime(taskName)
			c.emitReapEvent("debug", "orphan-reap-candidate-settled", taskName,
				"removed descriptor settled to a non-live SM state by the cache sweep; stale runtime bookkeeping cleared, no terminate needed")
			return true
		}
		if d, ok := value.(*api.SupervisorDaemon); ok && d != nil {
			confirmedReaps = append(confirmedReaps, d)
		}
		return true
	})
	for _, d := range confirmedReaps {
		c.resolveConfirmedReap(d)
	}
}

// armReapFollowup schedules a bounded follow-up resolution for a pendingReap
// task. It is the guaranteed second tick (finding 1): the two
// refreshSupervisorIntent call sites only fire on an intent CHANGE, so in the
// common uninstall/remove case (intent then stable) the confirming refresh
// would never arrive. The timer POSTS an EvReapFollowup event onto the loop;
// the on-loop handleReapFollowup re-reads intent from disk and resolves the
// task. Idempotent: a duplicate arm while one is already pending for the SAME
// generation is a no-op; a fresh pendingReap generation always arms a new timer.
//
// MUST run on the event loop. The timer is generation-tagged (finding C): the
// armed generation is recorded in reapFollowupArmed and posted in the event
// body, and disarmReapFollowup Stops the live handle so a canceled/confirmed
// reap's old timer cannot fire against a later window for the same task.
func (c *supervisorController) armReapFollowup(taskName string) {
	taskName = canonicalSupervisorTaskName(taskName)
	generation := c.currentReapGeneration(taskName)
	if existing, loaded := c.reapFollowupArmed.Load(taskName); loaded {
		if g, ok := existing.(int); ok && g == generation {
			return // a timer for the current generation is already pending
		}
		// A timer for a STALE generation is still registered (should be rare —
		// markPendingReap bumps the generation, and disarm Stops the old timer).
		// Stop it before arming the new one so it cannot fire against this window.
		c.stopReapTimer(taskName)
	}
	c.reapFollowupArmed.Store(taskName, generation)
	delay := c.reapFollowupDelay
	if delay <= 0 {
		delay = reapFollowupDefaultDelay
	}
	after := c.reapAfterFunc
	if after == nil {
		after = func(d time.Duration, f func()) reapTimer { return time.AfterFunc(d, f) }
	}
	// The timer callback runs on the timer goroutine and does NOTHING but POST
	// an EvReapFollowup onto the loop; ALL resolution runs in handleReapFollowup
	// on the loop goroutine (findings A/B/C/H/I). When the event loop is absent
	// (synchronous unit-test fixtures), invoke the handler inline so those tests
	// keep deterministic single-goroutine semantics.
	timer := after(delay, func() {
		if c.eventLoop == nil {
			c.handleReapFollowup(taskName, generation)
			return
		}
		c.eventLoop.Post(api.LoopEvent{
			Kind:     evReapFollowup,
			TaskName: taskName,
			Body: map[string]any{
				reapFollowupGenerationBodyKey: generation,
			},
		})
	})
	c.reapTimers.Store(taskName, timer)
}

// currentReapGeneration returns the task's current reap generation, defaulting
// to 0 when none is recorded. MUST run on the event loop.
func (c *supervisorController) currentReapGeneration(taskName string) int {
	if v, ok := c.reapGeneration.Load(taskName); ok {
		if g, ok := v.(int); ok {
			return g
		}
	}
	return 0
}

// bumpReapGeneration advances the task's reap generation and returns the new
// value. Called by markPendingReap when a FRESH pendingReap window opens for the
// task (a brand-new disappearance), so each window's follow-up timer carries a
// distinct generation and a stale timer from a previous window is detectable
// (finding C). MUST run on the event loop.
func (c *supervisorController) bumpReapGeneration(taskName string) int {
	next := c.currentReapGeneration(taskName) + 1
	c.reapGeneration.Store(taskName, next)
	return next
}

// disarmReapFollowup clears the armed record for a task whose reap was canceled
// or confirmed-dead, AND Stops the live timer handle so its callback never fires
// against a LATER window for the same task (finding C — a bare flag clear left
// the underlying time.AfterFunc running). It deliberately does NOT delete
// reapGeneration: the generation must stay MONOTONIC per task so a subsequent
// remove→reappear→remove-again opens a STRICTLY GREATER generation (bumpReapGeneration
// reads the current value + 1), letting the stale-timer guard in handleReapFollowup
// distinguish the first window's late timer from the second window. A still-pending
// timer that fires after disarm is neutralized by BOTH the generation guard and
// the pendingReap.Load miss. MUST run on the event loop.
func (c *supervisorController) disarmReapFollowup(taskName string) {
	taskName = canonicalSupervisorTaskName(taskName)
	c.reapFollowupArmed.Delete(taskName)
	c.stopReapTimer(taskName)
}

// stopReapTimer Stops + drops the live follow-up timer handle for a task, if
// any. Stop is best-effort: a timer that has already fired (its post is in
// flight) is additionally neutralized by the generation guard in
// handleReapFollowup. MUST run on the event loop.
func (c *supervisorController) stopReapTimer(taskName string) {
	if v, ok := c.reapTimers.LoadAndDelete(taskName); ok {
		if t, ok := v.(reapTimer); ok && t != nil {
			t.Stop()
		}
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

// getSMStateCanonical resolves the controller's SM state for a task by BOTH the
// canonical leading-backslash key AND the toggled bare key (pr302 r7 finding 4 —
// the SM-state mirror of LookupCanonical). Since pr302 r8 the whole SM keys ONE
// canonical space: handleLoopEvent canonicalizes ev.TaskName ONCE at the SM
// ingestion boundary (routeKey := canonicalSupervisorTaskName(ev.TaskName)) so
// every smStates.Store downstream keys canonical, and hydrateControllerRunningStates
// seeds smStates canonical too. The both-form probe here is therefore post-r8
// belt-and-suspenders: the ingestion boundary structurally prevents a bare-keyed
// StRunning row, so the toggled lookup is cheap insurance for a legacy remnant, not
// a live divergence any current write path produces. The getter tries the requested
// key first (the common canonical case), then the toggled form (strip-or-add the
// leading "\"). GetSMState itself keeps strict exact-key semantics for callers that
// depend on it.
func (c *supervisorController) getSMStateCanonical(taskName string) (api.SMState, bool) {
	if st, ok := c.GetSMState(taskName); ok {
		return st, true
	}
	if alt := toggleSupervisorTaskNameBackslash(taskName); alt != taskName {
		if st, ok := c.GetSMState(alt); ok {
			return st, true
		}
	}
	return api.StIdle, false
}

// loadReapMarkerCanonical probes a controller bookkeeping sync.Map (ownSpawned /
// reaperOutstanding) under BOTH the canonical and the toggled bare key (pr302 r7
// finding 4 sweep). Since pr302 r8 those markers are stored under the CANONICAL
// key: executeSideEffect's spawn-success branch stores under d.TaskName, and d is
// the canonicalized descriptor copy handleLoopEvent resolves after the SM ingestion
// boundary canonicalizes ev.TaskName. The both-form probe is therefore post-r8
// belt-and-suspenders — the ingestion boundary structurally prevents a bare-keyed
// marker, so the toggled lookup is cheap insurance for a legacy remnant, not a live
// divergence any current write path produces. Without it the F1 own-reaper deferral
// decision (owned || reaperPending) would read FALSE for such a remnant and wrongly
// take the non-deferred clear path. Mirrors getSMStateCanonical / LookupCanonical.
func (c *supervisorController) loadReapMarkerCanonical(m *sync.Map, taskName string) bool {
	if m == nil {
		return false
	}
	if _, ok := m.Load(taskName); ok {
		return true
	}
	if alt := toggleSupervisorTaskNameBackslash(taskName); alt != taskName {
		if _, ok := m.Load(alt); ok {
			return true
		}
	}
	return false
}

// toggleSupervisorTaskNameBackslash returns the OTHER backslash form of a task
// name: a canonical "\foo" toggles to the bare "foo" and a bare "foo" toggles to
// the canonical "\foo". Used by the canonical-aware reap-path lookups to probe a
// legacy bare-keyed entry by a canonical query (and vice versa). The empty string
// toggles to itself.
func toggleSupervisorTaskNameBackslash(taskName string) string {
	if taskName == "" {
		return taskName
	}
	if strings.HasPrefix(taskName, `\`) {
		return strings.TrimPrefix(taskName, `\`)
	}
	return `\` + taskName
}

// smStateIsReapable reports whether the controller's current SM state for the
// task names a LIVE daemon the reap should terminate — StSpawning, StRunning,
// StBackoffWaiting, or StExiting. StIdle and StQuarantined are settled (no
// live child to terminate), and an untracked task (GetSMState ok=false) is
// likewise not reapable. A task already at StExiting is included so a reap
// confirmed against a still-terminating descriptor is idempotent, but
// reapRemovedDaemon's own re-check below avoids double-driving a terminate.
//
// pr302 r7 finding 4: the lookup goes through getSMStateCanonical so a daemon
// started under a LEGACY BARE TaskName (SM state stored under the bare key) is
// still seen as reapable when the reap path queries it by the canonical name.
func (c *supervisorController) smStateIsReapable(taskName string) bool {
	st, ok := c.getSMStateCanonical(taskName)
	if !ok {
		return false
	}
	switch st {
	case api.StSpawning, api.StRunning, api.StBackoffWaiting, api.StExiting:
		return true
	}
	return false
}

// reapOutcome classifies the result of a confirmed orphan reap so the caller
// (resolveConfirmedReap) can decide whether to clear bookkeeping, preserve it
// for retry, or defer it to a later spawn-completion / exit event.
type reapOutcome int

const (
	// reapTerminatedDead — the orphan is gone: either the SM settled to StIdle
	// with no live child (StBackoffWaiting cancel) or the terminate returned nil
	// (PID confirmed gone). Safe to clear all bookkeeping.
	reapTerminatedDead reapOutcome = iota
	// reapDeferred — the reap could not complete synchronously: a queued stop is
	// pending against an in-flight spawn (StSpawning), or a terminate is already
	// in flight (StExiting). The completing spawn / exit event (routed via the
	// reaping shadow) applies the stop later. Preserve the entry + shadow.
	reapDeferred
	// reapTerminateFailed — the terminate was issued but FAILED (non-nil error;
	// the targeted PID may still be alive). Preserve the entry + pendingReap so a
	// later follow-up tick retries; clearing would lose the supervisor handle.
	reapTerminateFailed
)

// reapRemovedDaemon drives the SM-aware terminate for a descriptor that
// disappeared from intent and stayed absent across the verification window.
// It reuses the EXISTING terminate side-effect path — the same
// api.Transition row + executeSideEffect StExiting dispatch the operator-stop
// (EvIntentUpdate stopped) path uses — rather than a raw kill, so the
// audit trail (daemon-terminate-requested / daemon-terminated) and the
// Job-Object teardown stay identical to an operator-initiated stop.
//
// The standard handleLoopEvent path cannot be reused here because the
// descriptor has already been removed from intentCache. (handleLoopEvent now
// falls back to the reaping shadow, but that fallback serves the task's OWN
// in-flight loop events — EvChildExit / EvTimerDue / queued-stop completion —
// NOT a synthetic reap-driven EvIntentUpdate, which the controller drives
// directly here so the terminate result is observable.) We run the SM
// transition explicitly against the CAPTURED OLD descriptor and dispatch its
// side effect. The descriptor carries the TaskName + Command the production
// TerminateFunc needs; the live PID it kills comes from the runtime tracker
// keyed by TaskName (makeProductionTerminateFnWithStatePath), so the captured
// (pre-removal) descriptor terminates the right child.
//
// Returns a reapOutcome the caller uses to decide bookkeeping: a CONFIRMED-dead
// terminate clears everything; a FAILED terminate preserves it for retry
// (finding 2); a deferred (StSpawning queued stop / StExiting already-exiting)
// keeps the entry + shadow so the later spawn-completion / exit event finishes
// the stop (finding 4).
func (c *supervisorController) reapRemovedDaemon(d *api.SupervisorDaemon) reapOutcome {
	if c == nil || d == nil {
		return reapTerminatedDead
	}
	taskName := canonicalSupervisorTaskName(d.TaskName)

	// pr302 r7 finding 4: resolve the SM state under BOTH the canonical and the raw
	// bare key (getSMStateCanonical). A daemon STARTED by this supervisor under a
	// LEGACY BARE TaskName has its SM/queued/tracker rows keyed by the BARE name
	// (handleLoopEvent stores under the raw ev.TaskName), so a plain
	// GetSMState(canonical) MISSES the live StRunning state and bails early to
	// reapTerminatedDead — the terminate never fires and the orphan survives. `smKey`
	// is the ACTUAL key the state lives under (canonical if present there, else the
	// bare form); every smStates / queuedActions / tracker write below uses smKey so
	// the reap mutates the EXISTING row instead of creating a parallel canonical entry
	// that splits the key space. The terminate fn itself keys off the captured
	// descriptor's raw d.TaskName (tracker.Get(d.TaskName)), so the kill already
	// targets the right PID under either key form.
	currentState, ok := c.getSMStateCanonical(taskName)
	if !ok {
		return reapTerminatedDead
	}
	smKey := taskName
	if _, exact := c.GetSMState(taskName); !exact {
		if alt := toggleSupervisorTaskNameBackslash(taskName); alt != taskName {
			if _, altOK := c.GetSMState(alt); altOK {
				smKey = alt
			}
		}
	}
	// Already terminating (StExiting) — a terminate is in flight; do not
	// double-drive it. The subsequent EvChildExit (routed via the reaping shadow)
	// settles the task. Defer: the entry + shadow stay until that exit settles it.
	if currentState == api.StExiting {
		// pr302 r6 finding 3: a descriptor can reach StExiting because of a MANUAL
		// RESTART (queued_action=respawn) — the operator restarted the daemon, the
		// terminate fired, and the queued respawn is waiting for the real EvChildExit
		// to drive StExiting -> StSpawning. If the descriptor is then REMOVED from
		// intent and confirmed-absent, that queued respawn is now STALE: the daemon
		// the operator wanted restarted has been deleted. Left in place, the real
		// EvChildExit (StExiting + EvChildExit + queued_action=respawn -> StSpawning,
		// supervisor_state_machine.go:145) would RELAUNCH the removed descriptor —
		// resurrecting exactly the orphan this reap exists to kill, on a port the
		// operator just freed.
		//
		// CLEAR the queued respawn so the awaited EvChildExit instead drives
		// StExiting + queued_action="" -> StIdle (STOPPED, no respawn;
		// supervisor_state_machine.go:148). The terminate the manual restart already
		// issued is honored; only the now-meaningless relaunch is canceled. We do NOT
		// double-drive a terminate here (one is in flight) — we only cancel the stale
		// relaunch. queued_action values other than "respawn" ("" / "none" / "stop")
		// already drive StExiting -> StIdle on EvChildExit, so this clear is scoped to
		// the respawn case to avoid disturbing an operator-set stop already pending.
		if v, ok := c.queuedActions.Load(smKey); ok {
			if qa, ok := v.(string); ok && qa == "respawn" {
				c.queuedActions.Store(smKey, "")
				c.emitReapEvent("info", "orphan-reap-cleared-stale-respawn", taskName,
					"descriptor confirmed removed while the daemon was StExiting with a queued manual-restart respawn; clearing the stale queued_action=respawn so the awaited EvChildExit drives StExiting -> StIdle (STOPPED) instead of StExiting -> StSpawning (which would relaunch the removed descriptor and re-orphan it) (finding 3)")
			}
		}
		return reapDeferred
	}
	if !c.smStateIsReapable(taskName) {
		return reapTerminatedDead
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
	if v, ok := c.queuedActions.Load(smKey); ok {
		if qa, ok := v.(string); ok {
			smCtx.QueuedAction = qa
		}
	}

	newState, side, _, matched := api.Transition(currentState, api.EvIntentUpdate, smCtx)
	if !matched {
		// No terminate row for this state under stopped intent (e.g.
		// StQuarantined was excluded by smStateIsReapable already, so this is
		// defensive). Treat as confirmed-dead so the caller clears bookkeeping.
		return reapTerminatedDead
	}

	c.emitReapEvent("info", "orphan-reap-terminate", taskName,
		"descriptor confirmed removed from intent across the verification window; driving SM-aware terminate so the orphaned child releases its port (supervisor-side backstop for a skipped install-time kill)")

	// Mirror handleLoopEvent's queued-action write-back for the new side
	// string so a StSpawning self-loop's queued_action=stop is preserved for
	// the subsequent EvHealthOK/EvChildExit (consistency with the operator
	// stop path). Keyed by smKey (finding 4) so a legacy bare-key daemon's
	// queued_action is written under the SAME key its SM state lives under,
	// not a parallel canonical entry the spawn-completion event would never read.
	switch {
	case strings.Contains(side, "set queued_action=stop"):
		c.queuedActions.Store(smKey, "stop")
	case strings.Contains(side, "queued_action=respawn"):
		c.queuedActions.Store(smKey, "respawn")
	case strings.Contains(side, "queued_action=none"), strings.Contains(side, "clear queued_action"):
		c.queuedActions.Store(smKey, "")
	}
	c.smStates.Store(smKey, newState)
	if newState == api.StIdle && currentState != api.StIdle && c.tracker != nil {
		c.tracker.MarkExited(d.TaskName)
	}
	if c.tracker != nil && c.statePath != "" {
		_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, d.TaskName)
	}

	// Dispatch the side effect and classify the outcome by the NEW state:
	//
	//   - StExiting (from StRunning): the StExiting case fires c.terminate and
	//     returns its error. nil => the orphan is confirmed gone (clear); a
	//     non-nil error => the PID may still be alive, so preserve the entry +
	//     pendingReap for a follow-up retry rather than losing the supervisor
	//     handle (finding 2).
	//
	//   - StSpawning (from StSpawning, "set queued_action=stop"): NO terminate
	//     fired yet — the queued stop is applied by the later EvHealthOK /
	//     EvChildExit spawn-completion event, which now routes via the reaping
	//     shadow. Defer so the entry + queued stop + shadow survive until that
	//     event terminates the just-spawned child (finding 4).
	//
	//   - StIdle (from StBackoffWaiting, "cancel timer"): no live child existed
	//     (backoff holds no PID), the timer is canceled — confirmed-dead.
	sideErr := c.executeSideEffect(side, newState, d, api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: taskName})
	switch newState {
	case api.StExiting:
		// Finding F: a non-nil terminate error does NOT always mean "process may
		// still be alive". The production TerminateFunc returns errTerminateTargetGone
		// when the targeted process is ALREADY GONE (no running PID recorded; or the
		// kill succeeded but a post-kill teardown / persist failed). Those are
		// CONFIRMED-DEAD — clearing is correct; preserving + retrying forever would
		// loop and leave a later re-registration stuck at stale StRunning-no-PID.
		// Only a GENUINELY-uncertain error (PID-state query / verify / kill failure,
		// NOT wrapped with errTerminateTargetGone) is preserved for retry (finding 2).
		if sideErr != nil && !errors.Is(sideErr, errTerminateTargetGone) {
			// Terminate FAILED while the process may still be alive. Roll the SM
			// state (and the queued action we just wrote) BACK to the pre-transition
			// LIVE state so a later follow-up tick re-drives the FULL terminate from
			// StRunning rather than getting stuck at StExiting, where
			// reapRemovedDaemon's own StExiting short-circuit (which assumes a real
			// EvChildExit will finish the exit) would defer forever — no real exit is
			// coming for a failed terminate. Preserving the live state is the honest
			// reflection of reality (the daemon is, as far as we know, still running)
			// and is what makes the retry actually re-issue the kill (finding 2).
			//
			// pr302 r8 review finding 2: roll back under smKey — the SAME key the
			// forward writes above (1738 smStates.Store(smKey), 1732/1734/1736
			// queuedActions.Store(smKey)) used — NOT the canonical taskName. For a
			// pre-fix LEGACY BARE daemon whose SM/queued rows live under the bare key,
			// a canonical Store would leave smStates[bare]=StExiting STUCK and write a
			// parallel smStates[canonical]=<live> — the split key space the function's
			// own invariant (1640-1644) forbids and which would defeat the retry (the
			// follow-up tick reads the bare-keyed StExiting and defers forever). With
			// the single-key-space fix a NEW daemon's smKey == canonical, so this is a
			// no-op for the common case and the correct symmetric rollback for the
			// legacy bare-keyed remnant.
			c.smStates.Store(smKey, currentState)
			if v, ok := c.queuedActions.Load(smKey); ok {
				if qa, ok := v.(string); ok && qa == "" {
					// Clear the "queued_action=none" we wrote for the StExiting
					// transition so the rolled-back live state starts the retry
					// clean.
					c.queuedActions.Delete(smKey)
				}
			}
			return reapTerminateFailed
		}
		// sideErr == nil (clean terminate) OR errTerminateTargetGone (already gone):
		// the orphan is confirmed dead.
		//
		// Finding G: for an OWN-spawned daemon whose cmd.Wait reaper is still
		// outstanding, the terminate killed the child but the reaper goroutine will
		// STILL run its MarkExited + persist + post the real EvChildExit. If we
		// clearRemovedTaskRuntime (tracker.Remove + drop shadow) RIGHT NOW, that
		// later MarkExited resurrects a stale idle tracker row (the removal is not
		// durable) AND the real EvChildExit orphan-drops against the now-missing
		// shadow. So DEFER: keep the shadow + pendingReap so the real EvChildExit
		// routes via the shadow (StExiting + EvChildExit -> StIdle), settling the
		// task to a non-reapable state; the follow-up tick then clears the
		// bookkeeping AFTER the reaper's MarkExited has run, so the clear is durable.
		//
		// pr302 r7 finding 1: the deferral predicate is the OUTSTANDING OWN REAPER,
		// NOT the terminate error being nil. errTerminateTargetGone fires on TWO
		// production paths (makeProductionTerminateFnWithStatePath): case (a) no
		// running PID recorded (supervise.go:2237) — no live reaper is coming, safe to
		// clear; and case (b) the kill SUCCEEDED (MarkTerminated ran) but the
		// supervisor-state.json persist FAILED (supervise.go:2357). In case (b), if
		// this is an own-spawned daemon, its cmd.Wait reaper is STILL outstanding and
		// will later run MarkExited + post the real EvChildExit. Clearing on the
		// gone-error then races a re-add exactly as the nil case does: a late reaper
		// can clear the NEW pid or recreate a stale idle tracker row. So defer whenever
		// the own reaper is still outstanding, regardless of whether the terminate
		// returned nil or a gone-error. reaperOutstanding is the authoritative signal —
		// it is set at spawn and cleared ONLY when handleLoopEvent observes the real
		// EvChildExit — so when it is still set a real exit IS coming and the clear must
		// wait for it. A foreign warm-start PID (not own-spawned, no reaper outstanding)
		// has no cmd.Wait, so it is NOT deferred — synthesized/cleared immediately as
		// before. The marker lookups go through loadReapMarkerCanonical so a legacy
		// bare-key daemon's markers (stored under the bare key at spawn time) are still
		// seen when the reap path queries by the canonical name (finding 4 sweep).
		owned := c.loadReapMarkerCanonical(&c.ownSpawned, taskName)
		reaperPending := c.loadReapMarkerCanonical(&c.reaperOutstanding, taskName)
		if owned || reaperPending {
			note := "own-spawned daemon terminated; deferring the tracker/shadow clear until the daemon's own cmd.Wait reaper posts the real EvChildExit, so a late MarkExited cannot resurrect a stale idle row and the exit is not orphan-dropped (finding G)"
			if sideErr != nil {
				note = "own-spawned daemon terminate returned target-gone (kill succeeded but the state persist failed) while its cmd.Wait reaper is still outstanding; deferring the tracker/shadow clear until the real EvChildExit arrives so a late MarkExited cannot clear a re-added pid or recreate a stale idle row (finding 1)"
			}
			c.emitReapEvent("debug", "orphan-reap-await-own-reaper", taskName, note)
			return reapDeferred
		}
		// Confirmed dead with no own reaper outstanding — clear bookkeeping now.
		return reapTerminatedDead
	case api.StSpawning:
		// The reap set queued_action=stop on a still-spawning daemon (the terminate
		// is deferred to the spawn-completion event). Mark it REAP-originated so a
		// replace-in-place reappear before the spawn completes can clear the stop
		// and NOT terminate the re-added daemon (finding D).
		c.reapDeferredStop.Store(taskName, true)
		return reapDeferred
	default:
		return reapTerminatedDead
	}
}

// clearReapDeferredStopIfPending clears a REAP-originated queued_action=stop when
// a removed-while-StSpawning daemon REAPPEARS before its spawn completes (finding
// D) — but ONLY when the re-added intent declares the daemon RUNNING (pr302 r7
// finding 2). Without this, the queued stop the reap set survives the cancel and
// the next EvHealthOK drives the re-added daemon to StExiting -> terminate. It only
// clears a stop the REAP set (the marker), never an operator-set stop. MUST run on
// the event loop, AFTER the caches have been swapped from the fresh snapshot
// (applyReapSnapshot's step-3 swap precedes the reappear-arm reconcile), so the
// just-swapped stops cache reflects the re-added intent.
//
// pr302 r7 finding 2 (regression fix): the re-add can carry Desired=stopped. In
// that case the operator re-declared the daemon STOPPED, so the queued stop must be
// PRESERVED — the StSpawning state machine does NOT consult IntentDesired on its
// EvHealthOK transition (StSpawning + EvHealthOK is driven by queued_action alone),
// so clearing the queued stop UNCONDITIONALLY (the r6 behavior) would let the child
// reach StRunning even though the fresh intent says stopped. queueRespawnOnReapCancelIfNeeded
// already gates its respawn on the SAME just-swapped stops cache via IsActiveStop;
// this mirrors that predicate so the running-vs-stopped decision is consistent
// across both reappear sub-paths. The reapDeferredStop MARKER is consumed
// (LoadAndDelete) in either case — the reap window is closing regardless — so it
// cannot leak into a later re-registration window; only the queued_action mutation
// is gated on the re-add being running.
func (c *supervisorController) clearReapDeferredStopIfPending(taskName string) {
	taskName = canonicalSupervisorTaskName(taskName)
	if _, deferred := c.reapDeferredStop.LoadAndDelete(taskName); !deferred {
		return
	}
	// The re-added intent must be RUNNING for the reap-originated stop to be
	// cleared. Read the just-swapped stops cache (handleReapScan / applyReapSnapshot
	// swapped it before this reappear-arm runs).
	di := c.daemonIntent.Lookup(taskName)
	if stop, _ := di.IsActiveStop(time.Now().UTC()); stop {
		// Re-added WITH Desired=stopped: PRESERVE the queued stop so the
		// spawn-completion EvHealthOK still drives the just-started child to
		// stopped (StSpawning does not consult IntentDesired on that transition).
		c.emitReapEvent("debug", "orphan-reap-deferred-stop-preserved", taskName,
			"removed-while-spawning daemon re-appeared with Desired=stopped before its spawn completed; PRESERVING the reap-originated queued_action=stop so the spawn-completion EvHealthOK still drives the re-added child to stopped (the fresh intent says stopped, and StSpawning+EvHealthOK is driven by queued_action, not IntentDesired) (finding 2)")
		return
	}
	// Probe the queued-action key under BOTH forms (finding 4 sweep): a legacy
	// bare-key daemon's queued_action was written under the bare key by
	// reapRemovedDaemon's smKey, so a canonical-only lookup would miss it and leave
	// the stop in place — the spawn-completion event would then kill the re-added
	// running daemon. For the common canonical daemon the bare probe finds nothing.
	for _, key := range distinctTaskNameKeyForms(taskName) {
		if v, ok := c.queuedActions.Load(key); ok {
			if qa, ok := v.(string); ok && qa == "stop" {
				c.queuedActions.Store(key, "")
				c.emitReapEvent("debug", "orphan-reap-deferred-stop-cleared", taskName,
					"removed-while-spawning daemon re-appeared RUNNING before its spawn completed; clearing the reap-originated queued_action=stop so the spawn-completion event does NOT terminate the re-added daemon (finding D)")
			}
		}
	}
}

// distinctTaskNameKeyForms returns the task name plus its toggled-backslash form,
// deduplicated (the empty string and any name that toggles to itself yields a
// single-element slice). Used by the canonical-aware reap-path sweeps that must
// touch a per-task map entry stored under either the canonical or the raw bare key.
func distinctTaskNameKeyForms(taskName string) []string {
	alt := toggleSupervisorTaskNameBackslash(taskName)
	if alt == taskName {
		return []string{taskName}
	}
	return []string{taskName, alt}
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
	// pr302 r7 finding 4 sweep: the per-task SM / queued / own-spawned maps and the
	// tracker are keyed by the spawn-time TaskName, which for a LEGACY BARE daemon is
	// the bare form (no leading backslash) — see reapRemovedDaemon's smKey. A
	// canonical-only Delete would LEAK the bare-keyed rows. Delete BOTH key forms; for
	// the common canonical-key daemon the bare-form delete is a no-op (no such entry).
	// The reap-bookkeeping maps below (pendingReap / reapShadow / reapDeferred* /
	// follow-up) are keyed canonically by the reap path itself, so they need only the
	// canonical delete.
	bareKey := toggleSupervisorTaskNameBackslash(taskName)
	c.smStates.Delete(taskName)
	c.queuedActions.Delete(taskName)
	// P2-2 (Codex PR-3): a removed/de-registered task must not leave a stale F1
	// gateCleared bypass that a same-name re-registration would consume to skip the
	// port probe. Cheap no-op when unset.
	c.gateCleared.Delete(taskName)
	if bareKey != taskName {
		c.smStates.Delete(bareKey)
		c.queuedActions.Delete(bareKey)
		c.gateCleared.Delete(bareKey)
	}
	// Drop any pending orphan-reap candidate: a removal that reaches the
	// bookkeeping clear (either after a confirmed reap drove the terminate, or
	// because the task was never reapable) must not leave a stale pendingReap
	// entry that a later, unrelated refresh would re-evaluate. A re-registration
	// under the same task name starts the reap window fresh from a present
	// descriptor.
	c.pendingReap.Delete(taskName)
	// Drop the reaping shadow + disarm the follow-up tick: the reap is done
	// (terminated, settled, or the task was never reapable), so the descriptor
	// no longer needs to stay routable and no further follow-up resolution is
	// owed. A still-pending follow-up timer that later fires becomes a cheap
	// no-op (pendingReap.Load misses).
	c.reapShadow.Delete(taskName)
	c.disarmReapFollowup(taskName)
	// Drop any suppressed-backoff-timer marker (finding E): the reap is done, so
	// there is no blip to replay the timer against. A confirmed removal that
	// suppressed a backoff EvTimerDue during the window drove StBackoffWaiting ->
	// StIdle ("cancel timer") in reapRemovedDaemon, so the suppressed timer is
	// correctly abandoned here rather than leaked into a future window.
	c.reapDeferredTimerDue.Delete(taskName)
	// Drop the reap-originated deferred-stop marker (finding D): a confirmed
	// removal that deferred a StSpawning stop is being cleared (the stop was
	// applied by the spawn-completion event, or the task is gone), so the marker
	// must not leak into a later re-registration window.
	c.reapDeferredStop.Delete(taskName)
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
	if bareKey != taskName {
		c.ownSpawned.Delete(bareKey)
	}
	if c.tracker != nil {
		c.tracker.Remove(taskName)
		if bareKey != taskName {
			// A legacy bare-key daemon's tracker row is under the bare key (the
			// spawn-time TaskName). Remove + persist that form too so the orphan's
			// runtime row is durably cleared, not left stale under the bare key.
			c.tracker.Remove(bareKey)
		}
		if c.statePath != "" {
			_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, taskName)
			if bareKey != taskName {
				_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, bareKey)
			}
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

// lookupDescriptorWithShadow resolves the descriptor handleLoopEvent should run
// the SM against: the live intentCache row first, falling back to the reaping
// shadow when the task is in the pendingReap window (its descriptor removed from
// intent but its reap not yet confirmed). The shadow fallback is the common-root
// fix: without it, every in-flight event for a removed-but-not-yet-reaped task
// (EvChildExit from the child's own reaper, EvTimerDue from an armed backoff
// timer, the EvHealthOK/EvChildExit completing a queued stop on a StSpawning
// task) would miss the cache and be orphan-dropped — wedging the SM (stale
// StRunning over a dead child, a backoff that never re-arms, a queued stop lost
// before the spawn completes). Returning the shadow descriptor lets those events
// route NORMALLY through the SM. A genuinely-unknown task (no cache row, no
// shadow) returns ok=false and the caller drops the event as before.
func (c *supervisorController) lookupDescriptorWithShadow(taskName string) (*api.SupervisorDaemon, bool) {
	if c == nil {
		return nil, false
	}
	// pr302 r8 single-key-space invariant: handleLoopEvent now calls this with the
	// CANONICAL routeKey. The descriptor cache (IntentCache.daemonByTask) is keyed by the
	// RAW on-disk d.TaskName, which for a LEGACY / hand-written intent row is the BARE
	// form, so a plain Lookup(canonical) would MISS it — resolve via LookupCanonical
	// (probes both key forms). The reapShadow is already CANONICAL-keyed (the reap
	// detection side stores it under the canonical name), so a canonical Load hits; the
	// toggled-form fallback below is defensive belt-and-suspenders for any pre-fix
	// bare-keyed shadow that a cold-restart resume could carry.
	if c.intentCache != nil {
		if d, ok := c.intentCache.LookupCanonical(taskName); ok {
			return d, true
		}
	}
	for _, key := range distinctTaskNameKeyForms(taskName) {
		if v, ok := c.reapShadow.Load(key); ok {
			if d, ok := v.(*api.SupervisorDaemon); ok && d != nil {
				return d, true
			}
		}
	}
	return nil, false
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
	// Controller-internal reap lifecycle events are intercepted HERE — before any
	// of the api.Transition dispatch below — and routed to the reap handlers. They
	// are NOT rows in the SM table; running their mutations on this single loop
	// goroutine is the class fix for Codex pr302 r3 findings A/B/C/H/I (the
	// off-loop watcher / IPC / follow-up-timer goroutines now only DETECT + POST).
	switch ev.Kind {
	case evReapScan:
		prev, _ := ev.Body[reapScanPreviousNamesBodyKey].(map[string]struct{})
		updated, _ := ev.Body[reapScanIntentBodyKey].(*api.SupervisorIntentFile)
		stops, _ := ev.Body[reapScanStopsBodyKey].(*api.DaemonIntentFile)
		c.handleReapScan(prev, updated, stops)
		// Signal the synchronous reconcile-apply barrier (finding 1), if one is
		// attached. Closing AFTER handleReapScan returns makes the cache swap
		// happen-before the off-loop caller's post-barrier read. A nil/absent
		// channel (the IntentWatcher async path) is a no-op.
		if doneCh, ok := ev.Body[reapScanDoneBodyKey].(chan struct{}); ok && doneCh != nil {
			close(doneCh)
		}
		return
	case evReapFollowup:
		generation, _ := ev.Body[reapFollowupGenerationBodyKey].(int)
		c.handleReapFollowup(ev.TaskName, generation)
		return
	case evReapBarrier:
		// TEST-ONLY: signal the waiting test goroutine that the loop has drained
		// up to and including this event.
		if ch, ok := ev.Body[reapBarrierResultBodyKey].(chan struct{}); ok {
			close(ch)
		}
		return
	case evParoleTick:
		// F2 quarantine-parole scan, run ON the loop goroutine (posted by
		// runQuarantineParoleMonitor). Running it here serializes the parole map
		// mutations (create / clear / Delete / field advance) + the EvManualRestart
		// self-post against every other parole and SM mutation, closing the pre-fix
		// off-loop TOCTOU. Carries no TaskName (a fleet-wide scan), like evReapScan.
		now := time.Now().UTC()
		c.runQuarantineParoleTick(now)
		// L1 self-heal: the reallocation stabilize-dwell reset shares this tick's
		// cadence + loop goroutine (both mutate loop-owned in-memory maps only).
		c.runReallocDwellTick(now)
		return
	case evReallocApplied:
		// L1 self-heal: apply the off-loop reallocation worker's outcome ON the
		// loop (intent-cache refresh + respawn, or the pool-exhausted quarantine).
		// Not an api.Transition row; intercepted here like the evReap* / parole
		// events. The per-task in-flight marker is cleared by the worker's own defer
		// (handleReallocReq); handleReallocApplied does NOT re-clear it here.
		c.handleReallocApplied(ev)
		return
	}
	// pr302 r8 single-key-space invariant: canonicalize ev.TaskName ONCE at the SM
	// ingestion boundary so the ENTIRE SM + reap + loop bookkeeping system (smStates /
	// queuedActions / ownSpawned / reaperOutstanding / reapShadow / pendingReap /
	// reapDeferred* maps + the tracker calls) operates in ONE canonical key space.
	//
	// Root of the bug CLASS the r6/r7 detection-side helpers only half-closed: the
	// reap-DETECTION side (handleReapScan) keys CANONICAL (IntentCache.TaskNames()
	// canonicalizes; pendingReap/reapShadow/markPendingReap store canonical), and
	// hydrateControllerRunningStates already seeds smStates CANONICAL
	// (supervise_liveness.go), but this loop USED to key off the RAW ev.TaskName —
	// which for a LEGACY / hand-written intent row whose TaskName LACKS the leading
	// backslash (and for the real in-flight EvChildExit runCrashEventBridge posts with
	// the bare ev.Daemon.TaskName) is the BARE form. The two key spaces diverged and
	// the reap wedged (orphan-dropped real exit, stuck StExiting, backoff respawning a
	// removed descriptor). Canonicalizing here unifies them.
	//
	// Everything downstream (the api.Transition dispatch below, executeSideEffect, and
	// handleBackoffWaiting) receives this canonicalized ev + the canonicalized
	// descriptor copy resolved below, so their ev.TaskName / d.TaskName map writes
	// become canonical with NO edits to executeSideEffect (kept byte-identical for the
	// PR #303 rebase). The descriptor COPY keeps the spawn/terminate path correct: the
	// production spawn fn uses d.Command + d.Args verbatim (not d.TaskName), the tracker
	// + runningPIDs + overlay are all already canonical-keyed, and the serena-proxy
	// --task-name lives in d.Args (re-checked by the child against its OWN on-disk
	// descriptor), so the canonical TaskName on the in-memory copy changes no spawn arg.
	routeKey := canonicalSupervisorTaskName(ev.TaskName)
	ev.TaskName = routeKey
	// P2-A / P2-ii (Codex PR-3): loop-side stop re-check for EVERY automatic-trigger
	// EvManualRestart (F3 quarantine self-heal AND the F1 port-gate worker's reaped +
	// unverified arms — all flagged require_running_intent). Those triggers decide to
	// restart OFF-LOOP (sweep / worker goroutines), so an operator stop can land
	// between their decision and this Post (or be queued ahead of it). Re-check the
	// stop intent HERE, on the loop, where the intent read and the drop are serialized
	// against the stop's own EvIntentUpdate — and DROP the restart if the task is now
	// stopped, so an automatic trigger can never resurrect a just-stopped daemon. Any
	// off-loop cleanup already done (F3 reaped the forgotten own-child; F1 reaped its
	// squatter) is correct; only the RESTART is gated. `mcphub daemon recover` posts
	// an UNFLAGGED EvManualRestart → unconditional (unaffected). Uses the SAME stopped
	// predicate as the F2 parole gate + the F3 off-loop stoppedFn.
	if ev.Kind == api.EvManualRestart && ev.Body != nil && c.daemonIntent != nil {
		if req, _ := ev.Body[autoRestartRequireRunningIntentBodyKey].(bool); req {
			di := c.daemonIntent.Lookup(ev.TaskName)
			activeStop, _ := di.IsActiveStop(time.Now().UTC())
			desired := di.Desired
			if desired == "" {
				desired = api.IntentDesiredRunning
			}
			if desired != api.IntentDesiredRunning || activeStop {
				// Clear any one-shot gateCleared the F1 worker's unverified arm set
				// alongside this flagged EvManualRestart: dropping the restart here
				// leaves the SM untouched (we return before the general non-StSpawning
				// clear), so an unconsumed gateCleared would linger and let the NEXT
				// respawn skip the port probe. Both key forms (P2-B class).
				c.gateCleared.Delete(ev.TaskName)
				if bareKey := toggleSupervisorTaskNameBackslash(ev.TaskName); bareKey != ev.TaskName {
					c.gateCleared.Delete(bareKey)
				}
				if c.events != nil {
					_ = c.events.Emit(api.SupervisorEvent{
						Severity: "info",
						Source:   "reconcile",
						Event:    "automatic-restart-skipped-stopped",
						TaskName: ev.TaskName,
						Body: map[string]any{
							"note": "automatic restart (F3 self-heal or F1 port-gate worker) skipped: the daemon is stopped (an operator stop landed after the off-loop decision); any reaped forgotten own-child / squatter is cleaned up but the daemon is NOT restarted",
						},
					})
				}
				return
			}
		}
	}
	// P1a generation-stamped exit attribution — authoritative processing-time
	// stale guard. An EvChildExit carrying a pid_generation OLDER than the
	// tracker's current generation for the task is a late cmd.Wait exit of a
	// SUPERSEDED child. Drop it BEFORE any bookkeeping:
	//   - before reaperOutstanding.Delete (below): the CURRENT child's reaper
	//     is still live; clearing its marker on a stale exit would re-open the
	//     Conc-F3 synthesize race;
	//   - before the clean-exit-at-StRunning drop (:~2519): a stale CLEAN exit
	//     would otherwise store smStates=StIdle while the current child is alive
	//     (a second latent instance of this bug class).
	// Judge staleness by GENERATION, not PID equality: terminate clears
	// CurrentPID to 0 (via MarkTerminated→MarkExited) BEFORE the deliberately-
	// killed child's real exit arrives, and StExiting's queued respawn NEEDS
	// that exit — a PID guard would deadlock it. The just-terminated current
	// child's exit carries gen == current (passes). Exits WITHOUT a
	// pid_generation (the pre-child synthetic self-post and
	// synthesizeForeignChildExit) carry gen <= 0 and pass through unchanged — they
	// are "about the tracked child now" by construction. The liveness sweep's
	// assume-dead EvChildExit (pid_dead / pid_identity_missing /
	// pid_identity_mismatch disown) NOW stamps its SWEEP-TIME pid_generation
	// (commission P1): it reads the tracker at sweep time and the post can land
	// AFTER a respawn bumped the generation, so this guard must drop the stale
	// disown rather than attribute it to the fresh child (which would clear the
	// live new PID → the lost-child this PR exists to kill).
	if ev.Kind == api.EvChildExit && c.tracker != nil && ev.Body != nil {
		if gen, ok := ev.Body["pid_generation"].(int); ok && gen > 0 {
			if entry, ok2 := c.tracker.Get(ev.TaskName); ok2 && gen < entry.PIDGeneration {
				if c.events != nil {
					smState, _ := c.GetSMState(ev.TaskName)
					stalePID, _ := ev.Body["pid"].(int)
					_ = c.events.Emit(api.SupervisorEvent{
						Severity: "info",
						Source:   "lifecycle",
						Event:    "daemon-stale-exit-ignored",
						TaskName: ev.TaskName,
						Body: map[string]any{
							"pid":                stalePID,
							"pid_generation":     gen,
							"current_generation": entry.PIDGeneration,
							"sm_state":           string(smState),
							"note":               "late child exit of a superseded generation; no SM transition, reaperOutstanding left intact for the current child",
						},
					})
				}
				return
			}
		}
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
	resolved, ok := c.lookupDescriptorWithShadow(ev.TaskName)
	if !ok {
		// Daemon dropped from intent OR not yet known (controller
		// initial state, or a stale event posted by the cmd.Wait
		// goroutine after the descriptor was removed). Audit-log
		// and drop.
		//
		// The reaping shadow (lookupDescriptorWithShadow) deliberately keeps
		// this branch from firing during the pendingReap window: a task whose
		// descriptor was removed but is awaiting reap confirmation still routes
		// its in-flight EvChildExit / EvTimerDue / queued-stop-completion events
		// through the SM, so a child that exits, a backoff timer that fires, or a
		// spawn that completes during the window all transition normally instead
		// of being orphan-dropped (the common-root fix). Only a genuinely-unknown
		// task (never marked pendingReap, fully cleared) reaches here.
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
		// P2-B (Codex PR-3): a task removed while a port-gate request was in flight
		// can have clearRemovedTaskRuntime run BEFORE the off-loop worker stored
		// gateCleared, leaving a stale one-shot bypass for the removed task. The
		// worker's now-orphan EvManualRestart is dropped HERE (descriptor not found),
		// so clear the stale flag on this same drop — a same-name re-registration
		// must not inherit it and skip its port probe. Both key forms, matching
		// clearRemovedTaskRuntime; a Delete on an absent key is a cheap no-op.
		c.gateCleared.Delete(ev.TaskName)
		if bareKey := toggleSupervisorTaskNameBackslash(ev.TaskName); bareKey != ev.TaskName {
			c.gateCleared.Delete(bareKey)
		}
		completeIdleRespawnEvent(ev, fmt.Errorf("task %s not found in controller intent cache", ev.TaskName))
		return
	}

	// pr302 r8 single-key-space invariant: route the SM against a COPY of the resolved
	// descriptor whose TaskName is the canonical routeKey. `resolved` points into the
	// shared intentCache snapshot slice (or the reapShadow) — mutating its TaskName in
	// place would corrupt the cache for every other reader — so copy first, then set the
	// canonical TaskName. The copy flows into api.Transition's SMContext lookups,
	// executeSideEffect, and handleBackoffWaiting, so EVERY d.TaskName-keyed map write
	// downstream lands in the canonical key space WITHOUT editing executeSideEffect. The
	// spawn/terminate path stays correct: the production spawn fn builds the child cmd
	// from d.Command + d.Args (the serena-proxy --task-name lives in d.Args, unchanged
	// by the copy), and the tracker / runningPIDs / overlay are already canonical-keyed.
	dCopy := *resolved
	dCopy.TaskName = routeKey
	d := &dCopy

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
	// Finding E: SUPPRESS the respawn a StBackoffWaiting daemon's backoff timer
	// would otherwise drive while the task is in the pendingReap window. A
	// backoff-waiting task has NO live child; spawning the OLD descriptor during
	// the verification window binds a port the user just removed (a transient
	// orphan / port-collision with a replacement daemon). We record that the
	// timer FIRED (reapDeferredTimerDue) and return WITHOUT transitioning, so the
	// task stays StBackoffWaiting. If the removal turns out to be a replace-in-
	// place BLIP, the reappear path REPLAYS this dropped EvTimerDue so the daemon
	// is not stranded (the anti-stranding invariant of the original finding 5);
	// if the removal is confirmed, the reap drives StBackoffWaiting +
	// EvIntentUpdate(stopped) -> StIdle ("cancel timer") and clears the marker.
	if ev.Kind == api.EvTimerDue && currentState == api.StBackoffWaiting {
		if _, pending := c.pendingReap.Load(ev.TaskName); pending {
			c.reapDeferredTimerDue.Store(ev.TaskName, true)
			c.emitReapEvent("debug", "orphan-reap-backoff-timer-deferred", ev.TaskName,
				"a removed StBackoffWaiting daemon's backoff timer fired during the reap window; suppressing the respawn (finding E) and remembering the timer so a replace-in-place reappear can replay it without stranding the daemon")
			return
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
		// FIX-5b: this StRunning→StIdle early-return also leaves continuous StRunning,
		// so reset the stabilize-dwell clock here too (no-op unless a dwell entry
		// exists). The main-transition reset below is skipped on this return path.
		c.noteReallocDwellLeftRunning(ev.TaskName)
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

	// Ephemeral-collision self-heal (L1): a bind-refused EvChildExit of a
	// dynamic-pool proxy (its pool port was stolen by a foreign process) is moved
	// to a fresh pool port off the loop instead of crash-looping to quarantine.
	// Runs BEFORE the SM transition; returns true when it fully handled the event
	// (reallocated → held in backoff, worker dispatched) so we skip the crash
	// path. It returns false — falling through to the normal crash/backoff/
	// quarantine SM below — for a non-bind-refused exit, a fixed-global daemon,
	// and a dynamic proxy whose reallocation cap is exhausted (all after emitting
	// their L3 event). The clean-exit guard above already returned for exit 0, so
	// a bind-refused (non-zero) exit reaches here.
	if c.maybeHandleBindRefusedExit(d, ev, currentState, now) {
		return
	}

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
	if currentState == api.StRunning && newState != api.StRunning {
		// FIX-5b: a daemon leaving continuous StRunning restarts its stabilize-dwell
		// clock HERE, on the transition — not only when a later dwell tick samples a
		// non-Running state. This closes the leave-and-reenter-between-ticks hole that
		// would otherwise let a flapping daemon accrue non-continuous dwell and clear
		// its reallocation/crash windows early. A no-op unless the task has a dwell
		// entry (i.e. it previously bind-failed).
		c.noteReallocDwellLeftRunning(ev.TaskName)
	}
	c.smStates.Store(ev.TaskName, newState)

	// P2-2 (Codex PR-3): drop a stale F1 gateCleared bypass. The flag is a one-shot
	// the port-gate worker sets for its IMMEDIATE StSpawning re-entry (unverified →
	// spawn-as-today); the ONLY consumer is the gate's LoadAndDelete on a transition
	// TO StSpawning. If the task instead settles/stops before that re-entry — a
	// queued stop/graceful drives StBackoffWaiting/StQuarantined → StIdle, or any
	// other matched transition that is NOT to StSpawning — the flag would linger and
	// the NEXT unrelated respawn would skip the port probe and spawn over whatever
	// now owns the port. Clear it on every non-StSpawning matched transition; the
	// legitimate StSpawning re-entry (newState==StSpawning) is left for the gate to
	// consume, so this never races that consumption. Delete on an absent key is a
	// cheap no-op.
	if newState != api.StSpawning {
		c.gateCleared.Delete(ev.TaskName)
	}

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
		// L1 self-heal: leaving quarantine (manual recover / parole / intent
		// re-enable) also resets the reallocation window and the terminal-event
		// dedupe marker, so a recovered daemon starts its next ephemeral-collision
		// episode with a fresh reallocation budget and can re-emit its L3 event.
		c.tracker.ClearReallocations(ev.TaskName)
		c.bindAccessDeniedTerminalEmitted.Delete(canonicalSupervisorTaskName(ev.TaskName))
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
			// Absorbing quarantine: drop any stale threshold parole entry so a
			// class-flip (threshold → nil-spec) is never paroled (commission P2-2).
			c.clearQuarantineParole(ev.TaskName)
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
		// F1 pre-spawn port-owner gate (decision D-A), loop half. Scoped to the
		// create-process paths where a lost child may squat the port. This is the
		// CONVERGENT scope (Codex PR-3 P2-i): the per-event allowlist kept missing
		// paths (round-1 missed EvIntentUpdate, round-3 missed EvStart), so gate EVERY
		// create-process transition and EXCLUDE only the ONE that spawns into a
		// provably-own/free port. The complete create-process set in api.Transition
		// (grep "create-process") is: EvStart (StIdle), EvIntentUpdate(running)
		// (StIdle/StBackoffWaiting/StQuarantined), EvManualRestart
		// (StIdle/StBackoffWaiting/StQuarantined), EvTimerDue (StBackoffWaiting), and
		// EvChildExit (StExiting — the queued respawn after a CONTROLLED restart). Only
		// EvChildExit-at-StExiting reclaims our OWN just-terminated child's port (the
		// terminate side effect already ran + cleared CurrentPID), so gating it would
		// just probe our own dying child; exclude it. Every other event either spawns
		// into a possibly-squatted port (EvStart cold start with stale supervisor
		// state, EvIntentUpdate re-enable, EvManualRestart/EvTimerDue respawn) — and a
		// genuinely-free port simply probes free → proceeds (fail-open). Structurally
		// covers any FUTURE create-process event.
		//
		// The loop runs ONLY the fast deadline-bounded owner probe — NEVER the WMI
		// classify or the terminate-wait (codex-P1: those blocking calls belong on
		// the off-loop worker). Two short-circuits precede the probe:
		//   1. gateCleared: the worker classified the owner UNVERIFIABLE (no kill)
		//      and re-posted EvManualRestart with this one-shot flag so the loop
		//      spawns "as today" WITHOUT re-probing (else the still-owned port would
		//      loop forever). LoadAndDelete consumes it so the NEXT respawn re-gates.
		//   2. preSpawnPortGateHold: probe the owner. Free / error / deadline →
		//      proceed. Owned → hold in backoff (no crash increment) + dispatch the
		//      classify+reap to the worker, and return the sentinel.
		if ev.Kind != api.EvChildExit {
			if _, cleared := c.gateCleared.LoadAndDelete(ev.TaskName); !cleared {
				if held := c.preSpawnPortGateHold(d, ev); held != nil {
					return held
				}
			}
		}
		err := c.spawn(*d)
		// FAIL-CLOSED job-protection refusal (ROADMAP §11.3). The production
		// spawn closure returns errSpawnJobProtectionRefused when
		// --strict-job-protection is set AND the per-spawn Job Object could
		// not be allocated: NO child was started (the gate refuses before
		// cmd.Start), so there is no own-spawned reaper to record and no
		// orphan to reap. A Job-create failure is a recurring host-policy
		// condition, so routing through StBackoffWaiting would churn backoff
		// forever; quarantine the SM state directly instead (mirrors the
		// legacy serena nil-spec quarantine arm above). The spawn closure
		// already marked the tracker Quarantined + persisted state.json, so
		// here we only align smStates and surface the audit row. Return the
		// sentinel so the idle-respawn completion path (handleLoopEvent) and
		// the reconcile caller see the spawn failed; no EvChildExit/EvHealthOK
		// self-post fires for this case (the switch below matches neither).
		if errors.Is(err, errSpawnJobProtectionRefused) {
			c.smStates.Store(ev.TaskName, api.StQuarantined)
			// Absorbing quarantine: drop any stale threshold parole entry so a
			// class-flip (a paroled threshold daemon whose respawn now hits the
			// fail-closed job-protection refusal) is never paroled again into a
			// permanent spawn-refuse loop (commission P2-2).
			c.clearQuarantineParole(ev.TaskName)
			if c.events != nil {
				_ = c.events.Emit(api.SupervisorEvent{
					Severity: "error",
					Source:   "lifecycle",
					Event:    "daemon-quarantined",
					TaskName: d.TaskName,
					Body: map[string]any{
						"reason":    "per-spawn Job Object allocation failed and --strict-job-protection is set; spawn refused (fail-closed) so the daemon is not started without orphan-protection",
						"workspace": d.Workspace,
					},
				})
			}
			return err
		}
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
		// This case runs for EVERY matched transition landing in StQuarantined:
		// the THRESHOLD entry (StBackoffWaiting + EvTimerDue at threshold, side
		// "clear timer") AND the absorbing SELF-LOOPS (StQuarantined +
		// EvIntentUpdate(stopped) / + EvRequestGraceful — supervisor_state_
		// machine.go). F2: mark parole-eligible ONLY on the EvTimerDue threshold
		// ENTRY. Recording on a self-loop would let an ABSORBING quarantine
		// (strict-job-protection / legacy-serena-nil-spec, which deliberately
		// record NO eligibility) leak into the parole ladder via a later
		// stopped/graceful self-loop and get paroled every cooldown forever
		// (commission P2-1). The SM sets persistBefore=true here so the state is
		// already mirrored; the audit row below always emits.
		if ev.Kind == api.EvTimerDue {
			c.recordQuarantineParoleEligible(d.TaskName, time.Now().UTC())
		}
		if c.events != nil {
			_ = c.events.Emit(api.SupervisorEvent{
				Severity: "error",
				Source:   "lifecycle",
				Event:    "daemon-quarantined",
				TaskName: d.TaskName,
				Body: map[string]any{
					"reason": c.quarantineReasonMessage(d.TaskName),
					// workspace lets a future GUI serena-session-cleanup consumer
					// key teardown by the dead daemon's workspace path (empty for
					// global daemons). Sourced from the descriptor in scope here.
					"workspace": d.Workspace,
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
		// The marker lookups go through loadReapMarkerCanonical so a legacy
		// bare-key daemon's markers (stored under the canonical key at spawn
		// time after the r8 handleLoopEvent canonicalization, but possibly under
		// the bare key on a pre-r8 remnant) are still seen when this guard is
		// reached via reapRemovedDaemon's terminate, which passes the RAW
		// captured descriptor (d.TaskName is BARE for a legacy bare-key intent
		// row). A strict Load(d.TaskName) here would miss the canonical-stored
		// own-spawned/reaper markers for a bare daemon, read owned=false +
		// reaperPending=false, and synthesize a spurious EvChildExit that races
		// the daemon's REAL cmd.Wait reaper exit — double-emit (Conc-F3). This
		// keeps the synthesize-guard in agreement with reapRemovedDaemon's own
		// canonical-aware owned/reaperPending check (1839-1840) on the same reap
		// path for a bare own-spawned daemon (finding 4 sweep, pr302 r9).
		owned := c.loadReapMarkerCanonical(&c.ownSpawned, d.TaskName)
		reaperPending := c.loadReapMarkerCanonical(&c.reaperOutstanding, d.TaskName)
		if termErr == nil && !owned && !reaperPending {
			c.synthesizeForeignChildExit(d, ev)
		}
		// Propagate the terminate result. Existing callers (handleLoopEvent's
		// operator-stop path) ignore it — the idleRespawn channel is nil there —
		// but the orphan-reap path (reapRemovedDaemon) reads it to distinguish a
		// CONFIRMED-dead terminate from a FAILED one so it can preserve the
		// SM/tracker entry for retry instead of losing the supervisor handle
		// (finding 2).
		return termErr

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
		// F2: mark this THRESHOLD quarantine parole-eligible (idempotent /
		// ladder-preserving) so the parole monitor auto-recovers it after a
		// bounded cooldown instead of requiring a supervisor restart.
		c.recordQuarantineParoleEligible(d.TaskName, now)
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
					"reason":          c.quarantineReasonMessage(d.TaskName),
					"exit_code":       exitCode,
					// workspace lets a future GUI serena-session-cleanup consumer
					// key teardown by the dead daemon's workspace path (empty for
					// global daemons). Sourced from the descriptor in scope here.
					"workspace": d.Workspace,
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

	c.armRespawnBackoffTimer(*d, d.TaskName, backoff)
}

// armRespawnBackoffTimer arms the cancel-respecting backoff timer that re-posts
// EvTimerDue after `backoff`. Extracted from handleBackoffWaiting so the F1
// pre-spawn port-owner gate (holdSpawnInBackoff) can re-arm the SAME timer
// mechanism when it holds a doomed spawn back WITHOUT recording a crash — one
// owner for "arm the respawn timer", no duplicated timer-goroutine logic.
func (c *supervisorController) armRespawnBackoffTimer(descriptor api.SupervisorDaemon, taskName string, backoff time.Duration) {
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

// errSpawnHeldPortSquatter is the sentinel executeSideEffect returns when the
// F1 pre-spawn gate holds a spawn back rather than firing it: the intended port
// is held (foreign / reap-failed / rate-limited) or a verified-own squatter was
// just reaped and the respawn is deferred one short cycle so the freed port
// settles. On the backoff path the error is dropped (the re-armed timer drives
// the retry); on the force-respawn IPC path it surfaces as RESPAWN_FAILED with
// this honest message (the daemon self-heals on the re-armed retry regardless).
var errSpawnHeldPortSquatter = errors.New("spawn deferred: intended port held by a process the pre-spawn gate would not spawn over (a respawn is scheduled)")

// autoRestartRequireRunningIntentBodyKey is the CONVERGED marker (Codex PR-3
// P2-ii) that EVERY automatic-trigger EvManualRestart carries so handleLoopEvent
// re-checks the stop intent ON THE LOOP before spawning — the single race-free
// gate. It is set by F3 quarantine self-heal (supervise_liveness.go) AND by the F1
// port-gate worker (both the reaped and unverified arms), whose stop checks would
// otherwise race an operator stop off-loop. The ONLY unflagged EvManualRestart is
// `mcphub daemon recover` (operator-initiated) → unconditional restart, as today.
// The rule is uniform: automatic ⟹ flagged ⟹ stop-gated; operator ⟹ unflagged ⟹
// unconditional. (F2 parole already re-checks the stop intent ON the loop before
// posting — runQuarantineParoleTick runs in the loop handler — so it is race-free
// without the flag; it also carries the flag now for one uniform rule.)
const autoRestartRequireRunningIntentBodyKey = "require_running_intent"

const (
	// squatterForeignHoldDelay is the backoff re-probe cadence the loop half of
	// the F1 gate uses whenever it hands a request to the worker. The daemon stays
	// in backoff WITHOUT a crash increment — a spawn doomed to EADDRINUSE against a
	// process we cannot displace is not a daemon crash, so it must not fuel the
	// quarantine march — and the armed 30s timer keeps re-probing. When the worker
	// reaps a verified-own squatter it accelerates recovery by posting
	// EvManualRestart immediately (rather than waiting for this timer); a foreign /
	// reap-failed / rate-limited owner just rides this 30s cadence. 30s also
	// matches the worker's identity-lookup rate limit so a persistent foreign
	// holder yields one foreign event per interval, not a log-washing storm.
	squatterForeignHoldDelay = 30 * time.Second
	// portGateProbeDeadline bounds the owner probe the LOOP runs (production wires
	// portOwnerFn to a LoopbackPortOwnerPIDContext closure with this deadline). A
	// wedged netstat is killed at the deadline and surfaces as a probe error →
	// fail-open (proceed to spawn), so the controller event loop can never hang on
	// the probe. It is deliberately short: the probe is only a "is anyone on this
	// port" check, and a slow answer is treated as "no verifiable squatter".
	portGateProbeDeadline = 2 * time.Second
	// portGateChCapacity buffers loop→worker dispatches so a burst of respawns
	// never blocks the loop on the send. A full channel drops the dispatch
	// (port-gate-dispatch-dropped) and the armed 30s timer re-probes + re-dispatches.
	portGateChCapacity = 64
)

// portGateReq is one loop→worker F1 request: the resolved descriptor copy (its
// Port is the EffectiveDaemonPort the loop probed) plus the owner PID the loop
// observed. The worker re-derives everything else it needs (a fresh tracker
// snapshot + selfPID) so nothing mutable is shared across the loop/worker
// boundary except this immutable value.
type portGateReq struct {
	d        api.SupervisorDaemon
	ownerPID int
}

// preSpawnPortGateHold is the LOOP half of the F1 pre-spawn gate. It runs ONLY
// the fast, deadline-bounded owner probe (portOwnerFn) — NEVER the identity
// classify (WMI) or the terminate-wait, which belong on the off-loop worker
// (codex-P1). It returns nil to let the caller PROCEED to spawn (gate disabled /
// no resolvable port / port free / probe error or deadline → fail-open, matches
// today), or the errSpawnHeldPortSquatter sentinel after HOLDING the daemon in
// backoff (no crash increment) and dispatching the classify+reap to the worker.
//
// The reaped/unverified/foreign decision is the worker's; the loop only decides
// "is anyone on the port". Everything the loop does here is O(one bounded probe).
func (c *supervisorController) preSpawnPortGateHold(d *api.SupervisorDaemon, ev api.LoopEvent) error {
	// Gate disabled unless BOTH the probe and the worker channel are wired
	// (production runSupervise), so a half-wired controller never holds a daemon
	// in backoff with no worker to reap it. Direct-construction tests leave these
	// nil → spawn as today.
	if c == nil || d == nil || c.portOwnerFn == nil || c.portGateCh == nil {
		return nil
	}
	port, ok := api.EffectiveDaemonPort(*d)
	if !ok || port <= 0 {
		return nil // no resolvable port → nothing to gate on
	}
	ownerPID, ownerOK, err := c.portOwnerFn(port)
	if err != nil || !ownerOK {
		return nil // probe error / deadline / port genuinely FREE (!ok) → spawn (fail-open, matches today)
	}
	// P2-4 (Codex PR-3): the FREE signal is `!ownerOK`, NOT ownerPID<=0. On Linux
	// the probe returns (0, true) when the loopback socket EXISTS but belongs to an
	// unreadable different-UID process (occupied-hidden) — that is OCCUPIED, and
	// spawning into it EADDRINUSEs. Do NOT treat it as free: hold + dispatch. The
	// worker classifies (a non-positive PID fails gate 1 → Unverified) and its
	// occupied-hidden arm HOLDS without a kill or a spawn (never spawn over a
	// cross-user-held port). (On Windows the netstat parser rejects PID 0, so a
	// non-positive ownerPID with ownerOK=true only occurs on Linux.)
	//
	// Port owned: hand the classify+reap to the worker (non-blocking) and hold the
	// daemon in backoff. Dispatch a copy carrying the resolved port so the worker's
	// audit body records the port F1 gated on (classifyPortSquatter ignores Port).
	dResolved := *d
	dResolved.Port = port
	c.tryDispatchPortGate(portGateReq{d: dResolved, ownerPID: ownerPID})
	return c.holdSpawnInBackoff(d, ev, squatterForeignHoldDelay)
}

// tryDispatchPortGate hands a port-gate request to the off-loop worker WITHOUT
// blocking the loop. A full channel means the single worker is saturated; drop
// the dispatch (the daemon is already held with an armed 30s timer that will
// re-probe + re-dispatch) and emit port-gate-dispatch-dropped so a saturated
// worker is operator-visible. Called only from the loop goroutine.
func (c *supervisorController) tryDispatchPortGate(req portGateReq) {
	if c == nil || c.portGateCh == nil {
		return
	}
	task := canonicalSupervisorTaskName(req.d.TaskName)
	// Per-task dedupe (Codex PR-3 round-2 P2): skip if a request for this task is
	// already queued/processing. The in-flight request + the daemon's armed 30s
	// timer (which re-probes and re-dispatches once this one completes) cover the
	// task, so a delayed worker cannot accumulate stale duplicates.
	if _, loaded := c.portGateInFlight.LoadOrStore(task, struct{}{}); loaded {
		return
	}
	select {
	case c.portGateCh <- req:
	default:
		// Send failed (worker saturated): un-mark so a later dispatch is not
		// permanently suppressed; the armed 30s timer re-probes + re-dispatches.
		c.portGateInFlight.Delete(task)
		if c.events != nil {
			_ = c.events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "restart-policy",
				Event:    "port-gate-dispatch-dropped",
				TaskName: task,
				Body: map[string]any{
					"port":   req.d.Port,
					"reason": "port-gate worker channel full; the daemon stays held in backoff and its armed 30s timer will re-probe and re-dispatch",
				},
			})
		}
	}
}

// runPortGateWorker is the OFF-LOOP half of the F1 gate: exactly ONE goroutine
// per controller (started in runSupervise's production block), so it is the SOLE
// owner of c.squatterLimiter and the limiter stays lock-free (the loop must
// never touch it). It drains portGateCh and, per request, runs the blocking
// identity classify + identity-gated reap — the WMI lookup + up-to-5s
// terminate-wait that MUST NOT run on the event loop. It maps the outcome back to
// the loop via an EvManualRestart post (bounded by ctx so a stopped loop cannot
// leak the worker). It holds NO lock and shares no mutable state with the loop
// beyond the sync.Map gateCleared (worker Store / loop LoadAndDelete).
func (c *supervisorController) runPortGateWorker(ctx context.Context) {
	if c == nil || c.portGateCh == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-c.portGateCh:
			c.handlePortGateReq(ctx, req)
		}
	}
}

// handlePortGateReq runs the classify+reap for one request and maps the outcome
// back to the loop. Concurrency: the reaped PID is always DISOWNED —
// classifyPortSquatter gate 1 excludes this task's own CurrentPID and gate 2
// excludes every other tracked task's CurrentPID/OrphanPID, and the loop HELD
// this task (it will not spawn while the worker reaps), so the worker never
// targets a live tracked child. PID reuse is closed inside TerminatePIDWithIdentity
// (held-handle exe+basename+start-time re-verify), untouched here.
func (c *supervisorController) handlePortGateReq(ctx context.Context, req portGateReq) {
	if c == nil {
		return
	}
	task := canonicalSupervisorTaskName(req.d.TaskName)
	// Clear the dedupe marker on the way out so the daemon's 30s timer can
	// re-dispatch if it is still held after this request settles.
	defer c.portGateInFlight.Delete(task)

	// Staleness guard (Codex PR-3 round-2 P2): a request can go stale between the
	// loop's dispatch and the single FIFO worker processing it — a PRIOR request
	// for this task may already have reaped the squatter and respawned the daemon.
	// Acting on the stale request would classify the now-gone owner PID as
	// Unverified → post EvManualRestart (restarting the healthy StRunning daemon:
	// StRunning+EvManualRestart = terminate+respawn) and leave a stale gateCleared
	// bypass. Re-probe the intended port: if the SAME owner PID no longer holds it
	// (reaped, freed, or replaced by the respawned child), the squatter this request
	// targeted is gone → drop (no reap, no post, no gateCleared). If the squatter is
	// still there, the daemon cannot have recovered on that port, so acting is
	// correct. The re-probe runs off-loop (worker goroutine), so it does not
	// reintroduce the loop-blocking the round-1 fix removed.
	if c.portOwnerFn != nil {
		if port, ok := api.EffectiveDaemonPort(req.d); ok && port > 0 {
			ownerNow, ownerOK, probeErr := c.portOwnerFn(port)
			if probeErr != nil || !ownerOK || ownerNow != req.ownerPID {
				if c.events != nil {
					_ = c.events.Emit(api.SupervisorEvent{
						Severity: "info",
						Source:   "restart-policy",
						Event:    "port-gate-stale-drop",
						TaskName: task,
						Body: map[string]any{
							"port":           port,
							"dispatched_pid": req.ownerPID,
							"reason":         "the port owner changed since dispatch (a prior request reaped it, the port freed, or the respawned child now owns it) — the targeted squatter is gone; dropping the stale request without a reap/restart",
							"probe_error":    probeErr != nil,
							"port_owner_now": ownerNow,
						},
					})
				}
				return
			}
		}
	}

	selfPID := 0
	if supervisorSelfPIDFn != nil {
		selfPID = supervisorSelfPIDFn()
	}
	var tracked map[string]DaemonRuntimeEntry
	if c.tracker != nil {
		tracked = c.tracker.Snapshot()
	}
	switch reapSquatterForAutomaticTrigger(req.d, req.ownerPID, selfPID, tracked, c.squatterLimiter, c.events, squatterSourcePreSpawn, time.Now().UTC()) {
	case squatterAutoReaped:
		// TerminatePIDWithIdentity waited for process exit, so the LISTEN socket
		// is already released. Re-enter the gate (WITHOUT gateCleared) so the loop
		// re-probes the now-free port and spawns. If, exceptionally, the port is
		// still held, the re-probe simply re-holds + re-dispatches — self-healing.
		// P2-ii: flag require_running_intent so handleLoopEvent drops this automatic
		// restart if an operator stop raced this off-loop reap.
		if c.eventLoop != nil {
			_ = c.eventLoop.PostCtx(ctx, api.LoopEvent{
				Kind:     api.EvManualRestart,
				TaskName: task,
				Body:     map[string]any{autoRestartRequireRunningIntentBodyKey: true},
			})
		}
	case squatterAutoUnverified:
		// P2-4 (Codex PR-3): distinguish occupied-hidden from same-user-unverifiable.
		// A non-positive dispatched owner PID (Linux (0,true): a different-UID process
		// holds the port with an unreadable PID) is definitely NOT our child and is
		// unkillable — treat it like a foreign holder: HOLD (post nothing, do NOT set
		// gateCleared) so the loop never spawns into a cross-user-held port
		// (→ EADDRINUSE). The armed 30s timer re-probes; when the holder releases, the
		// freed port spawns.
		if req.ownerPID <= 0 {
			return
		}
		// A real owner PID we could not classify (e.g. a same-user process we could
		// not OpenProcess — transient ACCESS_DENIED). Preserve today's
		// unverified→proceed: set the one-shot gateCleared flag then re-enter, so the
		// loop spawns "as today" WITHOUT re-probing the still-owned port (which would
		// loop forever). P2-ii: flag require_running_intent so a raced operator stop
		// drops the restart on the loop; the loop-side drop also clears the gateCleared
		// it set here, so a dropped restart leaves no stale probe-skip bypass.
		c.gateCleared.Store(task, struct{}{})
		if c.eventLoop != nil {
			_ = c.eventLoop.PostCtx(ctx, api.LoopEvent{
				Kind:     api.EvManualRestart,
				TaskName: task,
				Body:     map[string]any{autoRestartRequireRunningIntentBodyKey: true},
			})
		}
	default:
		// squatterAutoForeign / squatterAutoReapFailed / squatterAutoRateLimited:
		// post NOTHING. The daemon is already held in backoff and its armed 30s
		// timer owns the re-probe cadence (today's foreign hold, no crash increment).
	}
}

// holdSpawnInBackoff returns a spawning daemon to StBackoffWaiting and re-arms
// its respawn timer WITHOUT recording a crash, so the F1 gate can defer a spawn
// (foreign holder / just-reaped / rate-limited) while the doomed-spawn crash
// march never advances. It mirrors handleBackoffWaiting's tracker/timer wiring
// minus the RecordCrashAndCountInWindow increment. Returns the sentinel so the
// force-respawn IPC path reports the deferral (the backoff path drops it).
func (c *supervisorController) holdSpawnInBackoff(d *api.SupervisorDaemon, ev api.LoopEvent, rearm time.Duration) error {
	if c == nil || d == nil {
		return errSpawnHeldPortSquatter
	}
	c.smStates.Store(ev.TaskName, api.StBackoffWaiting)
	if c.tracker != nil {
		c.tracker.MarkBackoff(ev.TaskName)
		if c.statePath != "" {
			_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, ev.TaskName)
		}
	}
	c.armRespawnBackoffTimer(*d, d.TaskName, rearm)
	return errSpawnHeldPortSquatter
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

// quarantineReasonMessage is the single owner of the daemon-quarantined
// `reason` string, emitted from both the backoff-path threshold breach and the
// EvTimerDue-at-threshold arm (Step-8 consistency — no stale duplicate wording).
// It parametrizes the threshold + window from the controller's own config
// (un-hardcoding the old literal "10+" / "30-min") and NAMES the honest
// recovery lever: quarantine no longer implies a supervisor restart is the ONLY
// way out — `mcphub daemon recover <task>` (P2b) reaps any port squatter and
// forces a respawn through the existing force path.
func (c *supervisorController) quarantineReasonMessage(taskName string) string {
	return fmt.Sprintf(
		"%d+ failures in %s sliding window; automatic respawns suspended — run 'mcphub daemon recover %s' (or POST /api/daemon/respawn with force=true) to reap any port squatter and force a respawn; a supervisor restart also clears the window",
		c.quarantineThreshold, formatQuarantineWindow(c.failureWindow), taskName)
}

// formatQuarantineWindow renders the failure window as a compact human string
// ("30-min" for a whole-minute window, else the Duration's own String()).
func formatQuarantineWindow(d time.Duration) string {
	mins := d.Minutes()
	if mins == float64(int64(mins)) {
		return fmt.Sprintf("%d-min", int64(mins))
	}
	return d.String()
}

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
// crashCh is closed. loop.Post is a BLOCKING send onto the event loop's
// main channel (NewEventLoop(1024) in runSupervise): if that channel is
// momentarily full the bridge blocks here rather than losing the event,
// which is the load-bearing half of the never-drop-a-real-child-exit
// invariant (the other half is the per-child wait goroutine's BLOCKING
// send onto crashCh — audit P3, 2026-06-20). Back-pressure always
// drains because the loop goroutine never waits on a wait goroutine, so
// it keeps consuming loop.ch regardless of how many sends are queued.
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
			// P1a: carry the child's PID + tracker generation so the
			// controller's processing-time stale guard (handleLoopEvent) can
			// drop a late exit of a superseded child. These are in-process
			// ints (the loop is not JSON) so handleLoopEvent's int assertion
			// matches — same as the existing exit_code int assertion in
			// handleBackoffWaiting. pid_generation > 0 for every real spawned
			// child (MarkSpawned bumps from 0); the guard treats <= 0 (absent
			// / synthetic) as current, so gen-less exits pass through.
			body["pid"] = ev.PID
			body["pid_generation"] = ev.PIDGeneration
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

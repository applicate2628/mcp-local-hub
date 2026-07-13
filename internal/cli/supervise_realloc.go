package cli

import (
	"context"
	"errors"
	"time"

	"mcp-local-hub/internal/api"
)

// Ephemeral-collision self-heal (L1). When a DYNAMIC-pool proxy (serena /
// workspace-LSP) exits with exitBindRefused — its 127.0.0.1 pool port was stolen
// by a foreign process (a Windows OS ephemeral-range consumer, e.g. an AdGuard
// IPC socket) — the supervisor moves OUR daemon to a fresh pool port instead of
// crash-looping it to quarantine on the dead port. Fixed-global daemons (ports
// baked into gate-OFF client URLs) are NOT moved; they emit the L3 event and
// ride the existing crash path. See
// work-items/active/2026-07-13-daemon-port-ephemeral-self-heal/design.md.

const (
	// reallocationCap is the maximum number of bind-refused REALLOCATIONS a
	// single daemon may take within the crash window (c.failureWindow, 30 min).
	// A within-cap reallocation does NOT increment the crash `failures` counter;
	// the (cap+1)-th bind-refused exit STOPS reallocating and FALLS THROUGH to
	// normal crash counting, so the existing 10-in-30-min quarantine catches a
	// daemon whose port keeps getting stolen. Bounded flap: 3 base reallocations
	// + ≤10 exponential-backoff crashes → quarantine. Reliability-engineer
	// threshold (design §C).
	reallocationCap = 3
	// reallocationStabilizeDwell is how long a reallocated daemon must dwell
	// CONTINUOUSLY in StRunning before its reallocation window + crash window are
	// reset (treated as genuinely recovered). It is DWELL-gated, NOT bare
	// StRunning: a bind-refused daemon reaches StRunning (EvHealthOK = process-
	// start success) BEFORE it exits on the failed external bind, so a naive
	// StRunning reset would forever-clear the counter and the cap would never
	// engage (forever-flap). A daemon that actually keeps its listener bound for
	// this long is healthy; a bind-refused one exits within ~1s of StRunning and
	// never accrues the dwell. Mirrors quarantineParoleStabilizeDwell.
	reallocationStabilizeDwell = 60 * time.Second
	// reallocChCapacity buffers loop→worker reallocation dispatches (mirrors
	// portGateChCapacity). A full channel drops the dispatch; the daemon is
	// already held in backoff with an armed fallback timer that re-drives.
	reallocChCapacity = 64
)

// evReallocApplied is the controller-internal loop event the off-loop
// reallocation worker posts to hand its result back to the loop goroutine. Like
// evReapScan / evParoleTick it is an api.SMEvent VALUE (fits LoopEvent.Kind) but
// is NOT a row in api.Transition — handleLoopEvent intercepts it at the top and
// routes it to handleReallocApplied, where the intent-cache refresh + respawn (or
// the pool-exhausted quarantine) run serialized on the single loop goroutine.
const evReallocApplied api.SMEvent = "realloc-applied"

// Reallocation result body keys (carried on evReallocApplied).
const (
	// reallocResultOutcomeBodyKey carries a reallocOutcome.
	reallocResultOutcomeBodyKey = "realloc_outcome"
	// reallocResultNewPortBodyKey / reallocResultOldPortBodyKey carry the port
	// move (in-process ints; the loop is not JSON, so the int assertions match).
	reallocResultNewPortBodyKey = "realloc_new_port"
	reallocResultOldPortBodyKey = "realloc_old_port"
	// reallocResultAttemptBodyKey carries the 1-based reallocation attempt number
	// within the window (for the L3 event).
	reallocResultAttemptBodyKey = "realloc_attempt"
	// reallocResultErrBodyKey carries the error string for a failed reallocation.
	reallocResultErrBodyKey = "realloc_err"
	// reallocResultIntentBodyKey carries the fresh-from-disk supervisor-intent
	// snapshot the WORKER read OFF the loop (P1-1). The loop applies it to the
	// descriptor cache INLINE via handleReapScan — no disk read on the loop, and no
	// blocking self-Post from the handler. Absent on non-reallocated outcomes.
	reallocResultIntentBodyKey = "realloc_intent"
)

// reallocOutcome is the off-loop worker's verdict, applied on the loop.
type reallocOutcome int

const (
	// reallocOutcomeFailed is the zero value ON PURPOSE (P3): a malformed or absent
	// evReallocApplied outcome body decodes to Failed, so the loop re-arms the
	// fallback retry rather than mistaking a garbled body for a phantom
	// "reallocated" and respawning on an unchanged/unknown port.
	reallocOutcomeFailed        reallocOutcome = iota // any other error → arm a fallback retry (re-reallocate)
	reallocOutcomeReallocated                         // AllocatePort + re-persist succeeded; respawn on the new port
	reallocOutcomePoolExhausted                       // pool full → distinct quarantine (design §D)
)

// reallocReq is one loop→worker reallocation request: the resolved descriptor
// copy (its Port is the EffectiveDaemonPort the loop observed) plus the 1-based
// attempt number the interception recorded. Immutable value across the boundary.
type reallocReq struct {
	d       api.SupervisorDaemon
	attempt int
}

// reallocDwellEntry tracks the per-task stabilize-dwell clock (loop-owned).
type reallocDwellEntry struct {
	// healthySince is when the daemon was first observed continuously in
	// StRunning since its last reallocation; zeroed whenever it is observed
	// outside StRunning. Once now-healthySince >= reallocationStabilizeDwell the
	// reallocation + crash windows are reset and the entry is deleted.
	healthySince time.Time
}

// L3 event constants — the single canonical daemon-bind-access-denied event,
// action-discriminated (design §L3).
const bindAccessDeniedEvent = "daemon-bind-access-denied"

const (
	bindAccessDeniedActionReallocated   = "reallocated"
	bindAccessDeniedActionCapExhausted  = "quarantined-realloc-cap-exhausted"
	bindAccessDeniedActionPoolExhausted = "quarantined-pool-exhausted"
	bindAccessDeniedActionRunHostRemedy = "quarantined-run-host-remedy"
)

// bindAccessDeniedRemedy is the actionable operator remedy carried in every L3
// event body. It names the host-level fix (move the OS ephemeral range off the
// pool) and the manual per-daemon recovery, so an operator reading the event
// never has to reverse-engineer the cause.
const bindAccessDeniedRemedy = "a foreign process holds this loopback port because the OS TCP ephemeral (dynamic) range overlaps mcphub's pool; move the range off the pool with `mcphub setup --fix-ephemeral-range` (admin) or `netsh int ipv4 set dynamicport tcp start=<above-pool> num=<n>`, then `mcphub daemon recover <task>`"

// supervisorEventIsBindRefusedExit reports whether an EvChildExit carries the
// exitBindRefused code a daemon proxy returns when its loopback bind was refused.
// A missing/other exit_code is NOT a bind refusal (the normal crash path).
func supervisorEventIsBindRefusedExit(ev api.LoopEvent) bool {
	if ev.Kind != api.EvChildExit || ev.Body == nil {
		return false
	}
	code, ok := ev.Body["exit_code"].(int)
	return ok && code == exitBindRefused
}

// bindRefusedPoolLabel classifies a descriptor's pool for the L3 event `pool`
// field, from the argv shape ALONE (the same single-owner argv predicates the
// port-resolve + protect sides use). It never loads a manifest.
func bindRefusedPoolLabel(d api.SupervisorDaemon) string {
	switch {
	case api.IsSerenaProxyDescriptor(d):
		return "serena-dynamic"
	case api.IsWorkspaceLSPProxyDescriptor(d):
		return "lsp-manifest"
	default:
		return "global-fixed"
	}
}

// maybeHandleBindRefusedExit is the LOOP-side entry point of the L1 self-heal,
// called from handleLoopEvent AFTER the descriptor + currentState are resolved
// and BEFORE the SM transition. It returns true when it fully HANDLED the event
// (the caller must return without running api.Transition) — the reallocation
// case; false to FALL THROUGH to the normal SM crash/backoff/quarantine path —
// the not-bind-refused, cap-exhausted-proxy, and fixed-global cases (the latter
// two after emitting their terminal L3 event once).
func (c *supervisorController) maybeHandleBindRefusedExit(d *api.SupervisorDaemon, ev api.LoopEvent, currentState api.SMState, now time.Time) bool {
	if c == nil || d == nil {
		return false
	}
	// Only a genuine bind-refused crash FROM StRunning self-heals. During a
	// controller-driven exit (StExiting) or backoff a code-3 exit is not a fresh
	// bind attempt; let the normal path own it. StRunning is where a freshly-
	// spawned daemon lands (spawn-success EvHealthOK is priority-drained before
	// the real EvChildExit) right before its external bind fails.
	if currentState != api.StRunning || !supervisorEventIsBindRefusedExit(ev) {
		return false
	}

	if bindRefusedPoolLabel(*d) == "global-fixed" {
		// Fixed global daemon: its port is baked into gate-OFF client URLs, so it
		// is NEVER reallocated. Emit the run-host-remedy L3 event ONCE per episode
		// and fall through to the existing crash/backoff/quarantine path (which
		// crash-loops to quarantine at 10, as today). Start a stabilize-dwell watch
		// (P3) so the terminal-event dedupe marker (and the crash window) is cleared
		// once the daemon dwells stably in StRunning again — otherwise a fixed-global
		// daemon that recovers WITHOUT ever quarantining (the foreign holder releases
		// the port) never clears the marker (the SM "reset failures" clear only fires
		// on quarantine-leave), suppressing the NEXT episode's L3 event forever. The
		// dwell gate keeps a flapping daemon from clearing prematurely.
		c.startReallocDwell(d.TaskName)
		c.emitBindAccessDeniedTerminalOnce(d, bindAccessDeniedActionRunHostRemedy)
		return false
	}

	// Dynamic-pool proxy: reallocation candidate. PEEK the reallocation window
	// (does NOT record — FIX-6 records a cap slot only on a COMPLETED reallocation,
	// in handleReallocApplied) to decide the cap and the 1-based attempt number.
	reallocInWindow := 0
	if c.tracker != nil {
		reallocInWindow = c.tracker.ReallocationCountInWindow(ev.TaskName, now, c.failureWindow)
	}
	if reallocInWindow >= reallocationCap {
		// Cap exhausted: STOP reallocating. Emit the cap-exhausted L3 event ONCE
		// and fall through to normal crash counting so the daemon marches to the
		// existing 10-in-30-min quarantine on its last-reallocated port.
		c.emitBindAccessDeniedTerminalOnce(d, bindAccessDeniedActionCapExhausted)
		return false
	}

	// Within cap: reallocate OFF the loop. FIX-6: do NOT record a cap slot here. A
	// wedged worker's repeated backstop retries (deduped by the in-flight marker), a
	// dropped dispatch, and a transient Failed outcome would each otherwise burn a
	// slot WITHOUT any replacement allocation, marching a merely-stuck daemon to a
	// false quarantine. The slot is recorded ONLY when the worker posts a COMPLETED
	// (Reallocated) outcome (handleReallocApplied), so the cap counts genuine
	// completed port moves. The L3-event attempt number is PREDICTED as peek+1: the
	// same-task in-flight dedup plus loop serialization admit exactly one completed
	// record per peek, so the recorded count lands at this value. Start the
	// stabilize-dwell clock, HOLD the daemon in backoff (no crash increment),
	// dispatch the worker, and ALWAYS arm a long backstop fallback timer (P2-1).
	attempt := reallocInWindow + 1
	c.startReallocDwell(ev.TaskName)
	c.holdInBackoffNoTimer(ev.TaskName)
	c.tryDispatchRealloc(reallocReq{d: *d, attempt: attempt})
	// ALWAYS arm the backstop, even on a delivered dispatch (P2-1). The worker's
	// registry Lock() + MutateSupervisorIntentIfChanged flock are BLOCKING with no
	// deadline: a co-process wedged holding workspaces.yaml.lock (or the in-flight
	// dedupe marker held by an already-wedged worker) would leave this daemon parked
	// in StBackoffWaiting with NO armed respawn — and nothing else restarts a
	// StBackoffWaiting daemon, so it strands until a full supervisor restart. On the
	// happy path the worker's evReallocApplied re-arms the ~1s respawn (moving the SM
	// off StBackoffWaiting) long before this 30s backstop fires, so
	// armRespawnBackoffTimer's stale-state re-check drops the backstop as a harmless
	// no-op. On a wedge the backstop re-drives the retry on the OLD port (which
	// self-heals again, or falls through to the crash path once the cap is spent) so
	// the daemon is NEVER stranded.
	c.armRespawnBackoffTimer(*d, ev.TaskName, squatterForeignHoldDelay)
	return true
}

// holdInBackoffNoTimer parks a task in StBackoffWaiting + tracker backoff (crash-
// free) WITHOUT arming a respawn timer — the reallocation worker owns the respawn
// via evReallocApplied. Mirrors holdSpawnInBackoff minus the timer.
func (c *supervisorController) holdInBackoffNoTimer(taskName string) {
	if c == nil {
		return
	}
	c.smStates.Store(taskName, api.StBackoffWaiting)
	if c.tracker != nil {
		c.tracker.MarkBackoff(taskName)
		if c.statePath != "" {
			_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, taskName)
		}
	}
}

// tryDispatchRealloc hands a reallocation request to the off-loop worker without
// blocking the loop (mirror tryDispatchPortGate). It returns true when the request
// was delivered to a fresh worker OR a prior request for the same task is already
// in flight (its worker WILL post a result that covers this task); it returns false
// when the request could not be delivered (channel full / unwired). The caller
// ALWAYS arms a backstop fallback timer regardless of this return (P2-1), so a
// dropped dispatch never strands the daemon — the bool drives only the
// realloc-dispatch-dropped diagnostic below (and the FIX-6 record-on-completion
// counting: a dropped/deduped dispatch records no cap slot because only a COMPLETED
// reallocation outcome records one). Per-task dedupe. Called only on the loop.
func (c *supervisorController) tryDispatchRealloc(req reallocReq) bool {
	if c == nil || c.reallocCh == nil {
		return false
	}
	task := canonicalSupervisorTaskName(req.d.TaskName)
	if _, loaded := c.reallocInFlight.LoadOrStore(task, struct{}{}); loaded {
		return true // an in-flight worker will post an outcome that covers this task
	}
	select {
	case c.reallocCh <- req:
		return true
	default:
		c.reallocInFlight.Delete(task)
		if c.events != nil {
			_ = c.events.Emit(api.SupervisorEvent{
				Severity: api.SupervisorEventSeverityWarn,
				Source:   api.SupervisorEventSourceRestartPolicy,
				Event:    "realloc-dispatch-dropped",
				TaskName: task,
				Body: map[string]any{
					"reason": "reallocation worker channel full; the daemon stays held in backoff and an armed fallback timer will re-drive the retry",
				},
			})
		}
		return false
	}
}

// runReallocWorker is the OFF-LOOP half of the self-heal: exactly ONE goroutine
// per controller (started in runSupervise). It drains reallocCh and runs the
// blocking AllocatePort + atomic registry/intent re-persist (reallocFn) that MUST
// NOT run on the event loop, mapping the outcome back via evReallocApplied.
func (c *supervisorController) runReallocWorker(ctx context.Context) {
	if c == nil || c.reallocCh == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-c.reallocCh:
			c.handleReallocReq(ctx, req)
		}
	}
}

// handleReallocReq runs one reallocation off the loop and posts the outcome back.
func (c *supervisorController) handleReallocReq(ctx context.Context, req reallocReq) {
	if c == nil {
		return
	}
	task := canonicalSupervisorTaskName(req.d.TaskName)
	defer c.reallocInFlight.Delete(task)

	oldPort, _ := api.EffectiveDaemonPort(req.d)

	if c.reallocFn == nil {
		// Unwired (direct-construction tests): no-op. The daemon's armed fallback
		// timer re-drives the retry so it is never stranded.
		return
	}

	newPort, err := c.reallocFn(req.d)
	outcome := reallocOutcomeFailed
	switch {
	case err == nil:
		outcome = reallocOutcomeReallocated
	case errors.Is(err, api.ErrPortPoolExhausted):
		outcome = reallocOutcomePoolExhausted
	}

	// P1-1: on a successful reallocation, read the fresh supervisor-intent
	// snapshot HERE (off the loop) so the loop can apply it to the descriptor
	// cache INLINE — without a disk read on the loop and without a blocking
	// self-Post from the handler (the old on-loop reapIntentReader() +
	// refreshSupervisorIntent did BOTH: a loop-goroutine disk read AND
	// eventLoop.Post — a BLOCKING send to the loop's own main channel from inside a
	// handler, which self-deadlocks the whole supervisor when the buffer is full,
	// exactly this feature's storm scenario). A read miss leaves freshIntent nil;
	// the loop then skips the cache refresh, leaving the cache at the old port. The
	// backstop timer re-drives a respawn, and the ≤60s IntentWatcher refresh (which
	// re-reads intent from disk and swaps the cache to the reallocated port) is the
	// durable rescue that moves the respawn onto the new port — so a transient read
	// failure never permanently strands the daemon.
	var freshIntent *api.SupervisorIntentFile
	if outcome == reallocOutcomeReallocated && c.reapIntentReader != nil {
		if updated, rerr := c.reapIntentReader(); rerr == nil {
			freshIntent = updated
		}
	}

	// Emit the REALLOCATED L3 event from the WORKER (off-loop) so its blocking flock
	// Emit + the best-effort foreign-holder identity probe never touch the event
	// loop. The pool-exhausted TERMINAL event is deliberately NOT emitted here
	// (FIX-7): the worker cannot know whether the loop will actually quarantine — an
	// operator stop landing in the reallocation window drives the daemon to StIdle
	// and the StBackoffWaiting guard in handleReallocApplied then SKIPS the
	// quarantine. Emitting quarantined-pool-exhausted here would lie about a
	// quarantine that never happened, so it is emitted on the loop AFTER the guard
	// confirms the real outcome. Pool-exhausted carries foreignHolderPort=0 anyway
	// (no identity probe), so nothing blocking moves onto the loop.
	if outcome == reallocOutcomeReallocated {
		c.emitBindAccessDenied(req.d, bindAccessDeniedActionReallocated, oldPort, map[string]any{
			"old_port":             oldPort,
			"new_port":             newPort,
			"reallocation_attempt": req.attempt,
			"cap":                  reallocationCap,
		}, oldPort)
	}

	if c.eventLoop == nil {
		return
	}
	body := map[string]any{
		reallocResultOutcomeBodyKey: outcome,
		reallocResultOldPortBodyKey: oldPort,
		reallocResultNewPortBodyKey: newPort,
		reallocResultAttemptBodyKey: req.attempt,
	}
	if freshIntent != nil {
		body[reallocResultIntentBodyKey] = freshIntent
	}
	if err != nil {
		body[reallocResultErrBodyKey] = err.Error()
	}
	_ = c.eventLoop.PostCtx(ctx, api.LoopEvent{
		Kind:     evReallocApplied,
		TaskName: task,
		Body:     body,
	})
}

// handleReallocApplied applies the worker's outcome ON the loop goroutine
// (intercepted at the top of handleLoopEvent). It NEVER runs blocking I/O.
func (c *supervisorController) handleReallocApplied(ev api.LoopEvent) {
	if c == nil || ev.Body == nil {
		return
	}
	outcome, _ := ev.Body[reallocResultOutcomeBodyKey].(reallocOutcome)
	task := canonicalSupervisorTaskName(ev.TaskName)

	switch outcome {
	case reallocOutcomeReallocated:
		newPort, _ := ev.Body[reallocResultNewPortBodyKey].(int)

		// FIX-6: record the reallocation cap slot HERE — a reallocation counts against
		// the cap ONLY when the worker actually COMPLETED a port move. A dropped or
		// in-flight-deduped dispatch and a transient Failed outcome never reach this
		// branch, so a wedged worker's repeated backstop retries can no longer burn cap
		// slots without a replacement allocation and march a merely-stuck daemon to a
		// false quarantine. This records into the SEPARATE reallocation window only;
		// the crash `failures` window that drives quarantine stays untouched.
		if c.tracker != nil {
			c.tracker.RecordReallocationAndCountInWindow(task, time.Now().UTC(), c.failureWindow)
		}

		// Move the descriptor cache onto the reallocated port so the respawn below
		// resolves argv=newPort. INVARIANT: the port MUST always land on newPort (the
		// realloc's authoritative move) — ONLY reallocSnapshotIncomingNewer does the
		// whole-snapshot apply; EVERY other order (Stale/Equal/Unorderable) does the
		// targeted patchCachedReallocatedPort. Three dispositions (FIX-3b + FIX-4b):
		//   - INCOMING NEWER: the worker carried a fresh whole-intent snapshot that is
		//     DEFINITELY newer than the cache → apply it wholesale via handleReapScan
		//     (the P1-1 inline apply: no loop-goroutine disk read, no blocking self-Post
		//     — the old reapIntentReader()+refreshSupervisorIntent did both and
		//     self-deadlocked the loop under a full buffer, exactly this feature's
		//     storm). We ARE the loop goroutine, so the cache swap completes here,
		//     before the respawn is armed.
		//   - INCOMING STALE (older-OR-EQUAL — the COMMON path): SKIP the WHOLE-snapshot
		//     apply so a genuinely-newer operator/reconciler intent is not clobbered (the
		//     IntentWatcher may have swapped one in during the worker's off-loop window —
		//     the ~5s foreign-holder L3 emit sits BETWEEN the off-loop read and the
		//     PostCtx, so an evReapScan(newer) can be posted AHEAD of this
		//     evReallocApplied). FIX-4b treats an EQUAL UpdatedAt as stale, and the
		//     PRODUCTION-NORMAL timeline IS equal: ReallocateDynamicPoolPort's step-4
		//     intent write does not stamp UpdatedAt, so the worker's carried snapshot
		//     shares the cache's timestamp and nothing has yet moved the cache descriptor
		//     to newPort. So we STILL patch JUST this descriptor's port onto the current
		//     cache (the FIX-3b targeted patch, idempotent — a no-op when the cache
		//     already holds newPort, the genuine operator-raced case where a newer intent
		//     already carried the move) so the respawn lands on newPort instead of the
		//     OLD stolen port. The patch touches only this descriptor's port + --port
		//     argv + serena RuntimeSpec, so it can never clobber a newer update to a
		//     DIFFERENT descriptor.
		//   - UNORDERABLE: no carried snapshot (worker disk read MISS) OR an unparseable
		//     UpdatedAt on either side (FIX-4b no longer fail-opens a parse hiccup into a
		//     blind whole-snapshot apply). We cannot trust a wholesale swap, so patch
		//     JUST this descriptor's port in the CURRENT cache to the newPort the worker
		//     returned (FIX-3b targeted patch). This removes the read-miss dependence on
		//     the ≤60s IntentWatcher — the watcher is now a backstop, not the primary
		//     rescue.
		if c.intentCache != nil {
			carried, _ := ev.Body[reallocResultIntentBodyKey].(*api.SupervisorIntentFile)
			switch reallocSnapshotOrder(c.intentCache.CurrentIntent(), carried) {
			case reallocSnapshotIncomingNewer:
				c.handleReapScan(c.intentCache.TaskNames(), carried, nil)
			case reallocSnapshotIncomingStale:
				// Older-OR-EQUAL (the common path, since step-4 leaves UpdatedAt
				// unchanged): DO NOT wholesale-apply the carried snapshot (that could
				// clobber a genuinely-newer operator/reconciler intent), but STILL patch
				// this descriptor's port onto the current cache so the respawn lands on
				// newPort, not the OLD stolen port. The patch is idempotent (a no-op when
				// the cache already holds newPort — the genuine operator-raced case where
				// a newer intent already carried the move) and touches only this
				// descriptor, so it never clobbers a newer update to a DIFFERENT one.
				c.patchCachedReallocatedPort(task, newPort)
				if c.events != nil {
					_ = c.events.Emit(api.SupervisorEvent{
						Severity: api.SupervisorEventSeverityInfo,
						Source:   api.SupervisorEventSourceRestartPolicy,
						Event:    "realloc-stale-snapshot-skipped",
						TaskName: task,
						Body: map[string]any{
							"note": "the reallocation worker's intent snapshot was older-or-equal to the intent already applied to the cache (an operator/reconciler update landed during the worker's off-loop window, OR step-4's intent write left UpdatedAt unchanged so the carried snapshot shares the cache timestamp); skipped the WHOLE-snapshot refresh to avoid clobbering a possibly-newer intent, and patched just this descriptor's reallocated port onto the current cache so the respawn lands on the new port",
						},
					})
				}
			default: // reallocSnapshotUnorderable — read miss or unparseable timestamps
				c.patchCachedReallocatedPort(task, newPort)
			}
		}
		// Only re-arm the respawn if the daemon is still held in backoff. An
		// operator stop landing in the reallocation window already drove it to
		// StIdle; the EvTimerDue would then no-op, but skipping the arm avoids a
		// pointless timer. The EvTimerDue itself re-checks the stop intent in the
		// SM (StBackoffWaiting+EvTimerDue drops to StIdle on stopped intent), so a
		// raced stop is also caught there.
		if c.smStateIs(task, api.StBackoffWaiting) {
			if d, ok := c.lookupDescriptorWithShadow(task); ok && d != nil {
				c.armRespawnBackoffTimer(*d, task, respawnBackoffStep)
			}
		}
	case reallocOutcomePoolExhausted:
		// Pool exhausted: quarantine with a DISTINCT reason (design §D),
		// parole-eligible on the F2 ladder like a threshold quarantine. GUARD (P2-3):
		// only quarantine if the daemon is STILL held in the reallocation backoff. An
		// operator stop landing in the reallocation window already drove it to StIdle;
		// stamping StQuarantined unconditionally here would OVERWRITE that stop (the
		// sibling reallocated + failed branches already carry this guard).
		if !c.smStateIs(task, api.StBackoffWaiting) {
			return
		}
		c.smStates.Store(task, api.StQuarantined)
		c.recordQuarantineParoleEligible(task, time.Now().UTC())
		if c.tracker != nil {
			c.tracker.MarkQuarantined(task)
			if c.statePath != "" {
				_ = persistDaemonRuntimeTracker(c.events, c.tracker, c.statePath, task)
			}
		}
		errStr, _ := ev.Body[reallocResultErrBodyKey].(string)
		// FIX-7: emit the quarantined-pool-exhausted TERMINAL L3 event HERE, AFTER the
		// StBackoffWaiting guard confirmed the daemon was ACTUALLY quarantined. The
		// worker no longer emits it pre-decision — it could not know an operator stop
		// would race the guard and skip the quarantine, so a pre-decision emit could
		// claim a quarantine that never happened. Reconstruct it on the loop from the
		// event body (old port + attempt + allocator diagnostic); foreignHolderPort=0
		// keeps the (blocking) identity probe off the loop, exactly as the worker's
		// prior pool-exhausted emit did.
		if d, ok := c.lookupDescriptorWithShadow(task); ok && d != nil {
			oldPort, _ := ev.Body[reallocResultOldPortBodyKey].(int)
			attempt, _ := ev.Body[reallocResultAttemptBodyKey].(int)
			c.emitBindAccessDenied(*d, bindAccessDeniedActionPoolExhausted, oldPort, map[string]any{
				"reallocation_attempt": attempt,
				"cap":                  reallocationCap,
				"allocator_error":      errStr,
			}, 0)
		}
		if c.events != nil {
			_ = c.events.Emit(api.SupervisorEvent{
				Severity: api.SupervisorEventSeverityError,
				Source:   api.SupervisorEventSourceLifecycle,
				Event:    "daemon-quarantined",
				TaskName: task,
				Body: map[string]any{
					"reason": "port pool exhausted during ephemeral-collision self-heal; no free pool port to move to",
					"detail": errStr,
				},
			})
		}
	default: // reallocOutcomeFailed
		// A transient reallocation failure (e.g. registry/intent write error).
		// Do NOT crash-count it — re-arm the fallback hold so the daemon retries
		// the OLD port, which will re-drive the self-heal (or fall through to the
		// crash path once the underlying failure clears).
		if c.smStateIs(task, api.StBackoffWaiting) {
			if d, ok := c.lookupDescriptorWithShadow(task); ok && d != nil {
				c.armRespawnBackoffTimer(*d, task, squatterForeignHoldDelay)
			}
		}
	}
}

// reallocSnapshotDisposition classifies the reallocation worker's carried intent
// snapshot (`incoming`) against the intent already applied to the cache (`current`),
// deciding how handleReallocApplied moves the cache onto the reallocated port.
type reallocSnapshotDisposition int

const (
	// reallocSnapshotUnorderable is the ZERO value on purpose: an absent snapshot
	// (worker disk read miss) or an unparseable/missing UpdatedAt on either side —
	// cases where the two cannot be chronologically ordered. The loop then falls back
	// to the FIX-3b targeted single-descriptor cache patch (patchCachedReallocatedPort)
	// rather than a blind whole-snapshot apply that could clobber a newer intent.
	reallocSnapshotUnorderable reallocSnapshotDisposition = iota
	// reallocSnapshotIncomingNewer: the carried snapshot is strictly newer than the
	// cache — apply it wholesale.
	reallocSnapshotIncomingNewer
	// reallocSnapshotIncomingStale: the carried snapshot is older-OR-EQUAL to the
	// cache — the handler skips the WHOLE-snapshot apply (so a newer operator/reconciler
	// intent is not clobbered) but STILL targeted-patches this descriptor's reallocated
	// port onto the current cache. EQUAL is the common path: step-4's intent write does
	// not stamp UpdatedAt, so the worker's carried snapshot shares the cache timestamp.
	reallocSnapshotIncomingStale
)

// reallocSnapshotOrder (FIX-4) orders the worker's carried intent snapshot against
// the intent already applied to the descriptor cache. Applying an older snapshot
// would clobber a newer operator/reconciler intent the IntentWatcher swapped in
// during the worker's off-loop window. Timestamps are compared as PARSED time.Time,
// not raw strings (RFC3339Nano string compare is NOT chronological across variable
// fractional digits — ".5Z" sorts after ".55Z"). FIX-4b hardens the prior
// strictly-After-only test: an EQUAL UpdatedAt now counts as STALE (flock serializes
// writes but does not make wall-clock timestamps strictly ordered — two writes can
// share a timestamp), and a parse failure / absent snapshot is UNORDERABLE (the
// caller does a targeted port patch) instead of the prior fail-OPEN blind apply.
func reallocSnapshotOrder(current, incoming *api.SupervisorIntentFile) reallocSnapshotDisposition {
	if incoming == nil {
		return reallocSnapshotUnorderable // no carried snapshot (worker read miss)
	}
	if current == nil {
		return reallocSnapshotIncomingNewer // empty cache: the carried snapshot is the freshest we have
	}
	curT, err1 := time.Parse(time.RFC3339Nano, current.UpdatedAt)
	incT, err2 := time.Parse(time.RFC3339Nano, incoming.UpdatedAt)
	if err1 != nil || err2 != nil {
		return reallocSnapshotUnorderable // cannot order (parse failure) → targeted patch, not a blind apply
	}
	if incT.After(curT) {
		return reallocSnapshotIncomingNewer
	}
	return reallocSnapshotIncomingStale // incoming <= current (FIX-4b: equal counts as stale)
}

// patchCachedReallocatedPort applies the FIX-3b targeted cache patch: it clones the
// CURRENT cache intent with JUST this descriptor's port moved to newPort (via the
// api-owned CloneIntentWithReallocatedPort — the single owner of a dynamic-pool port
// move, shared with ReallocateDynamicPoolPort's step-4 write) and swaps it into the
// cache through the SAME on-loop handleReapScan apply. Used when the reallocation
// worker could not carry a trustworthy whole-intent snapshot (disk read miss, or
// unorderable timestamps), so the respawn still resolves argv=newPort WITHOUT waiting
// for the ≤60s IntentWatcher. A no-op when the clone cannot be built (nil cache,
// absent descriptor, no --port argv, or newPort<=0): the daemon then rides the
// backstop + IntentWatcher as before. Called only on the loop.
func (c *supervisorController) patchCachedReallocatedPort(task string, newPort int) {
	if c == nil || c.intentCache == nil || newPort <= 0 {
		return
	}
	patched, ok := api.CloneIntentWithReallocatedPort(c.intentCache.CurrentIntent(), task, newPort)
	if !ok || patched == nil {
		return
	}
	c.handleReapScan(c.intentCache.TaskNames(), patched, nil)
}

// startReallocDwell (re)starts the per-task stabilize-dwell watch at the START of a
// bind-fail / reallocation episode — the moment the daemon has just LEFT continuous
// StRunning (its external bind failed). It creates the entry if absent AND resets
// healthySince so any partial dwell accrued from a PRIOR StRunning stretch is
// discarded: only a genuinely continuous StRunning dwell may reset the reallocation +
// crash windows (FIX-5b — the flapping-daemon gate). Both callers (the reallocation
// and fixed-global bind-fail paths) invoke it exactly at such a departure. Called on
// the loop, where reallocDwell entries are single-writer.
func (c *supervisorController) startReallocDwell(taskName string) {
	if c == nil {
		return
	}
	actual, _ := c.reallocDwell.LoadOrStore(canonicalSupervisorTaskName(taskName), &reallocDwellEntry{})
	if entry, ok := actual.(*reallocDwellEntry); ok && entry != nil {
		entry.healthySince = time.Time{}
	}
}

// noteReallocDwellLeftRunning resets the stabilize-dwell clock for a task that has
// just transitioned OUT of StRunning (FIX-5b). Resetting on the SM transition — not
// only when a periodic dwell tick happens to SAMPLE a non-Running state — closes the
// leave-and-reenter-between-ticks hole: a daemon that drops out of StRunning and
// returns before the next tick would otherwise accrue NON-continuous dwell and
// prematurely clear the reallocation / crash windows of a flapping daemon. A no-op
// when no dwell entry exists (the common case: the task never bind-failed). Called on
// the loop, where reallocDwell entries are single-writer.
func (c *supervisorController) noteReallocDwellLeftRunning(taskName string) {
	if c == nil {
		return
	}
	if v, ok := c.reallocDwell.Load(canonicalSupervisorTaskName(taskName)); ok {
		if entry, ok := v.(*reallocDwellEntry); ok && entry != nil {
			entry.healthySince = time.Time{}
		}
	}
}

// runReallocDwellTick evaluates every reallocation-dwell entry once, resetting
// the reallocation + crash windows for any daemon that has dwelt continuously in
// StRunning past reallocationStabilizeDwell (genuinely recovered). `now` is
// injected so tests drive the dwell deterministically. Called from the
// evParoleTick handler (same cadence + loop goroutine as the parole tick).
func (c *supervisorController) runReallocDwellTick(now time.Time) {
	if c == nil {
		return
	}
	c.reallocDwell.Range(func(k, v any) bool {
		task, _ := k.(string)
		entry, _ := v.(*reallocDwellEntry)
		if entry == nil {
			c.reallocDwell.Delete(k)
			return true
		}
		state, ok := c.getSMStateCanonical(task)
		if !ok {
			// Task no longer tracked (removed from intent). Drop the entry.
			c.reallocDwell.Delete(k)
			c.bindAccessDeniedTerminalEmitted.Delete(k)
			return true
		}
		if state != api.StRunning {
			// Not (yet) healthy — reset the dwell clock so a later StRunning must
			// accrue the FULL dwell afresh. This is the dwell-GATE: a bind-refused
			// daemon transiting StRunning briefly never reaches the threshold.
			entry.healthySince = time.Time{}
			return true
		}
		if entry.healthySince.IsZero() {
			entry.healthySince = now
		}
		if now.Sub(entry.healthySince) >= reallocationStabilizeDwell {
			// Genuinely recovered: reset BOTH windows so the next
			// ephemeral-collision episode starts with a fresh budget, drop the
			// terminal-event dedupe marker, and stop watching.
			if c.tracker != nil {
				c.tracker.ClearReallocations(task)
				c.tracker.ClearCrashes(task)
			}
			c.bindAccessDeniedTerminalEmitted.Delete(k)
			c.reallocDwell.Delete(k)
			if c.events != nil {
				_ = c.events.Emit(api.SupervisorEvent{
					Severity: api.SupervisorEventSeverityInfo,
					Source:   api.SupervisorEventSourceRestartPolicy,
					Event:    "daemon-realloc-stabilized",
					TaskName: task,
					Body: map[string]any{
						"note": "bind-refused daemon dwelt stably in Running past the stabilize dwell; reallocation + crash windows reset and the terminal-event dedupe marker cleared (a dynamic-pool daemon after reallocation, or a fixed-global daemon whose port freed)",
					},
				})
			}
		}
		return true
	})
}

// smStateIs reports whether the task's current SM state equals want.
func (c *supervisorController) smStateIs(taskName string, want api.SMState) bool {
	if c == nil {
		return false
	}
	s, ok := c.getSMStateCanonical(canonicalSupervisorTaskName(taskName))
	return ok && s == want
}

// emitBindAccessDeniedTerminalOnce emits a terminal (give-up) L3 event
// (cap-exhausted / run-host-remedy) at most once per episode. The per-task marker
// is cleared when the daemon stabilizes (dwell reset) or leaves quarantine
// (ClearCrashes site), so a fresh episode re-emits. Called on the loop; no
// foreign-holder probe (that blocking work belongs off-loop).
func (c *supervisorController) emitBindAccessDeniedTerminalOnce(d *api.SupervisorDaemon, action string) {
	if c == nil || d == nil {
		return
	}
	task := canonicalSupervisorTaskName(d.TaskName)
	if _, loaded := c.bindAccessDeniedTerminalEmitted.LoadOrStore(task, struct{}{}); loaded {
		return
	}
	port, _ := api.EffectiveDaemonPort(*d)
	// FIX-1 (NEW-1): pass foreignHolderPort=0, NOT `port`. This is the ON-LOOP
	// terminal emit; a >0 foreignHolderPort would fire reallocForeignHolderFn
	// (netstat ~2s + WMI/PowerShell identity ~3s) ON the event loop goroutine,
	// contradicting this fn's own "no foreign-holder probe" contract above and the
	// resolver's "off-loop worker only" contract. The foreign-holder PID/basename is
	// a nice-to-have in a give-up event, not worth stalling child-exit/IPC
	// processing for every fixed-global storm victim; the reallocated-outcome event
	// (emitted off-loop by the worker) still carries it.
	c.emitBindAccessDenied(*d, action, port, nil, 0)
}

// emitBindAccessDenied writes the single canonical daemon-bind-access-denied L3
// event. severity=warn for `reallocated`, error for any `quarantined-*` action.
// port is the descriptor's current/old port; foreignHolderPort (>0) triggers the
// best-effort REDACTED foreign_holder resolution (PID + basename ONLY). extra
// carries action-specific fields (new_port, reallocation_attempt, cap, ...).
func (c *supervisorController) emitBindAccessDenied(d api.SupervisorDaemon, action string, port int, extra map[string]any, foreignHolderPort int) {
	if c == nil || c.events == nil {
		return
	}
	severity := api.SupervisorEventSeverityError
	if action == bindAccessDeniedActionReallocated {
		severity = api.SupervisorEventSeverityWarn
	}
	body := map[string]any{
		"port":   port,
		"pool":   bindRefusedPoolLabel(d),
		"action": action,
		"remedy": bindAccessDeniedRemedy,
	}
	for k, v := range extra {
		body[k] = v
	}
	if c.ephemeralRangeContainsFn != nil && port > 0 {
		if inRange, known := c.ephemeralRangeContainsFn(port); known {
			body["inside_ephemeral_range"] = inRange
		}
	}
	// REDACTED foreign_holder: PID + basename ONLY. Never the command line /
	// executable path / env — those can carry attacker-controlled bytes or
	// secrets. Best-effort: omitted when the resolver is unwired or the holder is
	// gone (pid<=0).
	if foreignHolderPort > 0 && c.reallocForeignHolderFn != nil {
		if pid, basename := c.reallocForeignHolderFn(foreignHolderPort); pid > 0 {
			body["foreign_holder"] = map[string]any{
				"pid":      pid,
				"basename": basename,
			}
		}
	}
	_ = c.events.Emit(api.SupervisorEvent{
		Severity: severity,
		Source:   api.SupervisorEventSourceRestartPolicy,
		Event:    bindAccessDeniedEvent,
		TaskName: canonicalSupervisorTaskName(d.TaskName),
		Body:     body,
	})
}

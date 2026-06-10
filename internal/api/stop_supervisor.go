package api

// Phase A.1 (spec §4, docs/superpowers/specs/2026-06-10-clean-architecture-
// redesign.md) — make Stop/StopAll supervisor-aware. stopSupervisorOwnedDaemons
// is the stop-side mirror of restartSupervisorOwnedDaemons: instead of a
// per-target IPC respawn it issues ONE `reconcile --apply`, because the
// caller has already written Desired=stopped into the supervisor-intent.json
// `stops` sub-block (Phase 4-E2: the sole stop source; recordStopIntent runs
// BEFORE this pass); the supervisor re-reads the
// fresh intent, posts EvIntentUpdate per drifted task, and the SM drives
// StRunning→StExiting→StIdle — a deliberate stop the reaper does NOT
// respawn. The legacy alternative (killDaemonByPort = taskkill /F /T) is a
// NON-clean exit the supervisor reaper observes and respawns, which is the
// stop→respawn churn that quarantined live daemons.

import (
	"context"
	"errors"
	"strings"
)

// supervisorDispatchRowForTarget builds the per-target RestartResult row
// for a target whose stop/start intent was just dispatched through ONE
// `reconcile --apply`. It is the HONESTY seam shared by both the stop
// path (stopSupervisorOwnedDaemons) and the refused-restart branch
// (restartSupervisorOwnedDaemons): the transport-level reconcile call
// succeeding does NOT mean THIS target's EvIntentUpdate was posted.
//
// Under the no-legacy ownership model (spec §0.2) EVERY supervisor-intent
// row is supervisor-owned, so the supervisor's drift classifier posts
// EvIntentUpdate for a live regular global daemon (`daemon --server X
// --daemon Y`) exactly as it does for a proxy descriptor — that is the
// synchronous-dispatch case. The only entries that DON'T post are the
// edges: an already-idle/settled daemon whose terminate classifies no_op,
// or a target with no drift entry at all. So we look up THIS target's
// drift entry by task name (normalized leading-backslash on both sides)
// and:
//
//   - Action == post_ev_intent_update → plain success row (empty Err,
//     empty Code): the SM was actually dispatched for this target.
//   - any other Action (no_op, needs_manual_review) OR a missing drift
//     entry → success-but-deferred row: empty Err (the intent is durable
//     on disk) + Code = DeferredToIntentWatcherCode, so the row is not a
//     failure but also does not falsely claim synchronous SM dispatch.
//
// A truly-dispatched row carries no Code: both the stop and the
// refused-restart call sites dispatch through the SAME `reconcile --apply`,
// so neither has a per-target provenance code to attach to the dispatched
// row (the restart path's RESPAWN_REFUSED_INTENT_STOPPED code lives only on
// its OWN refusal/error rows, not on the reconcile-dispatched success row).
func supervisorDispatchRowForTarget(taskName string, drift []DriftEntry) RestartResult {
	if entry, ok := findDriftEntryForTask(taskName, drift); ok &&
		entry.Action == ReconcileActionPostEvIntentUpdate {
		// Truly dispatched through the SM this reconcile.
		return RestartResult{TaskName: taskName}
	}
	// Not posted for this target (terminate classifies no_op /
	// needs_manual_review, or no drift entry at all): durable on disk,
	// IntentWatcher converges within ~60s. Surface that honestly via the
	// typed Code; Err stays empty.
	return RestartResult{TaskName: taskName, Code: DeferredToIntentWatcherCode}
}

// findDriftEntryForTask returns the drift entry whose TaskName matches
// the target, comparing on the bare (leading-backslash-stripped) form so
// the api-side canonical name and the cli-side canonicalTaskNameForReconcile
// name compare equal regardless of which side added the prefix.
func findDriftEntryForTask(taskName string, drift []DriftEntry) (DriftEntry, bool) {
	want := strings.TrimPrefix(taskName, `\`)
	for _, entry := range drift {
		if strings.TrimPrefix(entry.TaskName, `\`) == want {
			return entry, true
		}
	}
	return DriftEntry{}, false
}

// supervisorReconcileApplyFunc is the side-neutral seam for dialing the
// supervisor IPC `reconcile --apply` verb. It is shared by BOTH the stop
// path (stopSupervisorOwnedDaemons, this file) and the restart path
// (restartSupervisorOwnedDaemons, restart_supervisor.go): both dispatch a
// drifted intent through the supervisor's state machine by re-reading the
// intent files from disk, refreshing the controller caches, and posting
// EvIntentUpdate per drift entry. One owner, one test hook — no duplicate
// reconcile seam (#279 fable r3 F-A).
type supervisorReconcileApplyFunc func(ctx context.Context, apply bool) (ReconcileResponse, error)

var supervisorReconcileApplyFn supervisorReconcileApplyFunc = DialSupervisorIPCReconcile

func setSupervisorReconcileApplyHookForTest(fn supervisorReconcileApplyFunc) func() {
	prev := supervisorReconcileApplyFn
	supervisorReconcileApplyFn = fn
	return func() { supervisorReconcileApplyFn = prev }
}

// stopSupervisorOwnedDaemons stops the supervisor-owned daemons in scope
// via the supervisor IPC reconcile verb. Returns (results, handled, err)
// with the same contract shape as restartSupervisorOwnedDaemons:
//
//   - No supervisor intent file, or no targets in scope → (nil, false,
//     nil): nothing is supervisor-owned here, the legacy kill path owns
//     the stop.
//   - IPC unavailable (errors.Is ErrSupervisorIPCUnavailable) → (nil,
//     false, nil): the supervisor is down, so nothing will respawn a
//     killed daemon — the legacy kill path is then correct (no reaper
//     to fight). The fallback covers any REMAINING SCHEDULER ROWS in
//     scope; it does NOT necessarily reach an orphan the dead
//     supervisor left behind, because stopKillCore iterates only
//     scheduler tasks and supervisor-owned daemons have no scheduler
//     row. An orphan that outlived a dead supervisor (the
//     job-protection-failure edge — see the Job Protection runbook in
//     CLAUDE.md) is a pre-existing gap this path does not close;
//     recordStopIntent has still written Desired=stopped, so the next
//     supervisor to come up will not respawn it.
//   - Reconcile reachable but failed → per-target error rows with
//     handled=true: the supervisor is ALIVE but did not confirm the
//     stop. Falling through to taskkill here would hand the reaper a
//     non-clean exit to respawn — the exact churn this path exists to
//     kill — so the caller must skip these task names and surface the
//     error rows instead.
//   - Success → per-target rows derived from the reconcile's per-target
//     drift action, NOT a blanket success. Under the no-legacy ownership
//     model (spec §0.2) every supervisor-intent row is supervisor-owned,
//     so the supervisor posts EvIntentUpdate for a live regular global
//     daemon (`daemon --server X --daemon Y`) exactly as it does for a
//     proxy descriptor — that target's row is a plain success. The only
//     edges that DON'T post are an already-idle/settled daemon (terminate
//     classifies no_op) or a target with no drift entry. We therefore
//     inspect THIS target's drift entry: a post_ev_intent_update entry →
//     plain success row; anything else (or a missing entry) →
//     success-but-deferred row (empty Err + Code =
//     DeferredToIntentWatcherCode) so the row never FALSELY claims a
//     synchronous SM dispatch that did not happen. The stop is durable
//     either way (Desired=stopped is on disk); the only difference is
//     whether convergence was synchronous or is the watcher's job.
//
// PRECONDITION: the caller must have recorded Desired=stopped intent for
// the targets (recordStopIntent) BEFORE calling — the reconcile reads the
// supervisor-intent.json `stops` sub-block (Phase 4-E2: the sole stop source)
// from disk, so a stop intent that is not yet on disk
// cannot be applied.
func stopSupervisorOwnedDaemons(ctx context.Context, server, daemonFilter string) ([]RestartResult, bool, error) {
	targets, err := loadSupervisorOwnedTargets(server, daemonFilter)
	if err != nil {
		return nil, false, err
	}
	if len(targets) == 0 {
		return nil, false, nil
	}
	resp, err := supervisorReconcileApplyFn(ctx, true)
	if err != nil {
		if errors.Is(err, ErrSupervisorIPCUnavailable) {
			return nil, false, nil
		}
		results := make([]RestartResult, 0, len(targets))
		for _, d := range targets {
			results = append(results, RestartResult{
				TaskName: d.TaskName,
				Err:      "supervisor stop reconcile: " + err.Error(),
			})
		}
		return results, true, nil
	}
	// Reconcile transport succeeded — but the response's per-target drift
	// tells us which targets the supervisor actually posted EvIntentUpdate
	// for (post_ev_intent_update drift entries) versus which converge only
	// via the IntentWatcher (the no_op / needs_manual_review / missing-entry
	// edges). Report each honestly.
	results := make([]RestartResult, 0, len(targets))
	for _, d := range targets {
		results = append(results, supervisorDispatchRowForTarget(d.TaskName, resp.Drift))
	}
	return results, true, nil
}

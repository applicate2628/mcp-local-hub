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
	"fmt"
	"strings"
	"time"
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
//   - Action == no_op OR a missing drift entry → success-but-deferred
//     row: empty Err (the intent is durable on disk) + Code =
//     DeferredToIntentWatcherCode, so the row is not a failure but also
//     does not falsely claim synchronous SM dispatch.
//   - Action == needs_manual_review OR any unrecognized action → error
//     row: applyReconcileDrift will not post EvIntentUpdate for the target,
//     so Err must carry the operator-visible failure.
//
// A truly-dispatched row carries no Code: both the stop and the
// refused-restart call sites dispatch through the SAME `reconcile --apply`,
// so neither has a per-target provenance code to attach to the dispatched
// row (the restart path's RESPAWN_REFUSED_INTENT_STOPPED code lives only on
// its OWN refusal/error rows, not on the reconcile-dispatched success row).
func supervisorDispatchRowForTarget(taskName string, drift []DriftEntry) RestartResult {
	if entry, ok := findDriftEntryForTask(taskName, drift); ok {
		switch entry.Action {
		case ReconcileActionPostEvIntentUpdate:
			// Truly dispatched through the SM this reconcile.
			return RestartResult{TaskName: taskName}
		case ReconcileActionNoOp:
			return RestartResult{TaskName: taskName, Code: DeferredToIntentWatcherCode}
		case ReconcileActionNeedsManualReview:
			return RestartResult{TaskName: taskName, Err: supervisorManualReviewStopError(taskName)}
		default:
			return RestartResult{TaskName: taskName, Err: supervisorUnhandledDriftActionStopError(taskName, entry.Action)}
		}
	}
	// No drift entry for this target: durable on disk, IntentWatcher converges
	// within ~60s. Surface that honestly via the typed Code; Err stays empty.
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

type supervisorStopBatchFunc func(ctx context.Context, command StopBatchCommandV1) (StopBatchResultV1, error)

var supervisorStopBatchFn supervisorStopBatchFunc = DialSupervisorIPCStopBatch

func setSupervisorReconcileApplyHookForTest(fn supervisorReconcileApplyFunc) func() {
	prev := supervisorReconcileApplyFn
	supervisorReconcileApplyFn = fn
	return func() {
		supervisorReconcileApplyFn = prev
	}
}

func setSupervisorStopBatchHookForTest(fn supervisorStopBatchFunc) func() {
	previous := supervisorStopBatchFn
	supervisorStopBatchFn = fn
	return func() { supervisorStopBatchFn = previous }
}

func stoppedSettlementResult(taskName string, result StopBatchResultV1, index int) RestartResult {
	if index < 0 || index >= len(result.Settlements) || result.Settlements[index].TaskName != taskName {
		return RestartResult{TaskName: taskName, Err: "supervisor stop settlement incomplete: settlement absent"}
	}
	row := result.Settlements[index]
	if row.State == StoppedSettlementStopped && row.Reason == StoppedSettlementReasonStopped {
		return RestartResult{TaskName: taskName}
	}
	detail := row.Reason
	if row.Error != "" {
		detail += ": " + row.Error
	}
	return RestartResult{TaskName: taskName, Err: "supervisor stop settlement " + string(row.State) + ": " + detail}
}

// stopSupervisorOwnedDaemons stops the supervisor-owned daemons in scope
// via the supervisor IPC reconcile verb. Returns (results, handled, err)
// with the same contract shape as restartSupervisorOwnedDaemons:
//
//   - No supervisor intent file, or no targets in scope → (nil, false,
//     nil): nothing is supervisor-owned here, the legacy kill path owns
//     the stop.
//   - IPC unavailable (errors.Is ErrSupervisorIPCUnavailable) + no live
//     supervisor owner → direct descriptor kill rows with handled=true: the
//     supervisor is down, so nothing will respawn a killed daemon, and fresh
//     v0.6 supervisor-owned global daemons have no scheduler rows for the
//     legacy path to find. Reuse the stop --force descriptor kill surface, but
//     pass no live PID map because IPC is unavailable; descriptor Port remains
//     the targetable kill surface and unsupported hosts fail loud per target.
//   - IPC unavailable + live/undeterminable supervisor owner → per-target
//     retryable error rows with handled=true: a live reaper can respawn a
//     non-clean descriptor kill, so do not force-kill under it.
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
//     non-failure edges that DON'T post are an already-idle/settled daemon
//     (terminate classifies no_op) or a target with no drift entry. We therefore
//     inspect THIS target's drift entry: a post_ev_intent_update entry → plain
//     success row; no_op/missing entry → success-but-deferred row (empty Err +
//     Code = DeferredToIntentWatcherCode) so the row never FALSELY claims a
//     synchronous SM dispatch that did not happen; needs_manual_review or an
//     unsupported action → error row because the supervisor will not apply it.
//     The stop is durable either way (Desired=stopped is on disk); the row
//     distinguishes synchronous dispatch, watcher convergence, and operator
//     intervention.
//
// PRECONDITION: the caller must have recorded Desired=stopped intent for
// the targets (recordStopIntent) BEFORE calling — the reconcile reads the
// supervisor-intent.json `stops` sub-block (Phase 4-E2: the sole stop source)
// from disk, so a stop intent that is not yet on disk
// cannot be applied.
func stopSupervisorOwnedDaemons(ctx context.Context, server, daemonFilter string) ([]RestartResult, bool, error) {
	intent, err := loadSupervisorOwnedIntent()
	if err != nil {
		return nil, false, err
	}
	targets := selectSupervisorOwnedTargets(intent, server, daemonFilter)
	if len(targets) == 0 {
		return nil, false, nil
	}
	results := make([]RestartResult, len(targets))
	batchTargets := make([]StopBatchTargetV1, 0, len(targets))
	admitted := make([]int, 0, len(targets))
	for i, d := range targets {
		results[i].TaskName = d.TaskName
		port, ok := EffectiveDaemonPort(d)
		if !ok {
			results[i].Err = "resolve stop settlement port: unavailable"
			continue
		}
		batchTargets = append(batchTargets, StopBatchTargetV1{TaskName: d.TaskName, ExpectedPort: port})
		admitted = append(admitted, i)
	}
	if len(admitted) == 0 {
		return results, true, nil
	}
	if intent == nil || intent.IntentGeneration == 0 {
		return nil, true, fmt.Errorf("supervisor stop intent generation unavailable")
	}
	intentSnapshot := cloneSupervisorIntentFile(intent)
	stopsSnapshot := intentSnapshot.StopsAsDaemonIntentFile()
	stopsCopy := &DaemonIntentFile{Tasks: make(map[string]DaemonIntent, len(stopsSnapshot.Tasks))}
	for taskName, stop := range stopsSnapshot.Tasks {
		stopsCopy.Tasks[taskName] = stop
	}
	command := StopBatchCommandV1{
		ProtocolVersion:  1,
		BatchID:          fmt.Sprintf("stop-%d", time.Now().UnixNano()),
		Targets:          batchTargets,
		IntentGeneration: intentSnapshot.IntentGeneration,
		SupervisorIntent: intentSnapshot,
		UnifiedStops:     stopsCopy,
	}
	batch, err := supervisorStopBatchFn(ctx, command)
	if err != nil {
		admittedTargets := make([]SupervisorDaemon, 0, len(admitted))
		for _, i := range admitted {
			admittedTargets = append(admittedTargets, targets[i])
		}
		if errors.Is(err, ErrSupervisorIPCUnavailable) {
			if rows, blocked := supervisorIPCUnavailableRetryRowsForLiveOwner(admittedTargets, err); blocked {
				for j, i := range admitted {
					results[i] = rows[j]
				}
				return results, true, nil
			}
			for _, i := range admitted {
				results[i] = forceKillOneSupervisorTarget(targets[i], nil)
			}
			return results, true, nil
		}
		for _, i := range admitted {
			results[i].Err = "supervisor stop reconcile: " + err.Error()
		}
		return results, true, nil
	}
	// Require one controller-owned same-order terminal settlement per target;
	// an older supervisor's missing or malformed batch echo fails closed.
	for batchIndex, i := range admitted {
		results[i] = stoppedSettlementResult(targets[i].TaskName, batch, batchIndex)
	}
	return results, true, nil
}

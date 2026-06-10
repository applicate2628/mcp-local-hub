package api

// Phase A.1 (spec §4, docs/superpowers/specs/2026-06-10-clean-architecture-
// redesign.md) — make Stop/StopAll supervisor-aware. stopSupervisorOwnedDaemons
// is the stop-side mirror of restartSupervisorOwnedDaemons: instead of a
// per-target IPC respawn it issues ONE `reconcile --apply`, because the
// caller has already written Desired=stopped into daemon-intent.json
// (recordStopIntent runs BEFORE this pass); the supervisor re-reads the
// fresh intent, posts EvIntentUpdate per drifted task, and the SM drives
// StRunning→StExiting→StIdle — a deliberate stop the reaper does NOT
// respawn. The legacy alternative (killDaemonByPort = taskkill /F /T) is a
// NON-clean exit the supervisor reaper observes and respawns, which is the
// stop→respawn churn that quarantined live daemons.

import (
	"context"
	"errors"
)

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
//   - Success → per-target success rows (one reconcile covers every
//     target; the supervisor fans EvIntentUpdate out per drifted task).
//
// PRECONDITION: the caller must have recorded Desired=stopped intent for
// the targets (recordStopIntent) BEFORE calling — the reconcile reads
// daemon-intent.json from disk, so a stop intent that is not yet on disk
// cannot be applied.
func stopSupervisorOwnedDaemons(ctx context.Context, server, daemonFilter string) ([]RestartResult, bool, error) {
	targets, err := loadSupervisorOwnedTargets(server, daemonFilter)
	if err != nil {
		return nil, false, err
	}
	if len(targets) == 0 {
		return nil, false, nil
	}
	if _, err := supervisorReconcileApplyFn(ctx, true); err != nil {
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
	results := make([]RestartResult, 0, len(targets))
	for _, d := range targets {
		results = append(results, RestartResult{TaskName: d.TaskName})
	}
	return results, true, nil
}

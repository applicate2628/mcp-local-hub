package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

type supervisorRestartRespawnFunc func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error)

var supervisorRestartRespawnFn supervisorRestartRespawnFunc = DialSupervisorIPCRespawn

func setSupervisorRestartHooksForTest(fn supervisorRestartRespawnFunc) func() {
	prev := supervisorRestartRespawnFn
	supervisorRestartRespawnFn = fn
	return func() { supervisorRestartRespawnFn = prev }
}

// selectSupervisorOwnedTargets filters intent.Daemons down to the rows a
// supervisor-side maintenance pass (restart respawn / stop reconcile)
// should act on for the given (server, daemonFilter) scope. Maintenance
// rows are skipped, server/daemon identity falls back to
// ParseManagedTaskName when the descriptor fields are blank, and every
// returned TaskName is normalized to canonical leading-backslash form.
// Shared by restartSupervisorOwnedDaemons and stopSupervisorOwnedDaemons
// (spec §4 Phase A.1).
func selectSupervisorOwnedTargets(intent *SupervisorIntentFile, server, daemonFilter string) []SupervisorDaemon {
	if intent == nil || len(intent.Daemons) == 0 {
		return nil
	}
	var targets []SupervisorDaemon
	for _, d := range intent.Daemons {
		if isSupervisorRestartMaintenanceTask(d.TaskName) {
			continue
		}
		rowServer := strings.TrimSpace(d.Server)
		rowDaemon := strings.TrimSpace(d.Daemon)
		if rowServer == "" || rowDaemon == "" {
			parsedServer, parsedDaemon := ParseManagedTaskName(d.TaskName)
			if rowServer == "" {
				rowServer = parsedServer
			}
			if rowDaemon == "" {
				rowDaemon = parsedDaemon
			}
		}
		if rowDaemon == "" {
			rowDaemon = "default"
		}
		if server != "" && rowServer != server {
			continue
		}
		if daemonFilter != "" && rowDaemon != daemonFilter {
			continue
		}
		d.TaskName = normalizeSupervisorRestartTaskName(d.TaskName)
		targets = append(targets, d)
	}
	return targets
}

// loadSupervisorOwnedTargets reads supervisor-intent.json from the
// default state dir and selects the supervisor-owned targets in scope.
// A missing intent file (no supervisor install) yields (nil, nil) so
// callers fall through to the legacy scheduler path.
func loadSupervisorOwnedTargets(server, daemonFilter string) ([]SupervisorDaemon, error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return nil, fmt.Errorf("resolve supervisor intent path: %w", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read supervisor-intent.json: %w", err)
	}
	return selectSupervisorOwnedTargets(intent, server, daemonFilter), nil
}

func restartSupervisorOwnedDaemons(ctx context.Context, server, daemonFilter string) ([]RestartResult, bool, error) {
	targets, err := loadSupervisorOwnedTargets(server, daemonFilter)
	if err != nil {
		return nil, false, err
	}
	if len(targets) == 0 {
		return nil, false, nil
	}
	results := make([]RestartResult, 0, len(targets))
	for _, d := range targets {
		// Dial with force=false WITHOUT writing intent first. The
		// pre-dial write (daba5d0, fable F1) made a stop reversible by
		// restart, but it bypassed the deliberate QUARANTINED force-gate:
		// a quarantined daemon's respawn is refused, yet the fresh
		// Desired=running + UpdatedAt was already on disk, so the
		// supervisor's IntentWatcher (≤60s poll, UpdatedAt-only diffs count
		// as changes) posted EvIntentUpdate(running) and StQuarantined +
		// EvIntentUpdate(running) drove StSpawning with failures RESET —
		// the refusal was answered but the spawn was delivered anyway,
		// without force (#279 fable N1, split-brain).
		//
		// The fix gates the intent write on the supervisor's typed refusal
		// code. The respawn now refuses with one of three distinguishable
		// outcomes:
		//
		//   - RESPAWN_REFUSED_INTENT_STOPPED → recoverable: the daemon is
		//     idle and the SM refused only because daemon-intent.json still
		//     records Desired=stopped (e.g. after a supervisor-aware stop).
		//     Write Desired=running, then dispatch ONE `reconcile --apply`
		//     (NOT a respawn redial). This is the F1 reversibility case,
		//     now intent-write-AFTER-first-refusal so the write only happens
		//     for a daemon the SM actually wants to spawn — never for a
		//     quarantined one. See the reconcile-vs-redial rationale at the
		//     branch below (#279 fable r3 F-A).
		//   - QUARANTINED → deliberate force-gate: error row verbatim, NO
		//     intent write. The force-gate holds (#279 fable N1 fix).
		//   - any other failure → error row, no intent write (pre-#279
		//     parity).
		result, err := supervisorRestartRespawnFn(ctx, d.TaskName, false, 5000)
		if err != nil {
			results = append(results, RestartResult{TaskName: d.TaskName, Err: err.Error()})
			continue
		}
		if result.Success {
			// Restore the pre-daba5d0 position: record Desired=running
			// AFTER a successful respawn so the persisted intent matches
			// the now-running daemon. recordRestartIntentForTask takes the
			// BARE task name (no leading backslash) and logs — never
			// propagates — its write failures through the io.Writer; pass
			// os.Stderr so that "logged, never propagated" is actually true
			// (#279 fable N2).
			NewAPI().recordRestartIntentForTask(strings.TrimPrefix(d.TaskName, `\`), os.Stderr)
			results = append(results, RestartResult{TaskName: d.TaskName, Code: result.Code})
			continue
		}
		if result.Code == RespawnRefusedIntentStoppedCode {
			// Recoverable stopped-intent refusal. Write Desired=running
			// (the F1 reversibility intent) NOW — intentionally before the
			// dispatch, and ONLY here, so the running intent is never
			// written for a quarantined daemon. recordRestartIntentForTask
			// logs write failures through os.Stderr and never propagates,
			// so we verify the write landed via a read-back below rather
			// than relying on a swallowed error.
			api := NewAPI()
			api.recordRestartIntentForTask(strings.TrimPrefix(d.TaskName, `\`), os.Stderr)
			if api.ReadDaemonIntent().File.Tasks[d.TaskName].Desired != IntentDesiredRunning {
				// The intent write itself failed (logged to os.Stderr).
				// Nothing on disk to converge — skip the reconcile and
				// emit an honest error row. The IntentWatcher will NOT
				// revive this daemon because disk still records stopped.
				results = append(results, RestartResult{
					TaskName: d.TaskName,
					Err:      "respawn refused (intent stopped); writing Desired=running failed, so the restart cannot be dispatched — see stderr for the write error",
					Code:     result.Code,
				})
				continue
			}
			// Dispatch the spawn via ONE `reconcile --apply` rather than a
			// respawn redial. The redial is dead against the real
			// supervisor: its SM gate reads the controller's daemonIntent
			// CACHE (supervisor_controller.go:709 c.daemonIntent.Lookup —
			// an atomic snapshot with NO disk fallback, :188-199), and that
			// cache is refreshed only by boot, the 60s IntentWatcher tick,
			// and the reconcile verb. The respawn verb never refreshes it,
			// so a redial ms after the disk write reads the stale "stopped"
			// snapshot and is refused again. reconcile --apply re-reads BOTH
			// intent files fresh from disk and refreshes the caches via
			// applyReconcileDrift (supervise_reconcile_ipc.go:593-596)
			// BEFORE posting, so the drift classifier's spawn direction
			// (!hasSched && supervisorOwned && intent=running →
			// post_ev_intent_update, classifyDriftAction supervise_
			// reconcile_ipc.go:475) posts EvIntentUpdate(running) and drives
			// StIdle→StSpawning. This mirrors the stop side, which is
			// synchronous via reconcile for exactly this cache-vs-disk
			// reason (#279 fable r3 F-A).
			if _, reconcileErr := supervisorReconcileApplyFn(ctx, true); reconcileErr != nil {
				// The supervisor was alive ms ago (it just refused the
				// respawn), so an ErrSupervisorIPCUnavailable here is no
				// different from any other reconcile failure: plain error
				// row, no fallback, no taskkill. The running intent IS on
				// disk (verified above), so the IntentWatcher converges the
				// spawn within ≤60s even though this synchronous dispatch
				// failed — say so honestly in the row.
				results = append(results, RestartResult{
					TaskName: d.TaskName,
					Err:      "supervisor reconcile (restart dispatch): " + reconcileErr.Error() + "; Desired=running is on disk, so the IntentWatcher converges the spawn within ~60s",
					Code:     result.Code,
				})
				continue
			}
			// reconcile --apply accepted: the spawn is dispatched through
			// the SM (mirror the stop side's success-row semantics).
			results = append(results, RestartResult{TaskName: d.TaskName})
			continue
		}
		// QUARANTINED (force-gate holds) and any other failure: error row,
		// NO intent write — the deliberate refusal is preserved and a
		// quarantined daemon never receives the running-intent that would
		// let the IntentWatcher bypass the force-gate.
		msg := result.Message
		if msg == "" {
			msg = result.Code
		}
		results = append(results, RestartResult{TaskName: d.TaskName, Err: msg, Code: result.Code})
	}
	return results, true, nil
}

func normalizeSupervisorRestartTaskName(taskName string) string {
	if taskName == "" || strings.HasPrefix(taskName, `\`) {
		return taskName
	}
	return `\` + taskName
}

func isSupervisorRestartMaintenanceTask(taskName string) bool {
	// Use the shared maintenance predicate so *-watchdog rows are skipped on
	// restart too, not just *-weekly-refresh: a watchdog row left in a legacy
	// or hand-edited supervisor-intent.json must not go through supervisor
	// respawn as if it were a daemon (deep-sec #268).
	return isMaintenanceTaskName(taskName)
}

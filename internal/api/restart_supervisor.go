package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
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
		// Both identity components KNOWN + blank descriptor fields: match by the
		// EXACT canonical task name, never by ParseManagedTaskName. The last-hyphen
		// split misattributes hyphenated daemon names — \mcp-local-hub-demo-alpha-beta
		// (server demo, daemon alpha-beta) parses as server demo-alpha / daemon beta,
		// so the (server,daemonFilter) filter below would skip the real target or
		// hit the wrong one. Mirrors supervisorIntentRowMatchesServerDaemon
		// (install.go — bot PR #288 r26, third member of the r19-F1/r20-F4 family).
		if server != "" && daemonFilter != "" && (rowServer == "" || rowDaemon == "") {
			want := canonicalIntentTaskKey("mcp-local-hub-" + server + "-" + daemonFilter)
			if canonicalIntentTaskKey(d.TaskName) != want {
				continue
			}
			d.TaskName = normalizeSupervisorRestartTaskName(d.TaskName)
			targets = append(targets, d)
			continue
		}
		// Populated-field rows, and the single-arg (server-only / unfiltered)
		// callers, keep the existing identity-derivation + filter. ParseManagedTaskName
		// only runs when at least one filter side is empty, so a hyphenated-daemon
		// mis-split can no longer reject the exact target above.
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
		//     idle and the SM refused only because the supervisor-intent.json
		//     stops sub-block still records Desired=stopped (Phase 4-E2: the
		//     sole stop source; e.g. after a supervisor-aware stop).
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
			//
			// Phase 4-E2: recordRestartIntentForTask now CLEARS the stop from
			// the supervisor-intent.json `stops` sub-block (Desired=running
			// drops the entry). The read-back therefore checks that the stop is
			// gone — by re-reading the sub-block authoritatively.
			//
			// P3b fail-closed: the verification must DISTINGUISH a genuine
			// no-stop (clear landed) from a read FAILURE of the now-sole
			// supervisor-intent.json. The plain IntentStillRunning predicate is
			// best-effort: its lookupSupervisorStop collapses a read error and a
			// real no-stop into the same (zero,false) → "running", so a
			// transient/corrupt read would report a silently-FAILED re-enable
			// write as success. supervisorStopClearVerified surfaces the read
			// error so we keep emitting the honest "clearing the stop failed"
			// error row on a read failure (diagnostic fidelity; the dispatched
			// reconcile below re-reads fresh and cannot revive a still-stopped
			// daemon, so this is fidelity-only, not a revive bug).
			api := NewAPI()
			api.recordRestartIntentForTask(strings.TrimPrefix(d.TaskName, `\`), os.Stderr)
			cleared, verifyErr := supervisorStopClearVerified(d.TaskName, time.Now().UTC())
			if !cleared {
				// The intent write itself failed (logged to os.Stderr), OR the
				// sole-source read-back failed so we cannot confirm the clear.
				// Either way: nothing trustworthy on disk to converge — skip the
				// reconcile and emit an honest error row. The IntentWatcher will
				// NOT revive a still-stopped daemon, and on a read failure we
				// must not claim a success we could not verify.
				msg := "respawn refused (intent stopped); clearing the stop (Desired=running) failed, so the restart cannot be dispatched — see stderr for the write error"
				if verifyErr != nil {
					msg = "respawn refused (intent stopped); verifying the cleared stop failed (read of supervisor-intent.json stops sub-block: " + verifyErr.Error() + "), so the restart cannot be dispatched — see stderr for any write error"
				}
				results = append(results, RestartResult{
					TaskName: d.TaskName,
					Err:      msg,
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
			// (!hasSched && intent=running → post_ev_intent_update,
			// classifyDriftAction in supervise_reconcile_ipc.go) posts
			// EvIntentUpdate(running) and drives StIdle→StSpawning. This
			// mirrors the stop side, which is synchronous via reconcile for
			// exactly this cache-vs-disk reason (#279 fable r3 F-A).
			resp, reconcileErr := supervisorReconcileApplyFn(ctx, true)
			if reconcileErr != nil {
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
			// reconcile --apply transport accepted — under the no-legacy
			// ownership model (spec §0.2) every supervisor-intent row is
			// supervisor-owned, so the drift classifier posts EvIntentUpdate
			// for a regular global daemon (`daemon --server X --daemon Y`)
			// on the spawn direction exactly as it does for a proxy
			// descriptor: that target's row is a plain success (truly
			// dispatched). The only edge that does NOT post is a target with
			// no matching drift entry. Inspect this target's drift entry so
			// the row states the truth: a post_ev_intent_update entry →
			// plain success row; otherwise → success-but-deferred row (empty
			// Err + Code = DeferredToIntentWatcherCode, IntentWatcher
			// converges within ~60s since Desired=running is durably on
			// disk, verified above).
			results = append(results, supervisorDispatchRowForTarget(d.TaskName, resp.Drift))
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

// supervisorStopClearVerified is the fail-closed read-back for the restart
// re-enable path. After recordRestartIntentForTask CLEARS the stop from the
// supervisor-intent.json `stops` sub-block (Desired=running drops the entry),
// this re-reads the sub-block — the SOLE stop source after Phase 4-E2 — and
// reports:
//
//   - (true, nil)  → the stop is gone (no entry, expired, or file genuinely
//     absent): the clear landed, dispatch may proceed.
//   - (false, nil) → an ACTIVE stop is still recorded at `now`: the clearing
//     write did not land; emit the honest error row.
//   - (false, err) → the sole-source read itself FAILED (corrupt/locked/IO):
//     we cannot confirm the clear, so fail closed and surface the read error.
//
// The third case is the P3b correction: the plain best-effort
// IntentStillRunning predicate collapses a read error into "running" and would
// have falsely reported a silently-failed re-enable as success. taskName is
// normalized to the canonical leading-backslash key the sub-block is keyed on.
func supervisorStopClearVerified(taskName string, now time.Time) (bool, error) {
	stopIntent, found, err := lookupSupervisorStopChecked(canonicalIntentTaskKey(taskName))
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	active, _ := stopIntent.IsActiveStop(now)
	return !active, nil
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

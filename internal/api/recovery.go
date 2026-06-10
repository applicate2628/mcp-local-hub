// Package api — Task 7 strictly-pure watchdog recovery state machine
// (watchdog plan v13 §1, §11, §12, §16, §18, §19, §21, §22).
//
// recovery.go owns the strictly-pure RecoverStoppedDaemons function and
// the canonical IsRealFailure(int32) bool predicate. Per plan §12 (strict
// purity): no I/O, no Cooldown mutation, no global state. The function
// takes a CooldownReader (read-only) and consults DaemonRegistry +
// OwnedXMLValidator interfaces — every dependency is injected so the
// driver alone owns persistence + Cooldown mutation.
//
// Decision tree (plan §1):
//
//	for row := range status:
//	  if isMaintenanceTaskName(row.TaskName)              { yield "maintenance"; continue }
//	  if !registry.IsManagedDaemon(row.TaskName)          { yield "orphan"; continue }
//	  if row.IsWorkspaceScoped && row.Lifecycle == LifecycleFailed { yield "lazy-proxy-failed-lifecycle"; continue }
//	  if row.State == "Running"                           { continue }   // healthy-Running RecordRunning is the driver's job
//	  if row.State not in {Ready, Stopped, Failed}        { continue }
//	  if !IsRealFailure(row.LastResult)                   { continue }
//
//	  active, reason := intent.Tasks[row.TaskName].IsActiveStop(now)
//	  if active                                            { yield reason; continue }
//
//	  if cool.IsRestartPending(row.TaskName, now)          { yield "restart-pending-skipped"; continue }
//	  if !cool.Due(row.TaskName, now)                      { yield "cooldown"; continue }
//	  if cool.ChronicLimitReached(row.TaskName)            { yield "chronic-failure"; continue }
//	  if !validator.IsOwnedAndValid(row.TaskName)          { yield "suspicious-xml"; continue }
//
//	  yield "restart" with Server, Daemon, Attempt = cool.AttemptsInWindow + 1
//
// IsRealFailure (plan §16, §18) is the single canonical exported
// classifier of Windows Task Scheduler LastResult values.  Tray and
// CLI helpers consume it via api.IsRealFailure; their old local copies
// are deleted in this same task.
package api

import (
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// RecoveryDecision — yielded action vocabulary.
// ---------------------------------------------------------------------------

// RecoveryDecision is one row in the per-tick decision output. The
// driver consumes the slice from RecoverStoppedDaemons and dispatches
// per Action:
//
//   - "restart": invoke the restart pipeline (cooldown.RecordAttempt,
//     mark restart-pending, call Restart/RestartContextWithSnapshot).
//   - "maintenance": no recovery; emit observability log entry.
//   - "orphan": no recovery; emit observability log entry. Driver may
//     also notify the operator that an unowned task is present.
//   - "lazy-proxy-failed-lifecycle": no recovery; the workspace registry
//     already records the failure cause and recovery is operator-driven.
//   - "user-stop" / "user-disabled" / "chronic-failure" / "uninstalled":
//     intent-file directives suppressing auto-revive. No recovery.
//     Reason carries the same string for forensic correlation.
//   - "clock-skew-future-suspect": intent UpdatedAt > now + 5min →
//     fail-CLOSED suppression pending operator review. Reason carries
//     the same string.
//   - "restart-pending-skipped": a prior tick marked the task as having
//     a restart in flight; this tick respects the lockout.
//   - "cooldown": Due(name) returned false; backoff window not over.
//   - "chronic-failure": ChronicLimitReached returned true; auto-revive
//     halted. Driver writes a persistent chronic-failure intent.
//   - "suspicious-xml": OwnedXMLValidator.IsOwnedAndValid returned false;
//     the task was tampered with or is structurally unowned.
//
// Server / Daemon are populated only when the row maps to a known
// (server, daemon) tuple. For "maintenance" / "orphan" they may be
// empty; the driver should not depend on them in those cases.
//
// Attempt is the per-cycle attempt number that this restart will be
// (1..AttemptWindowMax). Populated only for Action="restart"; zero in
// every other case.
//
// Reason mirrors the intent IntentReason* constants when the decision
// originated from intent-file evaluation; otherwise empty.
type RecoveryDecision struct {
	TaskName string
	Server   string
	Daemon   string
	Action   string
	Reason   string
	Attempt  int
}

// ---------------------------------------------------------------------------
// IsRealFailure — canonical LastResult classifier (plan §16, §18).
// ---------------------------------------------------------------------------

// tsInfoCodeMin / tsInfoCodeMax bound the Task Scheduler 2.0
// informational LastResult range. Codes in [0x41300, 0x4130F] are NOT
// failures: examples include 0x41300 (ready to run), 0x41301 (currently
// running), 0x41303 (task has not yet run). The watchdog and tray must
// suppress these so a freshly-installed never-run task does not paint a
// red badge or fire a "daemon failed" toast.
const (
	tsInfoCodeMin = 0x41300
	tsInfoCodeMax = 0x4130F
)

// userExitCodeMin / userExitCodeMax bound the conventional user-program
// exit code range. Per plan §18: 1..0xFFFF treated as a real failure
// (typical exit codes 1, 2, 87, 1063 + 16-bit ceiling). Positive values
// > 0xFFFF and outside the TS info range are treated conservatively as
// non-failures so an unfamiliar code does not blindly trigger restart.
const (
	userExitCodeMin int32 = 1
	userExitCodeMax int32 = 0xFFFF
)

// IsRealFailure reports whether a Windows Task Scheduler LastResult
// value should be treated as a real failure. Single canonical
// definition consumed by the watchdog driver, the tray icon
// aggregator, and CLI tray-state helpers (plan §18).
//
// LastResult semantics (Windows Task Scheduler 2.0):
//
//   - 0x0: clean success → not a failure.
//   - -1: documented sentinel for "task has never run"; emitted by
//     internal/scheduler/scheduler.go when schtasks /Query output omits
//     the "Last Result:" line. NOT a failure.
//   - [0x41300, 0x4130F]: TS informational codes (ready / running /
//     never-run / disabled). NOT failures.
//   - bit 31 set (read as int32: negative): HRESULT / NTSTATUS — real
//     failure. Includes E_FAIL (0x80004005 / -2147467259) and similar.
//   - [1, 0xFFFF]: typical user-program exit codes — real failure.
//   - Other (positive past 0xFFFF and not in TS info range): conservative
//     not-a-failure. The watchdog refuses to restart on an unfamiliar
//     code rather than risk a restart loop.
//
// Pure: no I/O, no global state, deterministic on the input alone.
func IsRealFailure(lastResult int32) bool {
	if lastResult == 0 || lastResult == -1 {
		return false
	}
	if lastResult >= tsInfoCodeMin && lastResult <= tsInfoCodeMax {
		return false
	}
	if lastResult < 0 {
		return true
	}
	if lastResult >= userExitCodeMin && lastResult <= userExitCodeMax {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// isMaintenanceTaskName — plan §21 maintenance filter.
// ---------------------------------------------------------------------------

// isMaintenanceTaskName recognizes scheduler tasks owned by mcp-local-hub
// itself: the watchdog (`...-watchdog`), the supervisor-liveness recovery
// task (`...-liveness`), and the weekly-refresh maintenance jobs
// (`...-weekly-refresh`). The watchdog must not auto-recover its own
// scheduler tasks — they are operationally stable scheduled jobs, not
// crash-prone daemons.
//
// The match is suffix-based so all naming variants are covered:
//   - hub-wide global watchdog: "\\mcp-local-hub-watchdog"
//   - hub-wide supervisor-liveness task: "\\mcp-local-hub-liveness"
//   - per-server / hub-wide weekly refresh:
//     "\\mcp-local-hub-<server>-weekly-refresh",
//     "\\mcp-local-hub-weekly-refresh".
//
// The `-liveness` suffix was added in Phase 3a (v0.6 spec §15 P1-b): the
// additive `\mcp-local-hub-liveness` task is a hub-wide maintenance job
// exactly like the watchdog, so it must be (1) skipped from the
// partial-uninstall "remaining servers" gate (internal/cli/setup.go's
// shouldRemoveGlobalWatchdog — otherwise ServerFromTaskName("...-liveness")
// returns "liveness", a non-empty pseudo-server that permanently poisons the
// last-server gate so the watchdog can never be torn down) and (2) excluded
// from the env-override / status maintenance classifiers the same way the
// watchdog is.
func isMaintenanceTaskName(name string) bool {
	return strings.HasSuffix(name, "-watchdog") ||
		strings.HasSuffix(name, "-liveness") ||
		strings.HasSuffix(name, "-weekly-refresh")
}

// IsMaintenanceTaskName is the exported alias used by cross-package
// callers (e.g. internal/cli/uninstall.go's "is this the last managed
// server?" gate). Same suffix-match contract as the unexported helper.
func IsMaintenanceTaskName(name string) bool {
	return isMaintenanceTaskName(name)
}

// ServerFromTaskName parses a Task Scheduler name like
// "\\mcp-local-hub-<server>-<daemon>" and returns the server segment.
// Returns "" for unparseable or hub-wide tasks (watchdog,
// hub-wide weekly-refresh). Mirrors parseTaskName's first return value
// from status_enrich.go but is exported for cross-package consumers
// that need only the server identity (e.g. partial-uninstall gating).
func ServerFromTaskName(taskName string) string {
	srv, _ := parseTaskName(taskName)
	return srv
}

// ---------------------------------------------------------------------------
// RecoverStoppedDaemons — strictly-pure decision tree (plan §1, §12).
// ---------------------------------------------------------------------------

// RecoverStoppedDaemons evaluates every row in `status` against the
// recovery decision tree and returns one RecoveryDecision per row that
// triggers an action. Healthy-Running rows that need no decision
// (cooldown reset is the driver's job, not this function's) are skipped
// silently — the returned slice carries only actionable rows.
//
// Strict purity (plan §12):
//   - No I/O, no clock reads beyond the supplied `now`.
//   - No mutation: takes CooldownReader, never Cooldown.
//   - Deterministic on the input alone. Safe for any number of
//     concurrent callers.
//
// Driver responsibility (NOT this function's job):
//   - Calling Cool.RecordRunning when a row is observed Running for
//     ≥5 minutes (the §6 reset rule).
//   - Calling Cool.RecordAttempt before issuing a restart.
//   - Calling Cool.MarkRestartPending / ClearRestartPending around the
//     restart call.
//   - Persisting the post-tick state via WriteWatchdogState.
//   - Writing intent files (chronic-failure on ChronicLimitReached, etc).
//
// Determinism: returned slice order matches input slice order.
func RecoverStoppedDaemons(
	now time.Time,
	status []DaemonStatus,
	intent DaemonIntentFile,
	cool CooldownReader,
	validator OwnedXMLValidator,
	registry DaemonRegistry,
) []RecoveryDecision {
	if cool == nil || validator == nil || registry == nil {
		// Degenerate input — no decisions to emit. Return empty slice
		// rather than nil so callers can range over the result without
		// special-casing nil. Strict purity keeps panics out.
		return []RecoveryDecision{}
	}

	out := make([]RecoveryDecision, 0, len(status))

	for _, row := range status {
		// 1. Maintenance tasks bypass every other check. The watchdog
		//    must not auto-recover its own scheduler tasks (plan §21).
		if isMaintenanceTaskName(row.TaskName) {
			out = append(out, RecoveryDecision{
				TaskName: row.TaskName,
				Server:   row.Server,
				Daemon:   row.Daemon,
				Action:   "maintenance",
			})
			continue
		}

		// 2. Orphan filter: tasks not in the managed registry are not
		//    ours to restart (plan §22). Operator inspection only.
		if !registry.IsManagedDaemon(row.TaskName) {
			out = append(out, RecoveryDecision{
				TaskName: row.TaskName,
				Server:   row.Server,
				Daemon:   row.Daemon,
				Action:   "orphan",
			})
			continue
		}

		// 3. Lazy-proxy with backend failure: workspace-scoped daemon
		//    is up (proxy responds) but its backend is in
		//    LifecycleFailed. Recovery is operator-driven; the
		//    workspace registry records the cause separately (plan
		//    §19). No restart from the watchdog.
		if row.IsWorkspaceScoped && row.Lifecycle == LifecycleFailed {
			out = append(out, RecoveryDecision{
				TaskName: row.TaskName,
				Server:   row.Server,
				Daemon:   row.Daemon,
				Action:   "lazy-proxy-failed-lifecycle",
			})
			continue
		}

		// 4. Running rows: skip silently. The healthy-Running
		//    RecordRunning reset is the driver's responsibility (plan
		//    §1 + §12). Nothing to yield here.
		if row.State == "Running" {
			continue
		}

		// 5. Only Ready / Stopped / Failed qualify for restart
		//    consideration (plan §1). Queued / Scheduled / etc. are
		//    transient transition states.
		if row.State != "Ready" && row.State != "Stopped" && row.State != "Failed" {
			continue
		}

		// 6. LastResult must be a real failure (plan §16, §18).
		//    Successes (0), never-run sentinel (-1), and TS info
		//    codes are NOT failures.
		if !IsRealFailure(row.LastResult) {
			continue
		}

		// 7. Intent file directives. IsActiveStop returns the active
		//    reason verbatim (user-stop / user-disabled / chronic-
		//    failure / uninstalled / clock-skew-future-suspect) so
		//    the decision carries forensic context.
		entry := intent.Tasks[row.TaskName]
		if active, reason := entry.IsActiveStop(now); active {
			out = append(out, RecoveryDecision{
				TaskName: row.TaskName,
				Server:   row.Server,
				Daemon:   row.Daemon,
				Action:   reason,
				Reason:   reason,
			})
			continue
		}

		// 8. Restart-pending lockout. A prior tick marked this task
		//    as having a restart in flight; this tick must respect
		//    the lockout (plan §31).
		if cool.IsRestartPending(row.TaskName, now) {
			out = append(out, RecoveryDecision{
				TaskName: row.TaskName,
				Server:   row.Server,
				Daemon:   row.Daemon,
				Action:   "restart-pending-skipped",
			})
			continue
		}

		// 9. Backoff: Due returned false → cooldown phase. Driver
		//    advances state on the next tick (plan §6).
		if !cool.Due(row.TaskName, now) {
			out = append(out, RecoveryDecision{
				TaskName: row.TaskName,
				Server:   row.Server,
				Daemon:   row.Daemon,
				Action:   "cooldown",
			})
			continue
		}

		// 10. Chronic-failure: ChronicLimitReached cycles exhausted.
		//     Driver writes a persistent chronic-failure intent so
		//     subsequent ticks see Active=stopped + reason=chronic-
		//     failure (plan §6).
		if cool.ChronicLimitReached(row.TaskName) {
			out = append(out, RecoveryDecision{
				TaskName: row.TaskName,
				Server:   row.Server,
				Daemon:   row.Daemon,
				Action:   "chronic-failure",
			})
			continue
		}

		// 11. Final security gate: structural ownership + XML
		//     validation. Suspicious tasks are NOT restarted (plan
		//     §5, §32, §47). Driver emits a high-priority audit
		//     entry separately.
		if !validator.IsOwnedAndValid(row.TaskName) {
			out = append(out, RecoveryDecision{
				TaskName: row.TaskName,
				Server:   row.Server,
				Daemon:   row.Daemon,
				Action:   "suspicious-xml",
			})
			continue
		}

		// 12. All gates passed. Yield restart with the per-cycle
		//     attempt number (1..AttemptWindowMax). The driver
		//     calls Cool.RecordAttempt to advance state; this
		//     function only reads the current value.
		out = append(out, RecoveryDecision{
			TaskName: row.TaskName,
			Server:   row.Server,
			Daemon:   row.Daemon,
			Action:   "restart",
			Attempt:  cool.AttemptsInWindow(row.TaskName) + 1,
		})
	}

	return out
}

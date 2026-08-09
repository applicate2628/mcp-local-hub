// Package api — Task 10 intent + audit wiring for existing commands
// (watchdog plan v13 §8, §11, §24, §51, §62, §65, Task 10).
//
// Task 10 wires Task 2's WriteDaemonIntent + Task 3's AppendIntentAudit
// into the existing mcphub install / stop / restart / uninstall /
// register paths so the watchdog driver (Task 9) sees the operator's
// intended state. Per the plan §65 acceptance gates the wiring honors
// these per-command timing + fail-handling rules:
//
//	mcphub stop --server X            BEFORE kill   fail-closed both
//	                                                ways (intent OR
//	                                                audit fail incl.
//	                                                ErrIdentityOversize)
//	mcphub stop --server X --force    skip intent   fail-closed if
//	                                                audit fails (incl.
//	                                                ErrIdentityOversize)
//	mcphub install <s>                AUDIT-FIRST   fail-closed; install
//	                                  (§62 v12)     rejected if audit
//	                                                fails; end state
//	                                                identical to never-
//	                                                attempted install
//	mcphub register <ws> <lang>       AFTER PASS    log warning + cont.
//	mcphub restart                    AFTER /Run    log warning + cont.
//	mcphub uninstall                  BEFORE delete log + proceed
//
// Every command-side audit entry uses the canonical Action label
// constants below (AuditAction*) so plan filters and status renders
// can pivot on Action without parsing free-form text.
//
// This file does NOT touch scheduler.New() — fail-closed semantics
// are implemented BEFORE any scheduler call so a rejected audit can
// abort without leaking partial scheduler state. Test path uses the
// daemonStateRootOverride seam (Task 1) + appendIntentAuditFn seam
// (Task 0/3) to redirect persistence and inject deterministic
// failures.
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"mcp-local-hub/internal/config"
)

// ---------------------------------------------------------------------------
// Canonical audit actions (plan §10, §51, §62, §65).
// ---------------------------------------------------------------------------

// AuditActionUserStop is emitted by `mcphub stop` (without --force)
// BEFORE killing the daemon. Records the operator-initiated stop so
// the watchdog respects it across the StopIntentTTL window.
const AuditActionUserStop = "user-stop"

// AuditActionForcedStopWithoutIntent is emitted by `mcphub stop
// --force` BEFORE killing. The kill path skips WriteDaemonIntent so
// the watchdog regards the daemon as still Desired=running and
// auto-revives it on the next tick — the audit entry is the ONLY
// trace that the operator forced a stop.
const AuditActionForcedStopWithoutIntent = "forced-stop-without-intent"

// AuditActionServerInstall is emitted by `mcphub install` BEFORE any
// scheduler / intent mutation per §62 audit-first canonical timing.
// On audit failure (any error, incl. ErrIdentityOversize) the install
// is REJECTED and the end state is identical to never-attempted.
const AuditActionServerInstall = "server-install"

// AuditActionServerRestarted is emitted by `mcphub restart` AFTER
// each `sch.Run` success. Audit failure is logged + tolerated;
// the restart already happened, so fail-closing here would be lying.
const AuditActionServerRestarted = "server-restarted"

// AuditActionServerUninstalled is emitted by `mcphub uninstall`
// BEFORE deleting any scheduler tasks. Audit failure is logged +
// tolerated — uninstall must still remove the tasks regardless.
const AuditActionServerUninstalled = "server-uninstalled"

// AuditActionWorkspaceRegistered is emitted by `mcphub register`
// AFTER the per-language readiness probe passes. Audit failure is
// logged + tolerated — the workspace registration is observable
// through the registry state on disk.
const AuditActionWorkspaceRegistered = "workspace-registered"

// AuditActionWatchdogInstallElevatedOverride is emitted by
// `mcphub setup` (and `mcphub watchdog install`) when the operator
// uses the --allow-elevated flag to bypass the §42 elevated-install
// refusal. Per §61 the audit write is fail-closed: any error
// translates to exit 11 (audit-required-but-failed) and the
// scheduled task is NOT installed.
const AuditActionWatchdogInstallElevatedOverride = "watchdog-install-elevated-override"

// AuditActionStopFailedNoKill is emitted by `mcphub stop` when
// stopTaskNamesForServer fails (workspace registry corrupt, manifest
// missing, etc.) BEFORE the intent path runs. Per Codex deep-sec PR #135
// Finding 3: the kill is skipped (fail-closed), but without an audit
// entry the operator is left with only stderr — a forensic gap. The
// audit append itself is best-effort: failure here is logged through the
// caller's diagnostic path (when one is available) and never masks the
// underlying load error returned to the caller.
const AuditActionStopFailedNoKill = "stop-failed-no-kill"

// auditWhoMcphubStop / auditWhoMcphubInstall / ... are the stable
// `who` labels recorded on every command-side audit entry. Kept
// human-readable and identical to the CLI surface so log readers
// can filter by command.
const (
	auditWhoMcphubStop         = "mcphub stop"
	auditWhoMcphubStopAll      = "mcphub stop --all"
	auditWhoMcphubStopForce    = "mcphub stop --force"
	auditWhoMcphubInstall      = "mcphub install"
	auditWhoMcphubRestart      = "mcphub restart"
	auditWhoMcphubUninstall    = "mcphub uninstall"
	auditWhoMcphubRegister     = "mcphub register"
	auditWhoMcphubSetup        = "mcphub setup"
	auditWhoMcphubWatchdogInst = "mcphub watchdog install"
)

// AuditWhoMcphubSetup is the public form of auditWhoMcphubSetup so
// the cli package can attach the same `who` label to the elevated-
// override audit entry that mcphub setup writes. Wrapping the
// unexported constant keeps both call-sites synchronized.
const AuditWhoMcphubSetup = auditWhoMcphubSetup

// AuditWhoMcphubWatchdogInstall is the public form of the watchdog-
// install `who` label so the cli surface can use the same audit
// label that the watchdog install path recorded historically.
const AuditWhoMcphubWatchdogInstall = auditWhoMcphubWatchdogInst

// ---------------------------------------------------------------------------
// StopOpts — fail-closed-aware stop entry point (plan §8, §11, §51).
// ---------------------------------------------------------------------------

// StopOpts controls a Stop invocation. Force toggles between the
// intent-writing fail-closed path (default) and the audit-only
// fail-closed path used by `mcphub stop --force`. The CLI surface
// for --force is owned by Task 11 / a future CLI change; Task 10
// only adds the API contract.
type StopOpts struct {
	// Server is the manifest server name (e.g., "serena"). Required.
	Server string
	// DaemonFilter, when non-empty, narrows the stop to one daemon
	// inside the server's manifest. Empty matches every daemon
	// declared by the server (including workspace-scoped lazy
	// proxies via the legacy listTasksForServer extended match).
	DaemonFilter string
	// Force, when true, skips WriteDaemonIntent so the watchdog
	// keeps the daemon Desired=running and auto-revives. The audit
	// entry uses Action=forced-stop-without-intent (priority=high)
	// so the bypass is forensically visible per §51.
	Force bool
}

// StopWithOpts is the fail-closed-aware Stop. Per the plan §8 + §51
// + §65 contracts:
//
//   - Without Force: WriteDaemonIntent (Desired=stopped,
//     Reason=user-stop) BEFORE the audit + kill. The audit entry
//     (Action=user-stop, Priority=high) is appended next; if EITHER
//     intent or audit fails, the kill is skipped and the error is
//     returned verbatim. Plan §51 explicitly classifies
//     ErrIdentityOversize as an audit failure so a malicious
//     oversized task name in a manifest cannot bypass the fail-closed
//     guarantee.
//   - With Force: skip WriteDaemonIntent. Audit entry
//     (Action=forced-stop-without-intent, Priority=high) is appended
//     first; if it fails (incl. ErrIdentityOversize), the kill is
//     skipped and the error is returned. Plan §8 v6: BOTH intent and
//     audit must succeed before kill — for --force the only "intent"
//     surface is the audit, so audit-fail still fail-closes.
//
// On success the function delegates to the existing Stop() kill
// path; the order is preserved (kill-by-port, then sch.Stop) so
// retro-tests of Stop's existing behavior keep passing.
//
// Stop(server, daemonFilter) is the back-compatible wrapper; it
// always uses Force=false. New callers (Task 11 CLI, future GUI
// surfaces) should use StopWithOpts directly.
func (a *API) StopWithOpts(opts StopOpts) ([]RestartResult, error) {
	taskNames, err := stopIntentTaskNamesForServer(opts.Server, opts.DaemonFilter)
	if err != nil {
		// Codex deep-sec PR #135 Finding 3: emit a stop-failed-no-kill audit
		// entry so the forensic trail records the blocked stop attempt even
		// when stopTaskNamesForServer fails before recordStopIntent could
		// run. The audit append is best-effort (failures here would be
		// logged in production through the caller's diagnostic path); the
		// underlying load error still propagates verbatim so the CLI prints
		// it and exits non-zero.
		recordStopFailedNoKill(opts.Server, opts.Force, err)
		return nil, err
	}
	if err := a.recordStopIntent(taskNames, opts.Force); err != nil {
		return nil, err
	}
	// Force skips the supervisor RECONCILE pass on purpose: --force records
	// NO Desired=stopped intent (that is its documented contract — the daemon
	// auto-revives), so a reconcile would read desired=running, post nothing,
	// and the stop would silently no-op. Instead the force branch DIRECTLY
	// kills both the supervisor-owned daemons (Phase F: no scheduler task —
	// stopForceKillSupervisorOwned resolves them from supervisor-intent.json
	// and kills by descriptor port / live PID; bot PR #288 F3) AND the legacy
	// scheduler-task rows (stopKillCore). Both kills are non-clean exits the
	// supervisor reaper observes and auto-revives — the documented force
	// semantic. The supervisor-owned task names are passed to stopKillCore as
	// the handled set so a row reached by both paths is never double-killed.
	if opts.Force {
		supResults, supervisorHandled, err := stopForceKillSupervisorOwned(context.Background(), opts.Server, opts.DaemonFilter)
		if err != nil {
			return nil, err
		}
		handledTasks := schedulerBlockedRestartTaskNames(supResults)
		killResults, err := a.stopKillCore(opts.Server, opts.DaemonFilter, handledTasks)
		if err != nil {
			// All-supervisor-owned install on a host with no usable scheduler
			// (POSIX beta): the supervisor-owned kills already ran, so a
			// scheduler-unavailable error is non-fatal — mirror the no-force
			// stopSupervisorAwareKill tolerance.
			if supervisorHandled && schedulerUnavailableError(err) {
				return supResults, nil
			}
			return nil, err
		}
		return append(supResults, killResults...), nil
	}
	// stopSupervisorAwareKill runs the supervisor reconcile pass (spec
	// §4 Phase A.1; the stop intent recorded above is on disk for it to
	// read) and then the kill path WITHOUT re-running intent/audit
	// (Stop's no-force entry already invokes recordStopIntent — calling
	// the public Stop here would double-record).
	return a.stopSupervisorAwareKill(opts.Server, opts.DaemonFilter)
}

// recordStopIntent runs the BEFORE-kill writes per the StopOpts.Force
// branch. Pure function modulo state-file I/O + audit-log I/O. Returns
// the first error and aborts; the caller MUST NOT proceed to kill on
// non-nil error per plan §51 fail-closed semantic.
//
// Audit appends route through the appendIntentAuditFn seam (Task 0)
// rather than (*API).AppendIntentAudit directly so tests can intercept
// shape + inject failures. The seam is bound at init() time by
// intent_audit.go to the production AppendIntentAudit so production
// callers reach real disk.
func (a *API) recordStopIntent(taskNames []string, force bool) error {
	return a.recordStopIntentAs(taskNames, force, auditWhoMcphubStop)
}

// recordStopIntentAs is recordStopIntent with an explicit `who` label for
// the non-force intent + user-stop-audit path, so StopAll's bulk stop is
// distinguishable from a targeted `mcphub stop X` in the forensic audit
// log (StopAll passes auditWhoMcphubStopAll). The --force branch keeps its
// own dedicated auditWhoMcphubStopForce label regardless of `who` — a
// forced stop is always recorded as the --force surface; StopAll never
// takes the force branch (its supervisor pass passes force=false).
func (a *API) recordStopIntentAs(taskNames []string, force bool, who string) error {
	now := time.Now().UTC()
	for _, tn := range taskNames {
		// Codex deep-sec PR #135 Finding 1: every audit entry carries
		// the canonical leading-backslash task identity so the on-disk
		// intent-audit log uses one shape downstream filters can pivot
		// on. The intent file itself is normalized inside
		// WriteDaemonIntent; we explicitly canonicalize here so audit
		// entries match.
		canonical := canonicalIntentTaskKey(tn)
		if force {
			// Audit-only path (--force). Plan §51: ErrIdentityOversize
			// from AppendIntentAudit fails closed.
			entry := NewIntentAuditEntry(
				WithAction(AuditActionForcedStopWithoutIntent),
				WithTask(canonical),
				WithWho(auditWhoMcphubStopForce),
				WithPriority("high"),
				WithReason("operator forced stop without recording intent"),
			)
			if err := emitCommandAudit(entry); err != nil {
				return fmt.Errorf("forced-stop audit failed for %s: %w", canonical, err)
			}
			continue
		}
		// Intent + audit path (no --force). Plan §8: intent BEFORE
		// kill; both must succeed.
		intent := DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now,
		}
		// Phase 4-E2: the stop lands in supervisor-intent.json's `stops`
		// sub-block (the SOLE stop source after daemon-intent.json is
		// deleted), NOT the legacy daemon-intent.json. WriteStopIntent keeps
		// the canonical-key + 1KB-cap + audit contract identical.
		if err := a.WriteStopIntent(canonical, intent, who); err != nil {
			return fmt.Errorf("stop intent failed for %s: %w", canonical, err)
		}
		// Explicit Action=user-stop audit entry (distinct from
		// WriteDaemonIntent's auto-emitted Action=set-intent record).
		// Required by §65 acceptance gates so log filters can pivot on
		// Action=user-stop. Per §51 this fails closed even on
		// ErrIdentityOversize.
		entry := NewIntentAuditEntry(
			WithAction(AuditActionUserStop),
			WithTask(canonical),
			WithWho(who),
			WithPriority("high"),
			WithReason(IntentReasonUserStop),
		)
		if err := emitCommandAudit(entry); err != nil {
			return fmt.Errorf("user-stop audit failed for %s: %w", canonical, err)
		}
	}
	return nil
}

// emitCommandAudit dispatches one audit entry through the
// appendIntentAuditFn seam (Task 0). Production wiring: intent_audit.go's
// init() binds the seam to (*API).AppendIntentAudit so real disk is
// reached. Tests installed via installRecordingAudit intercept the seam
// for shape + failure-injection assertions.
//
// Returns nil if the seam is not yet bound (defense vs. test ordering
// where init() has not run); production builds always have the binding
// available because both Task 3 (intent_audit.go) and Task 10 are
// compiled in.
func emitCommandAudit(entry IntentAuditEntry) error {
	if appendIntentAuditFn == nil {
		return nil
	}
	return appendIntentAuditFn(entry)
}

// recordStopFailedNoKill emits the stop-failed-no-kill audit entry per
// Codex deep-sec PR #135 Finding 3. Best-effort: if the audit dispatcher
// is not yet bound (init order edge case) or the append itself fails, the
// caller still returns the underlying stop error — never lose forensic
// context to a downstream audit hiccup. The Task field is intentionally
// empty: stopTaskNamesForServer failed before any task identity could be
// resolved, so there is no specific row to attribute. The Reason field
// captures the root-cause error string and the originating server name so
// log readers can pivot without parsing CLI stderr.
func recordStopFailedNoKill(server string, force bool, cause error) {
	who := auditWhoMcphubStop
	if force {
		who = auditWhoMcphubStopForce
	}
	reason := fmt.Sprintf("server=%s: %v", server, cause)
	entry := NewIntentAuditEntry(
		WithAction(AuditActionStopFailedNoKill),
		WithTask(""),
		WithWho(who),
		WithPriority("high"),
		WithReason(reason),
	)
	_ = emitCommandAudit(entry)
}

// ---------------------------------------------------------------------------
// Install audit-first (plan §62 canonical timing).
// ---------------------------------------------------------------------------

// installAuditTaskNames computes the set of scheduler-task names that
// `mcphub install` would create for (manifest, daemonFilter). Returns
// the names in plan order. Workspace-scoped manifests are rejected
// upstream by refuseWorkspaceScopedInstall; this helper only handles
// the global-kind code path.
//
// The plan order matches BuildPlanWithOpts — keeps intent/audit
// emission deterministic so per-task assertions in tests can use a
// fixed index.
func installAuditTaskNames(m *config.ServerManifest, daemonFilter string) []string {
	out := make([]string, 0, len(m.Daemons))
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		out = append(out, "mcp-local-hub-"+m.Name+"-"+d.Name)
	}
	return out
}

// recordInstallAuditPreMutation emits one server-install audit entry
// per planned task BEFORE any scheduler / intent mutation (plan §62
// audit-first canonical timing). Returns the first error and aborts;
// the caller MUST NOT proceed with executeInstallTo on non-nil error.
//
// On audit failure (any error, including ErrIdentityOversize) the end
// state is identical to never-attempted install: no scheduler tasks,
// no intent file modifications, no client-config writes. Tests in
// install_intent_test.go assert this end-state invariant.
func (a *API) recordInstallAuditPreMutation(m *config.ServerManifest, daemonFilter string) error {
	return a.recordInstallAuditForTasks(installAuditTaskNames(m, daemonFilter))
}

// recordInstallAuditForTasks emits one server-install audit entry per task in
// taskNames BEFORE any mutation, with the same fail-closed contract as
// recordInstallAuditPreMutation. Splitting the per-task emission from the
// task-list derivation lets the workspace-scoped fan-out (whose manifest has
// an empty m.Daemons) feed its MATERIALIZED per-workspace task names through
// the same fail-closed pipeline — see installPlanOpts.AuditTaskNames.
func (a *API) recordInstallAuditForTasks(taskNames []string) error {
	for _, tn := range taskNames {
		// Codex deep-sec PR #135 Finding 1: audit entries carry the
		// canonical leading-backslash task identity so the on-disk audit
		// log uses one shape downstream filters can pivot on.
		canonical := canonicalIntentTaskKey(tn)
		entry := NewIntentAuditEntry(
			WithAction(AuditActionServerInstall),
			WithTask(canonical),
			WithWho(auditWhoMcphubInstall),
			WithReason(IntentReasonInstall),
		)
		if err := emitCommandAudit(entry); err != nil {
			return fmt.Errorf("install audit failed for %s (refusing to proceed; manifest may have malicious oversized identifier): %w", canonical, err)
		}
	}
	return nil
}

// installAuditTaskNamesOrOverride returns override verbatim when it is
// non-empty, otherwise the manifest-derived installAuditTaskNames(m,
// daemonFilter). The fan-out install passes the materialized per-workspace
// task names as override because a DaemonTemplate manifest's m.Daemons is
// empty; every other caller passes nil and keeps the manifest-derived list.
func installAuditTaskNamesOrOverride(m *config.ServerManifest, daemonFilter string, override []string) []string {
	if len(override) > 0 {
		return override
	}
	return installAuditTaskNames(m, daemonFilter)
}

// recordInstallIntentPostSuccess writes Desired=running intent for
// every scheduler task created by a successful install. Audit
// failure is best-effort per the plan §65 install table — the
// install already happened, so logging + continuing is the honest
// contract (the watchdog still gets the intent file update, which is
// the load-bearing record).
func (a *API) recordInstallIntentPostSuccess(m *config.ServerManifest, daemonFilter string, w io.Writer) {
	now := time.Now().UTC()
	for _, tn := range installAuditTaskNames(m, daemonFilter) {
		// Codex deep-sec PR #135 Finding 1: WriteDaemonIntent normalizes
		// internally, but pre-canonicalizing here keeps the diagnostic
		// warning text consistent with what shows up on disk.
		canonical := canonicalIntentTaskKey(tn)
		intent := DaemonIntent{
			Desired:   IntentDesiredRunning,
			Reason:    IntentReasonInstall,
			UpdatedAt: now,
		}
		// Phase 4-E2: Desired=running clears any prior stop from the
		// supervisor-intent.json `stops` sub-block (re-enable on
		// (re)install). WriteStopIntent drops the entry when the directive
		// is not an active stop, so a freshly-installed daemon is no longer
		// suppressed — same net effect as the E1 running-tombstone the merge
		// loop dropped, applied directly to the sole stop source.
		if err := a.WriteStopIntent(canonical, intent, auditWhoMcphubInstall); err != nil {
			// Log + continue: install already happened. A missing stop
			// entry is treated as Desired=running per the bootstrap policy,
			// so a write failure here just loses the explicit re-enable
			// record.
			if w != nil {
				fmt.Fprintf(w, "warning: write install intent for %s: %v\n", canonical, err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Restart / Uninstall / Register helpers (plan §65 fail-handling table).
// ---------------------------------------------------------------------------

// recordRestartIntentForTask is invoked by Restart per task AFTER a
// successful sch.Run. Audit + intent failures are logged through w
// and never propagate — the restart already happened.
func (a *API) recordRestartIntentForTask(taskName string, w io.Writer) {
	now := time.Now().UTC()
	// Codex deep-sec PR #135 Finding 1: canonicalize identity before both
	// the intent write and the audit emission so log filters pivot on
	// one shape.
	canonical := canonicalIntentTaskKey(taskName)
	intent := DaemonIntent{
		Desired:   IntentDesiredRunning,
		Reason:    IntentReasonInstall, // re-asserts the install intent
		UpdatedAt: now,
	}
	// Phase 4-E2: Desired=running clears any prior stop from the
	// supervisor-intent.json `stops` sub-block, re-enabling a previously
	// stopped/quarantined daemon on restart (the RESPAWN_REFUSED_INTENT_STOPPED
	// → re-enable contract in gui/daemon_env.go). WriteStopIntent drops the
	// entry because Desired=running is not an active stop.
	if err := a.WriteStopIntent(canonical, intent, auditWhoMcphubRestart); err != nil {
		if w != nil {
			fmt.Fprintf(w, "warning: write restart intent for %s: %v\n", canonical, err)
		}
	}
	entry := NewIntentAuditEntry(
		WithAction(AuditActionServerRestarted),
		WithTask(canonical),
		WithWho(auditWhoMcphubRestart),
		WithReason("operator-initiated restart"),
	)
	if err := emitCommandAudit(entry); err != nil {
		if w != nil {
			fmt.Fprintf(w, "warning: write restart audit for %s: %v\n", canonical, err)
		}
	}
}

// recordUninstallIntentForTasks marks every task to be deleted as
// Desired=stopped + Reason=uninstalled BEFORE the scheduler.Delete
// call. Audit + intent failures are logged through w and never
// propagate — uninstall is idempotent and must succeed regardless.
func (a *API) recordUninstallIntentForTasks(taskNames []string, w io.Writer) {
	now := time.Now().UTC()
	for _, tn := range taskNames {
		// Codex deep-sec PR #135 Finding 1: canonicalize identity before
		// both the intent write and the audit emission.
		canonical := canonicalIntentTaskKey(tn)
		intent := DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUninstalled,
			UpdatedAt: now,
		}
		// Phase 4-E2: the uninstall tombstone (Desired=stopped,
		// Reason=uninstalled) lands in the supervisor-intent.json `stops`
		// sub-block so the reconcile loop respects it should the task
		// reappear (uninstalled never expires — no user-stop TTL).
		if err := a.WriteStopIntent(canonical, intent, auditWhoMcphubUninstall); err != nil {
			if w != nil {
				fmt.Fprintf(w, "warning: write uninstall intent for %s: %v\n", canonical, err)
			}
		}
		entry := NewIntentAuditEntry(
			WithAction(AuditActionServerUninstalled),
			WithTask(canonical),
			WithWho(auditWhoMcphubUninstall),
			WithReason(IntentReasonUninstalled),
		)
		if err := emitCommandAudit(entry); err != nil {
			if w != nil {
				fmt.Fprintf(w, "warning: write uninstall audit for %s: %v\n", canonical, err)
			}
		}
	}
}

// enrollRegisterWorkspaceAudit pre-declares the durable registration-success
// event without physically appending it. The outer registration transaction
// discards it on rollback and invokes it only after all compensations are
// disarmed and every resource finalizer succeeded.
func enrollRegisterWorkspaceAudit(transaction *registrationTransaction, taskName string) {
	canonical := canonicalIntentTaskKey(taskName)
	entry := NewIntentAuditEntry(
		WithAction(AuditActionWorkspaceRegistered),
		WithTask(canonical),
		WithWho(auditWhoMcphubRegister),
		WithReason(IntentReasonRegister),
	)
	transaction.AddAfterCommit("workspace-registered "+canonical, func() error {
		return emitCommandAudit(entry)
	})
}

// writeRegisterRunningIntentForTask clears any active stop before the proxy is
// relied on. The stop-intent owner snapshots and pre-arms exact task-scoped
// restoration under its existing lock before the write can partially persist.
func (a *API) writeRegisterRunningIntentForTask(
	taskName string,
	enroll stopIntentCompensationSink,
) (string, error) {
	canonical := canonicalIntentTaskKey(taskName)
	intent := DaemonIntent{
		Desired:   IntentDesiredRunning,
		Reason:    IntentReasonRegister,
		UpdatedAt: time.Now().UTC(),
	}
	return canonical, a.writeStopIntentWithCompensation(
		canonical,
		intent,
		auditWhoMcphubRegister,
		enroll,
	)
}

// ---------------------------------------------------------------------------
// Manifest-derived task name resolution for Stop (plan §11).
// ---------------------------------------------------------------------------

// stopTaskNamesForServer returns the canonical scheduler task names
// for `mcphub stop --server X [--daemon Y]`. Driven entirely by the
// manifest (and workspace registry for workspace-scoped servers) so
// the intent + audit records can be written WITHOUT a scheduler.New
// round-trip. Per plan §51 fail-closed: a manifest-known task that
// happens to be temporarily missing from scheduler.List is still
// recorded as Desired=stopped so the watchdog respects the operator
// directive once the task reappears.
//
// For workspace-scoped servers (Kind=workspace-scoped) the function
// returns every (workspace, language) task in the registry that
// matches the server's manifest. The optional daemonFilter narrows
// to one task suffix.
//
// Empty result is acceptable (caller may stop a server with no
// installed tasks); the caller proceeds to scheduler.List which
// returns an empty slice and Stop() exits without errors.
func stopTaskNamesForServer(server, daemonFilter string) ([]string, error) {
	if server == "" {
		return nil, errors.New("stop: server is required")
	}
	data, err := loadManifestYAMLEmbedFirst(server)
	if err != nil {
		// Surface the load error. Stop() retries the load via
		// listTasksForServer → serverIsWorkspaceScoped which
		// silently swallows; recordStopIntent must NOT silently
		// proceed because a missing manifest could indicate a
		// retired or typo'd server name. Returning empty + nil
		// would be safer for the legacy path but worse for the
		// fail-closed contract.
		return nil, fmt.Errorf("stop: load manifest %s: %w", server, err)
	}
	m, err := parseManifestForName(server, data)
	if err != nil {
		return nil, fmt.Errorf("stop: parse manifest %s: %w", server, err)
	}
	if m.Kind == config.KindWorkspaceScoped {
		// Workspace-scoped: the per-(workspace, language) task names
		// live in the workspace registry. Build the set + apply the
		// optional daemonFilter (matches by the language suffix).
		//
		// Bot P1.2 fix: registry path/load failures must propagate as
		// errors. Silently returning (nil, nil) would let Stop /
		// StopWithOpts proceed to stopKillCore with an empty intent
		// task-name set — daemons would be killed without any
		// stop intent recorded, and the watchdog would immediately
		// revive them. Plan §8 "Stop fail-closed both ways" requires
		// that if the intent task-name set cannot be determined, the
		// kill is skipped.
		regPath, regErr := DefaultRegistryPath()
		if regErr != nil {
			return nil, fmt.Errorf("stop: resolve workspace registry path for %s: %w", server, regErr)
		}
		reg := NewRegistry(regPath)
		if err := reg.Load(); err != nil {
			return nil, fmt.Errorf("stop: load workspace registry for %s: %w", server, err)
		}
		var out []string
		for _, e := range reg.Workspaces {
			tn := e.TaskName
			if tn == "" {
				continue
			}
			// B.1: this walk owns the LSP-server stop set
			// (kind=workspace-scoped LSP servers: mcp-language-server,
			// gopls-mcp). Serena (sentinel) rows belong to a different
			// server slug and must NEVER be swept by this LSP path,
			// regardless of daemonFilter — serena lifecycle goes
			// through its own stop path. v8 simplification of v6
			// "backend-aware filter" wording: the only correct LSP
			// filter is "skip sentinel rows unconditionally".
			//
			// Closes codex impl-r1 HIGH + sonnet impl-r1 MEDIUM B1:
			// previous code allowed sentinel rows when daemonFilter ==
			// "@serena", which would let an LSP stop call accidentally
			// sweep serena tasks. Unconditional skip prevents that.
			if e.Language == SerenaLanguageSentinel {
				continue
			}
			if daemonFilter != "" && e.Language != daemonFilter {
				continue
			}
			out = append(out, tn)
		}
		return out, nil
	}
	// Global server: one task per daemon in the manifest.
	var out []string
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		out = append(out, "mcp-local-hub-"+m.Name+"-"+d.Name)
	}
	return out, nil
}

func stopIntentTaskNamesForServer(server, daemonFilter string) ([]string, error) {
	taskNames, err := stopTaskNamesForServer(server, daemonFilter)
	if err != nil {
		return nil, err
	}
	targets, err := loadSupervisorOwnedTargets(server, daemonFilter)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(taskNames)+len(targets))
	out := make([]string, 0, len(taskNames)+len(targets))
	for _, name := range taskNames {
		key := strings.TrimPrefix(canonicalIntentTaskKey(name), `\`)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	for _, d := range targets {
		key := strings.TrimPrefix(canonicalIntentTaskKey(d.TaskName), `\`)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out, nil
}

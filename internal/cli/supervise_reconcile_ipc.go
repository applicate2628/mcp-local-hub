// Package cli — Phase A.3 (plan v10 §A.3, 2026-05-20) handler for the
// `reconcile` IPC verb.
//
// The supervisor's startup-time reconciler (supervise_reconcile.go)
// picks up drift (orphan tasks, intent-without-task pairs) automatically
// on cold restart. Operators on long-running supervisor processes who
// have surfaced drift via `mcphub status` need a way to trigger a
// reconcile in-place. `mcphub reconcile` sends an IPC `reconcile` verb
// to the running supervisor; this handler re-reads its intent file,
// walks the scheduler-registered tasks, computes the drift set, and
// (with --apply) posts EvIntentUpdate per drifted task so the SM drives
// the corrective transitions.
//
// Wire contract — see api.ReconcileArgs / api.ReconcileResponse /
// api.DriftEntry in internal/api/supervisor_ipc_types.go for the
// canonical type definitions.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// reconcileSchedulerListFn is a package-private test seam pointing at
// scheduler.New() + sch.List(). Production wiring constructs a real
// scheduler via reconcileSchedulerListFnDefault; tests inject a fake
// that returns a deterministic []scheduler.TaskStatus slice without
// touching the OS scheduler. Mirror of the reaperFn / quiesceHandlerFactory
// seam pattern at supervise.go:57 / :1253.
var reconcileSchedulerListFn = reconcileSchedulerListFnDefault
var reconcileSchedulerNewFn = scheduler.New

var reconcileHandlerTimeout = 25 * time.Second

// reconcileSchedulerListFnDefault wraps scheduler.New() + sch.List("mcp-local-hub-").
// Returns the slice + any underlying error verbatim. Kept as a free
// function (not a method on a wrapper struct) so the test seam swap
// is one variable assignment.
func reconcileSchedulerListFnDefault(ctx context.Context) ([]scheduler.TaskStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sch, err := reconcileSchedulerNewFn()
	if err != nil {
		return nil, fmt.Errorf("scheduler.New: %w", err)
	}
	if lister, ok := sch.(scheduler.ContextLister); ok {
		tasks, err := lister.ListContext(ctx, "mcp-local-hub-")
		if err != nil {
			return nil, fmt.Errorf("scheduler.List: %w", err)
		}
		return tasks, nil
	}

	type listResult struct {
		tasks []scheduler.TaskStatus
		err   error
	}
	done := make(chan listResult, 1)
	go func() {
		tasks, err := sch.List("mcp-local-hub-")
		done <- listResult{tasks: tasks, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("scheduler.List: %w", ctx.Err())
	case res := <-done:
		if res.err != nil {
			return nil, fmt.Errorf("scheduler.List: %w", res.err)
		}
		return res.tasks, nil
	}
}

// setReconcileSchedulerListFnForTest installs a test scheduler-list
// closure. Returns an uninstall function tests defer to restore the
// production wiring. Production code paths never invoke this.
func setReconcileSchedulerListFnForTest(fn func(context.Context) ([]scheduler.TaskStatus, error)) func() {
	prev := reconcileSchedulerListFn
	reconcileSchedulerListFn = fn
	return func() { reconcileSchedulerListFn = prev }
}

func setReconcileSchedulerNewFnForTest(fn func() (scheduler.Scheduler, error)) func() {
	prev := reconcileSchedulerNewFn
	reconcileSchedulerNewFn = fn
	return func() { reconcileSchedulerNewFn = prev }
}

// handleReconcile implements the `reconcile` IPC verb. Single-frame
// response. Behaviour:
//
//   - Parse req.Args → api.ReconcileArgs.
//   - Read supervisor-intent.json from deps.stateDir.
//   - Read daemon-intent.json (optional; missing → "no overrides").
//   - List all scheduler-registered tasks with prefix mcp-local-hub-.
//   - For each intent daemon: compare scheduler state vs intent desired
//     and emit one DriftEntry per (task, drift-class) pair.
//   - Orphan scheduler tasks (no matching intent) get a DriftEntry with
//     IntentDesired="?" and Action="needs_manual_review".
//   - When args.Apply: for each drift entry with Action="post_ev_intent_update",
//     post api.LoopEvent{Kind:api.EvIntentUpdate, TaskName:...} onto the
//     controller's event loop and increment AppliedCount.
//   - Emit one `mcphub-reconcile-invoked` audit event with body
//     {dry_run, drift_count, applied_count}.
//   - Return ReconcileResponse via IPCResponse.Result.
func handleReconcile(conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error {
	args, err := parseReconcileArgs(req.Args)
	if err != nil {
		return writeIPCFrame(conn, api.IPCResponse{
			ID:    req.ID,
			Error: &api.IPCErr{Code: "INVALID_ARGS", Message: err.Error()},
			Final: true,
		})
	}
	ctx, cancel := context.WithTimeout(baseReconcileContext(deps), reconcileHandlerTimeout)
	defer cancel()

	// (1) Read supervisor-intent.json. Missing file is not a hard
	// error — it just means the supervisor has no intent → no drift
	// can be computed against intent. We still call out to scheduler
	// to surface orphan tasks (scheduler has rows, intent is empty).
	intentPath := filepath.Join(deps.stateDir, "supervisor-intent.json")
	intent, err := readSupervisorIntentForReconcile(ctx, intentPath)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return writeReconcileTimeoutFrame(conn, req, err)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:    "RECONCILE_INTENT_READ_FAILED",
				Message: err.Error(),
			},
			Final: true,
		})
	}
	// Phase 4-E2: the supervisor-intent.json `stops` sub-block is the SOLE stop
	// source. A genuinely MISSING file (os.ErrNotExist) is tolerated for the
	// drift/orphan report below (intent synthesized as empty), but in apply-mode
	// the synthesized-empty stops MUST NOT refresh the controller cache — that
	// would clear the operator's last-known stops and a child-exit/SM evaluation
	// in that window would REVIVE a deliberately-stopped daemon (PR #278 P2
	// un-suppress / §3 fail-loud). This mirrors the sibling watcher path
	// resolveWatcherDaemonIntent (supervise.go:1187), where `supFailed` is true
	// for BOTH corrupt AND os.ErrNotExist and keeps the prior cache. Captured
	// here BEFORE `intent` is reassigned to the empty synthetic below.
	supervisorIntentMissing := errors.Is(err, os.ErrNotExist)
	if intent == nil {
		// Either ErrNotExist or read returned (nil, nil) — treat as
		// empty intent file (no daemons declared). The orphan-detection
		// loop below still runs.
		intent = &api.SupervisorIntentFile{}
	}
	updatedIntent := intent

	// (2) Read daemon-intent.json (Phase 4-E2: VESTIGIAL — the file is
	// deleted by the boot-merge and UnifiedStopsFile ignores it, so this read
	// returns IntentStateMissing in steady state). Retained as a defensive
	// read so a stale leftover is handled gracefully (still ignored by the
	// flipped UnifiedStopsFile). The SOLE stop source after E2 is the
	// supervisor-intent.json `stops` sub-block read at step (1); a corrupt
	// read of THAT file already fail-closed above with
	// RECONCILE_INTENT_READ_FAILED, so the cache is never refreshed from a
	// corrupt sole source.
	daemonIntentPath := filepath.Join(deps.stateDir, "daemon-intent.json")
	daemonIntentRes := api.ReadDaemonIntentFile(daemonIntentPath, daemonIntentReadTimeoutForReconcile(ctx))
	if errors.Is(daemonIntentRes.Err, context.DeadlineExceeded) ||
		errors.Is(daemonIntentRes.Err, context.Canceled) ||
		errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		errors.Is(ctx.Err(), context.Canceled) {
		err := daemonIntentRes.Err
		if err == nil {
			err = ctx.Err()
		}
		return writeReconcileTimeoutFrame(conn, req, err)
	}
	var rawDaemonIntent *api.DaemonIntentFile
	if daemonIntentRes.State == api.IntentStateValid {
		rawDaemonIntent = &daemonIntentRes.File
	} else if daemonIntentRes.Err != nil {
		emitReconcileDaemonIntentReadFailed(deps.events, daemonIntentPath, daemonIntentRes)
	}

	// Phase 4-E2: resolve the stop source through the unified view. The
	// supervisor-intent.json `stops` sub-block (updatedIntent, read at step 1)
	// is now the SOLE, AUTHORITATIVE source; UnifiedStopsFile ignores
	// rawDaemonIntent (the deleted/stale daemon-intent.json). `daemonIntentTasks`
	// (computeIntentDesired) derives from this sub-block — empty only when the
	// sub-block itself is empty. The drift report is computed against whatever
	// stops that sub-block carries, exactly as the startup reconciler treats it.
	updatedDaemonIntent := api.UnifiedStopsFile(updatedIntent, rawDaemonIntent)
	daemonIntentTasks := updatedDaemonIntent.Tasks

	// Cache-refresh fail-loud guard (PR #278 P2 / §3 contract, anchored to the
	// SOLE stop source after E2). The controller daemonIntentCache holds the
	// operator's last-known stops; refreshing it from a corrupt/unreadable sole
	// source would un-suppress a deliberately-stopped daemon — but the SOLE stop
	// source after E2 is the supervisor-intent.json `stops` sub-block, NOT
	// daemon-intent.json. The refresh source is updatedDaemonIntent =
	// UnifiedStopsFile(updatedIntent, rawDaemonIntent), and UnifiedStopsFile
	// deliberately IGNORES rawDaemonIntent (supervisor_intent.go:403) — it returns
	// the sub-block unconditionally. So the trust decision must follow the
	// AUTHORITATIVE supervisor-intent read, never the vestigial daemon-intent read:
	//   (a) supervisor-intent read OK  → REFRESH from the sub-block, regardless of
	//       any daemon-intent read error (the unified view ignores it anyway).
	//   (b) supervisor-intent MISSING under apply → preserve (nil) via the
	//       missing-sole-source guard immediately below.
	//   (c) supervisor-intent CORRUPT → already fail-closed UPSTREAM (step 1
	//       returns RECONCILE_INTENT_READ_FAILED before reaching here), so it is
	//       unreachable at this point.
	// Gating the refresh on daemonIntentRes.Err was the regression bot PR #286
	// caught: a stale daemon-intent.json directory (EISDIR → State=missing, Err
	// set) would skip the refresh and leave a FRESH operator stop in the
	// authoritative sub-block un-applied — drift computes the terminate and posts
	// EvIntentUpdate, but the controller reads the STALE cache and defaults the
	// daemon back to running. The daemon-intent-read state therefore does NOT gate
	// the refresh post-E2; only the missing-sole-source guard does.
	cacheRefreshSupervisorIntent := updatedIntent
	cacheRefreshDaemonIntent := updatedDaemonIntent
	// Apply-mode missing-sole-source guard (PR #278 P2, this-PR P2-BLOCKER): in
	// apply mode, a physically ABSENT supervisor-intent.json (the sole stop
	// source after E2) yields synthetic empty descriptor + stop views. Force nil
	// for BOTH apply cache-refresh sources so applyReconcileDrift preserves the
	// prior supervisor descriptor cache and daemonIntentCache instead of
	// orphaning live children or clearing operator stops. Dry-run / orphan-report
	// is unaffected: it never touches the cache, and the drift report below still
	// computes against the empty intent (the deliberate missing-file tolerance for
	// orphan detection).
	if args.Apply && supervisorIntentMissing {
		cacheRefreshSupervisorIntent = nil
		cacheRefreshDaemonIntent = nil
		emitSupervisorIntentCacheRefreshSkipped(deps.events, intentPath)
	}

	// (3) Scheduler snapshot.
	schedTasks, schedErr := reconcileSchedulerListFn(ctx)
	if errors.Is(schedErr, context.DeadlineExceeded) || errors.Is(schedErr, context.Canceled) {
		return writeReconcileTimeoutFrame(conn, req, schedErr)
	}
	if schedErr != nil {
		if api.SchedulerUnavailableError(schedErr) {
			schedTasks = nil
		} else {
			return writeIPCFrame(conn, api.IPCResponse{
				ID: req.ID,
				Error: &api.IPCErr{
					Code:    "RECONCILE_SCHEDULER_LIST_FAILED",
					Message: schedErr.Error(),
				},
				Final: true,
			})
		}
	}

	// (4) Build a normalized scheduler lookup keyed on canonical
	// task name (leading backslash). The scheduler may report names
	// in either form; we coerce.
	schedByTask := make(map[string]scheduler.TaskStatus, len(schedTasks))
	for _, t := range schedTasks {
		schedByTask[canonicalTaskNameForReconcile(t.Name)] = t
	}

	// (5) Walk intent → compute drift entries per declared daemon.
	drift := make([]api.DriftEntry, 0, len(intent.Daemons)+len(schedTasks))
	seenIntentTasks := make(map[string]struct{}, len(intent.Daemons))
	now := time.Now().UTC()
	for _, d := range intent.Daemons {
		taskName := canonicalTaskNameForReconcile(d.TaskName)
		seenIntentTasks[taskName] = struct{}{}

		intentDesired := computeIntentDesired(taskName, daemonIntentTasks, now)
		schedState, hasSched := lookupSchedulerState(schedByTask, taskName)
		smState := lookupControllerSMState(deps, taskName)
		// The loop variable `d` is the FRESH on-disk descriptor (the NEW
		// intent a global reinstall/upgrade just wrote). The controller's
		// intentCache still holds the descriptor the live child was spawned
		// WITH (the OLD intent) because applyReconcileDrift only refreshes
		// that cache AFTER this loop completes (step 7 below). Capturing the
		// cached descriptor HERE — before that refresh — lets the classifier
		// detect a same-task-name descriptor rewrite (port/command/args/
		// runtime_spec change) on a StRunning daemon and drive a restart so
		// the child is respawned with the NEW descriptor instead of being
		// classified as steady-state no_op. (Race note: the 60s IntentWatcher
		// could in principle refresh the cache to the NEW descriptor before a
		// reconcile fires, collapsing OLD==NEW and missing the restart; but
		// the watcher posts respawns only for stop-intent deltas, not
		// descriptor deltas — diffIntentSnapshots — so the install→reconcile
		// path remains the authoritative corrective channel and the cache
		// still holds OLD in that window.)
		cachedDescriptor := lookupControllerCachedDescriptor(deps, taskName)
		// `d` is a per-iteration copy of the slice element; take its address
		// for the drift comparison. classifyDriftAction uses it synchronously
		// within this iteration and never retains it, so the address is safe.
		newDescriptor := &d
		var action string

		// Orphaned-LSP-descriptor handling mirrors the startup reconciler guard
		// before the generic drift action filter. classifyDriftAction has no
		// registry view, so it would treat a running orphan (intent=running,
		// StRunning) as no_op and leave the unregistered proxy serving until a
		// supervisor restart. The shared predicate fails open on registry
		// read/lock failures, preserving the leave-alone posture for uncertain
		// registry state.
		if intentDesired == api.ReconcileIntentDesiredRunning &&
			isLSPWorkspaceProxyDescriptor(d) &&
			!api.LSPRegistryRowBacksDescriptor(d) {
			emitOrphanedLSPDescriptorSkipped(deps.events, d)
			if smStateIsLiveForOrphanStop(smState) {
				intentDesired = api.ReconcileIntentDesiredStopped
				action = api.ReconcileActionPostEvIntentUpdate
				if args.Apply {
					markOrphanedLSPStopIntentForReconcile(cacheRefreshDaemonIntent, taskName, now)
				}
			} else {
				action = api.ReconcileActionNeedsManualReview
			}
		} else {
			action = classifyDriftAction(schedState, hasSched, intentDesired, smState, cachedDescriptor, newDescriptor)
		}

		drift = append(drift, api.DriftEntry{
			TaskName:       taskName,
			SchedulerState: schedState,
			IntentDesired:  intentDesired,
			SMState:        smState,
			Action:         action,
		})
	}

	// (6) Orphan scheduler tasks (not in intent) — manual-review only.
	for taskName, t := range schedByTask {
		if _, ok := seenIntentTasks[taskName]; ok {
			continue
		}
		drift = append(drift, api.DriftEntry{
			TaskName:       taskName,
			SchedulerState: normalizeSchedulerState(t.State),
			IntentDesired:  api.ReconcileIntentDesiredUnknown,
			SMState:        lookupControllerSMState(deps, taskName),
			Action:         api.ReconcileActionNeedsManualReview,
		})
	}

	// (7) Apply mode: dispatch EvIntentUpdate per drift entry whose
	// action is post_ev_intent_update. Other actions (no_op,
	// needs_manual_review) never trigger SM events.
	appliedCount := 0
	if args.Apply {
		// Pass the cache-refresh sources (nil ONLY when the authoritative
		// supervisor-intent.json is physically absent under apply) rather than the
		// always-non-nil synthetic empty files, so applyReconcileDrift's nil guards
		// preserve the prior controller caches on a missing sole source.
		appliedCount = applyReconcileDrift(deps, drift, cacheRefreshSupervisorIntent, cacheRefreshDaemonIntent)
	}

	// (8) Audit emit. Failures are non-fatal (the response is still
	// honored); the event log itself surfaces emit errors via its own
	// degraded-write path.
	if deps.events != nil {
		_ = deps.events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "ipc",
			Event:    "mcphub-reconcile-invoked",
			Body: map[string]any{
				"dry_run":       !args.Apply,
				"drift_count":   len(drift),
				"applied_count": appliedCount,
			},
		})
	}

	// (9) Response.
	resp := api.ReconcileResponse{
		DryRun:       !args.Apply,
		DriftCount:   len(drift),
		AppliedCount: appliedCount,
		Drift:        drift,
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return writeIPCFrame(conn, api.IPCResponse{
			ID:    req.ID,
			Error: &api.IPCErr{Code: "RECONCILE_MARSHAL_FAILED", Message: err.Error()},
			Final: true,
		})
	}
	return writeIPCFrame(conn, api.IPCResponse{
		ID:     req.ID,
		OK:     true,
		Result: json.RawMessage(body),
		Final:  true,
	})
}

func baseReconcileContext(deps ipcDispatchDeps) context.Context {
	if deps.controllerProvider != nil {
		if ctrl := deps.controllerProvider(); ctrl != nil && ctrl.ctx != nil {
			return ctrl.ctx
		}
	}
	return context.Background()
}

func readSupervisorIntentForReconcile(ctx context.Context, path string) (*api.SupervisorIntentFile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	type readResult struct {
		intent *api.SupervisorIntentFile
		err    error
	}
	done := make(chan readResult, 1)
	go func() {
		intent, err := api.ReadSupervisorIntent(path)
		done <- readResult{intent: intent, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-done:
		return res.intent, res.err
	}
}

func daemonIntentReadTimeoutForReconcile(ctx context.Context) time.Duration {
	timeout := daemonIntentReadLockTimeout
	if ctx == nil {
		return timeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	return timeout
}

func writeReconcileTimeoutFrame(conn net.Conn, req api.IPCRequest, err error) error {
	if err == nil {
		err = context.DeadlineExceeded
	}
	return writeIPCFrame(conn, api.IPCResponse{
		ID: req.ID,
		Error: &api.IPCErr{
			Code:      "RECONCILE_TIMEOUT",
			Message:   err.Error(),
			Retryable: true,
		},
		Final: true,
	})
}

// parseReconcileArgs converts req.Args (which arrives as map[string]any
// from JSON decode) into api.ReconcileArgs. We round-trip through JSON
// rather than asserting per-field because (a) it matches the pattern
// the IPC types contract is designed around (single struct, single
// JSON shape) and (b) the field set may grow in future without callers
// having to remember every field's per-type assertion.
//
// Missing args (req.Args == nil) is allowed — defaults to Apply=false
// so a bare `reconcile` call is treated as a dry-run report.
func parseReconcileArgs(raw map[string]any) (api.ReconcileArgs, error) {
	var args api.ReconcileArgs
	if raw == nil {
		return args, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return args, fmt.Errorf("marshal args: %w", err)
	}
	if err := json.Unmarshal(b, &args); err != nil {
		return args, fmt.Errorf("unmarshal args into ReconcileArgs: %w", err)
	}
	return args, nil
}

// computeIntentDesired returns the per-task desired-state string. The
// daemon-intent.json overrides take precedence; absent entries default
// to "running" (the mixed-bootstrap default at
// daemon_intent.go:230).
func computeIntentDesired(taskName string, daemonIntent map[string]api.DaemonIntent, now time.Time) string {
	if daemonIntent == nil {
		return api.ReconcileIntentDesiredRunning
	}
	entry, ok := daemonIntent[taskName]
	if !ok {
		// Try the bare form as fallback. canonicalTaskNameForReconcile
		// already produced leading-backslash form, so a strict miss
		// here means the daemon-intent file is keyed bare.
		entry, ok = daemonIntent[strings.TrimPrefix(taskName, `\`)]
		if !ok {
			return api.ReconcileIntentDesiredRunning
		}
	}
	if active, _ := entry.IsActiveStop(now); active {
		return api.ReconcileIntentDesiredStopped
	}
	if entry.Desired == api.IntentDesiredRunning {
		return api.ReconcileIntentDesiredRunning
	}
	// Unknown / empty Desired in a present entry → treat as running
	// (matches the mixed-bootstrap default).
	return api.ReconcileIntentDesiredRunning
}

// lookupSchedulerState returns the normalized scheduler-state string +
// a bool indicating whether the scheduler has a registered task with
// this name at all. Absent task → ("missing", false).
func lookupSchedulerState(schedByTask map[string]scheduler.TaskStatus, taskName string) (string, bool) {
	t, ok := schedByTask[taskName]
	if !ok {
		return api.ReconcileSchedulerStateMissing, false
	}
	return normalizeSchedulerState(t.State), true
}

// normalizeSchedulerState maps the various scheduler-state strings the
// Windows / POSIX backends report into our reconcile vocabulary. The
// scheduler.TaskStatus.State value is backend-specific (Windows
// schtasks emits "Ready", "Running", "Disabled"; Linux/macOS use
// different vocabularies). We collapse to running / stopped because
// the drift classifier only cares about that distinction.
func normalizeSchedulerState(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "running":
		return api.ReconcileSchedulerStateRunning
	case "ready", "idle", "stopped", "disabled", "queued":
		return api.ReconcileSchedulerStateStopped
	case "":
		return api.ReconcileSchedulerStateStopped
	default:
		// Unknown state — surface verbatim so operators can investigate
		// rather than silently collapsing into "stopped" which would
		// mask scheduler-side anomalies.
		return raw
	}
}

// classifyDriftAction computes the Action field per (scheduler-state,
// intent-desired) pair. The matrix:
//
//	sched=missing   intent=running  → post_ev_intent_update (spawn directly from supervisor-intent.json), UNLESS SM=quarantined → needs_manual_review or SM=backoff-waiting → no_op
//	sched=missing   intent=stopped  → post_ev_intent_update (terminate) when the SM state is live; no_op otherwise
//	sched=running   intent=running  → no_op (steady state)
//	sched=running   intent=stopped  → post_ev_intent_update (terminate)
//	sched=stopped   intent=running  → post_ev_intent_update (spawn), UNLESS SM=quarantined → needs_manual_review or SM=backoff-waiting → no_op
//	sched=stopped   intent=stopped  → post_ev_intent_update (terminate) when the SM state is live; no_op otherwise
//	sched=<other>   *               → needs_manual_review (unknown scheduler state)
//
// No-legacy ownership (spec §0.2 — no compatibility, no migration, no old
// users): EVERY row in supervisor-intent.json is supervisor-owned. The
// supervisor has the full command in the descriptor and spawns regular
// daemons through the generic `daemon --server X --daemon Y` path directly
// from EvIntentUpdate, exactly as it spawns the proxy descriptors — so a
// missing scheduler row is NOT a "legacy task lost, operator must
// re-install" signal any more; it is simply the by-design state of a
// supervisor-intent descriptor. The old `sched=missing + intent=running →
// needs_manual_review` row (a relic of the scheduler era, when regular
// global daemons WERE Task Scheduler tasks and a missing row meant
// re-install) therefore DIES: the spawn now posts EvIntentUpdate directly.
// This pre-implements the Phase B/F ownership classification of the
// redesign spec ahead of the scheduler-task removal; the hasSched=true
// branches below are unchanged because real scheduler rows still reconcile
// against scheduler state until Phase F removes them.
//
// The terminate direction on the sched=missing row follows the same
// reasoning (spec §4 Phase A.1): supervisor-intent rows have NO scheduler
// row by design, so scheduler state can never witness them running — the
// controller's SM state is the only witness. Without this row, `mcphub
// stop` would write Desired=stopped into daemon-intent.json and the
// apply-mode reconcile would classify the still-running daemon as no_op;
// the caller's only remaining lever is taskkill, whose NON-clean exit the
// supervisor reaper observes and respawns — the stop→respawn churn that
// drives daemons into quarantine. Posting EvIntentUpdate instead lets the
// SM drive StRunning→StExiting→StIdle (deliberate stop, no respawn).
// Dead/settled SM states (StIdle, StQuarantined) stay no_op: there is
// nothing live to terminate, and a quarantined daemon's intent flip must
// not be treated as drift (same quarantine-respect as the startup
// reconciler's `isStopped && !running` gate).
//
// Spawn-direction quarantine-respect (intent=running + SM=quarantined →
// needs_manual_review): `reconcile --apply` now fires on EVERY `mcphub
// stop`/`mcphub restart` (via stop_supervisor.go / restart_supervisor.go),
// and the drift loop walks ALL supervisor-intent rows — not just the
// stop/restart target. The gate lives on BOTH spawn-direction arms: the
// no-scheduler-row arm (supervisor-intent descriptors, the common case) AND
// the scheduler-stopped arm (a daemon that quarantined while still carrying a
// stale/residual scheduler-stopped row). Either arm without the gate would
// revive the quarantined bystander on the next reconcile.
// A BYSTANDER daemon that is genuinely quarantined
// (StQuarantined) but whose intent is running (or absent → computeIntentDesired
// defaults to running) would otherwise classify post_ev_intent_update, and
// applyReconcileDrift would post EvIntentUpdate(running). The SM row
// StQuarantined + EvIntentUpdate(running) → StSpawning RESETS the failure
// window (supervisor_state_machine.go:204-206), so stopping or restarting ANY
// daemon would revive EVERY quarantined bystander with its quarantine wiped —
// breaking the quarantine contract ("force required") fleet-wide. Returning
// needs_manual_review (NOT post) closes that: apply-mode never dispatches
// EvIntentUpdate for the quarantined bystander, and the drift entry surfaces
// "quarantined daemon wants running — operator must force or reset" rather than
// pretending steady-state (no_op). This mirrors the terminate direction's
// settled-SM no_op (smStateIsLive excludes StQuarantined) AND the startup
// reconciler's quarantine-respect (the `isStopped && !running` gate plus the
// Reconciler never posting EvStart/EvIntentUpdate for an untouched quarantined
// row). Deliberate un-quarantine paths are untouched: a force respawn
// (EvManualRestart via handleRespawn force=true) and `install --upgrade
// --reset-failure-windows` both intentionally clear quarantine and never route
// through this classifier. The 60s IntentWatcher does NOT have this hole for
// bystanders (diffIntentSnapshots posts only for CHANGED intent entries, so an
// untouched bystander gets no event), so this classifier gate (mirrored on
// BOTH spawn-direction arms — !hasSched and scheduler-stopped) closes the
// reconcile-side bystander revival on every spawn-direction path.
//
// Spawn-direction settled-state preservation: StRunning is already at
// desired=running, and StBackoffWaiting is a supervisor-owned wait state. Its
// timer, not reconcile --apply, owns the next retry. Posting
// EvIntentUpdate(running) here would preempt the backoff timer on unrelated
// applies and collapse the crash-loop delay, so both spawn arms classify
// backoff no_op.
//
// Residual scheduler-stopped terminate direction mirrors sched=missing:
// scheduler rows can linger in Stopped after ownership moved to the supervisor,
// so a stopped intent plus a live SM row is still an active terminate request.
// Settled SM states remain no_op because there is no live child or pending
// supervisor action to cancel.
//
// Running-descriptor-drift restart direction (r38-2 P2): a StRunning daemon
// is normally steady-state no_op (it is already at desired=running). But when
// a global reinstall/upgrade REWRITES the SAME-task-name descriptor (changes
// the daemon's port, command, args, or runtime_spec/upstream-port) while the
// child is StRunning, the running child is still serving the OLD descriptor
// (old port/command) even though install has already written the NEW
// descriptor + client configs (clients now point at the new port/command that
// was never started). cachedDescriptor is what the running child was spawned
// with (the controller intentCache snapshot at classify time, BEFORE
// applyReconcileDrift refreshes it); newDescriptor is the freshly-read on-disk
// intent. When the two differ on a spawn-affecting field, classify a RESTART
// (post_ev_manual_restart) so applyReconcileDrift drives StRunning -> StExiting
// (issue terminate, queued_action=respawn) -> StSpawning, respawning the child
// from the refreshed (NEW) descriptor — instead of leaving the stale child up.
// Identical descriptors keep no_op (no churn on every apply). StBackoffWaiting
// stays no_op (its timer owns the retry and already respawns from the refreshed
// cache; preempting it would collapse the crash-loop delay) and StQuarantined
// stays needs_manual_review (a descriptor change must not silently un-quarantine
// — operator force/reset is required). Only the live StRunning child gets the
// in-place restart. Drift compares ONLY spawn-affecting fields
// (supervisorDescriptorSpawnDrift) — cosmetic fields (updated_at, manifest_hash)
// do not trigger a needless restart.
func classifyDriftAction(schedState string, hasSched bool, intentDesired string, smState api.SMState, cachedDescriptor, newDescriptor *api.SupervisorDaemon) string {
	if !hasSched {
		if intentDesired == api.ReconcileIntentDesiredRunning {
			if smState == api.StQuarantined {
				return api.ReconcileActionNeedsManualReview
			}
			if smState == api.StRunning {
				// Live child: restart it IFF the descriptor it was spawned
				// with drifted from the freshly-read intent; otherwise it is
				// genuinely steady-state.
				if supervisorDescriptorSpawnDrift(cachedDescriptor, newDescriptor) {
					return reconcileActionPostEvManualRestart
				}
				return api.ReconcileActionNoOp
			}
			if smState == api.StBackoffWaiting {
				return api.ReconcileActionNoOp
			}
			return api.ReconcileActionPostEvIntentUpdate
		}
		if intentDesired == api.ReconcileIntentDesiredStopped && smStateIsLive(smState) {
			return api.ReconcileActionPostEvIntentUpdate
		}
		return api.ReconcileActionNoOp
	}
	switch schedState {
	case api.ReconcileSchedulerStateRunning:
		if intentDesired == api.ReconcileIntentDesiredStopped {
			return api.ReconcileActionPostEvIntentUpdate
		}
		// Same running-descriptor-drift restart as the !hasSched arm: a
		// scheduler-running row that the controller is also driving as a live
		// StRunning child must respawn with the rewritten descriptor rather
		// than serve the stale one. Only the StRunning case (a controller-owned
		// live child the SM can terminate+respawn) is eligible; other SM states
		// under a scheduler-running row stay no_op (no live controller child to
		// restart in place).
		if smState == api.StRunning && supervisorDescriptorSpawnDrift(cachedDescriptor, newDescriptor) {
			return reconcileActionPostEvManualRestart
		}
		return api.ReconcileActionNoOp
	case api.ReconcileSchedulerStateStopped:
		if intentDesired == api.ReconcileIntentDesiredRunning {
			// Same spawn-direction quarantine-respect as the !hasSched arm:
			// a StQuarantined daemon that ALSO carries a (stale/residual)
			// scheduler-stopped row must NOT revive on every `reconcile
			// --apply` (= every `mcphub stop`/`mcphub restart`). The SM row
			// StQuarantined + EvIntentUpdate(running) → StSpawning RESETS the
			// failure window (supervisor_state_machine.go:204-206), so without
			// this gate stopping/restarting ANY daemon would revive a
			// quarantined bystander that happens to retain a scheduler row,
			// with its quarantine wiped. needs_manual_review surfaces it as
			// drift ("quarantined daemon wants running — operator must force or
			// reset") rather than pretending steady-state.
			if smState == api.StQuarantined {
				return api.ReconcileActionNeedsManualReview
			}
			if smState == api.StBackoffWaiting {
				return api.ReconcileActionNoOp
			}
			return api.ReconcileActionPostEvIntentUpdate
		}
		if intentDesired == api.ReconcileIntentDesiredStopped && smStateIsLive(smState) {
			return api.ReconcileActionPostEvIntentUpdate
		}
		return api.ReconcileActionNoOp
	default:
		// Unknown scheduler state (we surfaced it verbatim above) →
		// the SM has no defined transition for it. Operator review.
		return api.ReconcileActionNeedsManualReview
	}
}

// reconcileActionPostEvManualRestart is the drift Action for a StRunning
// daemon whose spawn-affecting descriptor was rewritten in place (a global
// reinstall/upgrade changed the same task name's port/command/args/
// runtime_spec). applyReconcileDrift posts EvManualRestart for it, driving
// StRunning -> StExiting (terminate the stale child) -> StSpawning (respawn
// from the refreshed NEW descriptor). Kept package-local: it is produced by
// classifyDriftAction and consumed by applyReconcileDrift in THIS file, and
// emitted verbatim on the DriftEntry.Action wire string alongside the
// api.ReconcileAction* values — the wire value is a plain string regardless of
// where the constant lives, so the contract is unchanged.
const reconcileActionPostEvManualRestart = "post_ev_manual_restart"

// reconcileManualRestartTerminateDescriptorBodyKey carries the pre-refresh
// descriptor that the currently-running child was spawned from. Descriptor-drift
// apply refreshes the controller cache before posting EvManualRestart so the
// eventual respawn uses the new descriptor; termination must still prove and
// kill the stale child against its old command identity.
const reconcileManualRestartTerminateDescriptorBodyKey = "terminate_descriptor"

// supervisorDescriptorSpawnDrift reports whether two descriptors for the SAME
// task differ on a field that AFFECTS the spawned child — the fields a respawn
// would launch with differently. It deliberately ignores cosmetic / metadata
// fields (UpdatedAt has no descriptor home; ManifestHash, Server, Daemon,
// TaskName are identity/provenance, not spawn inputs) so a no-op apply does not
// churn a restart.
//
// Compared spawn-affecting fields:
//   - Command, Args                — the generic `mcphub daemon ...` launch line
//   - Port                         — the client-facing bind port
//   - Env                          — generic-path child environment
//   - Workspace                    — workspace-scoped daemons key on this
//   - RuntimeSpec.{ChildCommand,ChildArgs,EnvRefs,UpstreamPort,ExternalPort,
//     WorkspacePath} — the materialized proxy-launch spec (a serena/LSP proxy
//     re-reads NONE of the manifest at spawn, so a proxy descriptor rewrite is
//     ENTIRELY captured by these spec fields)
//
// A nil old descriptor (no cached descriptor for this task — e.g. the
// controller never spawned it / cache miss) returns false: with no recorded
// spawned descriptor there is nothing to prove drift against, so we do NOT
// fabricate a restart (the smState==StRunning gate already bounds this to a
// live child, but a missing cache snapshot stays conservative no_op). A nil new
// descriptor likewise returns false (no fresh intent to drift toward).
func supervisorDescriptorSpawnDrift(oldDescriptor, newDescriptor *api.SupervisorDaemon) bool {
	if oldDescriptor == nil || newDescriptor == nil {
		return false
	}
	if oldDescriptor.Command != newDescriptor.Command {
		return true
	}
	if !equalStringSliceForReconcile(oldDescriptor.Args, newDescriptor.Args) {
		return true
	}
	if oldDescriptor.Port != newDescriptor.Port {
		return true
	}
	if oldDescriptor.Workspace != newDescriptor.Workspace {
		return true
	}
	if !equalStringMapForReconcile(oldDescriptor.Env, newDescriptor.Env) {
		return true
	}
	return runtimeSpecSpawnDrift(oldDescriptor.RuntimeSpec, newDescriptor.RuntimeSpec)
}

// runtimeSpecSpawnDrift compares the spawn-affecting fields of two
// DaemonRuntimeSpec pointers (the materialized proxy-launch spec). A
// presence change (nil <-> non-nil) is itself drift — a descriptor that
// gained or lost its runtime_spec spawns through a different launch path.
func runtimeSpecSpawnDrift(oldSpec, newSpec *api.DaemonRuntimeSpec) bool {
	if oldSpec == nil && newSpec == nil {
		return false
	}
	if oldSpec == nil || newSpec == nil {
		return true
	}
	if oldSpec.ChildCommand != newSpec.ChildCommand {
		return true
	}
	if !equalStringSliceForReconcile(oldSpec.ChildArgs, newSpec.ChildArgs) {
		return true
	}
	if oldSpec.UpstreamPort != newSpec.UpstreamPort {
		return true
	}
	if oldSpec.ExternalPort != newSpec.ExternalPort {
		return true
	}
	if oldSpec.WorkspacePath != newSpec.WorkspacePath {
		return true
	}
	return !equalStringMapForReconcile(oldSpec.EnvRefs, newSpec.EnvRefs)
}

// equalStringSliceForReconcile reports element-wise slice equality. A nil and
// an empty slice are treated as equal (both spawn the same empty arg list).
func equalStringSliceForReconcile(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalStringMapForReconcile reports key/value map equality. A nil and an
// empty map are treated as equal (both spawn the same empty environment).
func equalStringMapForReconcile(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va != vb {
			return false
		}
	}
	return true
}

// smStateIsLive reports whether the SM state names a daemon the
// supervisor is actively driving (a child exists, a spawn is in flight,
// or a backoff timer would respawn one). These are exactly the states
// from which api.Transition's EvIntentUpdate(stopped) rows make
// progress toward StIdle: StRunning→StExiting (issue terminate),
// StSpawning→queued_action=stop, StBackoffWaiting→StIdle (cancel
// timer), StExiting→clear queued_action (cancels a pending respawn).
// StIdle and StQuarantined are settled — see classifyDriftAction.
func smStateIsLive(s api.SMState) bool {
	switch s {
	case api.StSpawning, api.StRunning, api.StExiting, api.StBackoffWaiting:
		return true
	}
	return false
}

// smStateIsLiveForOrphanStop is the explicit state set where an orphaned LSP
// descriptor must be driven through EvIntentUpdate(stopped). These states all
// have api.Transition rows for stopped intent that either terminate a child,
// queue/cancel a stop, or cancel a respawn timer. StIdle and StQuarantined stay
// excluded so never-spawned/terminal orphans remain manual-review only.
func smStateIsLiveForOrphanStop(s api.SMState) bool {
	switch s {
	case api.StSpawning, api.StRunning, api.StExiting, api.StBackoffWaiting:
		return true
	}
	return false
}

// lookupControllerSMState reads the per-task SM state from the live
// controller (if any). Returns api.StIdle when the controller is
// absent or hasn't yet tracked a state for this task. Mirrors the
// pattern in supervise_respawn.go:120-132 where the handler reaches
// through deps.controllerProvider.
func lookupControllerSMState(deps ipcDispatchDeps, taskName string) api.SMState {
	if deps.controllerProvider == nil {
		return api.StIdle
	}
	ctrl := deps.controllerProvider()
	if ctrl == nil {
		return api.StIdle
	}
	st, _ := ctrl.GetSMState(taskName)
	return st
}

// lookupControllerCachedDescriptor returns the controller intentCache's
// CURRENT snapshot descriptor for the task — i.e. the descriptor the live
// child was spawned with — or nil when the controller / cache is absent or has
// no entry. It MUST be called from the reconcile drift loop BEFORE
// applyReconcileDrift refreshes the cache (step 7), so the returned descriptor
// is the OLD (pre-rewrite) descriptor that the running daemon is actually
// serving. classifyDriftAction compares it against the freshly-read on-disk
// descriptor to detect a same-task-name descriptor rewrite on a StRunning
// daemon. Mirrors the lookupControllerSMState reach-through pattern.
func lookupControllerCachedDescriptor(deps ipcDispatchDeps, taskName string) *api.SupervisorDaemon {
	if deps.controllerProvider == nil {
		return nil
	}
	ctrl := deps.controllerProvider()
	if ctrl == nil || ctrl.intentCache == nil {
		return nil
	}
	d, _ := ctrl.intentCache.Lookup(taskName)
	return d
}

func markOrphanedLSPStopIntentForReconcile(file *api.DaemonIntentFile, taskName string, now time.Time) {
	if file == nil {
		return
	}
	if file.Tasks == nil {
		file.Tasks = map[string]api.DaemonIntent{}
	}
	file.Tasks[taskName] = api.DaemonIntent{
		Desired:   api.IntentDesiredStopped,
		Reason:    api.IntentReasonUninstalled,
		UpdatedAt: now,
	}
}

// applyReconcileDrift dispatches one SM event per actionable drift entry and
// returns the count of events actually posted:
//
//   - Action=post_ev_intent_update  → EvIntentUpdate (spawn/terminate per the
//     refreshed intent).
//   - Action=post_ev_manual_restart → EvManualRestart (terminate the stale
//     live child + respawn from the NEW descriptor). The cache refresh below
//     runs FIRST, so by the time EvManualRestart is posted the intentCache
//     already holds the rewritten descriptor and the SM-driven respawn launches
//     the child with the NEW port/command/args/runtime_spec.
//
// Entries with other actions are skipped (no_op and needs_manual_review never
// dispatch SM events).
//
// When the controller is absent (rare — only in unit-test fixtures
// that don't wire one) the function silently skips dispatch; the
// drift entries are still reported in the response, but appliedCount
// will be 0 because there's no event loop to post onto.
//
// updatedIntent and daemonIntentCacheRefresh are cache-refresh sources, NOT the
// drift overlay. Nil means "the authoritative supervisor-intent.json source was
// physically absent under apply" and preserves the prior cache. Non-nil means
// the source read successfully and refreshes the matching cache. Descriptor-cache
// preservation keeps running children observable; daemonIntentCache preservation
// keeps prior stop intents from being silently cleared.
func applyReconcileDrift(
	deps ipcDispatchDeps,
	drift []api.DriftEntry,
	updatedIntent *api.SupervisorIntentFile,
	daemonIntentCacheRefresh *api.DaemonIntentFile,
) int {
	if deps.controllerProvider == nil {
		return 0
	}
	ctrl := deps.controllerProvider()
	if ctrl == nil {
		return 0
	}
	manualRestartTerminateDescriptors := map[string]*api.SupervisorDaemon{}
	for _, entry := range drift {
		if entry.Action != reconcileActionPostEvManualRestart {
			continue
		}
		if d := lookupControllerCachedDescriptor(deps, entry.TaskName); d != nil {
			copy := *d
			manualRestartTerminateDescriptors[entry.TaskName] = &copy
		}
	}
	if updatedIntent != nil {
		ctrl.refreshSupervisorIntent(updatedIntent)
	}
	if daemonIntentCacheRefresh != nil {
		ctrl.daemonIntent.Refresh(daemonIntentCacheRefresh)
	}
	if ctrl.eventLoop == nil {
		return 0
	}
	applied := 0
	for _, entry := range drift {
		var kind api.SMEvent
		switch entry.Action {
		case api.ReconcileActionPostEvIntentUpdate:
			kind = api.EvIntentUpdate
		case reconcileActionPostEvManualRestart:
			kind = api.EvManualRestart
		default:
			// no_op / needs_manual_review never dispatch SM events.
			continue
		}
		body := map[string]any(nil)
		if kind == api.EvManualRestart {
			if d := manualRestartTerminateDescriptors[entry.TaskName]; d != nil {
				body = map[string]any{reconcileManualRestartTerminateDescriptorBodyKey: d}
			}
		}
		ctrl.eventLoop.Post(api.LoopEvent{
			Kind:     kind,
			TaskName: entry.TaskName,
			Body:     body,
		})
		applied++
	}
	return applied
}

func emitReconcileDaemonIntentReadFailed(events *api.SupervisorEventLog, path string, res api.IntentReadResult) {
	if events == nil || res.Err == nil {
		return
	}
	body := map[string]any{
		"path":  path,
		"state": res.State,
		"err":   res.Err.Error(),
	}
	if res.QuarantinePath != "" {
		body["quarantine_path"] = res.QuarantinePath
	}
	_ = events.Emit(api.SupervisorEvent{
		Severity: api.SupervisorEventSeverityWarn,
		Source:   api.SupervisorEventSourceIPC,
		Event:    "daemon-intent-read-failed",
		Body:     body,
	})
}

func emitSupervisorIntentCacheRefreshSkipped(events *api.SupervisorEventLog, path string) {
	if events == nil {
		return
	}
	_ = events.Emit(api.SupervisorEvent{
		Severity: api.SupervisorEventSeverityWarn,
		Source:   api.SupervisorEventSourceIPC,
		Event:    "supervisor-intent-cache-refresh-skipped",
		Body: map[string]any{
			"path":   path,
			"reason": "supervisor-intent.json absent during apply; preserving previous controller caches",
		},
	})
}

// canonicalTaskNameForReconcile coerces a task name to leading-backslash
// canonical form so intent + scheduler + daemon-intent + SM-state
// lookups all agree on one key shape. Empty string is returned unchanged
// so downstream nil-checks remain explicit.
//
// Mirror of canonicalSupervisorTaskName at supervise_status.go:89 — we
// re-declare here to avoid coupling the reconcile handler to a status
// helper whose ownership is a sibling concern. Both functions share the
// same invariant (prepend "\" if missing).
func canonicalTaskNameForReconcile(taskName string) string {
	if taskName == "" {
		return taskName
	}
	if taskName[0] == '\\' {
		return taskName
	}
	return `\` + taskName
}

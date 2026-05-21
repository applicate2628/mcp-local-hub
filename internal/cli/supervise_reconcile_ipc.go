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

var reconcileHandlerTimeout = 25 * time.Second

// reconcileSchedulerListFnDefault wraps scheduler.New() + sch.List("mcp-local-hub-").
// Returns the slice + any underlying error verbatim. Kept as a free
// function (not a method on a wrapper struct) so the test seam swap
// is one variable assignment.
func reconcileSchedulerListFnDefault(ctx context.Context) ([]scheduler.TaskStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sch, err := scheduler.New()
	if err != nil {
		return nil, fmt.Errorf("scheduler.New: %w", err)
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
	if intent == nil {
		// Either ErrNotExist or read returned (nil, nil) — treat as
		// empty intent file (no daemons declared). The orphan-detection
		// loop below still runs.
		intent = &api.SupervisorIntentFile{}
	}
	updatedIntent := intent

	// (2) Read daemon-intent.json. Missing OR corrupt → "no overrides"
	// (the same fallback the startup reconciler uses; see
	// supervise_reconcile.go:124-130). We do NOT fail-close here
	// because the operator's primary need is the drift report; a
	// corrupt intent file is a separate alarm surfaced through the
	// usual quarantine path.
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
	var daemonIntentTasks map[string]api.DaemonIntent
	if daemonIntentRes.State == api.IntentStateValid {
		daemonIntentTasks = daemonIntentRes.File.Tasks
	}
	updatedDaemonIntent := &daemonIntentRes.File

	// (3) Scheduler snapshot.
	schedTasks, schedErr := reconcileSchedulerListFn(ctx)
	if errors.Is(schedErr, context.DeadlineExceeded) || errors.Is(schedErr, context.Canceled) {
		return writeReconcileTimeoutFrame(conn, req, schedErr)
	}
	if schedErr != nil {
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:    "RECONCILE_SCHEDULER_LIST_FAILED",
				Message: schedErr.Error(),
			},
			Final: true,
		})
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
	for _, d := range intent.Daemons {
		taskName := canonicalTaskNameForReconcile(d.TaskName)
		seenIntentTasks[taskName] = struct{}{}

		intentDesired := computeIntentDesired(taskName, daemonIntentTasks)
		schedState, hasSched := lookupSchedulerState(schedByTask, taskName)
		smState := lookupControllerSMState(deps, taskName)
		action := classifyDriftAction(schedState, hasSched, intentDesired)

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
		appliedCount = applyReconcileDrift(deps, drift, updatedIntent, updatedDaemonIntent)
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
func computeIntentDesired(taskName string, daemonIntent map[string]api.DaemonIntent) string {
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
	if entry.Desired == api.IntentDesiredStopped {
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
//	sched=missing   intent=running  → post_ev_intent_update (intent says run but no scheduler task — operator must re-install; we surface this as drift but EvIntentUpdate can't bring a missing task back)
//	sched=missing   intent=stopped  → no_op (intent says stop and there's no task; nothing to do)
//	sched=running   intent=running  → no_op (steady state)
//	sched=running   intent=stopped  → post_ev_intent_update (terminate)
//	sched=stopped   intent=running  → post_ev_intent_update (spawn)
//	sched=stopped   intent=stopped  → no_op (steady state)
//	sched=<other>   *               → needs_manual_review (unknown scheduler state)
//
// For sched=missing + intent=running we mark `needs_manual_review`
// rather than `post_ev_intent_update` because the EvIntentUpdate event
// cannot bring a missing scheduler task back — the operator needs
// `mcphub install --upgrade` to re-register the scheduler entry. The
// drift report surfaces the gap; --apply does not paper over it.
func classifyDriftAction(schedState string, hasSched bool, intentDesired string) string {
	if !hasSched {
		// Intent says running but scheduler has no row → needs install,
		// not an EvIntentUpdate. We surface the drift but refuse to
		// "apply" anything because the SM has no scheduler-side anchor
		// to drive.
		if intentDesired == api.ReconcileIntentDesiredRunning {
			return api.ReconcileActionNeedsManualReview
		}
		return api.ReconcileActionNoOp
	}
	switch schedState {
	case api.ReconcileSchedulerStateRunning:
		if intentDesired == api.ReconcileIntentDesiredStopped {
			return api.ReconcileActionPostEvIntentUpdate
		}
		return api.ReconcileActionNoOp
	case api.ReconcileSchedulerStateStopped:
		if intentDesired == api.ReconcileIntentDesiredRunning {
			return api.ReconcileActionPostEvIntentUpdate
		}
		return api.ReconcileActionNoOp
	default:
		// Unknown scheduler state (we surfaced it verbatim above) →
		// the SM has no defined transition for it. Operator review.
		return api.ReconcileActionNeedsManualReview
	}
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

// applyReconcileDrift posts EvIntentUpdate per drift entry with
// Action=post_ev_intent_update. Returns the count of events actually
// posted. Entries with other actions are skipped (no_op and
// needs_manual_review never dispatch SM events).
//
// When the controller is absent (rare — only in unit-test fixtures
// that don't wire one) the function silently skips dispatch; the
// drift entries are still reported in the response, but appliedCount
// will be 0 because there's no event loop to post onto.
func applyReconcileDrift(
	deps ipcDispatchDeps,
	drift []api.DriftEntry,
	updatedIntent *api.SupervisorIntentFile,
	updatedDaemonIntent *api.DaemonIntentFile,
) int {
	if deps.controllerProvider == nil {
		return 0
	}
	ctrl := deps.controllerProvider()
	if ctrl == nil {
		return 0
	}
	ctrl.intentCache.Refresh(updatedIntent)
	ctrl.daemonIntent.Refresh(updatedDaemonIntent)
	if ctrl.eventLoop == nil {
		return 0
	}
	applied := 0
	for _, entry := range drift {
		if entry.Action != api.ReconcileActionPostEvIntentUpdate {
			continue
		}
		ctrl.eventLoop.Post(api.LoopEvent{
			Kind:     api.EvIntentUpdate,
			TaskName: entry.TaskName,
		})
		applied++
	}
	return applied
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

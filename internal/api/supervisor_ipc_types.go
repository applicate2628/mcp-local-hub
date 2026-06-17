package api

// Phase A.3 (plan v10 §A.3, 2026-05-20) — IPC types for the
// `mcphub reconcile` operator command. These shapes are NEW v9/v10
// declarations layered on top of the existing IPCRequest / IPCResponse
// envelope in supervisor_ipc.go.
//
// Wire flow:
//
//	Request  : IPCRequest{Cmd: "reconcile", Args: {"apply": bool}}
//	Response : IPCResponse with Result decoded as ReconcileResponse below.
//
// The supervisor's reconcile handler (internal/cli/supervise_reconcile_ipc.go)
// walks (a) `<state-dir>/supervisor-intent.json` daemons, (b) the
// scheduler-registered `mcp-local-hub-*` task list, and (c) the
// per-task `daemon-intent.json` desired-state overrides; it produces
// one DriftEntry per (task_name, drift_class) pair. When Apply=true,
// the handler posts api.LoopEvent{Kind: api.EvIntentUpdate, ...} per
// drift entry so the supervisor's state machine drives Run/Stop/Delete
// transitions in-place WITHOUT a supervisor cold-restart.

// ReconcileArgs is the IPCRequest.Args payload for the `reconcile` verb.
// Apply=false (dry-run) means: build the drift report, emit the audit
// event, return ReconcileResponse with AppliedCount=0 and no event-loop
// posts. Apply=true (apply mode) means: same drift report, but ALSO
// post EvIntentUpdate per drift entry whose Action is
// "post_ev_intent_update"; AppliedCount counts the events posted.
type ReconcileArgs struct {
	Apply bool `json:"apply"` // dry-run when false; trigger SM transitions when true
}

// ReconcileResponse is the IPC response body shape returned by the
// reconcile handler. The CLI client (DialSupervisorIPCReconcile) decodes
// IPCResponse.Result into this type so the cobra `mcphub reconcile`
// command can print a human-readable table.
//
// DryRun mirrors the inverse of args.Apply so a caller inspecting the
// response after the fact can tell whether the handler actually
// dispatched events. DriftCount is len(Drift); AppliedCount is the
// count of entries the handler posted EvIntentUpdate for (always 0 when
// DryRun is true; <= DriftCount otherwise).
type ReconcileResponse struct {
	DryRun       bool         `json:"dry_run"`
	DriftCount   int          `json:"drift_count"`
	AppliedCount int          `json:"applied_count"` // 0 when dry-run
	Drift        []DriftEntry `json:"drift"`
}

// DriftEntry describes one (task_name, drift_class) pair the reconcile
// handler observed. The four fields are populated as follows:
//
//   - TaskName: canonical leading-backslash form, matching the
//     intent file's SupervisorDaemon.TaskName.
//   - SchedulerState: "running" | "stopped" | "missing". "missing" is
//     populated when the intent carries a daemon descriptor but the
//     scheduler has no registered task with that name (the operator
//     can re-install via `mcphub install --upgrade`).
//   - IntentDesired: "running" | "stopped" | "?". "?" is populated for
//     orphan scheduler tasks (scheduler has a task with no matching
//     intent descriptor; the operator must manually decide whether to
//     stop+delete or add an intent entry).
//   - SMState: the controller's per-task SM state at the time of the
//     reconcile request. Useful for operators who want to confirm the
//     scheduler-state vs intent-state drift isn't transient mid-transition
//     (e.g. StSpawning while waiting for EvHealthOK).
//   - Action: the handler's plan for this entry. "post_ev_intent_update"
//     for a fixable drift (--apply will dispatch); "no_op" for already-
//     aligned tasks reported for completeness; "needs_manual_review"
//     for orphans or ambiguous cases the SM cannot drive in-place.
type DriftEntry struct {
	TaskName       string  `json:"task_name"`
	SchedulerState string  `json:"scheduler_state"` // "running" | "stopped" | "missing"
	IntentDesired  string  `json:"intent_desired"`  // "running" | "stopped" | "?"
	SMState        SMState `json:"sm_state"`
	Action         string  `json:"action"` // "post_ev_intent_update" | "no_op" | "needs_manual_review"

	// HotSwap is the OBSERVATION-ONLY hot-swap eligibility verdict for this
	// daemon (zero-downtime hot-swap design, Slice 3). Additive + omitempty: a
	// pre-Slice-3 consumer ignores it, and orphan scheduler-task entries (no
	// intent descriptor to classify) leave it nil. It NEVER feeds the supervisor
	// Action — it exists so the reconcile/status surface can show which daemons
	// would benefit from the gate-ON migration before any behavior changes.
	HotSwap *HotSwapEligibility `json:"hot_swap,omitempty"`
}

// Reconcile action string constants — kept here next to the type so
// callers (handler + CLI + tests) agree on the exact wire vocabulary.
const (
	ReconcileActionPostEvIntentUpdate = "post_ev_intent_update"
	ReconcileActionNoOp               = "no_op"
	ReconcileActionNeedsManualReview  = "needs_manual_review"
)

// Reconcile scheduler-state constants. Mirror the JSON wire shape so
// callers don't need to remember literal strings.
const (
	ReconcileSchedulerStateRunning = "running"
	ReconcileSchedulerStateStopped = "stopped"
	ReconcileSchedulerStateMissing = "missing"
)

// Reconcile intent-desired constants. "?" is the orphan sentinel.
const (
	ReconcileIntentDesiredRunning = "running"
	ReconcileIntentDesiredStopped = "stopped"
	ReconcileIntentDesiredUnknown = "?"
)

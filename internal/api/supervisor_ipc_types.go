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
	Apply        bool             `json:"apply"` // dry-run when false; trigger SM transitions when true
	SettleTarget *ReconcileTarget `json:"settle_target,omitempty"`
}

// ReconcileTarget identifies one exact persisted workspace generation whose
// controller-owned runtime should be observed before the reconcile response is
// returned. RegisteredAt is the registry row's RFC3339Nano generation stamp;
// ExpectedPort is the port that exact row and its intent descriptor must name.
// The optional object is additive: callers that omit it retain the historical
// enqueue-and-return reconcile behavior.
type ReconcileTarget struct {
	WorkspaceKey  string `json:"workspace_key"`
	WorkspacePath string `json:"workspace_path"`
	TaskName      string `json:"task_name"`
	RegisteredAt  string `json:"registered_at"`
	ExpectedPort  int    `json:"expected_port"`
}

type ReconcileTargetSettlementState string

const (
	ReconcileTargetSettlementReady      ReconcileTargetSettlementState = "ready"
	ReconcileTargetSettlementIncomplete ReconcileTargetSettlementState = "incomplete"
	ReconcileTargetSettlementFailed     ReconcileTargetSettlementState = "failed"
)

const (
	ReconcileTargetReasonReady                    = "ready"
	ReconcileTargetReasonTargetUnsupported        = "target_unsupported"
	ReconcileTargetReasonControllerUnavailable    = "controller_unavailable"
	ReconcileTargetReasonEventLoopUnavailable     = "event_loop_unavailable"
	ReconcileTargetReasonSettlementTimeout        = "settlement_timeout"
	ReconcileTargetReasonSettlementCancelled      = "settlement_cancelled"
	ReconcileTargetReasonLivenessUnverified       = "liveness_unverified"
	ReconcileTargetReasonPortUnbound              = "port_unbound"
	ReconcileTargetReasonTargetGenerationReplaced = "target_generation_replaced"
	ReconcileTargetReasonIntentMissing            = "intent_missing"
	ReconcileTargetReasonSpawnFailed              = "spawn_failed"
	ReconcileTargetReasonBackoff                  = "backoff"
	ReconcileTargetReasonQuarantined              = "quarantined"
	ReconcileTargetReasonPortOwnerMismatch        = "port_owner_mismatch"
	ReconcileTargetReasonRegistryUnavailable      = "registry_unavailable"
)

// ReconcileTargetSettlement is present exactly when ReconcileArgs.SettleTarget
// was requested. Ready is a positive proof for the echoed generation only; an
// incomplete or failed state retains a stable reason and any causal detail.
type ReconcileTargetSettlement struct {
	State         ReconcileTargetSettlementState `json:"state"`
	Reason        string                         `json:"reason"`
	Target        ReconcileTarget                `json:"target"`
	CurrentPID    int                            `json:"current_pid,omitempty"`
	PIDGeneration int                            `json:"pid_generation,omitempty"`
	Error         string                         `json:"error,omitempty"`
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

	// SerenaOrphansRepaired / SerenaOrphansDeferred surface the serena
	// registry/intent self-heal (RepairSerenaIntentFromRegistry /
	// PreviewSerenaIntentRepairFromRegistry) computed in THIS SAME reconcile
	// pass — in BOTH dry-run and apply mode (BLOCKING 3 fix,
	// mcphub-register-intent REVISE round 2). Previously this repair only ever
	// ran (and was only ever visible) in apply mode, so a dry-run reconcile
	// could never show an operator what orphaned serena workspaces the very
	// next `--apply` was about to silently materialize. SerenaOrphansRepaired
	// is the count APPENDED in apply mode, or the count that WOULD be appended
	// in dry-run mode (identical classification either way — see
	// PreviewSerenaIntentRepairFromRegistry's doc comment). SerenaOrphansDeferred
	// names workspace keys whose orphan could not be repaired this pass (a
	// first-introduce-crash guard or a legacy nil-spec row) — same meaning in
	// both modes.
	SerenaOrphansRepaired int      `json:"serena_orphans_repaired"`
	SerenaOrphansDeferred []string `json:"serena_orphans_deferred,omitempty"`

	// SerenaRepairOutcome is the typed terminal result of the repair/preview
	// pass. It is additive on the wire: a newer client treats an absent or
	// unrecognized value from an older supervisor as incomplete rather than as
	// completed, while an older client ignores this field. See
	// SerenaIntentRepairOutcome for the stable vocabulary.
	SerenaRepairOutcome    SerenaIntentRepairOutcome      `json:"serena_repair_outcome"`
	SerenaRepairIncomplete []SerenaIntentRepairIncomplete `json:"serena_repair_incomplete,omitempty"`
	SerenaRepairRecovered  []SerenaIntentRepairRecovery   `json:"serena_repair_recovered,omitempty"`

	// SerenaRepairError carries the self-heal's own failure text when the
	// repair (apply) or preview (dry-run) could not COMPLETE — a malformed
	// serena catalog, a dynamic-pool fan-out the manifest shape rejected, an
	// intent write that hit I/O. Empty means the self-heal ran to a verdict.
	//
	// It exists because a failed repair is INVISIBLE in every other field of
	// this response: the orphan never reaches supervisor-intent.json, so it is
	// absent from Drift too, and DriftCount==0 + AppliedCount==0 reads exactly
	// like a healthy "no drift" pass while the registered workspace stays
	// unusable. The handler deliberately does NOT fail the whole reconcile over
	// it (the drift report is still valid, and `mcphub stop` / `mcphub restart`
	// dispatch apply-mode reconciles whose real work must not be blocked by an
	// unrelated serena row) — so representing it here is what keeps the caller
	// from reading silence as success. `mcphub reconcile` prints it and, in
	// --apply mode, exits non-zero on it.
	SerenaRepairError string `json:"serena_repair_error,omitempty"`

	// TargetSettlement is emitted only for an explicitly requested
	// SettleTarget. Its absence therefore preserves the old wire shape.
	TargetSettlement *ReconcileTargetSettlement `json:"target_settlement,omitempty"`
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

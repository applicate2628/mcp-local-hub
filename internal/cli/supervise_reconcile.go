// Package cli — Task 7.1 reconcile loop diff primitive.
//
// Spec §"Reconcile loop" + plan Task 7.1.
//
// The supervisor process (Task 6.1) holds two intent inputs:
//
//   - supervisor-intent.json — the authoritative list of daemons that
//     SHOULD be running, with full descriptor (command, args, env,
//     port, workspace). Written by `mcphub install` / manifest-edit
//     paths.
//   - daemon-intent.json — the per-daemon override file carrying
//     user-stop (TTL), user-disabled (permanent), and chronic-failure
//     (watchdog quarantine) decisions. Read with `DaemonIntent.IsActiveStop`
//     which encodes the full TTL + clock-skew + stale-bound decision
//     tree (`internal/api/daemon_intent.go:289-324`).
//
// `Reconciler.Reconcile` is the pure diff step:
//
//   - in intent + NOT IsActiveStop + NOT running → spawn (state-machine
//     `EvStart` fan-out)
//   - in intent + IsActiveStop + running        → terminate (state-machine
//     `EvIntentUpdate{stopped}` fan-out)
//   - in intent + IsActiveStop + NOT running    → no-op (quarantine respected)
//   - in intent + NOT IsActiveStop + running    → no-op (steady state)
//
// Orphans (running but NOT in intent) are intentionally NOT terminated
// here — Reconciler has no daemon descriptor to fan a terminate
// signal through. Cold-start reaper Task 13.1 owns orphan handling
// via the supervisor-state.json `Daemons` map which carries last-known
// PID + canonical task name for every supervised child.
//
// Pure separation: no I/O, no file reads, no clock calls. `Reconcile`
// receives the parsed intent files and a snapshot of currently-running
// task names, and returns nothing — side effects flow through the
// caller-supplied SpawnFunc / TerminateFunc closures. Tests inject
// recording closures; production wires them into the FIFO event loop
// (`internal/api.NewEventLoop`) which routes through the per-daemon
// state machine (`internal/api/supervisor_state_machine.go`).
package cli

import (
	"time"

	"mcp-local-hub/internal/api"
)

// SpawnFunc starts a daemon child. Returns the spawn error so the
// reconciler can record it (currently swallowed; a follow-up task may
// surface per-daemon failures through the audit log).
type SpawnFunc func(d api.SupervisorDaemon) error

// TerminateFunc signals a running daemon child to stop. Returns the
// termination error.
type TerminateFunc func(d api.SupervisorDaemon) error

// Reconciler diffs intent against currently-running daemons and fans
// out spawn/terminate actions. Construction is via NewReconciler; the
// zero value is intentionally not usable because nil fan-out funcs
// would silently swallow every reconcile decision.
//
// Phase A.2 wiring: when EventLoop is non-nil, the reconciler posts
// EvStart onto the loop instead of calling spawn directly. The
// controller's handleLoopEvent then routes through the formal
// api.Transition state machine, builds an SMContext from the cached
// daemon-intent + tracker state, and dispatches the spawn via
// executeSideEffect. Tests that don't wire a controller pass a nil
// EventLoop and fall back to the direct r.spawn call (no behavior
// change for the existing reconciler tests).
type Reconciler struct {
	spawn     SpawnFunc
	terminate TerminateFunc

	// EventLoop is the supervisor's FIFO event loop. When non-nil,
	// the reconciler posts EvStart events onto the loop for the
	// "spawn" branch of the diff. nil falls back to direct r.spawn
	// invocation (the pre-A.2 behavior preserved for tests).
	EventLoop *api.EventLoop

	// Events is the supervisor audit log. When non-nil, the
	// desired-set construction emits a single warn event for each
	// legacy nil-RuntimeSpec serena-proxy row it EXCLUDES from the
	// spawn-desired set (bot PR #246 r2 P2). Left nil by tests that
	// don't assert the audit row; production wiring sets it in
	// runSupervise alongside EventLoop.
	Events *api.SupervisorEventLog

	// LSPRegistryHasRow reports whether an LSP workspace-proxy descriptor
	// still has a backing registry row (a (workspace_key, language) entry in
	// workspaces.yaml). When non-nil, an LSP workspace-proxy descriptor whose
	// predicate returns false is EXCLUDED from the spawn-desired set instead of
	// spawned — the proxy would otherwise exit 1 with "not registered"
	// (internal/cli/daemon_workspace.go) and churn through restart backoff into
	// quarantine. This closes the orphaned-LSP-daemon quarantine bug where
	// `mcphub workspace unregister` removed the registry row but left the intent
	// descriptor behind. Left nil by tests that don't exercise the orphan path
	// (preserving the existing spawn-everything behavior); production wiring sets
	// it in runSupervise alongside EventLoop + Events.
	LSPRegistryHasRow func(d api.SupervisorDaemon) bool
}

// NewReconciler builds a Reconciler with the supplied fan-out
// closures. Both must be non-nil; tests can supply no-op closures
// to exercise just one side of the diff. EventLoop is left nil; the
// caller (runSupervise in Phase A.2 wiring) sets it after
// construction so the reconciler routes through the controller's
// state machine.
func NewReconciler(spawn SpawnFunc, terminate TerminateFunc) *Reconciler {
	return &Reconciler{spawn: spawn, terminate: terminate}
}

// Reconcile applies one diff pass against the supplied intent and
// running snapshot.
//
// Inputs:
//
//   - intent          — parsed supervisor-intent.json (the daemons we want)
//   - daemonIntent    — parsed daemon-intent.json (user-stop / chronic-failure
//     overrides). May be nil — caller treats missing as "no overrides".
//   - currentRunning  — map of canonical task_name → true for daemons
//     currently running under the supervisor. Caller (the lifecycle
//     side of supervise.go) is the source of truth.
//   - now             — reference time threaded into `DaemonIntent.IsActiveStop`
//     for TTL + clock-skew checks. Tests inject a fixed clock; production
//     passes `time.Now().UTC()`.
//
// The function is intentionally side-effect-free outside the supplied
// fan-out closures: it returns nothing, swallows per-daemon spawn /
// terminate errors (the closures themselves own the error path), and
// never mutates its inputs.
func (r *Reconciler) Reconcile(
	intent *api.SupervisorIntentFile,
	daemonIntent *api.DaemonIntentFile,
	currentRunning map[string]bool,
	now time.Time,
) {
	if intent == nil {
		return
	}
	for _, d := range intent.Daemons {
		// daemon-intent uses canonical leading-backslash keys; the
		// SupervisorDaemon.TaskName field is canonical by construction
		// (writer guarantees the leading backslash per
		// internal/api/daemon_intent.go:271-283 NormalizeTaskName).
		isStopped := false
		if daemonIntent != nil {
			entry, ok := daemonIntent.Tasks[d.TaskName]
			if ok {
				stopped, _ := entry.IsActiveStop(now)
				isStopped = stopped
			}
		}

		running := currentRunning[d.TaskName]

		// Desired-set exclusion for legacy nil-RuntimeSpec serena-proxy rows
		// (bot PR #246 r2 P2). A serena-proxy descriptor whose RuntimeSpec is
		// nil is a PRE-REDESIGN row (the fan-out before RuntimeSpec existed
		// ended its Args at --port with no --task-name and no runtime_spec).
		// After a binary upgrade the supervisor keeps these old rows until a
		// re-install re-materializes the intent. The redesigned serena-proxy
		// CANNOT make a nil-spec row work — it fails loud (no manifest fallback,
		// which would re-introduce the embed-shadow defect this redesign kills).
		//
		// r1 expressed this skip as `return nil` INSIDE the spawn closure, but
		// the controller's executeSideEffect treats a nil spawn error as SUCCESS:
		// it posts EvHealthOK and transitions StSpawning → StRunning, leaving a
		// PHANTOM running daemon (no process started) in supervisor-state.json +
		// IPC status. A "skip" expressed as a successful spawn is wrong.
		//
		// The correct skip is to EXCLUDE the row from the SPAWN-desired set HERE,
		// before any EvStart / spawn fires: the controller never sees EvStart for
		// it, so it is never spawned, never marked running, and never churns
		// restart backoff/quarantine. We emit ONE operator-actionable warn at the
		// exclusion point (not per spawn). SMOOTH auto-re-materialization of such
		// rows on `install --upgrade` is the cutover phase's §7.1 upgrade-gate
		// responsibility; the reconciler only excludes + signals. (Spec-bearing
		// serena-proxy rows are NOT excluded and spawn normally; the spawn closure
		// still injects MCPHUB_SUPERVISOR_INTENT_PATH for them. Global daemons
		// have a legitimately-nil RuntimeSpec and are NOT serena-proxy rows, so
		// they are NOT excluded.)
		//
		// The exclusion is SPAWN-ONLY: it is gated on !running so it cannot
		// strand an ALREADY-RUNNING legacy row (bot PR #246 r2 P2). If such a row
		// is running — e.g. a warm restart hydrated it from supervisor-state.json,
		// or it was spawned by a pre-redesign supervisor before the upgrade — and
		// daemon-intent.json marks it stopped, it MUST fall through to the
		// `isStopped && running` terminate branch below; suppressing that path too
		// would mean an operator stop/quarantine could never stop the live
		// process until the row is re-materialized or removed.
		if isSerenaProxyDescriptor(d) && d.RuntimeSpec == nil && !running {
			if r.Events != nil {
				_ = r.Events.Emit(api.SupervisorEvent{
					Severity: "warn",
					Source:   "lifecycle",
					Event:    "legacy-serena-descriptor-skipped",
					TaskName: d.TaskName,
					Body: map[string]any{
						"server": d.Server,
						"reason": "serena-proxy descriptor carries no runtime_spec (pre-redesign / stale row); excluded from the reconcile spawn-desired set instead of spawning a proxy that would fail loud and churn through restart backoff",
						"action": "run the serena dynamic-pool re-install/migrate to re-materialize this descriptor with a runtime_spec",
					},
				})
			}
			continue
		}

		// Desired-set exclusion for ORPHANED LSP workspace-proxy rows.
		// `mcphub workspace unregister` (and `--backend` variants) can remove
		// the registry row in workspaces.yaml WITHOUT removing the paired
		// supervisor-intent descriptor. The reconcile then spawns the now-
		// unbacked LSP proxy, which loads the registry, misses its
		// (workspace_key, language) row, and exits 1 with "not registered"
		// (internal/cli/daemon_workspace.go). The state machine treats that as
		// a real failure, churns through restart backoff, and finally
		// quarantines the daemon — a noisy, operator-confusing failure for a
		// descriptor that should simply not exist anymore.
		//
		// Mirror the serena-proxy skip above: EXCLUDE the row from the SPAWN-
		// desired set HERE (before any EvStart / spawn fires) when the predicate
		// reports no backing registry row. Gated on !running so an ALREADY-
		// RUNNING orphan still falls through to the `isStopped && running`
		// terminate branch (an operator stop must still be able to stop a live
		// process). The predicate is nil in tests that don't wire it, preserving
		// the spawn-everything default. Emit ONE operator-actionable warn at the
		// exclusion point so the orphan is visible without spawn-and-quarantine
		// churn; the operator clears it by re-running `mcphub install` /
		// re-registering the workspace (which re-materializes the row) or by
		// removing the stale descriptor.
		if r.LSPRegistryHasRow != nil && isLSPWorkspaceProxyDescriptor(d) && !running && !r.LSPRegistryHasRow(d) {
			// Single-owner emit shared with the apply-mode IPC drift classifier
			// (supervise_reconcile_ipc.go) so the two orphan-exclusion paths can
			// never diverge on the operator-facing message / remediation.
			emitOrphanedLSPDescriptorSkipped(r.Events, d)
			continue
		}

		switch {
		case !isStopped && !running:
			// Phase A.2: post EvStart through the controller's
			// event loop instead of calling spawn directly. The
			// controller routes through api.Transition which
			// honors the per-task SM state, sliding-window
			// failure count, and graceful-exit flag the bare
			// r.spawn call couldn't observe. EventLoop is nil
			// for legacy tests that never wired a controller;
			// fall back to direct r.spawn there.
			if r.EventLoop != nil {
				r.EventLoop.Post(api.LoopEvent{
					Kind:     api.EvStart,
					TaskName: d.TaskName,
				})
			} else {
				_ = r.spawn(d)
			}
		case isStopped && running:
			_ = r.terminate(d)
		}
		// !isStopped && running  → steady state, no-op
		// isStopped  && !running → quarantine respected, no-op
	}
	// Orphans (in currentRunning but NOT in intent) are NOT handled
	// here — Reconciler has no descriptor to terminate them through.
	// Task 13.1 cold-start reaper owns orphan cleanup via the
	// supervisor-state.json `Daemons` map.
}

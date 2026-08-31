package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
)

// respawnLateBindings carries the spawn/terminate closures that the
// `respawn` IPC handler needs but which are constructed AFTER the
// ipcDispatchDeps struct (the dispatcher accept-loop starts inside
// `if !noIPC` before makeProductionSpawnFnWithStatePath / Terminate
// helpers are built). The plan v5 calls this "option (b): late-binding
// closure" — preserves the existing IPC accept-loop startup ordering
// while still letting the respawn handler reach the production
// spawn/terminate plumbing once it exists.
//
// Reads and writes are RWMutex-guarded so the dispatcher accept-loop
// goroutine (reading) never tears against the supervisor startup
// goroutine (writing). The struct is shared by pointer so updates from
// runSupervise after deps construction become visible to handlers.
type respawnLateBindings struct {
	mu          sync.RWMutex
	spawnFn     SpawnFunc
	terminateFn TerminateFunc
}

// Get returns the currently-bound spawn/terminate closures, or (nil, nil)
// if they have not been wired yet (supervisor still starting). The
// caller MUST handle nil to surface a SUPERVISOR_STARTING error to the
// IPC client rather than nil-deref.
func (r *respawnLateBindings) Get() (SpawnFunc, TerminateFunc) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.spawnFn, r.terminateFn
}

// Set wires the spawn/terminate closures. Called once during supervisor
// startup after makeProductionSpawnFnWithStatePath + Terminate helpers
// are constructed.
func (r *respawnLateBindings) Set(s SpawnFunc, t TerminateFunc) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.spawnFn = s
	r.terminateFn = t
	r.mu.Unlock()
}

// IPC error codes for the respawn handler. Kept as constants so the
// GUI handler at /api/daemon/respawn can map them to HTTP status
// codes (UNKNOWN_TASK → 400, QUARANTINED → 409, RESPAWN_FAILED → 500).
const (
	ipcErrorUnknownTask              = "UNKNOWN_TASK"
	ipcErrorRespawnQuarantined       = "QUARANTINED"
	ipcErrorRespawnFailed            = "RESPAWN_FAILED"
	ipcErrorRespawnNotReady          = "RESPAWN_NOT_READY"
	ipcErrorRespawnTerminateFailed   = "RESPAWN_TERMINATE_FAILED"
	ipcErrorRespawnRefusedIntentStop = api.RespawnRefusedIntentStoppedCode
	ipcErrorRespawnStopSettlement    = "RESPAWN_STOP_SETTLEMENT_INCOMPLETE"
)

// errIdleRespawnRefusedIntentStopped is the typed sentinel the controller
// completes an idle-respawn event with when the SM refuses the spawn
// because the supervisor-intent.json stops sub-block records Desired=stopped
// (Phase 4-E2: the sole stop source; the SM side string
// is "RESTART_REFUSED_INTENT_STOPPED"). handleRespawn matches it via
// errors.Is to surface the DISTINCT ipcErrorRespawnRefusedIntentStop code
// instead of the generic RESPAWN_FAILED, so the restart caller can
// recover by writing Desired=running and retrying once (#279 fable N1).
var errIdleRespawnRefusedIntentStopped = errors.New("idle respawn refused: daemon-intent says Desired=stopped")

func shouldRouteNonRunningRespawnThroughController(ctrl *supervisorController, taskName string) (bool, error) {
	if ctrl == nil || ctrl.eventLoop == nil {
		return false, nil
	}
	smState, _ := ctrl.GetSMState(taskName)
	switch smState {
	case api.StIdle, api.StBackoffWaiting, api.StQuarantined:
		return true, nil
	case api.StSpawning, api.StRunning, api.StExiting:
		return false, fmt.Errorf("controller state %s is not directly spawnable without a live PID", smState)
	default:
		return false, fmt.Errorf("unknown controller state %q", smState)
	}
}

// handleRespawn implements the `respawn` IPC verb. Body shape:
//
//	{"id": N, "cmd": "respawn", "args": {"task_name": "\\mcp-local-hub-foo-default", "force": false}}
//
// Behaviour (per spec v4 §"Respawn from GUI"):
//
//   - task_name absent / unknown to current intent → UNKNOWN_TASK.
//   - daemon in `quarantine` state AND force == false → QUARANTINED
//     (the operator must pass force: true to bypass).
//   - daemon found, state OK or force-overridden → graceful terminate
//     via the production TerminateFunc (which marks the runtime tracker
//     entry as exited), then spawn via the production SpawnFunc with
//     the current intent + overlay merged at spawn time.
//   - Emits `supervisor-respawn-via-gui` (info) on success,
//     `supervisor-respawn-graceful-timeout` (warn) when the terminate
//     phase exceeds the graceful budget, and
//     `supervisor-respawn-refused-quarantined` (info) on QUARANTINED
//     refusal so the audit log captures the operator's intent without
//     the force escape hatch.
//
// Single-frame response: id echoed, Result on success, Error on
// failure, Final: true.
//
// The handler resolves the daemon descriptor from `deps.intent` because
// supervisor-intent.json is the canonical truth for which daemons the
// operator wants running; the runtime tracker carries the live state
// but a freshly-stopped daemon's intent entry persists across runtime
// transitions.
func handleRespawn(conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error {
	taskNameRaw, _ := req.Args["task_name"].(string)
	if taskNameRaw == "" {
		return writeIPCFrame(conn, api.IPCResponse{
			ID:    req.ID,
			Error: &api.IPCErr{Code: "INVALID_ARGS", Message: "task_name is required"},
			Final: true,
		})
	}
	force, _ := req.Args["force"].(bool)
	taskName := daemon_env_overlay.NormalizeOverlayKey(taskNameRaw)
	// The readiness token is acquired before touching provider/cache/tracker
	// state. Until the supervisor has published its fully wired controller and
	// safety path, respawn is unavailable rather than partially routed.
	spawnFn, terminateFn := deps.respawnLate.Get()
	if spawnFn == nil || terminateFn == nil {
		return writeIPCFrame(conn, api.IPCResponse{
			ID:    req.ID,
			Error: &api.IPCErr{Code: ipcErrorRespawnNotReady, Message: "supervisor still starting; respawn not yet wired", Retryable: true},
			Final: true,
		})
	}

	// Resolve the daemon descriptor. Prefer the controller's live intent
	// cache (refreshed by IntentWatcher on supervisor-intent.json mtime
	// changes) over deps.intent which is the startup snapshot and never
	// refreshed (closes bot PR#222 P2-2: after intent reload — add /
	// remove / replace daemon entries — the respawn handler would reject
	// valid tasks as UNKNOWN_TASK or act on stale descriptors during
	// long-lived supervisor sessions).
	//
	// We accept BOTH bare and canonical forms so a hand-edited
	// supervisor-intent.json (rare but possible) still matches.
	var desc *api.SupervisorDaemon
	var ctrl *supervisorController
	if deps.controllerProvider != nil {
		if ctrl = deps.controllerProvider(); ctrl != nil && ctrl.intentCache != nil {
			// Conc-F7 (PR #268 deep-sec P3): resolve through the single-owner
			// IntentCache.LookupCanonical (exact key + bare/canonical backslash
			// toggle) instead of re-implementing the scan inline. Semantically
			// identical to the old NormalizeOverlayKey loop here — both match a
			// descriptor whose TaskName equals taskName modulo the leading
			// backslash — but without duplicating the lookup logic.
			//
			// NOTE this deliberately does NOT close the staleness the bug doc
			// names: LookupCanonical reads the SAME ctrl.intentCache.snap the
			// inline loop did, so a descriptor edited on disk since the last 60s
			// IntentWatcher refresh is still seen stale here. That window is
			// sub-second on an operator-initiated respawn; closing it would cost
			// a fresh disk read per respawn, so it stays documented, not fixed.
			desc, _ = ctrl.intentCache.LookupCanonical(taskName)
		}
	}
	if desc == nil && deps.intent != nil {
		// Fallback: startup snapshot (legacy path; unit-test fixtures
		// often construct deps WITHOUT a controllerProvider).
		for i := range deps.intent.Daemons {
			d := &deps.intent.Daemons[i]
			if daemon_env_overlay.NormalizeOverlayKey(d.TaskName) == taskName {
				desc = d
				break
			}
		}
	}
	if desc == nil {
		return writeIPCFrame(conn, api.IPCResponse{
			ID:    req.ID,
			Error: &api.IPCErr{Code: ipcErrorUnknownTask, Message: "task_name not in supervisor-intent.json: " + taskNameRaw},
			Final: true,
		})
	}
	if !deps.allowDirectRespawnForTest && (ctrl == nil || ctrl.eventLoop == nil) {
		return writeIPCFrame(conn, api.IPCResponse{
			ID:    req.ID,
			Error: &api.IPCErr{Code: ipcErrorRespawnNotReady, Message: "supervisor controller is not ready; retry respawn", Retryable: true},
			Final: true,
		})
	}
	// Operator retry is a recovery trigger, not a direct spawn bypass.  Resolve
	// any exact pending stop receipt before deciding whether this request may
	// create a replacement process.
	if ctrl != nil {
		recoveryCtx, cancel := context.WithTimeout(baseReconcileContext(deps), api.DefaultTargetedReconcileTimeout)
		ctrl.enqueuePendingStopSettlementRecovery(recoveryCtx)
		cancel()
	}

	// bot PR #246 r2 P2-1: a legacy nil-RuntimeSpec serena-proxy descriptor
	// (a pre-redesign row left in supervisor-intent.json before RuntimeSpec
	// existed) cannot be respawned — the redesigned serena-proxy fails loud on a
	// nil spec (no manifest fallback, by design). The RECONCILER excludes such
	// rows from its spawn-desired set (supervise_reconcile.go), but THIS IPC
	// respawn path resolves the descriptor directly from the intent cache and
	// calls spawnFn itself, so it must apply the SAME exclusion here. Without it,
	// an operator/GUI respawn of an idle legacy row would cmd.Start a doomed
	// proxy that exits non-zero → crashCh → restart-policy backoff/quarantine —
	// the exact churn the reconcile exclusion exists to avoid. Refuse with a
	// clear, operator-actionable error instead of spawning.
	if isSerenaProxyDescriptor(*desc) && desc.RuntimeSpec == nil {
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:    ipcErrorRespawnFailed,
				Message: "legacy serena-proxy descriptor for " + taskNameRaw + " carries no runtime_spec (pre-redesign / stale row) and cannot be respawned; re-install the serena dynamic pool to re-materialize its runtime_spec, then retry",
			},
			Final: true,
		})
	}

	// State check uses the runtime tracker's canonical key form
	// (leading-backslash). NormalizeOverlayKey already produces that
	// form so the lookup is symmetric.
	state := daemonRuntimeStateIdle
	shouldTerminate := true
	if deps.runtimeTracker != nil {
		if entry, ok := deps.runtimeTracker.Get(taskName); ok {
			state = entry.State
			shouldTerminate = entry.CurrentPID > 0
		} else {
			shouldTerminate = false
		}
	}
	controllerState := api.StIdle
	controllerStateKnown := false
	if ctrl != nil {
		controllerState, controllerStateKnown = ctrl.GetSMState(taskName)
	}
	if controllerStateKnown {
		switch controllerState {
		case api.StIdle, api.StBackoffWaiting, api.StQuarantined:
			shouldTerminate = false
		}
	}
	if (state == daemonRuntimeStateQuarantine || (controllerStateKnown && controllerState == api.StQuarantined)) && !force {
		_ = deps.events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "ipc",
			Event:    "supervisor-respawn-refused-quarantined",
			Body: map[string]any{
				"task_name": taskName,
			},
		})
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:    ipcErrorRespawnQuarantined,
				Message: "daemon is quarantined; pass force=true to override",
			},
			Final: true,
		})
	}

	// An idle controller row is not enough to authorize a new spawn. A prior
	// crash or lost-child defect can leave CurrentPID cleared while the old
	// supervisor-owned root still owns its listener. Before the idle respawn
	// path posts a spawn, require the same controller-owned stop settlement the
	// CLI stop path consumes: FIFO processing complete, no tracked child, and
	// the exact listener free. Running/exiting restarts stay on their existing
	// controller terminate->child-exit->spawn path below.
	if !shouldTerminate && ctrl != nil && ctrl.eventLoop != nil {
		if port, portOK := api.EffectiveDaemonPort(*desc); portOK && port > 0 {
			settleCtx, cancel := context.WithTimeout(baseReconcileContext(deps), 5*time.Second)
			settlements := ctrl.settleStopBatchTargets(settleCtx, []api.StopBatchTargetV1{{TaskName: taskName, ExpectedPort: port}})
			cancel()
			settlement := api.StoppedSettlement{TaskName: taskName, State: api.StoppedSettlementIncomplete, Reason: api.StoppedSettlementReasonIdentityUnverified, Error: "stop settlement result missing"}
			if len(settlements) == 1 {
				settlement = settlements[0]
			}
			if settlement.State != api.StoppedSettlementStopped || settlement.Reason != api.StoppedSettlementReasonStopped {
				return writeIPCFrame(conn, api.IPCResponse{
					ID: req.ID,
					Error: &api.IPCErr{
						Code:      ipcErrorRespawnStopSettlement,
						Message:   fmt.Sprintf("respawn blocked until prior stop settles: %s: %s", settlement.Reason, settlement.Error),
						Retryable: settlement.State == api.StoppedSettlementIncomplete,
					},
					Final: true,
				})
			}
		}
	}

	const gracefulTimeoutMs = 5000

	// Running-daemon respawn routes THROUGH the controller's state machine
	// (Codex bot #268 P1, supervise_respawn.go:308). The controller is now
	// the only place that records ownSpawned/reaperOutstanding and advances
	// the SM; a direct terminate+spawn here would leave the replacement
	// invisible to those maps AND let the old child's late EvChildExit drive
	// backoff/respawn over the fresh PID. Driving StRunning -> EvManualRestart
	// -> StExiting -> terminate -> (observe exit) -> StSpawning on the single
	// FIFO loop serializes terminate before respawn, closing that race. The
	// controller's terminate/spawn closures are the SAME ones respawnLate
	// holds, so spawn semantics are identical; we wait for the SM to re-fire
	// the spawn (StRunning with a bumped PIDGeneration) within the graceful
	// budget to keep the synchronous IPC RespawnResult contract.
	//
	// Gated on a KNOWN controller state of StRunning OR StExiting. StRunning is
	// the steady-state "Apply + Restart" case (own-spawn sets it; warm-start
	// PIDs are seeded by hydrateControllerRunningStates at startup). StExiting
	// is the in-flight stop/restart case — the operator clicks Restart while a
	// terminate is already in progress and the tracker still holds a live
	// CurrentPID: EvManualRestart at StExiting COALESCES queued_action=respawn
	// through the SM, so it must ALSO stay on the controller path. A direct
	// terminate+spawn there would not be recorded as the controller's spawn,
	// and the old child's later EvChildExit could drive StExiting->StIdle +
	// MarkExited, clearing the tracker for the freshly spawned process and
	// leaving a live daemon untracked (Codex bot #268 P1). Only StSpawning
	// (EvManualRestart has no SM transition there) and the ctrl-not-yet-wired
	// window fall through to the legacy direct path below.
	if shouldTerminate && ctrl != nil && ctrl.eventLoop != nil &&
		controllerStateKnown &&
		(controllerState == api.StRunning || controllerState == api.StExiting) {
		if err := ctrl.postManualRestartAndWaitRunning(taskName, time.Duration(gracefulTimeoutMs)*time.Millisecond); err != nil {
			_ = deps.events.Emit(api.SupervisorEvent{
				Severity: "error",
				Source:   "ipc",
				Event:    "supervisor-respawn-controller-restart-failed",
				Body: map[string]any{
					"task_name": taskName,
					"err":       err.Error(),
				},
			})
			return writeIPCFrame(conn, api.IPCResponse{
				ID: req.ID,
				Error: &api.IPCErr{
					Code:    ipcErrorRespawnFailed,
					Message: fmt.Sprintf("controller-routed restart failed: %v", err),
				},
				Final: true,
			})
		}
		_ = deps.events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "ipc",
			Event:    "supervisor-respawn-via-gui",
			Body: map[string]any{
				"task_name": taskName,
				"force":     force,
				"outcome":   "spawned",
				"route":     "controller-manual-restart",
			},
		})
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			OK: true,
			Result: map[string]any{
				"task_name": taskName,
				"state":     "spawned",
				"route":     "controller-manual-restart",
			},
			Final: true,
		})
	}

	// Graceful terminate. terminateFn marks the runtime tracker entry
	// as exited and (for production wiring) signals the child process.
	// We don't poll for "really exited" beyond the terminateFn return —
	// the production tracker write happens inside terminateFn and the
	// next spawnFn call will observe the new state.
	termStart := time.Now()
	if shouldTerminate {
		if err := terminateFn(*desc); err != nil {
			// Terminate failure ABORTS the respawn (closes bot PR#222 P2-4:
			// previously we logged warn + continued to spawn unconditionally,
			// which could leave the old process alive while starting another
			// instance — duplicate-process / port-collision risk under
			// production wiring where terminate can fail for active daemons
			// when no PID is available in the startup snapshot).
			//
			// The handler returns the terminate error to the IPC caller so
			// the operator sees the precise failure mode instead of a
			// silent "respawn succeeded" + a port-conflict crash on the
			// next health check.
			_ = deps.events.Emit(api.SupervisorEvent{
				Severity: "error",
				Source:   "ipc",
				Event:    "supervisor-respawn-terminate-failed",
				Body: map[string]any{
					"task_name": taskName,
					"err":       err.Error(),
					"action":    "respawn aborted",
				},
			})
			return writeIPCFrame(conn, api.IPCResponse{
				ID: req.ID,
				Error: &api.IPCErr{
					Code:    ipcErrorRespawnTerminateFailed,
					Message: "terminate failed; respawn aborted to prevent duplicate process: " + err.Error(),
				},
				Final: true,
			})
		}
	}
	if time.Since(termStart) > gracefulTimeoutMs*time.Millisecond {
		_ = deps.events.Emit(api.SupervisorEvent{
			Severity: "warn",
			Source:   "ipc",
			Event:    "supervisor-respawn-graceful-timeout",
			Body: map[string]any{
				"task_name":  taskName,
				"timeout_ms": gracefulTimeoutMs,
				"elapsed_ms": time.Since(termStart).Milliseconds(),
			},
		})
	}

	var spawnErr error
	routeNonRunningRespawnThroughController := false
	if !shouldTerminate {
		// Seed the controller SM state from the persisted tracker state
		// when the controller has no entry yet (post-cold-restart window:
		// the tracker is hydrated from supervisor-state.json before the
		// controller has observed the task). Without this, a forced
		// respawn of a quarantined/backoff task would route through the
		// StIdle transition and skip the failure-window reset, letting the
		// daemon immediately re-quarantine off the stale crash window
		// (Codex bot #268 P2, supervise_respawn.go:75).
		if ctrl != nil {
			ctrl.hydrateSMStateFromTrackerIfMissing(taskName)
		}
		routeNonRunningRespawnThroughController, spawnErr = shouldRouteNonRunningRespawnThroughController(ctrl, taskName)
	}
	// Production reaches here only after both controller routes had their
	// chance: StRunning/StExiting returned above, and non-running states were
	// classified immediately above. Keep the raw closure seam test-only.
	if !deps.allowDirectRespawnForTest && !routeNonRunningRespawnThroughController {
		return writeIPCFrame(conn, api.IPCResponse{
			ID:    req.ID,
			Error: &api.IPCErr{Code: ipcErrorRespawnNotReady, Message: "controller cannot accept this respawn state yet; retry", Retryable: true},
			Final: true,
		})
	}
	if spawnErr == nil {
		if routeNonRunningRespawnThroughController {
			spawnErr = ctrl.postIdleRespawnAndWait(taskName, time.Duration(gracefulTimeoutMs)*time.Millisecond)
		} else {
			spawnErr = spawnFn(*desc)
		}
	}
	if spawnErr != nil {
		// A stopped-intent refusal is a DISTINCT, recoverable outcome —
		// not a generic spawn failure. The SM refused the spawn because
		// the supervisor-intent.json stops sub-block still records
		// Desired=stopped (Phase 4-E2: the sole stop source); the restart
		// caller resolves it by writing Desired=running and retrying once.
		// Surface the distinct code so it never gets conflated with a real
		// RESPAWN_FAILED nor accidentally bypasses the QUARANTINED
		// force-gate (#279 fable N1).
		if errors.Is(spawnErr, errIdleRespawnRefusedIntentStopped) {
			return writeIPCFrame(conn, api.IPCResponse{
				ID: req.ID,
				Error: &api.IPCErr{
					Code:    ipcErrorRespawnRefusedIntentStop,
					Message: "respawn refused: the supervisor-intent.json stops sub-block records Desired=stopped; write Desired=running first (mcphub restart re-asserts it) then retry",
				},
				Final: true,
			})
		}
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:    ipcErrorRespawnFailed,
				Message: fmt.Sprintf("spawn failed: %v", spawnErr),
			},
			Final: true,
		})
	}

	_ = deps.events.Emit(api.SupervisorEvent{
		Severity: "info",
		Source:   "ipc",
		Event:    "supervisor-respawn-via-gui",
		Body: map[string]any{
			"task_name": taskName,
			"force":     force,
			"outcome":   "spawned",
		},
	})

	return writeIPCFrame(conn, api.IPCResponse{
		ID: req.ID,
		OK: true,
		Result: map[string]any{
			"task_name": taskName,
			"state":     "spawned",
		},
		Final: true,
	})
}

package cli

import (
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
	ipcErrorUnknownTask            = "UNKNOWN_TASK"
	ipcErrorRespawnQuarantined     = "QUARANTINED"
	ipcErrorRespawnFailed          = "RESPAWN_FAILED"
	ipcErrorRespawnNotReady        = "RESPAWN_NOT_READY"
	ipcErrorRespawnTerminateFailed = "RESPAWN_TERMINATE_FAILED"
)

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
	if deps.controllerProvider != nil {
		if ctrl := deps.controllerProvider(); ctrl != nil && ctrl.intentCache != nil {
			if snap, ok := ctrl.intentCache.snap.Load().(*intentSnapshot); ok && snap != nil && snap.intent != nil {
				for i := range snap.intent.Daemons {
					d := &snap.intent.Daemons[i]
					if daemon_env_overlay.NormalizeOverlayKey(d.TaskName) == taskName {
						desc = d
						break
					}
				}
			}
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
	if state == daemonRuntimeStateQuarantine && !force {
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

	spawnFn, terminateFn := deps.respawnLate.Get()
	if spawnFn == nil || terminateFn == nil {
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:      ipcErrorRespawnNotReady,
				Message:   "supervisor still starting; respawn not yet wired",
				Retryable: true,
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
	const gracefulTimeoutMs = 5000
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

	if err := spawnFn(*desc); err != nil {
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:    ipcErrorRespawnFailed,
				Message: fmt.Sprintf("spawn failed: %v", err),
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

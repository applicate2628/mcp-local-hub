package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RespawnRefusedIntentStoppedCode is the distinct supervisor-side
// respawn refusal code returned when an idle daemon's respawn is
// refused because its daemon-intent.json still records Desired=stopped
// (the SM gate StIdle+EvManualRestart returns
// RESTART_REFUSED_INTENT_STOPPED — supervisor_state_machine.go). It is
// surfaced as a DISTINCT code (not the generic RESPAWN_FAILED) so the
// restart caller can tell a recoverable stopped-intent refusal — which
// it resolves by writing Desired=running and retrying once — apart from
// a genuine spawn failure or a deliberate QUARANTINED force-gate
// refusal. Wire-symmetric with cli.ipcErrorRespawnRefusedIntentStopped
// (#279 fable N1).
const RespawnRefusedIntentStoppedCode = "RESPAWN_REFUSED_INTENT_STOPPED"

// RespawnResult is the operator-facing outcome of a respawn IPC call.
// Code mirrors the supervisor-side IPC error codes so HTTP handlers
// can map them to status codes without re-parsing strings:
//
//   - "" (Success=true)              → 200
//   - "UNKNOWN_TASK"                 → 400 (task not in current intent)
//   - "QUARANTINED"                  → 409 (force=true required)
//   - "RESPAWN_NOT_READY"            → 503 (supervisor still starting)
//   - "RESPAWN_REFUSED_INTENT_STOPPED" → 409 (daemon-intent.json says Desired=stopped;
//     write Desired=running first, then retry — see restartSupervisorOwnedDaemons)
//   - "RESPAWN_FAILED"               → 500 (spawn closure returned error)
//   - "INVALID_ARGS"                 → 400 (missing task_name)
//   - "SUPERVISOR_UNAVAILABLE"       → 503 (no IPC: missing lock owner / dial failed)
//
// Message is the supervisor's human-readable explanation.
type RespawnResult struct {
	Success bool
	Code    string
	Message string
}

// DialSupervisorIPCRespawn sends a `respawn` request to the running
// supervisor via its IPC pipe and returns the structured outcome.
// Always returns a populated RespawnResult; the boolean `error` is
// reserved for transport-level failures (pipe dial, handshake) that
// don't have a supervisor-issued Code.
//
// `force=true` bypasses the supervisor's quarantine refusal; pass it
// only when the GUI explicitly surfaces a force-restart affordance to
// the operator.
//
// Mirrors the DialSupervisorIPCExit/Status pattern (same package); the
// IPC envelope shape is shared via internal types (IPCRequest, IPCResponse).
func DialSupervisorIPCRespawn(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs+5000)*time.Millisecond)
		defer cancel()
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return RespawnResult{}, fmt.Errorf("supervisor IPC respawn: resolve state dir: %w", err)
	}
	return dialSupervisorIPCRespawnFromStateDir(ctx, stateDir, taskName, force, timeoutMs)
}

func dialSupervisorIPCRespawnFromStateDir(ctx context.Context, stateDir, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	owner, err := ReadSupervisorLockOwner(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return RespawnResult{
				Code:    "SUPERVISOR_UNAVAILABLE",
				Message: fmt.Sprintf("supervisor.lock.owner.json not found at %s.owner.json", lockPath),
			}, nil
		}
		return RespawnResult{}, fmt.Errorf("supervisor IPC respawn: read %s.owner.json: %w", lockPath, err)
	}
	address := SupervisorIPCAddress(stateDir)
	conn, err := dialSupervisorIPC(ctx, address)
	if err != nil {
		if errors.Is(err, ErrSupervisorIPCUnavailable) || os.IsNotExist(err) {
			return RespawnResult{
				Code:    "SUPERVISOR_UNAVAILABLE",
				Message: fmt.Sprintf("dial %s failed: %v", address, err),
			}, nil
		}
		return RespawnResult{}, fmt.Errorf("supervisor IPC respawn: dial %s: %w", address, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		return RespawnResult{}, fmt.Errorf("supervisor IPC respawn: %w", err)
	}
	req := IPCRequest{
		Version: 1,
		ID:      time.Now().UnixNano(),
		Cmd:     "respawn",
		Args: map[string]any{
			"task_name": taskName,
			"force":     force,
		},
	}
	if err := writeSupervisorIPCRequest(conn, req); err != nil {
		return RespawnResult{}, fmt.Errorf("supervisor IPC respawn: send: %w", err)
	}
	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return RespawnResult{}, fmt.Errorf("supervisor IPC respawn: read response: %w", err)
	}
	if resp.ID != req.ID {
		return RespawnResult{}, fmt.Errorf("supervisor IPC respawn: response id mismatch: got %d want %d", resp.ID, req.ID)
	}
	if resp.Error != nil {
		return RespawnResult{
			Code:    resp.Error.Code,
			Message: resp.Error.Message,
		}, nil
	}
	if !resp.OK {
		return RespawnResult{}, fmt.Errorf("supervisor IPC respawn: non-OK response without error")
	}
	return RespawnResult{Success: true, Code: "OK"}, nil
}

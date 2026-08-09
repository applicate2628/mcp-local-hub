package api

// Phase A.3 (plan v10 §A.3, 2026-05-20) — IPC client for the `reconcile`
// verb. Mirrors DialSupervisorIPCStatus (supervisor_ipc_status_client.go)
// in shape: read supervisor.lock owner, validate hello-frame handshake,
// send a single IPCRequest, decode the response Body into the typed
// ReconcileResponse, return.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultReconcileTimeout is the operator-facing default deadline for
// `mcphub reconcile`. Plan §A.3 acceptance criterion: "returns within
// 30s OR explicit timeout error". The supervisor handler itself does
// not have a long-running phase (one scheduler.List call + a fixed
// number of EvIntentUpdate Posts onto a buffered channel), so 30s is
// a generous bound that still leaves the operator a clear bailout on
// a hung supervisor.
const DefaultReconcileTimeout = 30 * time.Second

// DefaultTargetedReconcileTimeout covers a complete targeted Serena
// settlement, including the daemon's 120-second first-bind allowance plus a
// control-plane margin equal to the ordinary reconcile budget. A targeted
// register must not time out earlier than the runtime descriptor it is waiting
// on, even after registry repair, drift computation, and IPC response work.
const DefaultTargetedReconcileTimeout = time.Duration(serenaStartupBindDeadlineSeconds)*time.Second + DefaultReconcileTimeout

// DialSupervisorIPCReconcile sends a reconcile request to the running
// supervisor and decodes the response. When apply is true the
// supervisor posts EvIntentUpdate per drift entry; when false the
// request is dry-run.
//
// The caller may supply ctx with a deadline to override the default
// 30s timeout. A nil ctx falls back to context.Background() +
// DefaultReconcileTimeout.
//
// Errors returned:
//   - ErrSupervisorIPCUnavailable wrapped — supervisor.lock.owner.json
//     missing or pipe/socket does not accept the dial. The CLI maps
//     this to an actionable "supervisor not running" message.
//   - wire / handshake / version / decode errors propagated verbatim
//     so operators see the precise failure cause in stderr.
func DialSupervisorIPCReconcile(ctx context.Context, apply bool) (ReconcileResponse, error) {
	return dialSupervisorIPCReconcile(ctx, apply, nil)
}

// DialSupervisorIPCReconcileTarget requests controller-owned settlement for
// one exact workspace generation. The existing DialSupervisorIPCReconcile
// entry point deliberately omits the additive target and retains its original
// wire shape.
func DialSupervisorIPCReconcileTarget(ctx context.Context, apply bool, target ReconcileTarget) (ReconcileResponse, error) {
	return dialSupervisorIPCReconcile(ctx, apply, &target)
}

func dialSupervisorIPCReconcile(ctx context.Context, apply bool, target *ReconcileTarget) (ReconcileResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		timeout := DefaultReconcileTimeout
		if target != nil {
			timeout = DefaultTargetedReconcileTimeout
		}
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: resolve state dir: %w", err)
	}
	return dialSupervisorIPCReconcileFromStateDirWithTarget(ctx, stateDir, apply, target)
}

// dialSupervisorIPCReconcileFromStateDir is the state-dir-injected
// inner helper so tests can target a temp directory. Production
// callers go through DialSupervisorIPCReconcile.
func dialSupervisorIPCReconcileFromStateDir(ctx context.Context, stateDir string, apply bool) (ReconcileResponse, error) {
	return dialSupervisorIPCReconcileFromStateDirWithTarget(ctx, stateDir, apply, nil)
}

func dialSupervisorIPCReconcileFromStateDirWithTarget(ctx context.Context, stateDir string, apply bool, target *ReconcileTarget) (ReconcileResponse, error) {
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	owner, err := ReadSupervisorLockOwner(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: supervisor.lock.owner.json not found at %s.owner.json: %w: %w",
				lockPath, ErrSupervisorIPCUnavailable, err)
		}
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: read %s.owner.json: %w", lockPath, err)
	}
	address := SupervisorIPCAddress(stateDir)
	conn, err := dialSupervisorIPC(ctx, address)
	if err != nil {
		if os.IsNotExist(err) {
			return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: dial %s: %w: %w", address, ErrSupervisorIPCUnavailable, err)
		}
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: dial %s: %w", address, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: %w", err)
	}
	args := map[string]any{
		"apply": apply,
	}
	if target != nil {
		args["settle_target"] = *target
	}
	req := IPCRequest{
		Version: 1,
		ID:      time.Now().UnixNano(),
		Cmd:     "reconcile",
		Args:    args,
	}
	if err := writeSupervisorIPCRequest(conn, req); err != nil {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: send request: %w", err)
	}
	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		// Ctx-deadline expiry surfaces here as a wrapped i/o timeout
		// because the connection deadline was bound from ctx above.
		// Wrap ctx.Err() so callers can use errors.Is(err, context.DeadlineExceeded).
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
			return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: %w: %w", ctxErr, err)
		}
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: read response: %w", err)
	}
	if resp.ID != req.ID {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: response id mismatch: got %d want %d", resp.ID, req.ID)
	}
	if resp.Error != nil {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: %s: %s", resp.Error.Code, resp.Error.Message)
	}
	if !resp.OK {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: non-OK response without error")
	}
	if len(resp.Result) == 0 {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: empty result body")
	}
	var out ReconcileResponse
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: decode result: %w", err)
	}
	return out, nil
}

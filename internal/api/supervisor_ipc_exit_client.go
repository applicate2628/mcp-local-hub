package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DialSupervisorIPCExit sends a graceful exit request to the running
// supervisor via its IPC pipe. Callers controlling an originally created
// generation use DialSupervisorIPCExitForOwner instead.
//
// Returns nil on a successful `graceful_exit_initiated=true` reply. Any
// failure (no supervisor lock owner sidecar, dial failure, handshake
// mismatch, non-OK response) returns a non-nil error so callers can
// fall through to force-kill.
//
// Caller MUST still wait for the supervisor process to exit after this
// returns nil — the supervisor's main goroutine begins teardown after
// writing the response frame.
func DialSupervisorIPCExit(ctx context.Context, timeoutMs int) error {
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
		return fmt.Errorf("supervisor IPC exit: resolve state dir: %w", err)
	}
	return dialSupervisorIPCExitFromStateDir(ctx, stateDir, timeoutMs)
}

func dialSupervisorIPCExitFromStateDir(ctx context.Context, stateDir string, timeoutMs int) error {
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	owner, err := ReadSupervisorLockOwner(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("supervisor IPC exit: supervisor.lock.owner.json not found at %s.owner.json: %w: %w",
				lockPath, ErrSupervisorIPCUnavailable, err)
		}
		return fmt.Errorf("supervisor IPC exit: read %s.owner.json: %w", lockPath, err)
	}
	return DialSupervisorIPCExitForOwner(ctx, stateDir, owner, timeoutMs)
}

// DialSupervisorIPCExitForOwner never resolves a replacement owner. The original
// generation is authenticated on the same connection that carries its exit.
func DialSupervisorIPCExitForOwner(ctx context.Context, stateDir string, expected SupervisorLockOwner, timeoutMs int) (retErr error) {
	return dialSupervisorIPCExitForOwner(ctx, stateDir, expected, nil, timeoutMs)
}

// DialSupervisorIPCExitForPeer validates EACH fresh exit connection against the
// independently captured original creator before sending any destructive frame.
func DialSupervisorIPCExitForPeer(ctx context.Context, stateDir string, owner SupervisorLockOwner, expected SupervisorProcessIdentityV1, timeoutMs int) error {
	return dialSupervisorIPCExitForOwner(ctx, stateDir, owner, &expected, timeoutMs)
}

func dialSupervisorIPCExitForOwner(ctx context.Context, stateDir string, expected SupervisorLockOwner, peer *SupervisorProcessIdentityV1, timeoutMs int) (retErr error) {
	if ctx == nil {
		return fmt.Errorf("supervisor IPC exit: context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	started, err := time.Parse(time.RFC3339Nano, expected.StartedAt)
	if !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir || expected.PID <= 0 || err != nil || started.IsZero() || timeoutMs <= 0 {
		return fmt.Errorf("supervisor IPC exit: explicit namespace, generation and positive grace required")
	}
	if peer != nil {
		if err := validateSupervisorControlPeerExpectation(expected, *peer); err != nil {
			return err
		}
	}
	address := SupervisorIPCAddress(stateDir)
	conn, err := dialSupervisorIPC(ctx, address)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("supervisor IPC exit: dial %s: %w: %w", address, ErrSupervisorIPCUnavailable, err)
		}
		return fmt.Errorf("supervisor IPC exit: dial %s: %w", address, err)
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("supervisor IPC exit: set deadline: %w", err)
		}
	}

	if peer != nil {
		if err := ValidateSupervisorControlPeerConnectionV1(conn, expected, *peer); err != nil {
			return err
		}
	}
	if err := validateSupervisorIPCHello(conn, expected); err != nil {
		return fmt.Errorf("supervisor IPC exit: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	req := IPCRequest{
		Version: 1,
		ID:      time.Now().UnixNano(),
		Cmd:     "exit",
		Args: map[string]any{
			"graceful":   true,
			"timeout_ms": timeoutMs,
		},
	}
	if err := writeSupervisorIPCRequest(conn, req); err != nil {
		return fmt.Errorf("supervisor IPC exit: send exit: %w", err)
	}
	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return fmt.Errorf("supervisor IPC exit: read response: %w", err)
	}
	if resp.ID != req.ID {
		return fmt.Errorf("supervisor IPC exit: response id mismatch: got %d want %d", resp.ID, req.ID)
	}
	if resp.Error != nil {
		return fmt.Errorf("supervisor IPC exit: %s: %s", resp.Error.Code, resp.Error.Message)
	}
	if !resp.OK {
		return fmt.Errorf("supervisor IPC exit: non-OK response without error")
	}
	// Verify the graceful_exit_initiated flag is set so we know the
	// supervisor actually entered the shutdown path.
	var result struct {
		GracefulExitInitiated bool `json:"graceful_exit_initiated"`
	}
	if len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return fmt.Errorf("supervisor IPC exit: decode result: %w (raw=%q)", err, string(resp.Result))
		}
	}
	if !result.GracefulExitInitiated {
		return fmt.Errorf("supervisor IPC exit: graceful_exit_initiated=false (raw=%q)", string(resp.Result))
	}
	return nil
}

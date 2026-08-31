package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SupervisorRecoveryStatusV1 is the recovery-only proof that the process
// holding supervisor.lock is also the named-pipe server which completed a
// protocol-authenticated status exchange.  ImagePath is intentionally an
// observation, not an admission predicate: a live supervisor can legitimately
// execute an alternate path while a canonical binary is replaced atomically.
type SupervisorRecoveryStatusV1 struct {
	Owner  SupervisorLockOwner
	Server SupervisorProcessIdentityV1
	Status []DaemonStatus
}

// DialSupervisorIPCRecoveryStatusV1 binds a recovery decision to one stable
// supervisor generation.  It is deliberately separate from the ordinary
// status client: the ordinary client preserves its legacy handshake contract,
// while recovery must fail closed unless sidecar, kernel pipe peer, hello, and
// status response all name the same live generation.
func DialSupervisorIPCRecoveryStatusV1(ctx context.Context, stateDir string) (SupervisorRecoveryStatusV1, error) {
	var zero SupervisorRecoveryStatusV1
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	owner, err := ReadSupervisorLockOwner(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return zero, fmt.Errorf("supervisor recovery status: owner sidecar unavailable: %w: %w", ErrSupervisorIPCUnavailable, err)
		}
		return zero, fmt.Errorf("supervisor recovery status: read owner sidecar: %w", err)
	}
	if owner.PID <= 0 || owner.StartedAt == "" {
		return zero, fmt.Errorf("supervisor recovery status: invalid owner generation")
	}
	conn, err := dialSupervisorIPC(ctx, SupervisorIPCAddress(stateDir))
	if err != nil {
		return zero, fmt.Errorf("supervisor recovery status: dial: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	server, err := ObserveSupervisorStatusServerIdentityV1(conn)
	if err != nil {
		return zero, fmt.Errorf("supervisor recovery status: kernel pipe identity: %w", err)
	}
	// Validate owns the PID and creation-time comparison.  The token, session,
	// and image values are kernel observations from this exact pipe server; the
	// image remains diagnostic so a verified old-path process is recoverable.
	if err := ValidateSupervisorStatusServerIdentityV1(owner, server, server.UserSID, server.SessionID, server.ImagePath); err != nil {
		return zero, fmt.Errorf("supervisor recovery status: generation mismatch: %w", err)
	}
	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		return zero, fmt.Errorf("supervisor recovery status: protocol authentication: %w", err)
	}
	req := IPCRequest{Version: 1, ID: time.Now().UnixNano(), Cmd: "status"}
	if err := writeSupervisorIPCRequest(conn, req); err != nil {
		return zero, fmt.Errorf("supervisor recovery status: send status: %w", err)
	}
	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return zero, fmt.Errorf("supervisor recovery status: read status: %w", err)
	}
	if resp.ID != req.ID || resp.Error != nil || !resp.OK {
		return zero, fmt.Errorf("supervisor recovery status: invalid status response")
	}
	rows, err := decodeSupervisorIPCStatusResult(resp.Result)
	if err != nil {
		return zero, fmt.Errorf("supervisor recovery status: decode status: %w", err)
	}
	// Re-read after the exchange.  A replacement while we were authenticating
	// invalidates the recovery decision even when both individual snapshots were
	// valid.
	after, err := ReadSupervisorLockOwner(lockPath)
	if err != nil {
		return zero, fmt.Errorf("supervisor recovery status: re-read owner sidecar: %w", err)
	}
	if after.PID != owner.PID || after.StartedAt != owner.StartedAt {
		return zero, fmt.Errorf("supervisor recovery status: owner generation changed during verification")
	}
	return SupervisorRecoveryStatusV1{Owner: owner, Server: server, Status: rows}, nil
}

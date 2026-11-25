package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// DialSupervisorIPCForceStop authenticates one connection, captures the target
// generation on it, and sends one force_stop command. An ambiguous response is
// never retried or converted into a PID/port kill. Native identity reads convey
// authentication facts only; the receiving Supervisor must retain the creator.
func DialSupervisorIPCForceStop(ctx context.Context, taskName string, port int) (result ForceStopResultV1, retErr error) {
	if ctx == nil {
		return result, &IPCErr{Code: ForceStopInvalidRequest, Message: "force_stop requires a context"}
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	conn, owner, err := dialSupervisorForceStopConnection(ctx)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
	return withForceStopExchangeContext(ctx, conn, func() (ForceStopResultV1, error) {
		return exchangeSupervisorForceStop(ctx, conn, owner, taskName, port)
	})
}

// RetainedSupervisorForceStop owns only a connection to the original Supervisor;
// native process authority remains in that Supervisor's original creator. The
// connection's removal capture cannot be transferred to a replacement peer.
type RetainedSupervisorForceStop struct {
	mu      sync.Mutex
	conn    net.Conn
	command ForceStopCommandV1
	nextID  int64
}

// PrepareSupervisorIPCRemovedForceStop must precede the descriptor mutation and
// must run outside its flock. Preparation does not terminate or change intent.
// The server's existing idle timeout bounds abandoned captures. An expired or
// broken connection is a refusal; callers must not redial or rediscover a PID.
func PrepareSupervisorIPCRemovedForceStop(ctx context.Context, taskName string, port int) (capture *RetainedSupervisorForceStop, retErr error) {
	if ctx == nil {
		return nil, &IPCErr{Code: ForceStopInvalidRequest, Message: "force_stop requires a context"}
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	conn, owner, err := dialSupervisorForceStopConnection(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if capture == nil {
			retErr = errors.Join(retErr, conn.Close())
		}
	}()
	var command ForceStopCommandV1
	var nextID int64
	result, err := withForceStopExchangeContext(ctx, conn, func() (ForceStopResultV1, error) {
		var err error
		command, nextID, err = readSupervisorForceStopTarget(ctx, conn, owner, taskName, port)
		if err != nil {
			return ForceStopResultV1{}, err
		}
		command.RemovalPhase = ForceStopRetainRemoved
		return exchangeSupervisorForceStopCommand(ctx, conn, nextID, command)
	})
	if err != nil {
		return nil, err
	}
	if result.State != ForceStopRetained {
		return nil, &IPCErr{Code: result.Reason, Message: result.Error}
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return &RetainedSupervisorForceStop{conn: conn, command: command, nextID: nextID + 1}, nil
}

// Complete consumes the capture once, after the descriptor commit and reconcile.
// Every outcome closes the original connection, including an ambiguous response.
func (capture *RetainedSupervisorForceStop) Complete(ctx context.Context) (result ForceStopResultV1, retErr error) {
	if capture == nil || ctx == nil {
		return result, &IPCErr{Code: ForceStopOwnerUnavailable, Message: "removed-target capture is unavailable"}
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.conn == nil {
		return result, &IPCErr{Code: ForceStopOwnerUnavailable, Message: "removed-target capture was already closed or consumed"}
	}
	conn := capture.conn
	capture.conn = nil
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	command := capture.command
	command.RemovalPhase = ForceStopCompleteRemoved
	return withForceStopExchangeContext(ctx, conn, func() (ForceStopResultV1, error) {
		return exchangeSupervisorForceStopCommand(ctx, conn, capture.nextID, command)
	})
}

func (capture *RetainedSupervisorForceStop) Close() error {
	if capture == nil {
		return nil
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.conn == nil {
		return nil
	}
	conn := capture.conn
	capture.conn = nil
	return conn.Close()
}

func dialSupervisorForceStopConnection(ctx context.Context) (net.Conn, SupervisorLockOwner, error) {
	var owner SupervisorLockOwner
	if err := ctx.Err(); err != nil {
		return nil, owner, err
	}
	if runtime.GOOS != "windows" {
		return nil, owner, &IPCErr{Code: ForceStopUnsupported, Message: "original creator tree termination is unavailable on this platform"}
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return nil, owner, err
	}
	owner, err = ReadSupervisorLockOwner(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, owner, fmt.Errorf("force_stop original Supervisor unavailable: %w: %w", ErrSupervisorIPCUnavailable, err)
		}
		return nil, owner, fmt.Errorf("force_stop owner metadata: %w: %w", ErrStatusSetupFailure, err)
	}
	// The protected owner sidecar selects the peer, and native token/session/image
	// facts bind it to this user's executing Hub. These are observations, not a
	// new child owner. Synchronous OS identity calls cannot be interrupted by ctx.
	self, err := ObserveSupervisorProcessIdentityV1(os.Getpid())
	if err != nil {
		return nil, owner, err
	}
	peer, err := ObserveSupervisorProcessIdentityV1(owner.PID)
	if err != nil {
		return nil, owner, err
	}
	if err := ValidateSupervisorStatusServerIdentityV1(owner, peer, self.UserSID, self.SessionID, self.ImagePath); err != nil {
		return nil, owner, fmt.Errorf("%w: %w", ErrSupervisorControlPeerIdentity, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, owner, err
	}
	conn, err := dialSupervisorIPC(ctx, SupervisorIPCAddress(stateDir))
	if err != nil {
		return nil, owner, fmt.Errorf("force_stop dial: %w: %w", ErrSupervisorIPCUnavailable, err)
	}
	if err := ValidateSupervisorControlPeerConnectionV1(conn, owner, peer); err != nil {
		return nil, owner, errors.Join(err, conn.Close())
	}
	return conn, owner, nil
}

func withForceStopExchangeContext(ctx context.Context, conn net.Conn, exchange func() (ForceStopResultV1, error)) (result ForceStopResultV1, retErr error) {
	defer func() { retErr = errors.Join(retErr, ctx.Err()) }()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return result, err
	}
	cancellationDone := make(chan error, 1)
	stopCancellation := context.AfterFunc(ctx, func() { cancellationDone <- conn.SetDeadline(time.Now()) })
	defer func() {
		if !stopCancellation() {
			retErr = errors.Join(retErr, <-cancellationDone)
		}
	}()
	return exchange()
}

// The caller authenticates the native connection before this wire-only body.
// Keeping the exchange separate permits deterministic framing/generation tests
// without fabricating a Windows native peer or a creator capability.
func exchangeSupervisorForceStop(ctx context.Context, conn net.Conn, owner SupervisorLockOwner, taskName string, port int) (ForceStopResultV1, error) {
	command, nextID, err := readSupervisorForceStopTarget(ctx, conn, owner, taskName, port)
	if err != nil {
		return ForceStopResultV1{}, err
	}
	return exchangeSupervisorForceStopCommand(ctx, conn, nextID, command)
}

func readSupervisorForceStopTarget(ctx context.Context, conn net.Conn, owner SupervisorLockOwner, taskName string, port int) (command ForceStopCommandV1, nextID int64, retErr error) {
	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		return command, 0, err
	}
	request := IPCRequest{Version: 1, ID: time.Now().UnixNano(), Cmd: "status"}
	response, err := exchangeForceStopRequest(ctx, conn, request)
	if err != nil {
		return command, 0, err
	}
	var status struct {
		Daemons []supervisorIPCStatusDaemon `json:"daemons"`
	}
	if err := json.Unmarshal(response.Result, &status); err != nil {
		return command, 0, fmt.Errorf("force_stop read generation: %w", err)
	}
	command.ProtocolVersion = 1
	for _, daemon := range status.Daemons {
		if canonicalIntentTaskKey(daemon.TaskName) != canonicalIntentTaskKey(taskName) {
			continue
		}
		if command.Target.TaskName != "" {
			return command, 0, &IPCErr{Code: ForceStopGenerationChanged, Message: "ambiguous task generation in Supervisor status"}
		}
		pid := daemon.CurrentPID
		if pid == 0 {
			// Status can classify a port-stale row as Restarting while the
			// original child is still alive. This remains only a candidate;
			// the controller must match its retained creator before acting.
			pid = daemon.StalePID
		}
		command.Target = ForceStopTargetV1{TaskName: canonicalIntentTaskKey(taskName), PID: pid, StartedAt: daemon.StartedAt, PIDGeneration: daemon.PIDGeneration, ExpectedPort: port}
	}
	if command.Target.PID <= 0 {
		return command, 0, &IPCErr{Code: ForceStopOwnerUnavailable, Message: "Supervisor reported no running generation; absence is not tree-closure proof"}
	}
	if command.Target.PIDGeneration <= 0 {
		return command, 0, &IPCErr{Code: ForceStopUnsupported, Message: "Supervisor does not expose the generation required by force_stop v1"}
	}
	if err := ValidateForceStopCommandV1(command); err != nil {
		return command, 0, err
	}
	return command, request.ID + 1, nil
}

func exchangeSupervisorForceStopCommand(ctx context.Context, conn net.Conn, id int64, command ForceStopCommandV1) (ForceStopResultV1, error) {
	var result ForceStopResultV1
	request := IPCRequest{Version: 1, ID: id, Cmd: "force_stop", Args: map[string]any{"command": command}}
	if command.RemovalPhase != "" {
		// An older force_stop v1 server ignores unknown command fields. A
		// distinct verb makes it refuse preparation before any destructive work.
		request.Cmd = "force_stop_removed"
	}
	response, err := exchangeForceStopRequest(ctx, conn, request)
	if err != nil {
		return result, err
	}
	if !response.Final {
		return result, fmt.Errorf("force_stop response is not final; outcome unknown")
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return result, fmt.Errorf("force_stop decode result: %w", err)
	}
	return result, validateForceStopResult(command, result)
}

func exchangeForceStopRequest(ctx context.Context, conn net.Conn, request IPCRequest) (supervisorIPCRawResponse, error) {
	if err := ctx.Err(); err != nil {
		return supervisorIPCRawResponse{}, err
	}
	if err := writeSupervisorIPCRequest(conn, request); err != nil {
		return supervisorIPCRawResponse{}, fmt.Errorf("force_stop send %s (outcome unknown; not retried): %w", request.Cmd, err)
	}
	response, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return response, fmt.Errorf("force_stop read %s (outcome unknown; not retried): %w", request.Cmd, err)
	}
	if response.ID != request.ID {
		return response, fmt.Errorf("force_stop response id mismatch")
	}
	if response.Error != nil {
		return response, response.Error
	}
	if !response.OK {
		return response, fmt.Errorf("force_stop non-OK response without cause")
	}
	return response, nil
}

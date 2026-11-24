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
	defer func() { retErr = errors.Join(retErr, ctx.Err()) }()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if runtime.GOOS != "windows" {
		return result, &IPCErr{Code: ForceStopUnsupported, Message: "original creator tree termination is unavailable on this platform"}
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return result, err
	}
	owner, err := ReadSupervisorLockOwner(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, fmt.Errorf("force_stop original Supervisor unavailable: %w: %w", ErrSupervisorIPCUnavailable, err)
		}
		return result, fmt.Errorf("force_stop owner metadata: %w: %w", ErrStatusSetupFailure, err)
	}
	// The protected owner sidecar selects the peer, and native token/session/image
	// facts bind it to this user's executing Hub. These are observations, not a
	// new child owner. Synchronous OS identity calls cannot be interrupted by ctx.
	self, err := ObserveSupervisorProcessIdentityV1(os.Getpid())
	if err != nil {
		return result, err
	}
	peer, err := ObserveSupervisorProcessIdentityV1(owner.PID)
	if err != nil {
		return result, err
	}
	if err := ValidateSupervisorStatusServerIdentityV1(owner, peer, self.UserSID, self.SessionID, self.ImagePath); err != nil {
		return result, fmt.Errorf("%w: %w", ErrSupervisorControlPeerIdentity, err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	conn, err := dialSupervisorIPC(ctx, SupervisorIPCAddress(stateDir))
	if err != nil {
		return result, fmt.Errorf("force_stop dial: %w: %w", ErrSupervisorIPCUnavailable, err)
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
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
	if err := ValidateSupervisorControlPeerConnectionV1(conn, owner, peer); err != nil {
		return result, err
	}
	return exchangeSupervisorForceStop(ctx, conn, owner, taskName, port)
}

// The caller authenticates the native connection before this wire-only body.
// Keeping the exchange separate permits deterministic framing/generation tests
// without fabricating a Windows native peer or a creator capability.
func exchangeSupervisorForceStop(ctx context.Context, conn net.Conn, owner SupervisorLockOwner, taskName string, port int) (ForceStopResultV1, error) {
	var result ForceStopResultV1
	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		return result, err
	}
	request := IPCRequest{Version: 1, ID: time.Now().UnixNano(), Cmd: "status"}
	response, err := exchangeForceStopRequest(ctx, conn, request)
	if err != nil {
		return result, err
	}
	var status struct {
		Daemons []supervisorIPCStatusDaemon `json:"daemons"`
	}
	if err := json.Unmarshal(response.Result, &status); err != nil {
		return result, fmt.Errorf("force_stop read generation: %w", err)
	}
	command := ForceStopCommandV1{ProtocolVersion: 1}
	for _, daemon := range status.Daemons {
		if canonicalIntentTaskKey(daemon.TaskName) != canonicalIntentTaskKey(taskName) {
			continue
		}
		if command.Target.TaskName != "" {
			return result, &IPCErr{Code: ForceStopGenerationChanged, Message: "ambiguous task generation in Supervisor status"}
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
		return result, &IPCErr{Code: ForceStopOwnerUnavailable, Message: "Supervisor reported no running generation; absence is not tree-closure proof"}
	}
	if command.Target.PIDGeneration <= 0 {
		return result, &IPCErr{Code: ForceStopUnsupported, Message: "Supervisor does not expose the generation required by force_stop v1"}
	}
	if err := ValidateForceStopCommandV1(command); err != nil {
		return result, err
	}
	request.ID++
	request.Cmd = "force_stop"
	request.Args = map[string]any{"command": command}
	response, err = exchangeForceStopRequest(ctx, conn, request)
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

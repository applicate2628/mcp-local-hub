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

// ErrSupervisorCapabilityUnsupported is the authenticated compatibility
// verdict for a supervisor which predates the read-only capabilities command.
// Callers may replace that proven legacy generation before their first control
// mutation; every transport, owner, or handshake failure remains fail-closed.
var ErrSupervisorCapabilityUnsupported = errors.New("supervisor control capabilities unsupported")

// ErrSupervisorCapabilityLegacy is the authenticated UNKNOWN_COMMAND verdict
// emitted only by supervisors which predate capabilities. It is the sole
// compatibility result that may trigger a replacement.
var ErrSupervisorCapabilityLegacy = errors.New("supervisor control capabilities legacy")

// ErrSupervisorCapabilityIncomplete is a valid capabilities envelope lacking
// a required feature. That supervisor is current-but-incompatible, not legacy;
// callers must fail closed rather than replace it.
var ErrSupervisorCapabilityIncomplete = errors.New("supervisor control capabilities incomplete")

// ErrSupervisorControlGenerationChanged marks a compatibility handoff whose
// authenticated legacy lock owner changed before its exact-generation action.
// Callers must fail closed rather than apply a legacy verdict to a successor.
var ErrSupervisorControlGenerationChanged = errors.New("supervisor control generation changed")

// SupervisorControlCapabilities names the control transactions that the
// current client will issue only after this authenticated read-only probe.
type SupervisorControlCapabilities struct {
	StopBatch bool `json:"stop_batch"`
	Respawn   bool `json:"respawn"`
}

// SupervisorControlProbe binds an authenticated capability verdict to the
// exact lock-owner generation that answered it. A caller replacing a legacy
// supervisor must keep using this generation for every following IPC and
// force-kill step; silently switching to a successor would let an old
// UNKNOWN_COMMAND verdict terminate a current supervisor.
type SupervisorControlProbe struct {
	Capabilities SupervisorControlCapabilities
	Owner        SupervisorLockOwner
}

// ProbeSupervisorControlCapabilities authenticates the lock-owner hello and
// asks the live supervisor for its control capability set without changing
// intent, runtime state, or processes.
func ProbeSupervisorControlCapabilities(ctx context.Context) (SupervisorControlCapabilities, error) {
	probe, err := ProbeSupervisorControlCapabilitiesSnapshot(ctx)
	return probe.Capabilities, err
}

// ProbeSupervisorControlCapabilitiesSnapshot is the generation-bound form of
// ProbeSupervisorControlCapabilities. It is read-only: it authenticates hello
// against the captured owner and sends only capabilities before returning the
// exact owner to a compatibility handoff.
func ProbeSupervisorControlCapabilitiesSnapshot(ctx context.Context) (SupervisorControlProbe, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return SupervisorControlProbe{}, fmt.Errorf("supervisor capabilities: resolve state dir: %w", err)
	}
	return probeSupervisorControlCapabilitiesSnapshotFromStateDir(ctx, stateDir)
}

func probeSupervisorControlCapabilitiesFromStateDir(ctx context.Context, stateDir string) (SupervisorControlCapabilities, error) {
	probe, err := probeSupervisorControlCapabilitiesSnapshotFromStateDir(ctx, stateDir)
	return probe.Capabilities, err
}

func probeSupervisorControlCapabilitiesSnapshotFromStateDir(ctx context.Context, stateDir string) (SupervisorControlProbe, error) {
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	owner, err := ReadSupervisorLockOwner(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return SupervisorControlProbe{}, fmt.Errorf("supervisor capabilities: owner sidecar unavailable: %w: %w", ErrSupervisorIPCUnavailable, err)
		}
		return SupervisorControlProbe{}, fmt.Errorf("supervisor capabilities: read owner sidecar: %w", err)
	}
	conn, err := dialSupervisorIPC(ctx, SupervisorIPCAddress(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return SupervisorControlProbe{}, fmt.Errorf("supervisor capabilities: dial: %w: %w", ErrSupervisorIPCUnavailable, err)
		}
		return SupervisorControlProbe{}, fmt.Errorf("supervisor capabilities: dial: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		return SupervisorControlProbe{}, fmt.Errorf("supervisor capabilities: protocol authentication: %w", err)
	}
	req := IPCRequest{Version: 1, ID: time.Now().UnixNano(), Cmd: "capabilities"}
	if err := writeSupervisorIPCRequest(conn, req); err != nil {
		return SupervisorControlProbe{}, fmt.Errorf("supervisor capabilities: send: %w", err)
	}
	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return SupervisorControlProbe{}, fmt.Errorf("supervisor capabilities: read response: %w", err)
	}
	if resp.ID != req.ID {
		return SupervisorControlProbe{}, fmt.Errorf("supervisor capabilities: response id mismatch: got %d want %d", resp.ID, req.ID)
	}
	if resp.Error != nil {
		if resp.Error.Code == "UNKNOWN_COMMAND" {
			return SupervisorControlProbe{Owner: owner}, fmt.Errorf("%w: %w: %s", ErrSupervisorCapabilityUnsupported, ErrSupervisorCapabilityLegacy, resp.Error.Message)
		}
		return SupervisorControlProbe{Owner: owner}, fmt.Errorf("supervisor capabilities: %s: %s", resp.Error.Code, resp.Error.Message)
	}
	if !resp.OK {
		return SupervisorControlProbe{Owner: owner}, fmt.Errorf("supervisor capabilities: non-OK response without error")
	}
	var caps SupervisorControlCapabilities
	if err := json.Unmarshal(resp.Result, &caps); err != nil {
		return SupervisorControlProbe{Owner: owner}, fmt.Errorf("supervisor capabilities: decode result: %w", err)
	}
	if !caps.StopBatch || !caps.Respawn {
		return SupervisorControlProbe{Capabilities: caps, Owner: owner}, fmt.Errorf("%w: %w: stop_batch=%t respawn=%t", ErrSupervisorCapabilityUnsupported, ErrSupervisorCapabilityIncomplete, caps.StopBatch, caps.Respawn)
	}
	return SupervisorControlProbe{Capabilities: caps, Owner: owner}, nil
}

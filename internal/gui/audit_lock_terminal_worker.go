package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
)

const (
	auditLockTerminalWorkerProtocolVersion = 1
	auditLockTerminalWorkerMessageMaxBytes = 64 * 1024
	auditLockTerminalWorkerStderrMaxBytes  = 16 * 1024
)

type auditLockTerminalWorkerFailure string

const (
	auditLockTerminalWorkerFailureTimeout           auditLockTerminalWorkerFailure = "DAEMON_RECOVERY_TERMINAL_WORKER_TIMEOUT"
	auditLockTerminalWorkerFailureContainmentFailed auditLockTerminalWorkerFailure = "DAEMON_RECOVERY_TERMINAL_WORKER_CONTAINMENT_FAILED"
	auditLockTerminalWorkerFailureProtocolInvalid   auditLockTerminalWorkerFailure = "DAEMON_RECOVERY_TERMINAL_WORKER_PROTOCOL_INVALID"
	auditLockTerminalWorkerFailureExecutionFailed   auditLockTerminalWorkerFailure = "DAEMON_RECOVERY_TERMINAL_WORKER_EXECUTION_FAILED"
	auditLockTerminalWorkerFailureStateDirFailed    auditLockTerminalWorkerFailure = "DAEMON_RECOVERY_TERMINAL_WORKER_STATE_DIR_FAILED"
	auditLockTerminalWorkerFailureUnproved          auditLockTerminalWorkerFailure = "DAEMON_RECOVERY_TERMINAL_WORKER_UNPROVED"
)

func auditLockTerminalWorkerFailureID(value string) (auditLockTerminalWorkerFailure, bool) {
	failure := auditLockTerminalWorkerFailure(value)
	switch failure {
	case auditLockTerminalWorkerFailureTimeout, auditLockTerminalWorkerFailureContainmentFailed,
		auditLockTerminalWorkerFailureProtocolInvalid, auditLockTerminalWorkerFailureExecutionFailed,
		auditLockTerminalWorkerFailureStateDirFailed, auditLockTerminalWorkerFailureUnproved:
		return failure, true
	default:
		return "", false
	}
}

type auditLockTerminalWorkerRequest struct {
	Version       int                       `json:"version"`
	Receipt       auditLockReceiptDTO       `json:"receipt"`
	Generation    uint64                    `json:"generation"`
	Confirm       bool                      `json:"confirm"`
	Status        string                    `json:"status"`
	Authorization string                    `json:"authorization"`
	Terminal      auditLockTerminalEvidence `json:"terminal"`
	AllowanceMS   int64                     `json:"allowance_ms"`
}

type auditLockTerminalWorkerResult struct {
	Version int                 `json:"version"`
	Outcome string              `json:"outcome"`
	Receipt auditLockReceiptDTO `json:"receipt"`
	Status  int                 `json:"status,omitempty"`
	Code    string              `json:"code,omitempty"`
	Failure string              `json:"failure,omitempty"`
}

// RunAuditLockTerminalWorker is the hidden same-binary leaf. It accepts no
// path: the canonical daemon state location is resolved inside the child.
func RunAuditLockTerminalWorker(in io.Reader, out io.Writer) error {
	limited := io.LimitReader(in, auditLockTerminalWorkerMessageMaxBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read terminal worker request: %w", err)
	}
	if len(raw) > auditLockTerminalWorkerMessageMaxBytes {
		return errors.New("terminal worker request exceeds bound")
	}
	var request auditLockTerminalWorkerRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode terminal worker request: %w", err)
	}
	if err := ensureAuditLockTerminalWorkerEOF(decoder); err != nil {
		return err
	}
	result := auditLockTerminalWorkerResult{Version: auditLockTerminalWorkerProtocolVersion, Receipt: request.Receipt}
	if request.Version != auditLockTerminalWorkerProtocolVersion || request.AllowanceMS <= 0 || !request.Confirm {
		result.Outcome, result.Failure = "rejected", string(auditLockTerminalWorkerFailureProtocolInvalid)
		return writeAuditLockTerminalWorkerResult(out, result)
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		result.Outcome, result.Failure = "uncertain", string(auditLockTerminalWorkerFailureStateDirFailed)
		return writeAuditLockTerminalWorkerResult(out, result)
	}
	adapter := &auditLockAdapter{
		storePath:              filepath.Join(stateDir, auditLockOccurrenceFileLeaf),
		storeLockHealth:        newOccurrenceStoreLockHealth(nil, emitOccurrenceStoreLockHealthEvent),
		lockTimeout:            time.Duration(request.AllowanceMS) * time.Millisecond,
		writeStateFileLockHeld: api.WriteStateFileBytesLockHeld,
		serverInstance:         request.Receipt.ServerInstance,
		generation:             request.Generation,
	}
	reservation := auditLockReservation{
		Receipt:       request.Receipt,
		Binding:       auditLockOccurrenceBinding{serverInstance: request.Receipt.ServerInstance, taskName: request.Receipt.TaskName, confirm: request.Confirm},
		MutationEpoch: auditLockMutationEpoch{ServerInstance: request.Receipt.ServerInstance, Generation: request.Generation},
	}
	transactionCtx, cancel := context.WithTimeout(context.Background(), time.Duration(request.AllowanceMS)*time.Millisecond)
	defer cancel()
	receipt, routeErr := adapter.terminalizeContext(transactionCtx, reservation, request.Status, request.Authorization, request.Terminal)
	result.Receipt = receipt
	if routeErr == nil {
		result.Outcome = "durable_terminal"
	} else if routeErr.code == string(daemonRecoverErrorBaselineStale) {
		result.Outcome, result.Status, result.Code = "baseline_stale", routeErr.status, routeErr.code
	} else {
		result.Outcome, result.Status, result.Code, result.Failure = "uncertain", routeErr.status, routeErr.code, string(auditLockTerminalWorkerFailureUnproved)
	}
	return writeAuditLockTerminalWorkerResult(out, result)
}

func ensureAuditLockTerminalWorkerEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple terminal worker JSON values")
}

func writeAuditLockTerminalWorkerResult(out io.Writer, result auditLockTerminalWorkerResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(raw) > auditLockTerminalWorkerMessageMaxBytes {
		return errors.New("terminal worker result exceeds bound")
	}
	_, err = out.Write(raw)
	return err
}

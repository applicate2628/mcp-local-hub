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

// ErrSerenaActivityCommitUnsupported identifies a mixed-version pair where a
// route/GUI process understands the durable activity protocol but its running
// supervisor does not.  Callers must fail the request explicitly rather than
// silently falling back to a direct registry write.
var ErrSerenaActivityCommitUnsupported = errors.New("supervisor serena activity commit unsupported")

const defaultSerenaActivityCommitTimeout = 5 * time.Second

// DialSupervisorIPCCommitSerenaActivity asks the running supervisor to
// validate and persist one successful Serena activity timestamp.  It performs
// no local registry I/O; the supervisor remains the only writer for this
// router-originated mutation.
func DialSupervisorIPCCommitSerenaActivity(ctx context.Context, request SerenaActivityCommitRequestV1) (SerenaActivityCommitReceiptV1, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultSerenaActivityCommitTimeout)
		defer cancel()
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: resolve state dir: %w", err)
	}
	return dialSupervisorIPCCommitSerenaActivityFromStateDir(ctx, stateDir, request)
}

func dialSupervisorIPCCommitSerenaActivityFromStateDir(ctx context.Context, stateDir string, request SerenaActivityCommitRequestV1) (SerenaActivityCommitReceiptV1, error) {
	if err := validateSerenaActivityCommitRequest(request); err != nil {
		return SerenaActivityCommitReceiptV1{}, err
	}
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	owner, err := ReadSupervisorLockOwner(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: supervisor.lock.owner.json not found at %s.owner.json: %w: %w", lockPath, ErrSupervisorIPCUnavailable, err)
		}
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: read %s.owner.json: %w", lockPath, err)
	}
	conn, err := dialSupervisorIPC(ctx, SupervisorIPCAddress(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: dial supervisor: %w: %w", ErrSupervisorIPCUnavailable, err)
		}
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: dial supervisor: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: %w", err)
	}
	req := IPCRequest{Version: 1, ID: time.Now().UnixNano(), Cmd: "commit_serena_activity", Args: map[string]any{"request": request}}
	if err := writeSupervisorIPCRequest(conn, req); err != nil {
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: send: %w", err)
	}
	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: read response: %w", err)
	}
	return decodeSerenaActivityCommitResponse(req, request, resp)
}

func decodeSerenaActivityCommitResponse(req IPCRequest, request SerenaActivityCommitRequestV1, resp supervisorIPCRawResponse) (SerenaActivityCommitReceiptV1, error) {
	if resp.ID != req.ID {
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: response id mismatch: got %d want %d", resp.ID, req.ID)
	}
	if resp.Error != nil {
		if resp.Error.Code == "UNKNOWN_COMMAND" {
			return SerenaActivityCommitReceiptV1{}, fmt.Errorf("%w: %s", ErrSerenaActivityCommitUnsupported, resp.Error.Message)
		}
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: %s: %s", resp.Error.Code, resp.Error.Message)
	}
	if !resp.OK || len(resp.Result) == 0 {
		return SerenaActivityCommitReceiptV1{}, errors.New("supervisor IPC commit_serena_activity: missing receipt")
	}
	var receipt SerenaActivityCommitReceiptV1
	if err := json.Unmarshal(resp.Result, &receipt); err != nil {
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("supervisor IPC commit_serena_activity: decode receipt: %w", err)
	}
	if receipt.ProtocolVersion != 1 || (receipt.State != "committed" && receipt.State != "already_committed") {
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("%w: invalid receipt", ErrSerenaActivityCommitUnsupported)
	}
	if receipt.WorkspaceKey != request.WorkspaceKey || receipt.TaskName != request.TaskName ||
		!receipt.RegisteredAt.Equal(request.RegisteredAt) || !receipt.ActivityAt.Equal(request.ActivityAt) {
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("%w: receipt does not echo requested generation", ErrSerenaActivityCommitUnsupported)
	}
	return receipt, nil
}

func validateSerenaActivityCommitRequest(request SerenaActivityCommitRequestV1) error {
	if request.ProtocolVersion != 1 || request.WorkspaceKey == "" || request.WorkspacePath == "" || request.TaskName == "" || request.ExpectedPort <= 0 || request.RegisteredAt.IsZero() || request.ActivityAt.IsZero() {
		return errors.New("supervisor IPC commit_serena_activity: invalid request")
	}
	return nil
}

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

var ErrLazyProxyReadinessUnsupported = errors.New("supervisor lazy proxy readiness commit unsupported")

const defaultLazyProxyReadinessCommitTimeout = 5 * time.Second

func DialSupervisorIPCCommitLazyProxyReadiness(ctx context.Context, event LazyProxyReadinessEventV1) (LazyProxyReadinessReceiptV1, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultLazyProxyReadinessCommitTimeout)
		defer cancel()
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return LazyProxyReadinessReceiptV1{}, fmt.Errorf("supervisor IPC commit_lazy_proxy_readiness: resolve state dir: %w", err)
	}
	return dialSupervisorIPCCommitLazyProxyReadinessFromStateDir(ctx, stateDir, event)
}

func dialSupervisorIPCCommitLazyProxyReadinessFromStateDir(ctx context.Context, stateDir string, event LazyProxyReadinessEventV1) (LazyProxyReadinessReceiptV1, error) {
	if err := validateLazyProxyReadinessEvent(event); err != nil {
		return LazyProxyReadinessReceiptV1{}, err
	}
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	owner, err := ReadSupervisorLockOwner(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return LazyProxyReadinessReceiptV1{}, fmt.Errorf("supervisor IPC commit_lazy_proxy_readiness: %w", ErrSupervisorIPCUnavailable)
		}
		return LazyProxyReadinessReceiptV1{}, err
	}
	conn, err := dialSupervisorIPC(ctx, SupervisorIPCAddress(stateDir))
	if err != nil {
		return LazyProxyReadinessReceiptV1{}, err
	}
	// A deadline remains the bounded fallback, while active cancellation closes
	// an already-established pipe so the lazy-proxy publisher worker can join
	// promptly during Stop. Join the AfterFunc callback before returning so it
	// never outlives the connection owner.
	cancelCloseDone := make(chan struct{})
	stopCancelClose := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		close(cancelCloseDone)
	})
	defer func() {
		if stopCancelClose() {
			close(cancelCloseDone)
		}
		<-cancelCloseDone
		_ = conn.Close()
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		return LazyProxyReadinessReceiptV1{}, lazyReadinessIPCContextError(ctx, err)
	}
	req := IPCRequest{Version: 1, ID: time.Now().UnixNano(), Cmd: "commit_lazy_proxy_readiness", Args: map[string]any{"event": event}}
	if err := writeSupervisorIPCRequest(conn, req); err != nil {
		return LazyProxyReadinessReceiptV1{}, lazyReadinessIPCContextError(ctx, err)
	}
	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return LazyProxyReadinessReceiptV1{}, lazyReadinessIPCContextError(ctx, err)
	}
	return decodeLazyProxyReadinessResponse(req, event, resp)
}

func lazyReadinessIPCContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("supervisor IPC commit_lazy_proxy_readiness: %w", ctxErr)
	}
	return err
}

func decodeLazyProxyReadinessResponse(req IPCRequest, event LazyProxyReadinessEventV1, resp supervisorIPCRawResponse) (LazyProxyReadinessReceiptV1, error) {
	if resp.ID != req.ID {
		return LazyProxyReadinessReceiptV1{}, errors.New("supervisor IPC lazy readiness response id mismatch")
	}
	if resp.Error != nil {
		if resp.Error.Code == "UNKNOWN_COMMAND" {
			return LazyProxyReadinessReceiptV1{}, ErrLazyProxyReadinessUnsupported
		}
		return LazyProxyReadinessReceiptV1{}, fmt.Errorf("supervisor IPC lazy readiness: %s", resp.Error.Code)
	}
	var receipt LazyProxyReadinessReceiptV1
	if !resp.OK || !resp.Final || len(resp.Result) == 0 || json.Unmarshal(resp.Result, &receipt) != nil ||
		(receipt.Disposition != "accepted" && receipt.Disposition != "duplicate" && receipt.Disposition != "superseded") || lazyReadinessTransitionRank(receipt.Transition) == 0 || receipt.WrapperPID != event.WrapperPID ||
		receipt.WrapperStartedAt != event.WrapperStartedAt {
		return LazyProxyReadinessReceiptV1{}, ErrLazyProxyReadinessUnsupported
	}
	if (receipt.Disposition == "accepted" || receipt.Disposition == "duplicate") && (receipt.BackendEpoch != event.BackendEpoch || receipt.Transition != event.Transition || receipt.PIDGeneration <= 0) {
		return LazyProxyReadinessReceiptV1{}, ErrLazyProxyReadinessUnsupported
	}
	if receipt.Disposition == "superseded" && (receipt.PIDGeneration <= 0 || receipt.BackendEpoch < event.BackendEpoch || (receipt.BackendEpoch == event.BackendEpoch && lazyReadinessTransitionRank(receipt.Transition) <= lazyReadinessTransitionRank(event.Transition))) {
		return LazyProxyReadinessReceiptV1{}, ErrLazyProxyReadinessUnsupported
	}
	return receipt, nil
}

func validateLazyProxyReadinessEvent(event LazyProxyReadinessEventV1) error {
	if event.ProtocolVersion != 1 || event.TaskName == "" || event.WrapperPID <= 0 || event.WrapperStartedAt == "" || event.BackendEpoch == 0 || event.ObservedAt == "" || lazyReadinessTransitionRank(event.Transition) == 0 {
		return errors.New("supervisor IPC lazy readiness: invalid event")
	}
	if _, err := time.Parse(time.RFC3339Nano, event.WrapperStartedAt); err != nil {
		return errors.New("supervisor IPC lazy readiness: invalid wrapper start")
	}
	if _, err := time.Parse(time.RFC3339Nano, event.ObservedAt); err != nil {
		return errors.New("supervisor IPC lazy readiness: invalid observation time")
	}
	return nil
}

func lazyReadinessTransitionRank(transition string) int {
	switch transition {
	case "starting":
		return 1
	case "ready":
		return 2
	case "idle", "failed":
		return 3
	default:
		return 0
	}
}

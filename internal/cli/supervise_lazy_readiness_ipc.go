package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"mcp-local-hub/internal/api"
)

func handleCommitLazyProxyReadiness(conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error {
	event, err := parseLazyProxyReadinessEvent(req.Args)
	if err != nil {
		return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, Error: &api.IPCErr{Code: "LAZY_READINESS_INVALID", Message: err.Error()}, Final: true})
	}
	if deps.runtimeTracker == nil {
		return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, Error: &api.IPCErr{Code: "LAZY_READINESS_REJECTED", Message: "runtime tracker unavailable"}, Final: true})
	}
	receipt, settled, err := deps.runtimeTracker.CommitLazyProxyReadinessEvent(event)
	if err != nil {
		code := "LAZY_READINESS_REJECTED"
		if errors.Is(err, errLazyReadinessInvalid) {
			code = "LAZY_READINESS_INVALID"
		} else if errors.Is(err, errLazyReadinessIncarnationMismatch) {
			code = "LAZY_READINESS_INCARNATION_MISMATCH"
		}
		return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, Error: &api.IPCErr{Code: code, Message: err.Error()}, Final: true})
	}
	if settled != nil && deps.events != nil {
		_ = deps.events.Emit(*settled)
	}
	return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, OK: true, Result: receipt, Final: true})
}

func parseLazyProxyReadinessEvent(raw map[string]any) (api.LazyProxyReadinessEventV1, error) {
	var wrapper struct {
		Event api.LazyProxyReadinessEventV1 `json:"event"`
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return api.LazyProxyReadinessEventV1{}, fmt.Errorf("encode event: %w", err)
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return api.LazyProxyReadinessEventV1{}, fmt.Errorf("decode event: %w", err)
	}
	return wrapper.Event, nil
}

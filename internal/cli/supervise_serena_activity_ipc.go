package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"

	"mcp-local-hub/internal/api"
)

// handleCommitSerenaActivity is the sole supervisor IPC mutation path for
// route/GUI-originated Serena activity. It delegates the whole validation and
// write to Registry.CommitSerenaActivity, which holds the registry lock across
// reload, generation/intent validation, and timestamp persistence.
func handleCommitSerenaActivity(conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error {
	request, err := parseSerenaActivityCommitRequest(req.Args)
	if err != nil {
		return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, Error: &api.IPCErr{Code: "INVALID_ARGS", Message: err.Error()}, Final: true})
	}
	receipt, err := api.NewRegistry(filepath.Join(deps.stateDir, "workspaces.yaml")).CommitSerenaActivity(filepath.Join(deps.stateDir, "supervisor-intent.json"), request)
	if err != nil {
		code := "ACTIVITY_COMMIT_FAILED"
		if errors.Is(err, api.ErrSerenaActivityTargetStale) {
			code = "STALE_ACTIVITY_TARGET"
		}
		return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, Error: &api.IPCErr{Code: code, Message: err.Error()}, Final: true})
	}
	return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, OK: true, Result: receipt, Final: true})
}

func parseSerenaActivityCommitRequest(raw map[string]any) (api.SerenaActivityCommitRequestV1, error) {
	var wrapper struct {
		Request api.SerenaActivityCommitRequestV1 `json:"request"`
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return api.SerenaActivityCommitRequestV1{}, fmt.Errorf("encode request: %w", err)
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return api.SerenaActivityCommitRequestV1{}, fmt.Errorf("decode request: %w", err)
	}
	if err := validateSerenaActivityCommitRequestForSupervisor(wrapper.Request); err != nil {
		return api.SerenaActivityCommitRequestV1{}, err
	}
	return wrapper.Request, nil
}

func validateSerenaActivityCommitRequestForSupervisor(request api.SerenaActivityCommitRequestV1) error {
	if request.ProtocolVersion != 1 || request.WorkspaceKey == "" || request.WorkspacePath == "" || request.TaskName == "" || request.ExpectedPort <= 0 || request.RegisteredAt.IsZero() || request.ActivityAt.IsZero() {
		return fmt.Errorf("invalid commit_serena_activity request")
	}
	return nil
}

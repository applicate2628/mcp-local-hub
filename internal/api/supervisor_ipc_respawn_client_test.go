package api

// Deep-review P4 fix: DialSupervisorIPCRespawn's documented contract
// (supervisor_ipc_respawn_client.go:63-71) states success is Code:"" — the
// implementation used to return Code:"OK" instead, a latent mismatch with
// both the doc and the RestartResult{..., Code: result.Code} success-row
// invariant ("empty Err, empty Code") that restart_supervisor.go and
// DeferredToIntentWatcherCode's doc comment rely on. This test pins the
// corrected contract end-to-end through the real IPC wire path (reusing the
// fake supervisor server harness already used by the status-client tests),
// not just at the unit-fixture level.

import (
	"context"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

func TestDialSupervisorIPCRespawn_SuccessCodeIsEmpty(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)

	stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
		if req.Cmd != "respawn" {
			t.Fatalf("cmd = %q, want respawn", req.Cmd)
		}
		return IPCResponse{ID: req.ID, OK: true}
	})
	defer stop()

	result, err := DialSupervisorIPCRespawn(context.Background(), `\mcp-local-hub-memory-default`, false, 5000)
	if err != nil {
		t.Fatalf("DialSupervisorIPCRespawn: %v", err)
	}
	if !result.Success {
		t.Fatalf("result = %+v, want Success=true", result)
	}
	// The documented contract (supervisor_ipc_respawn_client.go:63) is
	// "" (Success=true) → 200. A stray "OK" here would leak into
	// RestartResult.Code on the plain-success path (restart_supervisor.go:190)
	// and contradict DeferredToIntentWatcherCode's "empty Err, empty Code"
	// plain-success invariant.
	if result.Code != "" {
		t.Fatalf("result.Code = %q, want empty string per the documented success contract", result.Code)
	}
}

func TestDialSupervisorIPCRespawn_RefusalCodePropagated(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)

	stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
		if req.Cmd != "respawn" {
			t.Fatalf("cmd = %q, want respawn", req.Cmd)
		}
		return IPCResponse{
			ID:    req.ID,
			Error: &IPCErr{Code: "QUARANTINED", Message: "daemon is quarantined"},
		}
	})
	defer stop()

	result, err := DialSupervisorIPCRespawn(context.Background(), `\mcp-local-hub-memory-default`, false, 5000)
	if err != nil {
		t.Fatalf("DialSupervisorIPCRespawn: %v", err)
	}
	if result.Success {
		t.Fatalf("result = %+v, want Success=false on refusal", result)
	}
	if result.Code != "QUARANTINED" {
		t.Fatalf("result.Code = %q, want QUARANTINED (refusal codes pass through unchanged)", result.Code)
	}
}

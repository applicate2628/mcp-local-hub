package api

import (
	"context"
	"strings"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

// TestRespawnDialHelloMismatch pins the contract the GUI's 503-mapping fix
// (daemonRespawnHandler) depends on: a genuine transport-level failure (here,
// a hello handshake mismatch — the same class as a dial/connect failure)
// comes back as a non-nil `error`, NOT a populated RespawnResult with a
// supervisor-issued Code. dialSupervisorIPCRespawnFromStateDir classifies only
// ErrSupervisorIPCUnavailable / os.IsNotExist into the structured
// SUPERVISOR_UNAVAILABLE RespawnResult; every other dial/handshake failure
// — including this one — propagates as the function's error return. The
// GUI handler must map that error path to HTTP 503 (the supervisor is
// unreachable right now, a retryable condition), not 500.
// Short name on purpose: HardenedTempDir folds the test name into the POSIX
// socket path, and a long name overruns the sun_path limit (~104/108 bytes).
func TestRespawnDialHelloMismatch(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)

	// The fake server presents a DIFFERENT StartedAt in its hello than the
	// owner sidecar promised, so validateSupervisorIPCHello refuses the
	// connection — a deterministic stand-in for "owner file present but
	// nothing legitimate is listening" without relying on OS-specific
	// connection-refused timing.
	stop := startFakeSupervisorIPCStatusServer(t, stateDir,
		SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:01Z"},
		func(req IPCRequest) IPCResponse {
			t.Fatalf("client sent %q after mismatched hello; want close before request", req.Cmd)
			return IPCResponse{}
		})
	defer stop()

	result, err := dialSupervisorIPCRespawnFromStateDir(context.Background(), stateDir, `\mcp-local-hub-memory-default`, false, 5000)
	if err == nil {
		t.Fatalf("dialSupervisorIPCRespawnFromStateDir returned nil error on handshake mismatch; result=%+v", result)
	}
	if !strings.Contains(err.Error(), "hello mismatch") {
		t.Fatalf("err = %v, want hello mismatch (transport-level failure, not a classified RespawnResult.Code)", err)
	}
	// Confirm this is NOT the SUPERVISOR_UNAVAILABLE classified path — that
	// path returns (RespawnResult{Code: "SUPERVISOR_UNAVAILABLE"}, nil), the
	// opposite shape from what this test asserts above. A future change that
	// folds hello-mismatch into the classified-Code branch would silently
	// flip the GUI handler back to mapping this case via the result.Code
	// switch (503 either way today, but via a different — and currently
	// untested — code path) rather than the dialErr-is-503 branch this test
	// pins.
	if result.Code != "" || result.Success {
		t.Fatalf("result = %+v, want zero-value RespawnResult alongside a transport error", result)
	}
}

// TestDialSupervisorIPCRespawn_SuccessCodeIsEmpty pins DialSupervisorIPCRespawn's
// documented contract (supervisor_ipc_respawn_client.go:63-71): success is
// Code:"" — the implementation used to return Code:"OK" instead, a latent
// mismatch with both the doc and the RestartResult{..., Code: result.Code}
// success-row invariant ("empty Err, empty Code") that restart_supervisor.go
// and DeferredToIntentWatcherCode's doc comment rely on. This test pins the
// corrected contract end-to-end through the real IPC wire path (reusing the
// fake supervisor server harness already used by the status-client tests),
// not just at the unit-fixture level.
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

package api

import (
	"context"
	"errors"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

func TestProbeSupervisorControlCapabilities_AuthenticatedCurrentSupervisor(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-09-01T00:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)
	stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
		if req.Cmd != "capabilities" {
			t.Fatalf("cmd=%q want capabilities", req.Cmd)
		}
		return IPCResponse{ID: req.ID, OK: true, Result: SupervisorControlCapabilities{StopBatch: true, Respawn: true}, Final: true}
	})
	defer stop()

	caps, err := ProbeSupervisorControlCapabilities(context.Background())
	if err != nil || !caps.StopBatch || !caps.Respawn {
		t.Fatalf("caps=%+v err=%v", caps, err)
	}
}

func TestProbeSupervisorControlCapabilities_HistoricalV0432UnknownCommandIsTyped(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-09-01T00:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)
	stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
		return IPCResponse{ID: req.ID, Error: &IPCErr{Code: "UNKNOWN_COMMAND", Message: "unknown command capabilities"}, Final: true}
	})
	defer stop()

	probe, err := ProbeSupervisorControlCapabilitiesSnapshot(context.Background())
	if !errors.Is(err, ErrSupervisorCapabilityUnsupported) || !errors.Is(err, ErrSupervisorCapabilityLegacy) {
		t.Fatalf("err=%v, want typed legacy capability refusal", err)
	}
	if probe.Owner != owner {
		t.Fatalf("legacy probe owner=%+v want %+v", probe.Owner, owner)
	}
}

func TestVerifySupervisorControlGeneration_RefusesChangedSidecarBeforeStatus(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	writeSupervisorOwnerForTest(t, stateDir, SupervisorLockOwner{PID: 77, StartedAt: "new"})
	err := VerifySupervisorControlGeneration(context.Background(), SupervisorLockOwner{PID: 42, StartedAt: "old"})
	if err == nil {
		t.Fatal("changed owner was accepted")
	}
}

func TestProbeSupervisorControlCapabilities_PartialReplyRefusesBeforeMutation(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-09-01T00:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)
	stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
		return IPCResponse{ID: req.ID, OK: true, Result: SupervisorControlCapabilities{StopBatch: true, Respawn: false}, Final: true}
	})
	defer stop()

	_, err := ProbeSupervisorControlCapabilities(context.Background())
	if !errors.Is(err, ErrSupervisorCapabilityIncomplete) || errors.Is(err, ErrSupervisorCapabilityLegacy) {
		t.Fatalf("err=%v, want typed partial-capability refusal", err)
	}
}

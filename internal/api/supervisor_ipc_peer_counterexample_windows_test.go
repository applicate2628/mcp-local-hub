//go:build windows

package api

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// This is a protocol/model counterexample over existing B37 operations, not a
// native creator or connected-kernel identity oracle. Both identity operands
// below are synthetic. The real IPC exchange is served by the existing isolated
// in-process fixture. Generic owner/hello APIs intentionally retain this legacy
// behavior; the correction must move creator consumers to the strict peer API.
func TestSupervisorPeerMatchingHelloExistingOperationsModel(t *testing.T) {
	original := SupervisorLockOwner{PID: 4242, StartedAt: "2026-10-09T00:00:00Z"}
	expected := SupervisorProcessIdentityV1{
		PID: original.PID, CreationTime: original.StartedAt,
		UserSID: "S-1-5-21-9", SessionID: 3,
		ImagePath: "synthetic/installed/mcphub.exe",
	}
	if err := ValidateSupervisorStatusServerIdentityV1(original, expected, expected.UserSID, expected.SessionID, expected.ImagePath); err != nil {
		t.Fatalf("matching model identity refused: %v", err)
	}
	impostor := expected
	impostor.PID++
	if err := ValidateSupervisorStatusServerIdentityV1(original, impostor, expected.UserSID, expected.SessionID, expected.ImagePath); err == nil {
		t.Fatal("existing kernel-identity comparator accepted the wrong model PID")
	}

	for _, command := range []string{"status", "exit"} {
		t.Run(command, func(t *testing.T) {
			stateDir := hardenedTempDir(t)
			writeSupervisorOwnerForTest(t, stateDir, original)
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
			if !strings.Contains(SupervisorIPCAddress(stateDir), "-test-") {
				t.Fatal("refusing a non-test supervisor pipe")
			}
			var frames atomic.Int32
			stop := startFakeSupervisorIPCStatusServer(t, stateDir, original, func(req IPCRequest) IPCResponse {
				frames.Add(1)
				if req.Cmd != command {
					t.Errorf("received command %q, want %q", req.Cmd, command)
				}
				result := map[string]any{"state": "running", "daemons": []any{}}
				if command == "exit" {
					result = map[string]any{"graceful_exit_initiated": true}
				}
				return IPCResponse{ID: req.ID, OK: true, Final: true, Result: result}
			})
			defer stop()

			var err error
			if command == "status" {
				err = VerifySupervisorControlGenerationFromStateDir(context.Background(), stateDir, original)
			} else {
				// A distinct subtest creates a fresh listener/connection; no
				// readiness result authorizes this destructive protocol frame.
				err = DialSupervisorIPCExitForOwner(context.Background(), stateDir, original, 5000)
			}
			if err != nil {
				t.Fatalf("existing owner/hello exchange no longer demonstrates baseline acceptance: %v", err)
			}
			if got := frames.Load(); got != 1 {
				t.Fatalf("received %d protocol frames, want one", got)
			}
			t.Log("MODEL ONLY: matching hello accepted; independent wrong-PID model is rejected by the existing comparator, which this operation does not consume")
		})
	}
}

package api

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// This deliberately degraded in-memory transport has no native pipe handle.
// It is an unavailable-proof/unsupported-projection guard, not native evidence.
func TestSupervisorControlPeerUnavailableAndUnsupported(t *testing.T) {
	image, err := filepath.Abs("mcphub-peer-fixture.exe")
	if err != nil {
		t.Fatal(err)
	}
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-10-09T00:00:00Z"}
	expected := SupervisorProcessIdentityV1{PID: owner.PID, CreationTime: owner.StartedAt, UserSID: "S-1-5-21-9", ImagePath: image}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	err = ValidateSupervisorControlPeerConnectionV1(client, owner, expected)
	want := ErrSupervisorControlPeerIdentity
	if runtime.GOOS != "windows" {
		want = ErrSupervisorControlPeerUnsupported
	}
	if !errors.Is(err, want) {
		t.Fatalf("unavailable native connection proof=%v, want %v", err, want)
	}
}

// TestDialSupervisorIPCExit_HappyPath drives the production exit IPC
// client against a fake server that emits the canonical
// `graceful_exit_initiated=true` reply. Mirrors the structure of
// TestDialSupervisorIPCStatus_HappyPath so test infrastructure is
// shared (startFakeSupervisorIPCStatusServer, writeSupervisorOwnerForTest).
//
// PR #214 closes the QA-r6-finding-2 bug
// (work-items/bugs/2026-05-19-pr212-no-tests-for-dial-supervisor-ipc-exit.md)
// by adding parallel smoke coverage to the prior status-client tests.
func TestDialSupervisorIPCExit_HappyPath(t *testing.T) {
	stateDir := hardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)

	var seenCmd string
	stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
		seenCmd = req.Cmd
		// Verify the args payload the client sends — graceful:true is
		// the load-bearing field the supervisor handler keys off.
		if grace, _ := req.Args["graceful"].(bool); !grace {
			t.Errorf("req.Args[graceful] = false, want true")
		}
		return IPCResponse{
			ID: req.ID,
			OK: true,
			Result: map[string]any{
				"graceful_exit_initiated": true,
			},
			Final: true,
		}
	})
	defer stop()

	if err := DialSupervisorIPCExit(context.Background(), 5000); err != nil {
		t.Fatalf("DialSupervisorIPCExit: %v", err)
	}
	if seenCmd != "exit" {
		t.Fatalf("server saw cmd=%q, want exit", seenCmd)
	}
}

// TestDialSupervisorIPCExit_NoSupervisor asserts the
// ErrSupervisorIPCUnavailable wrap chain when the lock owner sidecar
// is absent — the same fail-soft shape the GUI's owner.Stop() drains
// to "expected" on supervisor-not-running shutdown.
func TestDialSupervisorIPCExit_NoSupervisor(t *testing.T) {
	stateDir := hardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)

	err := DialSupervisorIPCExit(context.Background(), 5000)
	if err == nil {
		t.Fatal("DialSupervisorIPCExit returned nil error with no supervisor lock owner")
	}
	if !errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Fatalf("err = %v, want errors.Is(ErrSupervisorIPCUnavailable)", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want errors.Is(os.ErrNotExist) chained", err)
	}
}

// TestDialSupervisorIPCExit_HandshakeMismatch asserts the hello-frame
// validation refuses to send the `exit` command when the supervisor's
// hello PID/StartedAt disagree with the lock owner sidecar. Defense-
// in-depth against an attacker-controlled pipe in the (rare) SID-
// resolution-failure fallback path.
func TestDialSupervisorIPCExit_HandshakeMismatch(t *testing.T) {
	stateDir := hardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)

	stop := startFakeSupervisorIPCStatusServer(t, stateDir,
		SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:01Z"},
		func(req IPCRequest) IPCResponse {
			t.Fatalf("client sent exit after handshake mismatch; want close before request")
			return IPCResponse{}
		})
	defer stop()

	err := DialSupervisorIPCExit(context.Background(), 5000)
	if err == nil {
		t.Fatal("DialSupervisorIPCExit returned nil error on handshake mismatch")
	}
	if !strings.Contains(err.Error(), "hello mismatch") {
		t.Fatalf("err = %v, want 'hello mismatch' in error text", err)
	}
}

// TestDialSupervisorIPCExit_NotInitiatedReturnsError covers the
// supervisor-side contract: the response MUST include
// `graceful_exit_initiated=true` for the client to treat shutdown as
// in-progress. A malformed reply (missing field, false value) must
// surface as an error so the GUI owner falls through to force-kill
// rather than waiting for an exit that never comes.
func TestDialSupervisorIPCExit_NotInitiatedReturnsError(t *testing.T) {
	stateDir := hardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)

	stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
		return IPCResponse{
			ID: req.ID,
			OK: true,
			// graceful_exit_initiated absent — equivalent to false per
			// the client's verification.
			Result: map[string]any{},
			Final:  true,
		}
	})
	defer stop()

	err := DialSupervisorIPCExit(context.Background(), 5000)
	if err == nil {
		t.Fatal("DialSupervisorIPCExit returned nil on missing graceful_exit_initiated")
	}
	if !strings.Contains(err.Error(), "graceful_exit_initiated=false") {
		t.Fatalf("err = %v, want error mentioning graceful_exit_initiated=false", err)
	}
}

func TestDialSupervisorIPCExitForOwnerNeverRetargetsSidecar(t *testing.T) {
	original := SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:00Z"}
	replacement := SupervisorLockOwner{PID: 4343, StartedAt: "2026-05-18T10:00:01Z"}
	t.Run("original_connection_with_replacement_sidecar", func(t *testing.T) {
		stateDir := hardenedTempDir(t)
		writeSupervisorOwnerForTest(t, stateDir, replacement)
		var seen atomic.Bool
		stop := startFakeSupervisorIPCStatusServer(t, stateDir, original, func(req IPCRequest) IPCResponse {
			seen.Store(req.Cmd == "exit")
			return IPCResponse{ID: req.ID, OK: true, Final: true, Result: map[string]any{"graceful_exit_initiated": true}}
		})
		defer stop()
		if err := DialSupervisorIPCExitForOwner(context.Background(), stateDir, original, 5000); err != nil {
			t.Fatal(err)
		}
		if !seen.Load() {
			t.Fatal("original bound connection did not receive its graceful exit")
		}
		current, err := ReadSupervisorLockOwner(stateDir + string(os.PathSeparator) + "supervisor.lock")
		if err != nil || current != replacement {
			t.Fatal("bound exit rewrote current replacement metadata")
		}
	})
	t.Run("replacement_connection_refuses_before_exit", func(t *testing.T) {
		stateDir := hardenedTempDir(t)
		writeSupervisorOwnerForTest(t, stateDir, replacement)
		stop := startFakeSupervisorIPCStatusServer(t, stateDir, replacement, func(IPCRequest) IPCResponse {
			t.Error("replacement received a destructive frame from the original binding")
			return IPCResponse{}
		})
		defer stop()
		if err := DialSupervisorIPCExitForOwner(context.Background(), stateDir, original, 5000); err == nil || !strings.Contains(err.Error(), "hello mismatch") {
			t.Fatal("replacement hello did not refuse the original expected generation")
		}
	})
}

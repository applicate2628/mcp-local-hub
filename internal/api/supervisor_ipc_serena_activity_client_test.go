package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

func TestDecodeLazyProxyReadinessResponse_RequiresCorrelatedFinalReceipt(t *testing.T) {
	event := LazyProxyReadinessEventV1{ProtocolVersion: 1, TaskName: `\mcp-local-hub-lsp-abcdef12-go`, WrapperPID: 42, WrapperStartedAt: "2026-09-12T16:00:00Z", BackendEpoch: 2, Transition: "ready", ObservedAt: "2026-09-12T16:00:01Z"}
	receipt := func(disposition string, generation int, epoch uint64, transition string) json.RawMessage {
		b, _ := json.Marshal(LazyProxyReadinessReceiptV1{Disposition: disposition, WrapperPID: 42, WrapperStartedAt: event.WrapperStartedAt, PIDGeneration: generation, BackendEpoch: epoch, Transition: transition})
		return b
	}
	for _, tc := range []struct {
		name string
		resp supervisorIPCRawResponse
		ok   bool
	}{
		{"accepted", supervisorIPCRawResponse{ID: 7, OK: true, Final: true, Result: receipt("accepted", 3, 2, "ready")}, true},
		{"duplicate", supervisorIPCRawResponse{ID: 7, OK: true, Final: true, Result: receipt("duplicate", 3, 2, "ready")}, true},
		{"superseded newer", supervisorIPCRawResponse{ID: 7, OK: true, Final: true, Result: receipt("superseded", 3, 3, "starting")}, true},
		{"superseded newer unknown transition", supervisorIPCRawResponse{ID: 7, OK: true, Final: true, Result: receipt("superseded", 3, 3, "unknown")}, false},
		{"superseded newer empty transition", supervisorIPCRawResponse{ID: 7, OK: true, Final: true, Result: receipt("superseded", 3, 3, "")}, false},
		{"missing generation", supervisorIPCRawResponse{ID: 7, OK: true, Final: true, Result: receipt("accepted", 0, 2, "ready")}, false},
		{"wrong epoch", supervisorIPCRawResponse{ID: 7, OK: true, Final: true, Result: receipt("accepted", 3, 3, "ready")}, false},
		{"wrong transition", supervisorIPCRawResponse{ID: 7, OK: true, Final: true, Result: receipt("accepted", 3, 2, "starting")}, false},
		{"wrong incarnation", supervisorIPCRawResponse{ID: 7, OK: true, Final: true, Result: json.RawMessage(`{"disposition":"accepted","wrapper_pid":43,"wrapper_started_at":"2026-09-12T16:00:00Z","pid_generation":3,"backend_epoch":2,"transition":"ready"}`)}, false},
		{"wrong id", supervisorIPCRawResponse{ID: 8, OK: true, Final: true, Result: receipt("accepted", 3, 2, "ready")}, false},
		{"not final", supervisorIPCRawResponse{ID: 7, OK: true, Result: receipt("accepted", 3, 2, "ready")}, false},
		{"unknown command", supervisorIPCRawResponse{ID: 7, Error: &IPCErr{Code: "UNKNOWN_COMMAND"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeLazyProxyReadinessResponse(IPCRequest{ID: 7}, event, tc.resp)
			if tc.ok && err != nil {
				t.Fatalf("err = %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("unexpected receipt acceptance")
			}
		})
	}
}

func TestDialSupervisorIPCCommitLazyProxyReadiness_CancelClosesBlockedRead(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-09-12T16:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)
	requestRead := make(chan struct{})
	peerClosed := make(chan struct{})
	stop := startFakeSupervisorIPCStatusServerWithConnection(t, stateDir, func(conn net.Conn) {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		hello, _ := json.Marshal(map[string]any{"hello": IPCHello{Version: 1, PID: owner.PID, StartedAt: owner.StartedAt}})
		if _, err := conn.Write(append(hello, '\n')); err != nil {
			return
		}
		line, err := readTestIPCLine(conn, 4096)
		if err != nil {
			return
		}
		var req IPCRequest
		if json.Unmarshal(line, &req) != nil || req.Cmd != "commit_lazy_proxy_readiness" {
			return
		}
		close(requestRead)
		var one [1]byte
		_, _ = conn.Read(one[:])
		close(peerClosed)
	})
	defer stop()

	ctx, cancel := context.WithTimeout(t.Context(), 750*time.Millisecond)
	defer cancel()
	event := LazyProxyReadinessEventV1{
		ProtocolVersion: 1, TaskName: `\mcp-local-hub-lsp-abcdef12-go`,
		WrapperPID: 42, WrapperStartedAt: "2026-09-12T16:00:00Z",
		BackendEpoch: 1, Transition: "starting", ObservedAt: "2026-09-12T16:00:01Z",
	}
	result := make(chan error, 1)
	go func() {
		_, err := dialSupervisorIPCCommitLazyProxyReadinessFromStateDir(ctx, stateDir, event)
		result <- err
	}()
	select {
	case <-requestRead:
	case <-time.After(time.Second):
		t.Fatal("fake supervisor did not receive readiness request")
	}
	cancelStarted := time.Now()
	cancel()
	select {
	case err := <-result:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(cancelStarted); elapsed > 200*time.Millisecond {
			t.Fatalf("cancel took %s, want active connection close", elapsed)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("cancel did not unblock readiness receipt read")
	}
	select {
	case <-peerClosed:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("fake supervisor did not observe the canceled client connection close")
	}
}

func TestDecodeSerenaActivityCommitResponse_UnknownCommandIsExplicitlyUnsupported(t *testing.T) {
	_, err := decodeSerenaActivityCommitResponse(
		IPCRequest{ID: 7},
		SerenaActivityCommitRequestV1{},
		supervisorIPCRawResponse{ID: 7, Error: &IPCErr{Code: "UNKNOWN_COMMAND", Message: "unknown IPC command: commit_serena_activity"}},
	)
	if !errors.Is(err, ErrSerenaActivityCommitUnsupported) {
		t.Fatalf("err = %v, want explicit mixed-version unsupported error", err)
	}
}

func TestValidateSerenaActivityCommitRequest_LegacyRequiresExplicitMarker(t *testing.T) {
	base := SerenaActivityCommitRequestV1{ProtocolVersion: 1, WorkspaceKey: "key", WorkspacePath: "/workspace", TaskName: `\mcp-local-hub-serena-key`, ExpectedPort: 9301, ActivityAt: time.Now().UTC()}
	if err := validateSerenaActivityCommitRequest(base); err == nil {
		t.Fatal("zero generation without explicit legacy marker must be rejected")
	}
	base.LegacyGenerationUnspecified = true
	if err := validateSerenaActivityCommitRequest(base); err != nil {
		t.Fatalf("explicit legacy generation request rejected: %v", err)
	}
	base.RegisteredAt = time.Now().UTC()
	if err := validateSerenaActivityCommitRequest(base); err == nil {
		t.Fatal("legacy marker with nonzero generation must be rejected")
	}
}

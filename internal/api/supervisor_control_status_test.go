package api

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

func TestSupervisorControlStatusExplicitAdmission(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-10-09T00:00:00Z"}
	withDaemonStateRootOverride(t, stateDir)
	writeSupervisorOwnerForTest(t, stateDir, owner)
	var contacted atomic.Int32
	stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
		contacted.Add(1)
		return IPCResponse{ID: req.ID, OK: true, Result: map[string]any{"state": "running", "daemons": []any{}}}
	})
	defer stop()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		dir   string
		owner SupervisorLockOwner
	}{
		{"nil-context", nil, stateDir, owner}, {"cancelled", canceled, stateDir, owner},
		{"empty-namespace", context.Background(), "", owner}, {"relative-namespace", context.Background(), "relative", owner},
		{"zero-owner", context.Background(), stateDir, SupervisorLockOwner{}},
		{"missing-start", context.Background(), stateDir, SupervisorLockOwner{PID: 4242}},
		{"malformed-start", context.Background(), stateDir, SupervisorLockOwner{PID: 4242, StartedAt: "bad"}},
		{"zero-start", context.Background(), stateDir, SupervisorLockOwner{PID: 4242, StartedAt: time.Time{}.Format(time.RFC3339Nano)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifySupervisorControlGenerationFromStateDir(tc.ctx, tc.dir, tc.owner); err == nil {
				t.Fatal("unbound explicit status admitted")
			}
		})
	}
	if contacted.Load() != 0 {
		t.Fatal("invalid explicit input contacted the ambient owner")
	}
}

func TestSupervisorControlStatusSingleFrameQualification(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		result                     any
		wrongID, wrongHello, nonOK bool
		wantErr                    bool
	}{
		{"empty-array", map[string]any{"state": "running", "daemons": []any{}}, false, false, false, false},
		{"empty-null", map[string]any{"state": "running", "daemons": nil}, false, false, false, false},
		{"null-result", json.RawMessage("null"), false, false, false, true},
		{"missing-result", nil, false, false, false, true}, {"empty-object", map[string]any{}, false, false, false, true},
		{"missing-daemons", map[string]any{"state": "running"}, false, false, false, true},
		{"missing-state", map[string]any{"daemons": []any{}}, false, false, false, true},
		{"accepted-stream-frame", map[string]any{"accepted": true}, false, false, false, true},
		{"wrong-state", map[string]any{"state": "stopped", "daemons": []any{}}, false, false, false, true},
		{"type-invalid-state", map[string]any{"state": 1, "daemons": []any{}}, false, false, false, true},
		{"malformed-daemons", map[string]any{"state": "running", "daemons": true}, false, false, false, true},
		{"wrong-id", map[string]any{"state": "running", "daemons": []any{}}, true, false, false, true},
		{"wrong-hello", map[string]any{"state": "running", "daemons": []any{}}, false, true, false, true},
		{"non-ok", map[string]any{"state": "running", "daemons": []any{}}, false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := apitest.HardenedTempDir(t)
			owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-10-09T00:00:00Z"}
			writeSupervisorOwnerForTest(t, stateDir, owner)
			hello := owner
			if tc.wrongHello {
				hello.StartedAt = "2026-10-09T00:00:01Z"
			}
			stop := startFakeSupervisorIPCStatusServer(t, stateDir, hello, func(req IPCRequest) IPCResponse {
				id := req.ID
				if tc.wrongID {
					id++
				}
				return IPCResponse{ID: id, OK: !tc.nonOK, Result: tc.result} // Final deliberately omitted: one status frame.
			})
			defer stop()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := VerifySupervisorControlGenerationFromStateDir(ctx, stateDir, owner)
			if (err != nil) != tc.wantErr {
				t.Fatalf("control qualification err=%v", err)
			}
		})
	}
	// The ordinary status row contract remains independent of control acceptance.
	for _, raw := range []string{"null", "{}"} {
		if rows, err := decodeSupervisorIPCStatusResult([]byte(raw)); err != nil || len(rows) != 0 {
			t.Fatalf("ordinary %s rows=%v err=%v", raw, rows, err)
		}
	}
}

func TestSupervisorControlStatusOwnerMismatchAndMissingReply(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-10-09T00:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, SupervisorLockOwner{PID: 4243, StartedAt: owner.StartedAt})
	if err := VerifySupervisorControlGenerationFromStateDir(context.Background(), stateDir, owner); err == nil {
		t.Fatal("replacement sidecar admitted")
	}
	writeSupervisorOwnerForTest(t, stateDir, owner)
	stop := startFakeSupervisorIPCStatusServerWithConnection(t, stateDir, func(conn net.Conn) {
		frame, _ := json.Marshal(map[string]any{"hello": IPCHello{Version: 1, PID: owner.PID, StartedAt: owner.StartedAt}})
		if _, err := conn.Write(append(frame, '\n')); err != nil {
			return
		}
		_, _ = readTestIPCLine(conn, 4096) // authenticated status request receives no reply.
	})
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := VerifySupervisorControlGenerationFromStateDir(ctx, stateDir, owner); err == nil {
		t.Fatal("missing reply admitted")
	}
}

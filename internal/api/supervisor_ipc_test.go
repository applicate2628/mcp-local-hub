package api

import (
	"encoding/json"
	"testing"
)

func TestIPCRequest_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		req  IPCRequest
	}{
		{"status", IPCRequest{ID: 42, Cmd: "status"}},
		{"exit-graceful", IPCRequest{ID: 43, Cmd: "exit", Args: map[string]any{"graceful": true, "timeout_ms": 5000.0}}},
		{"restart", IPCRequest{ID: 44, Cmd: "restart", Args: map[string]any{"server": "memory", "daemon": "default"}}},
		{"quiesce-timers", IPCRequest{ID: 46, Cmd: "quiesce-timers", Args: map[string]any{"timeout_ms": 30000.0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.req)
			if err != nil {
				t.Fatal(err)
			}
			var got IPCRequest
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.Cmd != tc.req.Cmd || got.ID != tc.req.ID {
				t.Fatalf("mismatch: got %+v want %+v", got, tc.req)
			}
		})
	}
}

func TestIPCHandshake_Validation(t *testing.T) {
	hello := IPCHello{Version: 1, PID: 12345, StartedAt: "2026-05-16T18:00:00Z"}
	expected := SupervisorLockOwner{PID: 12345, StartedAt: "2026-05-16T18:00:00Z"}
	if !ValidateHandshake(hello, expected) {
		t.Fatal("matching expectations should validate")
	}
	expected.PID = 99999
	if ValidateHandshake(hello, expected) {
		t.Fatal("PID mismatch should fail validation")
	}
}

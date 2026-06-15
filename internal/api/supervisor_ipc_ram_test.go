package api

import (
	"encoding/json"
	"testing"
)

// TestDecodeSupervisorIPCStatusResult_RAMRoundTrip asserts the supervisor IPC
// status decoder carries ram_bytes from the wire payload (emitted by the
// supervisor's per-pid GetProcessMemoryInfo lookup) through to
// DaemonStatus.RAMBytes — the field /api/status and the GUI Dashboard read.
// Without the wire field on supervisorIPCStatusDaemon, json.Unmarshal would
// silently drop ram_bytes (mirrors the OrphanPID/StalePID precedent).
//
// Pure synthetic payload — no live supervisor, scheduler, registry, or state
// path is touched.
func TestDecodeSupervisorIPCStatusResult_RAMRoundTrip(t *testing.T) {
	result := supervisorIPCStatusResult{
		State: "running",
		Daemons: []supervisorIPCStatusDaemon{
			{
				TaskName:   `\mcp-local-hub-memory-default`,
				Server:     "memory",
				Daemon:     "default",
				State:      "running",
				CurrentPID: 4242,
				RAMBytes:   48 * 1024 * 1024,
			},
			{
				// A daemon with no RAM figure (lookup failed / non-Windows):
				// ram_bytes omitted on the wire → RAMBytes decodes to 0.
				TaskName: `\mcp-local-hub-time-default`,
				Server:   "time",
				Daemon:   "default",
				State:    "running",
			},
		},
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal synthetic result: %v", err)
	}
	rows, err := decodeSupervisorIPCStatusResult(raw)
	if err != nil {
		t.Fatalf("decodeSupervisorIPCStatusResult: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows len = %d, want 2", len(rows))
	}
	byServer := map[string]DaemonStatus{}
	for _, r := range rows {
		byServer[r.Server] = r
	}
	if got := byServer["memory"].RAMBytes; got != uint64(48*1024*1024) {
		t.Fatalf("memory RAMBytes = %d, want %d", got, 48*1024*1024)
	}
	if got := byServer["time"].RAMBytes; got != 0 {
		t.Fatalf("time RAMBytes = %d, want 0 (absent on the wire)", got)
	}
}

// TestSupervisorIPCStatusDaemon_RAMBytesOmitemptyOnWire asserts ram_bytes is
// omitted from the JSON envelope when zero, so a daemon with no RAM figure
// does not inflate the status payload. (omitempty contract.)
func TestSupervisorIPCStatusDaemon_RAMBytesOmitemptyOnWire(t *testing.T) {
	zero, err := json.Marshal(supervisorIPCStatusDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory",
		Daemon:   "default",
	})
	if err != nil {
		t.Fatalf("marshal zero-RAM daemon: %v", err)
	}
	if got := string(zero); containsField(got, "ram_bytes") {
		t.Fatalf("ram_bytes present in zero-RAM envelope (should be omitempty): %s", got)
	}
	nonZero, err := json.Marshal(supervisorIPCStatusDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory",
		Daemon:   "default",
		RAMBytes: 1234,
	})
	if err != nil {
		t.Fatalf("marshal non-zero-RAM daemon: %v", err)
	}
	if got := string(nonZero); !containsField(got, "ram_bytes") {
		t.Fatalf("ram_bytes missing from non-zero envelope: %s", got)
	}
}

func containsField(jsonStr, field string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		return false
	}
	_, ok := m[field]
	return ok
}

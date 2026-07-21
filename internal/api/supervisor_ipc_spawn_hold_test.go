package api

import (
	"testing"
)

// TestDecodeSupervisorIPCStatusResult_SpawnHoldRoundTrip is the CONSUMER half
// of the pre-spawn existence-gate (P1.1) delivery chain.
//
// WHY IT EXISTS. The gate's whole value is delivery: the supervisor already
// knew the cause in the incident, and the operator still lost a day because
// nothing carried it to a surface they read. A review mutation deleted the
// SpawnHoldReason/SpawnHoldPath mapping in decodeSupervisorIPCStatusResult and
// the ENTIRE suite stayed green — the exact json.Unmarshal silent-drop trap
// this struct's own comments warn about. A future refactor of this seam would
// regress straight back to the incident's failure mode: supervisor knows,
// Dashboard shows an unexplained dead fleet, every test passes.
//
// The payload is a RAW JSON LITERAL, deliberately NOT the typed
// supervisorIPCStatusDaemon struct the ram_bytes precedent uses. Building the
// payload from the struct would make the test blind to a struct-TAG rename
// (marshal and unmarshal would drift together and still agree). Spelling the
// wire keys by hand pins the actual over-the-wire contract — the same strings
// the producer writes in internal/cli/supervise_status.go.
func TestDecodeSupervisorIPCStatusResult_SpawnHoldRoundTrip(t *testing.T) {
	raw := []byte(`{
	  "state": "running",
	  "daemons": [
	    {
	      "task_name": "\\mcp-local-hub-memory-default",
	      "server": "memory",
	      "daemon": "default",
	      "state": "backoff",
	      "spawn_hold_reason": "missing-binary",
	      "spawn_hold_path": "C:\\Users\\dev\\.local\\bin\\mcphub.exe"
	    },
	    {
	      "task_name": "\\mcp-local-hub-time-default",
	      "server": "time",
	      "daemon": "default",
	      "state": "running",
	      "current_pid": 4242
	    }
	  ]
	}`)

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

	const wantPath = `C:\Users\dev\.local\bin\mcphub.exe`
	held := byServer["memory"]
	if held.SpawnHoldReason != "missing-binary" {
		t.Fatalf("SpawnHoldReason = %q, want %q — the hold reason was dropped between the wire and DaemonStatus, so the GUI would show an unexplained stopped daemon",
			held.SpawnHoldReason, "missing-binary")
	}
	if held.SpawnHoldPath != wantPath {
		t.Fatalf("SpawnHoldPath = %q, want %q — without the path the operator is told something is missing but not WHAT",
			held.SpawnHoldPath, wantPath)
	}

	// A healthy daemon must decode to empty, never to a phantom hold.
	healthy := byServer["time"]
	if healthy.SpawnHoldReason != "" || healthy.SpawnHoldPath != "" {
		t.Fatalf("healthy daemon decoded a hold (%q / %q); absent wire fields must stay empty",
			healthy.SpawnHoldReason, healthy.SpawnHoldPath)
	}
}

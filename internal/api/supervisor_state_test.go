package api

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSupervisorState_RoundTrip(t *testing.T) {
	// v0.5.0 Fix Group 5: WriteSupervisorState now flows through
	// the hardened secure-write pipeline. See the matching note in
	// supervisor_intent_test.go for why hardenedTempDir is required.
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-state.json")

	want := SupervisorStateFile{
		Version: 1,
		Daemons: map[string]SupervisorDaemonState{
			`\mcp-local-hub-memory-default`: {
				State:         "running",
				CurrentPID:    12345,
				PIDGeneration: 7,
				StartedAt:     "2026-05-16T18:00:00.000000000Z",
			},
		},
		TransientPIDs: []TransientPID{
			{PID: 23456, Kind: "workspace-weekly-refresh", StartedAt: "2026-05-16T03:00:00.000000000Z"},
		},
		MaintenanceFiredAt: map[string]string{
			"workspace-weekly-refresh": "2026-05-15T03:00:00.000000000Z",
		},
	}
	if err := WriteSupervisorState(path, &want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSupervisorState(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Daemons[`\mcp-local-hub-memory-default`].PIDGeneration != 7 {
		t.Fatalf("pid_generation lost")
	}
	if len(got.TransientPIDs) != 1 {
		t.Fatalf("transient_pids lost")
	}
}

// TestSupervisorState_IgnoresRemovedRestartPolicyFields proves a
// pre-audit-P3 supervisor-state.json carrying restart_history /
// backoff_until / quarantine_since / queued_action still reads cleanly
// after those fields were removed from the Go struct: they fall under the
// "ignore unknown fields" contract, so an in-place binary downgrade or a
// stale on-disk file never bricks supervisor startup. The known fields are
// preserved.
func TestSupervisorState_IgnoresRemovedRestartPolicyFields(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-state.json")
	body := `{"version":1,"daemons":{"\\mcp-local-hub-memory-default":{` +
		`"state":"backoff-waiting","current_pid":0,"pid_generation":4,` +
		`"restart_history":[{"at":"2026-06-09T09:50:00.000000000Z","exit_code":1}],` +
		`"backoff_until":"2026-06-09T10:05:00.000000000Z",` +
		`"quarantine_since":"2026-06-09T09:30:00.000000000Z",` +
		`"queued_action":{"kind":"respawn","reason":"manual-restart"}}}}`
	if err := WriteStateFileAtomic(path, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSupervisorState(path)
	if err != nil {
		t.Fatalf("read state with removed restart-policy fields: %v", err)
	}
	daemon := got.Daemons[`\mcp-local-hub-memory-default`]
	if daemon.State != "backoff-waiting" || daemon.PIDGeneration != 4 {
		t.Fatalf("known daemon fields lost: %+v", daemon)
	}
}

func TestSupervisorState_IgnoresUnknownFields(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-state.json")
	body := `{"version":1,"daemons":{"\\mcp-local-hub-memory-default":{"state":"running","current_pid":12345,"job_protection":false,"future_daemon_field":"x"}},"transient_pids":[],"future_top_level":"x"}`
	if err := WriteStateFileAtomic(path, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSupervisorState(path)
	if err != nil {
		t.Fatalf("read with unknown fields: %v", err)
	}
	daemon := got.Daemons[`\mcp-local-hub-memory-default`]
	if daemon.State != "running" || daemon.CurrentPID != 12345 {
		t.Fatalf("known daemon fields lost: %+v", daemon)
	}
	if daemon.JobProtection == nil || *daemon.JobProtection {
		t.Fatalf("known job_protection field lost: %+v", daemon.JobProtection)
	}
}

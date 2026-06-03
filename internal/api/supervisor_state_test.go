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
				State:          "running",
				CurrentPID:     12345,
				PIDGeneration:  7,
				StartedAt:      "2026-05-16T18:00:00.000000000Z",
				RestartHistory: []RestartEvent{{At: "2026-05-16T17:50:00.000000000Z", ExitCode: 1}},
				BackoffUntil:   "",
				QueuedAction:   nil,
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

func TestSupervisorState_QueuedActionRoundTrip(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-state.json")
	respawn := QueuedAction{Kind: "respawn", Reason: "manual_restart"}
	want := SupervisorStateFile{
		Version: 1,
		Daemons: map[string]SupervisorDaemonState{
			`\mcp-local-hub-memory-default`: {
				State:        "exiting",
				QueuedAction: &respawn,
			},
		},
	}
	if err := WriteSupervisorState(path, &want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSupervisorState(path)
	if err != nil {
		t.Fatal(err)
	}
	qa := got.Daemons[`\mcp-local-hub-memory-default`].QueuedAction
	if qa == nil || qa.Kind != "respawn" {
		t.Fatalf("queued_action lost: %+v", qa)
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

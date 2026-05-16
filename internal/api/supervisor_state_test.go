package api

import (
	"path/filepath"
	"testing"
)

func TestSupervisorState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
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
	dir := t.TempDir()
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

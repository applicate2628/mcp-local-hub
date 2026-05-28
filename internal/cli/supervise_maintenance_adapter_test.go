package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestMaintenanceStateAdapter_PreservesTrackerStateAcrossMaintenanceWrite(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")

	tracker := NewDaemonRuntimeTracker()
	started := time.Date(2026, 5, 17, 4, 0, 0, 0, time.UTC)
	tracker.MarkSpawned(`\mcp-local-hub-memory-default`, 4242, started)
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("seed tracker state: %v", err)
	}

	adapter := newMaintenanceStateAdapter(statePath, nil)
	adapter.SetMaintenanceFiredAt("workspace-weekly-refresh", "2026-05-17T04:00:00Z")
	adapter.AddTransientPID(api.TransientPID{
		PID:       7777,
		Kind:      "workspace-weekly-refresh",
		StartedAt: "2026-05-17T04:00:00Z",
	})

	got, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if got.MaintenanceFiredAt["workspace-weekly-refresh"] != "2026-05-17T04:00:00Z" {
		t.Fatalf("maintenance_fired_at not persisted: %+v", got.MaintenanceFiredAt)
	}
	if len(got.TransientPIDs) != 1 || got.TransientPIDs[0].PID != 7777 {
		t.Fatalf("transient_pids not persisted: %+v", got.TransientPIDs)
	}
	if got.Daemons[`\mcp-local-hub-memory-default`].CurrentPID != 4242 {
		t.Fatalf("daemon runtime state was clobbered: %+v", got.Daemons)
	}
}

func TestMaintenanceStateAdapter_TrackerPersistPreservesMaintenanceState(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")

	adapter := newMaintenanceStateAdapter(statePath, nil)
	adapter.SetMaintenanceFiredAt("server-weekly-refresh", "2026-05-17T04:00:00Z")
	adapter.AddTransientPID(api.TransientPID{
		PID:       8888,
		Kind:      "server-weekly-refresh",
		StartedAt: "2026-05-17T04:00:00Z",
	})

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(`\mcp-local-hub-serena-default`, 5252, time.Date(2026, 5, 17, 4, 5, 0, 0, time.UTC))
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("persist tracker state: %v", err)
	}

	got, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if got.MaintenanceFiredAt["server-weekly-refresh"] != "2026-05-17T04:00:00Z" {
		t.Fatalf("tracker persist lost maintenance_fired_at: %+v", got.MaintenanceFiredAt)
	}
	if len(got.TransientPIDs) != 1 || got.TransientPIDs[0].PID != 8888 {
		t.Fatalf("tracker persist lost transient_pids: %+v", got.TransientPIDs)
	}
	if got.Daemons[`\mcp-local-hub-serena-default`].CurrentPID != 5252 {
		t.Fatalf("tracker daemon state missing: %+v", got.Daemons)
	}
}

func TestMaintenanceStateAdapter_RemoveTransientPIDRemovesAllMatches(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	adapter := newMaintenanceStateAdapter(statePath, nil)

	adapter.AddTransientPID(api.TransientPID{PID: 0, Kind: "workspace-weekly-refresh", StartedAt: "2026-05-17T04:00:00Z"})
	adapter.AddTransientPID(api.TransientPID{PID: 0, Kind: "server-weekly-refresh", StartedAt: "2026-05-17T04:00:01Z"})
	adapter.AddTransientPID(api.TransientPID{PID: 9999, Kind: "server-weekly-refresh", StartedAt: "2026-05-17T04:00:02Z"})
	adapter.RemoveTransientPID(0)

	got, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if len(got.TransientPIDs) != 1 || got.TransientPIDs[0].PID != 9999 {
		t.Fatalf("RemoveTransientPID must remove all matching PIDs only; got %+v", got.TransientPIDs)
	}
}

func TestMaintenanceSpawner_WaitTreatsExitCodeAsCleanProcessExit(t *testing.T) {
	sp := newMaintenanceSpawner(nil)
	timer := api.MaintenanceTimer{
		Name:    `\mcp-local-hub-maintenance-exit-code-test`,
		Kind:    "workspace-weekly-refresh",
		Command: shellPathForExitCodeTest(),
		Args:    shellArgsForExitCodeTest(7),
	}

	pid, err := sp.Start(timer)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("pid = %d, want > 0", pid)
	}
	if err := sp.Wait(pid); err != nil {
		t.Fatalf("Wait must return nil for an observed nonzero process exit code; got %v", err)
	}
}

func TestMaintenanceSpawner_StartFailureEmitsWarnAuditEvent(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	sp := newMaintenanceSpawner(events)
	_, err = sp.Start(api.MaintenanceTimer{
		Name:    `\mcp-local-hub-maintenance-missing-command-test`,
		Kind:    "workspace-weekly-refresh",
		Command: filepath.Join(stateDir, "definitely-missing-maintenance-command.exe"),
	})
	if err == nil {
		t.Fatal("Start returned nil for missing command")
	}

	raw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	if !strings.Contains(string(raw), `"event":"maintenance-spawn-failed"`) {
		t.Fatalf("maintenance-spawn-failed event missing from log:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"severity":"warn"`) {
		t.Fatalf("spawn failure must be warn severity:\n%s", raw)
	}
}

func shellPathForExitCodeTest() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "sh"
}

func shellArgsForExitCodeTest(code int) []string {
	if runtime.GOOS == "windows" {
		return []string{"/c", "exit", strconv.Itoa(code)}
	}
	return []string{"-c", "exit " + strconv.Itoa(code)}
}

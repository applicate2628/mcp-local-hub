package cli

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestDaemonRuntimeTracker_LifecycleTransitions(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	taskName := `mcp-local-hub-memory-default`
	startedAt := time.Date(2026, 5, 18, 10, 0, 0, 123456789, time.UTC)

	if _, ok := tracker.Get(taskName); ok {
		t.Fatal("new tracker unexpectedly has an entry")
	}

	tracker.MarkSpawned(taskName, 4321, startedAt)
	entry, ok := tracker.Get(`\mcp-local-hub-memory-default`)
	if !ok {
		t.Fatal("spawned entry missing")
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 4321 || entry.PIDGeneration != 1 {
		t.Fatalf("spawned entry = %+v, want running pid=4321 pid_generation=1", entry)
	}
	if !entry.StartedAt.Equal(startedAt) {
		t.Fatalf("started_at = %s, want %s", entry.StartedAt, startedAt)
	}

	tracker.MarkExited(taskName)
	entry, ok = tracker.Get(taskName)
	if !ok {
		t.Fatal("exited entry missing")
	}
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 || !entry.StartedAt.IsZero() || entry.PIDGeneration != 1 {
		t.Fatalf("exited entry = %+v, want idle pid=0 zero started_at generation preserved", entry)
	}

	spawnErr := errors.New("spawn failed")
	tracker.MarkSpawnFailed(taskName, spawnErr)
	entry, ok = tracker.Get(taskName)
	if !ok {
		t.Fatal("spawn-failed entry missing")
	}
	if entry.State != daemonRuntimeStateBackoff || entry.CurrentPID != 0 || entry.LastError != spawnErr.Error() || entry.RestartCount == 0 {
		t.Fatalf("spawn-failed entry = %+v, want backoff pid=0 last_error and restart_count", entry)
	}

	tracker.MarkSpawned(taskName, 9876, startedAt.Add(time.Minute))
	entry, ok = tracker.Get(taskName)
	if !ok {
		t.Fatal("respawned entry missing")
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 9876 || entry.PIDGeneration != 2 || entry.LastError != "" {
		t.Fatalf("respawned entry = %+v, want running pid=9876 generation=2 and cleared last_error", entry)
	}

	tracker.MarkTerminated(taskName)
	entry, ok = tracker.Get(taskName)
	if !ok {
		t.Fatal("terminated entry missing")
	}
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 || !entry.StartedAt.IsZero() {
		t.Fatalf("terminated entry = %+v, want idle pid=0 zero started_at", entry)
	}
}

func TestDaemonRuntimeTracker_ClearsJobProtectionWhenNoCurrentSpawn(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	taskName := `mcp-local-hub-memory-default`
	unprotected := false
	startedAt := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)

	tracker.MarkJobProtection(taskName, &unprotected)
	tracker.MarkSpawned(taskName, 4321, startedAt)
	entry, ok := tracker.Get(taskName)
	if !ok {
		t.Fatal("spawned entry missing")
	}
	if entry.JobProtection == nil || *entry.JobProtection != false {
		t.Fatalf("running entry JobProtection = %v, want explicit false", entry.JobProtection)
	}

	for _, tt := range []struct {
		name string
		mark func()
	}{
		{"spawn-failed", func() { tracker.MarkSpawnFailed(taskName, errors.New("spawn failed")) }},
		{"spawn-failed-preserve-pid", func() { tracker.MarkSpawnFailedPreservePID(taskName, errors.New("orphan"), 9876) }},
		{"exited", func() { tracker.MarkExited(taskName) }},
		{"backoff", func() { tracker.MarkBackoff(taskName) }},
		{"quarantined", func() { tracker.MarkQuarantined(taskName) }},
		{"terminated", func() { tracker.MarkTerminated(taskName) }},
	} {
		tracker.MarkJobProtection(taskName, &unprotected)
		tracker.MarkSpawned(taskName, 4321, startedAt)
		tt.mark()
		entry, ok := tracker.Get(taskName)
		if !ok {
			t.Fatalf("%s: entry missing", tt.name)
		}
		if entry.JobProtection != nil {
			t.Fatalf("%s: JobProtection = %v, want nil when no current spawn is running", tt.name, *entry.JobProtection)
		}
	}
}

func TestDaemonRuntimeTracker_PersistAndHydrate(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{},
		TransientPIDs: []api.TransientPID{
			{PID: 2222, Kind: "workspace-weekly-refresh", StartedAt: "2026-05-18T09:00:00Z"},
		},
		MaintenanceFiredAt: map[string]string{"workspace-weekly-refresh": "2026-05-18T09:00:00Z"},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	startedAt := time.Date(2026, 5, 18, 10, 0, 0, 123456789, time.UTC)
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(`\mcp-local-hub-memory-default`, 4321, startedAt)
	tracker.MarkSpawnFailed(`\mcp-local-hub-serena-codex`, errors.New("boom"))

	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("persist tracker: %v", err)
	}

	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted supervisor-state.json: %v", err)
	}
	mem := persisted.Daemons[`\mcp-local-hub-memory-default`]
	if mem.State != "running" || mem.CurrentPID != 4321 || mem.PIDGeneration != 1 || mem.StartedAt != startedAt.Format(time.RFC3339Nano) {
		t.Fatalf("persisted memory state = %+v, want running pid=4321 generation=1 started_at", mem)
	}
	serena := persisted.Daemons[`\mcp-local-hub-serena-codex`]
	if serena.State != "backoff-waiting" || serena.CurrentPID != 0 {
		t.Fatalf("persisted serena state = %+v, want backoff-waiting pid=0", serena)
	}
	if len(persisted.TransientPIDs) != 1 || persisted.MaintenanceFiredAt["workspace-weekly-refresh"] == "" {
		t.Fatalf("persist lost non-daemon state: %+v", persisted)
	}

	hydrated := NewDaemonRuntimeTracker()
	hydrated.HydrateFromState(persisted)
	entry, ok := hydrated.Get(`\mcp-local-hub-memory-default`)
	if !ok {
		t.Fatal("hydrated memory entry missing")
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 4321 || entry.PIDGeneration != 1 || !entry.StartedAt.Equal(startedAt) {
		t.Fatalf("hydrated memory entry = %+v, want running pid=4321 generation=1 started_at", entry)
	}
	entry, ok = hydrated.Get(`\mcp-local-hub-serena-codex`)
	if !ok {
		t.Fatal("hydrated serena entry missing")
	}
	if entry.State != daemonRuntimeStateBackoff || entry.CurrentPID != 0 {
		t.Fatalf("hydrated serena entry = %+v, want backoff pid=0", entry)
	}
}

func TestSupervisorCleanStartHydratesFromExistingState(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	startedAt := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			`\mcp-local-hub-memory-default`: {
				State:         "running",
				CurrentPID:    4321,
				PIDGeneration: 7,
				StartedAt:     startedAt.Format(time.RFC3339Nano),
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	tracker, err := loadDaemonRuntimeTrackerFromStatePath(statePath)
	if err != nil {
		t.Fatalf("load runtime tracker: %v", err)
	}
	entry, ok := tracker.Get(`\mcp-local-hub-memory-default`)
	if !ok {
		t.Fatal("hydrated runtime entry missing")
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 4321 || entry.PIDGeneration != 7 || !entry.StartedAt.Equal(startedAt) {
		t.Fatalf("hydrated runtime entry = %+v, want running pid=4321 generation=7 started_at", entry)
	}
}

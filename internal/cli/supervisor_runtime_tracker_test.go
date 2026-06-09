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

// TestDaemonRuntimeTracker_PersistPreservesFileOwnedFields is the Codex
// deep-sec PR #268 Conc-F1 guard: PersistTo rebuilds file.Daemons
// wholesale from the in-memory tracker, which models only State /
// CurrentPID / PIDGeneration / OrphanPID / JobProtection / StartedAt. The
// four file-owned fields the tracker does NOT model —
// RestartHistory / BackoffUntil / QuarantineSince / QueuedAction — must be
// preserved from the existing on-disk row across a persist that updates
// State/PID, not zero-filled. Pre-fix every persist erased them, and the
// persist-on-every-transition cadence made the loss guaranteed (the
// cross-restart 30-min sliding window read by HydrateFromState would be
// silently dropped on the first transition after cold start).
func TestDaemonRuntimeTracker_PersistPreservesFileOwnedFields(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	taskName := `\mcp-local-hub-memory-default`

	// Seed a row carrying all four file-owned fields with non-zero values,
	// as a quarantine/backoff path or a previous supervisor would have
	// written them.
	backoffUntil := "2026-06-09T10:05:00.000000000Z"
	quarantineSince := "2026-06-09T09:30:00.000000000Z"
	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			taskName: {
				State:         "backoff-waiting",
				CurrentPID:    0,
				PIDGeneration: 4,
				RestartHistory: []api.RestartEvent{
					{At: "2026-06-09T09:50:00.000000000Z", ExitCode: 1},
					{At: "2026-06-09T09:55:00.000000000Z", ExitCode: 2},
				},
				BackoffUntil:    backoffUntil,
				QuarantineSince: quarantineSince,
				QueuedAction:    &api.QueuedAction{Kind: "respawn", Reason: "manual-restart"},
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	// A persist driven by a state/PID transition (e.g. the daemon respawned).
	tracker := NewDaemonRuntimeTracker()
	startedAt := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	tracker.MarkSpawned(taskName, 5555, startedAt)
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("persist tracker: %v", err)
	}

	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted supervisor-state.json: %v", err)
	}
	row := persisted.Daemons[taskName]
	// State/PID/generation reflect the tracker's fresh values.
	if row.State != "running" || row.CurrentPID != 5555 || row.PIDGeneration != 1 || row.StartedAt != startedAt.Format(time.RFC3339Nano) {
		t.Fatalf("persisted row tracker fields = %+v, want running pid=5555 generation=1 started_at", row)
	}
	// The four file-owned fields are RETAINED, not zeroed.
	if len(row.RestartHistory) != 2 {
		t.Fatalf("RestartHistory zeroed by persist: %+v (Conc-F1 regression)", row.RestartHistory)
	}
	if row.RestartHistory[0].ExitCode != 1 || row.RestartHistory[1].ExitCode != 2 {
		t.Fatalf("RestartHistory corrupted by persist: %+v", row.RestartHistory)
	}
	if row.BackoffUntil != backoffUntil {
		t.Fatalf("BackoffUntil = %q, want preserved %q (Conc-F1 regression)", row.BackoffUntil, backoffUntil)
	}
	if row.QuarantineSince != quarantineSince {
		t.Fatalf("QuarantineSince = %q, want preserved %q (Conc-F1 regression)", row.QuarantineSince, quarantineSince)
	}
	if row.QueuedAction == nil || row.QueuedAction.Kind != "respawn" || row.QueuedAction.Reason != "manual-restart" {
		t.Fatalf("QueuedAction = %+v, want preserved {respawn manual-restart} (Conc-F1 regression)", row.QueuedAction)
	}
}

// TestDaemonRuntimeTracker_PersistDropsFieldsForRemovedDaemon is the
// adversarial complement to the Conc-F1 preserve fix: the preserve logic
// must NOT resurrect the four file-owned fields for a daemon the tracker
// no longer owns (e.g. removed via clearRemovedTaskRuntime). PersistTo
// rebuilds file.Daemons from the tracker snapshot, so a task absent from
// the tracker is dropped from the file entirely — including its four
// fields. This proves the preserve copy is scoped to tasks the tracker
// still tracks, not a blanket merge that would strand stale rows.
func TestDaemonRuntimeTracker_PersistDropsFieldsForRemovedDaemon(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	removed := `\mcp-local-hub-removed-default`
	kept := `\mcp-local-hub-memory-default`

	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			removed: {
				State:           "quarantined",
				PIDGeneration:   9,
				RestartHistory:  []api.RestartEvent{{At: "2026-06-09T09:00:00.000000000Z", ExitCode: 1}},
				QuarantineSince: "2026-06-09T09:00:00.000000000Z",
				QueuedAction:    &api.QueuedAction{Kind: "respawn", Reason: "x"},
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	// The tracker only knows about `kept` — `removed` was dropped (e.g. its
	// intent was removed and clearRemovedTaskRuntime called tracker.Remove).
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(kept, 4321, time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC))
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("persist tracker: %v", err)
	}

	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted supervisor-state.json: %v", err)
	}
	if _, ok := persisted.Daemons[removed]; ok {
		t.Fatalf("removed daemon resurrected by persist preserve logic: %+v", persisted.Daemons[removed])
	}
	if _, ok := persisted.Daemons[kept]; !ok {
		t.Fatalf("kept daemon missing after persist: %+v", persisted.Daemons)
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

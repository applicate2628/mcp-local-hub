package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
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
	if serena.State != "idle" || serena.CurrentPID != 0 {
		t.Fatalf("persisted serena state = %+v, want neutral idle pid=0", serena)
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
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 {
		t.Fatalf("hydrated serena entry = %+v, want idle pid=0", entry)
	}
}

// TestDaemonRuntimeTracker_PersistDoesNotEmitVestigialRestartFields pins
// the 2026-06-20 supervisor audit P3 decision (Option 2: DELETE the
// vestigial restart-policy fields, document in-memory-only). The
// restart-policy runtime state (30-min crash sliding window, backoff
// deadline, quarantine timestamp, queued post-exit action) is
// in-memory-only in DaemonRuntimeTracker and the SM's SMContext; it is
// NOT persisted and resets on cold restart by design. The persisted
// supervisor-state.json must therefore NEVER carry restart_history /
// backoff_until / quarantine_since / queued_action keys — the previous
// schema claimed they were persisted but no production path ever wrote a
// non-empty value (this superseded the Codex deep-sec PR #268 Conc-F1
// forward-copy, which only ever preserved empties).
//
// The test asserts on the RAW JSON bytes because the Go struct no longer
// has those fields at all — a struct-field assertion can't catch a
// regression that re-adds them, but a raw-key scan can.
func TestDaemonRuntimeTracker_PersistDoesNotEmitVestigialRestartFields(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	taskName := `\mcp-local-hub-memory-default`

	// Drive a crash + backoff + quarantine + spawn sequence so any path
	// that WOULD persist restart-policy state has fired before the persist.
	tracker := NewDaemonRuntimeTracker()
	startedAt := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	tracker.MarkSpawned(taskName, 5555, startedAt)
	tracker.RecordCrashAndCountInWindow(taskName, startedAt.Add(time.Minute), 30*time.Minute)
	tracker.MarkBackoff(taskName)
	tracker.MarkQuarantined(taskName)
	tracker.MarkSpawned(taskName, 6666, startedAt.Add(2*time.Minute))
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("persist tracker: %v", err)
	}

	// The tracker fields ARE persisted (state/pid/generation/started_at).
	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted supervisor-state.json: %v", err)
	}
	row := persisted.Daemons[taskName]
	if row.State != "running" || row.CurrentPID != 6666 || row.PIDGeneration != 2 {
		t.Fatalf("persisted row tracker fields = %+v, want running pid=6666 generation=2", row)
	}

	// The vestigial restart-policy keys MUST be absent from the on-disk JSON.
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read raw supervisor-state.json: %v", err)
	}
	for _, key := range []string{"restart_history", "backoff_until", "quarantine_since", "queued_action"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("persisted supervisor-state.json carries vestigial key %q (audit P3 regression):\n%s", key, raw)
		}
	}
}

func TestDaemonRuntimeTracker_PersistCollapsesRestartPolicyRuntimeStates(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	runningTask := `\mcp-local-hub-memory-default`
	backoffTask := `\mcp-local-hub-serena-default`
	quarantinedTask := `\mcp-local-hub-lsp-default`
	startedAt := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)

	hot := NewDaemonRuntimeTracker()
	hot.MarkSpawned(runningTask, 7777, startedAt)
	hot.MarkSpawned(backoffTask, 8888, startedAt.Add(time.Minute))
	hot.MarkBackoff(backoffTask)
	hot.MarkSpawned(quarantinedTask, 9999, startedAt.Add(2*time.Minute))
	hot.MarkQuarantined(quarantinedTask)
	if err := hot.PersistTo(statePath); err != nil {
		t.Fatalf("persist hot tracker: %v", err)
	}

	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted supervisor-state.json: %v", err)
	}
	if row := persisted.Daemons[runningTask]; row.State != "running" || row.CurrentPID != 7777 || row.PIDGeneration != 1 {
		t.Fatalf("running row = %+v, want running pid=7777 generation=1", row)
	}
	for _, taskName := range []string{backoffTask, quarantinedTask} {
		row := persisted.Daemons[taskName]
		if row.State != "idle" || row.CurrentPID != 0 || row.StartedAt != "" {
			t.Fatalf("restart-policy runtime row %s = %+v, want neutral idle pid=0 no started_at", taskName, row)
		}
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read raw supervisor-state.json: %v", err)
	}
	for _, transient := range []string{"backoff-waiting", "quarantined"} {
		if strings.Contains(string(raw), transient) {
			t.Fatalf("persisted supervisor-state.json carries transient state %q:\n%s", transient, raw)
		}
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	currentRunningVerifyPIDIdentityFn = func(proof process.PIDIdentityProof) error {
		if proof.PID != 7777 {
			t.Fatalf("verified PID = %d, want only running PID 7777", proof.PID)
		}
		return nil
	}
	currentRunningIsPIDAliveFn = func(pid int) bool {
		t.Fatalf("verified PID path should not need alive fallback, got pid %d", pid)
		return false
	}
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
	})

	cold, currentRunning, runningPIDs, err := loadSupervisorStartupRuntime(stateDir)
	if err != nil {
		t.Fatalf("loadSupervisorStartupRuntime: %v", err)
	}
	if !currentRunning[runningTask] || runningPIDs[runningTask].PID != 7777 {
		t.Fatalf("verified running daemon not seeded: currentRunning=%v runningPIDs=%v", currentRunning, runningPIDs)
	}
	for _, taskName := range []string{backoffTask, quarantinedTask} {
		if currentRunning[taskName] {
			t.Fatalf("non-running restart-policy row %s reached currentRunning: %v", taskName, currentRunning)
		}
		entry, ok := cold.Get(taskName)
		if !ok {
			t.Fatalf("cold tracker missing %s", taskName)
		}
		if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 {
			t.Fatalf("cold tracker %s = %+v, want idle pid=0", taskName, entry)
		}
	}
}

// TestDaemonRuntimeTracker_RestartPolicyStateResetsOnColdRestart pins the
// in-memory-only contract: a crash window + quarantine that exists in one
// supervisor's tracker does NOT survive a cold restart (a fresh tracker
// hydrated from the persisted file). This is the behavior the audit P3
// decision documents as intentional (pre-restart crashes are irrelevant
// to runtime respawn decisions; a cold restart is an operator reset).
func TestDaemonRuntimeTracker_RestartPolicyStateResetsOnColdRestart(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	taskName := `\mcp-local-hub-memory-default`

	now := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	hot := NewDaemonRuntimeTracker()
	hot.MarkSpawned(taskName, 5555, now)
	// Record several crashes so the sliding window is non-empty in-memory.
	for i := 0; i < 3; i++ {
		hot.RecordCrashAndCountInWindow(taskName, now.Add(time.Duration(i)*time.Minute), 30*time.Minute)
	}
	if got := hot.CrashCountInWindow(taskName, now.Add(5*time.Minute), 30*time.Minute); got != 3 {
		t.Fatalf("hot tracker crash window = %d, want 3", got)
	}
	if err := hot.PersistTo(statePath); err != nil {
		t.Fatalf("persist hot tracker: %v", err)
	}

	// Cold restart: a fresh tracker hydrated from the persisted state has
	// NO crash history (the window is in-memory-only and was not persisted).
	cold, err := loadDaemonRuntimeTrackerFromStatePath(statePath)
	if err != nil {
		t.Fatalf("load cold tracker: %v", err)
	}
	if got := cold.CrashCountInWindow(taskName, now.Add(5*time.Minute), 30*time.Minute); got != 0 {
		t.Fatalf("cold tracker crash window = %d, want 0 (reset on restart)", got)
	}
	entry, ok := cold.Get(taskName)
	if !ok {
		t.Fatal("cold tracker missing hydrated entry")
	}
	// RestartCount is in-memory-only and also resets to 0 on cold restart.
	if entry.RestartCount != 0 {
		t.Fatalf("cold tracker RestartCount = %d, want 0 (reset on restart)", entry.RestartCount)
	}
}

// TestDaemonRuntimeTracker_PersistDropsRowForRemovedDaemon proves
// PersistTo rebuilds file.Daemons wholesale from the tracker snapshot:
// a task absent from the tracker (e.g. removed via clearRemovedTaskRuntime
// → tracker.Remove) is dropped from the file entirely rather than stranded
// as a stale row. (The wholesale rebuild is also why no stale vestigial
// restart-policy fields can survive — there is no per-row merge anymore.)
func TestDaemonRuntimeTracker_PersistDropsRowForRemovedDaemon(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	removed := `\mcp-local-hub-removed-default`
	kept := `\mcp-local-hub-memory-default`

	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			removed: {
				State:         "quarantined",
				PIDGeneration: 9,
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

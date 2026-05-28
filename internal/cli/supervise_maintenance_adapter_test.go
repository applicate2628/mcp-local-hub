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

// TestMaintenanceStateAdapter_RemoveTransientClaimRemovesOnlyMatching
// covers PR #243 bot P2#4. Two maintenance kinds can each hold a PID=0
// claim simultaneously (the single-run guard is per-kind). Removing one
// kind's claim must NOT erase the other kind's still-valid claim, and
// must NOT touch real-PID entries. The old behaviour
// (RemoveTransientPID(0) removing ALL PID=0 entries) is the bug; the
// scheduler now removes claims by (PID==0, Kind, StartedAt) identity.
func TestMaintenanceStateAdapter_RemoveTransientClaimRemovesOnlyMatching(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	adapter := newMaintenanceStateAdapter(statePath, nil)

	const startedA = "2026-05-17T04:00:00Z"
	const startedB = "2026-05-17T04:00:01Z"
	for _, p := range []api.TransientPID{
		{PID: 0, Kind: "workspace-weekly-refresh", StartedAt: startedA},
		{PID: 0, Kind: "server-weekly-refresh", StartedAt: startedB},
		{PID: 9999, Kind: "server-weekly-refresh", StartedAt: "2026-05-17T04:00:02Z"},
	} {
		if err := adapter.AddTransientPID(p); err != nil {
			t.Fatalf("AddTransientPID(%+v): %v", p, err)
		}
	}

	// Remove only kind A's claim.
	adapter.RemoveTransientClaim("workspace-weekly-refresh", startedA)

	got, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if len(got.TransientPIDs) != 2 {
		t.Fatalf("RemoveTransientClaim must remove ONLY the matching claim; got %+v", got.TransientPIDs)
	}
	var sawClaimB, sawReal bool
	for _, p := range got.TransientPIDs {
		if p.PID == 0 && p.Kind == "server-weekly-refresh" && p.StartedAt == startedB {
			sawClaimB = true
		}
		if p.PID == 9999 {
			sawReal = true
		}
	}
	if !sawClaimB || !sawReal {
		t.Fatalf("kind B's claim AND the real PID must survive; got %+v", got.TransientPIDs)
	}

	// A non-matching identity is a no-op.
	adapter.RemoveTransientClaim("server-weekly-refresh", "no-such-startedat")
	if got, _ := api.ReadSupervisorState(statePath); len(got.TransientPIDs) != 2 {
		t.Fatalf("non-matching RemoveTransientClaim must be a no-op; got %+v", got.TransientPIDs)
	}

	// Real-PID drain still removes the real entry.
	adapter.RemoveTransientPID(9999)
	got, err = api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if len(got.TransientPIDs) != 1 || got.TransientPIDs[0].PID != 0 {
		t.Fatalf("RemoveTransientPID(real) must leave only kind B's claim; got %+v", got.TransientPIDs)
	}
}

// TestMaintenanceStateAdapter_ReconcileTransientPIDsRemovesDeadAndClaims
// covers PR #243 bot P2#2 + the dual-entry crash-window the consultant
// flagged. After the spawner drains/kills in-flight children at
// shutdown, the supervisor reconciles on-disk transient_pids by
// removing every entry whose PID is not alive. A PID=0 claim and a real
// PID for the SAME kind can legitimately coexist in a crash window;
// this proves the reconcile (with the production isPIDAlive) drops the
// claim and any dead PID, retains the live one, and never treats PID 0
// as a kill target.
func TestMaintenanceStateAdapter_ReconcileTransientPIDsRemovesDeadAndClaims(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	adapter := newMaintenanceStateAdapter(statePath, nil)

	live := os.Getpid() // the test process is always alive
	for _, p := range []api.TransientPID{
		{PID: 0, Kind: "workspace-weekly-refresh", StartedAt: "2026-05-17T04:00:00Z"},    // claim
		{PID: live, Kind: "workspace-weekly-refresh", StartedAt: "2026-05-17T04:00:01Z"}, // real, alive
	} {
		if err := adapter.AddTransientPID(p); err != nil {
			t.Fatalf("AddTransientPID(%+v): %v", p, err)
		}
	}

	// isPIDAlive is the production probe; isPIDAlive(0) is false (guard),
	// isPIDAlive(live) is true.
	removed := adapter.ReconcileTransientPIDs(isPIDAlive)

	if len(removed) != 1 || removed[0] != 0 {
		t.Fatalf("reconcile must remove the PID=0 claim only (live PID retained); removed=%v", removed)
	}
	got, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if len(got.TransientPIDs) != 1 || got.TransientPIDs[0].PID != live {
		t.Fatalf("only the live real PID must remain; got %+v", got.TransientPIDs)
	}
}

// TestMaintenanceSpawner_StartAbortsWhenGracefulInProgress covers PR
// #243 bot P1#2. When a supervisor graceful-exit is already in
// progress, Start must refuse to launch the child (so Shutdown cannot
// snapshot zero processes and exit while a fresh child is launching).
func TestMaintenanceSpawner_StartAbortsWhenGracefulInProgress(t *testing.T) {
	var graceful gracefulCounter
	graceful.Enter() // simulate an in-progress graceful exit
	sp := newMaintenanceSpawner(nil, &graceful)

	pid, err := sp.Start(api.MaintenanceTimer{
		Name:    `\mcp-local-hub-maintenance-graceful-abort-test`,
		Kind:    "workspace-weekly-refresh",
		Command: shellPathForExitCodeTest(),
		Args:    shellArgsForExitCodeTest(0),
	})
	if err == nil {
		t.Fatal("Start must abort when graceful-exit is in progress")
	}
	if pid != 0 {
		t.Fatalf("aborted Start must return pid 0; got %d", pid)
	}
	if len(sp.snapshotProcesses()) != 0 {
		t.Fatalf("aborted Start must not register a process; got %+v", sp.snapshotProcesses())
	}
}

func TestMaintenanceSpawner_WaitTreatsExitCodeAsCleanProcessExit(t *testing.T) {
	sp := newMaintenanceSpawner(nil, nil)
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

	sp := newMaintenanceSpawner(events, nil)
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

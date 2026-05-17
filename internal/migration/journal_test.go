package migration

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Test fixtures.
// ---------------------------------------------------------------------------

// testFixture bundles a temp state-dir, an install-dir, a fake scheduler,
// and a fake supervisor IPC into one struct so tests can inject the same
// surfaces into both forward and rollback flows.
type testFixture struct {
	State     State
	Scheduler *fakeScheduler
	IPC       *fakeIPCClient

	// Step-9 helpers — the test populates these to control which PIDs
	// resolve, which 4-gate signals look like, etc.
	pidByServerDaemon map[string]int
	portByPID         map[int]int
	identityByPID     map[int]ProcessIdentity
	killedPIDs        []int

	// PortBindWait fake — sequence of (port, returnErr) decisions in
	// arrival order. When the slice is exhausted, returns nil.
	portWaitReturns []error
	portWaitMu      sync.Mutex
	portWaitIdx     int

	// PortBindWaitBound fake (rollback step 10 — wait-until-bound).
	// Distinct from portWaitReturns above (step 3 — wait-until-unbound)
	// because the two have opposite semantics and tests need to control
	// each independently. When the slice is exhausted, returns nil.
	portWaitBoundReturns []error
	portWaitBoundMu      sync.Mutex
	portWaitBoundIdx     int

	// Telemetry counters.
	shimInstalledStrict *bool
	supervisorSpawned   int
	reconcileWaited     int
	shimUninstalled     int
	quarantineCalled    int
	forceKillCalled     int

	// Allow tests to override the canonical CurrentUser.
	CurrentUser string
}

// fakeScheduler implements SchedulerBackend with full recording.
type fakeScheduler struct {
	mu sync.Mutex

	// Programmed return values.
	EnumerateReturns []scheduler.TaskStatus
	EnumerateErr     error
	ExportByTask     map[string]string
	ExportErr        error

	// Recorded calls.
	DeletedTasks []string
	CreatedTasks []string // task names passed to CreateXML
	RunTasks     []string

	// CreateXML payloads (taskName → raw xml) so tests can verify the
	// rollback path re-registered the exact XML the journal stored.
	CreateXMLPayloads map[string]string
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{
		ExportByTask:      make(map[string]string),
		CreateXMLPayloads: make(map[string]string),
	}
}

func (f *fakeScheduler) EnumerateAllMcphubTasks() ([]scheduler.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.EnumerateReturns, f.EnumerateErr
}

func (f *fakeScheduler) ExportXML(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ExportErr != nil {
		return "", f.ExportErr
	}
	if raw, ok := f.ExportByTask[name]; ok {
		return raw, nil
	}
	return "", scheduler.ErrTaskNotFound
}

func (f *fakeScheduler) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeletedTasks = append(f.DeletedTasks, name)
	return nil
}

func (f *fakeScheduler) CreateXML(name, xml string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreatedTasks = append(f.CreatedTasks, name)
	f.CreateXMLPayloads[name] = xml
	return nil
}

func (f *fakeScheduler) Run(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.RunTasks = append(f.RunTasks, name)
	return nil
}

// fakeIPCClient records every command + args passed.
type fakeIPCClient struct {
	mu sync.Mutex

	Commands []struct {
		Cmd     string
		Args    map[string]any
		Timeout time.Duration
	}
	// Per-command return-value override (cmd → error).
	Returns map[string]error
}

func newFakeIPCClient() *fakeIPCClient {
	return &fakeIPCClient{Returns: make(map[string]error)}
}

func (f *fakeIPCClient) Send(cmd string, args map[string]any, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Commands = append(f.Commands, struct {
		Cmd     string
		Args    map[string]any
		Timeout time.Duration
	}{cmd, args, timeout})
	return f.Returns[cmd]
}

// ---------------------------------------------------------------------------
// Fixture builders.
// ---------------------------------------------------------------------------

// setupV04xFixture seeds a fresh state-dir + scheduler with TWO fake
// v0.4.x tasks (memory-default, time-default), produces clean XML for
// each (matching the pinned defaults so the classifier reports no
// abort), and wires injected callbacks for the OS-mutating step path.
func setupV04xFixture(t *testing.T) *testFixture {
	t.Helper()
	// v0.5.0 Fix Group 5: migration.lock owner sidecar +
	// supervisor-intent.json + supervisor-state.json writes flow
	// through the hardened secure-write pipeline. The state-dir
	// must pass the parent-dir gate; apitest.HardenedTempDir
	// installs the allowlist-conforming DACL/mode. installDir does
	// not currently host secure-write targets, so plain t.TempDir
	// remains acceptable there.
	stateDir := apitest.HardenedTempDir(t)
	installDir := t.TempDir()

	tx := &testFixture{
		State: State{
			StateDir:   stateDir,
			InstallDir: installDir,
			Now: func() time.Time {
				// Fixed clock — every call returns the same instant
				// so journal dir names are deterministic.
				return time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
			},
		},
		Scheduler:         newFakeScheduler(),
		IPC:               newFakeIPCClient(),
		CurrentUser:       "TestUser",
		pidByServerDaemon: make(map[string]int),
		portByPID:         make(map[int]int),
		identityByPID:     make(map[int]ProcessIdentity),
	}

	// Synthesize two clean tasks.
	tx.Scheduler.EnumerateReturns = []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-memory-default", Owner: tx.CurrentUser, State: "Ready"},
		{Name: "\\mcp-local-hub-time-default", Owner: tx.CurrentUser, State: "Ready"},
	}
	tx.Scheduler.ExportByTask["\\mcp-local-hub-memory-default"] = cleanV04xXML(t, "memory", "default", tx.CurrentUser, installDir)
	tx.Scheduler.ExportByTask["\\mcp-local-hub-time-default"] = cleanV04xXML(t, "time", "default", tx.CurrentUser, installDir)

	// No running daemons by default — the kill loop reports
	// "no-running-daemon" for each. Tests can override
	// pidByServerDaemon to exercise the kill path.

	return tx
}

// cleanV04xXML returns a clean v0.4.x XML body matching the pinned
// defaults so the classifier reports zero deviations. Spec line 240-248.
func cleanV04xXML(t *testing.T, server, daemon, currentUser, installDir string) string {
	t.Helper()
	cmd := filepath.Join(installDir, "mcphub.exe")
	// Use the pinned renderer for exact-match.
	return V04xTemplateXML(scheduler.TaskSpec{
		Name:         "\\mcp-local-hub-" + server + "-" + daemon,
		Description:  server + " daemon for " + daemon,
		Command:      cmd,
		Args:         []string{"daemon", "--server", server, "--daemon", daemon},
		WorkingDir:   installDir,
		LogonTrigger: true,
	}, currentUser)
}

func TestMigrationCarriesWorkingDir(t *testing.T) {
	tx := setupV04xFixture(t)
	journalDir := filepath.Join(tx.State.StateDir, "migration-journal-working-dir")
	tasks := []scheduler.TaskStatus{
		{Name: "\\mcp-local-hub-memory-default", Owner: tx.CurrentUser, State: "Ready"},
	}
	xmlByTask := map[string]string{
		"\\mcp-local-hub-memory-default": cleanV04xXML(t, "memory", "default", tx.CurrentUser, tx.State.InstallDir),
	}

	intent, err := deriveOrLoadIntent(journalDir, tasks, xmlByTask, false, tx.State.Now())
	if err != nil {
		t.Fatalf("deriveOrLoadIntent: %v", err)
	}
	if len(intent.Daemons) != 1 {
		t.Fatalf("Daemons len = %d, want 1", len(intent.Daemons))
	}
	if got := intent.Daemons[0].Workspace; got != tx.State.InstallDir {
		t.Fatalf("Workspace = %q, want migrated WorkingDirectory %q", got, tx.State.InstallDir)
	}
}

func TestMigrationJournalMarkerSurvivesProcessCrash(t *testing.T) {
	journalDir := filepath.Join(t.TempDir(), "journal")
	for _, marker := range []string{
		MarkerPrepared,
		MarkerPreOsMutating,
		MarkerOsMutatingComplete,
		MarkerCommitted,
		MarkerRollbackInProgress,
	} {
		if err := touchMarker(journalDir, marker); err != nil {
			t.Fatalf("touchMarker(%s): %v", marker, err)
		}
		path := filepath.Join(journalDir, marker)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("marker %s missing after durable touch: %v", marker, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("marker %s mode = %v, want regular file", marker, info.Mode())
		}
	}
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatalf("ReadDir journal: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") && strings.Contains(entry.Name(), ".tmp.") {
			t.Fatalf("temporary marker file survived durable rename: %s", entry.Name())
		}
	}
}

// fakeForwardOptions returns a ForwardOptions wired to the fixture's
// fakes. The PowerShellProbe returns (true, nil) — clean Win11 host.
// The kill loop is a no-op because tx.pidByServerDaemon is empty.
func fakeForwardOptions(t *testing.T, tx *testFixture) ForwardOptions {
	t.Helper()
	return ForwardOptions{
		Scheduler:       tx.Scheduler,
		CurrentUser:     tx.CurrentUser,
		PowerShellProbe: func() (bool, error) { return true, nil },
		WmicPresent:     func() bool { return true },
		LookupProcessIdentity: func(pid int) (ProcessIdentity, error) {
			if id, ok := tx.identityByPID[pid]; ok {
				return id, nil
			}
			return ProcessIdentity{}, errors.New("not found")
		},
		PortForPID: func(pid int) (int, bool) {
			if p, ok := tx.portByPID[pid]; ok {
				return p, true
			}
			return 0, false
		},
		PIDForServerDaemon: func(server, daemon string) (int, error) {
			if p, ok := tx.pidByServerDaemon[server+"/"+daemon]; ok {
				return p, nil
			}
			return 0, ErrProcessNotFound
		},
		KillPID: func(pid int) error {
			tx.killedPIDs = append(tx.killedPIDs, pid)
			return nil
		},
		PortBindWait: func(port int, timeout time.Duration) error {
			tx.portWaitMu.Lock()
			defer tx.portWaitMu.Unlock()
			if tx.portWaitIdx < len(tx.portWaitReturns) {
				err := tx.portWaitReturns[tx.portWaitIdx]
				tx.portWaitIdx++
				return err
			}
			return nil
		},
		ShimInstaller: func(strict bool) error {
			tx.shimInstalledStrict = &strict
			return nil
		},
		SupervisorSpawner: func() error {
			tx.supervisorSpawned++
			return nil
		},
		ReconcileReady: func(timeout time.Duration) error {
			tx.reconcileWaited++
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// Forward-migration tests.
// ---------------------------------------------------------------------------

// TestForwardMigration_FullFlow exercises the happy path: clean v0.4.x
// fixture → all four forward markers present → supervisor-intent.json
// landed in state-dir → no daemons derived because we don't seed
// running PIDs (the daemons are derived from the task argv, not from
// running processes, so they DO populate — verify).
func TestForwardMigration_FullFlow(t *testing.T) {
	tx := setupV04xFixture(t)
	opts := fakeForwardOptions(t, tx)
	if err := RunForward(tx.State, opts); err != nil {
		t.Fatalf("forward: %v", err)
	}
	journalDir := mustFindJournalDir(t, tx.State.StateDir)
	for _, m := range []string{MarkerPrepared, MarkerPreOsMutating, MarkerOsMutatingComplete, MarkerCommitted} {
		// Note: pre-os-mutating is conditional on at least one successful
		// kill. In the clean fixture (no daemons running) it is NOT touched.
		// Skip pre-os-mutating in the FULL-FLOW happy path.
		if m == MarkerPreOsMutating {
			continue
		}
		if _, err := os.Stat(filepath.Join(journalDir, m)); err != nil {
			t.Fatalf("marker %s missing: %v", m, err)
		}
	}
	intent, err := api.ReadSupervisorIntent(filepath.Join(tx.State.StateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("supervisor-intent.json missing: %v", err)
	}
	if len(intent.Daemons) == 0 {
		t.Fatal("no daemons derived from v0.4.x fixture")
	}
	// Verify per-daemon argv was parsed.
	foundMemory := false
	for _, d := range intent.Daemons {
		if d.Server == "memory" && d.Daemon == "default" {
			foundMemory = true
		}
	}
	if !foundMemory {
		t.Fatal("memory-default daemon missing from derived intent")
	}
	// Step 14 callback fired.
	if tx.reconcileWaited != 1 {
		t.Fatalf("reconcileReady not called: %d", tx.reconcileWaited)
	}
	// Tasks deleted at step 10.
	if len(tx.Scheduler.DeletedTasks) != 2 {
		t.Fatalf("expected 2 deletes, got %d", len(tx.Scheduler.DeletedTasks))
	}
}

// TestForwardMigration_AbortOnHardDeviation seeds a task with
// LogonType=Password (KindUnsupportedAbort). Without the discard flag,
// migration aborts; markers past `prepared` do NOT exist (the abort
// fires at step 6, before step 8 touch prepared, so even `prepared` is
// absent).
func TestForwardMigration_AbortOnHardDeviation(t *testing.T) {
	tx := setupV04xFixture(t)
	// Mutate one task's XML to trip the abort path.
	bad := strings.Replace(
		tx.Scheduler.ExportByTask["\\mcp-local-hub-memory-default"],
		"<LogonType>InteractiveToken</LogonType>",
		"<LogonType>Password</LogonType>",
		1,
	)
	tx.Scheduler.ExportByTask["\\mcp-local-hub-memory-default"] = bad

	opts := fakeForwardOptions(t, tx)
	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected hard-deviation abort, got nil")
	}
	if !errors.Is(err, ErrMigrationHardDeviation) {
		t.Fatalf("expected ErrMigrationHardDeviation, got: %v", err)
	}
	// Even the prepared marker should NOT have been written.
	journalDir := mustFindJournalDir(t, tx.State.StateDir)
	if _, statErr := os.Stat(filepath.Join(journalDir, MarkerPrepared)); statErr == nil {
		t.Fatal("prepared marker should not exist after hard-deviation abort")
	}
}

// TestForwardMigration_DiscardSchedulerCustomizationsBypassesAbort
// exercises the override flag: same hard-deviation XML, but the
// migration proceeds (committed marker present).
func TestForwardMigration_DiscardSchedulerCustomizationsBypassesAbort(t *testing.T) {
	tx := setupV04xFixture(t)
	bad := strings.Replace(
		tx.Scheduler.ExportByTask["\\mcp-local-hub-memory-default"],
		"<LogonType>InteractiveToken</LogonType>",
		"<LogonType>Password</LogonType>",
		1,
	)
	tx.Scheduler.ExportByTask["\\mcp-local-hub-memory-default"] = bad

	opts := fakeForwardOptions(t, tx)
	opts.DiscardSchedulerCustomizations = true
	if err := RunForward(tx.State, opts); err != nil {
		t.Fatalf("expected proceed with discard flag, got: %v", err)
	}
	journalDir := mustFindJournalDir(t, tx.State.StateDir)
	if _, err := os.Stat(filepath.Join(journalDir, MarkerCommitted)); err != nil {
		t.Fatalf("committed marker missing: %v", err)
	}
}

// TestForwardMigration_PowerShellLockedAborts exercises step 0: PS CLM
// rejection AND wmic absent → exit code 14.
func TestForwardMigration_PowerShellLockedAborts(t *testing.T) {
	tx := setupV04xFixture(t)
	opts := fakeForwardOptions(t, tx)
	opts.PowerShellProbe = func() (bool, error) { return false, nil }
	opts.WmicPresent = func() bool { return false }
	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected PowerShell-locked abort, got nil")
	}
	var ec *ExitCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("expected *ExitCodeError, got: %T %v", err, err)
	}
	if ec.Code != ExitMigrationPowerShellLocked {
		t.Fatalf("expected exit %d, got %d", ExitMigrationPowerShellLocked, ec.Code)
	}
	if !errors.Is(err, ErrPowerShellLocked) {
		t.Fatalf("expected ErrPowerShellLocked wrap: %v", err)
	}
}

// TestForwardMigration_PowerShellLockedButWmicPresent: PS rejected but
// wmic is on PATH → migration proceeds (step 0 passes via wmic
// fallback).
func TestForwardMigration_PowerShellLockedButWmicPresent(t *testing.T) {
	tx := setupV04xFixture(t)
	opts := fakeForwardOptions(t, tx)
	opts.PowerShellProbe = func() (bool, error) { return false, nil }
	opts.WmicPresent = func() bool { return true }
	if err := RunForward(tx.State, opts); err != nil {
		t.Fatalf("expected proceed with wmic fallback, got: %v", err)
	}
}

// TestForwardMigration_LockBusyReturns8 pre-acquires migration.lock in
// the test then verifies RunForward returns ExitInstallBusy.
func TestForwardMigration_LockBusyReturns8(t *testing.T) {
	tx := setupV04xFixture(t)
	// Pre-acquire migration.lock to simulate a concurrent installer.
	preAcq, err := api.AcquireSupervisorLock(filepath.Join(tx.State.StateDir, "migration"))
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer preAcq.Release()

	opts := fakeForwardOptions(t, tx)
	err = RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected lock-busy error, got nil")
	}
	var ec *ExitCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("expected *ExitCodeError, got: %T %v", err, err)
	}
	if ec.Code != ExitInstallBusy {
		t.Fatalf("expected exit %d, got %d", ExitInstallBusy, ec.Code)
	}
}

// TestForwardMigration_KillsRunningDaemon seeds a running daemon for
// memory-default and verifies the 4-gate ownership check passes, the
// PID gets killed, pre-os-mutating is touched, and killed-daemons.json
// records the event.
func TestForwardMigration_KillsRunningDaemon(t *testing.T) {
	tx := setupV04xFixture(t)
	const memPID = 4321
	const memPort = 9128
	tx.pidByServerDaemon["memory/default"] = memPID
	tx.portByPID[memPID] = memPort
	tx.identityByPID[memPID] = ProcessIdentity{
		PID:              memPID,
		Basename:         "mcphub.exe",
		CommandLine:      `"C:\App\mcphub.exe" daemon --server memory --daemon default`,
		ExecutablePath:   filepath.Join(tx.State.InstallDir, "mcphub.exe"),
		CreationDateUnix: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Unix(), // before lock-acquire
	}

	opts := fakeForwardOptions(t, tx)
	if err := RunForward(tx.State, opts); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if len(tx.killedPIDs) != 1 || tx.killedPIDs[0] != memPID {
		t.Fatalf("expected to kill PID %d, got: %v", memPID, tx.killedPIDs)
	}
	journalDir := mustFindJournalDir(t, tx.State.StateDir)
	if _, err := os.Stat(filepath.Join(journalDir, MarkerPreOsMutating)); err != nil {
		t.Fatalf("pre-os-mutating marker should exist after successful kill: %v", err)
	}
	// killed-daemons.json present and records the kill.
	raw, err := os.ReadFile(filepath.Join(journalDir, "killed-daemons.json"))
	if err != nil {
		t.Fatalf("killed-daemons.json missing: %v", err)
	}
	var kd killedDaemonsFile
	if err := json.Unmarshal(raw, &kd); err != nil {
		t.Fatalf("killed-daemons.json parse: %v", err)
	}
	found := false
	for _, k := range kd.Killed {
		if k.PID == memPID && k.OwnershipOK {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected memory PID in killed-daemons.json: %+v", kd.Killed)
	}
}

// TestForwardMigration_4GateOwnershipMismatchAborts seeds a running
// daemon BUT a wrong ExecutablePath (gate 4 fails). Per Lane F P0 #1,
// gate-4 failure aborts with MIGRATION_PORT_LOOKUP_INCONSISTENT rather
// than log+skip; otherwise the legacy task gets deleted while a
// foreign process keeps the port → supervisor restart collision.
// The audit row is still appended with the gate_failed reason so
// operators can diagnose what went wrong.
func TestForwardMigration_4GateOwnershipMismatchAborts(t *testing.T) {
	tx := setupV04xFixture(t)
	const otherPID = 9999
	tx.pidByServerDaemon["memory/default"] = otherPID
	tx.portByPID[otherPID] = 9128
	tx.identityByPID[otherPID] = ProcessIdentity{
		PID:              otherPID,
		Basename:         "mcphub.exe",
		CommandLine:      `mcphub.exe daemon --server memory --daemon default`,
		ExecutablePath:   `C:\OtherUser\Different\mcphub.exe`, // NOT under InstallDir
		CreationDateUnix: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Unix(),
	}

	opts := fakeForwardOptions(t, tx)
	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected gate-4 abort, got nil")
	}
	if !errors.Is(err, ErrMigrationPortLookupInconsistent) {
		t.Fatalf("expected ErrMigrationPortLookupInconsistent, got: %v", err)
	}
	if len(tx.killedPIDs) != 0 {
		t.Fatalf("gate-4 fail must abort BEFORE kill, got: %v", tx.killedPIDs)
	}
	journalDir := mustFindJournalDir(t, tx.State.StateDir)
	if _, err := os.Stat(filepath.Join(journalDir, MarkerPreOsMutating)); err == nil {
		t.Fatal("pre-os-mutating should NOT exist when no kill succeeded")
	}
	// killed-daemons.json must record the gate_failed reason.
	raw, _ := os.ReadFile(filepath.Join(journalDir, "killed-daemons.json"))
	var kd killedDaemonsFile
	_ = json.Unmarshal(raw, &kd)
	gateFailed := ""
	for _, k := range kd.Killed {
		if k.PID == otherPID {
			gateFailed = k.GateFailed
		}
	}
	if !strings.Contains(gateFailed, "ExecutablePath") {
		t.Fatalf("expected ExecutablePath in gate_failed reason, got: %q", gateFailed)
	}
}

// TestForwardMigration_PortLookupInconsistentAborts: PID resolves but
// PortForPID returns ok=false → ErrMigrationPortLookupInconsistent.
func TestForwardMigration_PortLookupInconsistentAborts(t *testing.T) {
	tx := setupV04xFixture(t)
	const memPID = 4321
	tx.pidByServerDaemon["memory/default"] = memPID
	// Intentionally NOT setting portByPID — PortForPID returns ok=false.
	tx.identityByPID[memPID] = ProcessIdentity{
		PID:              memPID,
		Basename:         "mcphub.exe",
		CommandLine:      "mcphub.exe daemon --server memory --daemon default",
		ExecutablePath:   filepath.Join(tx.State.InstallDir, "mcphub.exe"),
		CreationDateUnix: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Unix(),
	}

	opts := fakeForwardOptions(t, tx)
	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected port-lookup-inconsistent abort, got nil")
	}
	if !errors.Is(err, ErrMigrationPortLookupInconsistent) {
		t.Fatalf("expected ErrMigrationPortLookupInconsistent, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Journal-helper tests.
// ---------------------------------------------------------------------------

// TestPruneOldJournals_RetainsFiveNewest creates 7 journal dirs then
// verifies prune leaves the 5 newest.
func TestPruneOldJournals_RetainsFiveNewest(t *testing.T) {
	dir := t.TempDir()
	// Names sorted lexicographically → chronologically.
	names := []string{
		"migration-journal-20260501T000000Z-000000001",
		"migration-journal-20260502T000000Z-000000002",
		"migration-journal-20260503T000000Z-000000003",
		"migration-journal-20260504T000000Z-000000004",
		"migration-journal-20260505T000000Z-000000005",
		"migration-journal-20260506T000000Z-000000006",
		"migration-journal-20260507T000000Z-000000007",
	}
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(dir, n), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := PruneOldJournals(dir); err != nil {
		t.Fatalf("prune: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	var kept []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "migration-journal-") {
			kept = append(kept, e.Name())
		}
	}
	if len(kept) != 5 {
		t.Fatalf("expected 5 retained, got %d (%v)", len(kept), kept)
	}
	// The 5 newest by lex sort.
	for i, want := range names[2:] {
		if kept[i] != want {
			t.Fatalf("kept[%d] = %s, want %s", i, kept[i], want)
		}
	}
}

// TestPruneOldJournals_SweepsPruningDebris seeds a pre-existing
// .pruning-foo/ from a prior crash and verifies sweep cleans it before
// counting journals.
func TestPruneOldJournals_SweepsPruningDebris(t *testing.T) {
	dir := t.TempDir()
	// Pre-existing crash debris.
	if err := os.MkdirAll(filepath.Join(dir, ".pruning-migration-journal-old"), 0700); err != nil {
		t.Fatal(err)
	}
	// One real journal so PruneOldJournals has something to read.
	if err := os.MkdirAll(filepath.Join(dir, "migration-journal-20260501T000000Z-000000001"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := PruneOldJournals(dir); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".pruning-migration-journal-old")); !os.IsNotExist(err) {
		t.Fatal(".pruning-* debris should have been swept")
	}
	if _, err := os.Stat(filepath.Join(dir, "migration-journal-20260501T000000Z-000000001")); err != nil {
		t.Fatalf("real journal should survive: %v", err)
	}
}

// TestFindLatestJournal_PicksNewest verifies the lex/chronological sort.
func TestFindLatestJournal_PicksNewest(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"migration-journal-20260501T000000Z-000000001",
		"migration-journal-20260507T000000Z-000000007",
		"migration-journal-20260503T000000Z-000000003",
	}
	for _, n := range names {
		_ = os.MkdirAll(filepath.Join(dir, n), 0700)
	}
	got, err := FindLatestJournal(dir)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !strings.HasSuffix(got, "20260507T000000Z-000000007") {
		t.Fatalf("expected latest = 20260507..., got %s", got)
	}
}

// TestFindLatestJournal_EmptyDir returns empty string with no error.
func TestFindLatestJournal_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	got, err := FindLatestJournal(dir)
	if err != nil {
		t.Fatalf("find empty: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty result, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// Group 3 fixes — Lane C + Lane F P0/P1 regressions.
// ---------------------------------------------------------------------------

// TestForwardMigration_PreOsMutatingTouchedOnFirstKill verifies the
// per-task marker timing fix (Lane C P0 #1). Two tasks running; the
// FIRST kills successfully, the SECOND fails its port-bind wait. The
// pre-os-mutating marker MUST be present on disk because the first
// kill already mutated host state — a crash mid-second-task must
// resume into operator-choice-forward-or-rollback, not safe-abort.
func TestForwardMigration_PreOsMutatingTouchedOnFirstKill(t *testing.T) {
	tx := setupV04xFixture(t)
	const memPID, memPort = 4321, 9128
	const timePID, timePort = 4322, 9129
	tx.pidByServerDaemon["memory/default"] = memPID
	tx.portByPID[memPID] = memPort
	tx.identityByPID[memPID] = ProcessIdentity{
		PID:              memPID,
		Basename:         "mcphub.exe",
		CommandLine:      `mcphub.exe daemon --server memory --daemon default`,
		ExecutablePath:   filepath.Join(tx.State.InstallDir, "mcphub.exe"),
		CreationDateUnix: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Unix(),
	}
	tx.pidByServerDaemon["time/default"] = timePID
	tx.portByPID[timePID] = timePort
	tx.identityByPID[timePID] = ProcessIdentity{
		PID:              timePID,
		Basename:         "mcphub.exe",
		CommandLine:      `mcphub.exe daemon --server time --daemon default`,
		ExecutablePath:   filepath.Join(tx.State.InstallDir, "mcphub.exe"),
		CreationDateUnix: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Unix(),
	}

	opts := fakeForwardOptions(t, tx)
	// First task's port-bind wait succeeds (nil); second fails. The
	// marker is touched RIGHT AFTER the first KillPID returns nil and
	// BEFORE any port-bind wait, so the second-task failure must NOT
	// unwind it.
	tx.portWaitReturns = []error{nil, errors.New("port 9129 still bound")}
	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected port-release-wait failure on second task, got nil")
	}
	journalDir := mustFindJournalDir(t, tx.State.StateDir)
	if _, statErr := os.Stat(filepath.Join(journalDir, MarkerPreOsMutating)); statErr != nil {
		t.Fatalf("pre-os-mutating marker MUST exist after first successful kill: %v", statErr)
	}
}

// TestForwardMigration_ReconcileReadyTimeoutAutoRollbacks verifies
// Lane C P0 #2: a reconcile-ready timeout triggers RunRollback when
// ForwardOptions.RollbackOnFailure is wired. The fake reconcile
// returns a timeout error; the rollback callback must fire exactly
// once, and the committed marker must NOT be present.
func TestForwardMigration_ReconcileReadyTimeoutAutoRollbacks(t *testing.T) {
	tx := setupV04xFixture(t)
	opts := fakeForwardOptions(t, tx)
	opts.ReconcileReady = func(timeout time.Duration) error {
		tx.reconcileWaited++
		return errors.New("supervisor never reported ready within 30s")
	}
	rollbackInvocations := 0
	opts.RollbackOnFailure = func() *RollbackOptions {
		rollbackInvocations++
		return &RollbackOptions{
			Scheduler: tx.Scheduler,
			SupervisorIPC: func(cmd string, args map[string]any, timeout time.Duration) error {
				return tx.IPC.Send(cmd, args, timeout)
			},
			ProbeSupervisorTokenMismatch: func() error { return nil },
			ForceKillSupervisor: func() error {
				tx.forceKillCalled++
				return nil
			},
			PortBindWait: func(port int, timeout time.Duration) error { return nil },
			LookupProcessIdentity: func(pid int) (ProcessIdentity, error) {
				if id, ok := tx.identityByPID[pid]; ok {
					return id, nil
				}
				return ProcessIdentity{}, errors.New("not found")
			},
			QuarantineTranslator: func(_ State) error {
				tx.quarantineCalled++
				return nil
			},
			ShimUninstaller: func() error {
				tx.shimUninstalled++
				return nil
			},
			TimeWaitSettle:  10 * time.Millisecond,
			PortBindTimeout: 50 * time.Millisecond,
		}
	}

	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected error after auto-rollback completes, got nil")
	}
	if rollbackInvocations != 1 {
		t.Fatalf("RollbackOnFailure: want 1 invocation, got %d", rollbackInvocations)
	}
	journalDir := mustFindJournalDir(t, tx.State.StateDir)
	if _, statErr := os.Stat(filepath.Join(journalDir, MarkerCommitted)); statErr == nil {
		t.Fatal("committed marker must NOT be present after auto-rollback")
	}
	if tx.shimUninstalled != 1 {
		t.Fatalf("shimUninstaller should have fired during auto-rollback, got %d", tx.shimUninstalled)
	}
}

// TestRunForward_AutoRollback_PinsFailedJournalDir pins codex round-4
// Lane C P1: when RunForward triggers an auto-rollback after step 14
// reconcile-ready timeout, the RollbackOptions handed to RunRollback
// MUST carry the failed-forward journal dir in `.JournalDir`. Without
// this, RunRollback would call FindLatestJournal after re-acquiring
// migration.lock — and a NEWER journal created between the lock
// release and re-acquire would be rolled back instead of the failed
// forward.
//
// Test surface: a reconcile-ready timeout fires; the callback wraps
// the supplied RollbackOptions so the test can intercept the
// JournalDir field that RunForward populated; the test asserts the
// JournalDir matches the actual forward-run's journal dir.
func TestRunForward_AutoRollback_PinsFailedJournalDir(t *testing.T) {
	tx := setupV04xFixture(t)
	opts := fakeForwardOptions(t, tx)
	opts.ReconcileReady = func(timeout time.Duration) error {
		tx.reconcileWaited++
		return errors.New("supervisor never reported ready within 30s")
	}

	var capturedJournalDir string
	opts.RollbackOnFailure = func() *RollbackOptions {
		// Return a minimal RollbackOptions whose SupervisorIPC
		// captures the JournalDir RunForward set on the struct, then
		// no-ops the rest. The forward path is expected to OVERWRITE
		// or NOT-overwrite JournalDir; either way the test captures
		// whatever lands.
		rb := &RollbackOptions{
			Scheduler: tx.Scheduler,
			SupervisorIPC: func(cmd string, args map[string]any, timeout time.Duration) error {
				return nil
			},
			ProbeSupervisorTokenMismatch: func() error { return nil },
			ForceKillSupervisor:          func() error { return nil },
			PortBindWait:                 func(port int, timeout time.Duration) error { return nil },
			LookupProcessIdentity: func(pid int) (ProcessIdentity, error) {
				return ProcessIdentity{}, errors.New("not found")
			},
			QuarantineTranslator: func(_ State) error { return nil },
			ShimUninstaller:      func() error { return nil },
			TimeWaitSettle:       10 * time.Millisecond,
			PortBindTimeout:      50 * time.Millisecond,
		}
		// The captured value is set BEFORE RunForward returns the
		// callback's result. Defer-style read won't work because
		// RunForward dereferences the returned pointer at call time;
		// we want the JournalDir as RunForward sees it. So expose
		// via a thunk that asserts a field RunForward writes.
		// Convention: RunForward sets JournalDir on the returned
		// struct AFTER calling the factory. The fix wires this by
		// having RunForward mutate rb.JournalDir between the factory
		// call and the RunRollback dispatch.
		return rb
	}

	// We need to read JournalDir AFTER RunForward populates it. The
	// production wiring writes rb.JournalDir = <forward journal dir>
	// between the factory call and RunRollback. The test captures it
	// via a wrapper around the callback.
	origCallback := opts.RollbackOnFailure
	opts.RollbackOnFailure = func() *RollbackOptions {
		rb := origCallback()
		// Wrap SupervisorIPC so it captures the (now-populated)
		// JournalDir on first IPC call (which is the quiesce-timers
		// frame at rollback step 2, AFTER RunForward set JournalDir).
		origIPC := rb.SupervisorIPC
		rb.SupervisorIPC = func(cmd string, args map[string]any, timeout time.Duration) error {
			if capturedJournalDir == "" {
				capturedJournalDir = rb.JournalDir
			}
			return origIPC(cmd, args, timeout)
		}
		return rb
	}

	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected error after auto-rollback completes, got nil")
	}
	if capturedJournalDir == "" {
		t.Fatal("expected RunForward to populate RollbackOptions.JournalDir before invoking RunRollback")
	}
	wantDir := mustFindJournalDir(t, tx.State.StateDir)
	if capturedJournalDir != wantDir {
		t.Fatalf("captured RollbackOptions.JournalDir = %s, want %s (the failed-forward journal)", capturedJournalDir, wantDir)
	}
}

// TestForwardMigration_ReconcileReadyTimeoutNoCallbackFallsBack
// verifies the fall-back path: when RollbackOnFailure is nil, the
// existing "consider --rollback-to-legacy" error fires unchanged.
func TestForwardMigration_ReconcileReadyTimeoutNoCallbackFallsBack(t *testing.T) {
	tx := setupV04xFixture(t)
	opts := fakeForwardOptions(t, tx)
	opts.ReconcileReady = func(timeout time.Duration) error {
		return errors.New("reconcile timeout")
	}
	// Intentionally NOT setting opts.RollbackOnFailure.

	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected manual-rollback error, got nil")
	}
	if !strings.Contains(err.Error(), "--rollback-to-legacy") {
		t.Fatalf("expected manual-rollback fallback message, got: %v", err)
	}
}

// TestForwardMigration_CrossVersionResumeUsesSnapshotVerbatim verifies
// Lane C P1 #6: when a journal already contains a
// canonical-template-snapshot.xml from a prior version's render, the
// resume code path must read it verbatim and NOT re-render.
func TestForwardMigration_CrossVersionResumeUsesSnapshotVerbatim(t *testing.T) {
	tx := setupV04xFixture(t)
	journalDir := tx.State.journalDirForTime(tx.State.Now())
	if err := os.MkdirAll(journalDir, 0700); err != nil {
		t.Fatal(err)
	}
	const oldRenderedMarker = "<!-- LEGACY-RENDERER-MARKER-DO-NOT-OVERWRITE -->"
	oldXML := oldRenderedMarker + "\n<Task>old-renderer-output</Task>"
	if err := os.WriteFile(filepath.Join(journalDir, "canonical-template-snapshot.xml"), []byte(oldXML), 0600); err != nil {
		t.Fatal(err)
	}
	// Touch prepared + pre-os-mutating so initOrResumeJournalDir
	// classifies this as operator-choice-forward-or-rollback and
	// resumes into the existing journal.
	for _, m := range []string{MarkerPrepared, MarkerPreOsMutating} {
		if err := touchMarker(journalDir, m); err != nil {
			t.Fatal(err)
		}
	}

	opts := fakeForwardOptions(t, tx)
	if err := RunForward(tx.State, opts); err != nil {
		t.Fatalf("forward (resume): %v", err)
	}
	gotXML, readErr := os.ReadFile(filepath.Join(journalDir, "canonical-template-snapshot.xml"))
	if readErr != nil {
		t.Fatalf("snapshot file disappeared: %v", readErr)
	}
	if !strings.Contains(string(gotXML), oldRenderedMarker) {
		t.Fatalf("canonical-template-snapshot.xml was re-rendered (marker lost). got=%q", string(gotXML))
	}
}

// TestForwardMigration_RequiresNonEmptyInstallDir verifies Lane C P1 #7:
// RunForward fails closed when State.InstallDir is empty, BEFORE any
// OS-mutating step runs.
func TestForwardMigration_RequiresNonEmptyInstallDir(t *testing.T) {
	tx := setupV04xFixture(t)
	tx.State.InstallDir = ""
	opts := fakeForwardOptions(t, tx)
	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected install-dir validation error, got nil")
	}
	if !strings.Contains(err.Error(), "InstallDir") {
		t.Fatalf("expected error mentioning InstallDir, got: %v", err)
	}
	if len(tx.Scheduler.DeletedTasks) != 0 {
		t.Fatalf("InstallDir validation must run before OS mutation: %v", tx.Scheduler.DeletedTasks)
	}
}

// TestForwardMigration_RequiresReachableInstallDir verifies the
// stat-check sub-case of Lane C P1 #7: InstallDir set to a path that
// doesn't exist must fail closed.
func TestForwardMigration_RequiresReachableInstallDir(t *testing.T) {
	tx := setupV04xFixture(t)
	tx.State.InstallDir = filepath.Join(t.TempDir(), "does-not-exist-anywhere")
	opts := fakeForwardOptions(t, tx)
	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected install-dir reachability error, got nil")
	}
	if !strings.Contains(err.Error(), "InstallDir") {
		t.Fatalf("expected error mentioning InstallDir, got: %v", err)
	}
}

// TestForwardMigration_Gate4FailureAborts is the explicit Lane F P0 #1
// abort assertion: port-bound PID failing Gate 4 → abort with
// MIGRATION_PORT_LOOKUP_INCONSISTENT (also covered by the renamed
// _4GateOwnershipMismatchAborts above; this variant uses a different
// gate to demonstrate the policy holds for any of the four gates).
func TestForwardMigration_Gate4FailureAborts(t *testing.T) {
	tx := setupV04xFixture(t)
	const otherPID = 9999
	tx.pidByServerDaemon["memory/default"] = otherPID
	tx.portByPID[otherPID] = 9128
	// Identity has a wrong basename (Gate 1 fails too, but the test
	// asserts the abort policy applies — the kill must NOT happen).
	tx.identityByPID[otherPID] = ProcessIdentity{
		PID:              otherPID,
		Basename:         "impostor.exe",
		CommandLine:      `mcphub.exe daemon --server memory --daemon default`,
		ExecutablePath:   filepath.Join(tx.State.InstallDir, "mcphub.exe"),
		CreationDateUnix: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Unix(),
	}

	opts := fakeForwardOptions(t, tx)
	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected gate abort, got nil")
	}
	if !errors.Is(err, ErrMigrationPortLookupInconsistent) {
		t.Fatalf("expected ErrMigrationPortLookupInconsistent, got: %v", err)
	}
	if len(tx.killedPIDs) != 0 {
		t.Fatalf("gate fail must abort before kill, got: %v", tx.killedPIDs)
	}
}

// TestForwardMigration_RetryExhaustionAbortsIfDaemonStillExists
// verifies Lane F P0 #2: when LookupProcessIdentity returns a
// non-ErrProcessNotFound failure (retry exhausted on transient
// transport error), migration must abort with
// MIGRATION_PORT_LOOKUP_INCONSISTENT — NOT continue with an
// "identity-lookup-failed" skip that hides a still-running daemon.
func TestForwardMigration_RetryExhaustionAbortsIfDaemonStillExists(t *testing.T) {
	tx := setupV04xFixture(t)
	const memPID = 4321
	tx.pidByServerDaemon["memory/default"] = memPID
	tx.portByPID[memPID] = 9128
	// Generic transport error (NOT ErrProcessNotFound).
	opts := fakeForwardOptions(t, tx)
	opts.LookupProcessIdentity = func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{}, errors.New("powershell transport hang after 3 retries")
	}
	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected retry-exhaustion abort, got nil")
	}
	if !errors.Is(err, ErrMigrationPortLookupInconsistent) {
		t.Fatalf("expected ErrMigrationPortLookupInconsistent, got: %v", err)
	}
}

// TestForwardMigration_ProcessNotFoundProceedsWhenCrossCheckSilent
// verifies the genuine-unbound path: LookupProcessIdentity returns
// ErrProcessNotFound AND a fresh PIDForServerDaemon scan finds no
// matching process → migration proceeds, treating the daemon as
// unbound.
func TestForwardMigration_ProcessNotFoundProceedsWhenCrossCheckSilent(t *testing.T) {
	tx := setupV04xFixture(t)
	const memPID = 4321
	tx.pidByServerDaemon["memory/default"] = memPID
	tx.portByPID[memPID] = 9128

	opts := fakeForwardOptions(t, tx)
	opts.LookupProcessIdentity = func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{}, ErrProcessNotFound
	}
	// The FIRST PIDForServerDaemon call resolves the seeded PID
	// (initial port-resolve). The SECOND call (cross-check after
	// ErrProcessNotFound) reports "no matching process" → daemon
	// genuinely gone.
	pidLookups := 0
	opts.PIDForServerDaemon = func(server, daemon string) (int, error) {
		pidLookups++
		if pidLookups == 1 {
			if p, ok := tx.pidByServerDaemon[server+"/"+daemon]; ok {
				return p, nil
			}
			return 0, ErrProcessNotFound
		}
		return 0, ErrProcessNotFound
	}

	if err := RunForward(tx.State, opts); err != nil {
		t.Fatalf("expected proceed (genuine-unbound), got: %v", err)
	}
}

// TestForwardMigration_ProcessNotFoundButCrossCheckPositiveAborts:
// LookupProcessIdentity says ErrProcessNotFound but the process-list
// cross-check still finds a matching mcphub daemon argv → abort with
// MIGRATION_PORT_LOOKUP_INCONSISTENT.
func TestForwardMigration_ProcessNotFoundButCrossCheckPositiveAborts(t *testing.T) {
	tx := setupV04xFixture(t)
	const memPID = 4321
	tx.pidByServerDaemon["memory/default"] = memPID
	tx.portByPID[memPID] = 9128

	opts := fakeForwardOptions(t, tx)
	opts.LookupProcessIdentity = func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{}, ErrProcessNotFound
	}
	// Cross-check always returns a positive (daemon respawned or
	// the lookup was racy).
	opts.PIDForServerDaemon = func(server, daemon string) (int, error) {
		return memPID + 1, nil
	}

	err := RunForward(tx.State, opts)
	if err == nil {
		t.Fatal("expected cross-check abort, got nil")
	}
	if !errors.Is(err, ErrMigrationPortLookupInconsistent) {
		t.Fatalf("expected ErrMigrationPortLookupInconsistent, got: %v", err)
	}
}

// TestRollback_LegacyDirMissingFatal verifies Lane C P0 #3: rollback
// with absent legacy-tasks/ AND no committed marker must return an
// error rather than swallow the missing directory and exit nil.
func TestRollback_LegacyDirMissingFatal(t *testing.T) {
	tx := setupV04xFixture(t)
	// Hand-craft a journal that simulates a partially-progressed
	// forward migration: os-mutating-complete reached, but NO
	// legacy-tasks/ subdirectory and NO committed marker.
	journalDir := tx.State.journalDirForTime(tx.State.Now())
	if err := os.MkdirAll(journalDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{MarkerPrepared, MarkerPreOsMutating, MarkerOsMutatingComplete} {
		if err := touchMarker(journalDir, m); err != nil {
			t.Fatal(err)
		}
	}
	// Deliberately do NOT create legacy-tasks/.

	opts := RollbackOptions{
		Scheduler:                    tx.Scheduler,
		ProbeSupervisorTokenMismatch: func() error { return nil },
		ShimUninstaller:              func() error { return nil },
		QuarantineTranslator:         func(_ State) error { return nil },
		TimeWaitSettle:               10 * time.Millisecond,
	}
	err := RunRollback(tx.State, opts)
	if err == nil {
		t.Fatal("expected fatal error for missing legacy-tasks/ without committed marker")
	}
	if !strings.Contains(err.Error(), "legacy-tasks") {
		t.Fatalf("expected error mentioning legacy-tasks, got: %v", err)
	}
}

// TestRollback_LegacyDirEmptyCommittedZeroDaemonsAllowed verifies the
// genuine-zero-daemon migration variant: legacy-tasks/ is empty AND
// the journal carries a `committed` marker. Rollback must succeed
// with a warning logged to rollback-warnings.json.
func TestRollback_LegacyDirEmptyCommittedZeroDaemonsAllowed(t *testing.T) {
	tx := setupV04xFixture(t)
	journalDir := tx.State.journalDirForTime(tx.State.Now())
	if err := os.MkdirAll(filepath.Join(journalDir, "legacy-tasks"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{MarkerPrepared, MarkerPreOsMutating, MarkerOsMutatingComplete, MarkerCommitted} {
		if err := touchMarker(journalDir, m); err != nil {
			t.Fatal(err)
		}
	}

	opts := RollbackOptions{
		Scheduler:                    tx.Scheduler,
		ProbeSupervisorTokenMismatch: func() error { return nil },
		ShimUninstaller:              func() error { return nil },
		QuarantineTranslator:         func(_ State) error { return nil },
		TimeWaitSettle:               10 * time.Millisecond,
	}
	if err := RunRollback(tx.State, opts); err != nil {
		t.Fatalf("expected success with warning for zero-daemon committed migration, got: %v", err)
	}
	raw, readErr := os.ReadFile(filepath.Join(journalDir, "rollback-warnings.json"))
	if readErr != nil {
		t.Fatalf("rollback-warnings.json missing: %v", readErr)
	}
	var w rollbackWarningsFile
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("parse warnings: %v", err)
	}
	foundZero := false
	for _, entry := range w.Warnings {
		if strings.Contains(entry.Reason, "zero") || strings.Contains(entry.Reason, "empty") {
			foundZero = true
		}
	}
	if !foundZero {
		t.Fatalf("expected zero-daemon warning, got: %+v", w.Warnings)
	}
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// mustFindJournalDir returns the single migration-journal-* in stateDir
// or fails the test (used in flow tests with the fixed-clock fixture).
func mustFindJournalDir(t *testing.T, stateDir string) string {
	t.Helper()
	entries, _ := os.ReadDir(stateDir)
	var matches []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "migration-journal-") {
			matches = append(matches, e.Name())
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 journal dir, got %d: %v", len(matches), matches)
	}
	return filepath.Join(stateDir, matches[0])
}

// ---------------------------------------------------------------------------
// codex round-3 Lane C P1 #4: pre-os-mutating panic durability.
// ---------------------------------------------------------------------------

// TestForwardMigration_PreOsMutatingDurableAcrossPanic pins the
// codex round-3 Lane C P1 #4 contract: a panic between the kill
// syscall returning and the canonical pre-os-mutating marker write
// MUST leave the canonical marker on disk so resume classifies the
// journal as operator-choice-forward-or-rollback.
//
// Without this, an OS mutation (daemon killed) would happen but the
// journal would still be classified as `prepared`-only → safe-abort
// → operator is unaware a daemon was killed.
//
// The test seeds a running daemon, then injects a KillPID stub that
// panics right after returning nil. The deferred-recover in
// preUnregisterDaemonStop must promote the in-flight sentinel to
// the canonical MarkerPreOsMutating before re-raising. The test
// re-recovers at its own boundary so it can inspect the journal.
func TestForwardMigration_PreOsMutatingDurableAcrossPanic(t *testing.T) {
	tx := setupV04xFixture(t)
	const memPID = 5454
	tx.pidByServerDaemon["memory/default"] = memPID
	tx.portByPID[memPID] = 9128
	// Seed a process-identity row that the 4-gate ownership check
	// will pass on — use the installDir as the executable path
	// anchor so Gate 4 (ExecutablePath under InstallDir) succeeds.
	exePath := filepath.Join(tx.State.InstallDir, "mcphub.exe")
	tx.identityByPID[memPID] = ProcessIdentity{
		PID:              memPID,
		Basename:         "mcphub.exe",
		CommandLine:      exePath + " daemon --server memory --daemon default",
		ExecutablePath:   exePath,
		CreationDateUnix: tx.State.Now().Add(-time.Hour).Unix(), // older than lockAcquired
	}

	opts := fakeForwardOptions(t, tx)
	// LookupProcessIdentity returns the seeded identity (so gate
	// passes), so the kill loop reaches KillPID.
	opts.LookupProcessIdentity = func(pid int) (ProcessIdentity, error) {
		if id, ok := tx.identityByPID[pid]; ok {
			return id, nil
		}
		return ProcessIdentity{}, ErrProcessNotFound
	}
	// Killing PID: record the kill (it really happened from the
	// operator's perspective), then PANIC to simulate a fatal
	// runtime fault between the syscall returning and the marker
	// write. The recover in preUnregisterDaemonStop must promote
	// the in-flight sentinel to MarkerPreOsMutating.
	opts.KillPID = func(pid int) error {
		tx.killedPIDs = append(tx.killedPIDs, pid)
		panic(fmt.Sprintf("simulated runtime fault after kill PID %d", pid))
		// Unreachable: panic terminates control flow. Required to
		// satisfy Go's missing-return analysis for non-builtin
		// non-os.Exit panics inside a function-literal body.
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate up to the test; got nil — recover swallowed the panic")
		}
		// Locate the journal directory the failed run created.
		journalDir := mustFindJournalDir(t, tx.State.StateDir)

		// Canonical pre-os-mutating marker MUST be on disk so resume
		// classifies as operator-choice-forward-or-rollback.
		if _, err := os.Stat(filepath.Join(journalDir, MarkerPreOsMutating)); err != nil {
			t.Errorf("pre-os-mutating marker missing after panic — resume would safe-abort despite a killed daemon: %v", err)
		}

		// Resume classifier must agree: a marker file alone is not
		// enough — the classifier's verdict is what gates real-world
		// recovery.
		verdict := ClassifyResume(journalDir)
		if verdict.Action != "operator-choice-forward-or-rollback" {
			t.Errorf("ClassifyResume action want operator-choice-forward-or-rollback, got %q (markers=%v)",
				verdict.Action, verdict.Markers)
		}

		// The in-flight sentinel should not persist alongside the
		// canonical marker after a successful promotion path. A
		// stray sentinel is benign for resume but indicates the
		// rename didn't fire; surface as a warning rather than a
		// hard fail so the test stays useful on platforms where
		// rename semantics differ.
		if _, err := os.Stat(filepath.Join(journalDir, MarkerPreOsMutatingInFlight)); err == nil {
			t.Logf("note: in-flight sentinel persisted alongside canonical marker (rename may have fallen back to touch + remove)")
		}

		// The kill DID happen — record this so the assertion stays
		// honest about why the durability test matters.
		if len(tx.killedPIDs) == 0 {
			t.Fatalf("KillPID stub was never invoked — the test fixture did not reach the kill site")
		}
	}()

	_ = RunForward(tx.State, opts)
}

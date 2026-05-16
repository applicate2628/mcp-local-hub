package migration

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
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
	DeletedTasks  []string
	CreatedTasks  []string // task names passed to CreateXML
	RunTasks      []string

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
	stateDir := t.TempDir()
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

// fakeForwardOptions returns a ForwardOptions wired to the fixture's
// fakes. The PowerShellProbe returns (true, nil) — clean Win11 host.
// The kill loop is a no-op because tx.pidByServerDaemon is empty.
func fakeForwardOptions(t *testing.T, tx *testFixture) ForwardOptions {
	t.Helper()
	return ForwardOptions{
		Scheduler:        tx.Scheduler,
		CurrentUser:      tx.CurrentUser,
		PowerShellProbe:  func() (bool, error) { return true, nil },
		WmicPresent:      func() bool { return true },
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
		PIDForServerDaemon: func(server, daemon string) (int, bool) {
			if p, ok := tx.pidByServerDaemon[server+"/"+daemon]; ok {
				return p, true
			}
			return 0, false
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

// TestForwardMigration_4GateOwnershipMismatchSkips seeds a running
// daemon BUT a wrong ExecutablePath (gate 4 fails). Migration logs +
// skips; the PID is NOT killed; pre-os-mutating is NOT touched.
func TestForwardMigration_4GateOwnershipMismatchSkips(t *testing.T) {
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
	if err := RunForward(tx.State, opts); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if len(tx.killedPIDs) != 0 {
		t.Fatalf("4-gate mismatch should skip kill, got: %v", tx.killedPIDs)
	}
	journalDir := mustFindJournalDir(t, tx.State.StateDir)
	if _, err := os.Stat(filepath.Join(journalDir, MarkerPreOsMutating)); err == nil {
		t.Fatal("pre-os-mutating should NOT exist when no kill succeeded")
	}
	// killed-daemons.json should still record the skip-reason.
	raw, _ := os.ReadFile(filepath.Join(journalDir, "killed-daemons.json"))
	var kd killedDaemonsFile
	_ = json.Unmarshal(raw, &kd)
	skipReason := ""
	for _, k := range kd.Killed {
		if k.PID == otherPID {
			skipReason = k.GateSkipped
		}
	}
	if !strings.Contains(skipReason, "ExecutablePath") {
		t.Fatalf("expected ExecutablePath gate-skip reason, got: %q", skipReason)
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

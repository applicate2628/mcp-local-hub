package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// ---------------------------------------------------------------------------
// RepairSerenaIntentFromRegistry unit tests.
//
// Reuses autoRegisterTestEnv (serena_auto_register_test.go) which isolates all
// state into a per-test temp tree: a temp workspaces.yaml (via
// defaultRegistryPathFn), a temp state dir (via SetDaemonStateRootForTest,
// where supervisor-intent.json + supervisor-events.log live), and a stubbed
// serena catalog (loadSerenaCatalogManifest -> autoRegisterCatalogManifest).
// The repair reads registry + intent under fresh locks and APPENDS missing
// serena daemon rows — these tests assert the append-not-replace contract, the
// introduce-crash deferral, and the lock-contention skip.
// ---------------------------------------------------------------------------

// liveWorkspace returns a real (existing) directory to stand in for a workspace
// path so the repair's stale-path filter (workspacePathStale -> os.Stat) treats
// the row as live, not deleted. t.TempDir handles cleanup. Tests that want a
// STALE row use a non-existent path explicitly instead.
func liveWorkspace(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// mustStateDir resolves the (test-redirected) daemon state dir to thread into
// RepairSerenaIntentFromRegistry — matching the root seedIntent writes to, so the
// repair targets the same supervisor-intent.json the test seeded.
func mustStateDir(t *testing.T) string {
	t.Helper()
	d, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("resolve state dir: %v", err)
	}
	return d
}

// seedSerenaRegistryRow loads regPath under the registry lock and writes one
// serena sentinel row for (path, port), then saves. It honors the production
// invariant WorkspaceKey == WorkspaceKey(WorkspacePath) (auto-register derives
// the key from the canonical path — serena_auto_register.go:110+187), which is
// also what HasSerenaDaemonForWorkspaceKey + BuildSupervisorDaemonsForSerena
// rely on. Returns the derived key so the test can assert on it.
func seedSerenaRegistryRow(t *testing.T, regPath, path string, port int) string {
	t.Helper()
	key := WorkspaceKey(path)
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		t.Fatalf("lock registry: %v", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey:  key,
		WorkspacePath: path,
		Language:      SerenaLanguageSentinel,
		Backend:       SerenaServerName,
		Port:          port,
		TaskName:      "mcp-local-hub-serena-" + key,
		Languages:     []string{"python"},
	}); err != nil {
		t.Fatalf("put serena row: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	return key
}

// healthySerenaDaemon materializes the production-shape per-workspace serena
// daemon (carrying a RuntimeSpec) for one workspace, using the same
// BuildSupervisorDaemonsForSerena the install fan-out and the repair use. This
// keeps the seeded "healthy" intent row byte-consistent with what the repair
// would append. The WorkspaceKey is derived from path (production invariant).
func healthySerenaDaemon(t *testing.T, path string, port int) SupervisorDaemon {
	t.Helper()
	dyn, err := BuildInMemorySerenaDynamicPoolManifest(autoRegisterCatalogManifest())
	if err != nil {
		t.Fatalf("build dynamic-pool manifest: %v", err)
	}
	ws := WorkspaceEntry{
		WorkspaceKey:  WorkspaceKey(path),
		WorkspacePath: path,
		Language:      SerenaLanguageSentinel,
		Port:          port,
	}
	daemons := BuildSupervisorDaemonsForSerena(dyn, []WorkspaceEntry{ws}, "", "mcphub.exe")
	if len(daemons) != 1 {
		t.Fatalf("BuildSupervisorDaemonsForSerena produced %d daemons, want 1", len(daemons))
	}
	return daemons[0]
}

// seedIntent writes a supervisor-intent.json under the redirected state dir.
func seedIntent(t *testing.T, f *SupervisorIntentFile) string {
	t.Helper()
	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("resolve state dir: %v", err)
	}
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, f); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	return intentPath
}

// readIntent re-reads the on-disk intent.
func readIntent(t *testing.T, intentPath string) *SupervisorIntentFile {
	t.Helper()
	got, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	return got
}

func repairSerenaIntentForTest(t *testing.T, stateDir string) (int, []string, error) {
	t.Helper()
	result, err := NewAPI().RepairSerenaIntentFromRegistry(stateDir)
	return result.Repaired, result.Deferred, err
}

func previewSerenaIntentForTest(t *testing.T, stateDir string) (int, []string, error) {
	t.Helper()
	result, err := NewAPI().PreviewSerenaIntentRepairFromRegistry(stateDir)
	return result.Repaired, result.Deferred, err
}

// TestSerenaIntentRepairResultOutcomeContractFailsClosed keeps the typed
// outcome wire honest even if a future terminal path is added incorrectly.
// Every valid terminal classification retains its exact result/error pairing;
// empty, unknown, and impossible pairs are converted to the only safe error
// outcome rather than being mistaken for a completed no-op downstream.
func TestSerenaIntentRepairResultOutcomeContractFailsClosed(t *testing.T) {
	cause := errors.New("injected repair failure")
	tests := []struct {
		name        string
		result      SerenaIntentRepairResult
		err         error
		wantOutcome SerenaIntentRepairOutcome
		wantErr     bool
	}{
		{
			name:        "completed",
			result:      SerenaIntentRepairResult{Outcome: SerenaIntentRepairOutcomeCompleted},
			wantOutcome: SerenaIntentRepairOutcomeCompleted,
		},
		{
			name:        "skipped registry lock",
			result:      SerenaIntentRepairResult{Outcome: SerenaIntentRepairOutcomeSkippedRegistryLock},
			wantOutcome: SerenaIntentRepairOutcomeSkippedRegistryLock,
		},
		{
			name:        "skipped intent lock",
			result:      SerenaIntentRepairResult{Outcome: SerenaIntentRepairOutcomeSkippedIntentLock},
			wantOutcome: SerenaIntentRepairOutcomeSkippedIntentLock,
		},
		{
			name:        "error with cause",
			result:      SerenaIntentRepairResult{Outcome: SerenaIntentRepairOutcomeError},
			err:         cause,
			wantOutcome: SerenaIntentRepairOutcomeError,
			wantErr:     true,
		},
		{
			name:        "empty outcome",
			result:      SerenaIntentRepairResult{},
			wantOutcome: SerenaIntentRepairOutcomeError,
			wantErr:     true,
		},
		{
			name:        "unknown outcome",
			result:      SerenaIntentRepairResult{Outcome: SerenaIntentRepairOutcome("future")},
			wantOutcome: SerenaIntentRepairOutcomeError,
			wantErr:     true,
		},
		{
			name:        "error without cause",
			result:      SerenaIntentRepairResult{Outcome: SerenaIntentRepairOutcomeError},
			wantOutcome: SerenaIntentRepairOutcomeError,
			wantErr:     true,
		},
		{
			name:        "completed with cause",
			result:      SerenaIntentRepairResult{Outcome: SerenaIntentRepairOutcomeCompleted},
			err:         cause,
			wantOutcome: SerenaIntentRepairOutcomeError,
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotErr := validateSerenaIntentRepairResult(tt.result, tt.err)
			if got.Outcome != tt.wantOutcome {
				t.Fatalf("outcome = %q, want %q", got.Outcome, tt.wantOutcome)
			}
			if (gotErr != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error=%v", gotErr, tt.wantErr)
			}
		})
	}

	result, err := NewAPI().RepairSerenaIntentFromRegistry("")
	if err == nil {
		t.Fatal("empty state dir must return a causal error")
	}
	if result.Outcome != SerenaIntentRepairOutcomeError {
		t.Fatalf("empty state dir outcome = %q, want %q", result.Outcome, SerenaIntentRepairOutcomeError)
	}
}

// ---------------------------------------------------------------------------
// 1. Healthy — every serena registry row has its intent daemon → (0, nil, nil)
//    and the intent file is unchanged.
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_Healthy_NoOp(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	path, port := liveWorkspace(t), 9150
	seedSerenaRegistryRow(t, regPath, path, port)
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, path, port)},
	})

	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent bytes before: %v", err)
	}

	result, err := NewAPI().RepairSerenaIntentFromRegistry(mustStateDir(t))
	if err != nil {
		t.Fatalf("RepairSerenaIntentFromRegistry: unexpected error: %v", err)
	}
	if result.Outcome != SerenaIntentRepairOutcomeCompleted {
		t.Fatalf("outcome = %q, want %q", result.Outcome, SerenaIntentRepairOutcomeCompleted)
	}
	repaired := result.Repaired
	deferred := result.Deferred
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (healthy intent)", repaired)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want none", deferred)
	}

	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent bytes after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("intent file changed on a healthy no-op repair:\nbefore=%s\nafter=%s", before, after)
	}
}

// ---------------------------------------------------------------------------
// 2. Missing row appended — 1 healthy + 1 orphan registry row + a spec-bearing
//    intent → repaired==1; the orphan's daemon is NOW present AND the
//    pre-existing daemons are STILL present (count up by exactly 1).
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_MissingRowAppended(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	orphanPath, orphanPort := liveWorkspace(t), 9151

	healthyKey := seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	orphanKey := seedSerenaRegistryRow(t, regPath, orphanPath, orphanPort)

	// Intent carries ONLY the healthy daemon (spec-bearing) — the orphan's
	// daemon is missing (the crash window).
	healthy := healthySerenaDaemon(t, healthyPath, healthyPort)
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthy},
	})
	beforeCount := len(readIntent(t, intentPath).Daemons)

	repaired, deferred, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("RepairSerenaIntentFromRegistry: unexpected error: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want none", deferred)
	}

	got := readIntent(t, intentPath)
	// Append, not replace: count up by exactly 1.
	if len(got.Daemons) != beforeCount+1 {
		t.Errorf("daemon count = %d, want %d (append exactly one, not replace-all)", len(got.Daemons), beforeCount+1)
	}
	// The orphan's daemon is now present.
	if !got.HasSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Errorf("orphan key %q daemon missing after repair; daemons: %+v", orphanKey, got.Daemons)
	}
	// The pre-existing healthy daemon is still present.
	if !got.HasSerenaDaemonForWorkspaceKey(healthyKey) {
		t.Errorf("pre-existing healthy key %q daemon dropped by repair; daemons: %+v", healthyKey, got.Daemons)
	}
}

// ---------------------------------------------------------------------------
// 3. Clobber-safety vs concurrent row — the intent ALREADY contains a serena
//    daemon for a key NOT in the registry rows driving `missing` (simulating an
//    auto-register row committed concurrently); the repair APPENDS the missing
//    one and does NOT remove the extra daemon (final set ⊇ both).
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_DoesNotClobberConcurrentRow(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	orphanPath, orphanPort := liveWorkspace(t), 9151
	// A daemon for a workspace that is NOT in the registry — e.g. a concurrent
	// auto-register committed it to the intent after our registry snapshot, or
	// its registry row lives in a region we did not read. The repair must NOT
	// remove it. (Intent-only — never stale-checked — so a fake path is fine.)
	const concurrentPath, concurrentPort = `C:\ws\gamma`, 9152

	// Registry has ONLY the orphan row (drives `missing`).
	orphanKey := seedSerenaRegistryRow(t, regPath, orphanPath, orphanPort)
	concurrentKey := WorkspaceKey(concurrentPath)

	// Intent carries ONLY the concurrent (spec-bearing) daemon — NOT the orphan.
	concurrent := healthySerenaDaemon(t, concurrentPath, concurrentPort)
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{concurrent},
	})

	repaired, deferred, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("RepairSerenaIntentFromRegistry: unexpected error: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1 (the orphan)", repaired)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want none", deferred)
	}

	got := readIntent(t, intentPath)
	// Final set ⊇ both: the appended orphan AND the untouched concurrent row.
	if !got.HasSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Errorf("orphan key %q daemon missing after repair; daemons: %+v", orphanKey, got.Daemons)
	}
	if !got.HasSerenaDaemonForWorkspaceKey(concurrentKey) {
		t.Errorf("concurrent key %q daemon CLOBBERED by repair (must be preserved); daemons: %+v", concurrentKey, got.Daemons)
	}
	if len(got.Daemons) != 2 {
		t.Errorf("daemon count = %d, want 2 (concurrent preserved + orphan appended)", len(got.Daemons))
	}
}

// ---------------------------------------------------------------------------
// 4. Introduce-crash defer — orphan row(s) + an intent with NO runtime_spec
//    daemon → (0, <keys>, nil), intent NOT written (no daemon added).
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_IntroduceCrashDefers(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	orphanPath, orphanPort := liveWorkspace(t), 9151
	orphanKey := seedSerenaRegistryRow(t, regPath, orphanPath, orphanPort)

	// Intent has a daemon WITHOUT a runtime_spec (a legacy/global row) — so
	// HasRuntimeSpecRow() is false: this is the first-introduce-crash shape.
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Command: "mcphub.exe"},
		},
	})
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent bytes before: %v", err)
	}

	repaired, deferred, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("RepairSerenaIntentFromRegistry: unexpected error: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (introduce-crash must defer, not append)", repaired)
	}
	if len(deferred) != 1 || deferred[0] != orphanKey {
		t.Errorf("deferred = %v, want [%q]", deferred, orphanKey)
	}

	// Intent NOT written: no daemon added, bytes unchanged.
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent bytes after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("intent file changed on an introduce-crash defer (must not write):\nbefore=%s\nafter=%s", before, after)
	}
	if got := readIntent(t, intentPath); got.HasSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Errorf("orphan key %q daemon must NOT be added on an introduce-crash defer; daemons: %+v", orphanKey, got.Daemons)
	}
}

// ---------------------------------------------------------------------------
// 5. Lock contended — the intent flock is held by the test goroutine BEFORE the
//    call → the repair returns a zero result and does NOT block or modify.
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_IntentLockContended_Skips(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	const healthyPath, healthyPort = `C:\ws\alpha`, 9150
	const orphanPath, orphanPort = `C:\ws\beta`, 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	seedSerenaRegistryRow(t, regPath, orphanPath, orphanPort)

	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent bytes before: %v", err)
	}

	// Hold the supervisor-intent flock from the test goroutine BEFORE the call.
	held := flock.New(intentPath + supervisorIntentLockSuffix)
	locked, err := held.TryLock()
	if err != nil {
		t.Fatalf("test acquire intent flock: %v", err)
	}
	if !locked {
		t.Fatal("test could not acquire the intent flock to simulate contention")
	}
	defer func() { _ = held.Unlock() }()

	result, err := NewAPI().RepairSerenaIntentFromRegistry(mustStateDir(t))
	if err != nil {
		t.Fatalf("RepairSerenaIntentFromRegistry: unexpected error on contended lock: %v", err)
	}
	if result.Outcome != SerenaIntentRepairOutcomeSkippedIntentLock {
		t.Errorf("outcome = %q, want %q", result.Outcome, SerenaIntentRepairOutcomeSkippedIntentLock)
	}
	if result.Repaired != 0 {
		t.Errorf("repaired = %d, want 0 (intent lock contended → skip)", result.Repaired)
	}
	if len(result.Deferred) != 0 {
		t.Errorf("deferred = %v, want none (contended skip, not a deferral)", result.Deferred)
	}

	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent bytes after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("intent file modified despite contended lock:\nbefore=%s\nafter=%s", before, after)
	}
}

// seedSerenaRegistryRowWithKey is seedSerenaRegistryRow but with an EXPLICIT
// WorkspaceKey that may diverge from WorkspaceKey(path) — used only to construct
// the corrupt/legacy-row case the divergence guard must fail closed on.
func seedSerenaRegistryRowWithKey(t *testing.T, regPath, key, path string, port int) {
	t.Helper()
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		t.Fatalf("lock registry: %v", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey:  key,
		WorkspacePath: path,
		Language:      SerenaLanguageSentinel,
		Backend:       SerenaServerName,
		Port:          port,
		TaskName:      "mcp-local-hub-serena-" + key,
		Languages:     []string{"python"},
	}); err != nil {
		t.Fatalf("put serena row: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 6. Idempotency / no-re-append — a SECOND repair after a successful append is a
//    strict no-op (0, nil, nil) with a byte-identical intent. Locks in the
//    load-bearing invariant that a repaired daemon is seen as present on the
//    next startup (TaskName == "\mcp-local-hub-serena-"+WorkspaceKey(path) and
//    detection keys off ws.WorkspaceKey == WorkspaceKey(path)).
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_SecondCallIsNoOp(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	orphanPath, orphanPort := liveWorkspace(t), 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	orphanKey := seedSerenaRegistryRow(t, regPath, orphanPath, orphanPort)

	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})

	// First repair appends the orphan.
	repaired, _, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("first repair: unexpected error: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("first repair: repaired = %d, want 1", repaired)
	}
	afterFirst, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent after first repair: %v", err)
	}
	if !readIntent(t, intentPath).HasSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Fatalf("orphan %q not present after first repair", orphanKey)
	}

	// Second repair must be a strict no-op — NOT re-append the same daemon.
	repaired2, deferred2, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("second repair: unexpected error: %v", err)
	}
	if repaired2 != 0 {
		t.Errorf("second repair: repaired = %d, want 0 (no re-append)", repaired2)
	}
	if len(deferred2) != 0 {
		t.Errorf("second repair: deferred = %v, want none", deferred2)
	}
	afterSecond, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent after second repair: %v", err)
	}
	if string(afterFirst) != string(afterSecond) {
		t.Errorf("intent changed on the second (no-op) repair — infinite re-append risk:\nafter1=%s\nafter2=%s", afterFirst, afterSecond)
	}
}

// ---------------------------------------------------------------------------
// 7. Registry-lock contention — the REGISTRY flock is held before the call (the
//    distinct early-return at the registry TryLock, separate from the intent
//    contention of test #5, and the more likely production contention since
//    auto-register holds the registry lock across its whole flow).
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_RegistryLockContended_Skips(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	const healthyPath, healthyPort = `C:\ws\alpha`, 9150
	const orphanPath, orphanPort = `C:\ws\beta`, 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	seedSerenaRegistryRow(t, regPath, orphanPath, orphanPort)
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent before: %v", err)
	}

	// Hold the registry flock from the test goroutine BEFORE the call.
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		t.Fatalf("test acquire registry lock: %v", err)
	}
	defer unlock()

	result, err := NewAPI().RepairSerenaIntentFromRegistry(mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error on contended registry lock: %v", err)
	}
	if result.Outcome != SerenaIntentRepairOutcomeSkippedRegistryLock {
		t.Errorf("outcome = %q, want %q", result.Outcome, SerenaIntentRepairOutcomeSkippedRegistryLock)
	}
	if result.Repaired != 0 {
		t.Errorf("repaired = %d, want 0 (registry lock contended → skip)", result.Repaired)
	}
	if len(result.Deferred) != 0 {
		t.Errorf("deferred = %v, want none (contended skip)", result.Deferred)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("intent modified despite contended registry lock:\nbefore=%s\nafter=%s", before, after)
	}
}

// ---------------------------------------------------------------------------
// 8. Missing intent file — a genuinely fresh host (no supervisor-intent.json)
//    has no runtime_spec, so EVERY serena row defers (the pool must be
//    introduced via migrate); the repair must NOT panic and must NOT create the
//    intent file.
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_MissingIntentFile_Defers(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	orphanPath, orphanPort := liveWorkspace(t), 9151
	orphanKey := seedSerenaRegistryRow(t, regPath, orphanPath, orphanPort)

	// Do NOT seed an intent — the file is absent.
	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("resolve state dir: %v", err)
	}
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if _, statErr := os.Stat(intentPath); !os.IsNotExist(statErr) {
		t.Fatalf("precondition: intent file should be absent, stat err = %v", statErr)
	}

	repaired, deferred, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error on missing intent file: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (no runtime_spec → defer)", repaired)
	}
	if len(deferred) != 1 || deferred[0] != orphanKey {
		t.Errorf("deferred = %v, want [%q]", deferred, orphanKey)
	}
	if _, statErr := os.Stat(intentPath); !os.IsNotExist(statErr) {
		t.Errorf("intent file created on a defer (must not write); stat err = %v", statErr)
	}
}

// ---------------------------------------------------------------------------
// 9. Divergence guard — a registry row whose WorkspaceKey != WorkspaceKey(path)
//    (hand-edited / legacy) is SKIPPED, not appended, so a later startup does
//    not re-append it forever. The intent is unchanged.
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_DivergentRow_SkippedNotReappended(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	const divergentPath, divergentPort = `C:\ws\delta`, 9153
	const divergentKey = "hand-edited-bogus-key"

	if divergentKey == WorkspaceKey(divergentPath) {
		t.Fatalf("test setup invalid: divergentKey must NOT equal WorkspaceKey(%q)", divergentPath)
	}

	// A spec-bearing intent (healthy daemon for a DIFFERENT workspace) so the
	// live-add path WOULD be reached if the guard did not skip the divergent row.
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	seedSerenaRegistryRowWithKey(t, regPath, divergentKey, divergentPath, divergentPort)
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})
	beforeCount := len(readIntent(t, intentPath).Daemons)

	repaired, deferred, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (divergent row skipped, healthy row already present)", repaired)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want none (skip is not a defer)", deferred)
	}
	got := readIntent(t, intentPath)
	if len(got.Daemons) != beforeCount {
		t.Errorf("daemon count = %d, want %d (divergent row must NOT be appended)", len(got.Daemons), beforeCount)
	}
	if got.HasSerenaDaemonForWorkspaceKey(divergentKey) {
		t.Errorf("divergent key %q was appended (must be skipped to avoid infinite re-append)", divergentKey)
	}
}

// ---------------------------------------------------------------------------
// 10. Stale-path filter (bot PR #256 F1) — a registry row whose workspace dir no
//     longer exists is SKIPPED, not appended: BuildSupervisorDaemonsForSerena
//     emits the descriptor verbatim and the supervisor sets cmd.Dir = d.Workspace,
//     so a gone dir would spawn-loop. The install fan-out filters these and so
//     must the repair.
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_StaleWorkspacePath_Skipped(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	// A registered workspace whose directory has been deleted/moved: the temp dir
	// exists but this child does not, so workspacePathStale -> os.Stat IsNotExist.
	stalePath, stalePort := filepath.Join(t.TempDir(), "deleted-workspace"), 9151

	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	staleKey := seedSerenaRegistryRow(t, regPath, stalePath, stalePort)

	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})
	beforeCount := len(readIntent(t, intentPath).Daemons)

	repaired, deferred, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (stale-path row filtered, not appended)", repaired)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want none (stale is a skip, not a defer)", deferred)
	}
	got := readIntent(t, intentPath)
	if len(got.Daemons) != beforeCount {
		t.Errorf("daemon count = %d, want %d (stale row must NOT be appended — would spawn-loop on a gone cmd.Dir)", len(got.Daemons), beforeCount)
	}
	if got.HasSerenaDaemonForWorkspaceKey(staleKey) {
		t.Errorf("stale key %q was appended (must be skipped)", staleKey)
	}
}

// ---------------------------------------------------------------------------
// 11. Legacy nil-spec row (bot PR #256 F2) — a workspace whose intent row carries
//     the matching task name but RuntimeSpec == nil (a pre-redesign descriptor
//     the reconciler excludes from the spawn set) is DEFERRED, not treated as
//     healthy and not appended as a duplicate task name.
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_LegacyNilSpecRow_Deferred(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	legacyPath, legacyPort := liveWorkspace(t), 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	legacyKey := seedSerenaRegistryRow(t, regPath, legacyPath, legacyPort)

	// Intent: a spec-bearing healthy daemon (so HasRuntimeSpecRow() is true and the
	// live-add path is reachable) PLUS a legacy PRE-REDESIGN serena row for the
	// legacy workspace — matching task name, but RuntimeSpec == nil.
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			healthySerenaDaemon(t, healthyPath, healthyPort),
			{TaskName: `\mcp-local-hub-serena-` + legacyKey, Server: SerenaServerName, Command: "mcphub.exe"},
		},
	})
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent before: %v", err)
	}

	repaired, deferred, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (a nil-spec legacy row must defer, not append a duplicate task name)", repaired)
	}
	if len(deferred) != 1 || deferred[0] != legacyKey {
		t.Errorf("deferred = %v, want [%q] (legacy nil-spec workspace deferred to migrate)", deferred, legacyKey)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("intent written on a legacy-defer (must not append a duplicate):\nbefore=%s\nafter=%s", before, after)
	}
}

func TestRepairSerenaIntentFromRegistry_EventLogLockContentionDoesNotBlockOrHoldStateLocks(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	orphanPath, orphanPort := liveWorkspace(t), 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	seedSerenaRegistryRow(t, regPath, orphanPath, orphanPort)
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})

	stateDir := mustStateDir(t)
	eventLock := flock.New(filepath.Join(stateDir, SupervisorEventLogFileLeaf) + supervisorEventLogLockSuffix)
	if err := eventLock.Lock(); err != nil {
		t.Fatalf("lock supervisor event log: %v", err)
	}
	defer func() { _ = eventLock.Unlock() }()

	done := make(chan struct{})
	var repaired int
	var deferred []string
	var repairErr error
	go func() {
		defer close(done)
		repaired, deferred, repairErr = repairSerenaIntentForTest(t, stateDir)
	}()

	select {
	case <-done:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("RepairSerenaIntentFromRegistry blocked on a contended supervisor event log lock")
	}
	if repairErr != nil {
		t.Fatalf("RepairSerenaIntentFromRegistry: unexpected error: %v", repairErr)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}
	if len(deferred) != 0 {
		t.Fatalf("deferred = %v, want none", deferred)
	}

	reg := NewRegistry(regPath)
	regUnlock, ok, err := reg.TryLock()
	if err != nil {
		t.Fatalf("try-lock registry after repair: %v", err)
	}
	if !ok {
		t.Fatal("registry lock remained held after repair returned")
	}
	regUnlock()

	intentLock := flock.New(intentPath + supervisorIntentLockSuffix)
	intentLocked, err := intentLock.TryLock()
	if err != nil {
		t.Fatalf("try-lock intent after repair: %v", err)
	}
	if !intentLocked {
		t.Fatal("intent lock remained held after repair returned")
	}
	_ = intentLock.Unlock()
}

// ---------------------------------------------------------------------------
// 12. PendingSerenaRemoval guard (unregister-resurrects-serena-intent fix,
//     mcphub-register-intent REVISE round 2 BLOCKING 1) — a registry row
//     flagged PendingSerenaRemoval is SKIPPED, not treated as an orphan, even
//     though it has no matching intent daemon (the exact transient shape
//     unregister's teardown-before-delete ordering produces). Mirrors tests
//     9/10's skip-not-defer-not-append shape.
// ---------------------------------------------------------------------------

func TestRepairSerenaIntentFromRegistry_PendingSerenaRemoval_SkippedNotReappended(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	pendingPath, pendingPort := liveWorkspace(t), 9151

	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	pendingKey := seedSerenaRegistryRow(t, regPath, pendingPath, pendingPort)

	// Flag the pending row exactly as PruneWorkspacePhases does before it
	// removes the matching intent descriptor — through the production owner,
	// so the row carries the same FRESH lease stamp a real in-flight teardown
	// has. (Hand-setting the bool alone would model an UNSTAMPED mark, which is
	// the interrupted-unregister case covered in
	// serena_pending_removal_lease_test.go, not the in-flight one asserted
	// here.)
	if err := NewRegistry(regPath).SetSerenaPendingRemoval(pendingKey, "", true); err != nil {
		t.Fatalf("SetSerenaPendingRemoval: %v", err)
	}

	// Intent carries ONLY the healthy daemon — the pending row's descriptor
	// has already been removed by the (simulated) in-flight unregister, which
	// is exactly what makes it look like an orphan without the flag.
	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent before: %v", err)
	}

	repaired, deferred, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (pending-removal row must NOT be resurrected)", repaired)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want none (pending-removal is a skip, not a defer)", deferred)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("intent written for a pending-removal row (must not append):\nbefore=%s\nafter=%s", before, after)
	}
	if got := readIntent(t, intentPath); got.HasSerenaDaemonForWorkspaceKey(pendingKey) {
		t.Errorf("pending-removal key %q was appended (must be skipped)", pendingKey)
	}
}

// ---------------------------------------------------------------------------
// 13. Preview symmetry (BLOCKING 3 fix) — PreviewSerenaIntentRepairFromRegistry
//     reports the SAME count/deferred-keys RepairSerenaIntentFromRegistry
//     would act on, but NEVER writes supervisor-intent.json.
// ---------------------------------------------------------------------------

func TestPreviewSerenaIntentRepairFromRegistry_ReportsWithoutWriting(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	orphanPath, orphanPort := liveWorkspace(t), 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	orphanKey := seedSerenaRegistryRow(t, regPath, orphanPath, orphanPort)

	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent before: %v", err)
	}

	wouldRepair, deferred, err := previewSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("PreviewSerenaIntentRepairFromRegistry: unexpected error: %v", err)
	}
	if wouldRepair != 1 {
		t.Errorf("wouldRepair = %d, want 1 (the orphan)", wouldRepair)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want none", deferred)
	}

	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent after preview: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("preview WROTE supervisor-intent.json (must be read-only):\nbefore=%s\nafter=%s", before, after)
	}
	if got := readIntent(t, intentPath); got.HasSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Errorf("preview appended the orphan key %q (must never write)", orphanKey)
	}

	// A REAL repair afterward must still append it — the preview did not
	// consume or otherwise disturb the orphan's eligibility.
	repaired, deferred2, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("RepairSerenaIntentFromRegistry after preview: unexpected error: %v", err)
	}
	if repaired != 1 {
		t.Errorf("repaired after preview = %d, want 1", repaired)
	}
	if len(deferred2) != 0 {
		t.Errorf("deferred after preview = %v, want none", deferred2)
	}
	if got := readIntent(t, intentPath); !got.HasSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Errorf("orphan key %q not appended by the real repair following a preview", orphanKey)
	}
}

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// ---------------------------------------------------------------------------
// Test harness.
//
// All tests run under -tags=test_state_path_env so api.DaemonStateDir() and
// api.DefaultRegistryPath() resolve from env vars (LOCALAPPDATA / USERPROFILE on
// Windows; XDG_STATE_HOME / HOME on POSIX) into a per-test temp dir instead of
// the real per-user state dir. The state dir is where the driver writes
// supervisor-intent.json.
// ---------------------------------------------------------------------------

// migrateSerenaTestEnv redirects every state/registry/home resolution into a
// fresh per-test temp tree and installs a fake canonical mcphub binary so the
// real api.Preflight (LookPath + ensureCanonicalMcphubPresent) passes for the
// in-memory dynamic-pool manifest (which carries command: go). Returns the
// resolved state dir (where supervisor-intent.json lands) and the manifest-
// override dir (seed serena/manifest.yaml under it).
func migrateSerenaTestEnv(t *testing.T) (stateDir, manifestDir string) {
	t.Helper()
	root := t.TempDir()
	stateDir = filepath.Join(root, "state")
	manifestDir = filepath.Join(root, "manifests")
	home := filepath.Join(root, "home")
	for _, d := range []string{stateDir, manifestDir, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// The api-layer state dir (api.DaemonStateDir, used by
	// InstallParsedManifest) must be forced via the sanctioned cross-package
	// test seam: under test_state_path_env on Windows, DaemonStateDir still
	// prefers the real KnownFolder resolver over LOCALAPPDATA, so an env var
	// alone would NOT redirect it and the install would write to the real
	// per-user state dir. SetDaemonStateRootForTest sets the override that
	// takes precedence over the resolver in BOTH build variants.
	restoreStateRoot := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreStateRoot)

	// The registry (api.DefaultRegistryPath) resolves from LOCALAPPDATA
	// (Windows) / XDG_STATE_HOME (POSIX); ensureCanonicalMcphubPresent +
	// os.UserHomeDir resolve from USERPROFILE (Windows) / HOME (POSIX). Point
	// all of them at the per-test tree so nothing touches real state.
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	// Belt-and-suspenders: the cli stateDirFunc honors this too (used by the
	// audit-log open path).
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)

	// Install a fake canonical mcphub at <home>/.local/bin/mcphub[.exe] so
	// ensureCanonicalMcphubPresent (os.Stat) passes inside the real install.
	canonicalDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(canonicalDir, 0o755); err != nil {
		t.Fatalf("mkdir canonical dir: %v", err)
	}
	canonicalName := "mcphub"
	if runtime.GOOS == "windows" {
		canonicalName = "mcphub.exe"
	}
	if err := os.WriteFile(filepath.Join(canonicalDir, canonicalName), []byte(""), 0o755); err != nil {
		t.Fatalf("write fake canonical mcphub: %v", err)
	}
	return stateDir, manifestDir
}

// seedSerenaManifest writes a serena/manifest.yaml into the override dir with
// the given body. The driver reads it via api.ManifestGet("serena").
func seedSerenaManifest(t *testing.T, manifestDir, body string) string {
	t.Helper()
	dir := filepath.Join(manifestDir, "serena")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir serena manifest dir: %v", err)
	}
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write serena manifest: %v", err)
	}
	return path
}

// legacy2DaemonManifestYAML is the legacy claude+codex shape (command: go so the
// real Preflight LookPath passes; the in-memory builder reuses this command).
const legacy2DaemonManifestYAML = `name: serena
kind: global
transport: native-http
command: go
base_args:
  - --from
  - git+https://example/serena
  - serena
  - start-mcp-server
  - --transport
  - streamable-http
env:
  PYTHONUNBUFFERED: "1"
daemons:
  - name: claude
    context: claude-code
    port: 9121
    extra_args: [--context, claude-code]
  - name: codex
    context: codex
    port: 9122
    extra_args: [--context, codex]
weekly_refresh: false
`

// alreadyMigratedManifestYAML is the dynamic-pool target shape (daemon_template
// present, no daemons[]).
const alreadyMigratedManifestYAML = `name: serena
kind: workspace-scoped
transport: native-http
command: go
base_args:
  - --from
  - git+https://example/serena
  - serena
daemon_template:
  context: codex
  port_pool:
    start: 9150
    end: 9199
  extra_args_template:
    - --project
    - ${workspace.path}
weekly_refresh: false
`

// malformedManifestYAML is a partial/malformed shape: 3 daemons, no template.
const malformedManifestYAML = `name: serena
kind: global
transport: native-http
command: go
daemons:
  - name: claude
    context: claude-code
    port: 9121
  - name: codex
    context: codex
    port: 9122
  - name: extra
    context: other
    port: 9123
weekly_refresh: false
`

// stubInstall overrides installParsedManifestFn for the test scope and returns
// a restore func.
func stubInstall(t *testing.T, fn func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error)) func() {
	t.Helper()
	orig := installParsedManifestFn
	installParsedManifestFn = fn
	return func() { installParsedManifestFn = orig }
}

// stubReconcile overrides reconcileSerenaClientsFn for the test scope.
func stubReconcile(t *testing.T, fn func(ctx context.Context, w io.Writer) (*api.MigrateReport, error)) func() {
	t.Helper()
	orig := reconcileSerenaClientsFn
	reconcileSerenaClientsFn = fn
	return func() { reconcileSerenaClientsFn = orig }
}

// stubReap overrides migrateSerenaReapFn for the test scope (the §7.1
// reap-first primitive — runs BEFORE the spec-bearing intent write).
func stubReap(t *testing.T, fn func(ctx context.Context, w io.Writer) error) func() {
	t.Helper()
	orig := migrateSerenaReapFn
	migrateSerenaReapFn = fn
	return func() { migrateSerenaReapFn = orig }
}

// stubStart overrides migrateSerenaStartFn for the test scope (starts the
// successor supervisor AFTER the intent write commits).
func stubStart(t *testing.T, fn func(ctx context.Context, w io.Writer) error) func() {
	t.Helper()
	orig := migrateSerenaStartFn
	migrateSerenaStartFn = fn
	return func() { migrateSerenaStartFn = orig }
}

// stubSupervisorHealthy overrides migrateSerenaSupervisorHealthyFn for the test
// scope (Fix 5 idempotency-recovery health probe). The default impl probes the
// real supervisor lock / IPC; tests inject a deterministic verdict.
func stubSupervisorHealthy(t *testing.T, fn func() (bool, error)) func() {
	t.Helper()
	orig := migrateSerenaSupervisorHealthyFn
	migrateSerenaSupervisorHealthyFn = fn
	return func() { migrateSerenaSupervisorHealthyFn = orig }
}

// stubSupervisorRunning overrides migrateSerenaSupervisorRunningFn for the test
// scope (finding #3 pre-reap liveness probe). The default impl probes the real
// supervisor lock; cutover tests inject a deterministic verdict — true to model
// a running supervisor that gets reaped, false to model the no-supervisor cutover
// (write + start, no reap).
func stubSupervisorRunning(t *testing.T, fn func() (bool, error)) func() {
	t.Helper()
	orig := migrateSerenaSupervisorRunningFn
	migrateSerenaSupervisorRunningFn = fn
	return func() { migrateSerenaSupervisorRunningFn = orig }
}

// stubStartSupported overrides migrateSerenaStartSupportedFn for the test scope
// (finding #3 preflight). The default impl is the per-platform binding (true on
// Windows, false elsewhere); tests inject a deterministic verdict so the
// unsupported-start preflight can be exercised on any host (these tests run on
// Windows where the real binding returns true).
func stubStartSupported(t *testing.T, fn func() bool) func() {
	t.Helper()
	orig := migrateSerenaStartSupportedFn
	migrateSerenaStartSupportedFn = fn
	return func() { migrateSerenaStartSupportedFn = orig }
}

// stubAcquireInterlock overrides acquireSupervisorInterlockFn for the test scope
// (Phase 2 interlock seam). The default per-platform binding only acquires a real
// lock on Windows; tests inject a deterministic acquire so the interlock lifetime
// (held across reap→write→start, released before the start) is exercised on any
// host. realInterlockAcquire below is the canonical stub: it acquires the REAL
// supervisor.lock on the gate's exact path (api.DaemonStateDir()), matching the
// Windows production binding cross-platform.
func stubAcquireInterlock(t *testing.T, fn func() (*api.SupervisorLock, func(), error)) func() {
	t.Helper()
	orig := acquireSupervisorInterlockFn
	acquireSupervisorInterlockFn = fn
	return func() { acquireSupervisorInterlockFn = orig }
}

// realInterlockAcquire is a cross-platform stand-in for the Windows production
// interlock binding (defaultAcquireSupervisorInterlock): it acquires the REAL
// supervisor.lock on the §7.1 gate's exact path — filepath.Join(
// api.DaemonStateDir(), "supervisor.lock") — via the QUIET acquire (flock only,
// NO owner-sidecar write, matching production after bot PR #276 finding 1) and
// returns the handle plus an idempotent release. Using it as the acquire stub lets
// the lock-semantics tests (a contender's direct AcquireSupervisorLock must fail
// while held; the bypass token must pass the gate; a concurrent reap must read the
// OLD supervisor's intact sidecar rather than the migrate's PID) run identically on
// Windows and POSIX CI.
func realInterlockAcquire() (*api.SupervisorLock, func(), error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return nil, func() {}, err
	}
	lock, err := api.AcquireSupervisorLockQuiet(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		return nil, func() {}, err
	}
	released := false
	return lock, func() {
		if released {
			return
		}
		released = true
		lock.Release()
	}, nil
}

// seedSerenaWorkspace registers one serena workspace (sentinel row) WITH a port,
// rooted at an existing dir (so the install fan-out does not prune it as stale).
func seedSerenaWorkspace(t *testing.T, wsPath string) {
	t.Helper()
	seedSerenaWorkspaceWithPort(t, wsPath, 9150)
}

// seedSerenaWorkspaceNoPort registers a serena workspace WITHOUT a port so the
// migrate driver allocates one (the registry mutation rollback must undo).
func seedSerenaWorkspaceNoPort(t *testing.T, wsPath string) {
	t.Helper()
	seedSerenaWorkspaceWithPort(t, wsPath, 0)
}

func seedSerenaWorkspaceWithPort(t *testing.T, wsPath string, port int) {
	t.Helper()
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  api.WorkspaceKey(wsPath),
		WorkspacePath: wsPath,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          port,
	}); err != nil {
		t.Fatalf("put serena workspace: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
}

// loadRegistrySerenaPorts returns the workspace_key → port map of serena rows.
func loadRegistrySerenaPorts(t *testing.T, regPath string) map[string]int {
	t.Helper()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	out := map[string]int{}
	for _, e := range reg.SerenaEntries() {
		out[e.WorkspaceKey] = e.Port
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. Already-migrated → idempotent exit-0.
// ---------------------------------------------------------------------------

// TestMigrateSerena_CatalogDynamicPool_RuntimeLegacyMissing_Proceeds is the
// finding #2 regression guard (and the corrected contract that replaces the
// pre-fix catalog-shape no-op). The no-op decision is RUNTIME-AUTHORITATIVE: a
// dynamic-pool-shaped CATALOG (a future embedded-manifest update, or an operator
// editing only the manifest) does NOT make the migrate a no-op while THIS host's
// committed runtime intent is still legacy/missing. The catalog shape only
// classifies the SOURCE for building the manifest; with the cutover not yet run
// on this host, the migrate must PROCEED so the operator can actually cut over.
//
// (Before the fix, the catalog-shape branch short-circuited to no-op BEFORE the
// runtime check, leaving the legacy clients/supervisor in place with no way to
// run the cutover.)
func TestMigrateSerena_CatalogDynamicPool_RuntimeLegacyMissing_Proceeds(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	// CATALOG is ALREADY dynamic-pool shape (daemon_template present, no
	// daemons[]) → detectSerenaSourceState classifies it serenaSourceAlreadyMigrated.
	seedSerenaManifest(t, manifestDir, alreadyMigratedManifestYAML)
	// RUNTIME intent is MISSING (no supervisor-intent.json seeded) → the cutover
	// has NOT happened on this host.

	// One registered serena workspace (with a port already, rooted at an existing
	// dir) so the install fan-out materializes a spec-bearing row and the cutover
	// reap-first sequence (reap → install → start) fires — the strongest proof the
	// migrate PROCEEDED rather than no-op'd.
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	// Model a RUNNING supervisor (finding #3): a cutover with a live supervisor
	// reaps it before the spec-bearing write. The no-supervisor cutover is its own
	// test (TestMigrateSerena_NoRunningSupervisor_SkipsReap_WritesAndStarts).
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Stub start-support TRUE so this test exercises the install/reconcile/reap/start
	// path on non-Windows CI too (the default binding is false off Windows — bot PR
	// #250 finding #3; without it the run exits at the unsupported-start preflight).
	defer stubStartSupported(t, func() bool { return true })()

	reconcileInvoked := false
	reapInvoked := false
	startInvoked := false
	// REAL install (no stub) so the spec-bearing intent is actually written and we
	// can assert the cutover landed on disk.
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()
	restoreReap := stubReap(t, func(ctx context.Context, w io.Writer) error {
		reapInvoked = true
		return nil
	})
	defer restoreReap()
	restoreStart := stubStart(t, func(ctx context.Context, w io.Writer) error {
		startInvoked = true
		return nil
	})
	defer restoreStart()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("catalog-dynamic-pool + runtime-missing must PROCEED with the migration; got error: %v", err)
	}

	// The migration must have PROCEEDED: reconcile + reap + start all fired (the
	// full cutover sequence), NOT a no-op.
	if !reconcileInvoked || !reapInvoked || !startInvoked {
		t.Fatalf("catalog-dynamic-pool + runtime-missing must run the cutover (reconcile=%v reap=%v start=%v); a catalog-only no-op is the finding #2 bug",
			reconcileInvoked, reapInvoked, startInvoked)
	}
	// The no-op message must NOT appear.
	if strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("the migrate must not report a no-op when the runtime is still legacy/missing; got %q", buf.String())
	}
	// A spec-bearing supervisor-intent.json must now be on disk (the cutover
	// committed the dynamic-pool intent).
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("cutover must write supervisor-intent.json; read err = %v", err)
	}
	if !intent.HasRuntimeSpecRow() {
		t.Errorf("cutover must write a spec-bearing serena intent for the registered workspace; got %+v", intent.Daemons)
	}
}

// ---------------------------------------------------------------------------
// 2. Malformed → error.
// ---------------------------------------------------------------------------

func TestMigrateSerena_Malformed_Errors(t *testing.T) {
	_, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, malformedManifestYAML)

	installInvoked := false
	restoreInstall := stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		installInvoked = true
		return "", nil
	})
	defer restoreInstall()

	var buf bytes.Buffer
	err := runMigrateSerenaDynamicPool(context.Background(), &buf)
	if err == nil {
		t.Fatal("malformed manifest must error")
	}
	if !strings.Contains(err.Error(), "unrecognized state") || !strings.Contains(err.Error(), "manual reconciliation") {
		t.Errorf("error should name the malformed-state detection; got %v", err)
	}
	if installInvoked {
		t.Errorf("malformed manifest must fail before any install")
	}
}

// ---------------------------------------------------------------------------
// 3. Empty registry (zero workspaces) → installs zero-workspace intent, success.
//    claim #7 guard. Uses the REAL install so the zero-row intent is actually
//    written; reconcile + restart are stubbed (no live GUI; no spec rows means
//    the driver skips the restart anyway).
// ---------------------------------------------------------------------------

func TestMigrateSerena_EmptyRegistry_InstallsZeroWorkspaceIntent(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	reconcileInvoked := false
	reapInvoked := false
	startInvoked := false
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()
	restoreReap := stubReap(t, func(ctx context.Context, w io.Writer) error {
		reapInvoked = true
		return nil
	})
	defer restoreReap()
	restoreStart := stubStart(t, func(ctx context.Context, w io.Writer) error {
		startInvoked = true
		return nil
	})
	defer restoreStart()

	// No serena workspaces registered → registry stays empty.
	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("empty-registry migrate must succeed (claim #7); got error: %v", err)
	}
	// Intent file written with ZERO serena daemon rows.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("empty-registry migrate must write supervisor-intent.json; read err = %v", err)
	}
	for _, d := range intent.Daemons {
		if d.Server == "serena" {
			t.Errorf("zero-workspace install must write no serena daemon rows; found %+v", d)
		}
	}
	if intent.HasRuntimeSpecRow() {
		t.Errorf("zero-workspace install must have no runtime_spec rows")
	}
	// reconcile runs (clients still get pointed at the router); reap + start do
	// NOT (no spec-bearing write → no old-supervisor split-brain risk → no
	// cutover reap-first sequence).
	if !reconcileInvoked {
		t.Errorf("client-reconcile should run even on a zero-workspace migrate")
	}
	if reapInvoked {
		t.Errorf("no candidate workspaces → §7.1 reap must be skipped")
	}
	if startInvoked {
		t.Errorf("no candidate workspaces → §7.1 supervisor start must be skipped")
	}
	if !strings.Contains(buf.String(), "zero daemon rows") {
		t.Errorf("output should explain the zero-workspace install; got %q", buf.String())
	}
	// Fix 6: a NON-cutover (zero workspaces, no reap) must NOT print the outage
	// warning — the legacy daemons stay online.
	if strings.Contains(buf.String(), "briefly takes serena OFFLINE") {
		t.Errorf("a zero-workspace install must NOT print the cutover outage warning; got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// 4. Does NOT rewrite the disk manifest (the verified-defect regression guard).
//    Uses the REAL install; asserts the on-disk manifest bytes are unchanged.
// ---------------------------------------------------------------------------

func TestMigrateSerena_CallsInstallParsedManifest_NotDiskWrite(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	manifestPath := seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read seeded manifest: %v", err)
	}

	// Register one serena workspace so the install fans out a real
	// runtime_spec row (proving the install path ran end-to-end).
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	// Model a running supervisor so the cutover reaps it (finding #3).
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Start-support TRUE so the install/reap/start path runs on non-Windows CI too
	// (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	reapInvoked := false
	startInvoked := false
	intentAbsentAtReap := false
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()
	restoreReap := stubReap(t, func(ctx context.Context, w io.Writer) error {
		reapInvoked = true
		// REAP-FIRST: the spec-bearing intent must NOT be written yet when the
		// reap fires (reap precedes the write).
		if _, statErr := os.Stat(intentPath); os.IsNotExist(statErr) {
			intentAbsentAtReap = true
		}
		return nil
	})
	defer restoreReap()
	restoreStart := stubStart(t, func(ctx context.Context, w io.Writer) error {
		startInvoked = true
		return nil
	})
	defer restoreStart()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("migrate must succeed; got error: %v (out=%s)", err, buf.String())
	}

	// THE core regression guard: the on-disk manifest bytes are UNCHANGED.
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("re-read manifest: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("migrate must NOT rewrite the disk manifest\nBEFORE:\n%s\nAFTER:\n%s", before, after)
	}

	// And the install actually materialized a runtime_spec row in the intent.
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if !intent.HasRuntimeSpecRow() {
		t.Fatalf("install should have materialized a runtime_spec row for the registered workspace")
	}
	if !reapInvoked {
		t.Errorf("a spec-bearing migrate must drive the §7.1 reap")
	}
	if !intentAbsentAtReap {
		t.Errorf("reap-first ordering violated: the spec-bearing intent was already on disk when the reap fired")
	}
	if !startInvoked {
		t.Errorf("a spec-bearing migrate must start the successor supervisor after the intent write")
	}
}

// ---------------------------------------------------------------------------
// 5. Driver rollback restores the registry on install failure (reap-first
//    ordering: reconcile + reap have ALREADY run by the time the install/intent
//    write fails). The deferred outer rollback restores the prior registry rows;
//    the recovery start fires to restore a running supervisor (the still-on-disk
//    OLD intent), and the reconcile-restore undo runs.
// ---------------------------------------------------------------------------

func TestMigrateSerena_RollbackRestoresRegistry_OnInstallFailure(t *testing.T) {
	_, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// Seed a serena workspace WITHOUT a port so the driver allocates one
	// (the registry mutation the rollback must undo).
	ws := t.TempDir()
	seedSerenaWorkspaceNoPort(t, ws)
	// Model a running supervisor so the cutover reaps it; the reap is what makes
	// the recovery start fire after the install failure (finding #3).
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Start-support TRUE so the install path (which then fails) runs on non-Windows
	// CI too (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	// Snapshot the registry BEFORE the migrate.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	beforeReg := loadRegistrySerenaPorts(t, regPath)

	// Reconcile + reap succeed (they run BEFORE the install in the reap-first
	// ordering); the install/intent write then fails. Its OWN inner rollback
	// already ran by contract; the driver's outer stack must then restore the
	// registry + reconcile, and the recovery start must fire (a supervisor was
	// reaped, so a successor must be restored to read the still-on-disk OLD
	// intent — never leave no-supervisor-running silently).
	reconcileRan := false
	reapRan := false
	recoveryStartRan := false
	restoreInstall := stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		return "", errors.New("synthetic install failure (intent write)")
	})
	defer restoreInstall()
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileRan = true
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()
	restoreReap := stubReap(t, func(ctx context.Context, w io.Writer) error {
		reapRan = true
		return nil
	})
	defer restoreReap()
	restoreStart := stubStart(t, func(ctx context.Context, w io.Writer) error {
		recoveryStartRan = true
		return nil
	})
	defer restoreStart()

	var buf bytes.Buffer
	err = runMigrateSerenaDynamicPool(context.Background(), &buf)
	if err == nil {
		t.Fatal("install failure must propagate as a migrate error")
	}
	if !strings.Contains(err.Error(), "synthetic install failure") {
		t.Errorf("error should carry the install failure; got %v", err)
	}
	if !reconcileRan || !reapRan {
		t.Errorf("reap-first ordering: reconcile + reap must run BEFORE the install (reconcile=%v reap=%v)", reconcileRan, reapRan)
	}
	if !recoveryStartRan {
		t.Errorf("recovery invariant: a supervisor was reaped then the write failed, so the recovery start must fire to restore a running supervisor")
	}

	// The registry must be restored to its pre-migrate serena port state.
	afterReg := loadRegistrySerenaPorts(t, regPath)
	if len(afterReg) != len(beforeReg) {
		t.Fatalf("registry row count changed after rollback: before=%v after=%v", beforeReg, afterReg)
	}
	for key, port := range beforeReg {
		if afterReg[key] != port {
			t.Errorf("registry rollback mismatch for %s: before port=%d, after port=%d", key, port, afterReg[key])
		}
	}
}

// TestMigrateSerena_RegistryRollback_SurgicalPreservesConcurrentSerenaRow is the
// finding #1 regression guard. The registry rollback (restoreSerenaRegistryRelocking)
// runs after the lock was released during the reconcile/reap window (round-2
// Fix 3), so a CONCURRENT `mcphub workspace register` for serena may have
// committed a NEW serena row during that window. The old blanket
// "drop every serena row, re-add the snapshot" reset DELETED that concurrent
// row even though the migrate never touched it. The surgical restore must
// revert ONLY the snapshotted workspace keys and leave the concurrent serena
// row (and any non-serena row) intact.
func TestMigrateSerena_RegistryRollback_SurgicalPreservesConcurrentSerenaRow(t *testing.T) {
	migrateSerenaTestEnv(t)
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}

	// Snapshot = the serena rows as the migrate saw them BEFORE allocation:
	// workspace A with NO port yet (the migrate would allocate one).
	wsA := api.WorkspaceKey("/ws/a")
	snapshot := []api.WorkspaceEntry{{
		WorkspaceKey:  wsA,
		WorkspacePath: "/ws/a",
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          0,
	}}

	// On-disk registry as it stands at rollback time:
	//   - workspace A: the migrate allocated port 9150 (must be reverted to 0).
	//   - workspace B: a CONCURRENT serena registration that committed during
	//     the released-lock window (NOT in the snapshot → must survive).
	//   - workspace C: a non-serena LSP row (must survive).
	wsB := api.WorkspaceKey("/ws/b")
	wsC := api.WorkspaceKey("/ws/c")
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey: wsA, WorkspacePath: "/ws/a", Language: api.SerenaLanguageSentinel, Backend: "serena", Port: 9150,
	}); err != nil {
		t.Fatalf("put serena A: %v", err)
	}
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey: wsB, WorkspacePath: "/ws/b", Language: api.SerenaLanguageSentinel, Backend: "serena", Port: 9151,
	}); err != nil {
		t.Fatalf("put serena B (concurrent): %v", err)
	}
	if err := reg.PutLSP(api.WorkspaceEntry{
		WorkspaceKey: wsC, WorkspacePath: "/ws/c", Language: "go", Port: 9300,
	}); err != nil {
		t.Fatalf("put LSP C: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}

	// Run the surgical rollback.
	if err := restoreSerenaRegistryRelocking(regPath, snapshot); err != nil {
		t.Fatalf("restoreSerenaRegistryRelocking: %v", err)
	}

	// Re-read and assert.
	after := api.NewRegistry(regPath)
	if err := after.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}

	// A: reverted to the pre-migrate snapshot (port 0).
	gotA, okA := after.GetSerena(wsA)
	if !okA {
		t.Fatalf("workspace A serena row missing after rollback")
	}
	if gotA.Port != 0 {
		t.Errorf("workspace A port = %d after rollback, want 0 (reverted to pre-migrate snapshot)", gotA.Port)
	}

	// B: the concurrent serena registration MUST survive unchanged (this is the
	// bug the surgical restore fixes — the old blanket reset deleted it).
	gotB, okB := after.GetSerena(wsB)
	if !okB {
		t.Fatalf("concurrent serena workspace B was DELETED by the rollback — finding #1 regression (surgical restore must preserve serena rows not in the snapshot)")
	}
	if gotB.Port != 9151 {
		t.Errorf("concurrent serena workspace B port = %d after rollback, want 9151 (untouched)", gotB.Port)
	}

	// C: the non-serena LSP row MUST survive unchanged.
	gotC, okC := after.Get(wsC, "go")
	if !okC {
		t.Fatalf("non-serena LSP workspace C was DELETED by the rollback")
	}
	if gotC.Port != 9300 {
		t.Errorf("LSP workspace C port = %d after rollback, want 9300 (untouched)", gotC.Port)
	}
}

// TestMigrateSerena_RegistryRollback_SkipsConcurrentlyUnregisteredKey is the
// finding #1 DUAL regression guard (bot PR #250). The surgical restore fixed the
// concurrent-REGISTER clobber (round-3): keys not in the snapshot are left
// intact. Its dual is the concurrent-UNREGISTER case: a snapshotted workspace may
// have been REMOVED by a concurrent `mcphub workspace unregister --backend serena`
// during the released-lock window. Blindly re-PutSerena-ing it would RESURRECT a
// row the user just removed (and which the migrate no longer owns). The restore
// must SKIP a snapshot key that has disappeared from the reloaded registry, while
// still reverting the snapshot keys that remain.
func TestMigrateSerena_RegistryRollback_SkipsConcurrentlyUnregisteredKey(t *testing.T) {
	migrateSerenaTestEnv(t)
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}

	// Snapshot = the serena rows as the migrate saw them BEFORE allocation:
	//   - workspace A: present then, with NO port (the migrate allocated one).
	//   - workspace D: present then, with NO port (the migrate allocated one).
	wsA := api.WorkspaceKey("/ws/a")
	wsD := api.WorkspaceKey("/ws/d")
	snapshot := []api.WorkspaceEntry{
		{WorkspaceKey: wsA, WorkspacePath: "/ws/a", Language: api.SerenaLanguageSentinel, Backend: "serena", Port: 0},
		{WorkspaceKey: wsD, WorkspacePath: "/ws/d", Language: api.SerenaLanguageSentinel, Backend: "serena", Port: 0},
	}

	// On-disk registry as it stands at rollback time:
	//   - workspace A: GONE — a CONCURRENT unregister removed it during the
	//     released-lock window (it is NOT in the reloaded registry).
	//   - workspace D: the migrate allocated port 9152 (must be reverted to 0).
	//   - workspace B: a CONCURRENT serena registration (not in snapshot → survives).
	wsB := api.WorkspaceKey("/ws/b")
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey: wsD, WorkspacePath: "/ws/d", Language: api.SerenaLanguageSentinel, Backend: "serena", Port: 9152,
	}); err != nil {
		t.Fatalf("put serena D: %v", err)
	}
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey: wsB, WorkspacePath: "/ws/b", Language: api.SerenaLanguageSentinel, Backend: "serena", Port: 9151,
	}); err != nil {
		t.Fatalf("put serena B (concurrent): %v", err)
	}
	// NOTE: workspace A is deliberately NOT put — it was concurrently unregistered.
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}

	// Run the surgical rollback.
	if err := restoreSerenaRegistryRelocking(regPath, snapshot); err != nil {
		t.Fatalf("restoreSerenaRegistryRelocking: %v", err)
	}

	after := api.NewRegistry(regPath)
	if err := after.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}

	// A: MUST NOT be resurrected (the concurrent unregister removed it; the migrate
	// no longer owns it). This is the finding #1 dual.
	if _, okA := after.GetSerena(wsA); okA {
		t.Fatalf("concurrently-unregistered workspace A was RESURRECTED by the rollback — finding #1 dual regression (the restore must skip snapshot keys that disappeared)")
	}

	// D: the remaining snapshot key MUST still be reverted to its pre-migrate port 0.
	gotD, okD := after.GetSerena(wsD)
	if !okD {
		t.Fatalf("workspace D serena row missing after rollback (it was present on disk and in the snapshot — must be reverted, not dropped)")
	}
	if gotD.Port != 0 {
		t.Errorf("workspace D port = %d after rollback, want 0 (reverted to pre-migrate snapshot)", gotD.Port)
	}

	// B: the concurrent serena registration MUST survive unchanged.
	gotB, okB := after.GetSerena(wsB)
	if !okB {
		t.Fatalf("concurrent serena workspace B was DELETED by the rollback")
	}
	if gotB.Port != 9151 {
		t.Errorf("concurrent serena workspace B port = %d after rollback, want 9151 (untouched)", gotB.Port)
	}
}

// ---------------------------------------------------------------------------
// 6. REAP-FIRST ordering: the reap fires BEFORE the spec-bearing intent write
//    and the start fires AFTER it; AND the migrate fails loud when the prior
//    supervisor cannot be reaped — WITHOUT writing the new intent (finding #1:
//    the spec-bearing write must never land while a stuck old supervisor runs).
// ---------------------------------------------------------------------------

func TestMigrateSerena_DrivesSupervisorRestart(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	// Both subtests model a RUNNING supervisor so the cutover reaps it before the
	// spec-bearing write (finding #3). The parent-level defer covers both subtests.
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Start-support TRUE (parent-level, covers both subtests) so the install/reap/
	// start path runs on non-Windows CI too (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()

	t.Run("reap fires BEFORE the intent write; start fires AFTER", func(t *testing.T) {
		// Reset any intent from a prior subtest run.
		_ = os.Remove(intentPath)

		reapCalled := false
		startCalled := false
		intentAbsentAtReap := false
		intentSpecBearingAtStart := false
		var order []string
		restoreReap := stubReap(t, func(ctx context.Context, w io.Writer) error {
			reapCalled = true
			order = append(order, "reap")
			// REAP-FIRST: the spec-bearing intent must NOT be on disk yet.
			if _, statErr := os.Stat(intentPath); os.IsNotExist(statErr) {
				intentAbsentAtReap = true
			}
			return nil
		})
		defer restoreReap()
		restoreStart := stubStart(t, func(ctx context.Context, w io.Writer) error {
			startCalled = true
			order = append(order, "start")
			// START-AFTER: the spec-bearing intent must already be committed.
			if intent, err := api.ReadSupervisorIntent(intentPath); err == nil && intent.HasRuntimeSpecRow() {
				intentSpecBearingAtStart = true
			}
			return nil
		})
		defer restoreStart()

		var buf bytes.Buffer
		if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
			t.Fatalf("migrate must succeed: %v (out=%s)", err, buf.String())
		}
		if !reapCalled || !startCalled {
			t.Fatalf("a spec-bearing migrate must reap then start (reap=%v start=%v)", reapCalled, startCalled)
		}
		if len(order) != 2 || order[0] != "reap" || order[1] != "start" {
			t.Fatalf("ordering must be reap → (write) → start; got %v", order)
		}
		if !intentAbsentAtReap {
			t.Errorf("reap-first violated: the spec-bearing intent was already on disk when the reap fired")
		}
		if !intentSpecBearingAtStart {
			t.Errorf("the start must fire AFTER the spec-bearing intent write (intent not present/spec-bearing at start time)")
		}
		// Fix 6: a real cutover prints the operator outage NOTE up front.
		if !strings.Contains(buf.String(), "briefly takes serena OFFLINE") {
			t.Errorf("Fix 6: a cutover must print the operator outage warning; got %q", buf.String())
		}
	})

	t.Run("fail-loud when the prior supervisor cannot be reaped — intent NOT written", func(t *testing.T) {
		// Reset any intent from a prior subtest run so the absence assertion is real.
		_ = os.Remove(intentPath)

		installCalled := false
		startCalled := false
		restoreInstall := stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
			installCalled = true
			return a.InstallParsedManifest(ctx, m, opts)
		})
		defer restoreInstall()
		restoreReap := stubReap(t, func(ctx context.Context, w io.Writer) error {
			// Mirror ReapSupervisorForRestart's non-nil return when ExitGraceful
			// fails AND force-kill fails with a non-already-exited cause.
			return errors.New("force-kill supervisor failed during reap-before-restart: ACCESS_DENIED")
		})
		defer restoreReap()
		restoreStart := stubStart(t, func(ctx context.Context, w io.Writer) error {
			startCalled = true
			return nil
		})
		defer restoreStart()

		var buf bytes.Buffer
		err := runMigrateSerenaDynamicPool(context.Background(), &buf)
		if err == nil {
			t.Fatal("migrate must FAIL LOUD when the supervisor reap fails")
		}
		if !strings.Contains(err.Error(), "supervisor reap (§7.1) failed BEFORE the runtime_spec intent write") {
			t.Errorf("error should name the §7.1 reap failure; got %v", err)
		}
		if !strings.Contains(err.Error(), "ACCESS_DENIED") {
			t.Errorf("error should preserve the underlying reap failure; got %v", err)
		}
		// THE finding #1 guard: the spec-bearing intent must NEVER be written
		// when the reap fails (no install, no intent file on disk).
		if installCalled {
			t.Errorf("the intent write must NOT run after a reap failure (reap-first: reap is the gate before the write)")
		}
		if _, statErr := os.Stat(intentPath); !os.IsNotExist(statErr) {
			t.Errorf("a reap failure must leave NO spec-bearing intent on disk; stat err = %v", statErr)
		}
		if startCalled {
			t.Errorf("no successor start should fire after a pre-write reap failure (nothing was reaped successfully)")
		}
	})
}

// TestMigrateSerena_ReapViaFakeDeps proves the production REAP binding shape via
// ReapSupervisorForRestart with a fakeUpgradeDeps (the install_upgrade_test.go
// precedent): the reap runs quiesce → exit → (force-kill fallback) WITHOUT a
// binary swap (finding #4 — same binary; rename-aside would abort) and WITHOUT
// starting a successor, and it fails loud when force-kill fails.
func TestMigrateSerena_ReapViaFakeDeps(t *testing.T) {
	// Happy path: quiesce (clean) → exit ACK. No force-kill, no rename, no start.
	happy := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{ID: 1, OK: true, Result: map[string]any{"drained": 1.0, "still_running": []any{}}, Final: true},
		exitResult:    api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}
	if err := ReapSupervisorForRestart(context.Background(), ReapOpts{PipePath: "fake-pipe", Deps: happy}); err != nil {
		t.Fatalf("reap happy path: %v", err)
	}
	if !happy.quiesceCalled || !happy.exitCalled {
		t.Errorf("reap must drive quiesce + exit (quiesce=%v exit=%v)", happy.quiesceCalled, happy.exitCalled)
	}
	// FINDING #4: the reap path must NEVER replace the binary or start a successor.
	if happy.renameAsideCalled {
		t.Error("the reap path must NOT call RenameAsideBinary (same-binary cutover; no replacement)")
	}
	if happy.startCalled {
		t.Error("the reap path must NOT call StartSupervisor (the driver starts the successor itself, after the intent write)")
	}

	// Fail-loud: exit times out AND force-kill fails with a non-already-exited
	// cause → ReapSupervisorForRestart returns non-nil (the driver fails loud
	// BEFORE the intent write). Still no rename, no start.
	stuck := &fakeUpgradeDeps{
		exitErr:      errors.New("timeout"),
		forceKillErr: errors.New("ACCESS_DENIED: insufficient privileges to terminate the supervisor PID"),
	}
	err := ReapSupervisorForRestart(context.Background(), ReapOpts{PipePath: "fake-pipe", ExitTimeoutMs: 5000, Deps: stuck})
	if err == nil {
		t.Fatal("a stuck supervisor (force-kill failure) must make ReapSupervisorForRestart return non-nil so the migrate fails loud before the write")
	}
	if stuck.renameAsideCalled || stuck.startCalled {
		t.Errorf("the reap path must never rename or start (rename=%v start=%v)", stuck.renameAsideCalled, stuck.startCalled)
	}
}

// TestMigrateSerena_ReapVerifiesPortsOnGracefulExit is the Fix 1 BLOCKER
// regression guard (PR #250 deeper review). The supervisor's handleExit writes
// the graceful-exit success ACK BEFORE it tears down its daemon children, and a
// job_protection:false child (PR #242 fallback) can outlive the supervisor exit
// holding its TCP port. So a CLEAN graceful exit (no force-kill) must STILL run
// VerifyPortsUnbound — and if a port stays bound, the reap must fail loud so the
// migrate aborts before the spec-bearing intent write (legacy stays
// recoverable). Before Fix 1 the verify ran only on the force-kill path, so a
// graceful exit with a lingering port-holding child returned nil and the caller
// started a new supervisor straight into EADDRINUSE.
func TestMigrateSerena_ReapVerifiesPortsOnGracefulExit(t *testing.T) {
	// Clean quiesce + clean exit ACK → the force-kill fallback never fires.
	graceful := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{ID: 1, OK: true, Result: map[string]any{"drained": 0.0, "still_running": []any{}}, Final: true},
		exitResult:    api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}
	verifyCalled := false
	verifyPorts := []int{}
	err := ReapSupervisorForRestart(context.Background(), ReapOpts{
		PipePath:      "fake-pipe",
		Deps:          graceful,
		ExpectedPorts: []int{9150, 9151},
		VerifyPortsUnbound: func(ports []int, _ time.Duration) error {
			verifyCalled = true
			verifyPorts = append(verifyPorts, ports...)
			// Model a job_protection:false child still holding 9150 after the
			// graceful exit ACK.
			return errors.New("port 9150 not released within 10s: listen tcp 127.0.0.1:9150: bind: address already in use")
		},
	})
	if err == nil {
		t.Fatal("a graceful exit whose daemon ports stay bound must make the reap fail loud (Fix 1: verify runs on BOTH paths)")
	}
	if !strings.Contains(err.Error(), "port-unbound verification failed after supervisor reap") {
		t.Errorf("error should name the post-reap port-verify failure; got %v", err)
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("error should preserve the underlying bind failure; got %v", err)
	}
	// The verify MUST have run even though force-kill never did (this is the fix).
	if !verifyCalled {
		t.Fatal("VerifyPortsUnbound must run on the CLEAN graceful-exit path (Fix 1); it did not")
	}
	if graceful.forceKillCalled {
		t.Error("a clean graceful exit must NOT force-kill — the verify must run on the graceful path WITHOUT a force-kill")
	}
	if len(verifyPorts) != 2 || verifyPorts[0] != 9150 || verifyPorts[1] != 9151 {
		t.Errorf("verify must receive the OLD on-disk ExpectedPorts; got %v", verifyPorts)
	}
	if graceful.renameAsideCalled || graceful.startCalled {
		t.Errorf("the reap path must never rename or start (rename=%v start=%v)", graceful.renameAsideCalled, graceful.startCalled)
	}

	// Counterpart: graceful exit + ports confirmed unbound → the reap succeeds.
	cleanGraceful := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{ID: 1, OK: true, Result: map[string]any{"drained": 0.0, "still_running": []any{}}, Final: true},
		exitResult:    api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}
	cleanVerifyCalled := false
	if err := ReapSupervisorForRestart(context.Background(), ReapOpts{
		PipePath:      "fake-pipe",
		Deps:          cleanGraceful,
		ExpectedPorts: []int{9150},
		VerifyPortsUnbound: func(_ []int, _ time.Duration) error {
			cleanVerifyCalled = true
			return nil
		},
	}); err != nil {
		t.Fatalf("graceful exit with ports confirmed unbound must succeed: %v", err)
	}
	if !cleanVerifyCalled {
		t.Error("the verify must run on the graceful path even when it passes")
	}
	if cleanGraceful.forceKillCalled {
		t.Error("a clean graceful exit must not force-kill")
	}
}

// ---------------------------------------------------------------------------
// 7. Pre-existing nil-spec serena rows healed before spawn.
//    Seed a nil-spec serena descriptor in supervisor-intent.json; after the
//    REAL install (wholesale row replacement) the row carries a fresh
//    RuntimeSpec.
// ---------------------------------------------------------------------------

func TestMigrateSerena_NilSpecRowsHealedBeforeSpawn(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	// Model a running supervisor so the cutover reaps it (finding #3).
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Start-support TRUE so the install/reap/start path runs on non-Windows CI too
	// (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	// Seed a PRE-EXISTING supervisor-intent.json carrying a nil-spec serena
	// row for the same workspace (the pre-RuntimeSpec on-disk state §7).
	taskName := api.SerenaTaskNameForWorkspace(ws)
	preIntent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName:    taskName,
			Server:      "serena",
			Workspace:   ws,
			Command:     "mcphub",
			Args:        []string{"daemon-serena", "--workspace", ws},
			Port:        9150,
			RuntimeSpec: nil, // the nil-spec row the migrate must heal
		}},
	}
	if err := api.WriteStateFileAtomic(filepath.Join(stateDir, "supervisor-intent.json"), preIntent); err != nil {
		t.Fatalf("seed pre-existing nil-spec intent: %v", err)
	}

	specBearingAtStart := false
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()
	restoreReap := stubReap(t, func(ctx context.Context, w io.Writer) error { return nil })
	defer restoreReap()
	restoreStart := stubStart(t, func(ctx context.Context, w io.Writer) error {
		// Assert the row is HEALED (spec materialized) BEFORE the start
		// seam fires — i.e. before any spawn would happen.
		intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
		if err == nil {
			for _, d := range intent.Daemons {
				if d.Server == "serena" && d.RuntimeSpec != nil {
					specBearingAtStart = true
				}
			}
		}
		return nil
	})
	defer restoreStart()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("migrate must succeed: %v (out=%s)", err, buf.String())
	}

	// After the migrate, the serena row carries a fresh RuntimeSpec (the
	// nil-spec row was replaced wholesale by InstallParsedManifest).
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read healed intent: %v", err)
	}
	healed := false
	for _, d := range intent.Daemons {
		if d.Server == "serena" {
			if d.RuntimeSpec == nil {
				t.Errorf("serena row still has nil RuntimeSpec after migrate: %+v", d)
			} else {
				healed = true
			}
		}
	}
	if !healed {
		t.Fatal("expected at least one healed serena row with a fresh RuntimeSpec")
	}
	if !specBearingAtStart {
		t.Errorf("the row must be healed BEFORE the supervisor start/spawn (was nil-spec at start time)")
	}
}

// stubRestoreReconcile overrides restoreReconcileFn for the test scope.
func stubRestoreReconcile(t *testing.T, fn func(report *api.MigrateReport) error) func() {
	t.Helper()
	orig := restoreReconcileFn
	restoreReconcileFn = fn
	return func() { restoreReconcileFn = orig }
}

// ---------------------------------------------------------------------------
// (finding #1, the GAP that let the production-breaking flaw through) Models a
// LIVE supervisor at migrate time by holding the real supervisor.lock flock —
// the same signal SupervisorRunningUnderStateDir (and thus the §7.1
// InstallParsedManifest write gate) reads. With the REAL install, the
// spec-bearing write would be REFUSED while the supervisor lock is held. The
// reap-first ordering must reap (here: stubbed to RELEASE the flock, modeling
// the real reap killing the supervisor) BEFORE the write, so the gate passes
// naturally. The prior write-first ordering FAILED here because the gate refused
// the write while the supervisor ran; this test is the regression guard.
// ---------------------------------------------------------------------------

func TestMigrateSerena_LiveSupervisor_ReapClearsTheGateBeforeWrite(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	// Start-support TRUE so the install/reap/start path runs on non-Windows CI too
	// (default false off Windows — finding #3). Supervisor liveness comes from the
	// REAL lock (held below), not a stub, so leave migrateSerenaSupervisorRunningFn.
	defer stubStartSupported(t, func() bool { return true })()

	// Hold the supervisor.lock flock → SupervisorRunningUnderStateDir reports a
	// LIVE supervisor → the REAL InstallParsedManifest §7.1 gate refuses a
	// spec-bearing write while it is held.
	lock, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock (model live supervisor): %v", err)
	}
	lockReleased := false
	defer func() {
		if !lockReleased {
			lock.Release()
		}
	}()

	// Sanity: with the lock held, the gate sees a running supervisor.
	if running, _, perr := api.SupervisorRunningUnderStateDir(stateDir); perr != nil || !running {
		t.Fatalf("precondition: supervisor must read as running while the lock is held (running=%v err=%v)", running, perr)
	}

	reapFiredBeforeWrite := false
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()
	// The reap RELEASES the flock — modeling the real reap (quiesce → exit →
	// force-kill) reaping the live supervisor so the lock frees. This is the ONLY
	// thing that lets the subsequent REAL InstallParsedManifest write pass the gate.
	restoreReap := stubReap(t, func(ctx context.Context, w io.Writer) error {
		reapFiredBeforeWrite = true
		lock.Release()
		lockReleased = true
		return nil
	})
	defer restoreReap()
	restoreStart := stubStart(t, func(ctx context.Context, w io.Writer) error { return nil })
	defer restoreStart()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("reap-first migrate must succeed (the reap clears the gate before the write); got error: %v (out=%s)", err, buf.String())
	}
	if !reapFiredBeforeWrite {
		t.Fatal("the reap must have fired (it is what releases the supervisor lock so the write gate passes)")
	}
	// The spec-bearing intent was written — proving the gate passed because the
	// reap ran first. Under the OLD write-first ordering this write would have
	// been REFUSED by the §7.1 gate while the supervisor lock was held.
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read intent after reap-first migrate: %v", err)
	}
	if !intent.HasRuntimeSpecRow() {
		t.Fatal("the spec-bearing intent must be committed after the reap cleared the gate")
	}
}

// ---------------------------------------------------------------------------
// 8. (finding #5) Already-migrated by RUNTIME state: the committed
//    supervisor-intent.json carries a serena dynamic-pool row (runtime_spec
//    present) even though the CATALOG manifest is still the legacy 2-daemon
//    shape (the migrate never rewrites the catalog). The migrate must be an
//    idempotent no-op — NO reconcile/reap/write/start, never bounce the healthy
//    supervisor.
// ---------------------------------------------------------------------------

func TestMigrateSerena_AlreadyMigratedByRuntimeIntent_Idempotent(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	// CATALOG is still legacy — catalog-shape alone would re-trigger forever.
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// Seed a committed supervisor-intent.json whose serena row ALREADY carries a
	// materialized RuntimeSpec (the runtime cutover already happened).
	ws := t.TempDir()
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	migratedIntent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName:  api.SerenaTaskNameForWorkspace(ws),
			Server:    "serena",
			Workspace: ws,
			Command:   "mcphub",
			Args:      []string{"daemon-serena-proxy", "--workspace", ws},
			Port:      9150,
			RuntimeSpec: &api.DaemonRuntimeSpec{
				SpecVersion:   api.DaemonRuntimeSpecVersion,
				ChildCommand:  "go",
				ChildArgs:     []string{"--project", ws, "--context", "codex"},
				UpstreamPort:  9150 + 1,
				ExternalPort:  9150,
				WorkspacePath: ws,
			},
		}},
	}
	if err := api.WriteStateFileAtomic(intentPath, migratedIntent); err != nil {
		t.Fatalf("seed already-migrated runtime intent: %v", err)
	}
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read seeded intent: %v", err)
	}

	installInvoked, reconcileInvoked, reapInvoked, startInvoked := false, false, false, false
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		installInvoked = true
		return "", nil
	})()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		return &api.MigrateReport{}, nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()
	// Fix 5: a GENUINE no-op requires the supervisor to be running AND
	// reconcile-ready. Stub the health probe healthy so this stays a no-op test
	// (the not-healthy path is the recovery test below).
	healthProbed := false
	defer stubSupervisorHealthy(t, func() (bool, error) { healthProbed = true; return true, nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("already-migrated-by-runtime must be a no-op exit-0; got error: %v", err)
	}
	if installInvoked || reconcileInvoked || reapInvoked || startInvoked {
		t.Errorf("runtime-already-migrated (healthy supervisor) must not install/reconcile/reap/start (install=%v reconcile=%v reap=%v start=%v)",
			installInvoked, reconcileInvoked, reapInvoked, startInvoked)
	}
	if !healthProbed {
		t.Errorf("the idempotency branch must probe supervisor health to decide no-op vs recovery")
	}
	if !strings.Contains(buf.String(), "already migrated") || !strings.Contains(buf.String(), "runtime intent") {
		t.Errorf("output should explain the runtime-state no-op; got %q", buf.String())
	}
	// The committed intent is untouched (we did not bounce the healthy supervisor).
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("re-read intent: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("runtime-already-migrated must not rewrite the committed intent")
	}
}

// TestMigrateSerena_AlreadyMigratedByRuntime_NoHealthySupervisor_Recovers is the
// Fix 5 recovery guard (PR #250 deeper review — consultant Q2). A prior run
// committed the dynamic-pool intent then FAILED the start (Fix 4's readiness
// poll failed, or the host crashed): the committed intent is dynamic-pool but no
// reconcile-ready supervisor is running. Re-running must NOT silently no-op
// (which would leave the operator stuck — clients on the router, no daemons); it
// must drive the start to bring the already-committed intent live. Crucially it
// must NOT re-reap or re-write — the intent is already correct.
func TestMigrateSerena_AlreadyMigratedByRuntime_NoHealthySupervisor_Recovers(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	// CATALOG is still legacy — only the runtime intent says dynamic-pool.
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// Seed a committed dynamic-pool intent (the prior run's write that survived
	// even though the start failed).
	ws := t.TempDir()
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	migratedIntent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName:  api.SerenaTaskNameForWorkspace(ws),
			Server:    "serena",
			Workspace: ws,
			Command:   "mcphub",
			Args:      []string{"daemon-serena-proxy", "--workspace", ws},
			Port:      9150,
			RuntimeSpec: &api.DaemonRuntimeSpec{
				SpecVersion:   api.DaemonRuntimeSpecVersion,
				ChildCommand:  "go",
				ChildArgs:     []string{"--project", ws, "--context", "codex"},
				UpstreamPort:  9151,
				ExternalPort:  9150,
				WorkspacePath: ws,
			},
		}},
	}
	if err := api.WriteStateFileAtomic(intentPath, migratedIntent); err != nil {
		t.Fatalf("seed already-migrated runtime intent: %v", err)
	}
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read seeded intent: %v", err)
	}

	installInvoked, reconcileInvoked, reapInvoked, startInvoked := false, false, false, false
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		installInvoked = true
		return "", nil
	})()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		return &api.MigrateReport{}, nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()
	// Fix 5: NO reconcile-ready supervisor is running → recovery, not no-op.
	defer stubSupervisorHealthy(t, func() (bool, error) { return false, nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("recovery re-run must succeed; got error: %v (out=%s)", err, buf.String())
	}
	// THE Fix 5 guard: the start MUST fire (recovery), but NOT the reap or the
	// install/intent write (the intent is already correct).
	if !startInvoked {
		t.Error("recovery: the start must fire to bring the already-committed intent live")
	}
	if reapInvoked {
		t.Error("recovery must NOT re-reap (the intent is already correct; only the supervisor is missing)")
	}
	if installInvoked {
		t.Error("recovery must NOT re-write the intent (it is already dynamic-pool)")
	}
	if reconcileInvoked {
		t.Error("recovery must NOT re-reconcile clients (they are already on the router)")
	}
	if !strings.Contains(buf.String(), "recovering by starting the supervisor") {
		t.Errorf("output should explain the recovery; got %q", buf.String())
	}
	// The committed intent is byte-identical (no re-write).
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("re-read intent: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("recovery must not rewrite the already-committed intent")
	}
}

// TestMigrateSerena_AlreadyMigratedByRuntime_HealthProbeError_Recovers proves
// the conservative polarity of Fix 5: when supervisor health cannot be
// CONFIRMED (the probe errors — e.g. a lock-probe failure on a hardened host),
// the driver treats it as a recovery situation and drives the start rather than
// silently no-op'ing (which could strand a stuck/dead supervisor). The redundant
// start is benign: the supervisor singleton lock makes a duplicate exit, and the
// start's own readiness poll confirms any live supervisor.
func TestMigrateSerena_AlreadyMigratedByRuntime_HealthProbeError_Recovers(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	ws := t.TempDir()
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := api.WriteStateFileAtomic(intentPath, &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName:  api.SerenaTaskNameForWorkspace(ws),
			Server:    "serena",
			Workspace: ws,
			Port:      9150,
			RuntimeSpec: &api.DaemonRuntimeSpec{
				SpecVersion:   api.DaemonRuntimeSpecVersion,
				ChildCommand:  "go",
				ExternalPort:  9150,
				UpstreamPort:  9151,
				WorkspacePath: ws,
			},
		}},
	}); err != nil {
		t.Fatalf("seed runtime intent: %v", err)
	}

	reapInvoked, startInvoked := false, false
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()
	// Health probe ERRORS → not confirmed healthy → recovery.
	defer stubSupervisorHealthy(t, func() (bool, error) {
		return false, errors.New("probe supervisor liveness: flock probe failed")
	})()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("health-probe-error recovery must succeed; got error: %v (out=%s)", err, buf.String())
	}
	if !startInvoked {
		t.Error("a health-probe error must drive the recovery start (conservative polarity)")
	}
	if reapInvoked {
		t.Error("recovery must not re-reap")
	}
	if !strings.Contains(buf.String(), "could not determine supervisor health") {
		t.Errorf("output should surface the health-probe uncertainty; got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// 9. (finding #3) Partial reconcile (report.Failed non-empty) → the migrate
//    FAILS before the reap; the reconcile-restore + registry rollback fire;
//    legacy is untouched (no install, no reap, no start).
// ---------------------------------------------------------------------------

func TestMigrateSerena_PartialReconcile_FailsBeforeReap_RestoresClientsAndRegistry(t *testing.T) {
	_, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// Seed a serena workspace WITHOUT a port so the driver allocates one (the
	// registry mutation the rollback must undo).
	ws := t.TempDir()
	seedSerenaWorkspaceNoPort(t, ws)
	// Start-support TRUE so the run reaches the reconcile (where it then fails
	// partial) on non-Windows CI too, rather than exiting at the start preflight
	// (default false off Windows — finding #3). No supervisor is running here, so
	// willReap stays false regardless.
	defer stubStartSupported(t, func() bool { return true })()
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	beforeReg := loadRegistrySerenaPorts(t, regPath)

	installInvoked, reapInvoked, startInvoked := false, false, false
	restoreReconcileCalled := false
	var restoredReport *api.MigrateReport
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		installInvoked = true
		return "", nil
	})()
	// Reconcile reports ONE applied client + ONE failed client → partial.
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{
			Applied: []api.AppliedMigration{{Server: "serena", Client: "claude-code", URL: "http://127.0.0.1:9137/serena/mcp", BackupPath: "/fake/bak"}},
			Failed:  []api.FailedMigration{{Server: "serena", Client: "cursor", Err: "write denied"}},
		}, nil
	})()
	defer stubRestoreReconcile(t, func(report *api.MigrateReport) error {
		restoreReconcileCalled = true
		restoredReport = report
		return nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()

	var buf bytes.Buffer
	err = runMigrateSerenaDynamicPool(context.Background(), &buf)
	if err == nil {
		t.Fatal("a partial reconcile must FAIL the migrate (refuse to reap with a partially-migrated client set)")
	}
	if !strings.Contains(err.Error(), "client-reconcile to /serena/mcp router failed on 1 client") {
		t.Errorf("error should name the partial-reconcile failure; got %v", err)
	}
	// The reap/install/start must NOT run — legacy is untouched, point of no
	// return never reached.
	if reapInvoked {
		t.Errorf("the reap must NOT run after a partial reconcile (legacy stays up)")
	}
	if installInvoked {
		t.Errorf("the intent write must NOT run after a partial reconcile")
	}
	if startInvoked {
		t.Errorf("the supervisor start must NOT run after a partial reconcile")
	}
	// The reconcile-restore (restore already-rewritten clients to legacy) fired.
	if !restoreReconcileCalled {
		t.Errorf("the reconcile-restore compensator must fire on a partial reconcile")
	}
	if restoredReport == nil || len(restoredReport.Applied) != 1 || restoredReport.Applied[0].Client != "claude-code" {
		t.Errorf("the restore must receive the report with the applied claude-code row; got %+v", restoredReport)
	}
	// The registry is restored to its pre-migrate serena port state.
	afterReg := loadRegistrySerenaPorts(t, regPath)
	for key, port := range beforeReg {
		if afterReg[key] != port {
			t.Errorf("registry rollback mismatch for %s: before port=%d, after port=%d", key, port, afterReg[key])
		}
	}
}

// ---------------------------------------------------------------------------
// 10. (finding #2) Start failure AFTER the intent commit → the migrate fails
//     loud BUT the registry is NOT rolled back (the intent is the commit point;
//     rolling the registry back would create split-state: committed intent rows
//     with ports vs registry reverted to port 0).
// ---------------------------------------------------------------------------

func TestMigrateSerena_StartFailureAfterIntentCommit_DoesNotRollBackRegistry(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// Seed a serena workspace WITHOUT a port so the driver allocates one; after
	// the committed install the registry must RETAIN that allocation.
	ws := t.TempDir()
	seedSerenaWorkspaceNoPort(t, ws)
	// Model a running supervisor so the cutover reaps it (finding #3).
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Start-support TRUE so the install commits (and the start then fails) on
	// non-Windows CI too (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}

	restoreReconcileCalled := false
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	defer stubRestoreReconcile(t, func(report *api.MigrateReport) error {
		restoreReconcileCalled = true
		return nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { return nil })()
	// REAL install commits the spec-bearing intent; the START then fails.
	defer stubStart(t, func(ctx context.Context, w io.Writer) error {
		return errors.New("schtasks /Run failed: the supervisor scheduled task is missing")
	})()

	var buf bytes.Buffer
	err = runMigrateSerenaDynamicPool(context.Background(), &buf)
	if err == nil {
		t.Fatal("a post-commit start failure must surface as a migrate error")
	}
	if !strings.Contains(err.Error(), "supervisor start (§7.1) failed after the runtime_spec intent was committed") {
		t.Errorf("error should name the post-commit start failure; got %v", err)
	}
	if !strings.Contains(err.Error(), "registry is intentionally NOT rolled back") {
		t.Errorf("error should state the registry is NOT rolled back; got %v", err)
	}
	// FINDING #2: the registry allocation is RETAINED (no rollback → no split-state).
	afterReg := loadRegistrySerenaPorts(t, regPath)
	gotPort := afterReg[api.WorkspaceKey(ws)]
	if gotPort == 0 {
		t.Errorf("registry must RETAIN the allocated serena port after a post-commit start failure (got 0 → rolled back → split-state); rows=%+v", afterReg)
	}
	// And the committed intent is on disk with the matching daemon row.
	intent, ierr := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if ierr != nil {
		t.Fatalf("read committed intent: %v", ierr)
	}
	if !intent.HasRuntimeSpecRow() {
		t.Errorf("the spec-bearing intent must remain committed after a post-commit start failure")
	}
	// The reconcile-restore must NOT fire (the rollback stack was disarmed at the
	// commit point — disarming is what prevents the split-state).
	if restoreReconcileCalled {
		t.Errorf("the reconcile-restore must NOT fire after the intent commit (the outer rollback is disarmed)")
	}
}

// ---------------------------------------------------------------------------
// 11. (Fix 3) The registry flock is released BEFORE the multi-second reap, so a
//     concurrent registry op is not blocked across the reap window. The reap
//     stub TryLocks the registry lock and asserts it succeeds (proving the
//     migrate already released it after its Save).
// ---------------------------------------------------------------------------

func TestMigrateSerena_RegistryLockReleasedBeforeReap(t *testing.T) {
	_, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspaceNoPort(t, ws) // force an allocation (a real registry mutation under the lock)
	// Model a running supervisor so the cutover reaps it (finding #3); the reap
	// stub is where the lock-released assertion runs.
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Start-support TRUE so the install/reap/start path runs on non-Windows CI too
	// (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}

	lockFreeAtReap := false
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error {
		// TryLock the SAME lock file the registry uses (Registry.Lock locks
		// path + ".lock"). If the migrate still held it across the reap (Fix 3
		// regression), TryLock returns false. A separate flock handle from this
		// same process conflicts with the migrate's handle, so this is a real
		// "is the lock free right now" probe.
		fl := flock.New(regPath + ".lock")
		got, lerr := fl.TryLock()
		if lerr != nil {
			t.Errorf("reap-time TryLock errored: %v", lerr)
			return nil
		}
		if got {
			lockFreeAtReap = true
			_ = fl.Unlock()
		}
		return nil
	})()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { return nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("migrate must succeed: %v (out=%s)", err, buf.String())
	}
	if !lockFreeAtReap {
		t.Fatal("Fix 3: the registry flock must be RELEASED before the reap fires (it was still held)")
	}
}

// TestMigrateSerena_NoRunningSupervisor_SkipsReap_WritesAndStarts is the finding
// #3 regression guard (bot PR #250). willReap was previously len(allocated)>0, so
// the reap fired even when NO supervisor was running (the operator stopped it per
// §7.1 guidance, or a fresh host). On non-Windows the production reap stub always
// fails loud, so an operator who correctly stopped the supervisor could not
// complete the migrate even though the §7.1 install liveness gate was already
// satisfied (nothing to reap). The fix probes supervisor liveness before deciding
// to reap: a registered workspace + NO running supervisor → the reap is SKIPPED,
// the intent IS written (the gate passes — no supervisor), and a supervisor IS
// started afterward (still needed to bring the dynamic-pool intent live).
func TestMigrateSerena_NoRunningSupervisor_SkipsReap_WritesAndStarts(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	// One registered serena workspace (rooted at an existing dir, with a port) so
	// the install fans out a spec-bearing row — hasWorkspaces is true.
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	// NO supervisor running: the liveness probe reports not-running, and we do NOT
	// hold the supervisor.lock, so the REAL install's §7.1 gate also sees no
	// supervisor and ALLOWS the spec-bearing write.
	defer stubSupervisorRunning(t, func() (bool, error) { return false, nil })()
	// Start-support TRUE so the write + start path runs on non-Windows CI too
	// (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	reapInvoked, startInvoked := false, false
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	// REAL install (no stub) so the spec-bearing intent is genuinely committed —
	// proving the §7.1 gate passed because no supervisor is running.
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("a no-supervisor cutover must succeed (write + start, no reap); got error: %v (out=%s)", err, buf.String())
	}
	// THE finding #3 guard: the reap is NOT called (no supervisor to reap)…
	if reapInvoked {
		t.Error("the reap must be SKIPPED when no supervisor is running (finding #3)")
	}
	// …the intent IS written (spec-bearing)…
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("the intent must still be written when no supervisor is running; read err = %v", err)
	}
	if !intent.HasRuntimeSpecRow() {
		t.Errorf("the no-supervisor cutover must still write a spec-bearing intent; got %+v", intent.Daemons)
	}
	// …and a supervisor IS started (to bring the dynamic-pool intent live).
	if !startInvoked {
		t.Error("a supervisor must be started after the write to bring the dynamic-pool intent live (finding #3)")
	}
	// A no-supervisor cutover does not take an online daemon offline → no outage NOTE.
	if strings.Contains(buf.String(), "briefly takes serena OFFLINE") {
		t.Errorf("a no-supervisor cutover must NOT print the reap outage warning (nothing online to take offline); got %q", buf.String())
	}
}

// TestMigrateSerena_ConcurrentRegisterInWindow_AppearsInCommittedIntent is the
// finding #2 regression guard (bot PR #250). The migrate releases the registry
// lock after its first Save (Fix 3) so the slow reconcile + reap do not block
// concurrent registry ops. A concurrent `mcphub workspace register --backend
// serena` can therefore add a serena row DURING that released-lock window. Before
// the fix the install fanned out from the PRE-release `allocated` snapshot, so the
// new workspace was present in the registry + clients but ABSENT from
// supervisor-intent.json — the router would resolve a workspace whose daemon the
// restarted supervisor never spawns. The fix re-reads the registry under a
// re-acquired lock immediately before the install (and re-runs port allocation
// over the current rows), so the concurrently-registered workspace appears in the
// committed intent's daemon rows.
func TestMigrateSerena_ConcurrentRegisterInWindow_AppearsInCommittedIntent(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// One workspace registered BEFORE the migrate (the pre-window row).
	wsBefore := t.TempDir()
	seedSerenaWorkspace(t, wsBefore)
	// Model a running supervisor so the cutover reaps it (the reap stub is where we
	// simulate the concurrent register landing in the released-lock window).
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Start-support TRUE so the install/reap/start path runs on non-Windows CI too
	// (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	// The workspace that gets registered DURING the released-lock window. It is
	// rooted at an existing dir so the install fan-out does not prune it as stale,
	// and registered WITHOUT a port so the finding-#2 re-allocation must assign one.
	wsConcurrent := t.TempDir()

	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	// The reap fires AFTER the migrate released the registry lock and BEFORE the
	// finding-#2 re-read. Register the concurrent serena workspace here to model a
	// `mcphub workspace register` committing during the released-lock window.
	defer stubReap(t, func(ctx context.Context, w io.Writer) error {
		seedSerenaWorkspaceNoPort(t, wsConcurrent)
		return nil
	})()
	// REAL install (no stub) so the intent is genuinely committed and we can assert
	// the concurrent workspace's daemon row landed in it.
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { return nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("migrate must succeed: %v (out=%s)", err, buf.String())
	}

	// THE finding #2 guard: the committed intent carries a serena daemon row for
	// BOTH the pre-window workspace AND the concurrently-registered one.
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read committed intent: %v", err)
	}
	wantTasks := map[string]bool{
		api.SerenaTaskNameForWorkspace(wsBefore):     false,
		api.SerenaTaskNameForWorkspace(wsConcurrent): false,
	}
	for _, d := range intent.Daemons {
		if d.Server != "serena" {
			continue
		}
		if _, ok := wantTasks[d.TaskName]; ok {
			wantTasks[d.TaskName] = true
		}
	}
	if !wantTasks[api.SerenaTaskNameForWorkspace(wsConcurrent)] {
		t.Errorf("the concurrently-registered workspace %q is MISSING from the committed intent's daemon rows — finding #2 regression (the install must fan out from the re-read registry, not the pre-release snapshot); daemons=%+v", wsConcurrent, intent.Daemons)
	}
	if !wantTasks[api.SerenaTaskNameForWorkspace(wsBefore)] {
		t.Errorf("the pre-window workspace %q is missing from the committed intent's daemon rows; daemons=%+v", wsBefore, intent.Daemons)
	}
	// The concurrently-registered row must have been allocated a pool port by the
	// finding-#2 re-allocation (it was registered port-less).
	regPath, perr := api.DefaultRegistryPath()
	if perr != nil {
		t.Fatalf("registry path: %v", perr)
	}
	ports := loadRegistrySerenaPorts(t, regPath)
	if got := ports[api.WorkspaceKey(wsConcurrent)]; got == 0 {
		t.Errorf("the concurrently-registered port-less workspace must be allocated a pool port by the re-read+re-alloc; got 0 (rows=%+v)", ports)
	}
}

// ---------------------------------------------------------------------------
// 12. (Fix 2) The post-commit intent-verify re-read (intentHasRuntimeSpecRow)
//     fails with a real error AFTER a cutover reap+write. The driver must still
//     drive the recovery start (never leave no-supervisor-running silently)
//     rather than returning with the intent committed but no supervisor.
// ---------------------------------------------------------------------------

func TestMigrateSerena_PostCommitVerifyError_DrivesRecoveryStart(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	// Model a running supervisor so the cutover reaps it; the reap-specific
	// recovery message is what this test asserts (finding #3).
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Start-support TRUE so the install/reap path runs on non-Windows CI too
	// (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { return nil })()

	// Use the REAL install so the intent is genuinely committed; then corrupt the
	// committed intent file so the post-commit verify re-read (ReadSupervisorIntent)
	// fails — modeling the Windows file-handle/ACL contention race right after the
	// write. We do this by overriding the install seam to call the real install
	// THEN overwrite the file with non-JSON bytes.
	startCalled := false
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		path, ierr := a.InstallParsedManifest(ctx, m, opts)
		if ierr != nil {
			return "", ierr
		}
		// Corrupt the just-committed intent so the verify re-read errors (NOT
		// os.ErrNotExist — a genuine parse failure).
		if werr := os.WriteFile(path, []byte("{not-valid-json"), 0o600); werr != nil {
			t.Fatalf("corrupt committed intent for verify-error model: %v", werr)
		}
		return path, nil
	})()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startCalled = true; return nil })()

	var buf bytes.Buffer
	err := runMigrateSerenaDynamicPool(context.Background(), &buf)
	if err == nil {
		t.Fatal("a post-commit verify-read error must surface as a migrate error")
	}
	if !strings.Contains(err.Error(), "verify supervisor-intent runtime_spec rows after the committed write") {
		t.Errorf("error should name the post-commit verify failure; got %v", err)
	}
	// THE Fix 2 guard: even though the verify re-read failed, the recovery start
	// MUST fire (a supervisor was reaped; the committed intent must be brought
	// live) so we never leave no-supervisor-running silently.
	if !startCalled {
		t.Error("Fix 2: the verify-error path must drive the recovery start when a cutover reap occurred")
	}
	if !strings.Contains(buf.String(), "intent-verify re-read failed after the supervisor reap") {
		t.Errorf("output should explain the verify-error recovery; got %q", buf.String())
	}
	// The intent file is still on disk (committed, then corrupted by the model) —
	// the registry must NOT have been rolled back (it is the commit point).
	if _, statErr := os.Stat(intentPath); statErr != nil {
		t.Errorf("the committed intent file must remain on disk: %v", statErr)
	}
}

// TestMigrateSerena_PostCommitVerifyError_RecoveryStartAlsoFails surfaces BOTH
// errors when the verify-error recovery start ALSO fails.
func TestMigrateSerena_PostCommitVerifyError_RecoveryStartAlsoFails(t *testing.T) {
	_, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	// Model a running supervisor so the cutover reaps it (finding #3).
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()

	// Start-support TRUE so the install/reap path runs on non-Windows CI too
	// (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { return nil })()
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		path, ierr := a.InstallParsedManifest(ctx, m, opts)
		if ierr != nil {
			return "", ierr
		}
		if werr := os.WriteFile(path, []byte("{not-valid-json"), 0o600); werr != nil {
			t.Fatalf("corrupt committed intent: %v", werr)
		}
		return path, nil
	})()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error {
		return errors.New("schtasks /Run failed: supervisor task missing")
	})()

	var buf bytes.Buffer
	err := runMigrateSerenaDynamicPool(context.Background(), &buf)
	if err == nil {
		t.Fatal("a verify-error + failed recovery start must surface a migrate error")
	}
	if !strings.Contains(err.Error(), "verify supervisor-intent runtime_spec rows") {
		t.Errorf("error should carry the verify failure; got %v", err)
	}
	if !strings.Contains(err.Error(), "the supervisor start ALSO failed") {
		t.Errorf("error should surface BOTH the verify failure and the start failure; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 13. (finding #1 — bot PR #250) willStart is recomputed from the RE-READ install
//     snapshot, not the FIRST snapshot. When the first snapshot is EMPTY (so
//     willStart was fixed false) but a workspace is registered during the unlocked
//     reconcile/reap window, reReadAndAllocateSerenaForInstall picks it up and the
//     spec-bearing intent commits — the start MUST then fire even though the first
//     snapshot was empty. Without the recompute the start is skipped → clients on
//     the router, spec-bearing intent on disk, but no supervisor spawns the
//     newly-registered daemon.
// ---------------------------------------------------------------------------

func TestMigrateSerena_EmptyFirstSnapshot_WorkspaceRegisteredInWindow_RecomputesStart(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// FIRST snapshot is EMPTY: no serena workspace registered up front → the driver
	// computes hasWorkspaces=false, willStart=false, and (hasWorkspaces=false) never
	// probes supervisor liveness, so willReap=false (no reap fires for an empty
	// first snapshot).

	// The workspace that gets registered DURING the released-lock window. With an
	// empty first snapshot the reap is skipped, so the window injection point is the
	// RECONCILE stub (it fires after the first snapshot, before the finding-#2
	// re-read). Rooted at an existing dir + registered without a port so the re-read
	// re-allocation assigns one and the install fan-out does not prune it as stale.
	wsConcurrent := t.TempDir()
	// Start-support TRUE so the post-re-read recompute + start path runs on
	// non-Windows CI too (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	reconcileInvoked := false
	startInvoked := false
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		// Model a concurrent `mcphub workspace register --backend serena` committing
		// during the released-lock window (after the first snapshot, before re-read).
		seedSerenaWorkspaceNoPort(t, wsConcurrent)
		return &api.MigrateReport{}, nil
	})()
	// REAL install (no stub) so the spec-bearing intent is genuinely committed and
	// we can assert it carries the concurrently-registered workspace's row.
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("migrate must succeed: %v (out=%s)", err, buf.String())
	}
	if !reconcileInvoked {
		t.Fatal("reconcile must have run (the window-injection point)")
	}
	// THE finding #1 guard: the install fanned out a spec-bearing row for the
	// window-registered workspace…
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read committed intent: %v", err)
	}
	if !intent.HasRuntimeSpecRow() {
		t.Fatalf("the window-registered workspace must produce a spec-bearing intent; got %+v", intent.Daemons)
	}
	// …AND the start fired even though the FIRST snapshot was empty (willStart
	// recomputed from the re-read install snapshot). Before the fix, willStart was
	// fixed false from the empty first snapshot, so the start was SKIPPED.
	if !startInvoked {
		t.Fatal("finding #1: the supervisor start must fire when the re-read install snapshot is spec-bearing, even though the first snapshot was empty")
	}
}

// ---------------------------------------------------------------------------
// 14. (finding #2 — bot PR #250) The runtime-already-migrated branch reconciles
//     the committed intent against the CURRENT serena registry before declaring a
//     no-op. A serena workspace registered AFTER the initial cutover updates only
//     the registry (router-resolvable, but no daemon row → never spawned). The
//     re-run must fan it out (NOT no-op), reusing the install path; a fully-covered
//     intent + healthy supervisor must still be a genuine no-op.
// ---------------------------------------------------------------------------

// seedDynamicPoolIntentForWorkspaces writes a committed dynamic-pool
// supervisor-intent.json carrying a spec-bearing serena daemon row per workspace
// path (so serenaRuntimeIntentIsDynamicPool reports already-migrated and the
// finding-#2 drift check joins on SerenaTaskNameForWorkspace).
func seedDynamicPoolIntentForWorkspaces(t *testing.T, stateDir string, wsPaths ...string) {
	t.Helper()
	intent := &api.SupervisorIntentFile{Version: 1}
	port := 9150
	for _, ws := range wsPaths {
		intent.Daemons = append(intent.Daemons, api.SupervisorDaemon{
			TaskName:  api.SerenaTaskNameForWorkspace(ws),
			Server:    "serena",
			Workspace: ws,
			Command:   "mcphub",
			Args:      []string{"daemon-serena-proxy", "--workspace", ws},
			Port:      port,
			RuntimeSpec: &api.DaemonRuntimeSpec{
				SpecVersion:   api.DaemonRuntimeSpecVersion,
				ChildCommand:  "go",
				ChildArgs:     []string{"--project", ws, "--context", "codex"},
				UpstreamPort:  port + 100,
				ExternalPort:  port,
				WorkspacePath: ws,
			},
		})
		port++
	}
	if err := api.WriteStateFileAtomic(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed dynamic-pool intent: %v", err)
	}
}

func TestMigrateSerena_AlreadyMigrated_RegistryDrift_RefansOut(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	// CATALOG is still legacy — only the runtime intent says dynamic-pool.
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// Two serena workspaces in the CURRENT registry, both rooted at existing dirs.
	wsCovered := t.TempDir()
	wsDrifted := t.TempDir()
	seedSerenaWorkspace(t, wsCovered)
	seedSerenaWorkspace(t, wsDrifted)

	// Committed intent covers ONLY wsCovered → wsDrifted is registry drift (a
	// workspace registered after the initial cutover, present in the registry but
	// missing from the intent).
	seedDynamicPoolIntentForWorkspaces(t, stateDir, wsCovered)

	// Model a HEALTHY supervisor: without the finding-#2 drift check this would
	// short-circuit to the genuine no-op (the strongest proof the re-fanout is
	// drift-driven, not health-driven).
	defer stubSupervisorHealthy(t, func() (bool, error) { return true, nil })()
	// A running supervisor → the re-fanout reaps it before the spec-bearing re-write.
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Start-support TRUE so the re-fanout install/reap/start path runs on
	// non-Windows CI too (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	reconcileInvoked, reapInvoked, startInvoked := false, false, false
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		return &api.MigrateReport{}, nil
	})()
	// REAL install (no stub) so the re-fanout genuinely commits the intent and we
	// can assert the drifted workspace's daemon row landed in it.
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("drift re-fanout must succeed; got error: %v (out=%s)", err, buf.String())
	}
	// THE finding #2 guard: drift drives a re-fanout (install runs), NOT a no-op.
	if !reconcileInvoked || !reapInvoked || !startInvoked {
		t.Fatalf("drift must re-fan-out the install (reconcile=%v reap=%v start=%v); a no-op is the finding #2 bug",
			reconcileInvoked, reapInvoked, startInvoked)
	}
	if strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("a drifted registry must NOT report the no-op; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "re-fanning out") {
		t.Errorf("output should explain the drift re-fanout; got %q", buf.String())
	}
	// The committed intent now carries a daemon row for BOTH workspaces (the drifted
	// one landed).
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read re-fanned intent: %v", err)
	}
	want := map[string]bool{
		api.SerenaTaskNameForWorkspace(wsCovered): false,
		api.SerenaTaskNameForWorkspace(wsDrifted): false,
	}
	for _, d := range intent.Daemons {
		if d.Server == "serena" {
			if _, ok := want[d.TaskName]; ok {
				want[d.TaskName] = true
			}
		}
	}
	if !want[api.SerenaTaskNameForWorkspace(wsDrifted)] {
		t.Errorf("the drifted workspace %q must land in the re-fanned intent; daemons=%+v", wsDrifted, intent.Daemons)
	}
	if !want[api.SerenaTaskNameForWorkspace(wsCovered)] {
		t.Errorf("the already-covered workspace %q must remain in the re-fanned intent; daemons=%+v", wsCovered, intent.Daemons)
	}
}

// TestMigrateSerena_AlreadyMigrated_FullyCovered_HealthySupervisor_NoOp is the
// finding #2 negative guard: when the committed intent already covers EVERY
// current serena registry workspace and the supervisor is healthy, the migrate is
// a GENUINE no-op (no install/reconcile/reap/start, no bouncing the supervisor) —
// the drift check must not over-fire.
func TestMigrateSerena_AlreadyMigrated_FullyCovered_HealthySupervisor_NoOp(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	// The intent covers the only registry serena workspace → no drift.
	seedDynamicPoolIntentForWorkspaces(t, stateDir, ws)
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read seeded intent: %v", err)
	}

	installInvoked, reconcileInvoked, reapInvoked, startInvoked := false, false, false, false
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		installInvoked = true
		return "", nil
	})()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		return &api.MigrateReport{}, nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()
	defer stubSupervisorHealthy(t, func() (bool, error) { return true, nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("fully-covered + healthy must be a no-op; got error: %v (out=%s)", err, buf.String())
	}
	if installInvoked || reconcileInvoked || reapInvoked || startInvoked {
		t.Errorf("fully-covered no-op must not install/reconcile/reap/start (install=%v reconcile=%v reap=%v start=%v)",
			installInvoked, reconcileInvoked, reapInvoked, startInvoked)
	}
	if !strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("output should report the genuine no-op; got %q", buf.String())
	}
	if strings.Contains(buf.String(), "re-fanning out") {
		t.Errorf("a fully-covered intent must NOT trigger the drift re-fanout; got %q", buf.String())
	}
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("re-read intent: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("a no-op must not rewrite the committed intent")
	}
}

// TestMigrateSerena_AlreadyMigrated_StaleRegistryRow_NoDrift_NoOp guards the drift
// check's stale-path filter: a registry serena row whose workspace path no longer
// exists on disk is one the install would DROP before the intent write, so its
// absence from the intent is an INTENTIONAL skip, NOT drift. Counting it as drift
// would force an endless re-fanout loop (install drops it; drift re-detects it).
// With a healthy supervisor this must stay a genuine no-op.
func TestMigrateSerena_AlreadyMigrated_StaleRegistryRow_NoDrift_NoOp(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// A live workspace (covered by the intent) + a STALE one (path removed) that the
	// intent does NOT cover.
	wsLive := t.TempDir()
	wsStaleDir := t.TempDir()
	seedSerenaWorkspace(t, wsLive)
	seedSerenaWorkspace(t, wsStaleDir)
	// Remove the stale workspace's dir so its path no longer exists on disk.
	if err := os.RemoveAll(wsStaleDir); err != nil {
		t.Fatalf("remove stale workspace dir: %v", err)
	}
	// Intent covers only the live workspace; the stale one is absent (as the install
	// itself would leave it).
	seedDynamicPoolIntentForWorkspaces(t, stateDir, wsLive)

	installInvoked, reapInvoked, startInvoked := false, false, false
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		installInvoked = true
		return "", nil
	})()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()
	defer stubSupervisorHealthy(t, func() (bool, error) { return true, nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("a stale-only uncovered row must NOT count as drift; got error: %v (out=%s)", err, buf.String())
	}
	if installInvoked || reapInvoked || startInvoked {
		t.Errorf("a stale uncovered registry row must not trigger a re-fanout (install=%v reap=%v start=%v)",
			installInvoked, reapInvoked, startInvoked)
	}
	if !strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("output should report the genuine no-op (stale row is not drift); got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// 15. (finding #3 — bot PR #250) Non-Windows-style (start unsupported) cutover
//     with registered workspaces and no supervisor must FAIL LOUD BEFORE the
//     intent write / client rewrite — refusing to commit an intent the platform
//     cannot bring live. Asserts no intent committed and no client rewrite.
// ---------------------------------------------------------------------------

func TestMigrateSerena_StartUnsupported_Workspaces_FailsBeforeCommit(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	// Registered serena workspace → the cutover WILL require a start.
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	// No supervisor running (the operator stopped it / fresh host) — round-4 makes
	// willReap false here, but willStart is true so a start IS required.
	defer stubSupervisorRunning(t, func() (bool, error) { return false, nil })()
	// Model an UNSUPPORTED start platform (non-Windows) regardless of the test host.
	defer stubStartSupported(t, func() bool { return false })()

	reconcileInvoked, reapInvoked, startInvoked, installInvoked := false, false, false, false
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		return &api.MigrateReport{}, nil
	})()
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		installInvoked = true
		return "", nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()

	var buf bytes.Buffer
	err := runMigrateSerenaDynamicPool(context.Background(), &buf)
	if err == nil {
		t.Fatal("an unsupported-start cutover with workspaces must FAIL LOUD (refuse to commit an intent the platform cannot bring live)")
	}
	if !strings.Contains(err.Error(), "supervisor start primitive is not wired on this platform") {
		t.Errorf("error should name the unsupported-start preflight; got %v", err)
	}
	if !strings.Contains(err.Error(), "NO intent was written") {
		t.Errorf("error should state no intent was written; got %v", err)
	}
	// THE finding #3 guard: the preflight fires BEFORE the reconcile (client
	// rewrite), reap, install, and start — none must run.
	if reconcileInvoked {
		t.Errorf("the preflight must fail BEFORE the client reconcile (no client rewrite); reconcile ran")
	}
	if installInvoked {
		t.Errorf("the preflight must fail BEFORE the intent write; install ran")
	}
	if reapInvoked {
		t.Errorf("the preflight must fail BEFORE the reap; reap ran")
	}
	if startInvoked {
		t.Errorf("the preflight must fail before the (unsupported) start; start ran")
	}
	// No intent committed on disk.
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("the unsupported-start preflight must leave NO intent on disk; stat err = %v", statErr)
	}
}

// TestMigrateSerena_StartUnsupported_EmptyRegistry_Proceeds proves the preflight
// is scoped to cutovers that REQUIRE a start: a zero-workspace install needs no
// supervisor (willStart false), so even on an unsupported-start platform it
// proceeds and writes the zero-row intent (no reap/start required).
func TestMigrateSerena_StartUnsupported_EmptyRegistry_Proceeds(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	// No serena workspaces → willStart false → no preflight trip.
	defer stubStartSupported(t, func() bool { return false })()

	reapInvoked, startInvoked := false, false
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("a zero-workspace install must proceed even on an unsupported-start platform (no start required); got error: %v (out=%s)", err, buf.String())
	}
	// The zero-row intent was written; no reap/start fired.
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("zero-workspace install must write the intent; read err = %v", err)
	}
	if intent.HasRuntimeSpecRow() {
		t.Errorf("zero-workspace install must have no runtime_spec rows")
	}
	if reapInvoked || startInvoked {
		t.Errorf("a zero-workspace install needs no reap/start (reap=%v start=%v)", reapInvoked, startInvoked)
	}
}

// TestMigrateSerena_StartUnsupported_EmptyFirstSnapshot_WindowRegister_FailsBeforeCommit
// is the finding #1 × finding #3 INTERACTION guard. When the first snapshot is
// EMPTY, Preflight A (pre-reconcile) passes (willStart false). A workspace then
// registers during the released-lock window, the finding-#1 re-read recompute
// flips willStart true — and on an unsupported-start platform the second preflight
// (post-re-read, pre-install) must STILL fail loud BEFORE the intent commit, so the
// platform never commits an intent it cannot bring live. The deferred outer
// rollback restores the already-rewritten clients (reconcile-restore fires).
func TestMigrateSerena_StartUnsupported_EmptyFirstSnapshot_WindowRegister_FailsBeforeCommit(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	// FIRST snapshot EMPTY → Preflight A passes (willStart false, no reap probe).
	// Model an UNSUPPORTED start platform.
	defer stubStartSupported(t, func() bool { return false })()

	wsConcurrent := t.TempDir()
	installInvoked, reapInvoked, startInvoked := false, false, false
	restoreReconcileCalled := false
	// The reconcile runs (Preflight A passed) and is the window-injection point for
	// the concurrent register (the reap is skipped for an empty first snapshot).
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		seedSerenaWorkspaceNoPort(t, wsConcurrent)
		return &api.MigrateReport{Applied: []api.AppliedMigration{{Server: "serena", Client: "claude-code", URL: "http://127.0.0.1:9137/serena/mcp", BackupPath: "/fake/bak"}}}, nil
	})()
	defer stubRestoreReconcile(t, func(report *api.MigrateReport) error { restoreReconcileCalled = true; return nil })()
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		installInvoked = true
		return "", nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { reapInvoked = true; return nil })()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()

	var buf bytes.Buffer
	err := runMigrateSerenaDynamicPool(context.Background(), &buf)
	if err == nil {
		t.Fatal("an unsupported-start platform must fail loud when the re-read flips willStart true, even with an empty first snapshot")
	}
	if !strings.Contains(err.Error(), "supervisor start primitive is not wired on this platform") {
		t.Errorf("error should name the unsupported-start preflight; got %v", err)
	}
	// THE interaction guard: the second preflight fires BEFORE the install commit
	// (and before the unsupported start) — neither runs.
	if installInvoked {
		t.Errorf("the post-re-read preflight must fail BEFORE the intent write; install ran")
	}
	if startInvoked {
		t.Errorf("the unsupported start must never run; start ran")
	}
	// Empty first snapshot → no reap fired (probe never ran).
	if reapInvoked {
		t.Errorf("an empty-first-snapshot cutover never reaps; reap ran")
	}
	// No intent committed on disk.
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("the preflight must leave NO intent on disk; stat err = %v", statErr)
	}
	// The deferred outer rollback restored the already-rewritten clients (the
	// reconcile ran before the preflight tripped).
	if !restoreReconcileCalled {
		t.Errorf("the reconcile-restore must fire so the rewritten clients return to legacy")
	}
}

// ---------------------------------------------------------------------------
// Source-state detection unit coverage (the §D.3 table directly).
// ---------------------------------------------------------------------------

func TestDetectSerenaSourceState(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want serenaSourceState
		err  bool
	}{
		{"legacy 2-daemon", legacy2DaemonManifestYAML, serenaSourceLegacy2Daemon, false},
		{"already migrated", alreadyMigratedManifestYAML, serenaSourceAlreadyMigrated, false},
		{"malformed 3-daemon", malformedManifestYAML, serenaSourceMalformed, true},
		{"unified intermediate", `name: serena
kind: global
transport: native-http
command: go
daemons:
  - name: unified
    context: codex
    port: 9121
weekly_refresh: false
`, serenaSourceUnifiedIntermediate, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := config.ParseManifest(strings.NewReader(tc.yaml))
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			got, derr := detectSerenaSourceState(m)
			if tc.err && derr == nil {
				t.Fatalf("want detection error, got nil (state=%v)", got)
			}
			if !tc.err && derr != nil {
				t.Fatalf("unexpected detection error: %v", derr)
			}
			if got != tc.want {
				t.Errorf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 15. (finding #1 — bot PR #250, round-6) LATE REAP on the empty-first-snapshot +
//     window-register path. When the FIRST snapshot is EMPTY, hasWorkspaces is
//     false so the step-5 willReap probe is SKIPPED — even if a supervisor is
//     genuinely running. A workspace then registers during the released-lock
//     window, the 7c recompute flips willStart true, and the re-read install
//     snapshot becomes spec-bearing. InstallParsedManifest's §7.1 gate REFUSES a
//     spec-bearing write while a supervisor is running, so WITHOUT a late reap the
//     cutover would FAIL at that gate even on Windows. The fix re-probes liveness
//     after the recompute and reaps EXACTLY ONCE before the install, so the gate
//     then passes naturally.
//
// This is modeled exactly like TestMigrateSerena_LiveSupervisor_ReapClearsTheGate-
// BeforeWrite (a REAL install + a held real supervisor.lock so the §7.1 gate would
// refuse) but with an EMPTY first snapshot, so the reap that clears the gate is the
// LATE one (step 7d), not the step-7 cutover reap.
// ---------------------------------------------------------------------------

func TestMigrateSerena_EmptyFirstSnapshot_SupervisorRunning_LateReapClearsGate(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// FIRST snapshot is EMPTY: no serena workspace registered up front → the driver
	// computes hasWorkspaces=false, so it never probes supervisor liveness at step 5
	// and willReap is false — even though a supervisor IS running (the held lock
	// below).

	// Hold the REAL supervisor.lock so SupervisorRunningUnderStateDir reports a LIVE
	// supervisor → the REAL InstallParsedManifest §7.1 gate would REFUSE a
	// spec-bearing write while it is held. The same probe backs the default
	// migrateSerenaSupervisorRunningFn the late reap re-reads.
	lock, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock (model live supervisor): %v", err)
	}
	lockReleased := false
	defer func() {
		if !lockReleased {
			lock.Release()
		}
	}()
	// Sanity: with the lock held, the gate sees a running supervisor.
	if running, _, perr := api.SupervisorRunningUnderStateDir(stateDir); perr != nil || !running {
		t.Fatalf("precondition: supervisor must read as running while the lock is held (running=%v err=%v)", running, perr)
	}

	// Start-support TRUE so the post-re-read recompute + (late) reap + start path
	// runs on non-Windows CI too (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	// The workspace that registers DURING the released-lock window. The first
	// snapshot is empty and willReap is false, so the reconcile stub is the
	// window-injection point (it runs after the first snapshot, before the re-read).
	// Rooted at an existing dir + port-less so the re-read re-allocation assigns one
	// and the install fan-out does not prune it.
	wsConcurrent := t.TempDir()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		seedSerenaWorkspaceNoPort(t, wsConcurrent)
		return &api.MigrateReport{}, nil
	})()

	// The LATE reap stub: it must fire EXACTLY ONCE (no step-7 reap ran — willReap
	// was false), and it RELEASES the supervisor.lock to model the real reap killing
	// the supervisor, so the subsequent REAL install's §7.1 gate passes. It also
	// asserts reap-first ordering: the spec-bearing intent is not on disk yet.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	reapCount := 0
	intentAbsentAtReap := false
	defer stubReap(t, func(ctx context.Context, w io.Writer) error {
		reapCount++
		if _, statErr := os.Stat(intentPath); os.IsNotExist(statErr) {
			intentAbsentAtReap = true
		}
		// Model the real reap exiting the supervisor: release the lock so the §7.1
		// gate in the REAL install below no longer sees a running supervisor.
		lock.Release()
		lockReleased = true
		return nil
	})()
	// REAL install (no stub) so the spec-bearing write actually exercises the §7.1
	// gate — proving the late reap cleared it.
	startInvoked := false
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { startInvoked = true; return nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("the late-reap cutover must succeed (the §7.1 gate must pass after the late reap); got error: %v (out=%s)", err, buf.String())
	}

	// THE finding #1 (round-6) guards.
	if reapCount != 1 {
		t.Fatalf("the late reap must fire EXACTLY ONCE on the empty-first-snapshot + supervisor-running + window-register path; reapCount=%d", reapCount)
	}
	if !intentAbsentAtReap {
		t.Error("reap-first: the spec-bearing intent must NOT be on disk when the late reap fires")
	}
	// The spec-bearing intent committed (no §7.1-gate failure) and carries the
	// window-registered workspace's row.
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("the spec-bearing intent must be committed: %v", err)
	}
	if !intent.HasRuntimeSpecRow() {
		t.Fatalf("the window-registered workspace must produce a spec-bearing intent; got %+v", intent.Daemons)
	}
	foundConcurrent := false
	for _, d := range intent.Daemons {
		if d.Server == "serena" && d.TaskName == api.SerenaTaskNameForWorkspace(wsConcurrent) {
			foundConcurrent = true
		}
	}
	if !foundConcurrent {
		t.Errorf("the window-registered workspace %q must have a daemon row in the committed intent; daemons=%+v", wsConcurrent, intent.Daemons)
	}
	// The start fired (willStart was recomputed true).
	if !startInvoked {
		t.Error("the supervisor start must fire after the late-reap cutover commits the spec-bearing intent")
	}
}

// ---------------------------------------------------------------------------
// 16. (finding #2 — bot PR #250, round-6) On a PRE-COMMIT abort AFTER the re-read,
//     the port reReadAndAllocateSerenaForInstall assigned to a concurrently-added
//     serena row is UNDONE (reverted to its pre-re-read port 0), so the
//     registry/router is never left pointing that workspace at a dead, un-spawned
//     port. The snapshotted row (round-4 #1 surgical rollback owns it) restores
//     normally and independently.
//
// The install is stubbed to fail — standing in for any pre-commit abort landing
// after the re-read (the §7.1 install gate, a scheduler error, the second
// start-support preflight): all run the same deferred outer rollback stack, which
// now carries the finding-#2 re-read-allocation undo in addition to the snapshot
// restore.
// ---------------------------------------------------------------------------

func TestMigrateSerena_PreCommitAbort_UndoesReReadAllocation_OnConcurrentRow(t *testing.T) {
	_, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// A snapshotted serena workspace registered BEFORE the migrate, WITHOUT a port,
	// so the FIRST allocation assigns one (the snapshot rollback must revert it to
	// 0). hasWorkspaces is true → willReap is armed (a supervisor is running).
	wsBefore := t.TempDir()
	seedSerenaWorkspaceNoPort(t, wsBefore)
	// Model a running supervisor so the step-7 cutover reaps it; the reap stub is
	// the window-injection point for the concurrent register.
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// Start-support TRUE so the run reaches the (failing) install on non-Windows CI
	// too (default false off Windows — finding #3).
	defer stubStartSupported(t, func() bool { return true })()

	// The concurrently-added serena row that lands DURING the released-lock window
	// (registered port-less so the re-read re-allocation assigns it a NEW port — the
	// allocation the finding-#2 undo must clear on abort).
	wsConcurrent := t.TempDir()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	defer stubReap(t, func(ctx context.Context, w io.Writer) error {
		seedSerenaWorkspaceNoPort(t, wsConcurrent)
		return nil
	})()
	// PRE-COMMIT abort: the install fails AFTER the re-read (and the late/step-7
	// reap), so no intent commits and the deferred outer stack runs both the
	// snapshot restore AND the re-read-allocation undo.
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		return "", errors.New("synthetic pre-commit install failure (stands in for the §7.1 gate)")
	})()
	// A supervisor was reaped, so the install-failure path drives a recovery start.
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { return nil })()

	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}

	var buf bytes.Buffer
	if rerr := runMigrateSerenaDynamicPool(context.Background(), &buf); rerr == nil {
		t.Fatalf("the pre-commit install failure must propagate as a migrate error (out=%s)", buf.String())
	} else if !strings.Contains(rerr.Error(), "synthetic pre-commit install failure") {
		t.Errorf("error should carry the install failure; got %v", rerr)
	}

	ports := loadRegistrySerenaPorts(t, regPath)
	// THE finding #2 guard: the concurrently-added row's re-read-allocated port is
	// CLEARED on abort (reverted to its pre-re-read port 0) — NOT left dangling at
	// the allocated value. Before the fix the surgical snapshot rollback left this
	// non-snapshotted row untouched, so its re-read port survived → the router
	// pointed the workspace at a port no supervisor would ever spawn.
	if got, present := ports[api.WorkspaceKey(wsConcurrent)]; !present {
		t.Errorf("the concurrently-added serena row must still EXIST after abort (the undo reverts its port, it does not delete the row); rows=%+v", ports)
	} else if got != 0 {
		t.Errorf("finding #2: the concurrently-added row's re-read-allocated port must be CLEARED on a pre-commit abort; got %d, want 0 (rows=%+v)", got, ports)
	}
	// The snapshotted row restores normally and independently (seeded port-less →
	// the snapshot captured port 0 → the surgical restore reverts the first
	// allocation back to 0).
	if got := ports[api.WorkspaceKey(wsBefore)]; got != 0 {
		t.Errorf("the snapshotted row must restore to its pre-migrate port 0; got %d (rows=%+v)", got, ports)
	}
}

// ---------------------------------------------------------------------------
// Phase 2 — supervisor.lock interlock (.plans/2026-06/plan-serena-lock-interlock).
// ---------------------------------------------------------------------------

// readInterlockEventsLog returns the raw JSONL bytes of the supervisor-events.log
// under stateDir (empty string if absent). Phase-2 tests assert event presence by
// substring — the JSONL `"event":"<name>"` token is stable across schema fields.
// (Named distinctly from supervise_accept_loop_test.go's path-based
// readSupervisorEventsLog to avoid a same-package redeclaration.)
func readInterlockEventsLog(t *testing.T, stateDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read supervisor-events.log: %v", err)
	}
	return string(raw)
}

// TestMigrateSerena_Interlock_BlocksConcurrentSupervisorStartInWindow proves the
// interlock's core property: while the migrate HOLDS supervisor.lock across its
// reap→write→start critical section, a concurrent direct
// api.AcquireSupervisorLock on the gate's exact path (Revision 5 — NOT a child)
// FAILS, so no foreign supervisor can start in the window — yet the migrate's OWN
// spec-bearing write still commits (the typed bypass token passes the §7.1 gate
// because the held lock is the migrate's own handle).
func TestMigrateSerena_Interlock_BlocksConcurrentSupervisorStartInWindow(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	defer stubStartSupported(t, func() bool { return true })()
	// No supervisor running → no reap; the interlock is still acquired (gated on
	// installWorkspaces>0, not willReap) so the window exists around the write.
	defer stubSupervisorRunning(t, func() (bool, error) { return false, nil })()
	// The migrate's interlock is the REAL lock on the gate path (cross-platform stub).
	defer stubAcquireInterlock(t, realInterlockAcquire)()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()

	// REAL install (no stub) so the §7.1 gate actually runs; INSIDE the install
	// seam — the migrate is provably holding the interlock here (acquired at step
	// 7e, released only just before the start) — a concurrent direct acquire on the
	// gate's exact path MUST fail. Then delegate to the real install (forwarding
	// opts, which carries the bypass token) so the spec-bearing write commits.
	concurrentAcquireBlocked := false
	concurrentProbed := false
	restoreInstall := stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		concurrentProbed = true
		if lk, acqErr := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock")); acqErr != nil {
			concurrentAcquireBlocked = true
		} else {
			lk.Release() // should not happen; release so we don't wedge other tests
		}
		return a.InstallParsedManifest(ctx, m, opts)
	})
	defer restoreInstall()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { return nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("migrate must succeed (the bypass token clears the §7.1 gate under the caller's own lock); got error: %v (out=%s)", err, buf.String())
	}
	if !concurrentProbed {
		t.Fatal("the install seam must have run (the window-probe point)")
	}
	if !concurrentAcquireBlocked {
		t.Fatal("a concurrent api.AcquireSupervisorLock during the migrate's held-interlock window MUST fail (the interlock blocks every foreign supervisor start)")
	}
	// The spec-bearing intent committed — proving the bypass token let the migrate's
	// OWN write through the §7.1 gate while it held the lock.
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read intent after interlocked migrate: %v", err)
	}
	if !intent.HasRuntimeSpecRow() {
		t.Fatal("the spec-bearing intent must commit under the caller-held interlock (bypass token verified)")
	}
	// The audit log records the verified bypass.
	if log := readInterlockEventsLog(t, stateDir); !strings.Contains(log, "spec-bearing-write-allowed-under-caller-lock") {
		t.Errorf("expected the verified-bypass audit event; log=%s", log)
	}
}

// TestMigrateSerena_Interlock_ReleasedBeforeStart proves the hand-off: the
// interlock is RELEASED immediately before the successor start (so the started
// supervisor can AcquireSupervisorLock itself). The start stub observes the lock
// is ACQUIRABLE at start time.
func TestMigrateSerena_Interlock_ReleasedBeforeStart(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	defer stubStartSupported(t, func() bool { return true })()
	defer stubSupervisorRunning(t, func() (bool, error) { return false, nil })()
	defer stubAcquireInterlock(t, realInterlockAcquire)()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	// Stub install so the run reaches the start without a real write (the interlock
	// lifetime is the unit under test, not the §7.1 gate).
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		// Write a minimal spec-bearing intent so step-10's verify reads runtime_spec
		// and the NORMAL start path (not the scanErr recovery) fires.
		intentPath := filepath.Join(stateDir, "supervisor-intent.json")
		mi := &api.SupervisorIntentFile{
			Version: 1,
			Daemons: []api.SupervisorDaemon{{
				TaskName: api.SerenaTaskNameForWorkspace(ws),
				Server:   "serena",
				Port:     9150,
				RuntimeSpec: &api.DaemonRuntimeSpec{
					SpecVersion: api.DaemonRuntimeSpecVersion, ChildCommand: "go",
					UpstreamPort: 9151, ExternalPort: 9150, WorkspacePath: ws,
				},
			}},
		}
		if werr := api.WriteStateFileAtomic(intentPath, mi); werr != nil {
			return "", werr
		}
		return intentPath, nil
	})()

	lockAcquirableAtStart := false
	startObserved := false
	defer stubStart(t, func(ctx context.Context, w io.Writer) error {
		startObserved = true
		if lk, acqErr := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock")); acqErr == nil {
			lockAcquirableAtStart = true
			lk.Release()
		}
		return nil
	})()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("migrate must succeed; got error: %v (out=%s)", err, buf.String())
	}
	if !startObserved {
		t.Fatal("the start stub must have run")
	}
	if !lockAcquirableAtStart {
		t.Fatal("the interlock MUST be released before the start (the started supervisor must be able to AcquireSupervisorLock itself)")
	}
}

// TestMigrateSerena_Interlock_ReleasedOnRecoveryStartBranch is the Revision 3
// regression guard. The early recovery-start (alreadyMigrated && !drift &&
// !healthy) fires BEFORE the post-step-7 interlock acquire, so it must NOT release
// a never-acquired lock (the armed-on-acquire closure is a no-op there). The
// assertion is leak-free: a SECOND AcquireSupervisorLock AFTER the function
// returns SUCCEEDS.
func TestMigrateSerena_Interlock_ReleasedOnRecoveryStartBranch(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// Seed a committed dynamic-pool intent (runtime already migrated), no drift.
	ws := t.TempDir()
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	migratedIntent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: api.SerenaTaskNameForWorkspace(ws),
			Server:   "serena",
			Port:     9150,
			RuntimeSpec: &api.DaemonRuntimeSpec{
				SpecVersion: api.DaemonRuntimeSpecVersion, ChildCommand: "go",
				UpstreamPort: 9151, ExternalPort: 9150, WorkspacePath: ws,
			},
		}},
	}
	if err := api.WriteStateFileAtomic(intentPath, migratedIntent); err != nil {
		t.Fatalf("seed already-migrated runtime intent: %v", err)
	}
	// No-drift requires the registry serena row to match the intent's task.
	seedSerenaWorkspaceWithPort(t, ws, 9150)

	// !healthy → recovery start at the EARLY branch (before any interlock acquire).
	defer stubSupervisorHealthy(t, func() (bool, error) { return false, nil })()
	// If the interlock seam is ever called on this path, fail loud — the early
	// recovery start must NOT acquire it.
	defer stubAcquireInterlock(t, func() (*api.SupervisorLock, func(), error) {
		t.Fatalf("the interlock must NOT be acquired on the early recovery-start branch (acquire is post-step-7)")
		return nil, func() {}, nil
	})()
	// The early recovery start must see NO interlock held (acquirable).
	lockAcquirableInEarlyStart := false
	defer stubStart(t, func(ctx context.Context, w io.Writer) error {
		if lk, acqErr := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock")); acqErr == nil {
			lockAcquirableInEarlyStart = true
			lk.Release()
		}
		return nil
	})()
	// Reap/reconcile/install must NOT run on the recovery branch.
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { t.Fatalf("recovery branch must not reap"); return nil })()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		t.Fatalf("recovery branch must not reconcile")
		return nil, nil
	})()
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		t.Fatalf("recovery branch must not install")
		return "", nil
	})()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("recovery-start branch must succeed; got error: %v (out=%s)", err, buf.String())
	}
	if !lockAcquirableInEarlyStart {
		t.Fatal("the early recovery start must run with NO interlock held (it precedes the acquire)")
	}
	// THE leak guard: a SECOND acquire after the function returns must SUCCEED — a
	// leaked interlock would block it (and deadlock the next migrate).
	lk, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("interlock leaked: a post-return AcquireSupervisorLock must succeed, got %v", err)
	}
	lk.Release()
}

// TestMigrateSerena_Interlock_AcquireFailsLoud_WhenForeignHolderWonTheWindow:
// when a foreign holder already owns supervisor.lock at the post-reap acquire
// point, the migrate FAILS LOUD (do not block) and writes NO spec-bearing intent.
func TestMigrateSerena_Interlock_AcquireFailsLoud_WhenForeignHolderWonTheWindow(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	defer stubStartSupported(t, func() bool { return true })()
	// No supervisor "running" by the probe → no reap; the acquire at step 7e is the
	// failure point (the real binding contends the foreign-held lock).
	defer stubSupervisorRunning(t, func() (bool, error) { return false, nil })()
	defer stubAcquireInterlock(t, realInterlockAcquire)()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	// A FOREIGN holder owns the lock for the whole run.
	foreign, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("seed foreign holder: %v", err)
	}
	defer foreign.Release()

	installRan := false
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		installRan = true
		return "", nil
	})()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { return nil })()

	var buf bytes.Buffer
	err = runMigrateSerenaDynamicPool(context.Background(), &buf)
	if err == nil {
		t.Fatalf("the migrate MUST fail loud when a foreign holder won the interlock window (out=%s)", buf.String())
	}
	if !strings.Contains(err.Error(), "acquire the supervisor.lock interlock") {
		t.Errorf("error must name the interlock acquire failure; got %v", err)
	}
	if installRan {
		t.Error("the spec-bearing install must NOT run when the interlock acquire failed (pre-write)")
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Error("no spec-bearing intent must be written on the acquire-fail path (legacy untouched)")
	}
}

// TestMigrateSerena_Interlock_AcquiredImmediatelyAfterReap_ClosesPostReapGap is the
// bot PR #276 finding-2 regression guard. The interlock was historically acquired
// at step 7e — AFTER the step-7 reap, the registry re-read, the start-supported
// re-check, and the late-reap decision — leaving an UNLOCKED post-reap gap in which
// a foreign supervisor could take supervisor.lock. The fix acquires the interlock
// IMMEDIATELY after the reap (acquireInterlockOnce), so any actor that tries to
// take the lock in the post-reap work window now FAILS.
//
// Deterministic injection: a supervisor IS running at the probe (willReap=true), so
// the step-7 reap fires. The start-supported seam (migrateSerenaStartSupportedFn)
// runs in the post-reap gap — AFTER the step-7 reap, in the OLD code BEFORE the
// step-7e acquire. Inside it we attempt a concurrent api.AcquireSupervisorLock on
// the gate's exact path:
//   - OLD code (acquire at step 7e): the migrate had NOT yet acquired here → the
//     concurrent acquire would SUCCEED → the gap is open.
//   - FIXED code (acquire immediately after the reap): the migrate ALREADY holds
//     the lock here → the concurrent acquire FAILS → the gap is closed.
// The test asserts the concurrent acquire FAILS, so a regression that moves the
// acquire back past the post-reap work breaks it.
func TestMigrateSerena_Interlock_AcquiredImmediatelyAfterReap_ClosesPostReapGap(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	// A supervisor IS running → step-7 reap fires; the interlock must be taken right
	// after it (the fix), so the post-reap-gap probe below cannot acquire.
	defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
	// The step-7 reap is stubbed to succeed (no real supervisor to kill).
	defer stubReap(t, func(ctx context.Context, w io.Writer) error { return nil })()
	defer stubAcquireInterlock(t, realInterlockAcquire)()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()

	// The post-reap-gap probe: this seam fires AFTER the step-7 reap and (in the OLD
	// code) BEFORE the step-7e acquire. With the fix the migrate already holds the
	// lock here, so a concurrent acquire MUST fail.
	gapProbed := false
	concurrentAcquireBlockedInGap := false
	defer stubStartSupported(t, func() bool {
		gapProbed = true
		if lk, acqErr := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock")); acqErr != nil {
			concurrentAcquireBlockedInGap = true
		} else {
			lk.Release() // gap was open (regression); release so other tests are not wedged
		}
		return true
	})()

	// Stub install + start so the run completes (the §7.1 gate is not the unit under
	// test here; the post-reap-gap exclusion is).
	defer stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		intentPath := filepath.Join(stateDir, "supervisor-intent.json")
		mi := &api.SupervisorIntentFile{
			Version: 1,
			Daemons: []api.SupervisorDaemon{{
				TaskName: api.SerenaTaskNameForWorkspace(ws),
				Server:   "serena",
				Port:     9150,
				RuntimeSpec: &api.DaemonRuntimeSpec{
					SpecVersion: api.DaemonRuntimeSpecVersion, ChildCommand: "go",
					UpstreamPort: 9151, ExternalPort: 9150, WorkspacePath: ws,
				},
			}},
		}
		if werr := api.WriteStateFileAtomic(intentPath, mi); werr != nil {
			return "", werr
		}
		return intentPath, nil
	})()
	defer stubStart(t, func(ctx context.Context, w io.Writer) error { return nil })()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("migrate must succeed; got error: %v (out=%s)", err, buf.String())
	}
	if !gapProbed {
		t.Fatal("the post-reap-gap probe (start-supported seam) must have run")
	}
	if !concurrentAcquireBlockedInGap {
		t.Fatal("finding 2 REGRESSION: a concurrent AcquireSupervisorLock SUCCEEDED in the post-reap work window — the interlock was acquired too late (the post-reap gap is open). The fix must acquire the interlock IMMEDIATELY after the reap")
	}
}

// TestMigrateSerena_HandoffWindowEvent_EmittedOnReconcileRetry is the Revision 4
// observability guard: when the START primitive reports the benign
// release→child-acquire hand-off window materialized (the start stub calls
// migrateSerenaHandoffWindowFn), the driver emits the named
// supervisor-interlock-handoff-window event to supervisor-events.log.
func TestMigrateSerena_HandoffWindowEvent_EmittedOnReconcileRetry(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)
	defer stubStartSupported(t, func() bool { return true })()
	defer stubSupervisorRunning(t, func() (bool, error) { return false, nil })()
	defer stubAcquireInterlock(t, realInterlockAcquire)()
	defer stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})()
	// REAL install (the bypass token clears the gate under our own lock) so the
	// driver reaches the post-commit start with the hand-off observer wired.
	// The start stub simulates the Windows primitive observing a >1-retry reconcile.
	defer stubStart(t, func(ctx context.Context, w io.Writer) error {
		migrateSerenaHandoffWindowFn("reconcile-ready-retry")
		return nil
	})()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("migrate must succeed; got error: %v (out=%s)", err, buf.String())
	}
	log := readInterlockEventsLog(t, stateDir)
	if !strings.Contains(log, "supervisor-interlock-handoff-window") {
		t.Fatalf("expected the Revision-4 hand-off-window event in supervisor-events.log; log=%s", log)
	}
	if !strings.Contains(log, "reconcile-ready-retry") {
		t.Errorf("the hand-off event must carry the phase; log=%s", log)
	}
}

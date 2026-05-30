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

// stubRestart overrides migrateSerenaRestartFn for the test scope.
func stubRestart(t *testing.T, fn func(ctx context.Context, w io.Writer) error) func() {
	t.Helper()
	orig := migrateSerenaRestartFn
	migrateSerenaRestartFn = fn
	return func() { migrateSerenaRestartFn = orig }
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

func TestMigrateSerena_AlreadyMigrated_Idempotent(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, alreadyMigratedManifestYAML)

	// Guard: install + reconcile + restart must NOT be invoked on the
	// already-migrated no-op path.
	installInvoked := false
	reconcileInvoked := false
	restartInvoked := false
	restoreInstall := stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		installInvoked = true
		return "", nil
	})
	defer restoreInstall()
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()
	restoreRestart := stubRestart(t, func(ctx context.Context, w io.Writer) error {
		restartInvoked = true
		return nil
	})
	defer restoreRestart()

	var buf bytes.Buffer
	if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
		t.Fatalf("already-migrated must be a no-op exit-0; got error: %v", err)
	}
	if installInvoked || reconcileInvoked || restartInvoked {
		t.Errorf("already-migrated path must not install/reconcile/restart (install=%v reconcile=%v restart=%v)", installInvoked, reconcileInvoked, restartInvoked)
	}
	if !strings.Contains(buf.String(), "already migrated") {
		t.Errorf("output should explain the no-op; got %q", buf.String())
	}
	// No supervisor-intent.json written.
	if _, err := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(err) {
		t.Errorf("already-migrated must write nothing; stat supervisor-intent.json err = %v", err)
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
	restartInvoked := false
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		reconcileInvoked = true
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()
	restoreRestart := stubRestart(t, func(ctx context.Context, w io.Writer) error {
		restartInvoked = true
		return nil
	})
	defer restoreRestart()

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
	// reconcile runs (clients still get pointed at the router); restart does
	// NOT (no spec-bearing write → no old-supervisor split-brain risk).
	if !reconcileInvoked {
		t.Errorf("client-reconcile should run even on a zero-workspace migrate")
	}
	if restartInvoked {
		t.Errorf("no runtime_spec rows → §7.1 restart must be skipped")
	}
	if !strings.Contains(buf.String(), "zero daemon rows") {
		t.Errorf("output should explain the zero-workspace install; got %q", buf.String())
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

	restartInvoked := false
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()
	restoreRestart := stubRestart(t, func(ctx context.Context, w io.Writer) error {
		restartInvoked = true
		return nil
	})
	defer restoreRestart()

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
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if !intent.HasRuntimeSpecRow() {
		t.Fatalf("install should have materialized a runtime_spec row for the registered workspace")
	}
	if !restartInvoked {
		t.Errorf("a spec-bearing migrate must drive the §7.1 restart")
	}
}

// ---------------------------------------------------------------------------
// 5. Driver rollback restores the registry on install failure.
//    The install seam is stubbed to fail AFTER the driver has allocated +
//    saved registry ports; the deferred outer rollback must restore the prior
//    registry rows.
// ---------------------------------------------------------------------------

func TestMigrateSerena_RollbackRestoresRegistry_OnInstallFailure(t *testing.T) {
	_, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)

	// Seed a serena workspace WITHOUT a port so the driver allocates one
	// (the registry mutation the rollback must undo).
	ws := t.TempDir()
	seedSerenaWorkspaceNoPort(t, ws)

	// Snapshot the registry BEFORE the migrate.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	beforeReg := loadRegistrySerenaPorts(t, regPath)

	// Stub install to fail (its OWN inner rollback already ran by contract;
	// the driver's outer stack must then restore the registry).
	restoreInstall := stubInstall(t, func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
		return "", errors.New("synthetic install failure (intent write)")
	})
	defer restoreInstall()
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		t.Errorf("reconcile must NOT run after an install failure")
		return nil, nil
	})
	defer restoreReconcile()
	restoreRestart := stubRestart(t, func(ctx context.Context, w io.Writer) error {
		t.Errorf("restart must NOT run after an install failure")
		return nil
	})
	defer restoreRestart()

	var buf bytes.Buffer
	err = runMigrateSerenaDynamicPool(context.Background(), &buf)
	if err == nil {
		t.Fatal("install failure must propagate as a migrate error")
	}
	if !strings.Contains(err.Error(), "synthetic install failure") {
		t.Errorf("error should carry the install failure; got %v", err)
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

// ---------------------------------------------------------------------------
// 6. Drives the supervisor restart after the intent write; AND fails loud when
//    the prior supervisor cannot be exited.
// ---------------------------------------------------------------------------

func TestMigrateSerena_DrivesSupervisorRestart(t *testing.T) {
	stateDir, manifestDir := migrateSerenaTestEnv(t)
	seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
	ws := t.TempDir()
	seedSerenaWorkspace(t, ws)

	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()

	t.Run("restart fires AFTER the intent write", func(t *testing.T) {
		intentExistedAtRestart := false
		restartCalled := false
		restoreRestart := stubRestart(t, func(ctx context.Context, w io.Writer) error {
			restartCalled = true
			// The spec-bearing intent must already be on disk by the time the
			// restart seam fires (ordering: write → reconcile → restart).
			if intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json")); err == nil && intent.HasRuntimeSpecRow() {
				intentExistedAtRestart = true
			}
			return nil
		})
		defer restoreRestart()

		var buf bytes.Buffer
		if err := runMigrateSerenaDynamicPool(context.Background(), &buf); err != nil {
			t.Fatalf("migrate must succeed: %v (out=%s)", err, buf.String())
		}
		if !restartCalled {
			t.Fatal("the §7.1 cold-restart seam must be invoked for a spec-bearing migrate")
		}
		if !intentExistedAtRestart {
			t.Errorf("the restart must fire AFTER the spec-bearing intent write (intent not present/spec-bearing at restart time)")
		}
	})

	t.Run("fail-loud when the prior supervisor cannot be exited", func(t *testing.T) {
		restoreRestart := stubRestart(t, func(ctx context.Context, w io.Writer) error {
			// Mirror RunInstallUpgrade's non-nil return when ExitGraceful
			// fails AND force-kill fails with a non-already-exited cause.
			return errors.New("force-kill supervisor failed after graceful-exit timeout: ACCESS_DENIED")
		})
		defer restoreRestart()

		var buf bytes.Buffer
		err := runMigrateSerenaDynamicPool(context.Background(), &buf)
		if err == nil {
			t.Fatal("migrate must FAIL LOUD when the supervisor restart fails")
		}
		if !strings.Contains(err.Error(), "supervisor upgrade/restart gate (§7.1) failed") {
			t.Errorf("error should name the §7.1 gate failure; got %v", err)
		}
		if !strings.Contains(err.Error(), "ACCESS_DENIED") {
			t.Errorf("error should preserve the underlying restart failure; got %v", err)
		}
	})
}

// TestMigrateSerena_DrivesSupervisorRestart_ViaRunInstallUpgradeFakeDeps proves
// the production restart binding shape: the migrate restart seam, when driven
// through RunInstallUpgrade with a fakeUpgradeDeps (the install_upgrade_test.go
// precedent), invokes the cold-restart sequence and fails loud when force-kill
// fails.
func TestMigrateSerena_DrivesSupervisorRestart_ViaRunInstallUpgradeFakeDeps(t *testing.T) {
	// Happy path: rename → quiesce → exit → start, no abort.
	happy := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{ID: 1, OK: true, Result: map[string]any{"drained": 1.0, "still_running": []any{}}, Final: true},
		exitResult:    api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}
	if err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: "/fake/mcphub", NewBinary: "/fake/mcphub.new", PipePath: "fake-pipe", Deps: happy,
	}); err != nil {
		t.Fatalf("restart happy path: %v", err)
	}
	if !happy.startCalled {
		t.Error("StartSupervisor must run on the restart happy path")
	}

	// Fail-loud: exit times out AND force-kill fails with a non-already-exited
	// cause → RunInstallUpgrade returns non-nil (the driver wraps this).
	stuck := &fakeUpgradeDeps{
		exitErr:      errors.New("timeout"),
		forceKillErr: errors.New("ACCESS_DENIED: insufficient privileges to terminate the supervisor PID"),
	}
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: "/fake/mcphub", NewBinary: "/fake/mcphub.new", PipePath: "fake-pipe", ExitTimeoutMs: 5000, Deps: stuck,
	})
	if err == nil {
		t.Fatal("a stuck supervisor (force-kill failure) must make RunInstallUpgrade return non-nil so the migrate fails loud")
	}
	if stuck.startCalled {
		t.Error("StartSupervisor must NOT run when force-kill failed unsafely")
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

	specBearingAtRestart := false
	restoreReconcile := stubReconcile(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return &api.MigrateReport{}, nil
	})
	defer restoreReconcile()
	restoreRestart := stubRestart(t, func(ctx context.Context, w io.Writer) error {
		// Assert the row is HEALED (spec materialized) BEFORE the restart
		// seam fires — i.e. before any spawn would happen.
		intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
		if err == nil {
			for _, d := range intent.Daemons {
				if d.Server == "serena" && d.RuntimeSpec != nil {
					specBearingAtRestart = true
				}
			}
		}
		return nil
	})
	defer restoreRestart()

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
	if !specBearingAtRestart {
		t.Errorf("the row must be healed BEFORE the supervisor restart/spawn (was nil-spec at restart time)")
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

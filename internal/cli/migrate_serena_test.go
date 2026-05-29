// Package cli — tests for `mcphub migrate serena legacy-to-dynamic-pool`
// (Phase D.3b-2 of docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md).
//
// Harness pattern mirrors install_parsed_manifest_test.go (api side) and
// workspace_cmd_test.go (cli side):
//   - apitest.HardenedTempDir + MCPHUB_STATE_DIR_OVERRIDE + the exported
//     api.SetDaemonStateRootForTest seam so InstallParsedManifest (which
//     resolves api.DaemonStateDir()) and the migrate driver agree on one
//     temp state dir.
//   - MCPHUB_MANIFEST_DIR_OVERRIDE for the writable on-disk serena
//     manifest.yaml the driver reads / rewrites.
//   - A fresh-temp registry via the LOCALAPPDATA/XDG env seam
//     DefaultRegistryPath honors.
//   - installFakeScheduler equivalent via the exported api seam so the
//     driver's InstallParsedManifest call touches no real Task Scheduler.
package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// migrateSerenaFakeScheduler satisfies scheduler.Scheduler so the
// install pipeline routes through it instead of real Task Scheduler.
// The template manifest produces no scheduler tasks, so every method is
// inert; they exist only to satisfy the interface.
type migrateSerenaFakeScheduler struct{}

func (migrateSerenaFakeScheduler) Create(scheduler.TaskSpec) error { return nil }
func (migrateSerenaFakeScheduler) Delete(string) error            { return nil }
func (migrateSerenaFakeScheduler) Run(string) error               { return nil }
func (migrateSerenaFakeScheduler) ExportXML(string) ([]byte, error) {
	return []byte{}, nil
}
func (migrateSerenaFakeScheduler) ImportXML(string, []byte) error { return nil }
func (migrateSerenaFakeScheduler) Stop(string) error              { return nil }
func (migrateSerenaFakeScheduler) Status(name string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{Name: name}, nil
}
func (migrateSerenaFakeScheduler) List(string) ([]scheduler.TaskStatus, error) {
	return nil, nil
}

// migrateSerenaHarness wires every seam the driver touches and returns
// (stateDir, manifestDir). It:
//   - routes api state-dir resolution + cli stateDirFunc at one hardened
//     temp dir,
//   - routes the registry path at a fresh temp dir via the env seam,
//   - installs a fake scheduler + fake canonical-mcphub binary so the
//     install pipeline never touches the real OS,
//   - restores the InstallParsedManifest seam after the test.
func migrateSerenaHarness(t *testing.T) (stateDir, manifestDir string) {
	t.Helper()

	stateDir = apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreState)

	// Registry path env seam (DefaultRegistryPath consults LOCALAPPDATA on
	// Windows, XDG_STATE_HOME elsewhere). Point both at one fresh dir.
	regDir := t.TempDir()
	t.Setenv("LOCALAPPDATA", regDir)
	t.Setenv("XDG_STATE_HOME", regDir)

	// Writable manifest dir seam.
	manifestDir = t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)

	// Fake scheduler + fake canonical mcphub so the install pipeline's
	// Preflight + executeInstallTo never touch real OS surfaces. The
	// canonical binary path must exist on disk because Preflight's
	// ensureCanonicalMcphubPresent stats it; write a stub file (mirrors
	// api's preparePreflightBinaryChecks).
	restoreSch := api.SetTestSchedulerFactoryFn(func() (scheduler.Scheduler, error) {
		return migrateSerenaFakeScheduler{}, nil
	})
	t.Cleanup(restoreSch)
	binDir := t.TempDir()
	canonical := filepath.Join(binDir, api.MCPHubBinaryName())
	if err := os.WriteFile(canonical, []byte(""), 0o755); err != nil {
		t.Fatalf("write fake canonical mcphub: %v", err)
	}
	restoreCanonical := api.SetTestCanonicalMcphubPath(canonical)
	t.Cleanup(restoreCanonical)

	// Restore the InstallParsedManifest seam after every test.
	origInstall := installParsedManifestFn
	t.Cleanup(func() { installParsedManifestFn = origInstall })

	return stateDir, manifestDir
}

// writeSerenaManifest writes a manifest body to
// <manifestDir>/serena/manifest.yaml.
func writeSerenaManifest(t *testing.T, manifestDir, body string) string {
	t.Helper()
	sub := filepath.Join(manifestDir, "serena")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir serena dir: %v", err)
	}
	path := filepath.Join(sub, "manifest.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write serena manifest: %v", err)
	}
	return path
}

// legacy2DaemonManifestBody is a valid kind=global serena manifest with
// the two-daemon claude+codex shape (the pre-migration source state).
const legacy2DaemonManifestBody = `name: serena
kind: global
transport: native-http
command: go
base_args:
  - --from
  - git+https://example/serena
  - serena
  - start-mcp-server
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
client_bindings:
  - client: claude-code
    daemon: claude
    url_path: /mcp
  - client: codex-cli
    daemon: codex
    url_path: /mcp
weekly_refresh: false
`

// unifiedIntermediateManifestBody is the single-`unified`-daemon
// intermediate source state.
const unifiedIntermediateManifestBody = `name: serena
kind: global
transport: native-http
command: go
base_args:
  - --from
  - git+https://example/serena
  - serena
env:
  PYTHONUNBUFFERED: "1"
daemons:
  - name: unified
    context: ide-assistant
    port: 9121
    extra_args: [--context, ide-assistant]
client_bindings:
  - client: claude-code
    daemon: unified
    url_path: /mcp
weekly_refresh: false
`

// alreadyMigratedManifestBody is the target dynamic-pool shape:
// kind=workspace-scoped, no daemons[], daemon_template present.
const alreadyMigratedManifestBody = `name: serena
kind: workspace-scoped
transport: native-http
command: go
base_args:
  - --from
  - git+https://example/serena
  - serena
env:
  PYTHONUNBUFFERED: "1"
daemon_template:
  context: ide-assistant
  port_pool:
    start: 9400
    end: 9499
  extra_args_template:
    - --project
    - ${workspace.path}
client_bindings:
  - client: claude-code
    daemon: serena
    url_path: /mcp
weekly_refresh: false
`

// malformed3DaemonManifestBody has 3 daemons — not a recognized source
// state.
const malformed3DaemonManifestBody = `name: serena
kind: global
transport: native-http
command: go
base_args: [serena]
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
client_bindings:
  - client: claude-code
    daemon: claude
    url_path: /mcp
`

// seedSerenaWorkspaces registers n serena workspaces in the registry the
// env seam points at, returning the canonical workspace paths.
func seedSerenaWorkspaces(t *testing.T, paths ...string) {
	t.Helper()
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	for i, p := range paths {
		entry := api.WorkspaceEntry{
			WorkspaceKey:  api.WorkspaceKey(p),
			WorkspacePath: p,
			Language:      api.SerenaLanguageSentinel,
			Backend:       "serena",
			// Pre-migration the serena rows may carry no port yet
			// (port allocation is part of the migration). Leave Port=0
			// so allocateSerenaPorts assigns one. Use a non-colliding
			// sentinel via i only when a test wants a pre-set port.
			Languages:     []string{"go"},
			RegisteredVia: "manual",
		}
		_ = i
		if err := reg.PutSerena(entry); err != nil {
			t.Fatalf("PutSerena: %v", err)
		}
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
}

// runMigrateSerenaLegacyCmd executes `migrate serena legacy-to-dynamic-pool`
// through the real cobra tree (migrate parent) and returns combined output.
func runMigrateSerenaLegacyCmd(t *testing.T, extraArgs ...string) (string, error) {
	t.Helper()
	c := newMigrateCmd()
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SilenceErrors = true
	args := append([]string{"serena", "legacy-to-dynamic-pool"}, extraArgs...)
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

// readEventsLog returns the supervisor-events.log body (empty string if
// absent).
func readEventsLog(t *testing.T, stateDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, "supervisor-events.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read events log: %v", err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// Source-state detection
// ---------------------------------------------------------------------------

func TestMigrateSerena_DetectsLegacy2Daemon(t *testing.T) {
	m, err := config.ParseManifest(strings.NewReader(legacy2DaemonManifestBody))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	state, err := detectSerenaSourceState(m)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if state != serenaSourceLegacy2Daemon {
		t.Errorf("state = %v, want serenaSourceLegacy2Daemon", state)
	}
}

func TestMigrateSerena_DetectsUnifiedIntermediate(t *testing.T) {
	m, err := config.ParseManifest(strings.NewReader(unifiedIntermediateManifestBody))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	state, err := detectSerenaSourceState(m)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if state != serenaSourceUnifiedIntermediate {
		t.Errorf("state = %v, want serenaSourceUnifiedIntermediate", state)
	}
}

func TestMigrateSerena_DetectsAlreadyMigrated_NoOp(t *testing.T) {
	stateDir, manifestDir := migrateSerenaHarness(t)
	path := writeSerenaManifest(t, manifestDir, alreadyMigratedManifestBody)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	// Seed a workspace so an over-eager driver would otherwise have work
	// to do — the idempotent path must still write nothing.
	seedSerenaWorkspaces(t, "C:/work/alpha")

	out, err := runMigrateSerenaLegacyCmd(t)
	if err != nil {
		t.Fatalf("migrate (already-migrated): want exit 0, got %v\noutput: %s", err, out)
	}
	if !strings.Contains(strings.ToLower(out), "already migrated") {
		t.Errorf("output missing 'already migrated' notice:\n%s", out)
	}

	// ZERO writes: manifest byte-identical.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read manifest: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("already-migrated path mutated the manifest:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// No supervisor-intent.json committed.
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("already-migrated path wrote supervisor-intent.json; stat err = %v", statErr)
	}
}

func TestMigrateSerena_RejectsMalformedManifest(t *testing.T) {
	_, manifestDir := migrateSerenaHarness(t)
	writeSerenaManifest(t, manifestDir, malformed3DaemonManifestBody)
	seedSerenaWorkspaces(t, "C:/work/alpha")

	out, err := runMigrateSerenaLegacyCmd(t)
	if err == nil {
		t.Fatalf("malformed manifest must error, got success\noutput: %s", out)
	}
	if !strings.Contains(strings.ToLower(err.Error()+out), "manual reconciliation required") {
		t.Errorf("malformed error missing 'manual reconciliation required': err=%v\noutput=%s", err, out)
	}
}

func TestMigrateSerena_RejectsEmptyWorkspaceRegistry(t *testing.T) {
	_, manifestDir := migrateSerenaHarness(t)
	writeSerenaManifest(t, manifestDir, legacy2DaemonManifestBody)
	// No workspaces registered.

	out, err := runMigrateSerenaLegacyCmd(t)
	if err == nil {
		t.Fatalf("empty registry must error, got success\noutput: %s", out)
	}
	combined := strings.ToLower(err.Error() + out)
	if !strings.Contains(combined, "register at least one workspace") {
		t.Errorf("empty-registry error missing guidance: err=%v\noutput=%s", err, out)
	}
	if !strings.Contains(combined, "mcphub workspace register") {
		t.Errorf("empty-registry error missing command guidance: err=%v\noutput=%s", err, out)
	}
}

func TestMigrateSerena_AllocatesPortsForEachWorkspace(t *testing.T) {
	_, manifestDir := migrateSerenaHarness(t)
	manifestPath := writeSerenaManifest(t, manifestDir, legacy2DaemonManifestBody)
	wsAlpha := "C:/work/alpha"
	wsBeta := "C:/work/beta"
	seedSerenaWorkspaces(t, wsAlpha, wsBeta)

	out, err := runMigrateSerenaLegacyCmd(t)
	if err != nil {
		t.Fatalf("migrate: %v\noutput: %s", err, out)
	}

	// Registry: each serena workspace now has a distinct port in
	// [9400,9499] (the daemon_template pool the rewrite installs).
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	got := reg.SerenaEntries()
	if len(got) != 2 {
		t.Fatalf("serena entries = %d, want 2", len(got))
	}
	seenPorts := map[int]bool{}
	for _, e := range got {
		if e.Port < 9400 || e.Port > 9499 {
			t.Errorf("workspace %s port %d outside pool [9400,9499]", e.WorkspacePath, e.Port)
		}
		if seenPorts[e.Port] {
			t.Errorf("duplicate port %d allocated across workspaces", e.Port)
		}
		seenPorts[e.Port] = true
	}

	// Manifest rewritten to dynamic-pool: daemons[] dropped,
	// daemon_template present. Parse the on-disk result.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read rewritten manifest: %v", err)
	}
	rewritten, err := config.ParseManifest(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse rewritten manifest: %v\n%s", err, data)
	}
	if rewritten.Kind != config.KindWorkspaceScoped {
		t.Errorf("rewritten kind = %q, want %q", rewritten.Kind, config.KindWorkspaceScoped)
	}
	if len(rewritten.Daemons) != 0 {
		t.Errorf("rewritten daemons[] = %d, want 0", len(rewritten.Daemons))
	}
	if rewritten.DaemonTemplate == nil {
		t.Fatalf("rewritten manifest missing daemon_template")
	}
	if rewritten.DaemonTemplate.PortPool == nil ||
		rewritten.DaemonTemplate.PortPool.Start != 9400 ||
		rewritten.DaemonTemplate.PortPool.End != 9499 {
		t.Errorf("rewritten daemon_template.port_pool = %+v, want {9400,9499}", rewritten.DaemonTemplate.PortPool)
	}
}

func TestMigrateSerena_WritesAuditEvent(t *testing.T) {
	stateDir, manifestDir := migrateSerenaHarness(t)
	writeSerenaManifest(t, manifestDir, legacy2DaemonManifestBody)
	seedSerenaWorkspaces(t, "C:/work/alpha", "C:/work/beta")

	out, err := runMigrateSerenaLegacyCmd(t)
	if err != nil {
		t.Fatalf("migrate: %v\noutput: %s", err, out)
	}

	log := readEventsLog(t, stateDir)
	if !strings.Contains(log, `"event":"serena-dynamic-pool-migration"`) {
		t.Fatalf("audit event 'serena-dynamic-pool-migration' missing from log:\n%s", log)
	}
	// Body carries source_state + target_workspaces + allocated_ports.
	for _, want := range []string{"source_state", "target_workspaces", "allocated_ports"} {
		if !strings.Contains(log, want) {
			t.Errorf("audit event body missing %q:\n%s", want, log)
		}
	}
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

// TestMigrationDriver_RollbackOnIntentWriteFailure_RestoresManifestAndRegistry
// injects a failure at the InstallParsedManifest step (via the
// installParsedManifestFn seam) and asserts the OUTER rollback restored
// the manifest backup AND rolled the registry ports back to pre-migration.
func TestMigrationDriver_RollbackOnIntentWriteFailure_RestoresManifestAndRegistry(t *testing.T) {
	stateDir, manifestDir := migrateSerenaHarness(t)
	manifestPath := writeSerenaManifest(t, manifestDir, legacy2DaemonManifestBody)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	seedSerenaWorkspaces(t, "C:/work/alpha", "C:/work/beta")

	// Pre-migration registry snapshot: serena rows with Port==0.
	regPath, _ := api.DefaultRegistryPath()
	regBefore := api.NewRegistry(regPath)
	if err := regBefore.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	for _, e := range regBefore.SerenaEntries() {
		if e.Port != 0 {
			t.Fatalf("precondition: serena row %s already has port %d", e.WorkspacePath, e.Port)
		}
	}

	// Inject the intent-write failure.
	injected := errors.New("injected intent-write failure")
	installParsedManifestFn = func(_ context.Context, _ *api.API, _ *config.ServerManifest, _ api.InstallParsedManifestOpts) (string, error) {
		return "", injected
	}

	out, err := runMigrateSerenaLegacyCmd(t)
	if err == nil {
		t.Fatalf("driver must surface the injected failure, got success\noutput: %s", out)
	}
	if !errors.Is(err, injected) && !strings.Contains(err.Error(), "injected intent-write failure") {
		t.Errorf("returned error must wrap the injected failure, got: %v", err)
	}

	// Manifest restored byte-for-byte.
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("re-read manifest: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("manifest NOT restored after rollback:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// Registry rolled back: serena rows back to Port==0, no allocations
	// persisted.
	regAfter := api.NewRegistry(regPath)
	if err := regAfter.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	for _, e := range regAfter.SerenaEntries() {
		if e.Port != 0 {
			t.Errorf("registry NOT rolled back: serena row %s has port %d, want 0", e.WorkspacePath, e.Port)
		}
	}

	// No supervisor-intent.json committed (InstallParsedManifest failed
	// before/at the write; its inner stack owns that file).
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("supervisor-intent.json should not exist after failed migration; stat err = %v", statErr)
	}
}

// TestMigrationDriver_RollbackIncompleteAuditEventOnUndoFailure injects a
// failure at the InstallParsedManifest step AND makes ONE undo closure
// fail (the manifest restore, by deleting the manifest dir out from under
// it). Asserts the rollback-incomplete audit event fires with the failed
// step list AND the composite return error includes the rollback error.
func TestMigrationDriver_RollbackIncompleteAuditEventOnUndoFailure(t *testing.T) {
	stateDir, manifestDir := migrateSerenaHarness(t)
	writeSerenaManifest(t, manifestDir, legacy2DaemonManifestBody)
	seedSerenaWorkspaces(t, "C:/work/alpha")

	injected := errors.New("injected intent-write failure")
	installParsedManifestFn = func(_ context.Context, _ *api.API, _ *config.ServerManifest, _ api.InstallParsedManifestOpts) (string, error) {
		// Sabotage the manifest restore undo: replace the serena
		// manifest.yaml path with a directory so restoreManifest's
		// atomic rename onto the path fails. The registry restore undo
		// still succeeds, so exactly ONE undo fails — the composite
		// error + audit event must reflect that.
		serenaDir := filepath.Join(manifestDir, "serena")
		manifestFile := filepath.Join(serenaDir, "manifest.yaml")
		_ = os.Remove(manifestFile)
		if mkErr := os.Mkdir(manifestFile, 0o700); mkErr != nil {
			t.Fatalf("sabotage mkdir: %v", mkErr)
		}
		return "", injected
	}

	out, err := runMigrateSerenaLegacyCmd(t)
	if err == nil {
		t.Fatalf("driver must surface failure, got success\noutput: %s", out)
	}
	// Composite error: primary + rollback failure.
	if !strings.Contains(err.Error(), "rollback also failed") {
		t.Errorf("composite error missing 'rollback also failed': %v", err)
	}
	if !strings.Contains(err.Error(), "injected intent-write failure") {
		t.Errorf("composite error missing primary failure: %v", err)
	}

	// rollback-incomplete audit event fired.
	log := readEventsLog(t, stateDir)
	if !strings.Contains(log, `"event":"rollback-incomplete"`) {
		t.Fatalf("rollback-incomplete audit event missing:\n%s", log)
	}
	if !strings.Contains(log, `"severity":"error"`) {
		t.Errorf("rollback-incomplete event must be severity=error:\n%s", log)
	}
}

// ---------------------------------------------------------------------------
// Command routing — guard against the `migrate serena` shadowing regression
// ---------------------------------------------------------------------------

// TestMigrateSerena_RoutingPreservesStdioSerena asserts the cobra tree
// resolves:
//   - `migrate serena legacy-to-dynamic-pool` → the dynamic-pool leaf,
//   - `migrate serena` (+ extra server args) → the delegating `serena`
//     command that preserves the existing stdio→HTTP migrate behavior
//     (NOT an "unknown command" error and NOT the bare migrate parent),
//   - `migrate memory` → the migrate parent's own RunE (unchanged).
//
// Pure routing via Command.Find — no RunE execution, so it stays
// hermetic (does not touch client configs or the registry).
func TestMigrateSerena_RoutingPreservesStdioSerena(t *testing.T) {
	cases := []struct {
		args     []string
		wantUse  string
		wantArgs []string
	}{
		{[]string{"serena", "legacy-to-dynamic-pool"}, "legacy-to-dynamic-pool", nil},
		{[]string{"serena"}, "serena [server]...", nil},
		{[]string{"serena", "memory"}, "serena [server]...", []string{"memory"}},
		{[]string{"memory", "time"}, "migrate <server>...", []string{"memory", "time"}},
	}
	for _, tc := range cases {
		root := newMigrateCmd()
		got, remaining, err := root.Find(tc.args)
		if err != nil {
			t.Fatalf("Find(%v): %v", tc.args, err)
		}
		if got.Use != tc.wantUse {
			t.Errorf("Find(%v) resolved to %q, want %q", tc.args, got.Use, tc.wantUse)
		}
		if len(remaining) != len(tc.wantArgs) {
			t.Errorf("Find(%v) remaining args = %v, want %v", tc.args, remaining, tc.wantArgs)
			continue
		}
		for i := range remaining {
			if remaining[i] != tc.wantArgs[i] {
				t.Errorf("Find(%v) remaining[%d] = %q, want %q", tc.args, i, remaining[i], tc.wantArgs[i])
			}
		}
	}
}

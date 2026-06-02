package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
)

// ---------------------------------------------------------------------------
// Phase 5 Part B — AutoRegisterSerenaWorkspace unit tests.
//
// The install / immediate-reconcile / readiness steps are routed through
// package-level seams (autoRegisterInstallParsedManifestFn /
// autoRegisterReconcileFn / autoRegisterReadinessFn) plus the catalog seam
// (loadSerenaCatalogManifest) so the register + rollback + idempotency +
// concurrency logic is exercised WITHOUT a live supervisor or network listener.
// Registry state is isolated via the defaultRegistryPathFn seam (a per-test
// temp workspaces.yaml); the audit-log state dir is redirected to a temp dir
// via SetDaemonStateRootForTest so the best-effort emit never touches the
// developer's real %LOCALAPPDATA%.
// ---------------------------------------------------------------------------

// autoRegisterTestEnv isolates all auto-register state into a per-test temp
// tree: a temp workspaces.yaml (via defaultRegistryPathFn), a temp state dir
// (via SetDaemonStateRootForTest, for the audit log), and a stubbed serena
// catalog (a valid kind:global embed shape). It returns the registry path so a
// test can assert on-disk row presence/absence. The install/reconcile/readiness
// seams are NOT set here — each test wires them so the default real impls are
// never reached.
func autoRegisterTestEnv(t *testing.T) (regPath string) {
	t.Helper()
	root := t.TempDir()
	regPath = filepath.Join(root, "workspaces.yaml")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	prevReg := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return regPath, nil }
	t.Cleanup(func() { defaultRegistryPathFn = prevReg })

	restoreStateRoot := SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreStateRoot)

	prevCatalog := loadSerenaCatalogManifest
	loadSerenaCatalogManifest = func() (*config.ServerManifest, error) {
		return autoRegisterCatalogManifest(), nil
	}
	t.Cleanup(func() { loadSerenaCatalogManifest = prevCatalog })

	// Default the cutover seams to the LIVE-ADD posture (the prior intent already
	// carries a serena runtime_spec row, a supervisor is up) so the existing
	// happy-path / idempotency / concurrency / different-root tests exercise the
	// install→reconcile→readiness path with NO reap/start. The reap/start seams
	// are fail-if-called sentinels so any accidental cutover on a live-add path is
	// caught. INTRODUCE tests override autoRegisterPriorIntentHasSpecFn (+ the
	// liveness + reap/start seams) explicitly. (bot PR #253 finding 1)
	prevPrior := autoRegisterPriorIntentHasSpecFn
	autoRegisterPriorIntentHasSpecFn = func() (bool, error) { return true, nil }
	t.Cleanup(func() { autoRegisterPriorIntentHasSpecFn = prevPrior })

	prevSupRun := autoRegisterSupervisorRunningFn
	autoRegisterSupervisorRunningFn = func() (bool, error) { return true, nil }
	t.Cleanup(func() { autoRegisterSupervisorRunningFn = prevSupRun })

	prevReap := autoRegisterReapFn
	autoRegisterReapFn = func(context.Context) error {
		t.Fatalf("reap seam must not be called on the live-add path")
		return nil
	}
	t.Cleanup(func() { autoRegisterReapFn = prevReap })

	prevStart := autoRegisterStartFn
	autoRegisterStartFn = func(context.Context) error {
		t.Fatalf("start seam must not be called on the live-add path")
		return nil
	}
	t.Cleanup(func() { autoRegisterStartFn = prevStart })

	prevStartSupported := autoRegisterStartSupportedFn
	autoRegisterStartSupportedFn = func() bool { return true } // platform supports the cutover by default
	t.Cleanup(func() { autoRegisterStartSupportedFn = prevStartSupported })

	// Default: the install fan-out includes our daemon (the install seam returns a
	// non-existent temp intent path, so the real reader would always say absent).
	// The stale-skip test overrides this to return (false, nil).
	prevVerifyFanOut := autoRegisterVerifyFanOutFn
	autoRegisterVerifyFanOutFn = func(string, string) (bool, error) { return true, nil }
	t.Cleanup(func() { autoRegisterVerifyFanOutFn = prevVerifyFanOut })

	// Reset the per-key concurrency guard so a key registered in a prior test
	// (package-level map persists across tests) cannot leak a held/known lock.
	serenaAutoRegisterKeyMu.Lock()
	serenaAutoRegisterKeyLocks = map[string]*sync.Mutex{}
	serenaAutoRegisterKeyMu.Unlock()

	return regPath
}

// autoRegisterCatalogManifest is a valid serena catalog (current embedded
// kind:global 2-daemon shape). BuildInMemorySerenaDynamicPoolManifest +
// EffectiveSerenaPortPool both accept it; the effective pool is the built-in
// dynamic-pool default (9150..9199).
func autoRegisterCatalogManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		BaseArgs:  []string{"--from", "git+https://example/serena", "serena", "start-mcp-server", "--transport", "streamable-http"},
		Env:       map[string]string{"PYTHONUNBUFFERED": "1"},
		Daemons: []config.DaemonSpec{
			{Name: "claude", Context: "claude-code", Port: 9121, ExtraArgs: []string{"--context", "claude-code"}},
			{Name: "codex", Context: "codex", Port: 9122, ExtraArgs: []string{"--context", "codex"}},
		},
	}
}

// writeSerenaMarker creates <root>/.serena/project.yml with the given body and
// returns root.
func writeSerenaMarker(t *testing.T, root, body string) string {
	t.Helper()
	dir := filepath.Join(root, ".serena")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .serena: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write project.yml: %v", err)
	}
	return root
}

// stubAutoRegisterInstall overrides autoRegisterInstallParsedManifestFn for the
// test scope.
func stubAutoRegisterInstall(t *testing.T, fn func(ctx context.Context, a *API, m *config.ServerManifest, opts InstallParsedManifestOpts) (string, error)) {
	t.Helper()
	orig := autoRegisterInstallParsedManifestFn
	autoRegisterInstallParsedManifestFn = fn
	t.Cleanup(func() { autoRegisterInstallParsedManifestFn = orig })
}

// stubAutoRegisterReconcile overrides autoRegisterReconcileFn for the test scope.
func stubAutoRegisterReconcile(t *testing.T, fn func(ctx context.Context, apply bool) (ReconcileResponse, error)) {
	t.Helper()
	orig := autoRegisterReconcileFn
	autoRegisterReconcileFn = fn
	t.Cleanup(func() { autoRegisterReconcileFn = orig })
}

// stubAutoRegisterReadiness overrides autoRegisterReadinessFn for the test scope.
func stubAutoRegisterReadiness(t *testing.T, fn func(port int, timeout time.Duration) error) {
	t.Helper()
	orig := autoRegisterReadinessFn
	autoRegisterReadinessFn = fn
	t.Cleanup(func() { autoRegisterReadinessFn = orig })
}

// stubAutoRegisterPriorIntentHasSpec overrides the LIVE-ADD-vs-INTRODUCE branch
// seam (true = prior intent already has runtime_spec = live-add).
func stubAutoRegisterPriorIntentHasSpec(t *testing.T, fn func() (bool, error)) {
	t.Helper()
	orig := autoRegisterPriorIntentHasSpecFn
	autoRegisterPriorIntentHasSpecFn = fn
	t.Cleanup(func() { autoRegisterPriorIntentHasSpecFn = orig })
}

// stubAutoRegisterSupervisorRunning overrides the introduce-branch liveness seam.
func stubAutoRegisterSupervisorRunning(t *testing.T, fn func() (bool, error)) {
	t.Helper()
	orig := autoRegisterSupervisorRunningFn
	autoRegisterSupervisorRunningFn = fn
	t.Cleanup(func() { autoRegisterSupervisorRunningFn = orig })
}

// stubAutoRegisterCutover overrides BOTH cutover primitives (reap + start) for
// the test scope. Pass nil for either to simulate the not-wired build/platform.
func stubAutoRegisterCutover(t *testing.T, reap, start func(ctx context.Context) error) {
	t.Helper()
	origReap, origStart := autoRegisterReapFn, autoRegisterStartFn
	autoRegisterReapFn = reap
	autoRegisterStartFn = start
	t.Cleanup(func() { autoRegisterReapFn, autoRegisterStartFn = origReap, origStart })
}

// loadRegSerenaRows loads regPath and returns its serena rows.
func loadRegSerenaRows(t *testing.T, regPath string) []WorkspaceEntry {
	t.Helper()
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	return reg.SerenaEntries()
}

const validSerenaMarkerYAML = "project_name: demo\nlanguages:\n  - python\n  - go\n"

// ---------------------------------------------------------------------------
// 1. No marker → ErrNotASerenaProject, registry untouched.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_NoMarker_ReturnsErrNotASerenaProject(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	// Seams should NEVER be reached; make them fail loudly if they are.
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install seam must not be called when there is no marker")
		return "", nil
	})

	dir := t.TempDir() // no .serena/project.yml anywhere up the chain
	a := NewAPI()
	entry, err := a.AutoRegisterSerenaWorkspace(context.Background(), dir)
	if err == nil {
		t.Fatalf("expected error, got entry %+v", entry)
	}
	if !errors.Is(err, ErrNotASerenaProject) {
		t.Fatalf("error = %v, want ErrNotASerenaProject", err)
	}
	if entry != nil {
		t.Errorf("entry = %+v, want nil", entry)
	}
	// DoS bound: nothing registered.
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows, want 0 (no marker must never register)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 2. Marker present, empty languages → ErrNoLanguages, registry untouched.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_EmptyLanguages_ReturnsErrNoLanguages(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install seam must not be called when languages are empty")
		return "", nil
	})

	root := writeSerenaMarker(t, t.TempDir(), "project_name: demo\nlanguages: []\n")
	a := NewAPI()
	entry, err := a.AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatalf("expected error, got entry %+v", entry)
	}
	if !errors.Is(err, ErrNoLanguages) {
		t.Fatalf("error = %v, want ErrNoLanguages", err)
	}
	if entry != nil {
		t.Errorf("entry = %+v, want nil", entry)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows, want 0 (no-languages must never register)", len(rows))
	}
}

// blank-only languages (`[""]`) must also be rejected as ErrNoLanguages — the
// guard precondition must not accept a whitespace-only declaration.
func TestAutoRegisterSerena_BlankOnlyLanguages_ReturnsErrNoLanguages(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	root := writeSerenaMarker(t, t.TempDir(), "languages:\n  - \"\"\n  - \"   \"\n")
	a := NewAPI()
	_, err := a.AutoRegisterSerenaWorkspace(context.Background(), root)
	if !errors.Is(err, ErrNoLanguages) {
		t.Fatalf("error = %v, want ErrNoLanguages", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows, want 0", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 3. Happy path — seams faked to succeed.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_HappyPath_RegistersInstallsReconcilesAndProbes(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	var (
		installCalled    int32
		installWS        []WorkspaceEntry
		reconcileCalled  int32
		reconcileApply   bool
		readinessCalled  int32
		readinessPort    int
	)
	stubAutoRegisterInstall(t, func(_ context.Context, _ *API, m *config.ServerManifest, opts InstallParsedManifestOpts) (string, error) {
		atomic.AddInt32(&installCalled, 1)
		// Capture the Workspaces snapshot the install was fanned out from.
		installWS = append([]WorkspaceEntry(nil), opts.Workspaces...)
		// The synthesized manifest must be the workspace-scoped dynamic-pool shape.
		if m.Kind != config.KindWorkspaceScoped || m.DaemonTemplate == nil {
			t.Errorf("install manifest kind=%q daemon_template=%v, want workspace-scoped + non-nil template", m.Kind, m.DaemonTemplate != nil)
		}
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(_ context.Context, apply bool) (ReconcileResponse, error) {
		atomic.AddInt32(&reconcileCalled, 1)
		reconcileApply = apply
		return ReconcileResponse{}, nil
	})
	stubAutoRegisterReadiness(t, func(port int, _ time.Duration) error {
		atomic.AddInt32(&readinessCalled, 1)
		readinessPort = port
		return nil
	})

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	a := NewAPI()
	entry, err := a.AutoRegisterSerenaWorkspace(context.Background(), root)
	if err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace: %v", err)
	}
	if entry == nil {
		t.Fatal("entry = nil, want a registered entry")
	}

	// Returned entry shape.
	if entry.Language != SerenaLanguageSentinel {
		t.Errorf("entry.Language = %q, want %q", entry.Language, SerenaLanguageSentinel)
	}
	if entry.Backend != SerenaServerName {
		t.Errorf("entry.Backend = %q, want %q", entry.Backend, SerenaServerName)
	}
	if entry.WorkspacePath != mustCanonical(t, root) {
		t.Errorf("entry.WorkspacePath = %q, want canonical %q (bot PR #253 P2 — store canonical, matching manual register)", entry.WorkspacePath, mustCanonical(t, root))
	}
	if entry.Port < serenaDefaultPortPoolStart || entry.Port > serenaDefaultPortPoolEnd {
		t.Errorf("entry.Port = %d, want within built-in pool [%d,%d]", entry.Port, serenaDefaultPortPoolStart, serenaDefaultPortPoolEnd)
	}
	wantKey := WorkspaceKey(mustCanonical(t, root))
	if entry.WorkspaceKey != wantKey {
		t.Errorf("entry.WorkspaceKey = %q, want %q", entry.WorkspaceKey, wantKey)
	}
	if len(entry.Languages) != 2 || entry.Languages[0] != "python" || entry.Languages[1] != "go" {
		t.Errorf("entry.Languages = %v, want [python go]", entry.Languages)
	}
	if entry.RegisteredVia != "auto-detect" {
		t.Errorf("entry.RegisteredVia = %q, want auto-detect", entry.RegisteredVia)
	}

	// Registry has exactly the one row, with the same port.
	rows := loadRegSerenaRows(t, regPath)
	if len(rows) != 1 {
		t.Fatalf("registry has %d serena rows, want 1", len(rows))
	}
	if rows[0].Port != entry.Port || rows[0].WorkspaceKey != entry.WorkspaceKey {
		t.Errorf("on-disk row = {key=%s port=%d}, want {key=%s port=%d}", rows[0].WorkspaceKey, rows[0].Port, entry.WorkspaceKey, entry.Port)
	}

	// Seam call assertions.
	if installCalled != 1 {
		t.Errorf("install seam called %d times, want 1", installCalled)
	}
	// The install Workspaces snapshot must include the new entry.
	foundInWS := false
	for _, w := range installWS {
		if w.WorkspaceKey == entry.WorkspaceKey && w.Language == SerenaLanguageSentinel {
			foundInWS = true
		}
	}
	if !foundInWS {
		t.Errorf("install Workspaces = %v, must include the new serena entry (key %s)", installWS, entry.WorkspaceKey)
	}
	if reconcileCalled != 1 {
		t.Errorf("reconcile seam called %d times, want 1", reconcileCalled)
	}
	if !reconcileApply {
		t.Errorf("reconcile called with apply=false, want apply=true (immediate spawn)")
	}
	if readinessCalled != 1 {
		t.Errorf("readiness seam called %d times, want 1", readinessCalled)
	}
	if readinessPort != entry.Port {
		t.Errorf("readiness probed port %d, want the allocated port %d", readinessPort, entry.Port)
	}
}

// ---------------------------------------------------------------------------
// 4. Install seam fails (PRE-COMMIT) → rollback fires, registry row removed.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_InstallFails_RollsBackRegistry(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	installErr := errors.New("synthetic install failure")
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return "", installErr
	})
	// Reconcile + readiness must NOT be reached on a pre-commit install failure.
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatalf("reconcile seam must not be called when install fails pre-commit")
		return ReconcileResponse{}, nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error {
		t.Fatalf("readiness seam must not be called when install fails pre-commit")
		return nil
	})

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	a := NewAPI()
	entry, err := a.AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatalf("expected error, got entry %+v", entry)
	}
	if !errors.Is(err, installErr) {
		t.Fatalf("error = %v, want it to wrap the install failure", err)
	}
	if entry != nil {
		t.Errorf("entry = %+v, want nil on install failure", entry)
	}
	// Rollback must have removed the row.
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after install failure, want 0 (rollback must remove the row)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 5. Readiness seam fails POST-COMMIT → NO rollback; entry stays registered.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_ReadinessFails_PostCommit_NoRollback(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	})
	readinessErr := errors.New("synthetic readiness timeout")
	stubAutoRegisterReadiness(t, func(int, time.Duration) error {
		return readinessErr
	})

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	a := NewAPI()
	entry, err := a.AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatalf("expected error, got entry %+v", entry)
	}
	if !errors.Is(err, readinessErr) {
		t.Fatalf("error = %v, want it to wrap the readiness failure", err)
	}
	if entry != nil {
		t.Errorf("entry = %+v, want nil (router maps the error to 503)", entry)
	}
	// The committed intent owns the row: it MUST remain registered so the next
	// call resolves it (no split-state rollback after commit).
	rows := loadRegSerenaRows(t, regPath)
	if len(rows) != 1 {
		t.Fatalf("registry has %d serena rows after post-commit readiness failure, want 1 (no rollback after commit)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 6. Idempotency — two sequential calls, same path → second returns the
//    existing entry, exactly ONE registry row, install called exactly once.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_Idempotent_SecondCallReturnsExisting(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	var installCalled int32
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		atomic.AddInt32(&installCalled, 1)
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	a := NewAPI()

	first, err := a.AutoRegisterSerenaWorkspace(context.Background(), root)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := a.AutoRegisterSerenaWorkspace(context.Background(), root)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if second.Port != first.Port || second.WorkspaceKey != first.WorkspaceKey {
		t.Errorf("second entry {key=%s port=%d} != first {key=%s port=%d} (idempotent call must return the same row)",
			second.WorkspaceKey, second.Port, first.WorkspaceKey, first.Port)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d serena rows after two calls, want exactly 1", len(rows))
	}
	if installCalled != 1 {
		t.Errorf("install seam called %d times across two idempotent calls, want 1 (the second call short-circuits on the existing row)", installCalled)
	}
}

// A call from a SUBDIRECTORY of an already-registered workspace must resolve to
// the same root (ancestor-walk) and be idempotent — not register a second row.
func TestAutoRegisterSerena_FromSubdir_ResolvesToSameRoot_Idempotent(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	sub := filepath.Join(root, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	a := NewAPI()
	fromRoot, err := a.AutoRegisterSerenaWorkspace(context.Background(), root)
	if err != nil {
		t.Fatalf("register from root: %v", err)
	}
	fromSub, err := a.AutoRegisterSerenaWorkspace(context.Background(), sub)
	if err != nil {
		t.Fatalf("register from subdir: %v", err)
	}
	if fromSub.WorkspaceKey != fromRoot.WorkspaceKey {
		t.Errorf("subdir resolved to key %s, want the root key %s (ancestor-walk)", fromSub.WorkspaceKey, fromRoot.WorkspaceKey)
	}
	if fromSub.WorkspacePath != mustCanonical(t, root) {
		t.Errorf("subdir resolved WorkspacePath = %q, want canonical marker root %q", fromSub.WorkspacePath, mustCanonical(t, root))
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d serena rows, want 1 (subdir call must not double-register)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 7. Concurrency — two goroutines, same path → exactly one row, both get the
//    same entry. Run under -race.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_Concurrent_SamePath_RegistersOnce(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	var installCalled int32
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		atomic.AddInt32(&installCalled, 1)
		// Slow the winner slightly so the loser is forced to block on the per-key
		// mutex and then take the idempotent re-read path.
		time.Sleep(20 * time.Millisecond)
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	a := NewAPI()

	const N = 2
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []*WorkspaceEntry
		errs    []error
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			e, err := a.AutoRegisterSerenaWorkspace(context.Background(), root)
			mu.Lock()
			results = append(results, e)
			errs = append(errs, err)
			mu.Unlock()
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent call returned error: %v", err)
		}
	}
	// Exactly one row on disk.
	rows := loadRegSerenaRows(t, regPath)
	if len(rows) != 1 {
		t.Fatalf("registry has %d serena rows after concurrent register, want exactly 1", len(rows))
	}
	// Both callers observe the same row.
	if len(results) != N {
		t.Fatalf("got %d results, want %d", len(results), N)
	}
	if results[0] == nil || results[1] == nil {
		t.Fatalf("a concurrent caller got a nil entry: %+v", results)
	}
	if results[0].WorkspaceKey != results[1].WorkspaceKey || results[0].Port != results[1].Port {
		t.Errorf("concurrent callers got different entries: %+v vs %+v", results[0], results[1])
	}
	// The winner installed exactly once; the loser short-circuited on the
	// idempotent re-read (so install fired at most once).
	if installCalled != 1 {
		t.Errorf("install seam called %d times under concurrent same-path register, want exactly 1", installCalled)
	}
}

// ---------------------------------------------------------------------------
// 8. INTRODUCE + supervisor running (bot PR #253 finding 1) → the one-time
//    cutover: REAP → INSTALL → START, in that order, and NO immediate reconcile
//    (the started supervisor reconciles the committed intent itself).
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_Introduce_SupervisorRunning_ReapsInstallsStarts(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil }) // INTRODUCE
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })   // running → reap

	var (
		mu    sync.Mutex
		order []string
	)
	record := func(step string) { mu.Lock(); order = append(order, step); mu.Unlock() }
	stubAutoRegisterCutover(t,
		func(context.Context) error { record("reap"); return nil },
		func(context.Context) error { record("start"); return nil },
	)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		record("install")
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatalf("reconcile seam must not be called on the introduce/start path (the started supervisor reconciles itself)")
		return ReconcileResponse{}, nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	entry, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace: %v", err)
	}
	if entry == nil {
		t.Fatal("entry = nil, want a registered entry")
	}
	// Order MUST be reap → install → start (reap-first so the spec-bearing write
	// hits the §7.1 gate with no supervisor running).
	if len(order) != 3 || order[0] != "reap" || order[1] != "install" || order[2] != "start" {
		t.Fatalf("cutover order = %v, want [reap install start]", order)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d serena rows, want 1", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 9. INTRODUCE + NO supervisor running → no reap (nothing to kill); INSTALL →
//    START still run (the started supervisor brings the first daemon live).
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_Introduce_NoSupervisor_StartsWithoutReap(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil }) // INTRODUCE
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return false, nil })  // none → no reap

	var startCalled int32
	stubAutoRegisterCutover(t,
		func(context.Context) error { t.Fatalf("reap must not be called when no supervisor is running"); return nil },
		func(context.Context) error { atomic.AddInt32(&startCalled, 1); return nil },
	)
	var installCalled int32
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		atomic.AddInt32(&installCalled, 1)
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatalf("reconcile must not be called on the introduce/start path")
		return ReconcileResponse{}, nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root); err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace: %v", err)
	}
	if installCalled != 1 || startCalled != 1 {
		t.Errorf("installCalled=%d startCalled=%d, want 1 and 1", installCalled, startCalled)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d serena rows, want 1", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 10. INTRODUCE reap FAILS (pre-commit) → install NOT called, row rolled back.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_Introduce_ReapFails_RollsBack(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil })
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })

	reapErr := errors.New("synthetic reap failure")
	stubAutoRegisterCutover(t,
		func(context.Context) error { return reapErr },
		func(context.Context) error { t.Fatalf("start must not be called when reap fails before any install"); return nil },
	)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not be called when the reap fails")
		return "", nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if !errors.Is(err, reapErr) {
		t.Fatalf("error = %v, want it to wrap the reap failure", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after a failed reap, want 0 (rollback)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 11. INTRODUCE install FAILS after a successful reap → recovery START restores
//     the supervisor, and the row is rolled back (pre-commit).
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_Introduce_InstallFailsAfterReap_RecoversAndRollsBack(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil })
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })

	var startCalled int32
	stubAutoRegisterCutover(t,
		func(context.Context) error { return nil },
		func(context.Context) error { atomic.AddInt32(&startCalled, 1); return nil }, // recovery restart
	)
	installErr := errors.New("synthetic install failure after reap")
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return "", installErr
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if !errors.Is(err, installErr) {
		t.Fatalf("error = %v, want it to wrap the install failure", err)
	}
	if startCalled != 1 {
		t.Errorf("recovery start called %d times after install failed post-reap, want 1 (never leave no-supervisor-running)", startCalled)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after install failed post-reap, want 0 (rollback)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 12. INTRODUCE start FAILS POST-COMMIT → NO rollback (the intent owns the row).
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_Introduce_StartFailsPostCommit_NoRollback(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil })
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return false, nil }) // no reap; needStart only

	startErr := errors.New("synthetic post-commit start failure")
	stubAutoRegisterCutover(t,
		func(context.Context) error { t.Fatalf("reap must not be called (no supervisor running)"); return nil },
		func(context.Context) error { return startErr },
	)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if !errors.Is(err, startErr) {
		t.Fatalf("error = %v, want it to wrap the post-commit start failure", err)
	}
	// Intent is the commit point: the row MUST remain so the next call resolves it.
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d serena rows after a post-commit start failure, want 1 (no rollback after commit)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 13. INTRODUCE with cutover primitives NOT wired (nil) → fail loud, row rolled
//     back, install never reached.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_Introduce_CutoverUnwired_FailsLoudAndRollsBack(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil })
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })
	stubAutoRegisterCutover(t, nil, nil) // not wired on this build/platform
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not be reached when the cutover primitives are not wired")
		return "", nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected a fail-loud error when cutover primitives are not wired")
	}
	if !strings.Contains(err.Error(), "not supported on this build/platform") {
		t.Fatalf("error = %v, want it to name the unavailable cutover (nil primitives are caught by the same gate)", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after a not-wired-cutover failure, want 0 (rollback)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 14. Finding 2 — two DIFFERENT roots (sequential live-adds): the SECOND
//     install's Workspaces snapshot must include the FIRST root's row (the fresh
//     re-read captures it), so the second install never drops the first.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_DifferentRoots_SecondInstallSeesFirstRow(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	var (
		mu        sync.Mutex
		snapshots [][]WorkspaceEntry
	)
	stubAutoRegisterInstall(t, func(_ context.Context, _ *API, _ *config.ServerManifest, opts InstallParsedManifestOpts) (string, error) {
		mu.Lock()
		snapshots = append(snapshots, append([]WorkspaceEntry(nil), opts.Workspaces...))
		mu.Unlock()
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	rootA := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	rootB := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	a := NewAPI()
	entryA, err := a.AutoRegisterSerenaWorkspace(context.Background(), rootA)
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	entryB, err := a.AutoRegisterSerenaWorkspace(context.Background(), rootB)
	if err != nil {
		t.Fatalf("register B: %v", err)
	}

	if len(snapshots) != 2 {
		t.Fatalf("install called %d times, want 2", len(snapshots))
	}
	// The SECOND install (root B) must carry BOTH rows — proving the fresh
	// re-read captured root A and the install did not clobber it.
	second := snapshots[1]
	haveA, haveB := false, false
	for _, w := range second {
		if w.WorkspaceKey == entryA.WorkspaceKey {
			haveA = true
		}
		if w.WorkspaceKey == entryB.WorkspaceKey {
			haveB = true
		}
	}
	if !haveA || !haveB {
		t.Errorf("second install Workspaces = %v, want both A(%s) and B(%s) — the re-read must not drop the first root",
			second, entryA.WorkspaceKey, entryB.WorkspaceKey)
	}
	// Both rows persisted on disk.
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 2 {
		t.Errorf("registry has %d serena rows after two different-root registers, want 2", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 15. Relative path → ErrNotASerenaProject (bot PR #253 P1). A relative tool
//     path must NOT be resolved against the GUI's cwd and registered.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_RelativePath_ReturnsErrNotASerenaProject(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install seam must not be called for a relative path")
		return "", nil
	})
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), filepath.Join("some", "relative", "proj"))
	if !errors.Is(err, ErrNotASerenaProject) {
		t.Fatalf("error = %v, want ErrNotASerenaProject for a relative path", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after a relative-path call, want 0", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 16. INTRODUCE with the platform START primitive UNSUPPORTED (non-nil stub) →
//     fail loud BEFORE any reap/install commit (bot PR #253 P2). The reap/start
//     functions are wired (non-nil) but startSupported() is false.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_Introduce_StartUnsupported_FailsLoudPreCommit(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil }) // INTRODUCE
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })
	// reap/start are WIRED (non-nil) but the platform does NOT support the start.
	stubAutoRegisterCutover(t,
		func(context.Context) error { t.Fatalf("reap must not run when start is unsupported (pre-commit gate)"); return nil },
		func(context.Context) error { t.Fatalf("start must not run when start is unsupported"); return nil },
	)
	prevSupported := autoRegisterStartSupportedFn
	autoRegisterStartSupportedFn = func() bool { return false }
	t.Cleanup(func() { autoRegisterStartSupportedFn = prevSupported })
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not be reached when the platform cannot start the supervisor (pre-commit refusal)")
		return "", nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected a pre-commit fail-loud when the supervisor start is unsupported")
	}
	if !strings.Contains(err.Error(), "not supported on this build/platform") {
		t.Fatalf("error = %v, want it to name the unsupported cutover", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after an unsupported-start refusal, want 0 (rollback, nothing committed)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 17. Legacy singular `language:` form (bot PR #253 P2) → registers (not 422).
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_LegacySingularLanguage_Registers(t *testing.T) {
	autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), "project_name: demo\nlanguage: python\n") // legacy singular scalar
	entry, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err != nil {
		t.Fatalf("legacy singular `language:` must register, got: %v", err)
	}
	if len(entry.Languages) != 1 || entry.Languages[0] != "python" {
		t.Errorf("entry.Languages = %v, want [python] (legacy singular form)", entry.Languages)
	}
}

// ---------------------------------------------------------------------------
// 18. Readiness probe is bounded by the remaining context deadline (bot PR #253
//     P3) — a short ctx caps the probe below the 20s default.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_ReadinessHonorsContextDeadline(t *testing.T) {
	autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })
	var gotTimeout time.Duration
	stubAutoRegisterReadiness(t, func(_ int, timeout time.Duration) error { gotTimeout = timeout; return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(ctx, root); err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace: %v", err)
	}
	if gotTimeout <= 0 || gotTimeout > 2*time.Second {
		t.Errorf("readiness timeout = %v, want bounded by the ~2s ctx deadline (NOT the %v default)", gotTimeout, serenaAutoRegisterReadinessTimeout)
	}
}

// ---------------------------------------------------------------------------
// 19. Live-add to a STOPPED pool (bot PR #253 r4 P2) — prior intent has
//     runtime_spec but no supervisor runs → START (no reap), not a bare reconcile.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_LiveAddToStoppedPool_StartsWithoutReap(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return true, nil })  // prior HAS runtime_spec
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return false, nil })  // but supervisor is DOWN

	var startCalled int32
	stubAutoRegisterCutover(t,
		func(context.Context) error { t.Fatalf("reap must not run for a live-add (prior has runtime_spec)"); return nil },
		func(context.Context) error { atomic.AddInt32(&startCalled, 1); return nil },
	)
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatalf("a bare IPC reconcile must NOT be used when the supervisor is down — START must bring it live")
		return ReconcileResponse{}, nil
	})
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root); err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace: %v", err)
	}
	if startCalled != 1 {
		t.Errorf("start called %d times, want 1 (a live-add to a STOPPED pool must START the supervisor)", startCalled)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d rows, want 1", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 20. Stale-skip fan-out (bot PR #253 r4 P2) — the install succeeds but the
//     committed intent does NOT carry our daemon (dir vanished mid-install) →
//     PRE-COMMIT rollback, no readiness, registry row removed.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_StaleSkip_NotInIntent_RollsBack(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil // install "succeeds"
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error {
		t.Fatalf("readiness must not run when the fan-out verify fails pre-commit")
		return nil
	})
	// The committed intent does NOT carry our daemon.
	prevVerify := autoRegisterVerifyFanOutFn
	autoRegisterVerifyFanOutFn = func(string, string) (bool, error) { return false, nil }
	t.Cleanup(func() { autoRegisterVerifyFanOutFn = prevVerify })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "not included in the committed supervisor intent") {
		t.Fatalf("error = %v, want the stale-skip message", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d rows after a stale skip, want 0 (pre-commit rollback)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 21. Context cancelled BEFORE registration (bot PR #253 r4 P2) — a terminated
//     session (router cancels the detached ctx) aborts before any pool-port
//     allocation or registry write.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_ContextCancelledBeforeRegister_Aborts(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not run when ctx is cancelled before registration")
		return "", nil
	})
	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // terminated before the call
	_, err := NewAPI().AutoRegisterSerenaWorkspace(ctx, root)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled (pre-register abort)", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d rows after a pre-register cancel, want 0 (nothing saved)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 22. Context cancelled AFTER Save, BEFORE the install commit (bot PR #253 r4 P2)
//     — the 7c gate rolls back the registry row before any supervisor-intent write.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_ContextCancelledBeforeInstall_RollsBack(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel during the cutover decision (after the registry Save, before the
	// install) via the supervisor-running seam.
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { cancel(); return true, nil })
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not run when ctx is cancelled before the commit")
		return "", nil
	})
	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(ctx, root)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled (pre-install abort)", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d rows after a pre-install cancel, want 0 (rollback)", len(rows))
	}
}

// NOTE: mustCanonical(t, ws) is defined in register_test.go (same package) and
// reused here.

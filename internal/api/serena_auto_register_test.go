package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	if entry.WorkspacePath != root {
		t.Errorf("entry.WorkspacePath = %q, want %q", entry.WorkspacePath, root)
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
	if fromSub.WorkspacePath != root {
		t.Errorf("subdir resolved WorkspacePath = %q, want the marker root %q", fromSub.WorkspacePath, root)
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

// NOTE: mustCanonical(t, ws) is defined in register_test.go (same package) and
// reused here.

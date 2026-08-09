package api

import (
	"context"
	"errors"
	"fmt"
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

	// Default the interlock seam to NIL (the unwired posture): the existing
	// live-add / introduce tests do not exercise the Phase-2 interlock, and a nil
	// seam makes the introduce reap proceed without it (backward-compatible). The
	// package global persists across tests, so reset it here to avoid leakage from
	// a prior Phase-2 test that set it. Phase-2 introduce tests override it.
	prevInterlock := autoRegisterAcquireInterlockFn
	autoRegisterAcquireInterlockFn = nil
	t.Cleanup(func() { autoRegisterAcquireInterlockFn = prevInterlock })

	// Default the area-5 trust gate to TRUSTED so the existing happy-path /
	// idempotency / concurrency / introduce / different-root tests proceed
	// UNCHANGED (the falsifier that trusted behavior is preserved). The
	// dedicated trust-gate tests override this seam to untrusted / error to
	// assert the fail-closed refusal + zero side effects.
	prevTrust := serenaTrustedRootCheckFn
	serenaTrustedRootCheckFn = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { serenaTrustedRootCheckFn = prevTrust })

	// Reset the per-key concurrency guard so a key registered in a prior test
	// (package-level map persists across tests) cannot leak a held/known lock.
	serenaAutoRegisterKeyMu.Lock()
	serenaAutoRegisterKeyLocks = map[string]*sync.Mutex{}
	serenaAutoRegisterKeyMu.Unlock()

	return regPath
}

// stubSerenaTrustedRootCheck overrides the area-5 trust-gate seam for the test
// scope (trusted=true / untrusted=false / error). The default env wires it
// trusted; the trust-gate tests use this to drive the fail-closed paths.
func stubSerenaTrustedRootCheck(t *testing.T, fn func(root string) (bool, error)) {
	t.Helper()
	orig := serenaTrustedRootCheckFn
	serenaTrustedRootCheckFn = fn
	t.Cleanup(func() { serenaTrustedRootCheckFn = orig })
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

func stubAutoRegisterRegistryLock(t *testing.T, fn func(*Registry) (func() error, error)) {
	t.Helper()
	orig := autoRegisterRegistryLockFn
	autoRegisterRegistryLockFn = fn
	t.Cleanup(func() { autoRegisterRegistryLockFn = orig })
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

// stubAutoRegisterAcquireInterlock overrides the Phase-2 interlock acquire seam
// for the test scope (Revision 2 / Starter A — the INTRODUCE-while-running cutover
// holds supervisor.lock across reap→install→start).
func stubAutoRegisterAcquireInterlock(t *testing.T, fn func() (*SupervisorLock, func(), error)) {
	t.Helper()
	orig := autoRegisterAcquireInterlockFn
	autoRegisterAcquireInterlockFn = fn
	t.Cleanup(func() { autoRegisterAcquireInterlockFn = orig })
}

// realAutoRegisterInterlockAcquire is the cross-platform stand-in for the Windows
// production interlock binding: it acquires the REAL supervisor.lock on the §7.1
// gate's exact path (filepath.Join(DaemonStateDir(), "supervisor.lock") — the same
// resolver the install gate uses; redirected to the test temp dir via
// SetDaemonStateRootForTest in autoRegisterTestEnv) via the QUIET acquire (flock
// only, NO owner-sidecar write — matching production after bot PR #276 finding 1)
// and returns the handle plus an idempotent release. Using it as the acquire stub
// lets the bypass-token identity check (lk.path == the gate path), the held-lock
// lifetime, AND the finding-1 invariant (the reap reads the OLD supervisor's intact
// sidecar, not the caller's PID) run identically on Windows and POSIX CI.
func realAutoRegisterInterlockAcquire() (*SupervisorLock, func(), error) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return nil, func() {}, err
	}
	lock, err := AcquireSupervisorLockQuiet(filepath.Join(stateDir, "supervisor.lock"))
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

func TestAutoRegisterSerena_RegistryReleaseFails_PostCommitStartStillCompletes(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil })
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return false, nil })

	releaseErr := fmt.Errorf("%w: synthetic registry release failure", ErrLockReleaseUnconfirmed)
	stubAutoRegisterRegistryLock(t, func(reg *Registry) (func() error, error) {
		release, err := reg.Lock()
		if err != nil {
			return nil, err
		}
		return func() error {
			if err := release(); err != nil {
				return errors.Join(releaseErr, err)
			}
			return releaseErr
		}, nil
	})

	var startCalled, readinessCalled int32
	stubAutoRegisterCutover(t,
		func(context.Context) error {
			t.Fatal("reap must not be called when no supervisor is running")
			return nil
		},
		func(context.Context) error { atomic.AddInt32(&startCalled, 1); return nil },
	)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatal("reconcile must not be called on the introduce/start path")
		return ReconcileResponse{}, nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error {
		atomic.AddInt32(&readinessCalled, 1)
		return nil
	})

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	entry, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if !errors.Is(err, releaseErr) {
		t.Fatalf("error = %v, want registry release failure", err)
	}
	if entry == nil {
		t.Fatal("entry = nil, want the durably committed row")
	}
	if startCalled != 1 || readinessCalled != 1 {
		t.Fatalf("start=%d readiness=%d, want both 1 after the committed release error", startCalled, readinessCalled)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Fatalf("registry has %d serena rows, want the committed row retained", len(rows))
	}
}

func TestAutoRegisterSerena_RegistryReleaseFails_PostCommitReconcileStillCompletes(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	releaseErr := fmt.Errorf("%w: synthetic registry release failure", ErrLockReleaseUnconfirmed)
	stubAutoRegisterRegistryLock(t, func(reg *Registry) (func() error, error) {
		release, err := reg.Lock()
		if err != nil {
			return nil, err
		}
		return func() error {
			if err := release(); err != nil {
				return errors.Join(releaseErr, err)
			}
			return releaseErr
		}, nil
	})

	var reconcileCalled, readinessCalled int32
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		atomic.AddInt32(&reconcileCalled, 1)
		return ReconcileResponse{}, nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error {
		atomic.AddInt32(&readinessCalled, 1)
		return nil
	})

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	entry, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if !errors.Is(err, ErrLockReleaseUnconfirmed) {
		t.Fatalf("error = %v, want ErrLockReleaseUnconfirmed", err)
	}
	if entry == nil {
		t.Fatal("entry = nil, want the durably committed row")
	}
	if reconcileCalled != 1 || readinessCalled != 1 {
		t.Fatalf("reconcile=%d readiness=%d, want both 1 after the committed release error", reconcileCalled, readinessCalled)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Fatalf("registry has %d serena rows, want the committed row retained", len(rows))
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
// 20. Stale-skip via RequireWorkspaceKey (bot PR #253 r6 P2) — the install now
//     FAILS PRE-COMMIT when its stale-row filter drops our triggering workspace
//     (dir vanished mid-install), instead of committing an intent missing it and
//     verifying after. The helper must pass RequireWorkspaceKey, and a pre-commit
//     install error rolls the registry row back (live-add path: no reap/restart).
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_StaleSkip_RequireWorkspaceKeyDrop_RollsBack(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	// The real InstallParsedManifest refuses to write an intent missing the required
	// workspace; the mock asserts the helper passes the key and simulates that
	// pre-commit error. The helper treats it like any install failure → failPreCommit
	// rolls our row back. On the live-add path (default env) there is no reap, so no
	// recovery-restart (the introduce-path recovery is covered by
	// TestAutoRegisterSerena_Introduce_InstallFailsAfterReap_RecoversAndRollsBack).
	stubAutoRegisterInstall(t, func(_ context.Context, _ *API, _ *config.ServerManifest, opts InstallParsedManifestOpts) (string, error) {
		if opts.RequireWorkspaceKey == "" {
			t.Fatalf("auto-register must pass a non-empty RequireWorkspaceKey so a stale drop fails pre-commit")
		}
		return "", fmt.Errorf("InstallParsedManifest: required workspace key %q is not present in the merged supervisor intent (its directory may have been removed mid-install); refusing to commit an intent that drops it", opts.RequireWorkspaceKey)
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error {
		t.Fatalf("readiness must not run when the install fails pre-commit")
		return nil
	})

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "removed mid-install") {
		t.Fatalf("error = %v, want the pre-commit stale-drop message", err)
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

// ---------------------------------------------------------------------------
// 23. Live-add with UNDETERMINED liveness (bot PR #253 r5 P2) — the supervisor
//     liveness probe errors → START (not a bare reconcile that would no-op if the
//     supervisor is in fact down).
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_LiveAddUndeterminedLiveness_Starts(t *testing.T) {
	autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return true, nil }) // live-add
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return false, errors.New("probe failed") })

	var startCalled int32
	stubAutoRegisterCutover(t,
		func(context.Context) error { t.Fatalf("reap must not run for a live-add"); return nil },
		func(context.Context) error { atomic.AddInt32(&startCalled, 1); return nil },
	)
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatalf("a bare reconcile must NOT be used when liveness is undetermined — START must run")
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
		t.Errorf("start called %d times, want 1 (undetermined liveness on a live-add must START, not bare-reconcile)", startCalled)
	}
}

// ---------------------------------------------------------------------------
// 24. Committed install keeps the row (bot PR #253 r6) — with the post-commit
//     fan-out verify removed (RequireWorkspaceKey now guarantees presence
//     PRE-commit), a successful install IS the commit point: the row is kept and
//     the entry returned, no post-commit re-read. Replaces the old
//     transient-verify-error-keeps-row test, whose branch no longer exists.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_CommittedInstall_KeepsRow(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil // install committed
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	entry, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err != nil {
		t.Fatalf("a committed install must succeed (no post-commit verify): %v", err)
	}
	if entry == nil {
		t.Fatal("entry = nil, want the committed registration kept")
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d rows after a committed install, want 1 (row kept at the commit point)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 25. commitCtx survives a mid-cutover session cancel (bot PR #253 r6 #2) — the
//     request ctx is cancelled DURING the install (a session DELETE/sweep after
//     the 7c gate), but the post-commit START runs on commitCtx
//     (context.WithoutCancel), so the recovery-critical start is NOT aborted and
//     its ctx is not cancelled. A reaped-but-not-restarted half cutover would
//     leave NO supervisor running.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_CommitCtxSurvivesSessionCancel_StartStillRuns(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil }) // INTRODUCE
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return false, nil })  // no reap; needStart only

	ctx, cancel := context.WithCancel(context.Background())
	var startCalled int32
	var startCtxCancelled bool
	stubAutoRegisterCutover(t,
		func(context.Context) error { t.Fatalf("reap must not run (no supervisor)"); return nil },
		func(sctx context.Context) error {
			atomic.AddInt32(&startCalled, 1)
			startCtxCancelled = sctx.Err() != nil
			return nil
		},
	)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		// A session DELETE/sweep cancels the REQUEST ctx mid-install, AFTER the 7c
		// gate passed. The real install honors this at its commit hook (fix #1); here
		// the mock simulates a committed install so we can assert the post-commit
		// start still runs on commitCtx despite the cancel.
		cancel()
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(ctx, root); err != nil {
		t.Fatalf("post-commit start must run despite the request-ctx cancel: %v", err)
	}
	if atomic.LoadInt32(&startCalled) != 1 {
		t.Fatalf("start called %d times, want 1 (commitCtx severs the request-ctx cancel)", startCalled)
	}
	if startCtxCancelled {
		t.Error("post-commit start ran on a CANCELLED ctx — commitCtx (WithoutCancel) must not propagate the request cancel")
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d rows, want 1 (committed)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 26. HasSerenaDaemonForWorkspaceKey (bot PR #253 r6 #3) — the in-memory presence
//     check InstallParsedManifest uses for RequireWorkspaceKey. Matches the
//     canonical leading-backslash task name against "mcp-local-hub-serena-<key>".
// ---------------------------------------------------------------------------

func TestSupervisorIntentFile_HasSerenaDaemonForWorkspaceKey(t *testing.T) {
	intent := &SupervisorIntentFile{Daemons: []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-serena-abc123`},
		{TaskName: `\mcp-local-hub-memory-default`},
	}}
	if !intent.HasSerenaDaemonForWorkspaceKey("abc123") {
		t.Error("want present for registered key abc123 (leading backslash trimmed)")
	}
	if intent.HasSerenaDaemonForWorkspaceKey("nope") {
		t.Error("want absent for unregistered key nope")
	}
	if intent.HasSerenaDaemonForWorkspaceKey("memory-default") {
		t.Error("memory-default is NOT a serena key — must not match a non-serena daemon")
	}
	var nilIntent *SupervisorIntentFile
	if nilIntent.HasSerenaDaemonForWorkspaceKey("abc123") {
		t.Error("nil intent must report absent")
	}
}

// ---------------------------------------------------------------------------
// 27. Live-add reconcile UNAVAILABLE = supervisor exited post-probe (bot PR #253
//     r7 P2) — the liveness probe sampled the supervisor running (needStart=false),
//     but it exited before the reconcile, which returns ErrSupervisorIPCUnavailable.
//     The committed intent then has no running supervisor, so auto-register must
//     START one instead of discarding the error and relying on a dead supervisor.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_LiveAddReconcileUnavailable_StartsSupervisor(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return true, nil }) // LIVE-ADD
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })  // sampled running → needStart=false

	var startCalled int32
	stubAutoRegisterCutover(t,
		func(context.Context) error { t.Fatalf("reap must not run on the live-add path"); return nil },
		func(context.Context) error { atomic.AddInt32(&startCalled, 1); return nil },
	)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	// The reconcile finds the supervisor GONE (it exited between the probe and now).
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root); err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace: %v", err)
	}
	if atomic.LoadInt32(&startCalled) != 1 {
		t.Errorf("start called %d times, want 1 (an unavailable reconcile = supervisor gone → must start)", startCalled)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d rows, want 1 (committed despite the post-probe exit)", len(rows))
	}
}

// NOTE: mustCanonical(t, ws) is defined in register_test.go (same package) and
// reused here.

// ===========================================================================
// Untrusted-marker hardening + resolver-walk-parity tests
// (fix/serena-untrusted-marker-reader — the "do it right" follow-up to the
// closed PR #260, whose own tests MISSED the file-target walk regression).
// ===========================================================================

// ---------------------------------------------------------------------------
// R1. THE critical regression. absPath is a real FILE inside a .serena-marked
//     project. resolveSerenaProjectRoot must walk to the PARENT directory that
//     owns the marker (mirroring AncestorWalk), NOT probe
//     `<file>/.serena/project.yml` (which ENOTDIRs on POSIX and aborted the walk
//     in the closed PR). This regression only manifests on POSIX (Windows Lstat
//     on `<file>\.serena\...` returns a different, non-ENOTDIR error), so the
//     assertion is meaningful under GOOS=linux. We assert the END-TO-END
//     auto-register registers (walks file → parent root) so a future
//     resolver-walk regression is caught at the contract level, not just a unit.
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_FileTargetInsideProject_WalksToParent(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	// A real FILE nested inside the project (the path an MCP tool-call commonly
	// names — e.g. the file the agent is editing), NOT the project dir itself.
	srcDir := filepath.Join(root, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	fileTarget := filepath.Join(srcDir, "main.go")
	if err := os.WriteFile(fileTarget, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write file target: %v", err)
	}

	// Pre-check the unit directly so a failure points at the walk, not the whole
	// pipeline. This is the exact assertion the closed PR's tests omitted.
	gotRoot, err := resolveSerenaProjectRoot(fileTarget)
	if err != nil {
		t.Fatalf("resolveSerenaProjectRoot(%q) = error %v; want it to walk to the parent project root (file-target walk parity with AncestorWalk)", fileTarget, err)
	}
	if gotRoot != root {
		t.Fatalf("resolveSerenaProjectRoot(%q) = %q, want the marker-owning parent %q", fileTarget, gotRoot, root)
	}

	entry, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), fileTarget)
	if err != nil {
		t.Fatalf("auto-register from a file target inside the project must succeed (walk to parent): %v", err)
	}
	if entry == nil {
		t.Fatal("entry = nil, want the registered parent-root entry")
	}
	if entry.WorkspacePath != mustCanonical(t, root) {
		t.Errorf("entry.WorkspacePath = %q, want the canonical project root %q (walked from the file target)", entry.WorkspacePath, mustCanonical(t, root))
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d serena rows, want 1 (file-target call registers the parent root)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// R2. Oversized marker (>64 KiB) → rejected by the shared reader, BEFORE any
//     install/registry mutation. Exercises the call-site path
//     (AutoRegisterSerenaWorkspace → readSerenaProjectYMLLanguages →
//     ReadUntrustedSerenaProjectYML).
// ---------------------------------------------------------------------------

func TestAutoRegisterSerena_OversizedMarker_Rejected(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not run for an oversized (rejected) marker")
		return "", nil
	})

	// A valid languages line plus padding that pushes the file past the 64 KiB
	// cap. A comment line keeps the YAML parseable IF it were ever parsed — but
	// the size gate must reject it before the parse.
	body := validSerenaMarkerYAML + "# " + strings.Repeat("A", maxSerenaProjectYMLBytes) + "\n"
	root := writeSerenaMarker(t, t.TempDir(), body)

	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected an oversized-marker rejection, got nil")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("error = %v, want it to name the byte cap", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after an oversized-marker reject, want 0 (rejected before any mutation)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// R3. Symlink at the marker path → refused (never followed). Guarded by a
//     symlink-capability skip (this repo has known symlink-test-host
//     sensitivity: Windows non-admin lacks SeCreateSymbolicLinkPrivilege).
//     Exercises the shared reader directly so the regular-file refusal is
//     asserted independent of the walk (the walk treats a symlinked marker as a
//     hit — parity with AncestorWalk — and the READER is what refuses it).
// ---------------------------------------------------------------------------

func TestReadUntrustedSerenaProjectYML_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	realTarget := filepath.Join(dir, "real-project.yml")
	if err := os.WriteFile(realTarget, []byte(validSerenaMarkerYAML), 0o644); err != nil {
		t.Fatalf("write real target: %v", err)
	}
	link := filepath.Join(dir, "project.yml")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Skipf("symlink unsupported (likely Windows non-admin / no SeCreateSymbolicLinkPrivilege): %v", err)
	}

	_, err := ReadUntrustedSerenaProjectYML(context.Background(), link)
	if err == nil {
		t.Fatal("expected a symlink-marker refusal, got nil (the reader must NEVER follow a symlinked marker)")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("error = %v, want it to name the regular-file requirement", err)
	}
}

// R3b. Symlinked marker rejected end-to-end through auto-register too (the walk
//      finds it as a hit, the reader refuses it). Same symlink-capability skip.
func TestAutoRegisterSerena_SymlinkMarker_Rejected(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not run for a symlinked (refused) marker")
		return "", nil
	})

	root := t.TempDir()
	serenaDir := filepath.Join(root, ".serena")
	if err := os.MkdirAll(serenaDir, 0o755); err != nil {
		t.Fatalf("mkdir .serena: %v", err)
	}
	realTarget := filepath.Join(root, "real-project.yml")
	if err := os.WriteFile(realTarget, []byte(validSerenaMarkerYAML), 0o644); err != nil {
		t.Fatalf("write real target: %v", err)
	}
	marker := filepath.Join(serenaDir, "project.yml")
	if err := os.Symlink(realTarget, marker); err != nil {
		t.Skipf("symlink unsupported (likely Windows non-admin): %v", err)
	}

	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected a symlinked-marker refusal end-to-end, got nil")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("error = %v, want it to name the regular-file requirement", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after a symlink-marker refusal, want 0", len(rows))
	}
}

// ---------------------------------------------------------------------------
// R4. Non-regular marker — a DIRECTORY named `project.yml`. Two assertions:
//       (a) the WALK treats it as a hit (parity with AncestorWalk: any
//           successful Lstat of the marker stops the walk and returns the dir);
//       (b) the READER refuses it (not a regular file), so the end-to-end
//           auto-register fails cleanly with no registry mutation.
//     This documents the chosen mirror-AncestorWalk behavior: the walk is a
//     pure routing step; the hardening lives in the shared reader.
// ---------------------------------------------------------------------------

func TestResolveSerenaProjectRoot_MarkerDir_WalkHitsButReaderRefuses(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not run when the marker is a directory (reader refuses it)")
		return "", nil
	})

	root := t.TempDir()
	// Create `<root>/.serena/project.yml` AS A DIRECTORY (non-regular marker).
	markerDir := filepath.Join(root, ".serena", "project.yml")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatalf("mkdir marker dir: %v", err)
	}

	// (a) The walk hits it (mirror AncestorWalk — any successful Lstat is a hit).
	gotRoot, err := resolveSerenaProjectRoot(root)
	if err != nil {
		t.Fatalf("resolveSerenaProjectRoot(%q) = error %v; want the walk to HIT the non-regular marker dir (mirror AncestorWalk)", root, err)
	}
	if gotRoot != root {
		t.Fatalf("resolveSerenaProjectRoot(%q) = %q, want %q (walk stops at the marker-bearing dir)", root, gotRoot, root)
	}

	// (b) The reader refuses the directory marker; end-to-end fails, no mutation.
	_, err = NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected the reader to refuse a directory marker, got nil")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("error = %v, want it to name the regular-file requirement", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after a directory-marker refusal, want 0", len(rows))
	}
}

// ---------------------------------------------------------------------------
// R5. ctx cancellation propagates as context.Canceled (wrapped with %w) from the
//     shared reader, both before and after the read.
// ---------------------------------------------------------------------------

func TestReadUntrustedSerenaProjectYML_ContextCancelled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.yml")
	if err := os.WriteFile(path, []byte(validSerenaMarkerYAML), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the read

	_, err := ReadUntrustedSerenaProjectYML(ctx, path)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled to propagate (wrapped with %%w)", err)
	}
}

// R5b. A nil ctx is normalized to context.Background() (no panic) and reads OK.
func TestReadUntrustedSerenaProjectYML_NilContextNormalized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.yml")
	if err := os.WriteFile(path, []byte(validSerenaMarkerYAML), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	//nolint:staticcheck // SA1012: deliberately passing nil to assert normalization.
	data, err := ReadUntrustedSerenaProjectYML(nil, path)
	if err != nil {
		t.Fatalf("nil ctx must be normalized and read cleanly, got: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("want the marker bytes back, got empty")
	}
}

// ---------------------------------------------------------------------------
// R6. The shared reader's size cap directly (unit), independent of the call
//     sites: exactly-at-cap is accepted, one-over-cap is rejected.
// ---------------------------------------------------------------------------

func TestReadUntrustedSerenaProjectYML_SizeCapBoundary(t *testing.T) {
	dir := t.TempDir()

	atCap := filepath.Join(dir, "at-cap.yml")
	if err := os.WriteFile(atCap, make([]byte, maxSerenaProjectYMLBytes), 0o644); err != nil {
		t.Fatalf("write at-cap file: %v", err)
	}
	if data, err := ReadUntrustedSerenaProjectYML(context.Background(), atCap); err != nil {
		t.Fatalf("a file exactly at the cap must be accepted, got: %v", err)
	} else if len(data) != maxSerenaProjectYMLBytes {
		t.Fatalf("read %d bytes, want exactly the cap %d", len(data), maxSerenaProjectYMLBytes)
	}

	overCap := filepath.Join(dir, "over-cap.yml")
	if err := os.WriteFile(overCap, make([]byte, maxSerenaProjectYMLBytes+1), 0o644); err != nil {
		t.Fatalf("write over-cap file: %v", err)
	}
	if _, err := ReadUntrustedSerenaProjectYML(context.Background(), overCap); err == nil {
		t.Fatal("a file one byte over the cap must be rejected, got nil")
	} else if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("error = %v, want it to name the byte cap", err)
	}
}

// R6b. Missing marker → bare not-exist error (NOT %w-wrapped) so BOTH
//      os.IsNotExist(err) and errors.Is(err, os.ErrNotExist) detect it. The
//      `mcphub workspace register` caller relies on os.IsNotExist, which does
//      not unwrap %w chains.
func TestReadUntrustedSerenaProjectYML_MissingMarker_IsNotExistDetectable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.yml")
	_, err := ReadUntrustedSerenaProjectYML(context.Background(), path)
	if err == nil {
		t.Fatal("expected a not-found error for a missing marker, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("os.IsNotExist(err) = false for a missing marker; the `workspace register` not-found branch relies on it (err = %v)", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(err, os.ErrNotExist) = false for a missing marker (err = %v)", err)
	}
}

// ---------------------------------------------------------------------------
// R7. Both call-sites exercise the shared reader. The api side is covered above
//     (R2/R3b/R4 all flow AutoRegisterSerenaWorkspace →
//     readSerenaProjectYMLLanguages → ReadUntrustedSerenaProjectYML). This
//     guards the second call-site signature directly: readSerenaProjectYMLLanguages
//     now takes a context and honors a cancelled one (proving it routes through
//     the ctx-aware shared reader, not a bare os.ReadFile).
// ---------------------------------------------------------------------------

func TestReadSerenaProjectYMLLanguages_HonorsCancelledContext(t *testing.T) {
	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readSerenaProjectYMLLanguages(ctx, root)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled (proves the helper routes through the ctx-aware shared reader)", err)
	}
}

// ---------------------------------------------------------------------------
// R7. Symlink-to-DIRECTORY workspace root (bot PR #262 P2 finding 1). A symlink
//     whose target is a directory is a legitimate user-provided workspace-root
//     alias and must resolve: the walk must start AT the symlink (os.Stat
//     follows it to a dir) and find the target's `.serena/project.yml`, not
//     wrongly fall back to the parent. Regression guard for the Lstat-based
//     walk start. Symlink-capability skip (Windows non-admin).
// ---------------------------------------------------------------------------

func TestResolveSerenaProjectRoot_SymlinkDirRoot_Resolves(t *testing.T) {
	base := t.TempDir()
	realProject := filepath.Join(base, "real-project")
	if err := os.MkdirAll(filepath.Join(realProject, ".serena"), 0o755); err != nil {
		t.Fatalf("mkdir target project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realProject, ".serena", "project.yml"), []byte(validSerenaMarkerYAML), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realProject, link); err != nil {
		t.Skipf("symlink unsupported (likely Windows non-admin / no SeCreateSymbolicLinkPrivilege): %v", err)
	}

	root, err := resolveSerenaProjectRoot(link)
	if err != nil {
		t.Fatalf("a symlink-to-directory workspace root must resolve, got error: %v", err)
	}
	// The walk starts AT the symlink, so it returns the symlink path itself
	// (downstream registration canonicalizes it for de-dup).
	if root != link {
		t.Fatalf("resolved root = %q, want the symlink path %q (the walk starts at the symlink-to-dir)", root, link)
	}
}

// ---------------------------------------------------------------------------
// R8. Symlinked `.serena` container directory (bot PR #262 P2 finding 2). A
//     cloned repo that makes `.serena` itself a symlink to another directory
//     would redirect the marker read outside the project tree, even though the
//     leaf `project.yml` is a regular file. The reader must refuse the
//     symlinked container (it no-follows the `.serena` parent, not just the
//     leaf). Symlink-capability skip (Windows non-admin).
// ---------------------------------------------------------------------------

func TestReadUntrustedSerenaProjectYML_SymlinkedSerenaDir_Rejected(t *testing.T) {
	base := t.TempDir()
	// Attacker-controlled real dir OUTSIDE the project tree, with a valid marker.
	evilDir := filepath.Join(base, "evil")
	if err := os.MkdirAll(evilDir, 0o755); err != nil {
		t.Fatalf("mkdir evil: %v", err)
	}
	if err := os.WriteFile(filepath.Join(evilDir, "project.yml"), []byte(validSerenaMarkerYAML), 0o644); err != nil {
		t.Fatalf("write evil marker: %v", err)
	}
	// Project root whose `.serena` is a SYMLINK to evilDir (supply-chain redirect).
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.Symlink(evilDir, filepath.Join(root, ".serena")); err != nil {
		t.Skipf("symlink unsupported (likely Windows non-admin): %v", err)
	}

	_, err := ReadUntrustedSerenaProjectYML(context.Background(), filepath.Join(root, ".serena", "project.yml"))
	if err == nil {
		t.Fatal("expected refusal for a symlinked .serena container, got nil (a symlinked marker dir must not redirect the read outside the project tree)")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want it to name the symlink-container refusal", err)
	}
}

// ---------------------------------------------------------------------------
// Phase 2 (Revision 2 / Starter A) + bot PR #276 r2 P1 — the INTRODUCE-while-running
// cutover acquires the supervisor.lock interlock IMMEDIATELY AFTER its reap (NOT
// before): the running supervisor holds the lock, so the reap must free it first.
// ---------------------------------------------------------------------------

// TestAutoRegisterSerena_Introduce_AcquiresInterlockAfterReap asserts the ordering
// (bot PR #276 r2 P1): the interlock acquire fires AFTER the reap, not before. The
// running supervisor holds supervisor.lock; a pre-reap acquire could never succeed on
// this introduce-while-running case. The full introduce order is
// reap→acquire→install→start. (This test stubs the acquire to always succeed; the
// production-fidelity "a REAL held lock blocks a pre-reap acquire but the post-reap
// acquire succeeds" case is TestAutoRegisterSerena_Introduce_WhileSupervisorHoldsLock_ReapsThenAcquires_Succeeds.)
func TestAutoRegisterSerena_Introduce_AcquiresInterlockAfterReap(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil }) // INTRODUCE
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })   // running → reap

	var (
		mu    sync.Mutex
		order []string
	)
	record := func(step string) { mu.Lock(); order = append(order, step); mu.Unlock() }
	stubAutoRegisterAcquireInterlock(t, func() (*SupervisorLock, func(), error) {
		record("acquire")
		return realAutoRegisterInterlockAcquire()
	})
	stubAutoRegisterCutover(t,
		func(context.Context) error { record("reap"); return nil },
		func(context.Context) error { record("start"); return nil },
	)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		record("install")
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root); err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace: %v", err)
	}
	// reap MUST precede acquire; the full introduce order is reap→acquire→install→start.
	if len(order) != 4 || order[0] != "reap" || order[1] != "acquire" || order[2] != "install" || order[3] != "start" {
		t.Fatalf("cutover order = %v, want [reap acquire install start]", order)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d serena rows, want 1", len(rows))
	}
}

// TestAutoRegisterSerena_Introduce_WhileSupervisorHoldsLock_ReapsThenAcquires_Succeeds
// is the bot PR #276 r2 P1 regression guard — the exact case the bot said currently
// 503s. A REAL supervisor.lock is held BEFORE the call (simulating the live
// supervisor that holds it on its startup). The introduce-while-running cutover must
// nonetheless SUCCEED: the reap RELEASES that held lock (the reap stub here releases
// it, as the real reap kills the holder → the OS frees the flock), and only then does
// the post-reap acquire take the freed lock and install. A pre-reap acquire (the
// unfixed code) would TryLock the still-held lock, fail, and defer with a 503 — never
// reaching reap/install. This test FAILS on the unfixed acquire-then-reap ordering.
func TestAutoRegisterSerena_Introduce_WhileSupervisorHoldsLock_ReapsThenAcquires_Succeeds(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil }) // INTRODUCE
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })   // running → reap

	sd, derr := DaemonStateDir()
	if derr != nil {
		t.Fatalf("DaemonStateDir: %v", derr)
	}
	lockPath := filepath.Join(sd, "supervisor.lock")

	// Simulate the RUNNING supervisor holding supervisor.lock on its startup. We hold
	// the REAL flock so the production-fidelity TryLock contention is exercised.
	heldByOldSupervisor, herr := AcquireSupervisorLockQuiet(lockPath)
	if herr != nil {
		t.Fatalf("seed held supervisor.lock: %v", herr)
	}
	oldSupervisorReleased := false
	releaseOldSupervisor := func() {
		if oldSupervisorReleased {
			return
		}
		oldSupervisorReleased = true
		heldByOldSupervisor.Release()
	}
	t.Cleanup(releaseOldSupervisor)

	// The reap RELEASES the held lock (the real reap kills the supervisor that holds it;
	// the OS then frees the flock). Without this release the post-reap acquire would
	// still be blocked — proving the held lock is what the reap clears.
	var (
		reapRan, acquireRan, installRan bool
	)
	stubAutoRegisterCutover(t,
		func(context.Context) error {
			reapRan = true
			releaseOldSupervisor() // the reap freed the lock the dead supervisor held
			return nil
		},
		func(context.Context) error { return nil },
	)
	// Production interlock binding (REAL quiet acquire on the gate path). Before the
	// reap this would fail (lock held); after the reap it succeeds.
	stubAutoRegisterAcquireInterlock(t, func() (*SupervisorLock, func(), error) {
		acquireRan = true
		return realAutoRegisterInterlockAcquire()
	})
	stubAutoRegisterInstall(t, func(_ context.Context, _ *API, _ *config.ServerManifest, opts InstallParsedManifestOpts) (string, error) {
		installRan = true
		// The cutover holds its OWN (post-reap) lock now → a valid matching bypass token.
		if lk := opts.SupervisorLockBypass.lk; lk == nil || lk.path != lockPath {
			t.Errorf("install must receive a matching bypass token (post-reap held lock); got %+v", opts.SupervisorLockBypass.lk)
		}
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root); err != nil {
		t.Fatalf("introduce-while-supervisor-holds-lock must SUCCEED (reap frees the lock, then acquire takes it): %v", err)
	}
	if !reapRan || !acquireRan || !installRan {
		t.Fatalf("expected reap+acquire+install to all run; reap=%v acquire=%v install=%v", reapRan, acquireRan, installRan)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d serena rows, want 1 (the introduce committed)", len(rows))
	}
}

// TestAutoRegisterSerena_Introduce_DefersWhenInterlockHeldAfterReap_RecoversAndRollsBack:
// the post-reap acquire FAILS (a concurrent migrate/cutover reaped the same supervisor
// and won the race for the freed lock). Because the acquire is now AFTER the reap (bot
// PR #276 r2 P1), the reap DID run — so this is a POST-reap defer: the reap seam ran,
// the recovery-restart (failPreCommit) start seam runs (the system is not left
// supervisor-less by US — the winner restarts one, but we still attempt the recovery
// for our OWN reap), the registry row is rolled back, the install NEVER runs, and the
// distinct serena-auto-register-deferred-on-interlock event is emitted.
func TestAutoRegisterSerena_Introduce_DefersWhenInterlockHeldAfterReap_RecoversAndRollsBack(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil })
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })

	heldErr := errors.New("supervisor.lock held (quiet acquire could not take the flock)")
	var reapRan, startRan bool
	// The reap RUNS (defer is now POST-reap); the recovery-restart start RUNS too.
	stubAutoRegisterCutover(t,
		func(context.Context) error { reapRan = true; return nil },
		func(context.Context) error { startRan = true; return nil },
	)
	// Acquire fails AFTER the reap.
	stubAutoRegisterAcquireInterlock(t, func() (*SupervisorLock, func(), error) {
		if !reapRan {
			t.Fatalf("the interlock acquire must fire AFTER the reap (bot PR #276 r2 P1)")
		}
		return nil, func() {}, heldErr
	})
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must NOT run when the post-reap interlock acquire failed (defer before the write)")
		return "", nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected a defer-on-interlock error when the post-reap acquire fails")
	}
	if !errors.Is(err, heldErr) {
		t.Fatalf("error = %v, want it to wrap the held-interlock acquire failure", err)
	}
	// HONEST error (another cutover acquired the lock — NOT "supervisor running").
	if !strings.Contains(err.Error(), "supervisor.lock") {
		t.Errorf("error must name the honest interlock reason; got %v", err)
	}
	if !reapRan {
		t.Error("the reap must have run before the post-reap defer (the defer is after the reap)")
	}
	if !startRan {
		t.Error("the recovery-restart start must run on the post-reap defer (failPreCommit restores a supervisor for our own reap)")
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after a deferred introduce, want 0 (rollback)", len(rows))
	}
	// The DISTINCT deferred-on-interlock event was emitted (NOT spec-bearing-install-refused).
	sd, derr := DaemonStateDir()
	if derr != nil {
		t.Fatalf("DaemonStateDir: %v", derr)
	}
	raw, _ := os.ReadFile(filepath.Join(sd, SupervisorEventLogFileLeaf))
	if !strings.Contains(string(raw), "serena-auto-register-deferred-on-interlock") {
		t.Errorf("expected the distinct deferred-on-interlock event; log=%s", string(raw))
	}
	if strings.Contains(string(raw), "spec-bearing-install-refused") {
		t.Errorf("must NOT emit spec-bearing-install-refused on the interlock-defer path; log=%s", string(raw))
	}
}

// TestAutoRegisterSerena_Introduce_PassesInterlockBypassTokenToInstall asserts the
// install seam receives a NON-nil bypass token whose lock path MATCHES the §7.1
// gate's own supervisor.lock path (so the gate's identity check passes and the
// spec-bearing write proceeds while the cutover holds its own lock). (Named with
// "Interlock" so the Phase-2 -run filter 'Interlock|AutoRegisterIntroduce|...'
// reaches it.)
func TestAutoRegisterSerena_Introduce_PassesInterlockBypassTokenToInstall(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil })
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })

	stubAutoRegisterAcquireInterlock(t, realAutoRegisterInterlockAcquire)
	stubAutoRegisterCutover(t,
		func(context.Context) error { return nil },
		func(context.Context) error { return nil },
	)

	sd, derr := DaemonStateDir()
	if derr != nil {
		t.Fatalf("DaemonStateDir: %v", derr)
	}
	gateLockPath := filepath.Join(sd, "supervisor.lock")

	var (
		gotNonNil  bool
		gotMatched bool
	)
	stubAutoRegisterInstall(t, func(_ context.Context, _ *API, _ *config.ServerManifest, opts InstallParsedManifestOpts) (string, error) {
		// In-package test: inspect the opaque token's unexported lock pointer.
		if lk := opts.SupervisorLockBypass.lk; lk != nil {
			gotNonNil = true
			if lk.path == gateLockPath {
				gotMatched = true
			}
		}
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root); err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace: %v", err)
	}
	if !gotNonNil {
		t.Fatal("the install seam must receive a non-nil SupervisorLockBypass on the introduce-while-running path (the cutover holds its own lock)")
	}
	if !gotMatched {
		t.Fatalf("the bypass token's lock path must match the gate's supervisor.lock (%s) so the §7.1 identity check passes", gateLockPath)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d serena rows, want 1", len(rows))
	}
}

// TestAutoRegisterSerena_Introduce_NoSupervisor_AcquiresInterlockBeforeSpecBearingWrite
// is the bot PR #276 r4 P2 regression guard. The FIRST serena workspace is introduced
// while NO supervisor is running (needStart && !needReap && !priorHasSpec): the write
// is still spec-bearing (runtime_spec), so the write→start window must be protected by
// the interlock exactly as the migrate's step-7e no-reap boundary protects it. The
// cutover must: (a) acquire the interlock (the seam fires) WITHOUT reaping (no
// supervisor to reap), and (b) pass a NON-nil bypass token whose lock path matches the
// §7.1 gate's supervisor.lock to the install — so the held lock authorizes the
// spec-bearing write and excludes a foreign starter for the whole window. On the UNFIXED
// code the interlock seam never fires on the !needReap path and the install receives a
// zero-value bypass token → both assertions FAIL.
func TestAutoRegisterSerena_Introduce_NoSupervisor_AcquiresInterlockBeforeSpecBearingWrite(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil }) // INTRODUCE (spec-bearing)
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return false, nil })  // none → no reap

	sd, derr := DaemonStateDir()
	if derr != nil {
		t.Fatalf("DaemonStateDir: %v", derr)
	}
	gateLockPath := filepath.Join(sd, "supervisor.lock")

	var (
		mu          sync.Mutex
		order       []string
		acquireRan  bool
		installRan  bool
		startRan    bool
		gotNonNil   bool
		gotMatched  bool
		lockHeldDur bool // the install observed the interlock STILL held (lk.fl != nil)
	)
	record := func(step string) { mu.Lock(); order = append(order, step); mu.Unlock() }

	// Production-fidelity interlock binding (REAL quiet acquire on the gate path). No
	// supervisor holds the lock here, so the acquire succeeds.
	stubAutoRegisterAcquireInterlock(t, func() (*SupervisorLock, func(), error) {
		acquireRan = true
		record("acquire")
		return realAutoRegisterInterlockAcquire()
	})
	// The reap MUST NOT be called — there is no supervisor to reap on this path.
	stubAutoRegisterCutover(t,
		func(context.Context) error { t.Fatalf("reap must not be called on the no-supervisor introduce path"); return nil },
		func(context.Context) error { startRan = true; record("start"); return nil },
	)
	stubAutoRegisterInstall(t, func(_ context.Context, _ *API, _ *config.ServerManifest, opts InstallParsedManifestOpts) (string, error) {
		installRan = true
		record("install")
		if lk := opts.SupervisorLockBypass.lk; lk != nil {
			gotNonNil = true
			if lk.path == gateLockPath {
				gotMatched = true
			}
			if lk.fl != nil {
				lockHeldDur = true
			}
		}
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	// Live-add reconcile must NOT fire (this is an introduce/start path).
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatalf("reconcile must not be called on the introduce/start path")
		return ReconcileResponse{}, nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root); err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace (no-supervisor introduce): %v", err)
	}
	if !acquireRan {
		t.Fatal("the interlock acquire seam MUST fire on the no-supervisor introduce (spec-bearing write→start window must be protected) — bot PR #276 r4 P2")
	}
	if !installRan || !startRan {
		t.Fatalf("expected install+start to run; install=%v start=%v", installRan, startRan)
	}
	if !gotNonNil {
		t.Fatal("the install seam MUST receive a non-nil SupervisorLockBypass on the no-supervisor introduce (the cutover holds its own lock)")
	}
	if !gotMatched {
		t.Fatalf("the bypass token's lock path must match the gate's supervisor.lock (%s) so the §7.1 identity check passes", gateLockPath)
	}
	if !lockHeldDur {
		t.Fatal("the interlock must still be HELD (lk.fl != nil) when the spec-bearing install runs — held across write→start, not released early")
	}
	// Ordering: acquire BEFORE the spec-bearing install, and start AFTER it. (No reap.)
	if len(order) != 3 || order[0] != "acquire" || order[1] != "install" || order[2] != "start" {
		t.Fatalf("step order = %v, want [acquire install start] (no reap on the no-supervisor introduce)", order)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Errorf("registry has %d serena rows, want 1 (the no-supervisor introduce committed)", len(rows))
	}
}

// TestAutoRegisterSerena_Introduce_NoSupervisor_DefersWhenInterlockHeld_NoReap is the
// bot PR #276 r4 P2 defer guard. When a concurrent serena migrate/cutover already holds
// supervisor.lock, the no-supervisor introduce's interlock acquire FAILS. Because no
// supervisor was running, NOTHING is reaped (so failPreCommit owes no recovery restart):
// the cutover defers cleanly — install NEVER runs, the registry row is rolled back, the
// distinct serena-auto-register-deferred-on-interlock event is emitted (NOT
// spec-bearing-install-refused), and the honest 503 error names supervisor.lock. On the
// UNFIXED code no acquire happens on the !needReap path, so the install runs and the
// call SUCCEEDS → this test FAILS (it expects a defer error + no install).
func TestAutoRegisterSerena_Introduce_NoSupervisor_DefersWhenInterlockHeld_NoReap(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil }) // INTRODUCE
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return false, nil })  // none → no reap

	heldErr := errors.New("supervisor.lock held (quiet acquire could not take the flock)")
	// The reap MUST NOT run (no supervisor); the start MUST NOT run (no reap → no
	// recovery restart owed on the defer path, and the install never reaches a start).
	stubAutoRegisterCutover(t,
		func(context.Context) error { t.Fatalf("reap must not run on the no-supervisor introduce defer"); return nil },
		func(context.Context) error { t.Fatalf("start must not run on the no-supervisor introduce defer (no reap → no recovery restart owed)"); return nil },
	)
	// The acquire fails (a concurrent cutover holds the freed lock).
	stubAutoRegisterAcquireInterlock(t, func() (*SupervisorLock, func(), error) {
		return nil, func() {}, heldErr
	})
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must NOT run when the no-supervisor introduce interlock acquire failed (defer before the spec-bearing write)")
		return "", nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatal("expected a defer-on-interlock error when the no-supervisor introduce acquire fails")
	}
	if !errors.Is(err, heldErr) {
		t.Fatalf("error = %v, want it to wrap the held-interlock acquire failure", err)
	}
	// HONEST error (another cutover holds the lock — NOT "supervisor running").
	if !strings.Contains(err.Error(), "supervisor.lock") {
		t.Errorf("error must name the honest interlock reason; got %v", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows after a deferred no-supervisor introduce, want 0 (rollback)", len(rows))
	}
	// The DISTINCT deferred-on-interlock event was emitted (NOT spec-bearing-install-refused).
	sd, derr := DaemonStateDir()
	if derr != nil {
		t.Fatalf("DaemonStateDir: %v", derr)
	}
	raw, _ := os.ReadFile(filepath.Join(sd, SupervisorEventLogFileLeaf))
	if !strings.Contains(string(raw), "serena-auto-register-deferred-on-interlock") {
		t.Errorf("expected the distinct deferred-on-interlock event; log=%s", string(raw))
	}
	if strings.Contains(string(raw), "spec-bearing-install-refused") {
		t.Errorf("must NOT emit spec-bearing-install-refused on the interlock-defer path; log=%s", string(raw))
	}
}

// TestAutoRegisterSerena_Introduce_ReapTargetsOldSupervisorNotCaller is the bot PR
// #276 finding-1 regression guard. Under the reap-then-acquire ordering (r2 P1) the
// reap runs BEFORE any interlock touch, AND the post-reap acquire is QUIET (no
// sidecar write), so the sidecar names the OLD supervisor when the reap reads it —
// doubly safe. A regression to the FULL AcquireSupervisorLock would OVERWRITE
// supervisor.lock.owner.json with the CALLER's (router/GUI) PID; if such an acquire
// ever ran before the reap again, the reap — which reads that sidecar to choose the
// PID it taskkills/IPC-handshakes against — would target the CALLER instead of the
// old supervisor.
//
// The test seeds the sidecar with a sentinel OLD-supervisor PID, wires the REAL
// quiet acquire stub, and the reap stub asserts ReadSupervisorLockOwner STILL
// reports that sentinel — never the caller's os.Getpid(). A regression to the
// sidecar-overwriting acquire fails this assertion.
func TestAutoRegisterSerena_Introduce_ReapTargetsOldSupervisorNotCaller(t *testing.T) {
	_ = autoRegisterTestEnv(t) // isolates registry + state dir; path not asserted here
	stubAutoRegisterPriorIntentHasSpec(t, func() (bool, error) { return false, nil }) // INTRODUCE
	stubAutoRegisterSupervisorRunning(t, func() (bool, error) { return true, nil })   // running → reap

	sd, derr := DaemonStateDir()
	if derr != nil {
		t.Fatalf("DaemonStateDir: %v", derr)
	}
	lockPath := filepath.Join(sd, "supervisor.lock")
	// Seed the OLD supervisor's sidecar. A sentinel PID that is NOT this process's
	// PID so the assertion is unambiguous. (The reap stub reads the sidecar
	// directly; it does not go through AcquireSupervisorLock's liveness probe, so
	// any value works as the OLD-supervisor identity.)
	const oldSupervisorPID = 424242
	if oldSupervisorPID == os.Getpid() {
		t.Fatalf("sentinel PID collided with the test process PID; pick another")
	}
	if err := WriteStateFileAtomic(lockPath+".owner.json", SupervisorLockOwner{
		PID:       oldSupervisorPID,
		StartedAt: "2026-06-09T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed old-supervisor sidecar: %v", err)
	}

	// REAL quiet acquire (matches production); it must NOT clobber the seeded sidecar.
	stubAutoRegisterAcquireInterlock(t, realAutoRegisterInterlockAcquire)

	var reapSawPID int
	reapRan := false
	stubAutoRegisterCutover(t,
		func(context.Context) error {
			reapRan = true
			owner, oErr := ReadSupervisorLockOwner(lockPath)
			if oErr != nil {
				t.Errorf("reap could not read the owner sidecar (the quiet acquire must leave it intact): %v", oErr)
				return nil
			}
			reapSawPID = owner.PID
			return nil
		},
		func(context.Context) error { return nil },
	)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root); err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace: %v", err)
	}
	if !reapRan {
		t.Fatal("the reap seam must have run on the introduce-while-running path")
	}
	if reapSawPID == os.Getpid() {
		t.Fatalf("finding 1 REGRESSION: the reap read the CALLER's PID (%d) from the sidecar — the interlock acquire overwrote it; a concurrent reap would force-kill the caller, not the old supervisor", reapSawPID)
	}
	if reapSawPID != oldSupervisorPID {
		t.Fatalf("the reap must read the OLD supervisor's PID (%d) from the intact sidecar; got %d", oldSupervisorPID, reapSawPID)
	}
}


// ---------------------------------------------------------------------------
// AREA-5 TRUST GATE — the security core.
//
// AutoRegisterSerenaWorkspace must REFUSE (ErrSerenaRootNotTrusted) when the
// resolved marker-bearing root is NOT trusted, and must do so with ZERO side
// effects: no registry load/save, no pool-port allocation, no install, no
// reconcile, no readiness probe, and NO supervisor interlock acquire / reap /
// start. The refusal runs AFTER the marker check (composes with the DoS bound)
// and BEFORE step-3, so a refused root can never perturb the supervisor
// interlock.
// ---------------------------------------------------------------------------

// TestAutoRegisterSerena_UntrustedRoot_RefusesWithZeroSideEffects is THE
// security test. An untrusted (but marker-bearing) root must return
// ErrSerenaRootNotTrusted and touch NOTHING.
func TestAutoRegisterSerena_UntrustedRoot_RefusesWithZeroSideEffects(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	stubSerenaTrustedRootCheck(t, func(string) (bool, error) { return false, nil })

	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install seam must NOT be called for an untrusted root")
		return "", nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatalf("reconcile seam must NOT be called for an untrusted root")
		return ReconcileResponse{}, nil
	})
	stubAutoRegisterReadiness(t, func(int, time.Duration) error {
		t.Fatalf("readiness seam must NOT be called for an untrusted root")
		return nil
	})
	stubAutoRegisterAcquireInterlock(t, func() (*SupervisorLock, func(), error) {
		t.Fatalf("supervisor interlock must NOT be acquired for an untrusted root")
		return nil, func() {}, nil
	})

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	entry, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if err == nil {
		t.Fatalf("expected ErrSerenaRootNotTrusted, got entry %+v", entry)
	}
	if !errors.Is(err, ErrSerenaRootNotTrusted) {
		t.Fatalf("error = %v, want ErrSerenaRootNotTrusted", err)
	}
	if entry != nil {
		t.Errorf("entry = %+v, want nil", entry)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows, want 0 (untrusted root must never register)", len(rows))
	}
}

// TestAutoRegisterSerena_GateError_FailsClosed: a trusted-root check that ERRORS
// must fail CLOSED — refuse with ErrSerenaRootNotTrusted, no side effects.
func TestAutoRegisterSerena_GateError_FailsClosed(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	stubSerenaTrustedRootCheck(t, func(string) (bool, error) {
		return false, errors.New("simulated trusted-roots store load failure")
	})
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install seam must NOT be called when the trust gate errors (fail-closed)")
		return "", nil
	})

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if !errors.Is(err, ErrSerenaRootNotTrusted) {
		t.Fatalf("error = %v, want ErrSerenaRootNotTrusted (fail-closed on gate error)", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows, want 0 (gate error must fail closed)", len(rows))
	}
}

// TestAutoRegisterSerena_NilGate_FailsClosed: a NIL trust-gate seam (legacy /
// unwired) must fail CLOSED, never silently authorize.
func TestAutoRegisterSerena_NilGate_FailsClosed(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	stubSerenaTrustedRootCheck(t, nil)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install seam must NOT be called when the trust gate is nil (fail-closed)")
		return "", nil
	})

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if !errors.Is(err, ErrSerenaRootNotTrusted) {
		t.Fatalf("error = %v, want ErrSerenaRootNotTrusted (nil gate must fail closed)", err)
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 0 {
		t.Errorf("registry has %d serena rows, want 0 (nil gate must fail closed)", len(rows))
	}
}

// TestAutoRegisterSerena_NoMarker_ComposesBeforeTrustGate: the DoS marker bound
// runs FIRST — a path with no marker returns ErrNotASerenaProject and the trust
// seam is never consulted.
func TestAutoRegisterSerena_NoMarker_ComposesBeforeTrustGate(t *testing.T) {
	autoRegisterTestEnv(t)
	trustCalled := false
	stubSerenaTrustedRootCheck(t, func(string) (bool, error) {
		trustCalled = true
		return false, nil
	})

	dir := t.TempDir()
	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), dir)
	if !errors.Is(err, ErrNotASerenaProject) {
		t.Fatalf("error = %v, want ErrNotASerenaProject (marker DoS bound runs before the trust gate)", err)
	}
	if trustCalled {
		t.Error("trust gate must NOT be consulted for a path with no marker (the marker check returns first)")
	}
}

// TestAutoRegisterSerena_TrustGateConsultsResolvedRoot: the gate must consult
// the CANONICAL resolved root (the marker dir), not the raw tool-argument
// absPath, so a register-blessed tree matches when the agent calls from a
// subdirectory/file under the marked root.
func TestAutoRegisterSerena_TrustGateConsultsResolvedRoot(t *testing.T) {
	autoRegisterTestEnv(t)

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	wantRoot := root

	var sawRoot string
	stubSerenaTrustedRootCheck(t, func(r string) (bool, error) {
		sawRoot = r
		return false, nil
	})

	deepFile := filepath.Join(root, "pkg", "main.go")
	if err := os.MkdirAll(filepath.Dir(deepFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(deepFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _ = NewAPI().AutoRegisterSerenaWorkspace(context.Background(), deepFile)
	if sawRoot != wantRoot {
		t.Fatalf("trust gate saw root %q, want the resolved marker root %q", sawRoot, wantRoot)
	}
}

// TestAutoRegisterSerena_AutoRegisterNeverBlesses is the bless-not-on-router
// invariant for serena (mirror of TestLSPRouter_AutoRegisterPathDoesNotBless): a
// SUCCESSFUL auto-register must NOT write the trusted-roots store.
func TestAutoRegisterSerena_AutoRegisterNeverBlesses(t *testing.T) {
	autoRegisterTestEnv(t)

	stubSerenaTrustedRootCheck(t, func(string) (bool, error) { return true, nil })
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	storePath, err := DefaultLSPTrustedRootsPath()
	if err != nil {
		t.Fatalf("resolve trusted-roots store path: %v", err)
	}
	if _, statErr := os.Stat(storePath); statErr == nil {
		t.Fatalf("trusted-roots store unexpectedly exists before the router runs: %s", storePath)
	}

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root); err != nil {
		t.Fatalf("AutoRegisterSerenaWorkspace: %v", err)
	}

	if _, statErr := os.Stat(storePath); statErr == nil {
		t.Fatalf("serena auto-register blessed a trusted root (store created at %s) — re-opens the vulnerability (area-5 claim 10)", storePath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error on trusted-roots store: %v", statErr)
	}
	f, err := LoadDefaultLSPTrustedRoots()
	if err != nil {
		t.Fatalf("load trusted-roots after auto-register: %v", err)
	}
	if len(f.Roots) != 0 {
		t.Fatalf("serena auto-register must record zero trusted roots, got %v", f.Roots)
	}
}

// TestAutoRegisterSerena_RegressionGuard_BlessedParentAllowsSiblingIntroduce is
// the analyst-flagged regression made falsifiable (area-5 co-design). With the
// trust gate live, an explicit register that blesses a tree's root MUST let a
// SIBLING .serena project under that same root auto-introduce. We exercise the
// REAL trust predicate (WorkspaceRootTrusted, reading the live redirected store)
// — NOT the stubbed seam — so the bless→containment→authorize chain is proven
// end-to-end. The install/reconcile/readiness seams are faked (no supervisor),
// but the AUTHORIZATION decision is real.
func TestAutoRegisterSerena_RegressionGuard_BlessedParentAllowsSiblingIntroduce(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	// Wire the REAL trust predicate (the env defaults it to always-true; here we
	// want the genuine store-backed containment check).
	stubSerenaTrustedRootCheck(t, WorkspaceRootTrusted)

	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })
	stubAutoRegisterReadiness(t, func(int, time.Duration) error { return nil })

	// A tree root explicitly blessed (as `mcphub workspace register` would do via
	// serenaRegisterBlessTrustedRootFn → BlessDefaultTrustedRoot). The store is
	// redirected by autoRegisterTestEnv's SetDaemonStateRootForTest.
	treeRoot := t.TempDir()
	parent := filepath.Join(treeRoot, "monorepo")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := BlessDefaultTrustedRoot(parent); err != nil {
		t.Fatalf("bless parent (simulating explicit register): %v", err)
	}

	// A SIBLING serena project UNDER the blessed parent (never explicitly
	// registered itself). Pre-gate it would be refused; post-bless it must
	// auto-introduce.
	sibling := writeSerenaMarker(t, filepath.Join(parent, "service-b"), validSerenaMarkerYAML)

	entry, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), sibling)
	if err != nil {
		t.Fatalf("sibling under a blessed parent must auto-introduce; got %v", err)
	}
	if entry == nil {
		t.Fatal("entry = nil, want a registered sibling entry")
	}
	if rows := loadRegSerenaRows(t, regPath); len(rows) != 1 {
		t.Fatalf("want 1 serena row after sibling auto-introduce, got %d", len(rows))
	}

	// Negative half: a project OUTSIDE the blessed tree is still refused, so the
	// bless did not over-broaden trust.
	outside := writeSerenaMarker(t, filepath.Join(treeRoot, "elsewhere"), validSerenaMarkerYAML)
	if _, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), outside); !errors.Is(err, ErrSerenaRootNotTrusted) {
		t.Fatalf("a project outside the blessed tree must be refused; got %v", err)
	}
}

// TestAutoRegisterSerena_NotTrusted_TypedErrorCarriesResolvedRoot (area-5 r2):
// the refusal must be a *SerenaRootNotTrustedError whose ResolvedRoot is the
// CANONICAL marker root (NOT the raw tool-arg subpath/file), AND errors.Is must
// still match the ErrSerenaRootNotTrusted sentinel. This is the api-side
// contract the router relies on to name the authorizable root.
func TestAutoRegisterSerena_NotTrusted_TypedErrorCarriesResolvedRoot(t *testing.T) {
	autoRegisterTestEnv(t)
	stubSerenaTrustedRootCheck(t, func(string) (bool, error) { return false, nil })

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	wantRoot := mustCanonical(t, root)

	// Call from a FILE deep under the marker root — the typed error must carry the
	// resolved ROOT, not this subpath.
	deepFile := filepath.Join(root, "sub", "deep", "file.go")
	if err := os.MkdirAll(filepath.Dir(deepFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(deepFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), deepFile)
	if !errors.Is(err, ErrSerenaRootNotTrusted) {
		t.Fatalf("error = %v, want errors.Is ErrSerenaRootNotTrusted", err)
	}
	var typed *SerenaRootNotTrustedError
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want errors.As *SerenaRootNotTrustedError", err)
	}
	if typed.ResolvedRoot != wantRoot {
		t.Errorf("typed.ResolvedRoot = %q, want the canonical marker root %q (NOT the subpath %q)", typed.ResolvedRoot, wantRoot, deepFile)
	}
	if typed.ResolvedRoot == deepFile {
		t.Errorf("typed error leaked the raw tool-arg subpath %q", deepFile)
	}
}

// TestAutoRegisterSerena_GateError_TypedErrorCarriesRootAndCause (area-5 r2): the
// gate-error path is also a *SerenaRootNotTrustedError carrying the resolved
// root, so the router still names the authorizable root on a fail-closed gate
// error.
func TestAutoRegisterSerena_GateError_TypedErrorCarriesRootAndCause(t *testing.T) {
	autoRegisterTestEnv(t)
	stubSerenaTrustedRootCheck(t, func(string) (bool, error) {
		return false, errors.New("corrupt store")
	})

	root := writeSerenaMarker(t, t.TempDir(), validSerenaMarkerYAML)
	wantRoot := mustCanonical(t, root)

	_, err := NewAPI().AutoRegisterSerenaWorkspace(context.Background(), root)
	if !errors.Is(err, ErrSerenaRootNotTrusted) {
		t.Fatalf("error = %v, want errors.Is ErrSerenaRootNotTrusted", err)
	}
	var typed *SerenaRootNotTrustedError
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want errors.As *SerenaRootNotTrustedError", err)
	}
	if typed.ResolvedRoot != wantRoot {
		t.Errorf("typed.ResolvedRoot = %q, want %q", typed.ResolvedRoot, wantRoot)
	}
	if typed.Cause == nil || !strings.Contains(typed.Cause.Error(), "corrupt store") {
		t.Errorf("typed.Cause = %v, want it to carry the gate error", typed.Cause)
	}
}

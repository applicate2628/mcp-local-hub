package api

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/config"
)

// seedSerenaRegistryRow persists a serena workspace row to the test registry, exactly
// as auto-register would (TaskName = mcp-local-hub-serena-<key>).
func seedSerenaRegistryRow(t *testing.T, regPath, key, wsPath string, port int) {
	t.Helper()
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		t.Fatalf("seed: lock registry: %v", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		t.Fatalf("seed: load registry: %v", err)
	}
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey:  key,
		WorkspacePath: wsPath,
		Port:          port,
		Backend:       SerenaServerName,
		Language:      SerenaLanguageSentinel,
		TaskName:      "mcp-local-hub-serena-" + key,
		RegisteredVia: "auto-detect",
		Languages:     []string{"go"},
	}); err != nil {
		t.Fatalf("seed: put serena row %q: %v", key, err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("seed: save registry: %v", err)
	}
}

// writeTestIntentAt writes a supervisor intent at path carrying a serena daemon for
// each key; withSpec controls whether those daemons carry a runtime_spec (so
// HasRuntimeSpecRow reports true). An empty keys writes an intent with no serena
// daemons (no runtime_spec).
func writeTestIntentAt(t *testing.T, path string, keys []string, withSpec bool) {
	t.Helper()
	intent := &SupervisorIntentFile{Version: 1}
	for _, k := range keys {
		d := SupervisorDaemon{TaskName: `\mcp-local-hub-serena-` + k, Server: "serena", Command: "mcphub.exe"}
		if withSpec {
			d.RuntimeSpec = &DaemonRuntimeSpec{}
		}
		intent.Daemons = append(intent.Daemons, d)
	}
	if err := WriteSupervisorIntent(path, intent); err != nil {
		t.Fatalf("write test intent %s: %v", path, err)
	}
}

// seedSerenaIntent writes the PRE-install state-dir intent (the one the repair reads
// at orphan detection).
func seedSerenaIntent(t *testing.T, withDaemon []string, withSpec bool) {
	t.Helper()
	sd, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("seed: state dir: %v", err)
	}
	writeTestIntentAt(t, joinStateFilePath(sd, supervisorIntentFileLeaf), withDaemon, withSpec)
}

// installMockWriting returns an install seam that records the call, captures the
// Workspaces count, and writes a POST-install committed intent carrying committed (so
// the repair's convergence verify reads it via the returned path).
func installMockWriting(t *testing.T, calls *int32, gotWorkspaces *int, committed []string) func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
	return func(_ context.Context, _ *API, _ *config.ServerManifest, opts InstallParsedManifestOpts) (string, error) {
		atomic.AddInt32(calls, 1)
		if gotWorkspaces != nil {
			*gotWorkspaces = len(opts.Workspaces)
		}
		p := filepath.Join(t.TempDir(), "supervisor-intent.json")
		writeTestIntentAt(t, p, committed, true)
		return p, nil
	}
}

// 1. Healthy — every registry row owns its intent daemon → no repair, no install.
func TestRepairOrphanSerenaWorkspaces_Healthy_NoInstall(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	seedSerenaRegistryRow(t, regPath, "k1", "/proj/k1", 9150)
	seedSerenaRegistryRow(t, regPath, "k2", "/proj/k2", 9151)
	seedSerenaIntent(t, []string{"k1", "k2"}, true)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not run when every row owns its intent daemon")
		return "", nil
	})

	repaired, unresolved, err := NewAPI().RepairOrphanSerenaWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("RepairOrphanSerenaWorkspaces: %v", err)
	}
	if repaired != 0 || len(unresolved) != 0 {
		t.Errorf("repaired=%d unresolved=%v, want 0 and none (healthy)", repaired, unresolved)
	}
}

// 2. Live-add orphan — a row missing from a spec-bearing intent → re-install all rows;
//    the committed intent now carries the orphan → it is confirmed repaired.
func TestRepairOrphanSerenaWorkspaces_LiveAddOrphan_ReInstalls(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	seedSerenaRegistryRow(t, regPath, "k1", "/proj/k1", 9150)
	seedSerenaRegistryRow(t, regPath, "k2", "/proj/k2", 9151)
	seedSerenaIntent(t, []string{"k1"}, true) // k2 orphaned; intent HAS runtime_spec

	var installCalled int32
	var gotWorkspaces int
	stubAutoRegisterInstall(t, installMockWriting(t, &installCalled, &gotWorkspaces, []string{"k1", "k2"}))
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })

	repaired, unresolved, err := NewAPI().RepairOrphanSerenaWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("RepairOrphanSerenaWorkspaces: %v", err)
	}
	if repaired != 1 || len(unresolved) != 0 {
		t.Errorf("repaired=%d unresolved=%v, want 1 and none (k2 confirmed re-installed)", repaired, unresolved)
	}
	if atomic.LoadInt32(&installCalled) != 1 {
		t.Errorf("install called %d times, want 1", installCalled)
	}
	if gotWorkspaces != 2 {
		t.Errorf("install Workspaces = %d, want 2 (full fan-out re-installs all rows)", gotWorkspaces)
	}
}

// 3. Stale orphan — the install's stale-workspace filter DROPS a row whose dir was
//    removed; the committed intent still lacks it → it must NOT be counted as repaired,
//    it must be surfaced as unresolved (bot PR #254 P2).
func TestRepairOrphanSerenaWorkspaces_StaleOrphan_NotReportedRepaired(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	seedSerenaRegistryRow(t, regPath, "k1", "/proj/k1", 9150)
	seedSerenaRegistryRow(t, regPath, "k2", "/proj/k2-removed", 9151)
	seedSerenaIntent(t, []string{"k1"}, true) // k2 orphaned

	var installCalled int32
	// The install drops k2 (its dir vanished) — the committed intent carries only k1.
	stubAutoRegisterInstall(t, installMockWriting(t, &installCalled, nil, []string{"k1"}))
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })

	repaired, unresolved, err := NewAPI().RepairOrphanSerenaWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("RepairOrphanSerenaWorkspaces: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired=%d, want 0 (k2 was stale-dropped, must not be claimed repaired)", repaired)
	}
	if len(unresolved) != 1 || unresolved[0] != "k2" {
		t.Errorf("unresolved=%v, want [k2] (stale row surfaced for operator cleanup)", unresolved)
	}
}

// 4. Reconcile UNAVAILABLE post-install — the supervisor exited after GUI startup, so
//    the repair must START it (mirror auto-register r7), not leave the committed orphan
//    daemons unspawned (bot PR #254 P2).
func TestRepairOrphanSerenaWorkspaces_ReconcileUnavailable_StartsSupervisor(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	seedSerenaRegistryRow(t, regPath, "k1", "/proj/k1", 9150)
	seedSerenaRegistryRow(t, regPath, "k2", "/proj/k2", 9151)
	seedSerenaIntent(t, []string{"k1"}, true) // k2 orphaned; intent HAS runtime_spec

	var installCalled int32
	stubAutoRegisterInstall(t, installMockWriting(t, &installCalled, nil, []string{"k1", "k2"}))
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable // supervisor gone post-startup
	})
	var startCalled int32
	stubAutoRegisterCutover(t,
		func(context.Context) error { t.Fatalf("repair must not reap"); return nil },
		func(context.Context) error { atomic.AddInt32(&startCalled, 1); return nil },
	)

	repaired, unresolved, err := NewAPI().RepairOrphanSerenaWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("RepairOrphanSerenaWorkspaces: %v", err)
	}
	if repaired != 1 || len(unresolved) != 0 {
		t.Errorf("repaired=%d unresolved=%v, want 1 and none", repaired, unresolved)
	}
	if atomic.LoadInt32(&startCalled) != 1 {
		t.Errorf("start called %d times, want 1 (unavailable reconcile = supervisor gone → must start)", startCalled)
	}
}

// 5. Introduce-crash — orphan(s) with NO runtime_spec → defer to migrate, no install.
func TestRepairOrphanSerenaWorkspaces_IntroduceCrash_Defers(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	seedSerenaRegistryRow(t, regPath, "k1", "/proj/k1", 9150)
	// No intent file written → intent nil → no runtime_spec (first-introduce crash).
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not run for a first-introduce crash (no runtime_spec)")
		return "", nil
	})

	repaired, unresolved, err := NewAPI().RepairOrphanSerenaWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("RepairOrphanSerenaWorkspaces: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired=%d, want 0 (deferred, not repaired)", repaired)
	}
	if len(unresolved) != 1 || unresolved[0] != "k1" {
		t.Errorf("unresolved=%v, want [k1] (introduce-crash deferred to migrate)", unresolved)
	}
}

// 6. No serena rows — nothing to scan.
func TestRepairOrphanSerenaWorkspaces_NoRows_NoOp(t *testing.T) {
	autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not run with no registered serena rows")
		return "", nil
	})
	repaired, unresolved, err := NewAPI().RepairOrphanSerenaWorkspaces(context.Background())
	if err != nil || repaired != 0 || len(unresolved) != 0 {
		t.Errorf("got (%d, %v, %v), want (0, none, nil)", repaired, unresolved, err)
	}
}

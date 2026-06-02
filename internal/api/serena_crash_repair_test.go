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

// seedSerenaIntent writes a supervisor intent carrying a serena daemon for each key in
// withDaemon; withSpec controls whether those daemons carry a runtime_spec (so
// HasRuntimeSpecRow reports true). An empty withDaemon writes an intent with no serena
// daemons (no runtime_spec).
func seedSerenaIntent(t *testing.T, withDaemon []string, withSpec bool) {
	t.Helper()
	sd, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("seed: state dir: %v", err)
	}
	intentPath := joinStateFilePath(sd, supervisorIntentFileLeaf)
	intent := &SupervisorIntentFile{Version: 1}
	for _, k := range withDaemon {
		d := SupervisorDaemon{TaskName: `\mcp-local-hub-serena-` + k, Server: "serena", Command: "mcphub.exe"}
		if withSpec {
			d.RuntimeSpec = &DaemonRuntimeSpec{}
		}
		intent.Daemons = append(intent.Daemons, d)
	}
	if err := WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatalf("seed: write intent: %v", err)
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

	repaired, deferred, err := NewAPI().RepairOrphanSerenaWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("RepairOrphanSerenaWorkspaces: %v", err)
	}
	if repaired != 0 || len(deferred) != 0 {
		t.Errorf("repaired=%d deferred=%v, want 0 and none (healthy)", repaired, deferred)
	}
}

// 2. Live-add orphan — a row missing from a spec-bearing intent → re-install all rows.
func TestRepairOrphanSerenaWorkspaces_LiveAddOrphan_ReInstalls(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	seedSerenaRegistryRow(t, regPath, "k1", "/proj/k1", 9150)
	seedSerenaRegistryRow(t, regPath, "k2", "/proj/k2", 9151)
	seedSerenaIntent(t, []string{"k1"}, true) // k2 orphaned; intent HAS runtime_spec

	var installCalled int32
	var gotWorkspaces int
	stubAutoRegisterInstall(t, func(_ context.Context, _ *API, _ *config.ServerManifest, opts InstallParsedManifestOpts) (string, error) {
		atomic.AddInt32(&installCalled, 1)
		gotWorkspaces = len(opts.Workspaces)
		return filepath.Join(t.TempDir(), "supervisor-intent.json"), nil
	})
	stubAutoRegisterReconcile(t, func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil })

	repaired, deferred, err := NewAPI().RepairOrphanSerenaWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("RepairOrphanSerenaWorkspaces: %v", err)
	}
	if repaired != 1 || len(deferred) != 0 {
		t.Errorf("repaired=%d deferred=%v, want 1 and none (k2 re-installed)", repaired, deferred)
	}
	if atomic.LoadInt32(&installCalled) != 1 {
		t.Errorf("install called %d times, want 1", installCalled)
	}
	if gotWorkspaces != 2 {
		t.Errorf("install Workspaces = %d, want 2 (full fan-out re-installs all rows)", gotWorkspaces)
	}
}

// 3. Introduce-crash — orphan(s) with NO runtime_spec → defer to migrate, no install.
func TestRepairOrphanSerenaWorkspaces_IntroduceCrash_Defers(t *testing.T) {
	regPath := autoRegisterTestEnv(t)
	seedSerenaRegistryRow(t, regPath, "k1", "/proj/k1", 9150)
	// No intent file written → intent nil → no runtime_spec (first-introduce crash).
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not run for a first-introduce crash (no runtime_spec)")
		return "", nil
	})

	repaired, deferred, err := NewAPI().RepairOrphanSerenaWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("RepairOrphanSerenaWorkspaces: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired=%d, want 0 (deferred, not repaired)", repaired)
	}
	if len(deferred) != 1 || deferred[0] != "k1" {
		t.Errorf("deferred=%v, want [k1] (introduce-crash deferred to migrate)", deferred)
	}
}

// 4. No serena rows — nothing to scan.
func TestRepairOrphanSerenaWorkspaces_NoRows_NoOp(t *testing.T) {
	autoRegisterTestEnv(t)
	stubAutoRegisterInstall(t, func(context.Context, *API, *config.ServerManifest, InstallParsedManifestOpts) (string, error) {
		t.Fatalf("install must not run with no registered serena rows")
		return "", nil
	})
	repaired, deferred, err := NewAPI().RepairOrphanSerenaWorkspaces(context.Background())
	if err != nil || repaired != 0 || len(deferred) != 0 {
		t.Errorf("got (%d, %v, %v), want (0, none, nil)", repaired, deferred, err)
	}
}

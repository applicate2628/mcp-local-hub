// hub_listener_groups_phase3_test.go — groups/namespaces Phase 3.
//
// Integration test for the REAL ResolverSnapshot publish path wired into
// the gate-ON hub-listener startup.
//
// The Phase 3 diagnostic (work-items/decisions/2026-06-18-groups-
// namespaces-tool-visibility.md "Phase 3 diagnostic finding") confirmed
// PublishResolverSnapshot / BumpResolverOnManifestChange had ZERO
// production callers — only test files published. So in gate-ON
// hub-aggregate mode the live hub LoadResolverSnapshot()'d nil → empty
// participants → a dormant aggregate.
//
// These tests exercise the PRODUCTION publish seam
// publishResolverSnapshotForHubBind(a) — the exact function
// startHubMcpListener now calls after BindHubMcpListener succeeds — NOT
// the manual-PublishResolverSnapshot unit fixtures that masked the gap.
// They drive the seam from a hermetic set of manifests carrying
// client_bindings (seeded via MCPHUB_MANIFEST_DIR_OVERRIDE, the same
// embed-bypass seam a.Scan()/a.ManifestGet() honor) and assert the
// published snapshot is non-nil with the expected Bindings.
//
// Gate-OFF must NOT publish: startHubMcpListener short-circuits before
// the publish seam when disabled.
//
// Spec: groups/namespaces decision §"Phase 3 diagnostic finding" + Defect A.

package gui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// seedManifestDir writes a minimal manifest carrying a client_bindings
// row under a per-test temp dir and points MCPHUB_MANIFEST_DIR_OVERRIDE
// at it so a.Scan() + a.ManifestGet() (the production hub-bind manifest
// source) read ONLY these seeded manifests with no embed/disk leakage.
func seedManifestDir(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	srvDir := filepath.Join(root, "fs")
	if err := os.MkdirAll(srvDir, 0o700); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	manifest := `name: fs
kind: global
transport: stdio-bridge
command: echo
base_args: ["fs"]
daemons:
  - name: default
    port: 9201
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`
	if err := os.WriteFile(filepath.Join(srvDir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
}

// resetResolverSnapshot clears the package-level published snapshot so a
// test starts from the dormant (nil) production state and a prior test's
// publish cannot leak in. Uses the exported publish entry point with a
// nil store to reset, then verifies via Load.
func resetResolverSnapshot(t *testing.T) {
	t.Helper()
	// Every caller of this helper drives a REAL hub-listener start or the real
	// publish seam, and both walk the client registry (api.ResetHubPortContext →
	// ProbeHubGate → GetEntry on every constructed adapter,
	// internal/api/hub_gate_detect.go:56). None of them redirected the
	// client-config roots, so they read the operator's live configs. Installing
	// the sandbox HERE rather than in each caller is what keeps the next
	// real-listener test in this family safe by default.
	sandboxClientConfigHome(t)
	api.PublishResolverSnapshot(nil)
	if api.LoadResolverSnapshot() != nil {
		t.Fatalf("test setup: snapshot non-nil after reset")
	}
}

// TestGroupsPhase3_PublishSeamPopulatesSnapshotFromManifests drives the
// REAL publish seam and asserts it publishes a non-nil snapshot whose
// Bindings carry the seeded client_bindings row. This FAILS before the
// seam is wired (publishResolverSnapshotForHubBind undefined) and PASSES
// after.
func TestGroupsPhase3_PublishSeamPopulatesSnapshotFromManifests(t *testing.T) {
	seedManifestDir(t)
	resetResolverSnapshot(t)
	stateDir := apitest.HardenedTempDir(t)
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreState)
	startReadySupervisorFixture(t, stateDir, nil)

	a := api.NewAPI()
	if err := publishResolverSnapshotForHubBind(context.Background(), a); err != nil {
		t.Fatalf("publishResolverSnapshotForHubBind: %v", err)
	}

	snap := api.LoadResolverSnapshot()
	if snap == nil {
		t.Fatal("snapshot is nil after the publish seam ran — the dormant-aggregate gap is not closed")
	}
	binds, ok := snap.Bindings["claude-code"]
	if !ok {
		t.Fatalf("snapshot.Bindings missing claude-code key; bindings=%+v", snap.Bindings)
	}
	if len(binds) != 1 {
		t.Fatalf("claude-code bindings=%d want 1: %+v", len(binds), binds)
	}
}

// TestGroupsPhase3_GateOffDoesNotPublish pins the no-regression
// requirement: with the gate OFF, startHubMcpListener short-circuits
// BEFORE the publish seam, so no snapshot is published.
func TestGroupsPhase3_GateOffDoesNotPublish(t *testing.T) {
	seedManifestDir(t)
	resetResolverSnapshot(t)

	a := api.NewAPI()
	bundle, err := startHubMcpListener(context.Background(), false, a)
	if err != nil {
		t.Fatalf("startHubMcpListener (gate OFF) err: %v", err)
	}
	if bundle != nil {
		ShutdownHubListener(context.Background(), bundle)
		t.Fatalf("bundle non-nil with gate OFF")
	}
	if api.LoadResolverSnapshot() != nil {
		t.Errorf("snapshot published despite gate OFF — gate-OFF path must stay inert")
	}
}

package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/config"
)

// TestSeedOverlayFromDiscoveryWritesAutoDiscoveryRow seeds a synthetic
// temp dir with one fake binary and a fabricated global manifest
// declaring RequiredBinaries: [clangd]. The seeder must write an
// overlay row keyed by the canonical SupervisorDaemon.TaskName
// (\mcp-local-hub-clangd-default) with source: auto-discovery and a
// Path env value that joins the bin dir to ${parent_path} via the
// OS-correct PathListSeparator.
func TestSeedOverlayFromDiscoveryWritesAutoDiscoveryRow(t *testing.T) {
	binDir := apitest.HardenedTempDir(t)
	binName := "clangd"
	if runtime.GOOS == "windows" {
		binName = "clangd.exe"
	}
	binPath := filepath.Join(binDir, binName)
	if err := os.WriteFile(binPath, []byte{0x4d, 0x5a}, 0o755); err != nil {
		t.Fatalf("seed fake binary: %v", err)
	}

	manifest := &config.ServerManifest{
		Name:             "clangd",
		Kind:             "global",
		Transport:        "stdio-bridge",
		Command:          "clangd",
		RequiredBinaries: []string{"clangd"},
	}

	stateDir := apitest.HardenedTempDir(t)
	overlayPath := filepath.Join(stateDir, "daemon-env-overrides.yaml")

	ctx := context.Background()
	err := seedOverlayFromDiscovery(ctx, []*config.ServerManifest{manifest},
		overlayPath, []string{binDir})
	if err != nil {
		t.Fatalf("seedOverlayFromDiscovery: %v", err)
	}

	ov, err := daemon_env_overlay.Load(overlayPath)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	wantKey := `\mcp-local-hub-clangd-default`
	row, ok := ov.Daemons[wantKey]
	if !ok {
		got := make([]string, 0, len(ov.Daemons))
		for k := range ov.Daemons {
			got = append(got, k)
		}
		t.Fatalf("missing key %q in overlay; got %v", wantKey, got)
	}
	if row.Source != "auto-discovery" {
		t.Fatalf("Source = %q, want auto-discovery", row.Source)
	}
	// Auto-discovery rows MUST use the uppercase "PATH" key — POSIX
	// merges by exact case in mergeDaemonEnv (supervise.go:1664), so a
	// `Path` key would fail to override the parent's `PATH`. Bot review
	// PR #222 P1 finding (install_overlay_seed.go:199).
	gotPath, ok := row.Env["PATH"]
	if !ok {
		got := make([]string, 0, len(row.Env))
		for k := range row.Env {
			got = append(got, k)
		}
		t.Fatalf("env missing PATH key (got keys: %v)", got)
	}
	if !strings.Contains(gotPath, binDir) {
		t.Fatalf("PATH %q should contain binDir %q", gotPath, binDir)
	}
	if !strings.Contains(gotPath, "${parent_path}") {
		t.Fatalf("PATH %q should include ${parent_path} token", gotPath)
	}
	wantSep := string(os.PathListSeparator)
	if !strings.Contains(gotPath, wantSep+"${parent_path}") {
		t.Fatalf("PATH %q should join binDir to ${parent_path} via OS separator %q", gotPath, wantSep)
	}
}

// TestSeedOverlayFromDiscoveryPreservesOperatorRow seeds a temp overlay
// with an existing source: operator row and verifies that running the
// seeder against the same task name LEAVES that row untouched (CAS).
func TestSeedOverlayFromDiscoveryPreservesOperatorRow(t *testing.T) {
	binDir := apitest.HardenedTempDir(t)
	binName := "clangd"
	if runtime.GOOS == "windows" {
		binName = "clangd.exe"
	}
	if err := os.WriteFile(filepath.Join(binDir, binName), []byte{0x4d, 0x5a}, 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	stateDir := apitest.HardenedTempDir(t)
	overlayPath := filepath.Join(stateDir, "daemon-env-overrides.yaml")

	// Pre-seed an operator row.
	const operatorPath = "C:/operator/custom/bin"
	pre := daemon_env_overlay.WriteOverlay(overlayPath, func(o *daemon_env_overlay.Overlay) error {
		o.Daemons[`\mcp-local-hub-clangd-default`] = daemon_env_overlay.DaemonRow{
			Env:    map[string]string{"Path": operatorPath},
			Source: "operator",
		}
		return nil
	})
	if pre != nil {
		t.Fatalf("pre-seed operator row: %v", pre)
	}

	manifest := &config.ServerManifest{
		Name:             "clangd",
		Kind:             "global",
		Transport:        "stdio-bridge",
		Command:          "clangd",
		RequiredBinaries: []string{"clangd"},
	}

	err := seedOverlayFromDiscovery(context.Background(),
		[]*config.ServerManifest{manifest}, overlayPath, []string{binDir})
	if err != nil {
		t.Fatalf("seedOverlayFromDiscovery: %v", err)
	}

	ov, err := daemon_env_overlay.Load(overlayPath)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	row := ov.Daemons[`\mcp-local-hub-clangd-default`]
	if row.Source != "operator" {
		t.Fatalf("Source = %q, want operator (CAS must preserve)", row.Source)
	}
	if row.Env["Path"] != operatorPath {
		t.Fatalf("Path = %q, want %q (operator row must be unchanged)", row.Env["Path"], operatorPath)
	}
}

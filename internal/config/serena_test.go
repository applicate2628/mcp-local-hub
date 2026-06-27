package config

import (
	"os"
	"testing"
)

func TestSerenaManifestParses(t *testing.T) {
	f, err := os.Open("../../servers/serena/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := ParseManifest(f)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "serena" {
		t.Errorf("Name = %q", m.Name)
	}
	// Router-native dynamic-pool layout (area-4 flip): the shipped serena catalog
	// is kind: workspace-scoped with a daemon_template and NO static daemons[],
	// so NEW installs are router-native from the start (clients point at the
	// /serena/mcp router; the supervisor spawns one serena child PER WORKSPACE
	// from the template). This replaced the unified-intermediate shape (one
	// static `unified` daemon on --context codex at port 9121). See
	// servers/serena/manifest.yaml for the why-unified-on-codex rationale
	// (resource savings + search_for_pattern/activate_project for every agent).
	// EffectiveSerenaDaemonTemplate (internal/api/serena_dynamic_pool.go) now
	// reads its template from this embed verbatim instead of falling back to its
	// built-in default; the dynamic-pool synthesizer is an identity projection.
	if m.Kind != KindWorkspaceScoped {
		t.Errorf("Kind = %q, want %q (router-native dynamic-pool catalog)", m.Kind, KindWorkspaceScoped)
	}
	if len(m.Daemons) != 0 {
		t.Errorf("len(Daemons) = %d, want 0 (dynamic-pool: no static daemons[])", len(m.Daemons))
	}
	if m.DaemonTemplate == nil {
		t.Fatalf("DaemonTemplate = nil, want a dynamic-pool template (context/port_pool/extra_args_template)")
	}
	// The embed's daemon_template MUST match EffectiveSerenaDaemonTemplate's
	// built-in default EXACTLY so the synthesizer output stays byte-identical to
	// the prior unified-intermediate projection (the embed-wins template == the
	// prior default). The owner of those defaults is internal/api; this test pins
	// the literal YAML values so a drift on either side is caught.
	if m.DaemonTemplate.Context != "codex" {
		t.Errorf("DaemonTemplate.Context = %q, want %q", m.DaemonTemplate.Context, "codex")
	}
	if m.DaemonTemplate.PortPool == nil {
		t.Fatalf("DaemonTemplate.PortPool = nil, want {9150,9199}")
	}
	if m.DaemonTemplate.PortPool.Start != 9150 || m.DaemonTemplate.PortPool.End != 9199 {
		t.Errorf("DaemonTemplate.PortPool = {%d,%d}, want {9150,9199}",
			m.DaemonTemplate.PortPool.Start, m.DaemonTemplate.PortPool.End)
	}
	wantExtraArgs := []string{"--project", WorkspacePathToken}
	if len(m.DaemonTemplate.ExtraArgsTemplate) != len(wantExtraArgs) {
		t.Errorf("DaemonTemplate.ExtraArgsTemplate = %v, want %v", m.DaemonTemplate.ExtraArgsTemplate, wantExtraArgs)
	} else {
		for i, a := range wantExtraArgs {
			if m.DaemonTemplate.ExtraArgsTemplate[i] != a {
				t.Errorf("DaemonTemplate.ExtraArgsTemplate[%d] = %q, want %q", i, m.DaemonTemplate.ExtraArgsTemplate[i], a)
			}
		}
	}
	// The dynamic-pool catalog carries NO client_bindings: clients route through
	// the /serena/mcp router (established by `mcphub migrate serena
	// legacy-to-dynamic-pool` / the reconcile owner), not per-daemon bindings. A
	// binding referencing a now-absent named daemon would also break the generic
	// BuildPlanWithOpts install path (findDaemon would fail). Pin the absence.
	if len(m.ClientBindings) != 0 {
		t.Errorf("len(ClientBindings) = %d, want 0 (router-native: no per-daemon bindings on the dynamic-pool catalog)", len(m.ClientBindings))
	}

	// PYTHONUNBUFFERED=1 must reach the serena child env so Python
	// flushes stdout/stderr per-line into the rotated log file.
	// Without it, the codex daemon's silent crashes leave no
	// traceback (Python's 4 KB block-buffer never flushes before
	// exit). Codex CLI review on PR #34 — make this contract a
	// regression-guarded invariant, not a manifest comment that
	// might silently get dropped on a future edit.
	if got := m.Env["PYTHONUNBUFFERED"]; got != "1" {
		t.Errorf("manifest env PYTHONUNBUFFERED = %q, want \"1\" (required for line-buffered Python stderr → log diagnostics)", got)
	}
}

func TestPortsRegistryValid(t *testing.T) {
	f, err := os.Open("../../configs/ports.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := ParsePortRegistry(f); err != nil {
		t.Fatalf("ParsePortRegistry: %v", err)
	}
}

// TestPortsRegistryCoversAllShippedManifests guards that every (server,
// daemon, port) tuple declared in a shipped manifest.yaml has a matching
// entry in configs/ports.yaml. Without this the registry drifts: lldb
// and perftools manifests existed for weeks before ports.yaml caught up,
// so the registry was technically valid (no parse error) but not actually
// the source of truth it was supposed to be.
func TestPortsRegistryCoversAllShippedManifests(t *testing.T) {
	regFile, err := os.Open("../../configs/ports.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer regFile.Close()
	reg, err := ParsePortRegistry(regFile)
	if err != nil {
		t.Fatalf("ParsePortRegistry: %v", err)
	}

	// Index registry: (server, daemon) → port.
	regIndex := map[string]int{}
	for _, g := range reg.Global {
		regIndex[g.Server+"/"+g.Daemon] = g.Port
	}

	// Walk shipped manifests on disk (the test runs from internal/config so
	// the source tree is reachable via ../../servers/).
	serversDir := "../../servers"
	entries, err := os.ReadDir(serversDir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", serversDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mPath := serversDir + "/" + e.Name() + "/manifest.yaml"
		mFile, err := os.Open(mPath)
		if err != nil {
			continue // not a server dir
		}
		m, parseErr := ParseManifest(mFile)
		mFile.Close()
		if parseErr != nil {
			t.Errorf("ParseManifest %s: %v", mPath, parseErr)
			continue
		}
		// kind=workspace_scoped uses pool, not registry entry — skip.
		// (Currently no workspace_scoped manifests; this is forward-looking.)
		if m.Kind != KindGlobal {
			continue
		}
		for _, d := range m.Daemons {
			key := m.Name + "/" + d.Name
			port, ok := regIndex[key]
			if !ok {
				t.Errorf("ports.yaml missing entry for %s (manifest declares port %d)", key, d.Port)
				continue
			}
			if port != d.Port {
				t.Errorf("ports.yaml has %s=%d but manifest declares port %d", key, port, d.Port)
			}
		}
	}
}

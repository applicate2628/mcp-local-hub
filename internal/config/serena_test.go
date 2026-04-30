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
	// Shared-daemon layout: claude (claude-code context, shared by Claude
	// Code, Gemini CLI, and Antigravity) + codex (separate, Codex-specific).
	// Antigravity connects via `mcp relay` subprocess (stdio→HTTP bridge)
	// because its Cascade agent rejects direct loopback-HTTP entries.
	if len(m.Daemons) != 2 {
		t.Errorf("len(Daemons) = %d, want 2 (claude + codex)", len(m.Daemons))
	}
	if len(m.ClientBindings) != 4 {
		t.Errorf("len(ClientBindings) = %d, want 4 (claude-code, codex-cli, antigravity, gemini-cli)", len(m.ClientBindings))
	}
	// Claude Code, Gemini CLI, and Antigravity all route to the "claude" daemon.
	// Antigravity gets there via a stdio-relay spawn; the other two via HTTP.
	sharedClaude := map[string]bool{"claude-code": false, "gemini-cli": false, "antigravity": false}
	for _, b := range m.ClientBindings {
		if _, ok := sharedClaude[b.Client]; ok {
			if b.Daemon != "claude" {
				t.Errorf("binding %s.daemon = %q, want claude (shared daemon)", b.Client, b.Daemon)
			}
			sharedClaude[b.Client] = true
		}
	}
	for client, seen := range sharedClaude {
		if !seen {
			t.Errorf("binding for client %q not found", client)
		}
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

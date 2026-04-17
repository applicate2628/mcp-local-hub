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

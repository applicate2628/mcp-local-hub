package api

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

// §9.2 FAMILY-B drift-prevention coverage. These tests pin the property
// that makes the refactor worth doing: the scan/probe per-client set DERIVES
// from the canonical registry (clients.SupportedClientNames() +
// clients.ConfigPathForName), so a NEW registry client is auto-covered with
// ZERO scan.go edits. Each test fails loudly if a future change re-introduces
// the hardcoded-named-field drift.

// TestEffectiveConfigPaths_FoldsNamedFieldsAndMap verifies the single
// derivation point merges the back-compat named fields with the
// registry-derived ConfigPaths map, with ConfigPaths winning on conflict and
// empty paths dropped (the "absent when empty" contract the probe relies on).
func TestEffectiveConfigPaths_FoldsNamedFieldsAndMap(t *testing.T) {
	opts := ScanOpts{
		// Legacy named field only.
		ClaudeConfigPath: "/named/claude.json",
		// Named field that ConfigPaths will override.
		CodexConfigPath: "/named/codex.toml",
		// Named field left empty → must be dropped, not present as "".
		CursorConfigPath: "",
		ConfigPaths: map[string]string{
			// Override the codex named field.
			"codex-cli": "/map/codex.toml",
			// A registry client with NO named ScanOpts field — reachable
			// ONLY through the map. This is the drift the refactor closes.
			"copilot-cli": "/map/copilot.json",
			// Explicit "" must NOT add a key.
			"vscode": "",
		},
	}
	got := opts.effectiveConfigPaths()

	want := map[string]string{
		"claude-code": "/named/claude.json",
		"codex-cli":   "/map/codex.toml", // map wins over named field
		"copilot-cli": "/map/copilot.json",
	}
	if len(got) != len(want) {
		t.Fatalf("effectiveConfigPaths() = %v, want %v (len mismatch)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("effectiveConfigPaths()[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["cursor"]; ok {
		t.Errorf("empty named field must be dropped; cursor present = %q", got["cursor"])
	}
	if _, ok := got["vscode"]; ok {
		t.Errorf("explicit empty ConfigPaths value must not add a key; vscode present = %q", got["vscode"])
	}
}

// TestConfigPaths_AutoCoversRegistryClientViaMapOnly is the core anti-drift
// proof: a registry client supplied ONLY via the ConfigPaths map (no named
// ScanOpts field) is both presence-probed AND, when it has a clientScanners
// entry, entry-scanned — with no per-client code in probeClientConfigPresence
// or ScanFrom. We use "zed" because it has a scanner; the property
// generalizes to any future registry client given a scanner.
func TestConfigPaths_AutoCoversRegistryClientViaMapOnly(t *testing.T) {
	tmp := t.TempDir()
	zedPath := filepath.Join(tmp, "zed-settings.json")
	if err := os.WriteFile(zedPath, []byte(`{"context_servers":{"memory":{"command":"D:/dev/mcphub.exe","args":["relay","--url","http://localhost:9123/mcp"]}}}`), 0o600); err != nil {
		t.Fatalf("write zed settings: %v", err)
	}
	manifestDir := filepath.Join(tmp, "servers")
	if err := os.MkdirAll(filepath.Join(manifestDir, "memory"), 0o755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	a := NewAPI()
	// Supplied ONLY via the map — no ZedConfigPath named field.
	result, err := a.ScanFrom(ScanOpts{
		ConfigPaths: map[string]string{"zed": zedPath},
		ManifestDir: manifestDir,
	})
	if err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}
	var mem *ScanEntry
	for i := range result.Entries {
		if result.Entries[i].Name == "memory" {
			mem = &result.Entries[i]
		}
	}
	if mem == nil {
		t.Fatal("zed entry supplied via ConfigPaths map was not scanned (drift: map path ignored)")
	}
	if got := mem.ClientPresence["zed"].Transport; got != "relay" {
		t.Errorf("zed.Transport via map-only path: got %q, want relay", got)
	}
	// Presence probe must also see the map-only client.
	if got := result.ClientConfigPresence["zed"]; got != "ok" {
		t.Errorf("ClientConfigPresence[zed] via map-only path: got %q, want ok", got)
	}
}

// TestDefaultScanConfigPaths_CoversEverySupportedClient is the guard that
// fails if DefaultScanConfigPaths ever stops covering the full registry — the
// exact drift symptom (a registry client invisible to scan). Every
// SupportedClientNames() entry whose ConfigPathForName resolves must appear
// in the map. On a normal test host UserHomeDir resolves, so all of them must
// be present.
func TestDefaultScanConfigPaths_CoversEverySupportedClient(t *testing.T) {
	paths := DefaultScanConfigPaths()
	for _, name := range clients.SupportedClientNames() {
		wantPath, err := clients.ConfigPathForName(name)
		if err != nil {
			// Resolver failed for this client on this host — it is
			// legitimately omitted. Nothing to assert.
			continue
		}
		got, ok := paths[name]
		if !ok {
			t.Errorf("DefaultScanConfigPaths() missing registry client %q (§9.2 drift: client would be invisible to scan)", name)
			continue
		}
		if got != wantPath {
			t.Errorf("DefaultScanConfigPaths()[%q] = %q, want %q (must match the write-surface resolver)", name, got, wantPath)
		}
	}
}

// TestClientScanners_AreAllRegistryClients pins that the scanner registry has
// no orphan entries: every client id in clientScanners() is a real registry
// client. (The reverse — a registry client without a scanner — is allowed and
// intentional: it is presence-probed but not entry-scanned until a shape
// parser is written.) An orphan scanner key would be dead code that never
// dispatches because the ScanFrom loop is driven by SupportedClientNames().
func TestClientScanners_AreAllRegistryClients(t *testing.T) {
	supported := map[string]bool{}
	for _, name := range clients.SupportedClientNames() {
		supported[name] = true
	}
	for name, sc := range clientScanners() {
		if !supported[name] {
			t.Errorf("clientScanners() has key %q that is not a SupportedClientNames() client (dead/never-dispatched entry)", name)
		}
		if sc.scan == nil {
			t.Errorf("clientScanners()[%q] has nil scan func", name)
		}
		if sc.prefix == "" {
			t.Errorf("clientScanners()[%q] has empty error prefix", name)
		}
	}
}

// TestScanCoversTier1Clients verifies the skills-CLI TIER-1 registry clients
// are not merely presence-probed: they also have scanner registrations that
// discover existing MCP entries for migration/reconciliation workflows.
func TestScanCoversTier1Clients(t *testing.T) {
	tmp := t.TempDir()
	manifestDir := filepath.Join(tmp, "servers")
	if err := os.MkdirAll(filepath.Join(manifestDir, "memory"), 0o755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	paths := map[string]string{}
	for _, client := range []string{
		"bob", "codebuddy", "command-code", "cortex", "deepagents", "devin",
		"droid", "firebender", "iflow-cli", "junie", "kimi-code-cli", "kode",
		"ona", "qoder", "qoder-cn", "roo", "rovodev", "tabnine-cli",
	} {
		path := filepath.Join(tmp, client+".json")
		if err := os.WriteFile(path, []byte(`{"mcpServers":{"memory":{"url":"http://localhost:9123/mcp","type":"http"}}}`), 0o600); err != nil {
			t.Fatalf("write %s config: %v", client, err)
		}
		paths[client] = path
	}

	piPath := filepath.Join(tmp, "pi.json")
	if err := os.WriteFile(piPath, []byte(`{"mcpServers":{"memory":{"command":"/opt/mcphub","args":["relay","--url","http://localhost:9123/mcp"]}}}`), 0o600); err != nil {
		t.Fatalf("write pi config: %v", err)
	}
	paths["pi"] = piPath

	result, err := NewAPI().ScanFrom(ScanOpts{ConfigPaths: paths, ManifestDir: manifestDir})
	if err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}
	var mem *ScanEntry
	for i := range result.Entries {
		if result.Entries[i].Name == "memory" {
			mem = &result.Entries[i]
		}
	}
	if mem == nil {
		t.Fatal("tier-1 configs were presence-probed but not entry-scanned: missing memory entry")
	}
	for client := range paths {
		presence, ok := mem.ClientPresence[client]
		if !ok {
			t.Errorf("memory missing ClientPresence[%q] (scanner not registered or not dispatched)", client)
			continue
		}
		wantTransport := "http"
		if client == "pi" {
			wantTransport = "relay"
		}
		if presence.Transport != wantTransport {
			t.Errorf("%s transport = %q, want %q", client, presence.Transport, wantTransport)
		}
		if got := result.ClientConfigPresence[client]; got != "ok" {
			t.Errorf("ClientConfigPresence[%s] = %q, want ok", client, got)
		}
	}
	if mem.Status != "via-hub" {
		t.Errorf("memory Status with tier-1 hub bindings = %q, want via-hub", mem.Status)
	}
}

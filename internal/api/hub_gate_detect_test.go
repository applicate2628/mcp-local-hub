// hub_gate_detect_test.go — B2 footgun: gate-ON detection helper tests.

package api

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// hermeticHome moved to client_config_env_isolation_test.go, which is now the
// single owner of client-config path isolation for package api. It was
// generalized there (the non-home env set is separately callable as
// neutralizeClientConfigPathEnv) because two other fixtures in this package —
// setupAdoptTestEnv and withHermeticHomeForCleanup — own their own directory
// layout and so could not reuse this one, and both were leaking to the
// operator's real configs as a result.

// TestGatedOnClientsDetectsMcphubHubEntry pins B2: a client whose
// on-disk config carries the reserved `mcphub-hub` aggregate entry is
// reported gate-ON by GatedOnClients.
func TestGatedOnClientsDetectsMcphubHubEntry(t *testing.T) {
	home := hermeticHome(t)

	// Seed claude-code's ~/.claude.json with a mcphub-hub aggregate
	// entry — exactly the shape the gate-ON reconciler writes.
	cfg := filepath.Join(home, ".claude.json")
	body := `{"mcpServers":{"mcphub-hub":{"url":"http://127.0.0.1:3439/clients/claude-code/mcp","type":"http"}}}`
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatalf("seed claude-code config: %v", err)
	}

	gated := GatedOnClients()
	if !slices.Contains(gated, "claude-code") {
		t.Fatalf("GatedOnClients() = %v, want it to contain claude-code", gated)
	}
	if !AnyClientGatedOn() {
		t.Errorf("AnyClientGatedOn() = false, want true")
	}
}

// TestGatedOnClientsIgnoresNonHubEntries pins the negative: a client
// config with only ordinary per-server entries (no mcphub-hub
// aggregate) is NOT reported gate-ON. This is the at-rest gate-OFF
// state where --reset-port must remain allowed.
func TestGatedOnClientsIgnoresNonHubEntries(t *testing.T) {
	home := hermeticHome(t)

	cfg := filepath.Join(home, ".claude.json")
	// Ordinary per-daemon entries, no mcphub-hub aggregate.
	body := `{"mcpServers":{"time":{"url":"http://localhost:9128/mcp","type":"http"},"memory":{"url":"http://localhost:9129/mcp","type":"http"}}}`
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatalf("seed claude-code config: %v", err)
	}

	gated := GatedOnClients()
	if slices.Contains(gated, "claude-code") {
		t.Errorf("GatedOnClients() = %v, claude-code is gate-OFF but was reported gate-ON", gated)
	}
	if AnyClientGatedOn() {
		t.Errorf("AnyClientGatedOn() = true, want false on a gate-OFF host")
	}
}

// TestGatedOnClientsEmptyOnFreshHome pins the fresh-host case: no client
// configs exist at all → no client is gate-ON, so --reset-port is
// unguarded (the normal stuck-instance recovery path).
func TestGatedOnClientsEmptyOnFreshHome(t *testing.T) {
	hermeticHome(t)

	if gated := GatedOnClients(); len(gated) != 0 {
		t.Errorf("GatedOnClients() = %v on a fresh home, want empty", gated)
	}
	if AnyClientGatedOn() {
		t.Errorf("AnyClientGatedOn() = true on a fresh home, want false")
	}
}

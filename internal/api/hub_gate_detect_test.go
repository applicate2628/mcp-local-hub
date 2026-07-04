// hub_gate_detect_test.go — B2 footgun: gate-ON detection helper tests.

package api

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// hermeticHome redirects every home/profile env var that an adapter's
// path resolver consults to a fresh temp dir so clients.AllClients()
// resolves into the sandbox — never the developer's real client configs.
// LOCALAPPDATA is set belt-and-suspenders per the repo STATE SAFETY
// rule even though the api TestMain already fences DaemonStateDir.
func hermeticHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// APPDATA (Windows Roaming) MUST be pinned: several client-config resolvers
	// read os.Getenv("APPDATA") FIRST and only fall back to a home-derived path
	// when it is unset — vscode (clients.go defaultVSCodeConfigPath →
	// %APPDATA%\Code\User\mcp.json), amp, cline, devin, kilocode, roo, zed. Without
	// this, the gate-detect walk reads the developer's REAL %APPDATA% client configs
	// and a live mcphub-hub entry leaks in as a false gate-ON (the bug this closes).
	// Mirrors internal/gui/e2e/fixtures/hub.ts, which pins the same set.
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	// Client-specific override env vars that resolve a config path with ABSOLUTE
	// precedence and no home fallback — clear them so a real value in the host env
	// cannot leak a real client config into the hermetic home.
	for _, k := range []string{
		"COPILOT_HOME", "KIMI_CODE_HOME",
		"MIMOCODE_HOME", "MIMOCODE_CONFIG", "MIMOCODE_CONFIG_DIR", "MIMOCODE_CONFIG_CONTENT",
	} {
		t.Setenv(k, "")
	}
	// mimocode's machine-wide managed layer reads %ProgramData%\opencode (or the
	// MIMOCODE_TEST_MANAGED_CONFIG_DIR override) regardless of home — redirect it
	// into the hermetic tree so it reads an empty dir, never a real MDM config.
	t.Setenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR", filepath.Join(home, "ProgramData", "opencode"))
	return home
}

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

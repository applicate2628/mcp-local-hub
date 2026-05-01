package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMigrateReplacesStdioWithHTTPForOneClient verifies that a single-
// client migration rewrites the expected config entry without touching
// other clients.
//
// HOME/USERPROFILE are overridden so that each client adapter's internal
// os.UserHomeDir() call resolves to the tempdir, matching where the test
// wrote the config files. This keeps the adapter contract unchanged
// (production adapters still resolve via UserHomeDir) while giving the
// test a hermetic filesystem layout.
func TestMigrateReplacesStdioWithHTTPForOneClient(t *testing.T) {
	tmp := t.TempDir()

	// Redirect UserHomeDir() to tmp for Claude/Codex/Gemini/Antigravity
	// adapter path resolution on both POSIX (HOME) and Windows (USERPROFILE).
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{"memory":{"command":"npx","args":["-y","@x/memory"]}}}`), 0600); err != nil {
		t.Fatalf("write claude config: %v", err)
	}

	// Codex adapter resolves to ~/.codex/config.toml, so create that subdir.
	codexDir := filepath.Join(tmp, ".codex")
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		t.Fatalf("mkdir codex: %v", err)
	}
	codexPath := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(codexPath, []byte(`[mcp_servers.memory]
command = "npx"
args = ["-y", "@x/memory"]
`), 0600); err != nil {
		t.Fatalf("write codex config: %v", err)
	}

	// Fake manifest so the migration can resolve the daemon port and URL path.
	manifestDir := filepath.Join(tmp, "servers")
	if err := os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte(`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9123
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
weekly_refresh: false
`), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	a := NewAPI()
	report, err := a.MigrateFrom(MigrateOpts{
		Servers:        []string{"memory"},
		ClientsInclude: []string{"claude-code"},
		ScanOpts: ScanOpts{
			ClaudeConfigPath: claudePath,
			CodexConfigPath:  codexPath,
			ManifestDir:      manifestDir,
		},
	})
	if err != nil {
		t.Fatalf("MigrateFrom: %v", err)
	}
	if report == nil {
		t.Fatal("MigrateFrom returned nil report")
	}

	// Claude is now http
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read claude: %v", err)
	}
	var claudeCfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &claudeCfg); err != nil {
		t.Fatalf("unmarshal claude: %v", err)
	}
	if got := claudeCfg.MCPServers["memory"]["type"]; got != "http" {
		t.Errorf("claude memory.type: want http, got %v", got)
	}

	// Codex is unchanged (still has command=npx)
	cod, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex: %v", err)
	}
	if !strings.Contains(string(cod), `command = "npx"`) {
		t.Errorf("codex was unexpectedly migrated: %s", cod)
	}
}

// TestMigrateRotatesBackupsToKeepN verifies that MigrateFrom honors the
// user's `backups.keep_n` setting: a fresh timestamped backup is written
// AND older timestamped backups beyond keepN are pruned in place. The
// pristine `-original` sentinel must always survive the prune.
//
// Audit gap (phase-3b-ii backlog Round 1 #4): the registry exposed
// backups.keep_n with default 5, but no caller ever consumed it on the
// install or migrate write paths — so the setting was decorative and
// backup files accumulated unbounded across migrate cycles.
func TestMigrateRotatesBackupsToKeepN(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	// Redirect SettingsPath() to tmp so the test's SettingsSet doesn't
	// touch the developer machine's gui-preferences.yaml. SettingsPath
	// prefers LOCALAPPDATA, then XDG_DATA_HOME, then $HOME/.local/share.
	// Setting LOCALAPPDATA wins on every platform.
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "appdata"))

	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{"memory":{"command":"npx","args":["-y","@x/memory"]}}}`), 0600); err != nil {
		t.Fatalf("write claude config: %v", err)
	}

	// Pre-existing timestamped backups: 5 historical copies plus the
	// pristine -original sentinel. Stamp mtimes far apart so the prune
	// algorithm has unambiguous "newest" ordering. The sentinel goes
	// somewhere arbitrary in the past; the prune algorithm must NOT
	// touch it regardless.
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	if err := os.WriteFile(sentinel, []byte(`{"original":true}`), 0600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	now := time.Now()
	for i := 0; i < 5; i++ {
		ts := now.Add(time.Duration(-(i + 1)) * time.Hour).Format("20060102-150405")
		p := claudePath + ".bak-mcp-local-hub-" + ts
		if err := os.WriteFile(p, []byte(`{"old":true}`), 0600); err != nil {
			t.Fatalf("write old backup %d: %v", i, err)
		}
		// Set mtime so pruneOldTimestamped sees the intended ordering.
		mt := now.Add(time.Duration(-(i + 1)) * time.Hour)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatalf("chtimes old backup %d: %v", i, err)
		}
	}

	// Persist backups.keep_n=2 so MigrateFrom -> BackupKeep(2) prunes
	// down to exactly 2 timestamped (1 fresh from migrate + 1 oldest-
	// kept).
	a := NewAPI()
	if err := a.SettingsSet("backups.keep_n", "2"); err != nil {
		t.Fatalf("SettingsSet keep_n: %v", err)
	}

	// Manifest the migrate path needs.
	manifestDir := filepath.Join(tmp, "servers")
	if err := os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte(`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9123
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
weekly_refresh: false
`), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if _, err := a.MigrateFrom(MigrateOpts{
		Servers:        []string{"memory"},
		ClientsInclude: []string{"claude-code"},
		ScanOpts: ScanOpts{
			ClaudeConfigPath: claudePath,
			ManifestDir:      manifestDir,
		},
	}); err != nil {
		t.Fatalf("MigrateFrom: %v", err)
	}

	// Count timestamped backups (excluding -original sentinel) and
	// assert sentinel survival.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	prefix := filepath.Base(claudePath) + ".bak-mcp-local-hub-"
	timestamped := 0
	sentinelFound := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if strings.HasSuffix(name, "-original") {
			sentinelFound = true
			continue
		}
		timestamped++
	}
	if !sentinelFound {
		t.Errorf("pristine -original sentinel was pruned (must always survive)")
	}
	// keep_n=2 → at most 2 timestamped backups remain. We pre-seeded 5
	// and migrate added 1 fresh, so the prune saw 6 candidates and
	// dropped 4. Allow either 2 (new + 1 kept) or 1 (Windows second-
	// resolution mtime collision collapses fresh into oldest-kept).
	if timestamped < 1 || timestamped > 2 {
		t.Errorf("backups.keep_n=2 should leave 1-2 timestamped backups, got %d", timestamped)
	}
}

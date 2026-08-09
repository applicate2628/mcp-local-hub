package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
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
	// Phase 5 Task 5.1: adapter writes route through
	// SecureWriteClientConfig in production. %TEMP%-backed t.TempDir()
	// fails the parent-dir DACL gate on Windows; install the test
	// fallback so this legacy-flow test keeps working.
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()

	// Redirect UserHomeDir() to tmp for Claude/Codex/Gemini/Antigravity
	// adapter path resolution on both POSIX (HOME) and Windows (USERPROFILE).
	sandboxClientConfigHome(t, tmp)

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
	// Phase 5 Task 5.1: route adapter writes through the fallback so
	// the t.TempDir() parent dir's wide DACL doesn't trip the secure-
	// write gate on Windows.
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	sandboxClientConfigHome(t, tmp)
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

// TestMigrateSetsRelayExePathForZed proves the migrate-path relay-context
// gate (migrate.go ~line 158, `clients.IsRelayStdio(binding.Client)`) fires
// for the zed client, not just antigravity. zed is a relay-stdio adapter:
// its AddEntry rejects an entry whose RelayExePath is empty. Before the
// 4th-site fix the gate was hardcoded to `binding.Client == "antigravity"`,
// so a zed binding flowed through with RelayExePath unset and zed.AddEntry
// rejected it — the row landed in report.Failed. This test asserts the
// written zed config carries the canonical mcphub path as the relay
// `command`, which can only happen if the gate set entry.RelayExePath for
// the zed binding.
//
// Hermeticity / live-host safety: this host has a REAL Zed config at
// %APPDATA%\Zed\settings.json. zed's adapter resolves its ConfigPath from
// %APPDATA% (Windows) at adapter-construction time inside MigrateFrom's
// allClients map — the ScanOpts.ZedConfigPath field is scan-only and is NOT
// consumed by the write path (see migrate.go MigrateOpts doc). So we MUST
// redirect every env path source zed could resolve from (HOME, USERPROFILE,
// APPDATA, LOCALAPPDATA, XDG_CONFIG_HOME) to the test tmpdir BEFORE NewAPI()
// / MigrateFrom, and pre-create the zed config under the redirected APPDATA
// so adapter.Exists() returns true (migrate skips non-existent clients).
// canonicalMcphubPath is overridden to a deterministic tmp path so the
// asserted RelayExePath is stable, not host-dependent.
func TestMigrateSetsRelayExePathForZed(t *testing.T) {
	// %TEMP%-backed t.TempDir() fails the parent-dir DACL gate on Windows;
	// install the test fallback so the adapter write succeeds (same as the
	// single-client legacy-flow test above).
	t.Cleanup(SetClientWriteFallbackForTest())

	// LIVE-HOST HERMETICITY: the secure-write init-stub gate
	// (client_write_init.go) consults strict mode, and strict mode is TRUE
	// when the persisted supervisor-intent.json carries strict_mode=true —
	// read through a process-lifetime lazy cache (strictModeFromIntentCached).
	// On a host whose real supervisor-intent.json has strict_mode=true, that
	// value would bleed into this test and make the strict parent-dir DACL
	// gate refuse the zed config write on a %TEMP%-backed dir (whose parent
	// grants Authenticated Users). Defeat it exactly like the strict-mode-
	// sensitive tests do: redirect the state dir to an EMPTY hardened temp
	// dir (no intent file → strict-from-intent resolves FALSE via the
	// absent-intent relax branch), make the read-gate inert, reset the lazy
	// cache up front + on cleanup, and pin the strict env var OFF.
	t.Cleanup(SetDaemonStateRootForTest(apitest.HardenedTempDir(t)))
	t.Setenv(AllowUnhardenedStateReadEnv, "1")
	t.Setenv(RequireSingleUserHomeEnv, "")
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// Host the redirected env tree under a HARDENED (owner-only, PROTECTED)
	// dir so the zed config's parent (<tmp>/Zed) carries no Authenticated-
	// Users ACE — the secure-write parent-dir gate then passes cleanly under
	// both strict and relax postures, independent of host state.
	tmp := apitest.HardenedTempDir(t)

	// Redirect EVERY env path source zed (and the other adapters) could use,
	// on both Windows (USERPROFILE/APPDATA/LOCALAPPDATA) and POSIX
	// (HOME/XDG_CONFIG_HOME). zed's defaultZedConfigPath reads APPDATA first
	// on Windows, then HOME; XDG_CONFIG_HOME/HOME on POSIX.
	sandboxClientConfigHome(t, tmp)
	t.Setenv("APPDATA", tmp)
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)

	// Deterministic canonical mcphub path so RelayExePath is a stable
	// asserted value, not whatever ~/.local/bin resolves to on this host.
	canonicalMcphub := filepath.Join(tmp, "canonical", "mcphub.exe")
	if err := os.MkdirAll(filepath.Dir(canonicalMcphub), 0o755); err != nil {
		t.Fatalf("mkdir canonical dir: %v", err)
	}
	t.Cleanup(SetTestCanonicalMcphubPath(canonicalMcphub))

	// Pre-create the zed config at the path zed resolves under the redirected
	// APPDATA — %APPDATA%\Zed\settings.json, i.e. <tmp>/Zed/settings.json
	// (defaultZedConfigPath, zed.go). Without this file, adapter.Exists()
	// returns false and migrate skips zed quietly (no Applied, no Failed row).
	zedConfigPath := filepath.Join(tmp, "Zed", "settings.json")
	if err := os.MkdirAll(filepath.Dir(zedConfigPath), 0o755); err != nil {
		t.Fatalf("mkdir zed dir: %v", err)
	}
	if err := os.WriteFile(zedConfigPath, []byte("{\n  \"context_servers\": {}\n}\n"), 0o600); err != nil {
		t.Fatalf("write zed seed config: %v", err)
	}

	// Fake manifest with a zed client binding.
	manifestDir := filepath.Join(tmp, "servers")
	if err := os.MkdirAll(filepath.Join(manifestDir, "memory"), 0o755); err != nil {
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
  - client: zed
    daemon: default
    url_path: /mcp
weekly_refresh: false
`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	a := NewAPI()
	report, err := a.MigrateFrom(MigrateOpts{
		Servers:        []string{"memory"},
		ClientsInclude: []string{"zed"},
		ScanOpts: ScanOpts{
			ManifestDir:   manifestDir,
			ZedConfigPath: zedConfigPath, // scan-only field; harmless here.
		},
	})
	if err != nil {
		t.Fatalf("MigrateFrom: %v", err)
	}
	if report == nil {
		t.Fatal("MigrateFrom returned nil report")
	}

	// zed must be in Applied, NOT Failed. A Failed row means the gate did not
	// fire (RelayExePath empty → zed.AddEntry rejected) — the exact bug this
	// 4th-site fix closes.
	if len(report.Failed) != 0 {
		t.Fatalf("zed migrate reported Failed rows (gate did not set RelayExePath?): %+v", report.Failed)
	}
	var zedApplied bool
	for _, ap := range report.Applied {
		if ap.Client == "zed" && ap.Server == "memory" {
			zedApplied = true
		}
	}
	if !zedApplied {
		t.Fatalf("expected an Applied row for (memory, zed); got Applied=%+v", report.Applied)
	}

	// Read the written zed config; assert it carries the canonical mcphub
	// path as the relay `command` under context_servers.memory. This is the
	// RelayExePath proof — it is present iff the gate set entry.RelayExePath.
	data, err := os.ReadFile(zedConfigPath)
	if err != nil {
		t.Fatalf("read zed config: %v", err)
	}
	var zedCfg struct {
		ContextServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"context_servers"`
	}
	if err := json.Unmarshal(data, &zedCfg); err != nil {
		t.Fatalf("unmarshal zed config %q: %v", string(data), err)
	}
	mem, ok := zedCfg.ContextServers["memory"]
	if !ok {
		t.Fatalf("zed config has no context_servers.memory entry: %s", data)
	}
	if mem.Command != canonicalMcphub {
		t.Errorf("zed relay command (RelayExePath proof): got %q, want canonical %q", mem.Command, canonicalMcphub)
	}
	// The relay args must forward to the hub HTTP URL via `relay --url`.
	wantURL := "http://127.0.0.1:9123/mcp"
	var sawRelay, sawURL bool
	for i, arg := range mem.Args {
		if arg == "relay" {
			sawRelay = true
		}
		if arg == "--url" && i+1 < len(mem.Args) && mem.Args[i+1] == wantURL {
			sawURL = true
		}
	}
	if !sawRelay || !sawURL {
		t.Errorf("zed relay args: got %v, want [relay --url %s]", mem.Args, wantURL)
	}
}

// TestMigrateNonBindingClientSynthesizesEntry proves the Servers-matrix
// fix: a client that is NOT in the manifest's static client_bindings, but
// is explicitly targeted via ClientsInclude, gets a synthesized binding so
// Apply actually writes the hub-HTTP entry (previously a silent no-op).
//
// gemini-cli is the non-binding target — the manifest below binds only
// claude-code. The synthesized binding points at the primary daemon
// (named "default") with the canonical /mcp path, so gemini's config must
// end up with http://localhost:<port>/mcp.
func TestMigrateNonBindingClientSynthesizesEntry(t *testing.T) {
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	sandboxClientConfigHome(t, tmp)

	// Pre-create gemini's config so adapter.Exists() returns true (migrate
	// skips non-existent clients). gemini resolves ~/.gemini/settings.json.
	geminiDir := filepath.Join(tmp, ".gemini")
	if err := os.MkdirAll(geminiDir, 0755); err != nil {
		t.Fatalf("mkdir gemini: %v", err)
	}
	geminiPath := filepath.Join(geminiDir, "settings.json")
	if err := os.WriteFile(geminiPath, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatalf("write gemini config: %v", err)
	}

	manifestDir := filepath.Join(tmp, "servers")
	if err := os.MkdirAll(filepath.Join(manifestDir, "fetch"), 0755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	// Manifest binds ONLY claude-code — gemini-cli is NOT a binding.
	if err := os.WriteFile(filepath.Join(manifestDir, "fetch", "manifest.yaml"),
		[]byte(`name: fetch
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9133
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
weekly_refresh: false
`), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	a := NewAPI()
	report, err := a.MigrateFrom(MigrateOpts{
		Servers:        []string{"fetch"},
		ClientsInclude: []string{"gemini-cli"}, // non-binding target
		ScanOpts:       ScanOpts{ManifestDir: manifestDir},
	})
	if err != nil {
		t.Fatalf("MigrateFrom: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	var applied bool
	for _, ap := range report.Applied {
		if ap.Client == "gemini-cli" && ap.Server == "fetch" {
			applied = true
			if ap.URL != "http://127.0.0.1:9133/mcp" {
				t.Errorf("synthesized URL = %q, want http://127.0.0.1:9133/mcp", ap.URL)
			}
		}
	}
	if !applied {
		t.Fatalf("expected an Applied row for (fetch, gemini-cli); got Applied=%+v", report.Applied)
	}

	// Gemini config must now carry the hub-HTTP entry at the current port.
	data, err := os.ReadFile(geminiPath)
	if err != nil {
		t.Fatalf("read gemini: %v", err)
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal gemini: %v", err)
	}
	mem, ok := cfg.MCPServers["fetch"]
	if !ok {
		t.Fatalf("gemini config has no fetch entry: %s", data)
	}
	if mem["url"] != "http://127.0.0.1:9133/mcp" {
		t.Errorf("gemini fetch.url = %v, want http://127.0.0.1:9133/mcp", mem["url"])
	}
}

// TestMigrateNonBindingClientOverwritesStalePortEntry proves the migrate
// AddEntry overwrites a pre-existing stale-port hub entry on a targeted
// non-binding client with the correct CURRENT manifest port. This is the
// compounding-case fix: gemini had a leftover http://localhost:9121/mcp
// (old unified-serena port); migrate must replace it with the current 9133.
func TestMigrateNonBindingClientOverwritesStalePortEntry(t *testing.T) {
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	sandboxClientConfigHome(t, tmp)

	geminiDir := filepath.Join(tmp, ".gemini")
	if err := os.MkdirAll(geminiDir, 0755); err != nil {
		t.Fatalf("mkdir gemini: %v", err)
	}
	geminiPath := filepath.Join(geminiDir, "settings.json")
	// STALE entry at the OLD port 9121.
	if err := os.WriteFile(geminiPath, []byte(
		`{"mcpServers":{"fetch":{"url":"http://localhost:9121/mcp","type":"http"}}}`), 0600); err != nil {
		t.Fatalf("write gemini config: %v", err)
	}

	manifestDir := filepath.Join(tmp, "servers")
	if err := os.MkdirAll(filepath.Join(manifestDir, "fetch"), 0755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "fetch", "manifest.yaml"),
		[]byte(`name: fetch
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9133
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
weekly_refresh: false
`), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	a := NewAPI()
	report, err := a.MigrateFrom(MigrateOpts{
		Servers:        []string{"fetch"},
		ClientsInclude: []string{"gemini-cli"},
		ScanOpts:       ScanOpts{ManifestDir: manifestDir},
	})
	if err != nil {
		t.Fatalf("MigrateFrom: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}

	data, err := os.ReadFile(geminiPath)
	if err != nil {
		t.Fatalf("read gemini: %v", err)
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal gemini: %v", err)
	}
	mem := cfg.MCPServers["fetch"]
	if mem["url"] != "http://127.0.0.1:9133/mcp" {
		t.Errorf("stale-port entry was NOT overwritten: fetch.url = %v, want http://127.0.0.1:9133/mcp", mem["url"])
	}
	if strings.Contains(string(data), "9121") {
		t.Errorf("stale port 9121 still present after overwrite; file = %s", data)
	}
}

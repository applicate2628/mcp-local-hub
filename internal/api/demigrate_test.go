package api

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

// setupTmpHomeAndClaude redirects UserHomeDir to tmp and seeds
// .claude.json with the given body. Returns the claude config path.
func setupTmpHomeAndClaude(t *testing.T, body string) string {
	t.Helper()
	// Phase 5 Task 5.1: adapter writes now route through
	// SecureWriteClientConfig (handle-relative + parent-dir DACL gate).
	// %TEMP%-backed t.TempDir() on Windows fails the parent-dir
	// allowlist gate (Authenticated Users inherited from \Users). These
	// pre-Phase-5 tests exercise the legacy loose-writer flow; install
	// the test fallback so they keep working without a hardenedTempDir
	// migration. New tests that want to validate the secure-write
	// pipeline use hardenedTempDir directly (see
	// client_adapter_dacl_test.go).
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claude := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claude, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return claude
}

// TestDemigrate_RestoresStdioPerEntry round-trips a claude-code stdio
// entry through migrate → demigrate using a real manifest (so the
// client-bindings iteration is exercised, not the naive
// "iterate every installed adapter" pattern).
func TestDemigrate_RestoresStdioPerEntry(t *testing.T) {
	claudePath := setupTmpHomeAndClaude(t,
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	if err := os.MkdirAll(memDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifestBody := `name: memory
kind: global
transport: stdio-bridge
command: npx
base_args:
  - "-y"
  - "mem"

daemons:
  - name: default
    port: 9200

client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`
	if err := os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(manifestBody), 0600); err != nil {
		t.Fatal(err)
	}

	cc, _ := clients.NewClaudeCode()
	if _, err := cc.Backup(); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) > 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 restored row, got %d", len(report.Restored))
	}

	live, _ := os.ReadFile(claudePath)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["memory"].(map[string]any)
	if entry["type"] != "stdio" {
		t.Errorf("type=%v, want stdio", entry["type"])
	}
}

func TestDemigrate_OnlyIteratesManifestBindings(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)

	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	geminiDir := filepath.Join(tmp, ".gemini")
	if err := os.MkdirAll(geminiDir, 0700); err != nil {
		t.Fatal(err)
	}
	geminiPath := filepath.Join(geminiDir, "settings.json")
	geminiBefore := `{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp","type":"http","timeout":10000}}}`
	if err := os.WriteFile(geminiPath, []byte(geminiBefore), 0600); err != nil {
		t.Fatal(err)
	}

	ccBackup := claudePath + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(ccBackup, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	if err := os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
base_args: ["-y","mem"]
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	_, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}

	geminiAfter, _ := os.ReadFile(geminiPath)
	if string(geminiAfter) != geminiBefore {
		t.Errorf("gemini config was touched — manifest bindings only mention claude-code.\nbefore: %s\nafter:  %s",
			geminiBefore, string(geminiAfter))
	}
}

func TestDemigrate_ClientsIncludeFilter(t *testing.T) {
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)
	_ = os.WriteFile(claudePath+".bak-mcp-local-hub-20260101-000000", []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx"}}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: gemini-cli
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:        []string{"memory"},
		ClientsInclude: []string{"claude-code"},
		ScanOpts:       ScanOpts{ManifestDir: manifestDir},
		Writer:         io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 1 || report.Restored[0].Client != "claude-code" {
		t.Errorf("expected single claude-code restore, got %+v", report.Restored)
	}
}

func TestDemigrate_MultiServerNewestFirstSucceeds(t *testing.T) {
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"},"fs":{"type":"http","url":"http://localhost:9201/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	latest := claudePath + ".bak-mcp-local-hub-20260201-120000"
	if err := os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"},"fs":{"type":"stdio","command":"npx","args":["-y","fs"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	manifestDir := t.TempDir()
	fsDir := filepath.Join(manifestDir, "fs")
	_ = os.MkdirAll(fsDir, 0700)
	_ = os.WriteFile(filepath.Join(fsDir, "manifest.yaml"), []byte(
		`name: fs
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9201
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"fs"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) > 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 restored row, got %d", len(report.Restored))
	}
}

func TestDemigrate_MultiServerFallsBackToSentinel(t *testing.T) {
	// Earlier-migrated server's latest backup already holds the entry
	// in hub-managed form. Demigrate must fall back to the -original
	// sentinel (which captures true pre-hub state) rather than report
	// a clear but unhelpful failure.
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"},"fs":{"type":"http","url":"http://localhost:9201/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Latest backup: pre-fs-migrate, so memory is already in hub-managed
	// form here. Sentinel: pre-hub, so memory is stdio.
	latest := claudePath + ".bak-mcp-local-hub-20260201-120000"
	if err := os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"},"fs":{"type":"stdio","command":"npx"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	if err := os.WriteFile(sentinel, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) > 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 restored via sentinel fallback, got %+v", report.Restored)
	}
	// Live memory is back to stdio.
	live, _ := os.ReadFile(claudePath)
	var parsed map[string]any
	if err := json.Unmarshal(live, &parsed); err != nil {
		t.Fatal(err)
	}
	memEntry := parsed["mcpServers"].(map[string]any)["memory"].(map[string]any)
	if memEntry["command"] != "npx" {
		t.Errorf("live memory.command=%v, want npx; full live: %s", memEntry["command"], string(live))
	}
	if memEntry["type"] != "stdio" {
		t.Errorf("live memory.type=%v, want stdio", memEntry["type"])
	}
}

func TestDemigrate_FallsBackToRemoveEntryWhenBothBackupsHoldHubForm(t *testing.T) {
	// PR #186 (B4 fix) flips the prior semantic. Previously this
	// case (both latest backup AND sentinel hold the entry in
	// hub-managed form) returned a Failed row because demigrate
	// couldn't find a pre-hub form to restore. After B4: demigrate
	// falls back to RemoveEntry on the live config — the user is
	// rolling back hub-routing, and if no pre-hub form was ever
	// captured (mcphub installed entry from scratch, OR the April
	// 2026 codename rename `mcp-sync → mcp-local-hub` sealed the
	// sentinel AFTER the entry was already hub-managed) the correct
	// rollback target IS "no entry". The live config gets the entry
	// removed, the report shows Restored, and the GUI uncheck-and-
	// Apply flow completes successfully without B4's prior
	// fail-row.
	//
	// Empirical reproducer from session 2026-05-15 smoke: claude /
	// codex / gemini all hit this case because Claude Code uses
	// HTTP transport (no stdio "original") and mcp-sync→mcp-local-
	// hub rename sealed sentinels post-migrate.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)
	latest := claudePath + ".bak-mcp-local-hub-20260101-000000"
	_ = os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("expected 0 failures (RemoveEntry fallback should succeed), got %d: %+v", len(report.Failed), report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored row (via RemoveEntry fallback), got %+v", report.Restored)
	}
	if report.Restored[0].Server != "memory" || report.Restored[0].Client != "claude-code" {
		t.Errorf("Restored row wrong: %+v", report.Restored[0])
	}
	// Live config MUST no longer contain the memory entry — that is
	// the whole point of the RemoveEntry fallback.
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read live config: %v", err)
	}
	if strings.Contains(string(data), `"memory"`) {
		t.Errorf("RemoveEntry fallback did not remove memory entry from live config; file = %s", data)
	}
}

// TestDemigrate_RefusesRemoveEntryWhenLiveURLDoesNotMatchManifest pins
// codex bot r1 P1 closure on PR #186: the RemoveEntry fallback must
// NOT delete a live entry whose URL does not match the URL mcphub
// would have written (built from manifest's daemon port +
// binding.url_path). This guards against the data-loss case where a
// user configured a legitimate localhost HTTP MCP server BEFORE
// installing mcphub — both backup and sentinel would hold THAT
// user-owned entry (matching the loopback-URL heuristic in
// IsHubHTTPURL), and a blind RemoveEntry would delete it.
func TestDemigrate_RefusesRemoveEntryWhenLiveURLDoesNotMatchManifest(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	// Live entry points at port 7777 — a user's own MCP server,
	// NOT the manifest's daemon port (9200 below).
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:7777/mcp"}}}`), 0600)
	// Both backup and sentinel hold the same user-owned entry —
	// they predate any mcphub touch. IsHubHTTPURL matches them
	// (it accepts any loopback HTTP URL), but they are not
	// mcphub-managed.
	latest := claudePath + ".bak-mcp-local-hub-20260101-000000"
	_ = os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:7777/mcp"}}}`), 0600)
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:7777/mcp"}}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 0 {
		t.Errorf("expected 0 Restored — guard must refuse RemoveEntry on user-owned URL; got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 Failed row when guard refuses; got %d: %+v", len(report.Failed), report.Failed)
	}
	failMsg := report.Failed[0].Err
	if !strings.Contains(failMsg, "user-owned") || !strings.Contains(failMsg, "does not match") {
		t.Errorf("failure message must explain the URL-mismatch guard; got %q", failMsg)
	}
	// Live entry MUST be preserved.
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read live config: %v", err)
	}
	if !strings.Contains(string(data), `http://localhost:7777/mcp`) {
		t.Errorf("live user-owned entry was deleted (data-loss regression); file = %s", data)
	}
}

func TestDemigrate_FailsWhenOnlySentinelExistsAndLacksEntry(t *testing.T) {
	// Bot R4 P1 reproducer: all timestamped backups have been pruned
	// (e.g. via `backups clean --keep 0`) so LatestBackupPath returns
	// the pristine sentinel directly. If the server was added AFTER
	// the sentinel was written, the main restore path must apply the
	// same containment safety check as the fallback path — else
	// RestoreEntryFromBackup would silently delete the live entry.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)
	// Only the sentinel exists — timestamped backups pruned. Sentinel
	// is pristine pre-hub, so it does NOT contain memory (which was
	// added later).
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(`{"mcpServers":{}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 0 {
		t.Fatalf("expected 0 restored (sentinel lacks entry; silent-delete must be refused), got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	lowerErr := strings.ToLower(report.Failed[0].Err)
	if !strings.Contains(lowerErr, "sentinel") || !strings.Contains(lowerErr, "does not contain") {
		t.Errorf("failure message should indicate sentinel does not contain the entry: got %q", report.Failed[0].Err)
	}
	// Live config untouched.
	live, _ := os.ReadFile(claudePath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers := liveMap["mcpServers"].(map[string]any)
	if _, present := servers["memory"]; !present {
		t.Error("live config lost memory entry — sentinel-only path must not silently delete")
	}
}

func TestDemigrate_FailsWhenServerAddedAfterSentinelThenMigratedTwice(t *testing.T) {
	// Bot R2 P1 reproducer: operator installed mcphub (sentinel captured
	// as pristine pre-hub state), then LATER added serverX manually, then
	// migrated serverX twice. Latest backup holds X in hub-managed form.
	// Sentinel lacks X entirely (it was added after sentinel was written).
	// Naïve sentinel fallback would silently DELETE X from live and
	// count it as a successful rollback — destructive. Demigrate must
	// detect this via BackupContainsEntry pre-check and surface a clear
	// Failed row.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)
	// Latest backup = after first migrate (memory already hub-managed).
	latest := claudePath + ".bak-mcp-local-hub-20260301-120000"
	_ = os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)
	// Sentinel = pristine pre-hub, BEFORE memory was manually added.
	// memory is ABSENT from sentinel.
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(`{"mcpServers":{}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 0 {
		t.Fatalf("expected 0 restored (sentinel lacks entry, silent-delete must be refused), got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	lowerErr := strings.ToLower(report.Failed[0].Err)
	if !strings.Contains(lowerErr, "sentinel") || !strings.Contains(lowerErr, "does not contain") {
		t.Errorf("failure message should indicate sentinel does not contain the entry: got %q", report.Failed[0].Err)
	}
	// Live config must not have been touched — memory still present.
	live, _ := os.ReadFile(claudePath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers := liveMap["mcpServers"].(map[string]any)
	if _, present := servers["memory"]; !present {
		t.Error("live config lost memory entry — auto-rollback path must not silently delete user-added servers")
	}
}

func TestDemigrate_SingleServerMigratedTwiceRestoresViaSentinel(t *testing.T) {
	// Bot R1 P1 scenario: migrate serverA, then migrate serverA again.
	// The second migrate's backup captures post-first-migrate state,
	// so the entry is already hub-managed in the latest backup.
	// Demigrate must fall back to the sentinel.
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)
	// Latest backup = post-first-migrate (memory already http).
	latest := claudePath + ".bak-mcp-local-hub-20260301-120000"
	_ = os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)
	// Sentinel = pristine pre-hub (memory is stdio).
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) > 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 restored via sentinel fallback, got %+v", report.Restored)
	}
}

func TestDemigrate_NoBackupReportsFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	buf := &bytes.Buffer{}
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   buf,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) != 1 {
		t.Errorf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	if len(report.Restored) != 0 {
		t.Errorf("expected 0 restored, got %d", len(report.Restored))
	}
}

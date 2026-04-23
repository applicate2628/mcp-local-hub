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

func TestDemigrate_FailsWhenBothLatestAndSentinelRefuse(t *testing.T) {
	// Pathological: both latest backup AND sentinel hold the entry in
	// hub-managed form (e.g. user-edited sentinel or some unusual
	// install history). Demigrate must fail with a clear message
	// naming both paths.
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
	if len(report.Restored) != 0 {
		t.Fatalf("expected 0 restored (both backups hold hub-managed entry), got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	lowerErr := strings.ToLower(report.Failed[0].Err)
	if !strings.Contains(lowerErr, "sentinel") || !strings.Contains(lowerErr, "fallback") || !strings.Contains(lowerErr, "failed") {
		t.Errorf("failure message should mention sentinel fallback failed: got %q", report.Failed[0].Err)
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

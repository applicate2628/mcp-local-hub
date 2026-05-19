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

func TestDemigrate_FailsWhenBothLatestAndSentinelRefuseAndMarkerAbsent(t *testing.T) {
	// PR #187 (B4 ownership marker): both backups hold the entry in
	// hub-managed form, AND the managed-entries marker file has NO
	// record of this (client, server) tuple. Demigrate fails closed
	// — refuses to RemoveEntry because there's no positive proof
	// that mcphub installed this entry. This is the safe behavior
	// for user-owned entries that coincidentally match the hub-URL
	// heuristic but NOT the strict manifest-URL match.
	//
	// v0.4.2: when the live URL EXACTLY matches what mcphub would
	// have written (manifest daemon port + binding url_path), the
	// backfillMarkerIfEntryMatchesManifest helper kicks in and
	// allows demigrate to proceed. To preserve this test's
	// fail-closed scenario, the fixture uses port 9201 (NOT 9200,
	// which IS the manifest's expected port). The URL is therefore
	// loopback hub-shaped but does NOT strictly match manifest
	// expectations → backfill does NOT fire → fail-closed
	// behavior preserved.
	managedEntriesTestHelper(t) // redirects state-dir; marker file absent

	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	// Note: port 9201 (NOT 9200 — see test docstring).
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9201/mcp"}}}`), 0600)
	latest := claudePath + ".bak-mcp-local-hub-20260101-000000"
	_ = os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9201/mcp"}}}`), 0600)
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9201/mcp"}}}`), 0600)

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
		t.Fatalf("expected 0 restored (marker absent → fail-closed); got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	lowerErr := strings.ToLower(report.Failed[0].Err)
	if !strings.Contains(lowerErr, "marker") || !strings.Contains(lowerErr, "refusing") {
		t.Errorf("failure message should mention marker + refusing; got %q", report.Failed[0].Err)
	}
	// Live entry must be preserved.
	data, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(data), `"memory"`) {
		t.Errorf("live entry was deleted despite marker-absent (data-loss regression); file = %s", data)
	}
}

func TestDemigrate_SucceedsWhenBothLatestAndSentinelRefuseAndMarkerConfirms(t *testing.T) {
	// PR #187 positive-marker path: both backups hub-managed AND the
	// managed-entries marker file has (claude-code, memory) recorded.
	// Demigrate calls RemoveEntry on the live config, then Forgets
	// the marker row. Reports Restored.
	managedEntriesTestHelper(t)

	// Seed the marker with the (client, server) tuple.
	if err := RecordManagedEntry("claude-code", "memory"); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

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
		t.Fatalf("expected 0 failures (marker confirms ownership → RemoveEntry safe); got %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored; got %+v", report.Restored)
	}
	// Live entry must be removed.
	data, _ := os.ReadFile(claudePath)
	if strings.Contains(string(data), `"memory"`) {
		t.Errorf("RemoveEntry did not remove memory entry from live config; file = %s", data)
	}
	// Marker row must be forgotten (so a subsequent re-migrate
	// starts fresh).
	managed, _ := IsManagedEntry("claude-code", "memory")
	if managed {
		t.Errorf("marker row was not forgotten after successful RemoveEntry; IsManaged still true")
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

func TestDemigrate_ServerAddedAfterSentinelThenMigratedTwice_BackfillSucceeds(t *testing.T) {
	// Originally TestDemigrate_FailsWhenServerAddedAfterSentinelThenMigratedTwice
	// (Bot R2 P1 reproducer) asserted hard fail-closed behavior in this
	// scenario. That posture was reverted under the 2026-05-15
	// demigrate-fallback fix when the live entry's URL strictly matches
	// the manifest's expected `http://localhost:<daemon.port><url_path>`.
	//
	// Scenario: operator installed mcphub (sentinel captured as pristine
	// pre-hub state), then LATER ran `mcphub register memory` or
	// `mcphub migrate memory`, then migrated it again. Latest backup
	// holds memory in hub-managed form. Sentinel is empty (memory was
	// added AFTER sentinel was written). Live URL exactly equals what
	// mcphub WOULD have written for this manifest binding.
	//
	// Path B fix (2026-05-19): instead of failing closed, the new code
	// routes this through the same marker+backfill+RemoveEntry helper
	// the both-hub-managed branch already uses. The backfill helper
	// confirms the live URL exactly matches manifest expectation (port +
	// url_path + name), which is structurally indistinguishable from a
	// mcphub install — records the marker inline and removes the entry.
	// Safety: codex-bot P1 closure on PR #186 r1 already approved this
	// reasoning for Path A (both backups hub-managed); Path B inherits
	// the same logic because the threat model is identical.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)
	latest := claudePath + ".bak-mcp-local-hub-20260301-120000"
	_ = os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0600)
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
	if len(report.Failed) != 0 {
		t.Fatalf("expected 0 failures (backfill should match live URL to manifest), got %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored row (marker-backfill path succeeded); got %+v", report.Restored)
	}
	// Live entry must be removed (backfill confirmed mcphub-managed).
	data, _ := os.ReadFile(claudePath)
	if strings.Contains(string(data), `"memory"`) {
		t.Errorf("RemoveEntry did not remove memory entry from live config; file = %s", data)
	}
	// Marker row must be forgotten so a subsequent re-migrate
	// starts fresh (matches the both-hub-managed branch's
	// self-healing contract).
	managed, _ := IsManagedEntry("claude-code", "memory")
	if managed {
		t.Errorf("marker row was not forgotten after successful RemoveEntry; IsManaged still true")
	}
}

func TestDemigrate_ServerAddedAfterSentinel_LiveUrlDoesNotMatchManifest_FailsClosed(t *testing.T) {
	// Complementary to the test above: when the sentinel lacks the
	// entry AND the live URL does NOT match manifest expectation
	// (different port, different url_path, or no marker record) AND
	// the managed-entries marker has no record, demigrate must
	// fail-closed. This preserves the safety property: never delete
	// an entry that we cannot positively attribute to mcphub.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)
	claudePath := filepath.Join(tmp, ".claude.json")
	// Live URL points at port 9999 — DIFFERENT from the manifest's
	// expected port 9200. Backfill helper will reject the match.
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9999/mcp"}}}`), 0600)
	latest := claudePath + ".bak-mcp-local-hub-20260301-120000"
	_ = os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9999/mcp"}}}`), 0600)
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
		t.Fatalf("expected 0 restored (live URL diverges from manifest; backfill must reject), got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	lowerErr := strings.ToLower(report.Failed[0].Err)
	if !strings.Contains(lowerErr, "managed-entries marker has no record") {
		t.Errorf("failure should cite the marker-has-no-record reason (refusing without positive ownership evidence); got %q", report.Failed[0].Err)
	}
	// Live config untouched (the user-owned-shaped entry must survive).
	live, _ := os.ReadFile(claudePath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers := liveMap["mcpServers"].(map[string]any)
	if _, present := servers["memory"]; !present {
		t.Error("live config lost memory entry — backfill-rejected path must not delete user-shaped entries")
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

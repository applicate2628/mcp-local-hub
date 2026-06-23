package api

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

func TestDemigrate_MarkerPreseededButLiveURLMismatch_FailsClosed(t *testing.T) {
	// Regression guard: a stale marker must NOT be sufficient to
	// delete an entry when the current live shape no longer matches
	// the manifest-managed URL/relay expectation.
	managedEntriesTestHelper(t)
	if err := RecordManagedEntry("claude-code", "memory"); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9999/mcp"}}}`), 0o600)
	latest := claudePath + ".bak-mcp-local-hub-20260101-000000"
	_ = os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600)
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(`{"mcpServers":{}}`), 0o600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0o700)
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
`), 0o600)

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
		t.Fatalf("expected 0 restored with stale marker + live mismatch, got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	lowerErr := strings.ToLower(report.Failed[0].Err)
	if !strings.Contains(lowerErr, "no longer matches manifest-managed shape") {
		t.Errorf("failure should mention manifest-shape mismatch; got %q", report.Failed[0].Err)
	}
	live, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(live), `"memory"`) {
		t.Errorf("live entry was deleted despite stale marker mismatch; file=%s", live)
	}
}

func TestDemigrate_OnlySentinelExistsAndLacksEntry_BackfillMatch_Succeeds(t *testing.T) {
	// Originally TestDemigrate_FailsWhenOnlySentinelExistsAndLacksEntry
	// (Bot R4 P1 reproducer). Assertions FLIPPED under PR #220 r2
	// security review per the explicit policy at
	// work-items/bugs/2026-05-15-demigrate-fallback-when-no-pre-hub-form.md
	// §"Explicit policy on sentinel-only + backfill":
	//
	//   Backfill match (live URL exactly equals
	//   http://localhost:<daemon.port><url_path>) IS accepted as
	//   ownership evidence in the iteration-exhausted fallback,
	//   even when only the empty sentinel was available. The URL
	//   coincidence is vanishingly unlikely for genuine
	//   user-configured remote MCP servers, structurally
	//   indistinguishable from a mcphub install otherwise, and
	//   the user constraint "должно работать всегда" prioritizes
	//   operator unblocking.
	//
	// Scenario: all timestamped backups pruned, sentinel is empty.
	// Live URL exactly matches manifest expectation. New behavior:
	// iteration exhausts → tryMarkerOrBackfillRemove → backfill
	// records marker + RemoveEntry → 1 Restored.
	//
	// Strict mode and the complementary URL-mismatch fail-closed
	// case are covered by
	// TestDemigrate_OnlySentinelExistsAndLacksEntry_BackfillRejects_FailsClosed
	// below.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600)
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(`{"mcpServers":{}}`), 0o600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0o700)
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
`), 0o600)

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
		t.Fatalf("expected 0 failures (backfill matches manifest port + URL); got %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored (marker-backfill path succeeded); got %+v", report.Restored)
	}
	// Live entry removed (backfill confirmed mcphub-managed; no
	// pre-hub form anywhere in the backup chain).
	live, _ := os.ReadFile(claudePath)
	if strings.Contains(string(live), `"memory"`) {
		t.Errorf("RemoveEntry did not remove memory entry; file = %s", live)
	}
}

func TestDemigrate_OnlySentinelExistsAndLacksEntry_BackfillRejects_FailsClosed(t *testing.T) {
	// Complementary fail-closed coverage to the test above. Sentinel-
	// only + entry-missing-from-sentinel + URL does NOT match manifest
	// (live URL points at port 9999 vs manifest port 9200). Backfill
	// rejects, marker has no record, demigrate fails-closed.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9999/mcp"}}}`), 0o600)
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(`{"mcpServers":{}}`), 0o600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0o700)
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
`), 0o600)

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
		t.Fatalf("expected 0 restored (URL mismatch; backfill rejects), got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	lowerErr := strings.ToLower(report.Failed[0].Err)
	if !strings.Contains(lowerErr, "managed-entries marker has no record") {
		t.Errorf("failure should cite the marker-has-no-record reason; got %q", report.Failed[0].Err)
	}
	live, _ := os.ReadFile(claudePath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers := liveMap["mcpServers"].(map[string]any)
	if _, present := servers["memory"]; !present {
		t.Error("live config lost memory entry — backfill-rejected sentinel-only path must not delete")
	}
}

// TestDemigrate_InheritedImport_ClearerError reproduces the live import-inherited
// case (a hub-loopback entry that mcphub did not install — no marker, no backups,
// and a hub URL that does NOT match the manifest binding) and pins the
// OPERATOR-READABLE failure message:
//
//   - it names the import/edit-manually remedy (so an operator who sees a mimocode
//     ~/.claude.json-imported hub cell knows why Apply failed and what to do);
//   - it contains NO "<nil>" last-skip noise when there is no skip reason (zero
//     backups → the clean "no mcp-local-hub backups exist" reasonPrefix);
//   - the live config is UNTOUCHED (no-copy-up + fail-closed preserved) and the
//     run still reports the entry as FAILED.
//
// It KEEPS asserting the canonical "managed-entries marker has no record" /
// "refusing" refusal vocabulary — that is the established fail-closed contract the
// sibling demigrate tests guard; the fix ENRICHES the message, it does not drop it.
func TestDemigrate_InheritedImport_ClearerError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t) // isolate the marker store → no marker for `time`
	claudePath := filepath.Join(tmp, ".claude.json")
	// A hub-loopback URL (IsHubHTTPURL true) at a port that does NOT match the
	// manifest binding (9999 vs 9200), so the URL backfill rejects → !managed.
	// No backup files exist (zero candidates) → the clean reasonPrefix path.
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"time":{"type":"http","url":"http://localhost:9999/mcp"}}}`), 0o600)

	manifestDir := t.TempDir()
	srvDir := filepath.Join(manifestDir, "time")
	_ = os.MkdirAll(srvDir, 0o700)
	_ = os.WriteFile(filepath.Join(srvDir, "manifest.yaml"), []byte(
		`name: time
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
`), 0o600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"time"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 0 {
		t.Fatalf("expected 0 restored (import-inherited, fail-closed), got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 FAILED entry, got %d: %+v", len(report.Failed), report.Failed)
	}
	msg := report.Failed[0].Err
	lower := strings.ToLower(msg)
	// Clearer remedy hint is present.
	if !strings.Contains(lower, "import") {
		t.Errorf("error must name the import cause; got %q", msg)
	}
	if !strings.Contains(lower, "re-run migrate") {
		t.Errorf("error must name the re-run-migrate remedy; got %q", msg)
	}
	// No <nil> last-skip noise when there is no skip reason (zero backups).
	if strings.Contains(msg, "<nil>") {
		t.Errorf("error must NOT contain the <nil> last-skip noise; got %q", msg)
	}
	if strings.Contains(msg, "of 0 candidates") {
		t.Errorf("error must NOT contain the cryptic 'of 0 candidates' phrasing; got %q", msg)
	}
	// Canonical fail-closed vocabulary is preserved (sibling-test contract).
	if !strings.Contains(lower, "managed-entries marker has no record") {
		t.Errorf("error must still cite the marker-has-no-record refusal; got %q", msg)
	}
	// Live config untouched — no-copy-up + fail-closed.
	live, _ := os.ReadFile(claudePath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers, _ := liveMap["mcpServers"].(map[string]any)
	if _, present := servers["time"]; !present {
		t.Errorf("import-inherited fail-closed must NOT delete the live entry; file = %s", live)
	}
}

func TestDemigrate_ServerAddedAfterSentinelThenMigratedTwice_OnlyHubBackups_BackfillSucceeds(t *testing.T) {
	// Originally TestDemigrate_FailsWhenServerAddedAfterSentinelThenMigratedTwice
	// (Bot R2 P1 reproducer). Assertions flipped under the
	// 2026-05-19 newest-first iteration fix:
	//
	// Scenario: only ONE timestamped backup exists (memory hub-managed)
	// AND sentinel lacks the entry. Iteration through every backup
	// finds no pre-hub form → falls through to
	// tryMarkerOrBackfillRemove. The live URL exactly matches the
	// manifest's expected `http://localhost:9200/mcp`, so the
	// backfill helper records the marker inline and RemoveEntry runs.
	//
	// Safety guarantee preserved: this branch fires ONLY when every
	// available backup either lacks the entry or has it in
	// hub-managed form. If the operator wants to preserve a
	// user-direct pre-hub form, they must keep at least one backup
	// from before the first migrate — which is the default behavior
	// of `mcphub backups clean` (keep N=5). The complementary test
	// TestDemigrate_PreservesUserDirectFormViaOlderBackup covers
	// the case where such a backup exists.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600)
	latest := claudePath + ".bak-mcp-local-hub-20260301-120000"
	_ = os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600)
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(`{"mcpServers":{}}`), 0o600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0o700)
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
`), 0o600)

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
		t.Fatalf("expected 0 failures (backfill matches manifest port + URL); got %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored (marker-backfill path succeeded); got %+v", report.Restored)
	}
	// Live entry is removed (backfill confirmed mcphub-managed; no
	// pre-hub form anywhere in the backup chain).
	live, _ := os.ReadFile(claudePath)
	if strings.Contains(string(live), `"memory"`) {
		t.Errorf("RemoveEntry did not remove memory; file = %s", live)
	}
}

func TestDemigrate_ServerAddedAfterSentinel_AllBackupsHubManaged_BackfillRejects_FailsClosed(t *testing.T) {
	// Complementary fail-closed coverage: same backup setup as above
	// (only hub-managed timestamped + empty sentinel), but the live
	// URL does NOT match manifest expectation (port 9999 vs manifest
	// 9200). Backfill rejects, marker has no record, demigrate
	// fails-closed with the "marker has no record" reason. Live
	// config untouched.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9999/mcp"}}}`), 0o600)
	latest := claudePath + ".bak-mcp-local-hub-20260301-120000"
	_ = os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9999/mcp"}}}`), 0o600)
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	_ = os.WriteFile(sentinel, []byte(`{"mcpServers":{}}`), 0o600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0o700)
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
`), 0o600)

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
		t.Fatalf("expected 0 restored (URL mismatch; backfill rejects), got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	lowerErr := strings.ToLower(report.Failed[0].Err)
	if !strings.Contains(lowerErr, "managed-entries marker has no record") {
		t.Errorf("failure should cite the marker-has-no-record reason; got %q", report.Failed[0].Err)
	}
	live, _ := os.ReadFile(claudePath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers := liveMap["mcpServers"].(map[string]any)
	if _, present := servers["memory"]; !present {
		t.Error("live config lost memory entry — backfill-rejected path must not delete")
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

func TestDemigrate_PreservesUserDirectFormViaOlderBackup(t *testing.T) {
	// Failing test FIRST per TDD discipline. After this test, the proper
	// fix per work-items/bugs/2026-05-15-demigrate-fallback-when-no-pre-hub-form.md
	// §"Quality: Iterate timestamped backups newest-first" should make
	// this test pass.
	//
	// Scenario (PR #218 destructive case + restore-target proof):
	//
	// 1. Operator installed mcphub. Sentinel captured a snapshot
	//    that does NOT contain memory (memory was added later by the
	//    operator).
	// 2. Operator manually added memory as a direct/stdio entry
	//    (user-installed MCP server). This was captured in a
	//    timestamped backup as STDIO form.
	// 3. Operator ran `mcphub migrate memory`. The migrate created
	//    a NEWER timestamped backup just before the rewrite (newer
	//    backup ALSO contains memory in stdio form pre-rewrite OR
	//    in hub-managed form post-rewrite — depending on backup
	//    ordering vs the AddEntry call).
	// 4. After the migrate the live config has memory as
	//    hub-managed (http://localhost:<port>/mcp). The marker is
	//    populated (PR #187 contract).
	//
	// Now operator unchecks memory/<client> from the matrix and
	// clicks Apply. The demigrate flow:
	//
	//   - latest timestamped backup: memory hub-managed →
	//     ErrBackupEntryAlreadyMigrated
	//   - older timestamped backup: memory STDIO →
	//     RestoreEntryFromBackup writes stdio to live → SUCCESS
	//
	// Expected: 1 Restored, 0 Failed. Live config has memory as
	// stdio (the original user-installed form). NOT deleted.
	//
	// Pre-fix (current post-revert code): only consults the
	// LEXICOGRAPHICALLY-LATEST timestamped backup + the sentinel.
	// Both produce errors (latest=hub-managed, sentinel=missing-entry).
	// The marker fallback was already documented destructive (PR #218
	// reverted). So current code FAILS this test with "fail-closed,
	// edit manually" — protecting data but blocking the UI flow.
	//
	// Post-fix: iterate ALL backups newest-first, find the older
	// stdio backup, restore from THAT. Both preserves data AND
	// unblocks the UI.
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)

	claudePath := filepath.Join(tmp, ".claude.json")
	// Live config: memory is hub-managed (post-migrate).
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// NEWEST timestamped backup: post-migrate, memory hub-managed.
	newer := claudePath + ".bak-mcp-local-hub-20260519-130000"
	if err := os.WriteFile(newer, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// OLDER timestamped backup: pre-migrate, memory as STDIO
	// (user-installed direct entry).
	older := claudePath + ".bak-mcp-local-hub-20260518-080000"
	if err := os.WriteFile(older, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-memory"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Sentinel: pre-install snapshot, memory absent.
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	if err := os.WriteFile(sentinel, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Seed marker (post-migrate state).
	if err := RecordManagedEntry("claude-code", "memory"); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0o700)
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
`), 0o600)

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
		t.Fatalf("expected 0 failures (older backup has pre-hub form; restore must use it); got %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored (memory restored to stdio form); got %+v", report.Restored)
	}
	// CRITICAL: live config must NOT have deleted memory. It MUST
	// have memory as stdio (the pre-hub user-installed form).
	live, _ := os.ReadFile(claudePath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers, _ := liveMap["mcpServers"].(map[string]any)
	mem, present := servers["memory"]
	if !present {
		t.Fatalf("DESTRUCTIVE: memory entry was DELETED from live config (should have been restored to stdio form). Live = %s", live)
	}
	memMap, _ := mem.(map[string]any)
	if memMap["type"] != "stdio" {
		t.Errorf("memory entry was not restored to STDIO form. Got %+v", memMap)
	}
	if memMap["command"] != "npx" {
		t.Errorf("memory command not preserved as npx. Got %+v", memMap)
	}
}

func TestDemigrate_NoBackupHubURLEntry_RemovedViaHubURLCorroboration(t *testing.T) {
	// Bug: GUI Servers matrix — unchecking a via-hub server (e.g. `fetch`)
	// that was added http-from-start (its client entry was written by mcphub
	// DIRECTLY as a hub-HTTP URL, never migrated from a stdio form, so NO
	// backup of any codename exists) + Apply did NOT uncheck the cell — the
	// entry was never removed.
	//
	// Root cause: with zero backups, allowURLBackfill = len(backups)>0 ||
	// sawLegacy = false, so the URL-backfill RemoveEntry was refused
	// (fail-closed) even though the live entry's URL is a hub URL.
	//
	// Fix (user product-decision): a via-hub entry whose live URL satisfies
	// clients.IsHubHTTPURL is PROVABLY hub-installed (no operator manually
	// points a client at the hub's own loopback port), so demigrate may
	// RemoveEntry even with NO backup. The hub URL IS the ownership
	// corroboration. The strict manifest match still gates the actual
	// deletion (port 9200 + /mcp below exactly matches the manifest), so a
	// hub URL whose port/path does NOT match still fails closed.
	//
	// (This test was previously TestDemigrate_NoBackupReportsFailure, which
	// asserted the pre-fix fail-closed behavior for exactly this scenario.
	// That contract is what the fix deliberately changes. The complementary
	// negative — a NON-hub URL with no backup still fails closed — is
	// TestDemigrate_NoBackupNonHubURLEntry_FailsClosed below.)
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t) // marker store init; marker file absent (pre-marker entry)
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
	if len(report.Failed) != 0 {
		t.Fatalf("expected 0 failures (hub URL corroborates removal); got %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored (hub-URL entry removed), got %d: %+v", len(report.Restored), report.Restored)
	}
	// Live entry must be removed so a re-scan no longer classifies it via-hub.
	live, _ := os.ReadFile(claudePath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers, _ := liveMap["mcpServers"].(map[string]any)
	if _, present := servers["memory"]; present {
		t.Errorf("hub-URL entry was NOT removed (matrix cell would stay checked); file = %s", live)
	}
}

func TestDemigrate_NoBackupNonHubURLEntry_FailsClosed(t *testing.T) {
	// Negative complement to
	// TestDemigrate_NoBackupHubURLEntry_RemovedViaHubURLCorroboration: a
	// NON-hub URL (an operator's own remote MCP server) with NO backup must
	// STILL fail closed. The hub-URL corroboration only fires for
	// clients.IsHubHTTPURL URLs; a remote https URL is NOT one, so demigrate
	// keeps refusing to delete it (it might be a genuine user-configured
	// server). Live config untouched.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"https://remote.example.com:9200/mcp"}}}`), 0600)

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
	if len(report.Restored) != 0 {
		t.Fatalf("expected 0 restored (non-hub URL, no backup → fail closed); got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	lowerErr := strings.ToLower(report.Failed[0].Err)
	if !strings.Contains(lowerErr, "managed-entries marker has no record") {
		t.Errorf("failure should cite the marker-has-no-record reason; got %q", report.Failed[0].Err)
	}
	// Live entry must be preserved (potentially user-owned remote server).
	live, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(live), `"memory"`) {
		t.Errorf("non-hub-URL entry was deleted despite no corroboration (data-loss regression); file = %s", live)
	}
}

// seedGeminiForNonBinding redirects HOME/USERPROFILE to tmp and writes a
// gemini-cli config (~/.gemini/settings.json) with the given body. Returns
// the gemini config path. gemini-cli is the non-binding client used by the
// synthesized-binding demigrate tests below (the manifests bind only
// claude-code).
func seedGeminiForNonBinding(t *testing.T, tmp, body string) string {
	t.Helper()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	geminiDir := filepath.Join(tmp, ".gemini")
	if err := os.MkdirAll(geminiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	geminiPath := filepath.Join(geminiDir, "settings.json")
	if err := os.WriteFile(geminiPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return geminiPath
}

// writeNonBindingManifest writes a fetch manifest that binds ONLY
// claude-code (so gemini-cli is a non-binding client) with one default
// daemon on the given port.
func writeNonBindingManifest(t *testing.T, port int) string {
	t.Helper()
	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "fetch")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `name: fetch
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: ` + itoaPort(port) + `
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`
	if err := os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return manifestDir
}

func itoaPort(p int) string {
	return strconv.Itoa(p)
}

// TestDemigrate_NonBindingClient_MarkerCorroborated_Removed proves PART 1
// (demigrate-any-client) works through the UNCHANGED strict removal gate: a
// client NOT in the manifest's client_bindings (gemini-cli here) holds a
// hub-loopback entry whose port EXACTLY matches the current manifest daemon
// (9133), AND the managed-entries marker records (gemini-cli, fetch) as
// mcphub-installed. Demigrate targeted at gemini-cli synthesizes a binding
// and removes the entry via the legitimate marker + exact-manifest-match
// path — no stale-port relaxation involved. This is the safe replacement for
// the removed stale-port-loopback test; it demonstrates the synthesized
// binding routes through the strict, fail-closed-by-default gate.
func TestDemigrate_NonBindingClient_MarkerCorroborated_Removed(t *testing.T) {
	tmp := t.TempDir()
	managedEntriesTestHelper(t) // marker store init
	if err := RecordManagedEntry("gemini-cli", "fetch"); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	// Exact-port hub-loopback entry (9133 == manifest daemon) so the strict
	// liveEntryMatchesManifestBinding gate passes; removal proceeds via the
	// marker, NOT via any port-mismatch relaxation.
	geminiPath := seedGeminiForNonBinding(t, tmp,
		`{"mcpServers":{"fetch":{"url":"http://localhost:9133/mcp","type":"http"}}}`)
	manifestDir := writeNonBindingManifest(t, 9133)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:        []string{"fetch"},
		ClientsInclude: []string{"gemini-cli"}, // non-binding target
		ScanOpts:       ScanOpts{ManifestDir: manifestDir},
		Writer:         io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("expected 0 failures (marker + exact-port match removable on non-binding client); got %+v", report.Failed)
	}
	if len(report.Restored) != 1 || report.Restored[0].Client != "gemini-cli" {
		t.Fatalf("expected 1 Restored for gemini-cli; got %+v", report.Restored)
	}
	live, _ := os.ReadFile(geminiPath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers, _ := liveMap["mcpServers"].(map[string]any)
	if _, present := servers["fetch"]; present {
		t.Errorf("marker-corroborated entry was NOT removed on non-binding client; file = %s", live)
	}
}

// TestDemigrate_NonBindingClient_NonHubURL_FailsClosed proves the deep-sec
// invariant survives on the synthesized-binding path: an operator's own
// remote https MCP server on a non-binding client is NEVER removed.
func TestDemigrate_NonBindingClient_NonHubURL_FailsClosed(t *testing.T) {
	tmp := t.TempDir()
	managedEntriesTestHelper(t)
	geminiPath := seedGeminiForNonBinding(t, tmp,
		`{"mcpServers":{"fetch":{"url":"https://api.example.com/mcp","type":"http"}}}`)
	manifestDir := writeNonBindingManifest(t, 9133)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:        []string{"fetch"},
		ClientsInclude: []string{"gemini-cli"},
		ScanOpts:       ScanOpts{ManifestDir: manifestDir},
		Writer:         io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 0 {
		t.Fatalf("expected 0 restored (non-hub remote https → fail closed); got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	// Operator's remote MCP server must be preserved.
	live, _ := os.ReadFile(geminiPath)
	if !strings.Contains(string(live), "api.example.com") {
		t.Errorf("non-hub remote https entry was deleted on non-binding client (data-loss); file = %s", live)
	}
}

// TestDemigrate_NonBindingClient_StdioEntry_FailsClosed proves a stdio
// (command-bearing) entry on a non-binding client is NEVER removed by the
// synthesized-binding path. A stdio entry's GetEntry returns live.URL=="",
// so IsHubHTTPURL is false → the hub-loopback relaxation does not fire, and
// there is no backup/marker, so demigrate fails closed and leaves the
// user-owned stdio entry intact.
func TestDemigrate_NonBindingClient_StdioEntry_FailsClosed(t *testing.T) {
	tmp := t.TempDir()
	managedEntriesTestHelper(t)
	geminiPath := seedGeminiForNonBinding(t, tmp,
		`{"mcpServers":{"fetch":{"command":"npx","args":["-y","fetch-server"]}}}`)
	manifestDir := writeNonBindingManifest(t, 9133)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:        []string{"fetch"},
		ClientsInclude: []string{"gemini-cli"},
		ScanOpts:       ScanOpts{ManifestDir: manifestDir},
		Writer:         io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 0 {
		t.Fatalf("expected 0 restored (stdio entry is user-owned → fail closed); got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	// Stdio entry must be preserved verbatim.
	live, _ := os.ReadFile(geminiPath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers, _ := liveMap["mcpServers"].(map[string]any)
	entry, present := servers["fetch"].(map[string]any)
	if !present {
		t.Fatalf("stdio entry was DELETED on non-binding client (data-loss); file = %s", live)
	}
	if entry["command"] != "npx" {
		t.Errorf("stdio entry command not preserved; got %+v", entry)
	}
}

func TestDemigrate_NonBindingClient_UnrelatedBackupLacksEntry_FailsClosed(t *testing.T) {
	// Regression for targeted non-binding demigrate: a timestamped
	// mcp-local-hub backup for the client can be unrelated to this
	// server/client pair. If that backup lacks the target entry, it must
	// not be treated as proof that demigrate may delete a later user-owned
	// same-name live entry.
	tmp := t.TempDir()
	managedEntriesTestHelper(t)
	geminiPath := seedGeminiForNonBinding(t, tmp,
		`{"mcpServers":{"fetch":{"url":"http://localhost:9133/mcp","type":"http"},"other":{"url":"http://localhost:9000/mcp","type":"http"}}}`)
	backup := geminiPath + ".bak-mcp-local-hub-20260601-120000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"other":{"url":"http://localhost:9000/mcp","type":"http"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestDir := writeNonBindingManifest(t, 9133)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:        []string{"fetch"},
		ClientsInclude: []string{"gemini-cli"},
		ScanOpts:       ScanOpts{ManifestDir: manifestDir},
		Writer:         io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 0 {
		t.Fatalf("expected 0 restored (unrelated backup is not ownership proof); got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	live, _ := os.ReadFile(geminiPath)
	var liveMap map[string]any
	_ = json.Unmarshal(live, &liveMap)
	servers, _ := liveMap["mcpServers"].(map[string]any)
	if _, present := servers["fetch"]; !present {
		t.Fatalf("user-owned non-binding entry was deleted from unrelated backup path; file = %s", live)
	}
}

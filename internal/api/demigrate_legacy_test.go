package api

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMemoryManifest writes a minimal single-binding (claude-code)
// manifest for `memory` on port 9200 into manifestDir and returns
// manifestDir. Shared by the legacy-fallback demigrate tests.
func writeMemoryManifest(t *testing.T, manifestDir string) {
	t.Helper()
	memDir := filepath.Join(manifestDir, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `name: memory
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
`
	if err := os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDemigrate_UserDirectEntry_RestoredNotDeleted is THE PR #218
// regression guard (brief-named). An entry that was ORIGINALLY user-
// direct (stdio) and later migrated must be RESTORED from the older
// pre-hub backup, NEVER deleted via RemoveEntry — even though the latest
// backup + sentinel both hold the entry in hub-managed form and the
// ownership marker confirms mcphub manages it now.
//
// Proof that RemoveEntry provably cannot fire on a migrated-existing
// entry: the newest-first iteration finds the older stdio backup BEFORE
// the RemoveEntry fallback is ever reached (the fallback runs only when
// `restoredFrom == ""` after BOTH the mcp-local-hub iteration AND the
// legacy iteration). Here the older mcp-local-hub backup positively
// holds the pre-hub form, so the restore branch fires and the fallback
// is unreachable. The assertion below — live entry STILL present AND in
// stdio form — fails if RemoveEntry ever ran (it would have deleted the
// key).
func TestDemigrate_UserDirectEntry_RestoredNotDeleted(t *testing.T) {
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)

	claudePath := filepath.Join(tmp, ".claude.json")
	// Live: hub-managed (post-migrate).
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// NEWEST timestamped backup: hub-managed (snapshot taken after migrate).
	newest := claudePath + ".bak-mcp-local-hub-20260520-130000"
	if err := os.WriteFile(newest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// OLDER timestamped backup: the genuine pre-hub user-direct STDIO form.
	older := claudePath + ".bak-mcp-local-hub-20260518-080000"
	if err := os.WriteFile(older, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-memory"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Sentinel: hub-managed too (sealed after the April rename — the exact
	// condition that made the original bug irrecoverable via sentinel).
	sentinel := claudePath + ".bak-mcp-local-hub-original"
	if err := os.WriteFile(sentinel, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Marker says mcphub manages it — the exact precondition that made
	// PR #218 delete the entry. The fix must STILL restore, not delete.
	if err := RecordManagedEntry("claude-code", "memory"); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	manifestDir := t.TempDir()
	writeMemoryManifest(t, manifestDir)

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
		t.Fatalf("expected 0 failures (older backup has pre-hub form); got %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored; got %+v", report.Restored)
	}

	live, _ := os.ReadFile(claudePath)
	var lm map[string]any
	if err := json.Unmarshal(live, &lm); err != nil {
		t.Fatal(err)
	}
	servers, _ := lm["mcpServers"].(map[string]any)
	mem, present := servers["memory"]
	if !present {
		t.Fatalf("DESTRUCTIVE REGRESSION: memory was DELETED (RemoveEntry fired on a migrated-existing entry). Live = %s", live)
	}
	memMap, _ := mem.(map[string]any)
	if memMap["type"] != "stdio" || memMap["command"] != "npx" {
		t.Errorf("memory not restored to pre-hub stdio form; got %+v", memMap)
	}
	// Marker must remain (RemoveEntry+Forget never ran). If the entry had
	// been deleted, ForgetManagedEntry would have cleared this.
	managed, _ := IsManagedEntry("claude-code", "memory")
	if !managed {
		t.Errorf("marker was forgotten — implies RemoveEntry path ran (it must not have)")
	}
}

// TestDemigrate_IteratesNewestFirst_SkipsHubManaged proves the iteration
// chooses an OLDER pre-hub backup over a NEWER hub-managed one (brief-
// named). Same skeleton as the destructive guard but focused purely on
// the ordering: newest backup is hub-managed and must be skipped in favor
// of the older stdio backup.
func TestDemigrate_IteratesNewestFirst_SkipsHubManaged(t *testing.T) {
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)

	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Newer hub-managed backup (must be skipped).
	if err := os.WriteFile(claudePath+".bak-mcp-local-hub-20260520-130000", []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Older pre-hub backup (must be chosen).
	if err := os.WriteFile(claudePath+".bak-mcp-local-hub-20260518-080000", []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	manifestDir := t.TempDir()
	writeMemoryManifest(t, manifestDir)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) != 0 || len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored / 0 Failed; got restored=%+v failed=%+v", report.Restored, report.Failed)
	}
	live, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(live), `"stdio"`) || !strings.Contains(string(live), `"npx"`) {
		t.Errorf("expected live memory restored to older stdio form; live=%s", live)
	}
}

// TestDemigrate_FallbackToRemoveEntry_WhenNoPreHubFormFound proves the
// RemoveEntry fallback fires ONLY when no backup anywhere (timestamped +
// sentinel + legacy) holds a pre-hub form, with ownership confirmed by
// the manifest-URL backfill (brief-named). The entry genuinely never
// existed in pre-hub form here — it was installed by mcphub from scratch.
func TestDemigrate_FallbackToRemoveEntry_WhenNoPreHubFormFound(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)

	claudePath := filepath.Join(tmp, ".claude.json")
	// Live URL exactly matches manifest expectation → backfill confirms
	// ownership.
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Every backup hub-managed OR lacks the entry; legacy backup also
	// hub-managed (so the legacy iteration also finds no pre-hub form).
	if err := os.WriteFile(claudePath+".bak-mcp-local-hub-20260301-120000", []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath+".bak-mcp-local-hub-original", []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath+".bak-2026-04-15-mcp-sync", []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	manifestDir := t.TempDir()
	writeMemoryManifest(t, manifestDir)

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
		t.Fatalf("expected 0 failures (backfill confirms ownership → RemoveEntry); got %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored (RemoveEntry fallback); got %+v", report.Restored)
	}
	live, _ := os.ReadFile(claudePath)
	if strings.Contains(string(live), `"memory"`) {
		t.Errorf("RemoveEntry did not remove memory (no pre-hub form anywhere); live=%s", live)
	}
}

// TestDemigrate_LegacyPrefixFallback proves the genuinely-new behavior:
// when every mcp-local-hub backup (timestamped + sentinel) holds the
// entry in hub-managed form, but a LEGACY-codename backup
// (settings.json-style bak-<date>-mcp-sync) holds its true pre-hub form,
// demigrate restores from the legacy backup — it does NOT fall through to
// RemoveEntry. This is the user's originally-reported case (April-2026
// mcp-sync→mcp-local-hub rename). Without this fix the entry was deleted
// via the marker/backfill path (verified empirically pre-fix).
func TestDemigrate_LegacyPrefixFallback(t *testing.T) {
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)

	claudePath := filepath.Join(tmp, ".claude.json")
	// Live: hub-managed; URL matches manifest (so the RemoveEntry path
	// WOULD fire if the legacy fallback didn't intercept — making this a
	// real test that legacy restore takes precedence over deletion).
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// All mcp-local-hub backups + sentinel: hub-managed (no pre-hub form).
	if err := os.WriteFile(claudePath+".bak-mcp-local-hub-20260429-004626", []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath+".bak-mcp-local-hub-original", []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// TRUE pre-hub state ONLY in legacy mcp-sync prefix (stdio form).
	if err := os.WriteFile(claudePath+".bak-2026-04-15-mcp-sync", []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"uvx","args":["mcp-server-memory"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Marker confirms mcphub-managed — the precondition that, absent the
	// legacy fallback, would route to RemoveEntry and delete the entry.
	if err := RecordManagedEntry("claude-code", "memory"); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	manifestDir := t.TempDir()
	writeMemoryManifest(t, manifestDir)

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
		t.Fatalf("expected 0 failures (legacy backup has pre-hub form); got %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored (legacy-prefix restore); got %+v", report.Restored)
	}
	live, _ := os.ReadFile(claudePath)
	var lm map[string]any
	if err := json.Unmarshal(live, &lm); err != nil {
		t.Fatal(err)
	}
	servers, _ := lm["mcpServers"].(map[string]any)
	mem, present := servers["memory"]
	if !present {
		t.Fatalf("DESTRUCTIVE: memory deleted instead of restored from legacy backup. Live = %s", live)
	}
	memMap, _ := mem.(map[string]any)
	if memMap["type"] != "stdio" || memMap["command"] != "uvx" {
		t.Errorf("memory not restored to legacy pre-hub stdio form; got %+v", memMap)
	}
}

// TestDemigrate_LegacyPrefixSkipsHubManagedLegacyBackup is the
// destructive-safety complement: a legacy backup that LACKS the entry
// must NOT trigger a delete from the legacy path, and a legacy backup
// holding only a hub-managed form must be skipped. Here the legacy
// mcp-sync backup is hub-managed AND a separate plain-timestamp legacy
// backup lacks the entry; the older plain legacy backup holds the real
// pre-hub form. The legacy iteration must skip the hub-managed and the
// entry-absent legacy backups and restore from the pre-hub one — never
// deleting via the legacy path.
func TestDemigrate_LegacyPrefixSkipsHubManagedAndAbsentLegacyBackups(t *testing.T) {
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)

	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// mcp-local-hub backups all hub-managed.
	if err := os.WriteFile(claudePath+".bak-mcp-local-hub-20260429-004626", []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Legacy mcp-sync (consulted FIRST): hub-managed → must be skipped,
	// not used to delete.
	if err := os.WriteFile(claudePath+".bak-2026-04-20-mcp-sync", []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Legacy plain-timestamp NEWER: lacks the entry → must be skipped, NOT
	// allowed to delete the live entry.
	if err := os.WriteFile(claudePath+".bak-20260102-090000", []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Legacy plain-timestamp OLDER: the real pre-hub stdio form → restore.
	if err := os.WriteFile(claudePath+".bak-20260101-090000", []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"uvx","args":["mcp-server-memory"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecordManagedEntry("claude-code", "memory"); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	manifestDir := t.TempDir()
	writeMemoryManifest(t, manifestDir)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) != 0 || len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored / 0 Failed; got restored=%+v failed=%+v", report.Restored, report.Failed)
	}
	live, _ := os.ReadFile(claudePath)
	var lm map[string]any
	if err := json.Unmarshal(live, &lm); err != nil {
		t.Fatal(err)
	}
	mem, present := lm["mcpServers"].(map[string]any)["memory"]
	if !present {
		t.Fatalf("DESTRUCTIVE: memory deleted by an entry-absent legacy backup. Live = %s", live)
	}
	if mm, _ := mem.(map[string]any); mm["command"] != "uvx" {
		t.Errorf("memory not restored from the older pre-hub legacy backup; got %+v", mem)
	}
}

// TestDemigrate_LegacyPrefixFallback_NoCurrentBackup is the bot PR #257 P2
// guard: when there are ZERO current-codename (`bak-mcp-local-hub-*`) backups
// at all — only a legacy mcp-sync backup holding the pre-hub form — demigrate
// must STILL reach the legacy fallback and restore, NOT early-fail with "no
// backup found". Before the fix, the `len(backups)==0` branch reported failure
// and `continue`d, leaving the legacy fallback unreachable in exactly the
// cross-rename upgrade case it was added for.
func TestDemigrate_LegacyPrefixFallback_NoCurrentBackup(t *testing.T) {
	t.Cleanup(SetClientWriteFallbackForTest())
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	managedEntriesTestHelper(t)

	claudePath := filepath.Join(tmp, ".claude.json")
	// Live: hub-managed.
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// NO bak-mcp-local-hub-* backups at all. ONLY a legacy mcp-sync backup
	// with the true pre-hub stdio form.
	if err := os.WriteFile(claudePath+".bak-2026-04-15-mcp-sync", []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"uvx","args":["mcp-server-memory"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecordManagedEntry("claude-code", "memory"); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	manifestDir := t.TempDir()
	writeMemoryManifest(t, manifestDir)

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
		t.Fatalf("expected 0 failures (legacy backup has pre-hub form; no current backup must NOT early-fail); got %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 Restored (legacy restore reached with empty current-backup set); got %+v", report.Restored)
	}
	live, _ := os.ReadFile(claudePath)
	var lm map[string]any
	if err := json.Unmarshal(live, &lm); err != nil {
		t.Fatal(err)
	}
	mem, present := lm["mcpServers"].(map[string]any)["memory"]
	if !present {
		t.Fatalf("memory missing — legacy fallback was not reached on an empty current-backup set (P2 dead-code). Live = %s", live)
	}
	if mm, _ := mem.(map[string]any); mm["command"] != "uvx" {
		t.Errorf("memory not restored from legacy backup; got %+v", mem)
	}
}

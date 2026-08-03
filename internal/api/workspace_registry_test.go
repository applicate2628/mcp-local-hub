package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
)

func TestRegistry_RoundtripEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspaces.yaml")
	reg := NewRegistry(path)
	if err := reg.Save(); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	reg2 := NewRegistry(path)
	if err := reg2.Load(); err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if len(reg2.Workspaces) != 0 {
		t.Errorf("expected 0 workspaces, got %d", len(reg2.Workspaces))
	}
}

func TestRegistry_RoundtripWithEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspaces.yaml")
	reg := NewRegistry(path)
	reg.Put(WorkspaceEntry{
		WorkspaceKey:  "3f2a8c91",
		WorkspacePath: "c:/users/alice/projects/foo",
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9200,
		TaskName:      "mcp-local-hub-lsp-3f2a8c91-python",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-python", "claude-code": "mcp-language-server-python"},
		WeeklyRefresh: true,
	})
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reg2 := NewRegistry(path)
	if err := reg2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := reg2.Get("3f2a8c91", "python")
	if !ok {
		t.Fatal("entry missing after roundtrip")
	}
	if got.Port != 9200 {
		t.Errorf("Port = %d, want 9200", got.Port)
	}
	if got.ClientEntries["codex-cli"] != "mcp-language-server-python" {
		t.Errorf("ClientEntries[codex-cli] = %q", got.ClientEntries["codex-cli"])
	}
}

func TestRegistry_LoadAcceptsLargeWorkspacesFile(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "workspaces.yaml")
	raw, wantEntries := largeWorkspaceRegistryYAML(t)
	if len(raw) <= maxStateFileBytes {
		t.Fatalf("large workspace registry fixture is only %d bytes; want above hub-state cap %d", len(raw), maxStateFileBytes)
	}
	if int64(len(raw)) > maxIntentFileBytes {
		t.Fatalf("large workspace registry fixture grew past established large-state cap: %d > %d", len(raw), maxIntentFileBytes)
	}
	if got := stateFileReadCapBytes(path); got <= maxStateFileBytes {
		t.Errorf("stateFileReadCapBytes(%q) = %d, want a workspaces.yaml-specific cap above %d", path, got, maxStateFileBytes)
	}
	if err := WriteStateFileBytesLockHeld(path, raw); err != nil {
		t.Fatalf("seed large workspace registry: %v", err)
	}

	reg := NewRegistry(path)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load rejected %d-byte workspaces.yaml: %v", len(raw), err)
	}
	if len(reg.Workspaces) != wantEntries {
		t.Fatalf("Workspaces len = %d, want %d", len(reg.Workspaces), wantEntries)
	}
}

func largeWorkspaceRegistryYAML(t *testing.T) ([]byte, int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("version: 1\nworkspaces:\n")
	entries := 0
	for b.Len() <= maxStateFileBytes+4096 {
		i := strconv.Itoa(entries)
		b.WriteString("  - workspace_key: ws")
		b.WriteString(i)
		b.WriteString("\n    workspace_path: C:/workspace/")
		b.WriteString(i)
		b.WriteString("/")
		b.WriteString(strings.Repeat("x", 96))
		b.WriteString("\n    language: python\n    backend: mcp-language-server\n    port: 9")
		b.WriteString(i)
		b.WriteString("\n    task_name: mcp-local-hub-lsp-ws")
		b.WriteString(i)
		b.WriteString("-python\n    client_entries:\n      codex-cli: mcp-language-server-python\n    weekly_refresh: true\n")
		entries++
	}
	return []byte(b.String()), entries
}

// TestRegistry_SaveDoesNotWriteDeadBakFile verifies that Registry.Save no
// longer writes the old rolling .bak sidecar. The sidecar was never consumed by
// rollback/recovery code, bypassed the hardened state-file writer, and added an
// extra read-before-write failure mode before the canonical save.
func TestRegistry_SaveDoesNotWriteDeadBakFile(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "workspaces.yaml")
	if err := WriteStateFileBytesLockHeld(path, []byte("version: 1\nworkspaces:\n  - workspace_key: oldentry\n    language: python\n    port: 9200\n")); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(path)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	reg.Put(WorkspaceEntry{
		WorkspaceKey: "newentry", Language: "go", Port: 9201,
		Backend: "gopls-mcp", TaskName: "mcp-local-hub-lsp-newentry-go",
	})
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Save wrote dead .bak sidecar; stat err=%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if !bytes.Contains(raw, []byte("newentry")) {
		t.Errorf("registry missing new entry; got %s", raw)
	}
}

// TestRegistry_LockPreventsSimultaneousWriters spawns two goroutines; each
// acquires the registry lock, sleeps, writes a distinct entry, unlocks. Both
// entries must survive.
func TestRegistry_LockPreventsSimultaneousWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspaces.yaml")
	reg := NewRegistry(path)
	_ = reg.Save() // create empty

	var wg sync.WaitGroup
	write := func(key string, port int) {
		defer wg.Done()
		r := NewRegistry(path)
		unlock, err := r.Lock()
		if err != nil {
			t.Errorf("Lock: %v", err)
			return
		}
		defer assertRegistryReleased(t, unlock)
		if err := r.Load(); err != nil {
			t.Errorf("Load: %v", err)
			return
		}
		r.Put(WorkspaceEntry{
			WorkspaceKey: key, Language: "python",
			Backend: "mcp-language-server", Port: port,
			TaskName: "t-" + key,
		})
		if err := r.Save(); err != nil {
			t.Errorf("Save: %v", err)
		}
	}
	wg.Add(2)
	go write("aaa11111", 9200)
	go write("bbb22222", 9201)
	wg.Wait()

	final := NewRegistry(path)
	if err := final.Load(); err != nil {
		t.Fatal(err)
	}
	if len(final.Workspaces) != 2 {
		t.Fatalf("expected 2 entries after concurrent writers, got %d: %+v", len(final.Workspaces), final.Workspaces)
	}
}

func TestRegistry_LifecycleFieldsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspaces.yaml")
	reg := NewRegistry(path)
	now := time.Now().UTC().Truncate(time.Second)
	reg.Put(WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws/foo",
		Language: "python", Backend: "mcp-language-server", Port: 9200,
		TaskName:           "mcp-local-hub-lsp-abcd1234-python",
		ClientEntries:      map[string]string{"codex-cli": "mcp-language-server-python"},
		Lifecycle:          LifecycleActive,
		LastMaterializedAt: now,
		LastToolsCallAt:    now,
		LastError:          "", // healthy
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	reg2 := NewRegistry(path)
	if err := reg2.Load(); err != nil {
		t.Fatal(err)
	}
	got, ok := reg2.Get("abcd1234", "python")
	if !ok {
		t.Fatal("entry missing")
	}
	if got.Lifecycle != LifecycleActive {
		t.Errorf("Lifecycle = %q, want active", got.Lifecycle)
	}
	if !got.LastMaterializedAt.Equal(now) {
		t.Errorf("LastMaterializedAt = %v, want %v", got.LastMaterializedAt, now)
	}
}

func TestRegistry_PutLastToolsCallAtPreservesLifecycleAndLastError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	reg := NewRegistry(path)
	oldToolsCallAt := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	newToolsCallAt := oldToolsCallAt.Add(10 * time.Minute)
	reg.Put(WorkspaceEntry{
		WorkspaceKey:    "abcd1234",
		WorkspacePath:   "/ws/foo",
		Language:        SerenaLanguageSentinel,
		Backend:         "serena",
		Lifecycle:       LifecycleFailed,
		LastError:       "diagnostic must remain",
		LastToolsCallAt: oldToolsCallAt,
		ClientEntries:   map[string]string{},
	})
	if err := reg.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := reg.PutLastToolsCallAt("abcd1234", SerenaLanguageSentinel, newToolsCallAt); err != nil {
		t.Fatalf("PutLastToolsCallAt: %v", err)
	}
	gotReg := NewRegistry(path)
	if err := gotReg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := gotReg.Get("abcd1234", SerenaLanguageSentinel)
	if !ok {
		t.Fatal("entry missing")
	}
	if got.Lifecycle != LifecycleFailed || got.LastError != "diagnostic must remain" {
		t.Fatalf("lifecycle/error = %q/%q, want preserved failed diagnostic", got.Lifecycle, got.LastError)
	}
	if !got.LastToolsCallAt.Equal(newToolsCallAt) {
		t.Fatalf("LastToolsCallAt = %v, want %v", got.LastToolsCallAt, newToolsCallAt)
	}
}

// TestRegistry_PutLifecycleNoOpOnMissingEntry guards against ghost-row
// resurrection: after Unregister removes a (workspace_key, language) row,
// a still-running proxy process MAY emit a late lifecycle write.
// PutLifecycle must silently no-op in that case rather than construct a
// bare entry with no port/task/bindings — that would leave a partial
// ghost record in workspaces.yaml and `mcphub workspaces` output,
// breaking the Unregister contract.
func TestRegistry_PutLifecycleNoOpOnMissingEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	reg := NewRegistry(path)
	// Seed nothing: the (workspace_key, language) is unregistered.
	if err := reg.PutLifecycle("deadbeef", "python", LifecycleFailed, "late write"); err != nil {
		t.Fatalf("PutLifecycle: %v", err)
	}
	// Reload a fresh registry to assert nothing was persisted.
	reg2 := NewRegistry(path)
	if err := reg2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := reg2.Get("deadbeef", "python"); ok {
		t.Error("PutLifecycle resurrected a ghost entry for unregistered (key, lang)")
	}
	if len(reg2.Workspaces) != 0 {
		t.Errorf("registry has %d entries, want 0 (ghost-row leak)", len(reg2.Workspaces))
	}
}

func TestRegistry_LastErrorTruncation(t *testing.T) {
	path := t.TempDir() + "/r.yaml"
	reg := NewRegistry(path)
	// Seed the entry first — PutLifecycle no-ops when the entry is absent
	// (ghost-resurrection guard); truncation is verified on a Put update,
	// mirroring the proxy's real flow: Register seeds, proxy updates.
	reg.Put(WorkspaceEntry{WorkspaceKey: "abcd1234", Language: "python", Backend: "mcp-language-server"})
	if err := reg.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	big := strings.Repeat("x", 500)
	if err := reg.PutLifecycle("abcd1234", "python", LifecycleFailed, big); err != nil {
		t.Fatalf("PutLifecycle: %v", err)
	}
	e, ok := reg.Get("abcd1234", "python")
	if !ok {
		t.Fatal("missing entry after PutLifecycle")
	}
	if len(e.LastError) > MaxLastErrorBytes {
		t.Errorf("LastError length = %d, want <= %d", len(e.LastError), MaxLastErrorBytes)
	}
}

// B.1 (serena sentinel + dual-gate) test contract.
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md B.1.

func TestRegistry_PutLSP_RejectsAtPrefixLanguage(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "workspaces.yaml"))
	cases := []string{"@serena", "@anything", "@"}
	for _, lang := range cases {
		err := reg.PutLSP(WorkspaceEntry{
			WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: lang,
			Backend: "mcp-language-server", Port: 9200, TaskName: "t",
		})
		if err == nil {
			t.Errorf("PutLSP(language=%q) returned nil, want error", lang)
		}
	}
	if len(reg.Workspaces) != 0 {
		t.Errorf("PutLSP rejection should not mutate Workspaces; got %d entries", len(reg.Workspaces))
	}
	if err := reg.PutLSP(WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: "python",
		Backend: "mcp-language-server", Port: 9200, TaskName: "t",
	}); err != nil {
		t.Errorf("PutLSP(language=python) returned error: %v", err)
	}
}

func TestRegistry_PutSerena_RequiresExactSentinel(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "workspaces.yaml"))
	rejected := []string{"@other", "@serena2", "serena", "python", ""}
	for _, lang := range rejected {
		err := reg.PutSerena(WorkspaceEntry{
			WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: lang,
			Backend: "serena", Port: 9500, TaskName: "t",
		})
		if err == nil {
			t.Errorf("PutSerena(language=%q) returned nil, want error", lang)
		}
	}
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: SerenaLanguageSentinel,
		Backend: "serena", Port: 9500, TaskName: "t-serena",
	}); err != nil {
		t.Errorf("PutSerena(language=SerenaLanguageSentinel) returned error: %v", err)
	}
	if got, ok := reg.GetSerena("abcd1234"); !ok || got.Language != SerenaLanguageSentinel {
		t.Errorf("GetSerena round-trip failed; got %+v ok=%v", got, ok)
	}
}

func TestRegistry_SerenaSentinel_RoundTripsNewFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspaces.yaml")
	reg := NewRegistry(path)
	registeredAt := time.Now().UTC().Truncate(time.Second)
	languages := []string{"go", "typescript", "markdown"}
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "c:/ws/foo",
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9500,
		TaskName:      "mcp-local-hub-serena-abcd1234",
		RegisteredAt:  registeredAt,
		RegisteredVia: "manual",
		Languages:     languages,
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reg2 := NewRegistry(path)
	if err := reg2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := reg2.GetSerena("abcd1234")
	if !ok {
		t.Fatal("GetSerena returned !ok after round-trip")
	}
	if got.RegisteredVia != "manual" {
		t.Errorf("RegisteredVia = %q, want manual", got.RegisteredVia)
	}
	if !got.RegisteredAt.Equal(registeredAt) {
		t.Errorf("RegisteredAt = %v, want %v", got.RegisteredAt, registeredAt)
	}
	if len(got.Languages) != len(languages) {
		t.Fatalf("Languages len = %d, want %d", len(got.Languages), len(languages))
	}
	for i, lang := range languages {
		if got.Languages[i] != lang {
			t.Errorf("Languages[%d] = %q, want %q", i, got.Languages[i], lang)
		}
	}
	reg3 := NewRegistry(filepath.Join(dir, "lsp-only.yaml"))
	if err := reg3.PutLSP(WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: "python",
		Backend: "mcp-language-server", Port: 9200, TaskName: "t",
	}); err != nil {
		t.Fatalf("PutLSP seed: %v", err)
	}
	if err := reg3.Save(); err != nil {
		t.Fatalf("Save lsp-only: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "lsp-only.yaml"))
	if err != nil {
		t.Fatalf("read lsp-only yaml: %v", err)
	}
	if strings.Contains(string(raw), "registered_at:") || strings.Contains(string(raw), "registered_via:") || strings.Contains(string(raw), "languages:") {
		t.Errorf("LSP-row YAML must not emit B.1 fields when zero; got:\n%s", raw)
	}
}

func TestRegistry_BeginSerenaPendingRemoval_StagesAndRollbackRestores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	reg := NewRegistry(path)
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "c:/ws/foo",
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9500,
		TaskName:      "mcp-local-hub-serena-abcd1234",
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	rollback, err := reg.BeginSerenaPendingRemoval("abcd1234", "", "")
	if err != nil || rollback == nil {
		t.Fatalf("BeginSerenaPendingRemoval = rollback %v, err %v", rollback != nil, err)
	}
	reg2 := NewRegistry(path)
	if err := reg2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.GetSerena("abcd1234")
	if !ok {
		t.Fatal("row missing after BeginSerenaPendingRemoval")
	}
	if !got.PendingSerenaRemoval {
		t.Error("PendingSerenaRemoval = false after BeginSerenaPendingRemoval")
	}

	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	reg3 := NewRegistry(path)
	if err := reg3.Load(); err != nil {
		t.Fatalf("reload after rollback: %v", err)
	}
	got3, ok := reg3.GetSerena("abcd1234")
	if !ok {
		t.Fatal("row missing after rollback")
	}
	if got3.PendingSerenaRemoval {
		t.Error("PendingSerenaRemoval = true after rollback")
	}
}

// TestRegistry_BeginSerenaPendingRemoval_NoOpOnMissingRow mirrors
// TestRegistry_PutLifecycleNoOpOnMissingEntry's ghost-resurrection guard: a
// workspace key with no serena row is a silent no-op, never a ghost row.
func TestRegistry_BeginSerenaPendingRemoval_NoOpOnMissingRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	reg := NewRegistry(path)
	rollback, err := reg.BeginSerenaPendingRemoval("deadbeef", "", "")
	if err != nil || rollback != nil {
		t.Fatalf("BeginSerenaPendingRemoval = rollback %v, err %v; want nil, nil", rollback != nil, err)
	}
	reg2 := NewRegistry(path)
	if err := reg2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reg2.Workspaces) != 0 {
		t.Errorf("registry has %d entries, want 0 (ghost-row leak)", len(reg2.Workspaces))
	}
}

// TestRegistry_BeginSerenaPendingRemoval_BothCanonicalAndLegacyKeys stages BOTH
// rows when the canonical and legacy keys diverge and both have a serena row
// — mirrors how unregister's own DeleteSerenaRow handles the same
// canonical/legacy pair (workspace_cmd.go, prune_workspace.go).
func TestRegistry_BeginSerenaPendingRemoval_BothCanonicalAndLegacyKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	reg := NewRegistry(path)
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey: "canonical1", WorkspacePath: "c:/ws/foo",
		Language: SerenaLanguageSentinel, Backend: "serena", Port: 9500,
		TaskName: "mcp-local-hub-serena-canonical1",
	}); err != nil {
		t.Fatalf("PutSerena canonical: %v", err)
	}
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey: "legacy999", WorkspacePath: "c:/ws/foo",
		Language: SerenaLanguageSentinel, Backend: "serena", Port: 9501,
		TaskName: "mcp-local-hub-serena-legacy999",
	}); err != nil {
		t.Fatalf("PutSerena legacy: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	rollback, err := reg.BeginSerenaPendingRemoval("canonical1", "legacy999", "")
	if err != nil || rollback == nil {
		t.Fatalf("BeginSerenaPendingRemoval = rollback %v, err %v", rollback != nil, err)
	}
	reg2 := NewRegistry(path)
	if err := reg2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	canonical, ok := reg2.GetSerena("canonical1")
	if !ok || !canonical.PendingSerenaRemoval {
		t.Errorf("canonical row PendingSerenaRemoval = %v (ok=%v), want true", canonical.PendingSerenaRemoval, ok)
	}
	legacy, ok := reg2.GetSerena("legacy999")
	if !ok || !legacy.PendingSerenaRemoval {
		t.Errorf("legacy row PendingSerenaRemoval = %v (ok=%v), want true", legacy.PendingSerenaRemoval, ok)
	}
}

func TestRegistry_BeginSerenaPendingRemoval_RollbackOwnership(t *testing.T) {
	const (
		canonical  = "abcd1234"
		legacy     = "beefcafe"
		oldGen     = "0123456789abcdef0123456789abcdef"
		attemptGen = "fedcba9876543210fedcba9876543210"
		thirdGen   = "11111111111111111111111111111111"
	)
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	priorAt := time.Date(2026, time.August, 3, 12, 0, 0, 123456789, time.UTC)
	reg := NewRegistry(path)
	for _, row := range []WorkspaceEntry{
		{WorkspaceKey: canonical, WorkspacePath: "c:/ws/canonical", Language: SerenaLanguageSentinel, Backend: "serena", Port: 9500, PendingSerenaRemoval: true, PendingSerenaRemovalAt: priorAt, PendingSerenaRemovalGeneration: oldGen},
		{WorkspaceKey: legacy, WorkspacePath: "c:/ws/legacy", Language: SerenaLanguageSentinel, Backend: "serena", Port: 9501},
	} {
		if err := reg.PutSerena(row); err != nil {
			t.Fatalf("PutSerena: %v", err)
		}
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	rollback, err := NewRegistry(path).BeginSerenaPendingRemoval(canonical, legacy, attemptGen)
	if err != nil || rollback == nil {
		t.Fatalf("BeginSerenaPendingRemoval = rollback %v, err %v", rollback != nil, err)
	}
	staged := NewRegistry(path)
	if err := staged.Load(); err != nil {
		t.Fatalf("load staged registry: %v", err)
	}
	candidate, ok := staged.GetSerena(canonical)
	if !ok || !candidate.PendingSerenaRemoval || candidate.PendingSerenaRemovalGeneration != attemptGen {
		t.Fatalf("canonical staged tuple = %+v, want attempt generation", candidate)
	}
	legacyCandidate, ok := staged.GetSerena(legacy)
	if !ok || !legacyCandidate.PendingSerenaRemoval || !legacyCandidate.PendingSerenaRemovalAt.Equal(candidate.PendingSerenaRemovalAt) || legacyCandidate.PendingSerenaRemovalGeneration != attemptGen {
		t.Fatalf("legacy staged tuple = %+v, want same exact attempt as canonical %+v", legacyCandidate, candidate)
	}

	// A later Begin owns the legacy tuple. The first rollback restores only the
	// canonical pre-state and exposes the conflict without clobbering it.
	laterRollback, err := NewRegistry(path).BeginSerenaPendingRemoval(legacy, "", thirdGen)
	if err != nil || laterRollback == nil {
		t.Fatalf("later BeginSerenaPendingRemoval = rollback %v, err %v", laterRollback != nil, err)
	}
	_ = laterRollback // Deliberately retain the later writer's staged tuple.
	if err := rollback(); !errors.Is(err, ErrSerenaPendingRemovalRollbackConflict) {
		t.Fatalf("rollback error = %v, want typed ownership conflict", err)
	}
	after := NewRegistry(path)
	if err := after.Load(); err != nil {
		t.Fatalf("load after rollback: %v", err)
	}
	gotCanonical, _ := after.GetSerena(canonical)
	if !gotCanonical.PendingSerenaRemoval || !gotCanonical.PendingSerenaRemovalAt.Equal(priorAt) || gotCanonical.PendingSerenaRemovalGeneration != oldGen {
		t.Fatalf("canonical pre-state was not restored exactly: %+v", gotCanonical)
	}
	gotLegacy, _ := after.GetSerena(legacy)
	if gotLegacy.PendingSerenaRemovalGeneration != thirdGen {
		t.Fatalf("third-party tuple was clobbered: %+v", gotLegacy)
	}
	if err := rollback(); !errors.Is(err, ErrSerenaPendingRemovalRollbackConflict) {
		t.Fatalf("idempotent rollback error = %v, want retained typed conflict only", err)
	}
}

func TestRegistry_BeginSerenaPendingRemoval_RollbackDoesNotRecreateDeletedRows(t *testing.T) {
	const key = "abcd1234"
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	reg := NewRegistry(path)
	if err := reg.PutSerena(WorkspaceEntry{WorkspaceKey: key, WorkspacePath: "c:/ws", Language: SerenaLanguageSentinel, Backend: "serena"}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rollback, err := NewRegistry(path).BeginSerenaPendingRemoval(key, key, "0123456789abcdef0123456789abcdef")
	if err != nil || rollback == nil {
		t.Fatalf("BeginSerenaPendingRemoval = rollback %v, err %v", rollback != nil, err)
	}
	deleted := NewRegistry(path)
	if err := deleted.Load(); err != nil {
		t.Fatalf("load staged registry: %v", err)
	}
	deleted.RemoveSerena(key)
	if err := deleted.Save(); err != nil {
		t.Fatalf("save deleted registry: %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback after row deletion: %v", err)
	}
	final := NewRegistry(path)
	if err := final.Load(); err != nil {
		t.Fatalf("load final registry: %v", err)
	}
	if _, ok := final.GetSerena(key); ok {
		t.Fatal("rollback recreated a successfully deleted Serena row")
	}
}

func TestRegistry_BeginSerenaPendingRemoval_CommitUnknownRestoresPreState(t *testing.T) {
	const (
		key        = "abcd1234"
		generation = "0123456789abcdef0123456789abcdef"
	)
	path := filepath.Join(t.TempDir(), "workspaces.yaml")
	seed := NewRegistry(path)
	if err := seed.PutSerena(WorkspaceEntry{WorkspaceKey: key, WorkspacePath: "c:/ws", Language: SerenaLanguageSentinel, Backend: "serena"}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if err := seed.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	commitUnknown := errors.New("registry writer could not reopen after rename")
	staging := NewRegistry(path)
	staging.savePendingRemovalFn = func(r *Registry) error {
		if err := r.Save(); err != nil {
			return err
		}
		return commitUnknown
	}
	rollback, err := staging.BeginSerenaPendingRemoval(key, "", generation)
	if !errors.Is(err, commitUnknown) || rollback == nil {
		t.Fatalf("BeginSerenaPendingRemoval = rollback %v, err %v; want commit-unknown rollback", rollback != nil, err)
	}
	staged := NewRegistry(path)
	if err := staged.Load(); err != nil {
		t.Fatalf("load published staged tuple: %v", err)
	}
	if row, _ := staged.GetSerena(key); !row.PendingSerenaRemoval || row.PendingSerenaRemovalGeneration != generation {
		t.Fatalf("commit-unknown stage was not published: %+v", row)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback commit-unknown tuple: %v", err)
	}
	restored := NewRegistry(path)
	if err := restored.Load(); err != nil {
		t.Fatalf("load restored registry: %v", err)
	}
	if row, _ := restored.GetSerena(key); row.PendingSerenaRemoval || !row.PendingSerenaRemovalAt.IsZero() || row.PendingSerenaRemovalGeneration != "" {
		t.Fatalf("commit-unknown rollback did not restore exact pre-state: %+v", row)
	}
}

func TestRegistry_SerenaSentinel_CoexistsWithLSPRows(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "workspaces.yaml"))
	lsps := []string{"go", "typescript", "markdown"}
	for i, lang := range lsps {
		if err := reg.PutLSP(WorkspaceEntry{
			WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: lang,
			Backend: "mcp-language-server", Port: 9200 + i, TaskName: "lsp-" + lang,
		}); err != nil {
			t.Fatalf("PutLSP %q: %v", lang, err)
		}
	}
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: SerenaLanguageSentinel,
		Backend: "serena", Port: 9500, TaskName: "serena-abcd1234",
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if n := len(reg.Workspaces); n != 4 {
		t.Fatalf("Workspaces count = %d, want 4 (3 LSP + 1 serena)", n)
	}
	lspOnly := reg.LSPEntries()
	if len(lspOnly) != 3 {
		t.Errorf("LSPEntries len = %d, want 3", len(lspOnly))
	}
	for _, e := range lspOnly {
		if e.Language == SerenaLanguageSentinel {
			t.Errorf("LSPEntries leaked sentinel row: %+v", e)
		}
	}
	serenaOnly := reg.SerenaEntries()
	if len(serenaOnly) != 1 {
		t.Errorf("SerenaEntries len = %d, want 1", len(serenaOnly))
	}
	if len(serenaOnly) == 1 && serenaOnly[0].Language != SerenaLanguageSentinel {
		t.Errorf("SerenaEntries[0].Language = %q, want %q", serenaOnly[0].Language, SerenaLanguageSentinel)
	}
	lspByWs := reg.ListByWorkspaceLSP("abcd1234")
	if len(lspByWs) != 3 {
		t.Errorf("ListByWorkspaceLSP len = %d, want 3", len(lspByWs))
	}
	for _, e := range lspByWs {
		if e.Language == SerenaLanguageSentinel {
			t.Errorf("ListByWorkspaceLSP leaked sentinel row: %+v", e)
		}
	}
	if got, ok := reg.GetSerena("abcd1234"); !ok || got.TaskName != "serena-abcd1234" {
		t.Errorf("GetSerena = %+v ok=%v, want serena-abcd1234", got, ok)
	}
}

func TestRegistry_AllocateSerenaPort_FirstFreeFromPool(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "workspaces.yaml"))
	pool := config.PortPool{Start: 9500, End: 9509}
	p, err := reg.AllocateSerenaPort(pool)
	if err != nil {
		t.Fatalf("AllocateSerenaPort on empty registry: %v", err)
	}
	if p != 9500 {
		t.Errorf("first allocation = %d, want 9500", p)
	}
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey: "a", WorkspacePath: "c:/a", Language: SerenaLanguageSentinel,
		Backend: "serena", Port: 9500, TaskName: "ta",
	}); err != nil {
		t.Fatalf("PutSerena seed: %v", err)
	}
	p2, err := reg.AllocateSerenaPort(pool)
	if err != nil {
		t.Fatalf("AllocateSerenaPort after seed: %v", err)
	}
	if p2 != 9501 {
		t.Errorf("second allocation = %d, want 9501", p2)
	}
	if err := reg.PutLSP(WorkspaceEntry{
		WorkspaceKey: "b", WorkspacePath: "c:/b", Language: "python",
		Backend: "mcp-language-server", Port: 9501, TaskName: "tb",
	}); err != nil {
		t.Fatalf("PutLSP seed: %v", err)
	}
	p3, err := reg.AllocateSerenaPort(pool)
	if err != nil {
		t.Fatalf("AllocateSerenaPort with mixed seeds: %v", err)
	}
	if p3 != 9502 {
		t.Errorf("third allocation = %d, want 9502 (skip 9500=serena, 9501=lsp)", p3)
	}
}

func TestRegistry_AllocateSerenaPort_ExhaustionReturnsError(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "workspaces.yaml"))
	pool := config.PortPool{Start: 9500, End: 9501}
	for i, p := range []int{9500, 9501} {
		key := []string{"a", "b"}[i]
		if err := reg.PutSerena(WorkspaceEntry{
			WorkspaceKey: key, WorkspacePath: "c:/" + key, Language: SerenaLanguageSentinel,
			Backend: "serena", Port: p, TaskName: "t" + key,
		}); err != nil {
			t.Fatalf("seed %d: %v", p, err)
		}
	}
	_, err := reg.AllocateSerenaPort(pool)
	if err == nil {
		t.Fatal("expected ErrPortPoolExhausted when pool full")
	}
	if !errors.Is(err, ErrPortPoolExhausted) {
		t.Errorf("err should unwrap to ErrPortPoolExhausted; got: %v", err)
	}
	if _, err := reg.AllocateSerenaPort(config.PortPool{Start: 0, End: 0}); err == nil {
		t.Error("invalid pool {start=0,end=0}: want error, got nil")
	}
}

func TestRegistry_LegacyEntryReadAccepted(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "workspaces.yaml")
	legacy := []byte(`version: 1
workspaces:
  - workspace_key: abcd1234
    workspace_path: c:/ws/foo
    language: python
    backend: mcp-language-server
    port: 9200
    task_name: mcp-local-hub-lsp-abcd1234-python
    client_entries:
      codex-cli: mcp-language-server-python
    weekly_refresh: true
`)
	if err := WriteStateFileBytesLockHeld(path, legacy); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(path)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load legacy: %v", err)
	}
	if len(reg.Workspaces) != 1 {
		t.Fatalf("Workspaces len = %d, want 1", len(reg.Workspaces))
	}
	e := reg.Workspaces[0]
	if e.WorkspaceKey != "abcd1234" || e.Language != "python" || e.Port != 9200 {
		t.Errorf("legacy entry corrupted: %+v", e)
	}
	if !e.RegisteredAt.IsZero() {
		t.Errorf("RegisteredAt should be zero on legacy entry, got %v", e.RegisteredAt)
	}
	if e.RegisteredVia != "" {
		t.Errorf("RegisteredVia should be empty on legacy entry, got %q", e.RegisteredVia)
	}
	if len(e.Languages) != 0 {
		t.Errorf("Languages should be empty on legacy entry, got %v", e.Languages)
	}
}

func TestWorkspaceRegistryConsumers_ClassifyByBackend(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "workspaces.yaml"))
	lsps := []string{"go", "python", "typescript"}
	for i, lang := range lsps {
		if err := reg.PutLSP(WorkspaceEntry{
			WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: lang,
			Backend: "mcp-language-server", Port: 9200 + i, TaskName: "t-" + lang,
		}); err != nil {
			t.Fatalf("PutLSP %q: %v", lang, err)
		}
	}
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: SerenaLanguageSentinel,
		Backend: "serena", Port: 9500, TaskName: "t-serena",
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}
	if got := reg.LSPEntries(); len(got) != 3 {
		t.Errorf("LSPEntries: got %d rows, want 3", len(got))
	}
	if got := reg.ListByWorkspaceLSP("abcd1234"); len(got) != 3 {
		t.Errorf("ListByWorkspaceLSP: got %d rows, want 3", len(got))
	}
	taken := reg.AllocatedPorts()
	if !taken[9500] {
		t.Errorf("AllocatedPorts missing serena port 9500; got %v", taken)
	}
	if got := reg.ListByWorkspace("abcd1234"); len(got) != 4 {
		t.Errorf("ListByWorkspace (safe-include): got %d rows, want 4", len(got))
	}
	if got := reg.SerenaEntries(); len(got) != 1 {
		t.Errorf("SerenaEntries: got %d, want 1", len(got))
	}
}

func TestRegistry_Unregister_DefaultBackendSemantics(t *testing.T) {
	seed := func(t *testing.T) *Registry {
		t.Helper()
		reg := NewRegistry(filepath.Join(t.TempDir(), "workspaces.yaml"))
		for i, lang := range []string{"go", "python"} {
			if err := reg.PutLSP(WorkspaceEntry{
				WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: lang,
				Backend: "mcp-language-server", Port: 9200 + i, TaskName: "t-" + lang,
			}); err != nil {
				t.Fatalf("PutLSP %q: %v", lang, err)
			}
		}
		if err := reg.PutLSP(WorkspaceEntry{
			WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: "go-gopls",
			Backend: "gopls-mcp", Port: 9202, TaskName: "t-gopls",
		}); err != nil {
			t.Fatalf("PutLSP gopls: %v", err)
		}
		if err := reg.PutSerena(WorkspaceEntry{
			WorkspaceKey: "abcd1234", WorkspacePath: "c:/ws", Language: SerenaLanguageSentinel,
			Backend: "serena", Port: 9500, TaskName: "t-serena",
		}); err != nil {
			t.Fatalf("PutSerena: %v", err)
		}
		return reg
	}
	t.Run("default removes LSP, sentinel preserved", func(t *testing.T) {
		reg := seed(t)
		n := reg.RemoveByBackend("abcd1234", "")
		if n != 3 {
			t.Errorf("removed = %d, want 3 (2 LSP + 1 gopls)", n)
		}
		if _, ok := reg.GetSerena("abcd1234"); !ok {
			t.Error("serena row was removed; default backend filter must preserve it")
		}
		if got := reg.LSPEntries(); len(got) != 0 {
			t.Errorf("LSPEntries after default removal: got %d, want 0", len(got))
		}
	})
	t.Run("backend=serena removes only sentinel", func(t *testing.T) {
		reg := seed(t)
		n := reg.RemoveByBackend("abcd1234", "serena")
		if n != 1 {
			t.Errorf("removed = %d, want 1", n)
		}
		if _, ok := reg.GetSerena("abcd1234"); ok {
			t.Error("serena row should be gone")
		}
		if got := reg.LSPEntries(); len(got) != 3 {
			t.Errorf("LSP rows should survive; got %d, want 3", len(got))
		}
	})
	t.Run("backend=all removes every row", func(t *testing.T) {
		reg := seed(t)
		n := reg.RemoveByBackend("abcd1234", "all")
		if n != 4 {
			t.Errorf("removed = %d, want 4", n)
		}
		if got := reg.ListByWorkspace("abcd1234"); len(got) != 0 {
			t.Errorf("residual rows after --backend all: %d", len(got))
		}
	})
	t.Run("backend=mcp-language-server narrows by Backend field", func(t *testing.T) {
		reg := seed(t)
		n := reg.RemoveByBackend("abcd1234", "mcp-language-server")
		if n != 2 {
			t.Errorf("removed = %d, want 2 (only mcp-language-server rows)", n)
		}
		if _, ok := reg.Get("abcd1234", "go-gopls"); !ok {
			t.Error("gopls-mcp row must survive backend=mcp-language-server")
		}
		if _, ok := reg.GetSerena("abcd1234"); !ok {
			t.Error("serena row must survive backend=mcp-language-server")
		}
	})
	t.Run("removal on other workspaces is a no-op", func(t *testing.T) {
		reg := seed(t)
		n := reg.RemoveByBackend("nonexistent", "all")
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
		if got := reg.ListByWorkspace("abcd1234"); len(got) != 4 {
			t.Errorf("seed should survive; got %d, want 4", len(got))
		}
	})
}

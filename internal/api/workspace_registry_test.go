package api

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
		WorkspacePath: "c:/users/dima/projects/foo",
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

// TestRegistry_SaveBacksUpPreMutationFile verifies that Save() preserves the
// prior file contents as a rolling .bak before overwriting. This is the
// recovery mechanism; it does not simulate a crash — it simply asserts the
// backup-before-write primitive that makes crash recovery possible.
func TestRegistry_SaveBacksUpPreMutationFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspaces.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nworkspaces:\n  - workspace_key: oldentry\n    language: python\n    port: 9200\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Attempt to write invalid YAML via the atomic helper — simulate by passing
	// bytes that round-trip fine but rename must succeed. We assert the bak
	// file exists AFTER a successful save, proving pre-mutate backup works.
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
	// .bak must exist + contain the old entry.
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read bak: %v", err)
	}
	if !bytes.Contains(bak, []byte("oldentry")) {
		t.Errorf("bak missing old entry; got %s", bak)
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
		defer unlock()
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

func TestRegistry_LastErrorTruncation(t *testing.T) {
	reg := NewRegistry(t.TempDir() + "/r.yaml")
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

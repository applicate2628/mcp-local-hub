package lsp_routing

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

func TestRefreshCapturesEntriesBeforeRegistryRelease(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), "resolver.go"))
	if err != nil {
		t.Fatalf("ReadFile resolver.go: %v", err)
	}
	capture := bytes.Index(src, []byte("entries = r.reg.LSPEntries()"))
	release := bytes.Index(src, []byte("releaseErr := unlock()"))
	if capture < 0 || release < 0 {
		t.Fatalf("refresh ordering anchors missing: capture=%d release=%d", capture, release)
	}
	if capture > release {
		t.Fatal("refresh releases the registry lock before capturing LSP entries")
	}
}

func TestRefreshSnapshotsRegistryBeforeUnlock(t *testing.T) {
	dir := t.TempDir()
	entries := make([]api.WorkspaceEntry, 512)
	for i := range entries {
		entries[i] = api.WorkspaceEntry{
			WorkspaceKey:  fmt.Sprintf("workspace-%04d", i),
			WorkspacePath: filepath.Join(dir, fmt.Sprintf("workspace-%04d", i)),
			Language:      "go",
			Port:          9200 + i,
		}
	}
	regPath := makeRegistryWithLSP(t, dir, entries)
	reg := api.NewRegistry(regPath)
	resolver := NewWorkspaceResolver(reg, regPath, testLanguages())

	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 8; iteration++ {
				resolver.mu.Lock()
				resolver.loaded = false
				resolver.mu.Unlock()
				resolver.refresh()
			}
		}()
	}
	wg.Wait()

	resolver.mu.RLock()
	got := len(resolver.cached)
	resolver.mu.RUnlock()
	if got != len(entries) {
		t.Fatalf("cached entries = %d, want %d", got, len(entries))
	}
}

func testLanguages() []config.LanguageSpec {
	return []config.LanguageSpec{
		{Name: "go", Backend: "gopls-mcp", ProjectMarkers: []string{"go.mod"}},
		{Name: "rust", Backend: "mcp-language-server", ProjectMarkers: []string{"Cargo.toml"}},
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustCanonical(t *testing.T, path string) string {
	t.Helper()
	canon, err := api.CanonicalWorkspacePath(path)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath(%q): %v", path, err)
	}
	return canon
}

func makeRegistryWithLSP(t *testing.T, dir string, entries []api.WorkspaceEntry) string {
	t.Helper()
	regPath := filepath.Join(dir, "workspaces.yaml")
	reg := api.NewRegistry(regPath)
	for _, e := range entries {
		if err := reg.PutLSP(e); err != nil {
			t.Fatalf("PutLSP: %v", err)
		}
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return regPath
}

func TestResolveByPath_GoMarkerReturnsCanonicalRootWithoutRegistration(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "go-project")
	writeFile(t, filepath.Join(project, "go.mod"))
	file := filepath.Join(project, "cmd", "app", "main.go")
	writeFile(t, file)

	regPath := makeRegistryWithLSP(t, root, nil)
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath, testLanguages())

	got, err := resolver.ResolveByPath(file, "go")
	if err != nil {
		t.Fatalf("ResolveByPath: %v", err)
	}
	if got.WorkspaceRoot != mustCanonical(t, project) {
		t.Fatalf("WorkspaceRoot = %q, want %q", got.WorkspaceRoot, mustCanonical(t, project))
	}
	if got.Registered {
		t.Fatalf("Registered = true, want false for registry miss")
	}
	if got.Entry != nil {
		t.Fatalf("Entry = %+v, want nil on registry miss", got.Entry)
	}
}

func TestResolveByPath_NestedLanguageMarkerWins(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "mono")
	child := filepath.Join(parent, "services", "api")
	writeFile(t, filepath.Join(parent, "go.mod"))
	writeFile(t, filepath.Join(child, "go.mod"))
	file := filepath.Join(child, "pkg", "handler.go")
	writeFile(t, file)

	resolver := NewWorkspaceResolver(nil, "", testLanguages())
	got, err := resolver.ResolveByPath(file, "go")
	if err != nil {
		t.Fatalf("ResolveByPath: %v", err)
	}
	if got.WorkspaceRoot != mustCanonical(t, child) {
		t.Fatalf("WorkspaceRoot = %q, want nearest marker root %q", got.WorkspaceRoot, mustCanonical(t, child))
	}
}

func TestResolveByPath_RustCargoTomlMatchesExistingRegistration(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "rust-project")
	writeFile(t, filepath.Join(project, "Cargo.toml"))
	file := filepath.Join(project, "src", "lib.rs")
	writeFile(t, file)
	canon := mustCanonical(t, project)

	regPath := makeRegistryWithLSP(t, root, []api.WorkspaceEntry{
		{WorkspaceKey: api.WorkspaceKey(canon), WorkspacePath: canon, Language: "rust", Backend: "mcp-language-server", Port: 9207},
	})
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath, testLanguages())

	got, err := resolver.ResolveByPath(file, "rust")
	if err != nil {
		t.Fatalf("ResolveByPath: %v", err)
	}
	if !got.Registered {
		t.Fatalf("Registered = false, want true")
	}
	if got.Entry == nil {
		t.Fatal("Entry = nil, want matching rust row")
	}
	if got.Entry.Language != "rust" || got.Entry.Port != 9207 {
		t.Fatalf("Entry = %+v, want rust port 9207", got.Entry)
	}
}

func TestResolveByPath_MatchesLegacySymlinkWorkspaceKey(t *testing.T) {
	root := t.TempDir()
	realProject := filepath.Join(root, "real-project")
	writeFile(t, filepath.Join(realProject, "go.mod"))
	file := filepath.Join(realProject, "cmd", "app", "main.go")
	writeFile(t, file)
	aliasProject := filepath.Join(root, "alias-project")
	if err := os.Symlink(realProject, aliasProject); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	aliasFile := filepath.Join(aliasProject, "cmd", "app", "main.go")
	legacyPath, err := api.CanonicalWorkspacePathLegacyCompat(aliasProject)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePathLegacyCompat: %v", err)
	}
	legacyKey := api.WorkspaceKey(legacyPath)
	canon := mustCanonical(t, aliasProject)
	if legacyKey == api.WorkspaceKey(canon) {
		t.Skip("legacy and symlink-resolved workspace keys are identical")
	}

	regPath := makeRegistryWithLSP(t, root, []api.WorkspaceEntry{
		{
			WorkspaceKey:  legacyKey,
			WorkspacePath: legacyPath,
			Language:      "go",
			Backend:       "gopls-mcp",
			Port:          9221,
			TaskName:      "mcp-local-hub-lsp-" + legacyKey + "-go",
		},
	})
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath, testLanguages())

	got, err := resolver.ResolveByPath(aliasFile, "go")
	if err != nil {
		t.Fatalf("ResolveByPath: %v", err)
	}
	if !got.Registered {
		t.Fatal("Registered = false, want true via legacy symlink key")
	}
	if got.Entry == nil {
		t.Fatal("Entry = nil, want legacy registry row")
	}
	if got.Entry.WorkspaceKey != legacyKey || got.Entry.Port != 9221 {
		t.Fatalf("Entry = %+v, want legacy key %s port 9221", got.Entry, legacyKey)
	}
	if got.WorkspaceKey != legacyKey {
		t.Fatalf("WorkspaceKey = %q, want matched legacy key %q", got.WorkspaceKey, legacyKey)
	}
}

func TestResolveByPath_RegisteredMatchIsLanguageSpecific(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "mixed-project")
	writeFile(t, filepath.Join(project, "Cargo.toml"))
	file := filepath.Join(project, "src", "lib.rs")
	writeFile(t, file)
	canon := mustCanonical(t, project)

	regPath := makeRegistryWithLSP(t, root, []api.WorkspaceEntry{
		{WorkspaceKey: api.WorkspaceKey(canon), WorkspacePath: canon, Language: "go", Backend: "gopls-mcp", Port: 9201},
	})
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath, testLanguages())

	got, err := resolver.ResolveByPath(file, "rust")
	if err != nil {
		t.Fatalf("ResolveByPath: %v", err)
	}
	if got.Registered {
		t.Fatalf("Registered = true, want false because only go is registered")
	}
	if got.Entry != nil {
		t.Fatalf("Entry = %+v, want nil for language-specific miss", got.Entry)
	}
}

func TestResolveByPath_GitFallbackWhenNoLanguageMarkerExists(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "plain-git")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	file := filepath.Join(project, "src", "main.go")
	writeFile(t, file)

	resolver := NewWorkspaceResolver(nil, "", testLanguages())
	got, err := resolver.ResolveByPath(file, "go")
	if err != nil {
		t.Fatalf("ResolveByPath: %v", err)
	}
	if got.WorkspaceRoot != mustCanonical(t, project) {
		t.Fatalf("WorkspaceRoot = %q, want git fallback root %q", got.WorkspaceRoot, mustCanonical(t, project))
	}
}

func TestResolveByPath_NoMarkerOrGitReturnsWorkspaceNotFound(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "loose", "main.go")
	writeFile(t, file)

	resolver := NewWorkspaceResolver(nil, "", testLanguages())
	got, err := resolver.ResolveByPath(file, "go")
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("err = %v, want ErrWorkspaceNotFound", err)
	}
	if got != nil {
		t.Fatalf("result = %+v, want nil on not found", got)
	}
}

func TestResolveByPath_RefreshesRegistryOnMtimeAdvance(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "reload-project")
	writeFile(t, filepath.Join(project, "go.mod"))
	file := filepath.Join(project, "main.go")
	writeFile(t, file)
	canon := mustCanonical(t, project)

	regPath := makeRegistryWithLSP(t, root, nil)
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath, testLanguages())

	first, err := resolver.ResolveByPath(file, "go")
	if err != nil {
		t.Fatalf("first ResolveByPath: %v", err)
	}
	if first.Registered {
		t.Fatal("first Registered = true, want false before registry row")
	}

	if err := reg.PutLSP(api.WorkspaceEntry{
		WorkspaceKey:  api.WorkspaceKey(canon),
		WorkspacePath: canon,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9203,
	}); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save updated registry: %v", err)
	}
	nextMtime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(regPath, nextMtime, nextMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	second, err := resolver.ResolveByPath(file, "go")
	if err != nil {
		t.Fatalf("second ResolveByPath: %v", err)
	}
	if !second.Registered {
		t.Fatal("second Registered = false, want true after registry reload")
	}
	if second.Entry == nil || second.Entry.Port != 9203 {
		t.Fatalf("second Entry = %+v, want port 9203", second.Entry)
	}
}

// TestNewReadOnlyWorkspaceResolver_NeverBlocksOnConcurrentExclusiveLock is the
// LSP twin of serena_routing's identically-named test — the P2-3 falsifying
// test (adversarial cross-family review of Increment 1). See that package's
// doc comment for the full safety argument. This test holds the registry's
// exclusive lock externally (simulating a concurrent GUI mutation) and proves
// a read-only LSP resolver's refresh still completes instead of blocking.
//
// Mutation-proven: constructing via NewWorkspaceResolver (the locked path)
// instead of NewReadOnlyWorkspaceResolver reproduces the pre-fix blocking
// behavior — see TestNewWorkspaceResolver_BlocksOnConcurrentExclusiveLock
// immediately below for the contrasting, intentional production behavior.
func TestNewReadOnlyWorkspaceResolver_NeverBlocksOnConcurrentExclusiveLock(t *testing.T) {
	root := t.TempDir()
	regPath := filepath.Join(root, "workspaces.yaml")
	seedReg := api.NewRegistry(regPath)
	if err := seedReg.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewReadOnlyWorkspaceResolver(reg, regPath, testLanguages())
	writeFile(t, filepath.Join(root, "anything", "go.mod"))
	loose := filepath.Join(root, "anything", "main.go")
	writeFile(t, loose)
	if _, err := resolver.ResolveByPath(loose, "go"); err != nil && !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("prime resolve: %v", err)
	}

	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(regPath, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	holderReg := api.NewRegistry(regPath)
	unlock, err := holderReg.Lock()
	if err != nil {
		t.Fatalf("acquire holder lock: %v", err)
	}
	defer unlock()

	done := make(chan struct{})
	go func() {
		_, _ = resolver.ResolveByPath(loose, "go")
		close(done)
	}()
	select {
	case <-done:
		// Correct: the read-only resolver never contended for the lock.
	case <-time.After(3 * time.Second):
		t.Fatal("read-only LSP resolver blocked on a concurrently-held exclusive registry lock — it must reload unlocked, not contend with a GUI writer")
	}
}

// TestNewWorkspaceResolver_BlocksOnConcurrentExclusiveLock pins the
// CONTRASTING, intentional production behavior: the ordinary (non-read-only)
// LSP resolver's refresh DOES take the registry's exclusive lock. Mirrors
// the serena_routing package's identically-named test.
func TestNewWorkspaceResolver_BlocksOnConcurrentExclusiveLock(t *testing.T) {
	root := t.TempDir()
	regPath := filepath.Join(root, "workspaces.yaml")
	seedReg := api.NewRegistry(regPath)
	if err := seedReg.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath, testLanguages())
	writeFile(t, filepath.Join(root, "anything", "go.mod"))
	loose := filepath.Join(root, "anything", "main.go")
	writeFile(t, loose)
	if _, err := resolver.ResolveByPath(loose, "go"); err != nil && !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("prime resolve: %v", err)
	}

	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(regPath, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	holderReg := api.NewRegistry(regPath)
	unlock, err := holderReg.Lock()
	if err != nil {
		t.Fatalf("acquire holder lock: %v", err)
	}

	done := make(chan struct{})
	go func() {
		_, _ = resolver.ResolveByPath(loose, "go")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("production LSP resolver did NOT block on a concurrently-held exclusive registry lock — expected it to wait")
	case <-time.After(300 * time.Millisecond):
		// Correct: still blocked after a short wait.
	}
	unlock()
	select {
	case <-done:
		// Correct: unblocks once the holder releases.
	case <-time.After(3 * time.Second):
		t.Fatal("production LSP resolver never unblocked after the holder released the lock")
	}
}

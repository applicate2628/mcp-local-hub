package lsp_routing

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

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

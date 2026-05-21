package serena_routing

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func makeWorkspace(t *testing.T, root, name string) string {
	t.Helper()
	wsPath := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(wsPath, ".serena"), 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	marker := filepath.Join(wsPath, ".serena", "project.yml")
	if err := os.WriteFile(marker, []byte("project_name: "+name+"\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	canon, err := api.CanonicalWorkspacePath(wsPath)
	if err != nil {
		t.Fatalf("canonicalize %s: %v", wsPath, err)
	}
	return canon
}

func makeRegistryWithSerena(t *testing.T, dir string, entries []api.WorkspaceEntry) string {
	t.Helper()
	regPath := filepath.Join(dir, "workspaces.yaml")
	reg := api.NewRegistry(regPath)
	for _, e := range entries {
		e.Language = api.SerenaLanguageSentinel
		if err := reg.PutSerena(e); err != nil {
			t.Fatalf("PutSerena: %v", err)
		}
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return regPath
}

func TestResolveByPath_AbsoluteMatch(t *testing.T) {
	root := t.TempDir()
	wsPath := makeWorkspace(t, root, "PaperPane")

	regPath := makeRegistryWithSerena(t, root, []api.WorkspaceEntry{
		{
			WorkspaceKey:  api.WorkspaceKey(wsPath),
			WorkspacePath: wsPath,
			Backend:       "serena",
			Port:          9301,
			TaskName:      "mcp-local-hub-serena-paperpane",
		},
	})

	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath)

	nested := filepath.Join(wsPath, "src", "foo.cpp")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(nested, []byte("// hi"), 0o644); err != nil {
		t.Fatalf("write nested: %v", err)
	}

	entry, err := resolver.ResolveByPath(nested)
	if err != nil {
		t.Fatalf("ResolveByPath returned error: %v", err)
	}
	if entry == nil {
		t.Fatal("ResolveByPath returned nil entry without error")
	}
	if entry.Port != 9301 {
		t.Errorf("Port = %d, want 9301", entry.Port)
	}
	if entry.WorkspacePath != wsPath {
		t.Errorf("WorkspacePath = %q, want %q", entry.WorkspacePath, wsPath)
	}
	if entry.Language != api.SerenaLanguageSentinel {
		t.Errorf("Language = %q, want %q", entry.Language, api.SerenaLanguageSentinel)
	}
}

func TestResolveByPath_RelativeMatchFirstWorkspace(t *testing.T) {
	root := t.TempDir()
	wsAlpha := makeWorkspace(t, root, "Alpha")
	wsBeta := makeWorkspace(t, root, "Beta")
	wsGamma := makeWorkspace(t, root, "Gamma")

	// Only Alpha (alphabetically first) has src/foo.cpp present on disk.
	if err := os.MkdirAll(filepath.Join(wsAlpha, "src"), 0o755); err != nil {
		t.Fatalf("mkdir alpha src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsAlpha, "src", "foo.cpp"), []byte(""), 0o644); err != nil {
		t.Fatalf("write alpha foo: %v", err)
	}

	regPath := makeRegistryWithSerena(t, root, []api.WorkspaceEntry{
		// Insert in non-alphabetic order to prove the resolver sorts.
		{WorkspaceKey: api.WorkspaceKey(wsGamma), WorkspacePath: wsGamma, Backend: "serena", Port: 9303},
		{WorkspaceKey: api.WorkspaceKey(wsBeta), WorkspacePath: wsBeta, Backend: "serena", Port: 9302},
		{WorkspaceKey: api.WorkspaceKey(wsAlpha), WorkspacePath: wsAlpha, Backend: "serena", Port: 9301},
	})

	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath)

	entry, err := resolver.ResolveByPath(filepath.Join("src", "foo.cpp"))
	if err != nil {
		t.Fatalf("ResolveByPath: %v", err)
	}
	if entry == nil {
		t.Fatal("nil entry without error")
	}
	if entry.Port != 9301 {
		t.Errorf("Port = %d, want 9301 (Alpha first by sort order)", entry.Port)
	}
	if entry.WorkspacePath != wsAlpha {
		t.Errorf("WorkspacePath = %q, want %q", entry.WorkspacePath, wsAlpha)
	}
}

func TestResolveByPath_RelativePathDotDotEscapeRejected(t *testing.T) {
	root := t.TempDir()
	wsAlpha := makeWorkspace(t, root, "Alpha")
	wsBeta := makeWorkspace(t, root, "Beta")

	betaFile := filepath.Join(wsBeta, "file.cpp")
	if err := os.WriteFile(betaFile, []byte(""), 0o644); err != nil {
		t.Fatalf("write beta file: %v", err)
	}
	externalFile := filepath.Join(root, "external", "foo")
	if err := os.MkdirAll(filepath.Dir(externalFile), 0o755); err != nil {
		t.Fatalf("mkdir external: %v", err)
	}
	if err := os.WriteFile(externalFile, []byte(""), 0o644); err != nil {
		t.Fatalf("write external file: %v", err)
	}

	regPath := makeRegistryWithSerena(t, root, []api.WorkspaceEntry{
		{WorkspaceKey: api.WorkspaceKey(wsBeta), WorkspacePath: wsBeta, Backend: "serena", Port: 9302},
		{WorkspaceKey: api.WorkspaceKey(wsAlpha), WorkspacePath: wsAlpha, Backend: "serena", Port: 9301},
	})

	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath)

	entry, err := resolver.ResolveByPath(filepath.Join("..", "Beta", "file.cpp"))
	if err != nil {
		t.Fatalf("ResolveByPath beta escape: %v", err)
	}
	if entry == nil {
		t.Fatal("nil entry without error")
	}
	if entry.WorkspacePath == wsAlpha {
		t.Fatalf("ResolveByPath returned Alpha for escaped relative path; want Beta")
	}
	if entry.WorkspacePath != wsBeta {
		t.Fatalf("WorkspacePath = %q, want %q", entry.WorkspacePath, wsBeta)
	}

	entry, err = resolver.ResolveByPath(filepath.Join("..", "external", "foo"))
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("ResolveByPath external escape err = %v, want ErrWorkspaceNotFound", err)
	}
	if entry != nil {
		t.Fatalf("ResolveByPath external escape entry = %+v, want nil", entry)
	}
}

func TestResolveByPath_NoMatch_ReturnsErrWorkspaceNotFound(t *testing.T) {
	root := t.TempDir()
	wsPath := makeWorkspace(t, root, "Project")
	regPath := makeRegistryWithSerena(t, root, []api.WorkspaceEntry{
		{
			WorkspaceKey:  api.WorkspaceKey(wsPath),
			WorkspacePath: wsPath,
			Backend:       "serena",
			Port:          9301,
		},
	})
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath)

	unrelated := filepath.Join(t.TempDir(), "elsewhere", "nested", "file.txt")
	if err := os.MkdirAll(filepath.Dir(unrelated), 0o755); err != nil {
		t.Fatalf("mkdir unrelated: %v", err)
	}
	if err := os.WriteFile(unrelated, []byte(""), 0o644); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}

	entry, err := resolver.ResolveByPath(unrelated)
	if err == nil {
		t.Fatalf("expected ErrWorkspaceNotFound, got entry=%+v", entry)
	}
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("err = %v, want ErrWorkspaceNotFound", err)
	}
	if entry != nil {
		t.Errorf("entry = %+v on error, want nil", entry)
	}
}

func TestAncestorWalk_FindsProjectYml(t *testing.T) {
	root := t.TempDir()
	wsPath := makeWorkspace(t, root, "DeepProject")

	deep := filepath.Join(wsPath, "src", "a", "b", "c", "d", "foo.cpp")
	if err := os.MkdirAll(filepath.Dir(deep), 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}
	if err := os.WriteFile(deep, []byte(""), 0o644); err != nil {
		t.Fatalf("write deep: %v", err)
	}

	resolver := NewWorkspaceResolver(nil, "")
	got, err := resolver.AncestorWalk(deep)
	if err != nil {
		t.Fatalf("AncestorWalk: %v", err)
	}

	canonGot, err := api.CanonicalWorkspacePath(got)
	if err != nil {
		t.Fatalf("canon got %s: %v", got, err)
	}
	if canonGot != wsPath {
		t.Errorf("AncestorWalk = %q (canon %q), want %q", got, canonGot, wsPath)
	}
}

func TestAncestorWalk_NoMarker_ReturnsErrWorkspaceNotFound(t *testing.T) {
	root := t.TempDir()
	noMarker := filepath.Join(root, "alone", "deep", "file.txt")
	if err := os.MkdirAll(filepath.Dir(noMarker), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(noMarker, []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resolver := NewWorkspaceResolver(nil, "")
	_, err := resolver.AncestorWalk(noMarker)
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("err = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestAncestorWalk_RejectsRelativeInput(t *testing.T) {
	resolver := NewWorkspaceResolver(nil, "")
	_, err := resolver.AncestorWalk("relative/path.txt")
	if !errors.Is(err, ErrInvalidPath) {
		t.Errorf("err = %v, want ErrInvalidPath", err)
	}
}

func TestResolveByPath_EmptyPath_ReturnsErrInvalidPath(t *testing.T) {
	resolver := NewWorkspaceResolver(nil, "")
	_, err := resolver.ResolveByPath("")
	if !errors.Is(err, ErrInvalidPath) {
		t.Errorf("err = %v, want ErrInvalidPath", err)
	}
}

func TestResolveByPath_EmptyRegistry_ReturnsErrWorkspaceNotFound(t *testing.T) {
	root := t.TempDir()
	regPath := filepath.Join(root, "workspaces.yaml")
	reg := api.NewRegistry(regPath)
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath)

	_, err := resolver.ResolveByPath(filepath.Join(root, "anything"))
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("err = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestResolveByPath_IgnoresLSPEntries(t *testing.T) {
	// SerenaEntries() filters Language == SerenaLanguageSentinel; a row
	// with Language == "python" must not match the absolute-path branch.
	root := t.TempDir()
	wsPath := makeWorkspace(t, root, "OnlyLSP")

	regPath := filepath.Join(root, "workspaces.yaml")
	reg := api.NewRegistry(regPath)
	if err := reg.PutLSP(api.WorkspaceEntry{
		WorkspaceKey:  api.WorkspaceKey(wsPath),
		WorkspacePath: wsPath,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9200,
	}); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolver := NewWorkspaceResolver(reg, regPath)

	nested := filepath.Join(wsPath, "main.py")
	if err := os.WriteFile(nested, []byte(""), 0o644); err != nil {
		t.Fatalf("write nested: %v", err)
	}

	_, err := resolver.ResolveByPath(nested)
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("err = %v, want ErrWorkspaceNotFound (LSP row must be filtered out)", err)
	}
}

func TestCanonicalizeWorkspacePath_WindowsDriveLetter(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific drive-letter normalization")
	}
	got := canonicalizeWorkspacePath("C:" + string(filepath.Separator) + "Users")
	if len(got) < 2 || got[0] != 'c' {
		t.Errorf("canonicalizeWorkspacePath drive letter not lowercased: %q", got)
	}
	if !strings.HasPrefix(strings.ToLower(got), "c:") {
		t.Errorf("canonicalizeWorkspacePath = %q, want c:-prefixed", got)
	}
}

func TestCanonicalizeWorkspacePath_EmptyReturnsEmpty(t *testing.T) {
	if got := canonicalizeWorkspacePath(""); got != "" {
		t.Errorf("canonicalize(empty) = %q, want empty", got)
	}
}

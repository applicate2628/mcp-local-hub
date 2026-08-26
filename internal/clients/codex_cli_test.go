package clients

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func setupCodexConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func createCodexJunctionForTest(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("Windows junction coverage")
	}
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v: %s", err, strings.TrimSpace(string(output)))
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove junction %s: %v", link, err)
		}
	})
}

func writeCodexProjectConfigForTest(t *testing.T, projectRoot string) string {
	t.Helper()
	path := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[mcp_servers.serena-latest]\ncommand = 'uvx'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertCodexLayerRefusalDidNotMutate(t *testing.T, path string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read unchanged fixture: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("Codex layer refusal mutated %s", filepath.Base(path))
	}
}

func TestCodexCLI_AddEntry_ReplaceStdioBlock(t *testing.T) {
	initial := `[mcp_servers.serena]
command = "uvx"
args = ["--from", "git+...", "serena", "start-mcp-server"]
startup_timeout_sec = 30.0

[mcp_servers.other]
command = "echo"
args = ["hi"]
`
	path := setupCodexConfig(t, initial)
	c := &codexCLI{path: path}

	err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9122/mcp"})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(path)
	s := string(raw)
	// TOML accepts both "basic" (double-quoted) and "literal" (single-quoted) strings;
	// go-toml/v2 emits literal strings for simple ASCII. Accept either.
	if !strings.Contains(s, `url = "http://localhost:9122/mcp"`) && !strings.Contains(s, `url = 'http://localhost:9122/mcp'`) {
		t.Errorf("URL not set: %s", s)
	}
	// Verify old `command` field was removed from the serena block (wholesale replace).
	// Quote-agnostic check: just look for the key name inside the block.
	serenaStart := strings.Index(s, "[mcp_servers.serena]")
	otherStart := strings.Index(s, "[mcp_servers.other]")
	if serenaStart >= 0 && otherStart > serenaStart {
		if strings.Contains(s[serenaStart:otherStart], "command") {
			t.Errorf("old command field not removed from serena block:\n%s", s[serenaStart:otherStart])
		}
	}
	// Other section preserved
	if !strings.Contains(s, "[mcp_servers.other]") {
		t.Error("other section dropped")
	}
}

func TestCodexCLI_RemoveEntry(t *testing.T) {
	initial := `[mcp_servers.serena]
url = "http://localhost:9122/mcp"

[mcp_servers.memory]
url = "http://localhost:9140/mcp"
`
	path := setupCodexConfig(t, initial)
	c := &codexCLI{path: path}
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "serena") {
		t.Errorf("serena not removed: %s", raw)
	}
	if !strings.Contains(string(raw), "memory") {
		t.Error("memory also removed (should be preserved)")
	}
}

func TestCodexCollisionRelocatesOnlyGlobalHTTPEntry(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "global", "config.toml")
	projectPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	global := `[mcp_servers.serena-latest]
url = "http://127.0.0.1:9304/mcp"
enabled = false

[mcp_servers.unrelated]
url = "http://127.0.0.1:9305/mcp"
`
	project := `[mcp_servers.serena-latest]
command = "uvx"
args = ["serena", "start-mcp-server"]
enabled = true
`
	if err := os.WriteFile(globalPath, []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	projectBefore, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}

	c := &codexCLI{path: globalPath}
	resolution, err := c.ResolveTransportTarget(CodexTransportTargetRequest{LogicalEntryName: "serena-latest", DesiredTransport: CodexTransportHTTP, ProjectRoot: root, WorkingDir: root})
	if err != nil {
		t.Fatalf("ResolveTransportTarget: %v", err)
	}
	if !resolution.CrossLayerCollision || resolution.TargetEntryName != "serena-latest-mcphub" {
		t.Fatalf("resolution = %#v, want cross-layer alias", resolution)
	}
	if _, err := c.RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "serena-latest",
		TargetEntryName: resolution.TargetEntryName,
		Entry:           MCPEntry{Name: resolution.TargetEntryName, URL: "http://127.0.0.1:9304/mcp"},
		ExpectedSource:  CodexTransportHTTP,
	}); err != nil {
		t.Fatalf("RelocateHTTPEntry: %v", err)
	}

	projectAfter, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(projectBefore, projectAfter) {
		t.Fatalf("project config changed:\nbefore=%s\nafter=%s", projectBefore, projectAfter)
	}
	updated, err := c.readTOML()
	if err != nil {
		t.Fatal(err)
	}
	servers, _ := updated["mcp_servers"].(map[string]any)
	if _, exists := servers["serena-latest"]; exists {
		t.Fatalf("old global id remains after relocation: %#v", servers)
	}
	if _, exists := servers["serena-latest-mcphub"]; !exists {
		t.Fatalf("target alias missing after relocation: %#v", servers)
	}
	if _, exists := servers["unrelated"]; !exists {
		t.Fatalf("unrelated global entry lost: %#v", servers)
	}
}

func TestCodexTargetUsesDesiredHTTPWhenGlobalAbsentAndProjectStdio(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "global", "config.toml")
	projectPath := filepath.Join(root, ".codex", "config.toml")
	project := `[mcp_servers.serena-latest]
command = "uvx"
args = ["serena", "start-mcp-server"]
`
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	projectBefore, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}

	resolution, err := (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena-latest",
		DesiredTransport: CodexTransportHTTP,
		ProjectRoot:      root,
		WorkingDir:       root,
	})
	if err != nil {
		t.Fatalf("ResolveTransportTarget: %v", err)
	}
	if !resolution.CrossLayerCollision || resolution.TargetEntryName != "serena-latest-mcphub" {
		t.Fatalf("resolution = %#v, want HTTP-vs-stdio alias", resolution)
	}
	projectAfter, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(projectBefore, projectAfter) {
		t.Fatalf("project config changed during resolution:\nbefore=%s\nafter=%s", projectBefore, projectAfter)
	}
	if _, err := os.Stat(globalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolver created global config: stat error = %v", err)
	}
}

func TestCodexTargetKeepsLogicalNameForMatchingDesiredTransport(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "global", "config.toml")
	projectPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(`[mcp_servers.serena-latest]
url = "http://127.0.0.1:9304/mcp"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	resolution, err := (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena-latest",
		DesiredTransport: CodexTransportHTTP,
		ProjectRoot:      root,
		WorkingDir:       root,
	})
	if err != nil {
		t.Fatalf("ResolveTransportTarget: %v", err)
	}
	if resolution.CrossLayerCollision || resolution.TargetEntryName != "serena-latest" {
		t.Fatalf("resolution = %#v, want logical target without alias", resolution)
	}
}

func TestCodexTargetReportsExactExistingHTTPAliasWithoutWriteAuthorization(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "global", "config.toml")
	projectPath := filepath.Join(root, ".codex", "config.toml")
	global := `[mcp_servers.serena-latest-mcphub]
url = "http://127.0.0.1:9304/mcp"
startup_timeout_sec = 10
`
	project := `[mcp_servers.serena-latest]
command = "uvx"
args = ["serena", "start-mcp-server"]
`
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalPath, []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	globalBefore, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	projectBefore, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}

	resolution, err := (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena-latest",
		DesiredTransport: CodexTransportHTTP,
		DesiredEntry: MCPEntry{
			Name: "serena-latest-mcphub",
			URL:  "http://127.0.0.1:9304/mcp",
		},
		ProjectRoot: root,
		WorkingDir:  root,
	})
	if err != nil {
		t.Fatalf("ResolveTransportTarget: %v", err)
	}
	if resolution.TargetEntryName != "serena-latest-mcphub" || !resolution.ExistingExactTarget {
		t.Fatalf("resolution = %#v, want exact existing alias classification", resolution)
	}
	globalAfter, _ := os.ReadFile(globalPath)
	projectAfter, _ := os.ReadFile(projectPath)
	if !bytes.Equal(globalBefore, globalAfter) || !bytes.Equal(projectBefore, projectAfter) {
		t.Fatal("exact-existing inspection mutated config bytes")
	}
}

func TestCodexTargetRejectsUnownedExistingAlias(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "global", "config.toml")
	projectPath := filepath.Join(root, ".codex", "config.toml")
	global := `[mcp_servers.serena-latest-mcphub]
url = "http://127.0.0.1:9305/mcp"
startup_timeout_sec = 10
`
	project := `[mcp_servers.serena-latest]
command = "uvx"
`
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalPath, []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena-latest",
		DesiredTransport: CodexTransportHTTP,
		DesiredEntry: MCPEntry{
			Name: "serena-latest-mcphub",
			URL:  "http://127.0.0.1:9304/mcp",
		},
		ProjectRoot: root,
		WorkingDir:  root,
	})
	if !errors.Is(err, ErrCodexTargetNameConflict) {
		t.Fatalf("ResolveTransportTarget error = %v, want target-name conflict", err)
	}
}

func TestCodexTransportLayerRejectsProjectRootJunctionWithoutMutation(t *testing.T) {
	globalPath := setupCodexConfig(t, "[mcp_servers.serena-latest]\nurl = 'http://127.0.0.1:9304/mcp'\n")
	globalBefore, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}

	realRoot := filepath.Join(t.TempDir(), "real-project")
	projectPath := writeCodexProjectConfigForTest(t, realRoot)
	projectBefore, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	rootJunction := filepath.Join(t.TempDir(), "project-junction")
	createCodexJunctionForTest(t, rootJunction, realRoot)

	_, err = (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena-latest",
		DesiredTransport: CodexTransportHTTP,
		ProjectRoot:      rootJunction,
		WorkingDir:       rootJunction,
	})
	if !errors.Is(err, ErrCodexLayerParseFailed) {
		t.Fatalf("ResolveTransportTarget error = %v, want ErrCodexLayerParseFailed for project-root junction", err)
	}
	if strings.Contains(err.Error(), rootJunction) || strings.Contains(err.Error(), realRoot) {
		t.Fatalf("junction refusal leaked an absolute path: %v", err)
	}
	assertCodexLayerRefusalDidNotMutate(t, globalPath, globalBefore)
	assertCodexLayerRefusalDidNotMutate(t, projectPath, projectBefore)
}

func TestCodexTransportLayerRejectsCodexAncestorJunctionWithoutMutation(t *testing.T) {
	globalPath := setupCodexConfig(t, "[mcp_servers.serena-latest]\nurl = 'http://127.0.0.1:9304/mcp'\n")
	globalBefore, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}

	projectRoot := t.TempDir()
	outsideCodexDir := filepath.Join(t.TempDir(), "outside-codex")
	projectPath := filepath.Join(outsideCodexDir, "config.toml")
	if err := os.MkdirAll(outsideCodexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte("[mcp_servers.serena-latest]\ncommand = 'uvx'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectBefore, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	createCodexJunctionForTest(t, filepath.Join(projectRoot, ".codex"), outsideCodexDir)

	_, err = (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena-latest",
		DesiredTransport: CodexTransportHTTP,
		ProjectRoot:      projectRoot,
		WorkingDir:       projectRoot,
	})
	if !errors.Is(err, ErrCodexLayerParseFailed) {
		t.Fatalf("ResolveTransportTarget error = %v, want ErrCodexLayerParseFailed for .codex junction", err)
	}
	if strings.Contains(err.Error(), projectRoot) || strings.Contains(err.Error(), outsideCodexDir) {
		t.Fatalf("junction refusal leaked an absolute path: %v", err)
	}
	assertCodexLayerRefusalDidNotMutate(t, globalPath, globalBefore)
	assertCodexLayerRefusalDidNotMutate(t, projectPath, projectBefore)
}

func TestCodexTransportLayerRejectsLeafSymlinkWithoutMutation(t *testing.T) {
	globalPath := setupCodexConfig(t, "[mcp_servers.serena-latest]\nurl = 'http://127.0.0.1:9304/mcp'\n")
	globalBefore, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectRoot, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	realProjectPath := filepath.Join(t.TempDir(), "project-config.toml")
	projectBefore := []byte("[mcp_servers.serena-latest]\ncommand = 'uvx'\n")
	if err := os.WriteFile(realProjectPath, projectBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.Symlink(realProjectPath, leaf); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("file symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}

	_, err = (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena-latest",
		DesiredTransport: CodexTransportHTTP,
		ProjectRoot:      projectRoot,
		WorkingDir:       projectRoot,
	})
	if !errors.Is(err, ErrCodexLayerParseFailed) {
		t.Fatalf("ResolveTransportTarget error = %v, want ErrCodexLayerParseFailed for config symlink", err)
	}
	assertCodexLayerRefusalDidNotMutate(t, globalPath, globalBefore)
	assertCodexLayerRefusalDidNotMutate(t, realProjectPath, projectBefore)
}

func TestCodexTransportLayerReadsRealProjectConfigWithoutMutation(t *testing.T) {
	globalPath := setupCodexConfig(t, "[mcp_servers.serena-latest]\nurl = 'http://127.0.0.1:9304/mcp'\n")
	globalBefore, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := t.TempDir()
	projectPath := writeCodexProjectConfigForTest(t, projectRoot)
	projectBefore, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena-latest",
		DesiredTransport: CodexTransportHTTP,
		ProjectRoot:      projectRoot,
		WorkingDir:       projectRoot,
	})
	if err != nil {
		t.Fatalf("ResolveTransportTarget: %v", err)
	}
	if !result.CrossLayerCollision || result.TargetEntryName != "serena-latest-mcphub" {
		t.Fatalf("resolution = %#v, want collision alias", result)
	}
	assertCodexLayerRefusalDidNotMutate(t, globalPath, globalBefore)
	assertCodexLayerRefusalDidNotMutate(t, projectPath, projectBefore)
}

func TestCodexTransportLayerMissingProjectConfigRemainsOptional(t *testing.T) {
	globalPath := setupCodexConfig(t, "[mcp_servers.serena-latest]\nurl = 'http://127.0.0.1:9304/mcp'\n")
	projectRoot := t.TempDir()

	result, err := (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena-latest",
		DesiredTransport: CodexTransportHTTP,
		ProjectRoot:      projectRoot,
		WorkingDir:       projectRoot,
	})
	if err != nil {
		t.Fatalf("ResolveTransportTarget missing optional project config: %v", err)
	}
	if result.CrossLayerCollision || result.ProjectLayerPresent || result.TargetEntryName != "serena-latest" {
		t.Fatalf("resolution = %#v, want absent optional project layer", result)
	}
}

func TestCodexTransportLayerRejectsWorkingDirectoryEscapeWithoutMutation(t *testing.T) {
	globalPath := setupCodexConfig(t, "[mcp_servers.serena-latest]\nurl = 'http://127.0.0.1:9304/mcp'\n")
	globalBefore, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := t.TempDir()
	outsideWorkingDir := t.TempDir()

	_, err = (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{
		LogicalEntryName: "serena-latest",
		DesiredTransport: CodexTransportHTTP,
		ProjectRoot:      projectRoot,
		WorkingDir:       outsideWorkingDir,
	})
	if !errors.Is(err, ErrCodexLayerRootUnresolved) {
		t.Fatalf("ResolveTransportTarget error = %v, want ErrCodexLayerRootUnresolved", err)
	}
	if strings.Contains(err.Error(), projectRoot) || strings.Contains(err.Error(), outsideWorkingDir) {
		t.Fatalf("working-directory escape refusal leaked an absolute path: %v", err)
	}
	assertCodexLayerRefusalDidNotMutate(t, globalPath, globalBefore)
}

func TestCodexRelocationWriteFailureRestoresExactGlobalBytes(t *testing.T) {
	path := setupCodexConfig(t, `[mcp_servers.serena-latest]
url = "http://127.0.0.1:9304/mcp"

[mcp_servers.unrelated]
url = "http://127.0.0.1:9305/mcp"
`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	_, err = c.RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "serena-latest",
		TargetEntryName: "serena-latest-mcphub",
		Entry:           MCPEntry{Name: "serena-latest-mcphub", URL: "http://127.0.0.1:9304/mcp"},
		ExpectedSource:  CodexTransportHTTP,
		WriteConfig: func(path string, data []byte) error {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return err
			}
			return errors.New("injected post-publish failure")
		},
	})
	if err == nil {
		t.Fatal("RelocateHTTPEntry unexpectedly succeeded")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("failed relocation did not restore exact bytes:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestCodexRelocationRepeatIsExplicitAndDoesNotWrite(t *testing.T) {
	path := setupCodexConfig(t, `[mcp_servers.serena-latest]
url = "http://127.0.0.1:9304/mcp"
`)
	c := &codexCLI{path: path}
	req := CodexHTTPRelocation{
		SourceEntryName: "serena-latest",
		TargetEntryName: "serena-latest-mcphub",
		Entry:           MCPEntry{Name: "serena-latest-mcphub", URL: "http://127.0.0.1:9304/mcp"},
		ExpectedSource:  CodexTransportHTTP,
	}
	first, err := c.RelocateHTTPEntry(req)
	if err != nil {
		t.Fatalf("first RelocateHTTPEntry: %v", err)
	}
	req.SourceSnapshot = first.SourceSnapshot
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.RelocateHTTPEntry(req)
	if err != nil {
		t.Fatalf("repeat RelocateHTTPEntry: %v", err)
	}
	if result.Outcome != CodexHTTPRelocationAlreadyConfigured {
		t.Fatalf("repeat outcome = %q, want explicit already configured", result.Outcome)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("repeat relocation wrote config bytes")
	}
}

func TestCodexInverseRelocationRestoresSourceAndRemovesTargetAtomically(t *testing.T) {
	path := setupCodexConfig(t, `[mcp_servers.serena-latest-mcphub]
url = "http://127.0.0.1:9304/mcp"
startup_timeout_sec = 10

[mcp_servers.unrelated]
url = "http://127.0.0.1:9305/mcp"
`)
	c := &codexCLI{path: path}
	result, err := c.RestoreRelocatedHTTPEntry(CodexHTTPInverseRelocation{
		SourceEntryName: "serena-latest",
		TargetEntryName: "serena-latest-mcphub",
		Target:          MCPEntry{Name: "serena-latest-mcphub", URL: "http://127.0.0.1:9304/mcp"},
		SourceSnapshot: map[string]any{
			"command": "uvx",
			"args":    []any{"serena", "start-mcp-server"},
		},
	})
	if err != nil {
		t.Fatalf("RestoreRelocatedHTTPEntry: %v", err)
	}
	if result.Outcome != CodexHTTPInverseRestored {
		t.Fatalf("inverse result = %#v", result)
	}
	updated, err := c.readTOML()
	if err != nil {
		t.Fatal(err)
	}
	servers, _ := updated["mcp_servers"].(map[string]any)
	if _, exists := servers["serena-latest-mcphub"]; exists {
		t.Fatal("target alias remains after inverse relocation")
	}
	source, exists := servers["serena-latest"].(map[string]any)
	transport, _, transportErr := codexTransportOfEntry(source, "serena-latest")
	if !exists || transportErr != nil || transport != CodexTransportStdio {
		t.Fatalf("source snapshot was not restored: %#v", source)
	}
	if _, exists := servers["unrelated"]; !exists {
		t.Fatal("inverse relocation lost unrelated table")
	}
}

func TestCodexRelocationRefusesUnownedAlias(t *testing.T) {
	path := setupCodexConfig(t, `[mcp_servers.serena-latest-mcphub]
url = "http://127.0.0.1:9311/mcp"
`)
	before, _ := os.ReadFile(path)
	_, err := (&codexCLI{path: path}).RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "serena-latest",
		TargetEntryName: "serena-latest-mcphub",
		Entry:           MCPEntry{Name: "serena-latest-mcphub", URL: "http://127.0.0.1:9304/mcp"},
		ExpectedSource:  CodexTransportHTTP,
	})
	if !errors.Is(err, ErrCodexLayerCollisionUnowned) {
		t.Fatalf("RelocateHTTPEntry error = %v, want unowned target conflict", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("unowned alias refusal mutated config")
	}
}

func TestCodexCollisionRefusesOccupiedTargetWithoutMutatingProjectOrGlobal(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "global", "config.toml")
	projectPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	global := `[mcp_servers.serena-latest]
url = "http://127.0.0.1:9304/mcp"

[mcp_servers.serena-latest-mcphub]
url = "http://127.0.0.1:9306/mcp"
`
	project := `[mcp_servers.serena-latest]
command = "uvx"
args = ["serena"]
`
	if err := os.WriteFile(globalPath, []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	globalBefore, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	projectBefore, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = (&codexCLI{path: globalPath}).ResolveTransportTarget(CodexTransportTargetRequest{LogicalEntryName: "serena-latest", DesiredTransport: CodexTransportHTTP, ProjectRoot: root, WorkingDir: root})
	if !errors.Is(err, ErrCodexTargetNameConflict) {
		t.Fatalf("ResolveTransportTarget error = %v, want target-name conflict", err)
	}
	globalAfter, _ := os.ReadFile(globalPath)
	projectAfter, _ := os.ReadFile(projectPath)
	if !bytes.Equal(globalBefore, globalAfter) || !bytes.Equal(projectBefore, projectAfter) {
		t.Fatal("collision inspection mutated config bytes")
	}
}

func TestCodexCLI_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	got, ok, err := c.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestCodexCLI_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	live := `[mcp_servers.memory]
url = "http://localhost:9123/mcp"
startup_timeout_sec = 10.0
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	backupBody := `[mcp_servers.memory]
command = "npx"
args = ["-y", "mem"]
`
	if err := os.WriteFile(backup, []byte(backupBody), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	// go-toml/v2 emits literal strings (single-quoted) for simple ASCII;
	// accept either quoting style, matching TestCodexCLI_AddEntry_ReplaceStdioBlock.
	if !strings.Contains(s, `command = "npx"`) && !strings.Contains(s, `command = 'npx'`) {
		t.Errorf("expected restored stdio command, got:\n%s", s)
	}
	if strings.Contains(s, `url = "http`) || strings.Contains(s, `url = 'http`) {
		t.Errorf("hub-HTTP url should be gone after restore, got:\n%s", s)
	}
}

func TestCodexCLI_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	live := `[mcp_servers.newserver]
url = "http://localhost:9999/mcp"
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	if err := c.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "newserver") {
		t.Errorf("newserver should have been removed, got:\n%s", string(data))
	}
}

func TestCodexCLI_BackupKeepSameSecondSnapshotsRemainDistinctForRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	c := &codexCLI{path: path}
	first := `[mcp_servers.memory]
command = "npx"
args = ["first"]
`
	second := `[mcp_servers.memory]
command = "npx"
args = ["second"]
`

	var bak1, bak2 string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		removeTimestampedBackupsForTest(t, path)
		waitForFreshBackupSecondForTest()
		if err := os.WriteFile(path, []byte(first), 0600); err != nil {
			t.Fatal(err)
		}
		var err error
		bak1, err = c.BackupKeep(0)
		if err != nil {
			t.Fatalf("first BackupKeep: %v", err)
		}
		if err := os.WriteFile(path, []byte(second), 0600); err != nil {
			t.Fatal(err)
		}
		bak2, err = c.BackupKeep(0)
		if err != nil {
			t.Fatalf("second BackupKeep: %v", err)
		}
		if backupSecondStampForTest(bak1) == backupSecondStampForTest(bak2) {
			break
		}
	}
	if backupSecondStampForTest(bak1) != backupSecondStampForTest(bak2) {
		t.Fatalf("could not exercise same-second backups; got %q and %q", bak1, bak2)
	}
	if bak1 == bak2 {
		t.Fatalf("same-second BackupKeep calls returned the same path %q", bak1)
	}
	if err := c.RestoreEntryFromBackupForRollback(bak1, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback(%s): %v", bak1, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored config: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "first") || strings.Contains(s, "second") {
		t.Fatalf("rollback from first backup restored wrong snapshot:\n%s", s)
	}
	secondData, err := os.ReadFile(bak2)
	if err != nil {
		t.Fatalf("read second backup: %v", err)
	}
	if !strings.Contains(string(secondData), "second") {
		t.Fatalf("second backup does not hold its own snapshot:\n%s", secondData)
	}
}

func waitForFreshBackupSecondForTest() {
	stamp := time.Now().Format("20060102-150405")
	for time.Now().Format("20060102-150405") == stamp {
		time.Sleep(5 * time.Millisecond)
	}
}

func backupSecondStampForTest(path string) string {
	idx := strings.LastIndex(path, backupSuffixPrefix)
	if idx < 0 {
		return ""
	}
	suffix := path[idx+len(backupSuffixPrefix):]
	if len(suffix) < len("20060102-150405") {
		return suffix
	}
	return suffix[:len("20060102-150405")]
}

func removeTimestampedBackupsForTest(t *testing.T, livePath string) {
	t.Helper()
	dir := filepath.Dir(livePath)
	base := filepath.Base(livePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read backup dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, base+backupSuffixPrefix) || name == base+originalSentinelSuffix {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatalf("remove old backup %s: %v", name, err)
		}
	}
}

func TestCodexCLI_RestoreEntryFromBackup_RefusesHubHTTPBackupEntry(t *testing.T) {
	// Backup was taken AFTER an earlier migrate already rewrote this
	// entry to hub-HTTP form. Defensive refuse.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`[mcp_servers.memory]
url = "http://localhost:9200/mcp"
startup_timeout_sec = 10.0
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`[mcp_servers.memory]
url = "http://localhost:9200/mcp"
startup_timeout_sec = 10.0
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

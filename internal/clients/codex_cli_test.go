package clients

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseManifest_McpLanguageServerShipped(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	yamlPath := filepath.Join(repoRoot, "servers", "mcp-language-server", "manifest.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read %s: %v", yamlPath, err)
	}
	m, err := ParseManifest(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Kind != KindWorkspaceScoped {
		t.Fatalf("Kind = %q, want workspace-scoped", m.Kind)
	}
	want := map[string]string{
		"clangd": "mcp-language-server", "fortran": "mcp-language-server",
		"go":         "gopls-mcp",
		"javascript": "mcp-language-server", "python": "mcp-language-server",
		"rust": "mcp-language-server", "typescript": "mcp-language-server",
		"vscode-css": "mcp-language-server", "vscode-html": "mcp-language-server",
	}
	got := map[string]string{}
	for _, l := range m.Languages {
		got[l.Name] = l.Backend
		if l.Transport != LanguageTransportStdio {
			t.Errorf("language %s: Transport = %q, want stdio in v1", l.Name, l.Transport)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("languages: got %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for name, backend := range want {
		if got[name] != backend {
			t.Errorf("languages[%s].backend = %q, want %q", name, got[name], backend)
		}
	}
	if m.PortPool.Start != 9200 || m.PortPool.End != 9299 {
		t.Errorf("PortPool = %+v, want {9200,9299}", m.PortPool)
	}
}

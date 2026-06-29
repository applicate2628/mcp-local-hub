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
	markers := map[string][]string{}
	for _, l := range m.Languages {
		got[l.Name] = l.Backend
		markers[l.Name] = l.ProjectMarkers
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
	wantMarkers := map[string][]string{
		"clangd":      {"compile_commands.json", ".clangd"},
		"fortran":     {"fpm.toml", ".fortlsrc", ".fortls.json", ".fortls"},
		"go":          {"go.mod"},
		"javascript":  {"package.json", "tsconfig.json", "jsconfig.json"},
		"python":      {"pyproject.toml", "setup.py", "setup.cfg"},
		"rust":        {"Cargo.toml"},
		"typescript":  {"package.json", "tsconfig.json", "jsconfig.json"},
		"vscode-css":  {"package.json"},
		"vscode-html": {"package.json"},
	}
	for name, want := range wantMarkers {
		gotMarkers := markers[name]
		if len(gotMarkers) != len(want) {
			t.Fatalf("languages[%s].project_markers = %v, want %v", name, gotMarkers, want)
		}
		for i := range want {
			if gotMarkers[i] != want[i] {
				t.Fatalf("languages[%s].project_markers = %v, want %v", name, gotMarkers, want)
			}
		}
	}
	if m.PortPool.Start != 9400 || m.PortPool.End != 9599 {
		t.Errorf("PortPool = %+v, want {9400,9599}", m.PortPool)
	}
}

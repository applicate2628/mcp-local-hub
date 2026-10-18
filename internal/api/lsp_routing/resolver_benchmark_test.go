package lsp_routing

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// BenchmarkWorkspaceResolverResolveByPath includes marker discovery, the
// shared observer's freshness check, scalar candidate scan and selected-row
// materialization. The known registration is last to exercise the full scan.
func BenchmarkWorkspaceResolverResolveByPath(b *testing.B) {
	for _, fixture := range []struct {
		name     string
		minBytes int
	}{
		{name: "representative-7KiB", minBytes: 7 * 1024},
		{name: "near-cap-14MiB", minBytes: 14 * 1024 * 1024},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			root := b.TempDir()
			project := filepath.Join(root, "registered-project")
			if err := os.MkdirAll(project, 0o700); err != nil {
				b.Fatal(err)
			}
			for path, data := range map[string]string{
				filepath.Join(project, "go.mod"):  "module benchmark\n",
				filepath.Join(project, "main.go"): "package main\n",
			} {
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					b.Fatal(err)
				}
			}
			canonical, err := api.CanonicalWorkspacePath(project)
			if err != nil {
				b.Fatal(err)
			}
			var yaml bytes.Buffer
			yaml.WriteString("version: 1\nworkspaces:\n")
			for i := 0; yaml.Len() < fixture.minBytes; i++ {
				fmt.Fprintf(&yaml, "  - workspace_key: other-%06d\n    workspace_path: /synthetic/%06d\n    language: go\n    backend: gopls-mcp\n    port: 9200\n", i, i)
			}
			fmt.Fprintf(&yaml, "  - workspace_key: %q\n    workspace_path: %q\n    language: go\n    backend: gopls-mcp\n    port: 9201\n", api.WorkspaceKey(canonical), canonical)
			registryPath := filepath.Join(root, "workspaces.yaml")
			if err := api.WriteStateFileBytesLockHeld(registryPath, yaml.Bytes()); err != nil {
				b.Fatal(err)
			}
			resolver := NewReadOnlyWorkspaceResolver(api.NewRegistry(registryPath), registryPath,
				[]config.LanguageSpec{{Name: "go", Backend: "gopls-mcp", ProjectMarkers: []string{"go.mod"}}})
			file := filepath.Join(project, "main.go")
			warm, err := resolver.ResolveByPath(file, "go")
			if err != nil || !warm.Registered || warm.Entry == nil || warm.Entry.Port != 9201 {
				b.Fatalf("warm public result = %+v, err = %v", warm, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := resolver.ResolveByPath(file, "go")
				if err != nil || !result.Registered || result.Entry == nil || result.Entry.Port != 9201 {
					b.Fatalf("public result = %+v, err = %v", result, err)
				}
			}
		})
	}
}

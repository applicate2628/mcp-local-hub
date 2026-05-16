// internal/clients/init_empty_test.go
//
// Per-adapter coverage for InitEmpty() (v0.4.5 init-button feature).
// Each adapter's empty stub bytes are pinned so a future stub-shape
// change cannot silently produce a config the parent CLI would
// reject. The adapter pointer constructors (`&vscodeClient{path:p}`)
// match the language-server-test pattern so the test does not depend
// on HOME / USERPROFILE / APPDATA env vars resolving on the host.
package clients

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitEmpty_PerAdapter_StubBytes(t *testing.T) {
	cases := []struct {
		name string
		make func(path string) Client
		rel  string
		want string
	}{
		{
			name: "claude-code",
			make: func(p string) Client { return &claudeCode{path: p} },
			rel:  ".claude.json",
			want: "{\n  \"mcpServers\": {}\n}\n",
		},
		{
			name: "codex-cli",
			make: func(p string) Client { return &codexCLI{path: p} },
			rel:  ".codex/config.toml",
			want: "[mcp_servers]\n",
		},
		{
			name: "cursor",
			make: func(p string) Client {
				return &cursorClient{jsonMCPClient: &jsonMCPClient{path: p, clientName: "cursor", urlField: "url"}}
			},
			rel:  ".cursor/mcp.json",
			want: "{\n  \"mcpServers\": {}\n}\n",
		},
		{
			name: "vscode",
			make: func(p string) Client { return &vscodeClient{path: p} },
			rel:  "AppData/Roaming/Code/User/mcp.json",
			want: "{\n  \"servers\": {}\n}\n",
		},
		{
			name: "qwen-cli",
			make: func(p string) Client {
				return &qwenCLI{jsonMCPClient: &jsonMCPClient{path: p, clientName: "qwen-cli", urlField: "httpUrl"}}
			},
			rel:  ".qwen/settings.json",
			want: "{\n  \"mcpServers\": {}\n}\n",
		},
		{
			name: "json-mcp-gemini-like",
			make: func(p string) Client {
				return &jsonMCPClient{path: p, clientName: "gemini-cli", urlField: "url"}
			},
			rel:  ".gemini/settings.json",
			want: "{\n  \"mcpServers\": {}\n}\n",
		},
		{
			name: "antigravity-via-base",
			make: func(p string) Client {
				return &antigravityClient{
					jsonMCPClient: &jsonMCPClient{path: p, clientName: "antigravity", urlField: "command"},
				}
			},
			rel:  ".gemini/antigravity/mcp_config.json",
			want: "{\n  \"mcpServers\": {}\n}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.rel)
			c := tc.make(path)

			if err := c.InitEmpty(); err != nil {
				t.Fatalf("InitEmpty: %v", err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read stub: %v", err)
			}
			if string(body) != tc.want {
				t.Errorf("stub=%q, want %q", body, tc.want)
			}
		})
	}
}

// TestInitEmpty_Idempotent guards the second-click contract: a stub
// already on disk MUST NOT be overwritten. The test seeds a custom
// payload, calls InitEmpty, and verifies the file bytes are
// unchanged.
func TestInitEmpty_Idempotent(t *testing.T) {
	cases := []struct {
		name string
		make func(path string) Client
		rel  string
	}{
		{"claude-code", func(p string) Client { return &claudeCode{path: p} }, ".claude.json"},
		{"codex-cli", func(p string) Client { return &codexCLI{path: p} }, ".codex/config.toml"},
		{"cursor", func(p string) Client {
			return &cursorClient{jsonMCPClient: &jsonMCPClient{path: p, clientName: "cursor", urlField: "url"}}
		}, ".cursor/mcp.json"},
		{"vscode", func(p string) Client { return &vscodeClient{path: p} }, "AppData/Roaming/Code/User/mcp.json"},
		{"qwen-cli", func(p string) Client {
			return &qwenCLI{jsonMCPClient: &jsonMCPClient{path: p, clientName: "qwen-cli", urlField: "httpUrl"}}
		}, ".qwen/settings.json"},
		{"json-mcp", func(p string) Client {
			return &jsonMCPClient{path: p, clientName: "gemini-cli", urlField: "url"}
		}, ".gemini/settings.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir parent: %v", err)
			}
			seed := []byte(`{"mcpServers":{"existing":{"command":"foo"}}}`)
			if err := os.WriteFile(path, seed, 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}

			c := tc.make(path)
			if err := c.InitEmpty(); err != nil {
				t.Fatalf("InitEmpty: %v", err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(body) != string(seed) {
				t.Errorf("idempotent InitEmpty rewrote bytes: got=%q want=%q", body, seed)
			}
		})
	}
}

// TestInitEmpty_CreatesMissingParentDirs guards the adapter-level
// contract: WriteConfigFile mkdir-p's the immediate parent so a
// fresh `~/.cursor/`, `%APPDATA%\Code\User\`, etc. tree is created
// alongside the stub. The /api/init-client-config endpoint adds a
// separate parent-presence gate to prevent surprising tree creation
// on hosts where the client is not installed — but at the adapter
// level the helper is permissive so BackupKeep's seed-then-backup
// path keeps working on a never-installed host.
func TestInitEmpty_CreatesMissingParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "tree", "config.json")
	c := &claudeCode{path: path}
	if err := c.InitEmpty(); err != nil {
		t.Fatalf("InitEmpty: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("stub not created at nested path: %v", err)
	}
}

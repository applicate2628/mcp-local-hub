package clients

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestRemoteHTTPHeaders_RoundTrip pins the invariant that every
// remote-http-capable adapter's GetEntry returns Headers identical to
// those AddEntry wrote. Install rollback's priorEntry snapshot relies
// on this: if GetEntry strips headers, rollback restores a URL-only
// entry and silently breaks auth on a partial-failure install.
func TestRemoteHTTPHeaders_RoundTrip(t *testing.T) {
	headers := map[string]string{
		"Authorization": "Bearer secret-token",
		"X-User-Id":     "abc-123",
	}
	url := "https://remote.example.com/mcp"
	name := "remote-test"

	mkJSON := func(t *testing.T, filename string) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, filename)
		if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mkTOML := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(p, []byte(""), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name  string
		build func(t *testing.T) Client
	}{
		{"claude-code", func(t *testing.T) Client {
			return &claudeCode{path: mkJSON(t, "settings.json")}
		}},
		{"codex-cli", func(t *testing.T) Client {
			return &codexCLI{path: mkTOML(t)}
		}},
		{"cursor", func(t *testing.T) Client {
			p := mkJSON(t, "mcp.json")
			return &cursorClient{jsonMCPClient: &jsonMCPClient{path: p, clientName: "cursor", urlField: "url"}}
		}},
		{"gemini-cli", func(t *testing.T) Client {
			p := mkJSON(t, "settings.json")
			return &geminiCLI{jsonMCPClient: &jsonMCPClient{path: p, clientName: "gemini-cli", urlField: "url"}}
		}},
		{"qwen-cli", func(t *testing.T) Client {
			p := mkJSON(t, "settings.json")
			return &qwenCLI{jsonMCPClient: &jsonMCPClient{path: p, clientName: "qwen-cli", urlField: "httpUrl"}}
		}},
		{"vscode", func(t *testing.T) Client {
			return &vscodeClient{path: mkJSON(t, "mcp.json")}
		}},
		{"mimocode", func(t *testing.T) Client {
			// State-safe single-file construction: only `path` is set
			// (configFile/overlayDir/inlineContent empty), so readLayerFiles
			// operates within the temp dir and never reaches the real
			// ~/.config/mimocode or any MIMOCODE_* env-resolved layer. The
			// temp dir has no sibling config.json/mimocode.jsonc, so the merge
			// is effectively single-file. mimocode is HTTP-native (bot PR #420
			// finding 5): AddEntry writes type:remote url+headers, GetEntry
			// round-trips them.
			return &mimoCodeClient{path: mkJSON(t, "mimocode.json")}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.build(t)
			if err := c.AddEntry(MCPEntry{Name: name, URL: url, Headers: headers}); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			got, err := c.GetEntry(name)
			if err != nil {
				t.Fatalf("GetEntry: %v", err)
			}
			if got == nil {
				t.Fatalf("GetEntry returned nil")
			}
			if got.URL != url {
				t.Errorf("URL = %q, want %q", got.URL, url)
			}
			if !reflect.DeepEqual(got.Headers, headers) {
				t.Errorf("Headers = %v, want %v", got.Headers, headers)
			}
		})
	}
}

// TestRemoteHTTPHeaders_AbsentReturnsNil confirms GetEntry leaves
// Headers nil when the on-disk entry has no headers — so empty maps
// don't surface as material drift in rollback diffs.
func TestRemoteHTTPHeaders_AbsentReturnsNil(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte(`{"mcpServers":{"r":{"url":"https://x/mcp","type":"http"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: p}
	got, err := c.GetEntry("r")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil {
		t.Fatalf("GetEntry returned nil")
	}
	if got.Headers != nil {
		t.Errorf("Headers = %v, want nil", got.Headers)
	}
}

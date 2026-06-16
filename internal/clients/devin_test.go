package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func newDevinForTest(t *testing.T, initial string) *devinClient {
	t.Helper()
	dir := t.TempDir()
	devinDir := filepath.Join(dir, "devin")
	if err := os.MkdirAll(devinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(devinDir, "config.json")
	if initial != "" {
		if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &devinClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "devin",
		urlField:   "url",
	}}
}

func TestDevin_Name(t *testing.T) {
	c := newDevinForTest(t, `{"mcpServers":{}}`)
	if got := c.Name(); got != "devin" {
		t.Errorf("Name() = %q, want devin", got)
	}
}

func TestDevin_IsRelayStdio(t *testing.T) {
	c := newDevinForTest(t, `{"mcpServers":{}}`)
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (devin is URL-native HTTP)")
	}
}

// TestDevin_AddEntry_WritesMinimalURLShape verifies the documented minimal
// `{"url":...}` shape — transport inferred from `url`, NO `type`/`transport`
// field and NO `disabled` flag — plus `headers` when provided. Siblings and
// top-level fields survive the merge.
func TestDevin_AddEntry_WritesMinimalURLShape(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		entry   MCPEntry
	}{
		{
			name:    "into existing mcpServers preserves siblings",
			initial: `{"other":"keep-me","mcpServers":{"existing":{"url":"http://localhost:1/mcp"}}}`,
			entry:   MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"},
		},
		{
			name:    "into empty stub",
			initial: `{"mcpServers":{}}`,
			entry:   MCPEntry{Name: "memory", URL: "http://127.0.0.1:9128/mcp"},
		},
		{
			name:    "with headers",
			initial: `{"mcpServers":{}}`,
			entry:   MCPEntry{Name: "time", URL: "http://localhost:9130/mcp", Headers: map[string]string{"X-Auth": "tok"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newDevinForTest(t, tc.initial)
			if err := c.AddEntry(tc.entry); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(c.path)
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("parse: %v", err)
			}
			servers, ok := parsed["mcpServers"].(map[string]any)
			if !ok {
				t.Fatalf("mcpServers missing or wrong type: %v", parsed["mcpServers"])
			}
			got, ok := servers[tc.entry.Name].(map[string]any)
			if !ok {
				t.Fatalf("entry %q missing: %v", tc.entry.Name, servers)
			}
			if got["url"] != tc.entry.URL {
				t.Errorf("url = %v, want %q", got["url"], tc.entry.URL)
			}
			// Devin infers transport from `url`; the minimal documented shape
			// carries no type/transport/disabled.
			if _, has := got["type"]; has {
				t.Errorf("unexpected 'type' field for Devin (transport inferred from url): %v", got)
			}
			if _, has := got["transport"]; has {
				t.Errorf("unexpected 'transport' field for Devin minimal shape: %v", got)
			}
			if _, has := got["disabled"]; has {
				t.Errorf("unexpected 'disabled' flag for Devin minimal shape: %v", got)
			}
			if _, has := got["command"]; has {
				t.Errorf("unexpected stdio 'command' field in HTTP entry: %v", got)
			}
			if len(tc.entry.Headers) > 0 {
				hdrs, ok := got["headers"].(map[string]any)
				if !ok || hdrs["X-Auth"] != "tok" {
					t.Errorf("headers = %v, want X-Auth:tok", got["headers"])
				}
			} else if _, hasHdrs := got["headers"]; hasHdrs {
				t.Errorf("headers present with no Headers supplied: %v", got["headers"])
			}
		})
	}
}

func TestDevin_GetEntry_RoundTrip(t *testing.T) {
	c := newDevinForTest(t, `{"mcpServers":{}}`)
	want := MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp", Headers: map[string]string{"X-Auth": "tok"}}
	if err := c.AddEntry(want); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	got, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != want.URL {
		t.Fatalf("GetEntry = %v, want URL %q", got, want.URL)
	}
	if got.Headers["X-Auth"] != "tok" {
		t.Errorf("Headers = %v, want X-Auth:tok", got.Headers)
	}
	if missing, err := c.GetEntry("nope"); err != nil || missing != nil {
		t.Errorf("GetEntry(missing) = %v, %v; want nil, nil", missing, err)
	}
}

func TestDevin_RemoveEntry_Idempotent(t *testing.T) {
	c := newDevinForTest(t, `{"mcpServers":{"serena":{"url":"http://localhost:9121/mcp"}}}`)
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if got, _ := c.GetEntry("serena"); got != nil {
		t.Errorf("entry still present after RemoveEntry: %v", got)
	}
	if err := c.RemoveEntry("serena"); err != nil {
		t.Errorf("second RemoveEntry not idempotent: %v", err)
	}
}

func TestDevin_InitEmpty(t *testing.T) {
	c := newDevinForTest(t, "")
	created, err := c.InitEmpty()
	if err != nil {
		t.Fatalf("InitEmpty: %v", err)
	}
	if !created {
		t.Error("InitEmpty created=false on first call, want true")
	}
	raw, _ := os.ReadFile(c.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse stub: %v", err)
	}
	if _, ok := parsed["mcpServers"].(map[string]any); !ok {
		t.Errorf("stub missing mcpServers object: %v", parsed)
	}
	if created2, err := c.InitEmpty(); err != nil || created2 {
		t.Errorf("second InitEmpty = %v, %v; want false, nil (idempotent)", created2, err)
	}
}

func TestDevin_Exists_DirHeuristic(t *testing.T) {
	c := newDevinForTest(t, "")
	if !c.Exists() {
		t.Error("Exists() = false when devin dir present, want true")
	}
}

func TestDevin_BackupKeep_FreshHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devin", "config.json")
	c := &devinClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "devin",
		urlField:   "url",
	}}
	bak, err := c.BackupKeep(3)
	if err != nil {
		t.Fatalf("BackupKeep on fresh host: %v", err)
	}
	if _, err := os.Stat(bak); err != nil {
		t.Errorf("backup file not written: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config stub not seeded: %v", err)
	}
}

// TestDevin_DefaultConfigPath_PerOS verifies the per-OS path resolution:
// Windows uses %APPDATA%\devin\config.json; POSIX uses
// $XDG_CONFIG_HOME/devin/config.json (or ~/.config/devin/config.json when
// XDG_CONFIG_HOME is unset).
func TestDevin_DefaultConfigPath_PerOS(t *testing.T) {
	home := filepath.Join("home", "u")
	if runtime.GOOS == "windows" {
		t.Run("APPDATA set", func(t *testing.T) {
			custom := filepath.Join("C:", "AppData")
			t.Setenv("APPDATA", custom)
			got := defaultDevinConfigPath(home)
			want := filepath.Join(custom, "devin", "config.json")
			if got != want {
				t.Errorf("path = %q, want %q", got, want)
			}
		})
		t.Run("APPDATA unset falls back to home AppData/Roaming", func(t *testing.T) {
			t.Setenv("APPDATA", "")
			got := defaultDevinConfigPath(home)
			want := filepath.Join(home, "AppData", "Roaming", "devin", "config.json")
			if got != want {
				t.Errorf("path = %q, want %q", got, want)
			}
		})
	} else {
		t.Run("XDG_CONFIG_HOME set", func(t *testing.T) {
			custom := filepath.Join("/tmp", "xdg")
			t.Setenv("XDG_CONFIG_HOME", custom)
			got := defaultDevinConfigPath(home)
			want := filepath.Join(custom, "devin", "config.json")
			if got != want {
				t.Errorf("path = %q, want %q", got, want)
			}
		})
		t.Run("XDG_CONFIG_HOME unset falls back to home/.config", func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", "")
			got := defaultDevinConfigPath(home)
			want := filepath.Join(home, ".config", "devin", "config.json")
			if got != want {
				t.Errorf("path = %q, want %q", got, want)
			}
		})
	}

	c, err := NewDevin()
	if err != nil {
		t.Fatalf("NewDevin: %v", err)
	}
	if c.Name() != "devin" {
		t.Errorf("Name() = %q, want devin", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false")
	}
}

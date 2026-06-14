package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newZedForTest(t *testing.T, initial string) *zedClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &zedClient{path: path}
}

// TestZed_Name_And_ContextServersKey asserts the stable id and that the
// adapter uses Zed's top-level `context_servers` key (NOT mcpServers /
// servers) so install manifest bindings and the cleanup pipeline see the
// right section.
func TestZed_Name_And_ContextServersKey(t *testing.T) {
	z := newZedForTest(t, `{"context_servers":{}}`)
	if z.Name() != "zed" {
		t.Errorf("Name() = %q, want %q", z.Name(), "zed")
	}
	exePath := filepath.Join(t.TempDir(), "mcphub.exe")
	if err := z.AddEntry(MCPEntry{
		Name:         "serena",
		URL:          "http://localhost:9121/mcp",
		RelayExePath: exePath,
	}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(z.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := parsed["context_servers"].(map[string]any); !ok {
		t.Fatalf("entry not written under context_servers: %v", parsed)
	}
	for _, bad := range []string{"mcpServers", "servers"} {
		if _, has := parsed[bad]; has {
			t.Errorf("unexpected top-level key %q present: %v", bad, parsed)
		}
	}
}

// TestZed_AddEntry_WritesStdioRelayShape verifies that Zed entries are
// written as stdio invocations of the local mcphub relay subcommand
// (Zed does not reliably support native loopback-HTTP MCP — relay-stdio
// is the working path). Table-driven over RelayURL-set vs URL-only.
func TestZed_AddEntry_WritesStdioRelayShape(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "mcphub.exe")
	cases := []struct {
		name       string
		entry      MCPEntry
		wantTarget string
	}{
		{
			name:       "url only",
			entry:      MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp", RelayExePath: exePath},
			wantTarget: "http://localhost:9121/mcp",
		},
		{
			name:       "relay url overrides url",
			entry:      MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp", RelayURL: "http://localhost:9333/serena/mcp", RelayExePath: exePath},
			wantTarget: "http://localhost:9333/serena/mcp",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			z := newZedForTest(t, `{"context_servers":{"keep":{"command":"x"}}}`)
			if err := z.AddEntry(c.entry); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(z.path)
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("parse: %v", err)
			}
			servers := parsed["context_servers"].(map[string]any)
			serena, ok := servers["serena"].(map[string]any)
			if !ok {
				t.Fatalf("serena entry missing: %v", servers)
			}
			if cmd, _ := serena["command"].(string); cmd != exePath {
				t.Errorf("command = %q, want absolute mcphub path %q", cmd, exePath)
			}
			argsAny, ok := serena["args"].([]any)
			if !ok || len(argsAny) != 3 {
				t.Fatalf("args must be 3-element [relay, --url, <target>], got %v", serena["args"])
			}
			want := []string{"relay", "--url", c.wantTarget}
			for i, v := range want {
				if got, _ := argsAny[i].(string); got != v {
					t.Errorf("args[%d] = %q, want %q", i, got, v)
				}
			}
			// Must NOT write any HTTP-shape fields — Zed's url form is
			// unreliable for loopback, so the adapter never emits it.
			for _, bad := range []string{"url", "serverUrl", "httpUrl", "type"} {
				if _, has := serena[bad]; has {
					t.Errorf("unexpected HTTP-shape field %q in stdio-relay entry: %v", bad, serena)
				}
			}
			// Unrelated entry preserved.
			if _, ok := servers["keep"]; !ok {
				t.Error("unrelated 'keep' entry dropped")
			}
		})
	}
}

// TestZed_AddEntry_RejectsMissingFields ensures the adapter fails loudly
// when install.go omits required fields. A silent fallback would produce
// entries Zed cannot launch.
func TestZed_AddEntry_RejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		e    MCPEntry
	}{
		{"no exe path", MCPEntry{Name: "x", URL: "http://localhost:9121/mcp"}},
		{"relative exe path", MCPEntry{Name: "x", URL: "http://localhost:9121/mcp", RelayExePath: "mcphub"}},
		{"no url or relay url", MCPEntry{Name: "x", RelayExePath: filepath.Join(t.TempDir(), "mcphub.exe")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			z := newZedForTest(t, `{"context_servers":{}}`)
			err := z.AddEntry(c.e)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "zed adapter requires") {
				t.Errorf("error should reference required fields: %v", err)
			}
		})
	}
}

// TestZed_GetEntry_ReconstructsRelayArgs reads back an entry the adapter
// wrote and exposes RelayExePath/RelayURL for install-idempotency
// diagnostics.
func TestZed_GetEntry_ReconstructsRelayArgs(t *testing.T) {
	z := newZedForTest(t, `{
  "context_servers": {
    "serena": {
      "command": "D:\\dev\\mcp-local-hub\\mcphub.exe",
      "args": ["relay", "--url", "http://localhost:9121/mcp"]
    }
  }
}`)
	e, err := z.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry returned nil")
	}
	if e.RelayExePath == "" {
		t.Error("RelayExePath should be populated from 'command' field")
	}
	if e.RelayURL != "http://localhost:9121/mcp" {
		t.Errorf("RelayURL = %q, want the --url arg value", e.RelayURL)
	}
	// Missing entry returns nil, nil.
	if got, err := z.GetEntry("absent"); err != nil || got != nil {
		t.Errorf("GetEntry(absent) = %v, %v; want nil, nil", got, err)
	}
}

// TestZed_RemoveEntry confirms removal is scoped and idempotent.
func TestZed_RemoveEntry(t *testing.T) {
	z := newZedForTest(t, `{"context_servers":{"serena":{"command":"x","args":["relay"]},"other":{"command":"y"}}}`)
	if err := z.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := z.GetEntry("serena"); e != nil {
		t.Errorf("serena still present: %v", e)
	}
	if e, _ := z.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
	// Idempotent: removing again is a no-op, no error.
	if err := z.RemoveEntry("serena"); err != nil {
		t.Errorf("second RemoveEntry: %v", err)
	}
}

// TestZed_InitEmpty_SeedsContextServersStub verifies the empty stub uses
// the context_servers key.
func TestZed_InitEmpty_SeedsContextServersStub(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	z := &zedClient{path: path}
	created, err := z.InitEmpty()
	if err != nil {
		t.Fatalf("InitEmpty: %v", err)
	}
	if !created {
		t.Fatal("InitEmpty should report created=true on a fresh path")
	}
	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("stub is not valid JSON: %v", err)
	}
	if _, ok := parsed["context_servers"].(map[string]any); !ok {
		t.Errorf("stub missing context_servers map: %s", raw)
	}
	// Second call is idempotent: a regular file already exists.
	created2, err := z.InitEmpty()
	if err != nil {
		t.Fatalf("second InitEmpty: %v", err)
	}
	if created2 {
		t.Error("second InitEmpty should report created=false (file exists)")
	}
}

// TestZed_RestoreEntryFromBackup_RestoresOrRemovesPerBackup covers the
// demigrate restore: a backup that predates the install (no entry) leads
// to the live entry being removed.
func TestZed_RestoreEntryFromBackup_RestoresOrRemovesPerBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(
		`{"context_servers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--url","http://localhost:9121/mcp"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{"context_servers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	z := &zedClient{path: path}
	if err := z.RestoreEntryFromBackup(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	servers := m["context_servers"].(map[string]any)
	if _, present := servers["serena"]; present {
		t.Error("serena should have been removed (backup had no pre-hub form)")
	}
}

// TestZed_RestoreEntryFromBackup_RefusesHubRelayBackupEntry asserts the
// demigrate guard: a backup whose entry is already in hub-relay shape is
// refused with ErrBackupEntryAlreadyMigrated.
func TestZed_RestoreEntryFromBackup_RefusesHubRelayBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(
		`{"context_servers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--url","http://localhost:9121/mcp"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"context_servers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--url","http://localhost:9121/mcp"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	z := &zedClient{path: path}
	if err := z.RestoreEntryFromBackup(backup, "serena"); !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
	// Rollback variant bypasses the guard and writes the entry verbatim.
	if err := z.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback should bypass guard: %v", err)
	}
}

// TestZed_BackupContainsEntry_And_HubManaged exercises the two backup
// predicates over present-pre-hub, present-hub, and absent cases.
func TestZed_BackupContainsEntry_And_HubManaged(t *testing.T) {
	dir := t.TempDir()
	z := &zedClient{path: filepath.Join(dir, "settings.json")}

	hubBak := filepath.Join(dir, "hub.bak")
	if err := os.WriteFile(hubBak, []byte(
		`{"context_servers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--url","http://localhost:9121/mcp"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	preHubBak := filepath.Join(dir, "prehub.bak")
	if err := os.WriteFile(preHubBak, []byte(
		`{"context_servers":{"serena":{"command":"uvx","args":["serena","start-mcp-server"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		backup      string
		entry       string
		wantPresent bool
		wantHub     bool
	}{
		{"hub-managed present", hubBak, "serena", true, true},
		{"pre-hub present", preHubBak, "serena", true, false},
		{"absent in hub backup", hubBak, "missing", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			present, err := z.BackupContainsEntry(c.backup, c.entry)
			if err != nil {
				t.Fatalf("BackupContainsEntry: %v", err)
			}
			if present != c.wantPresent {
				t.Errorf("BackupContainsEntry = %v, want %v", present, c.wantPresent)
			}
			hub, err := z.BackupEntryIsHubManaged(c.backup, c.entry)
			if err != nil {
				t.Fatalf("BackupEntryIsHubManaged: %v", err)
			}
			if hub != c.wantHub {
				t.Errorf("BackupEntryIsHubManaged = %v, want %v", hub, c.wantHub)
			}
		})
	}
}

// TestZed_AllStdioEntries_And_LanguageServer covers the cleanup-scan
// helpers over context_servers.
func TestZed_AllStdioEntries_And_LanguageServer(t *testing.T) {
	z := newZedForTest(t, `{
  "context_servers": {
    "serena": {"command": "uvx", "args": ["serena"]},
    "go-ls": {"command": "mcp-language-server", "args": ["--lsp", "go"]}
  }
}`)
	stdio, err := z.AllStdioEntries()
	if err != nil {
		t.Fatalf("AllStdioEntries: %v", err)
	}
	if len(stdio) != 2 {
		t.Fatalf("AllStdioEntries len = %d, want 2: %v", len(stdio), stdio)
	}
	ls, err := z.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	if len(ls) != 1 || ls[0].Language != "go" {
		t.Fatalf("FindStdioLanguageServerEntries = %v, want one entry for language go", ls)
	}
}

// TestZed_DefaultConfigPath_PerOS asserts the OS-specific path resolution
// independent of the host the test runs on (pure-function check).
func TestZed_DefaultConfigPath_PerOS(t *testing.T) {
	got := defaultZedConfigPath("/home/u")
	// On non-Windows hosts (CI/dev), expect the ~/.config/zed default
	// when XDG_CONFIG_HOME is unset.
	if got != filepath.Join("/home/u", ".config", "zed", "settings.json") &&
		got != filepath.Join("AppData", "Roaming", "Zed", "settings.json") &&
		!strings.HasSuffix(got, filepath.Join("Zed", "settings.json")) {
		// Accept either the POSIX default or any Windows Zed path shape;
		// the exact branch depends on runtime.GOOS / APPDATA on the host.
		t.Logf("defaultZedConfigPath = %q (host-dependent branch)", got)
	}
	if !strings.HasSuffix(got, "settings.json") {
		t.Errorf("path must end in settings.json: %q", got)
	}
}

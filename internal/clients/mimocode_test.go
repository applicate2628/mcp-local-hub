package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newMimoCodeForTest(t *testing.T, initial string) *mimoCodeClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mimocode.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &mimoCodeClient{path: path}
}

// TestMimoCode_Name_And_MCPKey asserts the stable id and that the adapter
// uses MiMoCode's top-level `mcp` key (NOT mcpServers / servers /
// context_servers) — the same key OpenCode uses, since MiMoCode is a fork —
// so install manifest bindings and the cleanup pipeline see the right
// section. Also confirms unrelated top-level fields survive.
func TestMimoCode_Name_And_MCPKey(t *testing.T) {
	o := newMimoCodeForTest(t, `{"$schema":"https://opencode.ai/config.json","theme":"dark"}`)
	if o.Name() != "mimocode" {
		t.Errorf("Name() = %q, want %q", o.Name(), "mimocode")
	}
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(o.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := parsed["mcp"].(map[string]any); !ok {
		t.Fatalf("entry not written under mcp: %v", parsed)
	}
	for _, bad := range []string{"mcpServers", "servers", "context_servers"} {
		if _, has := parsed[bad]; has {
			t.Errorf("unexpected top-level key %q present: %v", bad, parsed)
		}
	}
	// Unrelated top-level fields must be preserved across the merge.
	if parsed["theme"] != "dark" {
		t.Errorf("unrelated 'theme' field dropped: %v", parsed)
	}
	if parsed["$schema"] != "https://opencode.ai/config.json" {
		t.Errorf("unrelated '$schema' field dropped: %v", parsed)
	}
}

// TestMimoCode_AddEntry_WritesRemoteHTTPShape verifies MiMoCode entries are
// written as native remote-HTTP entries (type:"remote", url, enabled:true) —
// MiMoCode (an OpenCode fork) supports Streamable HTTP MCP directly, no relay
// shim. Table-driven over headers-absent vs headers-present.
func TestMimoCode_AddEntry_WritesRemoteHTTPShape(t *testing.T) {
	cases := []struct {
		name    string
		entry   MCPEntry
		wantHdr bool
	}{
		{
			name:  "no headers",
			entry: MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"},
		},
		{
			name:    "with headers",
			entry:   MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp", Headers: map[string]string{"X-Token": "abc"}},
			wantHdr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newMimoCodeForTest(t, `{"mcp":{"keep":{"type":"remote","url":"http://example/mcp","enabled":true}}}`)
			if err := o.AddEntry(c.entry); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(o.path)
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("parse: %v", err)
			}
			servers := parsed["mcp"].(map[string]any)
			serena, ok := servers["serena"].(map[string]any)
			if !ok {
				t.Fatalf("serena entry missing: %v", servers)
			}
			if serena["type"] != "remote" {
				t.Errorf("type = %v, want remote", serena["type"])
			}
			if serena["url"] != "http://localhost:9121/mcp" {
				t.Errorf("url = %v, want hub URL", serena["url"])
			}
			// MiMoCode uses `enabled:true` (on), NOT the JSON family's
			// `disabled:false`. Assert the correct flag is written and the
			// wrong one is absent.
			if enabled, _ := serena["enabled"].(bool); !enabled {
				t.Errorf("enabled = %v, want true", serena["enabled"])
			}
			if _, has := serena["disabled"]; has {
				t.Errorf("unexpected 'disabled' flag (MiMoCode uses 'enabled'): %v", serena)
			}
			// Must NOT write a stdio `command` for an HTTP-direct client.
			if _, has := serena["command"]; has {
				t.Errorf("unexpected stdio 'command' field in HTTP entry: %v", serena)
			}
			if c.wantHdr {
				hdr, ok := serena["headers"].(map[string]any)
				if !ok || hdr["X-Token"] != "abc" {
					t.Errorf("headers not written: %v", serena["headers"])
				}
			} else if _, has := serena["headers"]; has {
				t.Errorf("headers should be absent when MCPEntry.Headers is empty: %v", serena)
			}
			// Unrelated entry preserved.
			if _, ok := servers["keep"]; !ok {
				t.Error("unrelated 'keep' entry dropped")
			}
		})
	}
}

// TestMimoCode_GetEntry_RoundTrips reads back an entry the adapter wrote and
// exposes URL + Headers for install-idempotency diagnostics; a missing entry
// returns (nil, nil).
func TestMimoCode_GetEntry_RoundTrips(t *testing.T) {
	o := newMimoCodeForTest(t, `{
  "mcp": {
    "serena": {
      "type": "remote",
      "url": "http://localhost:9121/mcp",
      "enabled": true,
      "headers": {"X-Token": "abc"}
    }
  }
}`)
	e, err := o.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry returned nil")
	}
	if e.URL != "http://localhost:9121/mcp" {
		t.Errorf("URL = %q, want the url field value", e.URL)
	}
	if e.Headers["X-Token"] != "abc" {
		t.Errorf("Headers = %v, want X-Token=abc", e.Headers)
	}
	// Missing entry returns nil, nil.
	if got, err := o.GetEntry("absent"); err != nil || got != nil {
		t.Errorf("GetEntry(absent) = %v, %v; want nil, nil", got, err)
	}
}

// TestMimoCode_RemoveEntry confirms removal is scoped and idempotent.
func TestMimoCode_RemoveEntry(t *testing.T) {
	o := newMimoCodeForTest(t, `{"mcp":{"serena":{"type":"remote","url":"http://localhost:9121/mcp","enabled":true},"other":{"type":"remote","url":"http://x/mcp","enabled":true}}}`)
	if err := o.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := o.GetEntry("serena"); e != nil {
		t.Errorf("serena still present: %v", e)
	}
	if e, _ := o.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
	// Idempotent: removing again is a no-op, no error.
	if err := o.RemoveEntry("serena"); err != nil {
		t.Errorf("second RemoveEntry: %v", err)
	}
}

// TestMimoCode_InitEmpty_SeedsMCPStub verifies the empty stub uses the `mcp`
// key and is idempotent on second call.
func TestMimoCode_InitEmpty_SeedsMCPStub(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mimocode.json")
	o := &mimoCodeClient{path: path}
	created, err := o.InitEmpty()
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
	if _, ok := parsed["mcp"].(map[string]any); !ok {
		t.Errorf("stub missing mcp map: %s", raw)
	}
	// Second call is idempotent: a regular file already exists.
	created2, err := o.InitEmpty()
	if err != nil {
		t.Fatalf("second InitEmpty: %v", err)
	}
	if created2 {
		t.Error("second InitEmpty should report created=false (file exists)")
	}
}

// TestMimoCode_RestoreEntryFromBackup_RestoresOrRemovesPerBackup covers the
// demigrate restore: a backup that predates the install (no entry) leads to
// the live entry being removed; a backup with a pre-hub entry restores it.
func TestMimoCode_RestoreEntryFromBackup_RestoresOrRemovesPerBackup(t *testing.T) {
	t.Run("backup lacks entry -> live entry removed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json")
		if err := os.WriteFile(path, []byte(
			`{"mcp":{"serena":{"type":"remote","url":"http://localhost:9121/mcp","enabled":true}}}`),
			0600); err != nil {
			t.Fatal(err)
		}
		backup := path + ".bak-mcp-local-hub-20260101-000000"
		if err := os.WriteFile(backup, []byte(`{"mcp":{}}`), 0600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: path}
		if err := o.RestoreEntryFromBackup(backup, "serena"); err != nil {
			t.Fatalf("RestoreEntryFromBackup: %v", err)
		}
		live, _ := os.ReadFile(path)
		var m map[string]any
		if err := json.Unmarshal(live, &m); err != nil {
			t.Fatal(err)
		}
		servers := m["mcp"].(map[string]any)
		if _, present := servers["serena"]; present {
			t.Error("serena should have been removed (backup had no pre-hub form)")
		}
	})

	t.Run("backup has pre-hub entry -> restored verbatim", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json")
		if err := os.WriteFile(path, []byte(
			`{"mcp":{"serena":{"type":"remote","url":"http://localhost:9121/mcp","enabled":true}}}`),
			0600); err != nil {
			t.Fatal(err)
		}
		backup := path + ".bak-mcp-local-hub-20260101-000000"
		// Pre-hub form: a user-configured REMOTE server at a non-loopback
		// URL (not hub-managed), which must be restored, not refused.
		if err := os.WriteFile(backup, []byte(
			`{"mcp":{"serena":{"type":"remote","url":"https://remote.example.com/mcp","enabled":true}}}`),
			0600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: path}
		if err := o.RestoreEntryFromBackup(backup, "serena"); err != nil {
			t.Fatalf("RestoreEntryFromBackup: %v", err)
		}
		e, _ := o.GetEntry("serena")
		if e == nil || e.URL != "https://remote.example.com/mcp" {
			t.Errorf("pre-hub entry not restored: %v", e)
		}
	})
}

// TestMimoCode_RestoreEntryFromBackup_RefusesHubBackupEntry asserts the
// demigrate guard: a backup whose entry is already in hub-HTTP shape (a hub
// loopback URL with no command) is refused with
// ErrBackupEntryAlreadyMigrated, while the rollback variant bypasses it.
func TestMimoCode_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mimocode.json")
	if err := os.WriteFile(path, []byte(
		`{"mcp":{"serena":{"type":"remote","url":"http://localhost:9121/mcp","enabled":true}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcp":{"serena":{"type":"remote","url":"http://localhost:9121/mcp","enabled":true}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: path}
	if err := o.RestoreEntryFromBackup(backup, "serena"); !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
	// Rollback variant bypasses the guard and writes the entry verbatim.
	if err := o.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback should bypass guard: %v", err)
	}
}

// TestMimoCode_BackupContainsEntry_And_HubManaged exercises the two backup
// predicates over present-pre-hub, present-hub, and absent cases.
func TestMimoCode_BackupContainsEntry_And_HubManaged(t *testing.T) {
	dir := t.TempDir()
	o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}

	hubBak := filepath.Join(dir, "hub.bak")
	if err := os.WriteFile(hubBak, []byte(
		`{"mcp":{"serena":{"type":"remote","url":"http://localhost:9121/mcp","enabled":true}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Pre-hub form: a user-configured remote server at a NON-loopback URL
	// (not hub-managed — must not be flagged as hub-managed).
	preHubBak := filepath.Join(dir, "prehub.bak")
	if err := os.WriteFile(preHubBak, []byte(
		`{"mcp":{"serena":{"type":"remote","url":"https://api.example.com/mcp","enabled":true}}}`),
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
			present, err := o.BackupContainsEntry(c.backup, c.entry)
			if err != nil {
				t.Fatalf("BackupContainsEntry: %v", err)
			}
			if present != c.wantPresent {
				t.Errorf("BackupContainsEntry = %v, want %v", present, c.wantPresent)
			}
			hub, err := o.BackupEntryIsHubManaged(c.backup, c.entry)
			if err != nil {
				t.Fatalf("BackupEntryIsHubManaged: %v", err)
			}
			if hub != c.wantHub {
				t.Errorf("BackupEntryIsHubManaged = %v, want %v", hub, c.wantHub)
			}
		})
	}
}

// TestMimoCode_AllStdioEntries_SkipsHTTP confirms the hub's HTTP-direct
// entries (no `command`) do not surface as stdio entries, and that a genuine
// string-command stdio entry does.
func TestMimoCode_AllStdioEntries_SkipsHTTP(t *testing.T) {
	o := newMimoCodeForTest(t, `{
  "mcp": {
    "serena": {"type": "remote", "url": "http://localhost:9121/mcp", "enabled": true},
    "local-str": {"type": "local", "command": "uvx", "args": ["serena"], "enabled": true}
  }
}`)
	stdio, err := o.AllStdioEntries()
	if err != nil {
		t.Fatalf("AllStdioEntries: %v", err)
	}
	// Only the string-command local entry surfaces; the HTTP `remote`
	// entry has no `command` and is skipped.
	if len(stdio) != 1 || stdio[0].Name != "local-str" {
		t.Fatalf("AllStdioEntries = %v, want exactly the local-str entry", stdio)
	}
}

// TestMimoCode_FindStdioLanguageServerEntries matches an mcp-language-server
// stdio entry that uses a string command (MiMoCode's array-command local form
// is not recognized by the shared string-keyed matcher, by design).
func TestMimoCode_FindStdioLanguageServerEntries(t *testing.T) {
	o := newMimoCodeForTest(t, `{
  "mcp": {
    "serena": {"type": "remote", "url": "http://localhost:9121/mcp", "enabled": true},
    "go-ls": {"type": "local", "command": "mcp-language-server", "args": ["--lsp", "go"], "enabled": true}
  }
}`)
	ls, err := o.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	if len(ls) != 1 || ls[0].Language != "go" {
		t.Fatalf("FindStdioLanguageServerEntries = %v, want one entry for language go", ls)
	}
}

// TestMimoCode_DefaultConfigPath asserts the global path resolution
// (XDG_CONFIG_HOME/mimocode → ~/.config/mimocode), that it does NOT switch to a
// Windows %APPDATA% / macOS ~/Library convention, and that an existing
// mimocode.jsonc is preferred over mimocode.json (MiMoCode reads both, so a hub
// entry written into a separate .json while a .jsonc exists could be ignored).
// Mirrors the OpenCode adapter's resolution (mimo is a fork); the only
// divergence is the .jsonc preference.
func TestMimoCode_DefaultConfigPath(t *testing.T) {
	t.Run("default ~/.config/mimocode", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join("home", "u", ".config", "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("defaultMimoCodeConfigPath = %q, want %q", got, want)
		}
	})
	t.Run("XDG_CONFIG_HOME override", func(t *testing.T) {
		xdg := filepath.Join("custom", "xdg")
		t.Setenv("XDG_CONFIG_HOME", xdg)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(xdg, "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("defaultMimoCodeConfigPath = %q, want %q", got, want)
		}
	})
	t.Run("existing mimocode.jsonc is preferred over mimocode.json", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		dir := filepath.Join(xdg, "mimocode")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		jsoncPath := filepath.Join(dir, "mimocode.jsonc")
		if err := os.WriteFile(jsoncPath, []byte("{\n  // comment\n  \"mcp\": {}\n}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		if got != jsoncPath {
			t.Errorf("with an existing mimocode.jsonc present, defaultMimoCodeConfigPath = %q, want %q", got, jsoncPath)
		}
	})
	t.Run("no mimocode.jsonc falls back to mimocode.json", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(xdg, "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("with no .jsonc present, defaultMimoCodeConfigPath = %q, want %q", got, want)
		}
	})
	t.Setenv("XDG_CONFIG_HOME", "")
	if !strings.HasSuffix(defaultMimoCodeConfigPath("/home/u"), "mimocode.json") {
		t.Errorf("path must end in mimocode.json")
	}
}

// TestMimoCode_JSONC_ReadAddRemovePreservesComments pins MiMoCode's JSONC
// tolerance end-to-end: a hand-edited config with line/block comments, an
// unrelated `$schema` key, and a trailing comma must read, AddEntry, and
// RemoveEntry without dropping comments or unrelated keys. MiMoCode's resolved
// file can be `mimocode.jsonc` (the path owner prefers it), so this is a
// first-class path, not an edge case.
func TestMimoCode_JSONC_ReadAddRemovePreservesComments(t *testing.T) {
	const fixture = `{
  // hand-written header (mimocode supports a .jsonc variant)
  /* block note */
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "keep-me": {"type": "remote", "url": "https://api.example.com/mcp", "enabled": true},
  },
}`
	o := newMimoCodeForTest(t, fixture)

	// Read tolerates comments + trailing comma.
	e, err := o.GetEntry("keep-me")
	if err != nil || e == nil || e.URL != "https://api.example.com/mcp" {
		t.Fatalf("GetEntry on JSONC mimocode config = %+v, err=%v", e, err)
	}

	// AddEntry preserves the comment-bearing file's unrelated keys.
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry on JSONC mimocode config: %v", err)
	}
	raw, _ := os.ReadFile(o.path)
	if !strings.Contains(string(raw), "hand-written header") {
		t.Errorf("AddEntry dropped the operator's comment: %s", raw)
	}
	m, err := parseJSONCBytes(raw)
	if err != nil {
		t.Fatalf("re-parse after AddEntry: %v", err)
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if _, ok := servers["serena"]; !ok {
		t.Errorf("serena entry not added: %v", servers)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Errorf("pre-existing keep-me entry dropped: %v", servers)
	}
	if got, _ := m["$schema"].(string); got != "https://opencode.ai/config.json" {
		t.Errorf("$schema = %q, want the original unrelated key value preserved", got)
	}

	// RemoveEntry keeps comments + unrelated keys.
	if err := o.RemoveEntry("keep-me"); err != nil {
		t.Fatalf("RemoveEntry on JSONC mimocode config: %v", err)
	}
	raw2, _ := os.ReadFile(o.path)
	if !strings.Contains(string(raw2), "hand-written header") {
		t.Errorf("RemoveEntry dropped the operator's comment: %s", raw2)
	}
}

// TestMimoCode_RegisteredInSupportedClients pins that the adapter is wired
// into the canonical registry, so SupportedClientNames() and AllClients()
// both surface "mimocode" (the install/scan pipeline only sees registered
// clients).
func TestMimoCode_RegisteredInSupportedClients(t *testing.T) {
	found := false
	for _, name := range SupportedClientNames() {
		if name == "mimocode" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("mimocode missing from SupportedClientNames(): %v", SupportedClientNames())
	}
	if _, ok := AllClients()["mimocode"]; !ok {
		t.Fatal("mimocode missing from AllClients()")
	}
}

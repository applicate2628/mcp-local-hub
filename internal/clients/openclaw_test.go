package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newOpenClawForTest(t *testing.T, initial string) *openClawClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &openClawClient{path: path}
}

// nestedServers walks parsed["mcp"]["servers"] and fails the test if either
// level is missing or the wrong type — the load-bearing nested-key invariant
// for OpenClaw.
func nestedServers(t *testing.T, parsed map[string]any) map[string]any {
	t.Helper()
	mcp, ok := parsed[openClawMCPKey].(map[string]any)
	if !ok {
		t.Fatalf("config missing top-level %q object: %v", openClawMCPKey, parsed)
	}
	servers, ok := mcp[openClawServersKey].(map[string]any)
	if !ok {
		t.Fatalf("config missing nested %q.%q object: %v", openClawMCPKey, openClawServersKey, parsed)
	}
	return servers
}

// TestOpenClaw_Name_And_NestedKey asserts the stable id and that the adapter
// writes entries under the NESTED `mcp.servers` path (NOT a top-level
// mcpServers / servers / context_servers), so install manifest bindings and
// the cleanup pipeline see the right section. Also confirms unrelated
// top-level fields AND unrelated sibling keys on the `mcp` object survive.
func TestOpenClaw_Name_And_NestedKey(t *testing.T) {
	o := newOpenClawForTest(t, `{"$schema":"https://openclaw.ai/config.json","theme":"dark","mcp":{"sessionIdleTtlMs":600000,"servers":{}}}`)
	if o.Name() != "openclaw" {
		t.Errorf("Name() = %q, want %q", o.Name(), "openclaw")
	}
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(o.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := nestedServers(t, parsed)
	if _, ok := servers["serena"].(map[string]any); !ok {
		t.Fatalf("entry not written under mcp.servers: %v", parsed)
	}
	// No alternate top-level MCP key leaked.
	for _, bad := range []string{"mcpServers", "servers", "context_servers"} {
		if _, has := parsed[bad]; has {
			t.Errorf("unexpected top-level key %q present: %v", bad, parsed)
		}
	}
	// Unrelated top-level fields preserved across the merge.
	if parsed["theme"] != "dark" {
		t.Errorf("unrelated 'theme' field dropped: %v", parsed)
	}
	if parsed["$schema"] != "https://openclaw.ai/config.json" {
		t.Errorf("unrelated '$schema' field dropped: %v", parsed)
	}
	// Unrelated sibling key on the `mcp` object preserved (the nested merge
	// must not clobber the rest of the `mcp` object).
	mcp := parsed[openClawMCPKey].(map[string]any)
	if _, has := mcp["sessionIdleTtlMs"]; !has {
		t.Errorf("unrelated mcp.sessionIdleTtlMs sibling dropped: %v", mcp)
	}
}

// TestOpenClaw_AddEntry_CreatesNestedPathFromBareRoot verifies AddEntry
// materializes the full `mcp` -> `servers` path even when the source config
// has neither (a hand-authored config that never declared MCP, or a config
// missing the `servers` child under an existing `mcp` object).
func TestOpenClaw_AddEntry_CreatesNestedPathFromBareRoot(t *testing.T) {
	cases := []struct {
		name    string
		initial string
	}{
		{"empty object", `{}`},
		{"top-level fields only, no mcp", `{"theme":"dark"}`},
		{"mcp present but no servers child", `{"mcp":{"sessionIdleTtlMs":0}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newOpenClawForTest(t, c.initial)
			if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(o.path)
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("parse: %v", err)
			}
			servers := nestedServers(t, parsed)
			if _, ok := servers["serena"].(map[string]any); !ok {
				t.Fatalf("serena entry not written under mcp.servers: %v", parsed)
			}
		})
	}
}

// TestOpenClaw_AddEntry_WritesHTTPShape verifies OpenClaw entries are written
// as native streamable-HTTP definitions (url, transport:"streamable-http",
// enabled:true) — OpenClaw supports Streamable HTTP MCP directly, no relay
// shim. Table-driven over headers-absent vs headers-present.
func TestOpenClaw_AddEntry_WritesHTTPShape(t *testing.T) {
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
			o := newOpenClawForTest(t, `{"mcp":{"servers":{"keep":{"url":"http://example/mcp","transport":"streamable-http","enabled":true}}}}`)
			if err := o.AddEntry(c.entry); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(o.path)
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("parse: %v", err)
			}
			servers := nestedServers(t, parsed)
			serena, ok := servers["serena"].(map[string]any)
			if !ok {
				t.Fatalf("serena entry missing: %v", servers)
			}
			if serena["url"] != "http://localhost:9121/mcp" {
				t.Errorf("url = %v, want hub URL", serena["url"])
			}
			if serena["transport"] != "streamable-http" {
				t.Errorf("transport = %v, want streamable-http", serena["transport"])
			}
			// OpenClaw uses `enabled:true` (on), NOT the JSON family's
			// `disabled:false`. Assert the correct flag is written and the
			// wrong one is absent.
			if enabled, _ := serena["enabled"].(bool); !enabled {
				t.Errorf("enabled = %v, want true", serena["enabled"])
			}
			if _, has := serena["disabled"]; has {
				t.Errorf("unexpected 'disabled' flag (OpenClaw uses 'enabled'): %v", serena)
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

// TestOpenClaw_GetEntry_RoundTrips reads back an entry the adapter wrote and
// exposes URL + Headers for install-idempotency diagnostics; a missing entry
// returns (nil, nil).
func TestOpenClaw_GetEntry_RoundTrips(t *testing.T) {
	o := newOpenClawForTest(t, `{
  "mcp": {
    "servers": {
      "serena": {
        "url": "http://localhost:9121/mcp",
        "transport": "streamable-http",
        "enabled": true,
        "headers": {"X-Token": "abc"}
      }
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
	// A config with no mcp/servers at all also returns nil, nil (no panic).
	bare := newOpenClawForTest(t, `{"theme":"dark"}`)
	if got, err := bare.GetEntry("serena"); err != nil || got != nil {
		t.Errorf("GetEntry on mcp-less config = %v, %v; want nil, nil", got, err)
	}
}

// TestOpenClaw_RemoveEntry confirms removal is scoped and idempotent, and
// that removing from an mcp-less config is a no-op (no error, no panic).
func TestOpenClaw_RemoveEntry(t *testing.T) {
	o := newOpenClawForTest(t, `{"mcp":{"servers":{"serena":{"url":"http://localhost:9121/mcp","transport":"streamable-http","enabled":true},"other":{"url":"http://x/mcp","transport":"streamable-http","enabled":true}}}}`)
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
	// Removing from an mcp-less config is a no-op.
	bare := newOpenClawForTest(t, `{"theme":"dark"}`)
	if err := bare.RemoveEntry("serena"); err != nil {
		t.Errorf("RemoveEntry on mcp-less config: %v", err)
	}
}

// TestOpenClaw_InitEmpty_SeedsNestedStub verifies the empty stub uses the
// nested `mcp.servers` path and is idempotent on second call.
func TestOpenClaw_InitEmpty_SeedsNestedStub(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	o := &openClawClient{path: path}
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
	nestedServers(t, parsed) // fails if mcp.servers missing
	// Second call is idempotent: a regular file already exists.
	created2, err := o.InitEmpty()
	if err != nil {
		t.Fatalf("second InitEmpty: %v", err)
	}
	if created2 {
		t.Error("second InitEmpty should report created=false (file exists)")
	}
}

// TestOpenClaw_RestoreEntryFromBackup_RestoresOrRemovesPerBackup covers the
// demigrate restore: a backup that predates the install (no entry) leads to
// the live entry being removed; a backup with a pre-hub entry restores it.
func TestOpenClaw_RestoreEntryFromBackup_RestoresOrRemovesPerBackup(t *testing.T) {
	t.Run("backup lacks entry -> live entry removed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "openclaw.json")
		if err := os.WriteFile(path, []byte(
			`{"mcp":{"servers":{"serena":{"url":"http://localhost:9121/mcp","transport":"streamable-http","enabled":true}}}}`),
			0600); err != nil {
			t.Fatal(err)
		}
		backup := path + ".bak-mcp-local-hub-20260101-000000"
		if err := os.WriteFile(backup, []byte(`{"mcp":{"servers":{}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		o := &openClawClient{path: path}
		if err := o.RestoreEntryFromBackup(backup, "serena"); err != nil {
			t.Fatalf("RestoreEntryFromBackup: %v", err)
		}
		live, _ := os.ReadFile(path)
		var m map[string]any
		if err := json.Unmarshal(live, &m); err != nil {
			t.Fatal(err)
		}
		servers := nestedServers(t, m)
		if _, present := servers["serena"]; present {
			t.Error("serena should have been removed (backup had no pre-hub form)")
		}
	})

	t.Run("backup has pre-hub entry -> restored verbatim", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "openclaw.json")
		if err := os.WriteFile(path, []byte(
			`{"mcp":{"servers":{"serena":{"url":"http://localhost:9121/mcp","transport":"streamable-http","enabled":true}}}}`),
			0600); err != nil {
			t.Fatal(err)
		}
		backup := path + ".bak-mcp-local-hub-20260101-000000"
		// Pre-hub form: a user-configured REMOTE server at a non-loopback URL
		// (not hub-managed), which must be restored, not refused.
		if err := os.WriteFile(backup, []byte(
			`{"mcp":{"servers":{"serena":{"url":"https://remote.example.com/mcp","transport":"streamable-http","enabled":true}}}}`),
			0600); err != nil {
			t.Fatal(err)
		}
		o := &openClawClient{path: path}
		if err := o.RestoreEntryFromBackup(backup, "serena"); err != nil {
			t.Fatalf("RestoreEntryFromBackup: %v", err)
		}
		e, _ := o.GetEntry("serena")
		if e == nil || e.URL != "https://remote.example.com/mcp" {
			t.Errorf("pre-hub entry not restored: %v", e)
		}
	})
}

// TestOpenClaw_RestoreEntryFromBackup_RefusesHubBackupEntry asserts the
// demigrate guard: a backup whose entry is already in hub-HTTP shape (a hub
// loopback URL with no command) is refused with ErrBackupEntryAlreadyMigrated,
// while the rollback variant bypasses it.
func TestOpenClaw_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(path, []byte(
		`{"mcp":{"servers":{"serena":{"url":"http://localhost:9121/mcp","transport":"streamable-http","enabled":true}}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcp":{"servers":{"serena":{"url":"http://localhost:9121/mcp","transport":"streamable-http","enabled":true}}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	o := &openClawClient{path: path}
	if err := o.RestoreEntryFromBackup(backup, "serena"); !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
	// Rollback variant bypasses the guard and writes the entry verbatim.
	if err := o.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback should bypass guard: %v", err)
	}
}

// TestOpenClaw_BackupContainsEntry_And_HubManaged exercises the two backup
// predicates over present-pre-hub, present-hub, and absent cases.
func TestOpenClaw_BackupContainsEntry_And_HubManaged(t *testing.T) {
	dir := t.TempDir()
	o := &openClawClient{path: filepath.Join(dir, "openclaw.json")}

	hubBak := filepath.Join(dir, "hub.bak")
	if err := os.WriteFile(hubBak, []byte(
		`{"mcp":{"servers":{"serena":{"url":"http://localhost:9121/mcp","transport":"streamable-http","enabled":true}}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Pre-hub form: a user-configured remote server at a NON-loopback URL
	// (not hub-managed — must not be flagged as hub-managed).
	preHubBak := filepath.Join(dir, "prehub.bak")
	if err := os.WriteFile(preHubBak, []byte(
		`{"mcp":{"servers":{"serena":{"url":"https://api.example.com/mcp","transport":"streamable-http","enabled":true}}}}`),
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

// TestOpenClaw_AllStdioEntries_SkipsHTTP confirms the hub's HTTP-direct
// entries (no `command`) do not surface as stdio entries, and that a genuine
// string-command stdio entry under the nested key does.
func TestOpenClaw_AllStdioEntries_SkipsHTTP(t *testing.T) {
	o := newOpenClawForTest(t, `{
  "mcp": {
    "servers": {
      "serena": {"url": "http://localhost:9121/mcp", "transport": "streamable-http", "enabled": true},
      "local-str": {"command": "uvx", "args": ["serena"], "enabled": true}
    }
  }
}`)
	stdio, err := o.AllStdioEntries()
	if err != nil {
		t.Fatalf("AllStdioEntries: %v", err)
	}
	// Only the string-command stdio entry surfaces; the HTTP entry has no
	// `command` and is skipped.
	if len(stdio) != 1 || stdio[0].Name != "local-str" {
		t.Fatalf("AllStdioEntries = %v, want exactly the local-str entry", stdio)
	}
}

// TestOpenClaw_FindStdioLanguageServerEntries matches an mcp-language-server
// stdio entry that uses a string command under the nested key.
func TestOpenClaw_FindStdioLanguageServerEntries(t *testing.T) {
	o := newOpenClawForTest(t, `{
  "mcp": {
    "servers": {
      "serena": {"url": "http://localhost:9121/mcp", "transport": "streamable-http", "enabled": true},
      "go-ls": {"command": "mcp-language-server", "args": ["--lsp", "go"], "enabled": true}
    }
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

// TestOpenClaw_DefaultConfigPath asserts the ~/.openclaw/openclaw.json path
// resolution and that it does NOT switch to a Windows %APPDATA% / macOS
// ~/Library convention (OpenClaw uses ~/.openclaw on every OS).
func TestOpenClaw_DefaultConfigPath(t *testing.T) {
	got := defaultOpenClawConfigPath(filepath.Join("home", "u"))
	want := filepath.Join("home", "u", ".openclaw", "openclaw.json")
	if got != want {
		t.Errorf("defaultOpenClawConfigPath = %q, want %q", got, want)
	}
	if !strings.HasSuffix(defaultOpenClawConfigPath("/home/u"), filepath.Join(".openclaw", "openclaw.json")) {
		t.Errorf("path must end in .openclaw/openclaw.json")
	}
}

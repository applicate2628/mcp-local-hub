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

// TestMimoCode_GetEntry_LocalEntryCarriesRaw_RoundTripPreservesIt pins Finding
// 5: a user's LOCAL entry (command ARRAY, `environment`, NO url) must survive
// the install/register snapshot-rollback (GetEntry → AddEntry(*priorEntry))
// UNCHANGED. Pre-fix GetEntry returned an MCPEntry with empty URL, and the
// rollback's AddEntry rewrote the local entry as a broken
// `{"type":"remote","url":"","enabled":true}` REMOTE entry — corruption.
// GetEntry now carries the verbatim shape in MCPEntry.Raw and AddEntry writes
// Raw back unchanged, so the round-trip is faithful.
func TestMimoCode_GetEntry_LocalEntryCarriesRaw_RoundTripPreservesIt(t *testing.T) {
	o := newMimoCodeForTest(t, `{
  "mcp": {
    "memory": {
      "type": "local",
      "command": ["npx", "-y", "@modelcontextprotocol/server-memory"],
      "environment": {"API_KEY": "secret"},
      "enabled": true
    }
  }
}`)
	prior, err := o.GetEntry("memory")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if prior == nil {
		t.Fatal("GetEntry returned nil for a present local entry")
	}
	// A local entry has no url; the lean URL representation cannot carry it, so
	// the raw shape must be present for faithful restore.
	if prior.URL != "" {
		t.Errorf("local entry URL = %q, want empty (no url field)", prior.URL)
	}
	if prior.Raw == nil {
		t.Fatal("local entry must carry its verbatim shape in MCPEntry.Raw")
	}
	if prior.Raw["type"] != "local" {
		t.Errorf("Raw[type] = %v, want local", prior.Raw["type"])
	}

	// Simulate the install-rollback round-trip: clobber the entry (as a failed
	// install would), then restore the prior snapshot via AddEntry.
	if err := o.AddEntry(MCPEntry{Name: "memory", URL: "http://localhost:9123/mcp"}); err != nil {
		t.Fatalf("AddEntry (simulated install): %v", err)
	}
	if err := o.AddEntry(*prior); err != nil {
		t.Fatalf("AddEntry (rollback restore): %v", err)
	}

	// The restored entry must be the ORIGINAL local shape, not a remote url:"" entry.
	restored, err := o.GetEntry("memory")
	if err != nil {
		t.Fatalf("GetEntry after restore: %v", err)
	}
	if restored == nil || restored.Raw == nil {
		t.Fatal("restored local entry lost its raw shape")
	}
	if restored.Raw["type"] != "local" {
		t.Errorf("restored Raw[type] = %v, want local (rollback must NOT rewrite it as remote)", restored.Raw["type"])
	}
	if _, hasURL := restored.Raw["url"]; hasURL {
		t.Errorf("restored local entry must NOT have a url field; got %v", restored.Raw["url"])
	}
	cmd, ok := restored.Raw["command"].([]any)
	if !ok || len(cmd) != 3 || cmd[0] != "npx" {
		t.Errorf("restored command array corrupted: %v", restored.Raw["command"])
	}
	env, ok := restored.Raw["environment"].(map[string]any)
	if !ok || env["API_KEY"] != "secret" {
		t.Errorf("restored environment lost: %v", restored.Raw["environment"])
	}
}

// TestMimoCode_DeepMergeRead pins Finding 1 at the adapter level: GetEntry reads
// the DEEP-MERGED view across the accepted global layers, so a server defined
// only in config.json is visible even when the highest-precedence file
// (mimocode.jsonc) holds only unrelated settings, and a later layer overrides an
// earlier one per server name.
func TestMimoCode_DeepMergeRead(t *testing.T) {
	t.Setenv("MIMOCODE_CONFIG", "")
	t.Setenv("MIMOCODE_CONFIG_DIR", "")
	t.Setenv("MIMOCODE_HOME", "")
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "mimocode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// config.json holds memory (stale) AND a unique server `legacy`.
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9999/STALE","enabled":true},"legacy":{"type":"remote","url":"http://localhost:9100/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// mimocode.jsonc (top layer) overrides memory's URL and adds only settings.
	jsoncPath := filepath.Join(dir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath,
		[]byte("{\n  // top layer\n  \"theme\": \"dark\",\n  \"mcp\": {\"memory\": {\"type\":\"remote\",\"url\":\"http://localhost:9123/mcp\",\"enabled\":true}}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	home, _ := os.UserHomeDir()
	o := &mimoCodeClient{path: jsoncPath, home: home}

	// `legacy` lives only in config.json — it must be visible.
	leg, err := o.GetEntry("legacy")
	if err != nil {
		t.Fatalf("GetEntry(legacy): %v", err)
	}
	if leg == nil || leg.URL != "http://localhost:9100/mcp" {
		t.Errorf("config.json-only server must be visible via deep-merge; got %+v", leg)
	}
	// `memory` is overridden by the top layer (later wins).
	mem, err := o.GetEntry("memory")
	if err != nil {
		t.Fatalf("GetEntry(memory): %v", err)
	}
	if mem == nil || mem.URL != "http://localhost:9123/mcp" {
		t.Errorf("later layer must override per server name; got %+v", mem)
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

// TestMimoCode_DefaultConfigPath asserts the global path resolution precedence
// (MIMOCODE_HOME/config → XDG_CONFIG_HOME/mimocode → ~/.config/mimocode), that
// it does NOT switch to a Windows %APPDATA% / macOS ~/Library convention, and
// that an existing mimocode.jsonc is preferred over mimocode.json (because
// MiMoCode merges .jsonc OVER .json at load time — see defaultMimoCodeConfigPath
// doc; sources verified against the MiMoCode config-overrides docs 2026-06).
func TestMimoCode_DefaultConfigPath(t *testing.T) {
	// Isolate from any ambient env on the host running the tests.
	t.Setenv("MIMOCODE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	t.Run("default ~/.config/mimocode", func(t *testing.T) {
		t.Setenv("MIMOCODE_HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join("home", "u", ".config", "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("defaultMimoCodeConfigPath = %q, want %q", got, want)
		}
	})
	t.Run("XDG_CONFIG_HOME override", func(t *testing.T) {
		t.Setenv("MIMOCODE_HOME", "")
		xdg := filepath.Join("custom", "xdg")
		t.Setenv("XDG_CONFIG_HOME", xdg)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(xdg, "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("defaultMimoCodeConfigPath = %q, want %q", got, want)
		}
	})
	t.Run("MIMOCODE_HOME wins over XDG, config/ subdir", func(t *testing.T) {
		// Absolute MIMOCODE_HOME → $MIMOCODE_HOME/config/mimocode.json,
		// taking precedence over XDG and home. Use an absolute temp dir so the
		// absolute-path gate (filepath.IsAbs) accepts it on every OS.
		mh := t.TempDir()
		t.Setenv("MIMOCODE_HOME", mh)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join("custom", "xdg"))
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(mh, "config", "mimocode.json")
		if got != want {
			t.Errorf("defaultMimoCodeConfigPath = %q, want %q", got, want)
		}
	})
	t.Run("relative MIMOCODE_HOME is ignored (docs require absolute)", func(t *testing.T) {
		t.Setenv("MIMOCODE_HOME", filepath.Join("not", "absolute"))
		t.Setenv("XDG_CONFIG_HOME", "")
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join("home", "u", ".config", "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("relative MIMOCODE_HOME should be ignored; got %q, want %q", got, want)
		}
	})
	t.Run("existing mimocode.jsonc is preferred over mimocode.json", func(t *testing.T) {
		// Point XDG at a real temp dir and seed a mimocode.jsonc there: the
		// resolver must target the .jsonc (the file MiMoCode actually honors).
		xdg := t.TempDir()
		t.Setenv("MIMOCODE_HOME", "")
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
		t.Setenv("MIMOCODE_HOME", "")
		t.Setenv("XDG_CONFIG_HOME", xdg)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(xdg, "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("with no .jsonc present, defaultMimoCodeConfigPath = %q, want %q", got, want)
		}
	})
	t.Setenv("MIMOCODE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if !strings.HasSuffix(defaultMimoCodeConfigPath("/home/u"), "mimocode.json") {
		t.Errorf("path must end in mimocode.json")
	}
}

// TestMimoCode_ConfigFilePrecedence pins the documented global-file merge
// order (config.json < mimocode.json < mimocode.jsonc, "merged in that order,
// later overrides earlier"): the path owner targets the HIGHEST-precedence
// EXISTING file so the hub write wins the merge and scan/backup read the file
// that actually holds entries. Crucially it covers `config.json` — an install
// whose MCP entries live solely there used to be invisible to scan/backup
// (Findings 1+4). Env is isolated so an ambient MIMOCODE_* on the host running
// the tests cannot leak in.
func TestMimoCode_ConfigFilePrecedence(t *testing.T) {
	t.Setenv("MIMOCODE_CONFIG", "")
	t.Setenv("MIMOCODE_CONFIG_DIR", "")
	t.Setenv("MIMOCODE_HOME", "")

	seed := func(t *testing.T, dir string, names ...string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(`{"mcp":{}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("config.json only is selected (not silently skipped)", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		dir := filepath.Join(xdg, "mimocode")
		seed(t, dir, "config.json")
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(dir, "config.json")
		if got != want {
			t.Errorf("config.json-only install: got %q, want %q", got, want)
		}
	})

	t.Run("mimocode.json wins over config.json", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		dir := filepath.Join(xdg, "mimocode")
		seed(t, dir, "config.json", "mimocode.json")
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(dir, "mimocode.json")
		if got != want {
			t.Errorf("mimocode.json+config.json: got %q, want %q", got, want)
		}
	})

	t.Run("mimocode.jsonc wins over both", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		dir := filepath.Join(xdg, "mimocode")
		seed(t, dir, "config.json", "mimocode.json", "mimocode.jsonc")
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(dir, "mimocode.jsonc")
		if got != want {
			t.Errorf("all three present: got %q, want %q", got, want)
		}
	})

	t.Run("nothing present falls back to mimocode.json", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(xdg, "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("empty dir: got %q, want %q", got, want)
		}
	})
}

// TestMimoCode_ConfigEnvOverrides pins the documented env precedence
// MIMOCODE_CONFIG (file) > MIMOCODE_CONFIG_DIR (dir) > MIMOCODE_HOME > XDG >
// default (Finding 2). MIMOCODE_CONFIG points straight at a file and bypasses
// all file probing; MIMOCODE_CONFIG_DIR replaces the directory but keeps the
// in-dir file preference. Relative values are ignored (absolute-path
// requirement mirrors MIMOCODE_HOME).
func TestMimoCode_ConfigEnvOverrides(t *testing.T) {
	t.Setenv("MIMOCODE_CONFIG", "")
	t.Setenv("MIMOCODE_CONFIG_DIR", "")
	t.Setenv("MIMOCODE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	t.Run("MIMOCODE_CONFIG (absolute file) is used verbatim, no probing", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "custom.jsonc")
		t.Setenv("MIMOCODE_CONFIG", f)
		// Even with MIMOCODE_HOME + XDG set, the direct file override wins.
		t.Setenv("MIMOCODE_HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		if got != f {
			t.Errorf("MIMOCODE_CONFIG override: got %q, want %q", got, f)
		}
	})

	t.Run("relative MIMOCODE_CONFIG is ignored", func(t *testing.T) {
		t.Setenv("MIMOCODE_CONFIG", filepath.Join("not", "absolute", "x.json"))
		t.Setenv("MIMOCODE_CONFIG_DIR", "")
		t.Setenv("MIMOCODE_HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join("home", "u", ".config", "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("relative MIMOCODE_CONFIG should be ignored: got %q, want %q", got, want)
		}
	})

	t.Run("MIMOCODE_CONFIG_DIR (absolute) replaces the dir, keeps mimocode.json(c) preference", func(t *testing.T) {
		t.Setenv("MIMOCODE_CONFIG", "")
		cd := t.TempDir()
		t.Setenv("MIMOCODE_CONFIG_DIR", cd)
		// MIMOCODE_HOME present but lower precedence — CONFIG_DIR must win.
		t.Setenv("MIMOCODE_HOME", t.TempDir())
		// Seed mimocode.json in the custom dir → resolver picks it.
		if err := os.WriteFile(filepath.Join(cd, "mimocode.json"), []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(cd, "mimocode.json")
		if got != want {
			t.Errorf("MIMOCODE_CONFIG_DIR override: got %q, want %q", got, want)
		}
	})

	t.Run("config.json is NOT selected under MIMOCODE_CONFIG_DIR (global-dir-only layer — Finding 2)", func(t *testing.T) {
		// Docs (https://mimo.xiaomi.com/mimocode/config-overrides): "`.mimocode/`
		// and MIMOCODE_CONFIG_DIR likewise use mimocode.json(c)" — config.json is
		// a GLOBAL-default-dir-only layer MiMo does NOT load from a custom dir. A
		// custom dir holding ONLY config.json must therefore resolve to a fresh
		// mimocode.json (the file the adapter creates + MiMo will actually load),
		// NOT the config.json (which MiMo would ignore).
		t.Setenv("MIMOCODE_CONFIG", "")
		cd := t.TempDir()
		t.Setenv("MIMOCODE_CONFIG_DIR", cd)
		t.Setenv("MIMOCODE_HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		if err := os.WriteFile(filepath.Join(cd, "config.json"), []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(cd, "mimocode.json")
		if got != want {
			t.Errorf("config.json under a custom CONFIG_DIR must be ignored; got %q, want %q", got, want)
		}
	})

	t.Run("relative MIMOCODE_CONFIG_DIR is ignored (falls to MIMOCODE_HOME)", func(t *testing.T) {
		t.Setenv("MIMOCODE_CONFIG", "")
		t.Setenv("MIMOCODE_CONFIG_DIR", filepath.Join("rel", "dir"))
		mh := t.TempDir()
		t.Setenv("MIMOCODE_HOME", mh)
		t.Setenv("XDG_CONFIG_HOME", "")
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(mh, "config", "mimocode.json")
		if got != want {
			t.Errorf("relative MIMOCODE_CONFIG_DIR should fall through to MIMOCODE_HOME: got %q, want %q", got, want)
		}
	})
}

// TestMimoCode_JSONC_ReadAddRemovePreservesComments pins MiMoCode's JSONC
// tolerance end-to-end: a hand-edited config with line/block comments, an
// unrelated `$schema` key, and a trailing comma must read, AddEntry, and
// RemoveEntry without dropping comments or unrelated keys. MiMoCode's resolved
// file can be `mimocode.jsonc` (the path owner prefers it), so this is a
// first-class path, not an edge case. Mirrors TestJSONC_OpenCode_*.
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

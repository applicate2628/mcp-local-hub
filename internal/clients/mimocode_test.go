package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolateMimoCodeEnv clears every MiMoCode config-resolution env var for the
// duration of a test so no inherited MIMOCODE_*/XDG_CONFIG_HOME from the
// developer's shell leaks in. t.Setenv restores them at test end. State-safety:
// the adapter must NEVER read or write the developer's real
// ~/.config/mimocode — every test uses t.TempDir paths and isolates env.
func isolateMimoCodeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MIMOCODE_CONFIG", "MIMOCODE_CONFIG_DIR", "MIMOCODE_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
}

func newMimoCodeForTest(t *testing.T, initial string) *mimoCodeClient {
	t.Helper()
	isolateMimoCodeEnv(t)
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

// TestMimoCode_GetEntry_LocalEntryReturnsNil pins the rollback-safety contract
// (spec #4 / bot PR #420 r4+r5): a LOCAL entry (type:"local", a `command`
// array, NO url) is NOT representable by the lean MCPEntry. GetEntry must
// return (nil, nil) for it so the install/register rollback snapshot/restore
// SKIPS it rather than rewriting it into a broken {type:remote, url:""} entry.
func TestMimoCode_GetEntry_LocalEntryReturnsNil(t *testing.T) {
	o := newMimoCodeForTest(t, `{
  "mcp": {
    "local-srv": {
      "type": "local",
      "command": ["npx", "-y", "some-mcp"],
      "environment": {"API_KEY": "x"},
      "enabled": true
    }
  }
}`)
	e, err := o.GetEntry("local-srv")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e != nil {
		t.Fatalf("GetEntry on a URL-less local entry = %+v, want nil (rollback must skip, not corrupt to url:\"\")", e)
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
	isolateMimoCodeEnv(t)
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
		isolateMimoCodeEnv(t)
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
		isolateMimoCodeEnv(t)
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
	isolateMimoCodeEnv(t)
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
	isolateMimoCodeEnv(t)
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

// TestMimoCode_DefaultConfigPath asserts the global path resolution, the
// MiMoCode env precedence (MIMOCODE_CONFIG > MIMOCODE_CONFIG_DIR >
// MIMOCODE_HOME > XDG_CONFIG_HOME > ~/.config/mimocode), that relative env
// values are IGNORED, that it does NOT switch to a Windows %APPDATA% / macOS
// ~/Library convention, and that an existing mimocode.jsonc is preferred over
// mimocode.json. Faithful to paths.ts + global.ts (mimo is an OpenCode fork);
// the only divergences are the .jsonc preference and the env chain.
func TestMimoCode_DefaultConfigPath(t *testing.T) {
	t.Run("default ~/.config/mimocode", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join("home", "u", ".config", "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("defaultMimoCodeConfigPath = %q, want %q", got, want)
		}
	})
	t.Run("absolute XDG_CONFIG_HOME override", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		xdg := t.TempDir() // t.TempDir is absolute
		t.Setenv("XDG_CONFIG_HOME", xdg)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(xdg, "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("defaultMimoCodeConfigPath = %q, want %q", got, want)
		}
	})
	t.Run("relative XDG_CONFIG_HOME is IGNORED", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		// A relative XDG value is ignored (XDG spec + global.ts posture) →
		// falls back to ~/.config/mimocode, never the relative dir.
		t.Setenv("XDG_CONFIG_HOME", filepath.Join("rel", "xdg"))
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join("home", "u", ".config", "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("relative XDG_CONFIG_HOME should be ignored: got %q, want %q", got, want)
		}
	})
	t.Run("MIMOCODE_HOME override (absolute → /config)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		mh := t.TempDir()
		t.Setenv("MIMOCODE_HOME", mh)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(mh, "config", "mimocode.json")
		if got != want {
			t.Errorf("MIMOCODE_HOME resolution = %q, want %q", got, want)
		}
	})
	t.Run("MIMOCODE_CONFIG_DIR override (absolute DIR, verbatim)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		cd := t.TempDir()
		t.Setenv("MIMOCODE_CONFIG_DIR", cd)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(cd, "mimocode.json")
		if got != want {
			t.Errorf("MIMOCODE_CONFIG_DIR resolution = %q, want %q", got, want)
		}
	})
	t.Run("MIMOCODE_CONFIG (absolute FILE) used verbatim, bypasses dir probing", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		f := filepath.Join(t.TempDir(), "my-custom.json")
		t.Setenv("MIMOCODE_CONFIG", f)
		// Even with CONFIG_DIR also set, the FILE override wins.
		t.Setenv("MIMOCODE_CONFIG_DIR", t.TempDir())
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		if got != f {
			t.Errorf("MIMOCODE_CONFIG should be used verbatim: got %q, want %q", got, f)
		}
	})
	t.Run("precedence: CONFIG > CONFIG_DIR > HOME > XDG", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		cfg := filepath.Join(t.TempDir(), "winner.json")
		t.Setenv("MIMOCODE_CONFIG", cfg)
		t.Setenv("MIMOCODE_CONFIG_DIR", t.TempDir())
		t.Setenv("MIMOCODE_HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		if got := defaultMimoCodeConfigPath(filepath.Join("home", "u")); got != cfg {
			t.Errorf("MIMOCODE_CONFIG must win the precedence chain: got %q", got)
		}
	})
	t.Run("existing mimocode.jsonc is preferred over mimocode.json", func(t *testing.T) {
		isolateMimoCodeEnv(t)
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
		isolateMimoCodeEnv(t)
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(xdg, "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("with no .jsonc present, defaultMimoCodeConfigPath = %q, want %q", got, want)
		}
	})
	t.Run("no config.json layer (only mimocode.json/.jsonc)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		dir := filepath.Join(xdg, "mimocode")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// A lone config.json must NOT be selected — paths.ts loads only
		// ${name}.{json,jsonc}.
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(dir, "mimocode.json")
		if got != want {
			t.Errorf("config.json must not be a selected layer: got %q, want %q", got, want)
		}
	})
}

// TestMimoCode_InDirLayerMerge pins the in-dir two-file deep merge (spec #7):
// an entry in mimocode.json is visible when mimocode.jsonc also exists (and
// vice versa), and on a same-key conflict the .jsonc layer wins.
func TestMimoCode_InDirLayerMerge(t *testing.T) {
	t.Run("entry in .json visible when .jsonc exists", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		jsonPath := filepath.Join(dir, "mimocode.json")
		jsoncPath := filepath.Join(dir, "mimocode.jsonc")
		if err := os.WriteFile(jsonPath, []byte(`{"mcp":{"only-json":{"type":"remote","url":"http://localhost:9001/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// .jsonc exists but carries an UNRELATED setting (no mcp entry).
		if err := os.WriteFile(jsoncPath, []byte("{\n  // jsonc layer\n  \"theme\": \"dark\"\n}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Write target is the .jsonc (top layer), but the read must still see
		// the lower-layer .json entry.
		o := &mimoCodeClient{path: jsoncPath}
		e, err := o.GetEntry("only-json")
		if err != nil {
			t.Fatalf("GetEntry: %v", err)
		}
		if e == nil || e.URL != "http://localhost:9001/mcp" {
			t.Errorf("lower-layer .json entry not visible through merged read: %+v", e)
		}
	})
	t.Run("entry in .jsonc visible when .json exists", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		jsonPath := filepath.Join(dir, "mimocode.json")
		jsoncPath := filepath.Join(dir, "mimocode.jsonc")
		if err := os.WriteFile(jsonPath, []byte(`{"theme":"light"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(jsoncPath, []byte("{\n  // jsonc layer\n  \"mcp\": {\"only-jsonc\": {\"type\":\"remote\",\"url\":\"http://localhost:9002/mcp\",\"enabled\":true}}\n}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: jsoncPath}
		e, err := o.GetEntry("only-jsonc")
		if err != nil {
			t.Fatalf("GetEntry: %v", err)
		}
		if e == nil || e.URL != "http://localhost:9002/mcp" {
			t.Errorf(".jsonc-layer entry not visible through merged read: %+v", e)
		}
	})
	t.Run(".jsonc wins on same-key conflict", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		jsonPath := filepath.Join(dir, "mimocode.json")
		jsoncPath := filepath.Join(dir, "mimocode.jsonc")
		if err := os.WriteFile(jsonPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://json-layer/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(jsoncPath, []byte("{\n  \"mcp\": {\"serena\": {\"type\":\"remote\",\"url\":\"http://jsonc-layer/mcp\",\"enabled\":true}}\n}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: jsoncPath}
		e, err := o.GetEntry("serena")
		if err != nil {
			t.Fatalf("GetEntry: %v", err)
		}
		if e == nil || e.URL != "http://jsonc-layer/mcp" {
			t.Errorf("on conflict the .jsonc layer must win: %+v", e)
		}
	})
}

// TestMimoCode_RemoveEntry_ClearsBothLayers pins that RemoveEntry deletes the
// entry from BOTH layer files (spec #7 / bot PR #420 r4): a lower-layer entry
// must not be left active when only the top layer is patched.
func TestMimoCode_RemoveEntry_ClearsBothLayers(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "mimocode.json")
	jsoncPath := filepath.Join(dir, "mimocode.jsonc")
	// Same-named entry present in BOTH layers.
	if err := os.WriteFile(jsonPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://json-layer/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsoncPath, []byte("{\n  \"mcp\": {\"serena\": {\"type\":\"remote\",\"url\":\"http://jsonc-layer/mcp\",\"enabled\":true}}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: jsoncPath}
	if err := o.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	// After remove, the merged read must show NO serena (cleared from both).
	if e, _ := o.GetEntry("serena"); e != nil {
		t.Errorf("serena still visible after RemoveEntry — a layer was left active: %+v", e)
	}
	// Verify each on-disk layer no longer carries serena.
	for _, p := range []string{jsonPath, jsoncPath} {
		data, _ := os.ReadFile(p)
		m, err := parseJSONCBytes(data)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		servers, _ := m[mimoCodeMCPKey].(map[string]any)
		if _, present := servers["serena"]; present {
			t.Errorf("serena still present in layer %s after RemoveEntry", filepath.Base(p))
		}
	}
}

// TestMimoCode_ExplicitPath_NoSiblingMerge pins the explicit-path honoring
// (spec #6 / bot PR #420 r4): when the adapter path is an EXPLICIT override
// whose basename is NOT a known layer file name, the layer resolver returns
// just that file and never pulls in sibling mimocode.json/.jsonc — so a
// temp/test scan never reaches the real ~/.config/mimocode.
func TestMimoCode_ExplicitPath_NoSiblingMerge(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	// A sibling mimocode.json that MUST be ignored when the explicit path is a
	// differently-named file.
	if err := os.WriteFile(filepath.Join(dir, "mimocode.json"), []byte(`{"mcp":{"sibling":{"type":"remote","url":"http://sibling/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(dir, "explicit-override.json")
	if err := os.WriteFile(explicit, []byte(`{"mcp":{"chosen":{"type":"remote","url":"http://chosen/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	layers := mimoCodeLayerFiles(explicit)
	if len(layers) != 1 || layers[0] != explicit {
		t.Fatalf("explicit override must resolve to exactly [%q], got %v", explicit, layers)
	}
	o := &mimoCodeClient{path: explicit}
	// The chosen file's entry is visible; the sibling's is NOT.
	if e, _ := o.GetEntry("chosen"); e == nil {
		t.Error("explicit file's own entry should be visible")
	}
	if e, _ := o.GetEntry("sibling"); e != nil {
		t.Errorf("sibling mimocode.json entry must NOT be merged for an explicit path: %+v", e)
	}
}

// TestMimoCode_JSONC_ReadAddRemovePreservesComments pins MiMoCode's JSONC
// tolerance end-to-end: a hand-edited config with line/block comments, an
// unrelated `$schema` key, and a trailing comma must read, AddEntry, and
// RemoveEntry without dropping comments or unrelated keys. MiMoCode's resolved
// file can be `mimocode.jsonc` (the path owner prefers it), so this is a
// first-class path, not an edge case.
func TestMimoCode_JSONC_ReadAddRemovePreservesComments(t *testing.T) {
	isolateMimoCodeEnv(t)
	const fixture = `{
  // hand-written header (mimocode supports a .jsonc variant)
  /* block note */
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "keep-me": {"type": "remote", "url": "https://api.example.com/mcp", "enabled": true},
  },
}`
	// Use a real mimocode.jsonc so the layer resolver treats it as a known
	// layer (single file in an otherwise-empty dir).
	dir := t.TempDir()
	path := filepath.Join(dir, "mimocode.jsonc")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: path}

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

// TestMimoCode_NoWindowsAppDataConvention guards that the path resolver stays
// on the XDG ~/.config/mimocode location on every OS (it never switches to a
// Windows %APPDATA% / macOS ~/Library convention — mimo is XDG-only).
func TestMimoCode_NoWindowsAppDataConvention(t *testing.T) {
	isolateMimoCodeEnv(t)
	got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
	if !strings.HasSuffix(got, filepath.Join(".config", "mimocode", "mimocode.json")) {
		t.Errorf("path must end in .config/mimocode/mimocode.json on %s, got %q", runtime.GOOS, got)
	}
}

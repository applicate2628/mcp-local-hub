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
	for _, k := range []string{"MIMOCODE_CONFIG", "MIMOCODE_CONFIG_CONTENT", "MIMOCODE_CONFIG_DIR", "MIMOCODE_HOME", "XDG_CONFIG_HOME"} {
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

// TestMimoCode_GetEntry_LocalEntryCarriesRaw pins the rollback-safety contract
// (spec #5 / bot PR #420): a LOCAL entry (type:"local", a `command` ARRAY, NO
// url) is NOT representable by the lean URL/Headers MCPEntry. GetEntry must NOT
// return nil (which would make the install rollback's nil-prior else-branch
// RemoveEntry DELETE the operator's original) and must NOT project it onto a
// broken {type:remote, url:""} entry. Instead it carries the verbatim entry map
// in MCPEntry.Raw (URL left empty) so AddEntry(*prior) restores it byte-exact.
func TestMimoCode_GetEntry_LocalEntryCarriesRaw(t *testing.T) {
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
	if e == nil {
		t.Fatal("GetEntry on a local entry returned nil — rollback nil-prior would DELETE the operator's original")
	}
	if e.URL != "" {
		t.Errorf("local entry URL = %q, want empty (Raw carries the value)", e.URL)
	}
	if e.Raw == nil {
		t.Fatal("local entry MCPEntry.Raw is nil — rollback cannot restore it")
	}
	if e.Raw["type"] != "local" {
		t.Errorf("Raw['type'] = %v, want local (verbatim entry preserved)", e.Raw["type"])
	}
	if _, ok := e.Raw["command"].([]any); !ok {
		t.Errorf("Raw['command'] should preserve the array shape: %#v", e.Raw["command"])
	}
}

// TestMimoCode_LocalEntry_RollbackRoundTrip pins the END-TO-END data-safety of
// the local-entry rollback (spec #5 / VERDICT A regression): a GetEntry →
// AddEntry(*prior) cycle (exactly what install.go:2607-2616 runs on rollback)
// restores the operator's local command-array entry byte-identical. Without the
// Raw field, the prior is nil → rollback RemoveEntry → the operator's entry is
// gone (data loss); with Raw, AddEntry writes the raw map verbatim.
func TestMimoCode_LocalEntry_RollbackRoundTrip(t *testing.T) {
	const original = `{
  "mcp": {
    "local-srv": {
      "type": "local",
      "command": ["npx", "-y", "some-mcp", "--flag"],
      "environment": {"API_KEY": "x"},
      "enabled": true
    }
  }
}`
	o := newMimoCodeForTest(t, original)

	// 1. Snapshot the prior (rollback step 1).
	prior, err := o.GetEntry("local-srv")
	if err != nil || prior == nil {
		t.Fatalf("GetEntry prior = %+v, err=%v; want non-nil", prior, err)
	}

	// 2. Hub install overwrites it with a remote-http entry.
	if err := o.AddEntry(MCPEntry{Name: "local-srv", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry (install): %v", err)
	}

	// 3. Rollback restores the prior verbatim (Raw wins; URL/Headers ignored).
	if err := o.AddEntry(*prior); err != nil {
		t.Fatalf("AddEntry(*prior) (rollback): %v", err)
	}

	// The restored entry must be the operator's local command-array form again,
	// not a {type:remote,url:""} corruption and not deleted.
	raw, _ := os.ReadFile(o.path)
	m, err := parseJSONCBytes(raw)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	servers := m[mimoCodeMCPKey].(map[string]any)
	restored, ok := servers["local-srv"].(map[string]any)
	if !ok {
		t.Fatalf("local-srv missing after rollback (deleted = data loss): %v", servers)
	}
	if restored["type"] != "local" {
		t.Errorf("restored type = %v, want local", restored["type"])
	}
	cmd, ok := restored["command"].([]any)
	if !ok || len(cmd) != 4 || cmd[0] != "npx" {
		t.Errorf("restored command array not preserved: %#v", restored["command"])
	}
	if _, has := restored["url"]; has {
		t.Errorf("restored entry has a url field — local entry was corrupted to remote: %v", restored)
	}
	env, ok := restored["environment"].(map[string]any)
	if !ok || env["API_KEY"] != "x" {
		t.Errorf("restored environment not preserved: %#v", restored["environment"])
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

// TestMimoCode_DefaultConfigPath asserts the global WRITE-target resolution: the
// global DIR follows MIMOCODE_HOME > XDG_CONFIG_HOME > ~/.config/mimocode (with
// relative env values IGNORED and no Windows %APPDATA% / macOS ~/Library
// convention), and the write FILE is ALWAYS the fixed seed mimocode.json — it
// does NOT float to mimocode.jsonc (bot PR #420 finding 5) and config.json is
// never a write target. MIMOCODE_CONFIG / MIMOCODE_CONFIG_DIR are READ layers,
// NOT write targets (bot PR #420 finding 1). Faithful to config.ts
// loadInstanceState + paths.ts + global.ts (mimo is an OpenCode fork).
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
	t.Run("MIMOCODE_CONFIG_DIR is an OVERLAY: write target stays in the GLOBAL dir", func(t *testing.T) {
		// Spec #3: MIMOCODE_CONFIG_DIR is an ADDITIONAL overlay dir merged ON TOP
		// of the global dir (config.ts appends it to `directories`), NOT a
		// replacement. The hub WRITES the canonical per-user global file (which
		// MiMoCode still loads); the overlay only contributes to READS. So the
		// write target must resolve into the GLOBAL dir, not the custom dir.
		isolateMimoCodeEnv(t)
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg) // global dir = $XDG/mimocode
		t.Setenv("MIMOCODE_CONFIG_DIR", t.TempDir())
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(xdg, "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("with MIMOCODE_CONFIG_DIR set, write target must stay in the global dir: got %q, want %q", got, want)
		}
	})
	t.Run("MIMOCODE_CONFIG (absolute FILE) is a READ LAYER, NOT the write target", func(t *testing.T) {
		// bot PR #420 finding 1: MIMOCODE_CONFIG merges ABOVE the global layers,
		// it does NOT replace them — so the WRITE target stays the global seed
		// mimocode.json (the hub never writes the operator's MIMOCODE_CONFIG file).
		isolateMimoCodeEnv(t)
		f := filepath.Join(t.TempDir(), "my-custom.json")
		t.Setenv("MIMOCODE_CONFIG", f)
		t.Setenv("MIMOCODE_CONFIG_DIR", t.TempDir())
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(xdg, "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("MIMOCODE_CONFIG must NOT become the write target (it is a read layer): got %q, want %q", got, want)
		}
	})
	t.Run("write-dir precedence: HOME > XDG; CONFIG/CONFIG_DIR are read layers, not write targets", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		mh := t.TempDir()
		t.Setenv("MIMOCODE_CONFIG", filepath.Join(t.TempDir(), "custom.json"))
		t.Setenv("MIMOCODE_CONFIG_DIR", t.TempDir())
		t.Setenv("MIMOCODE_HOME", mh)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // lower precedence than HOME
		want := filepath.Join(mh, "config", "mimocode.json")
		if got := defaultMimoCodeConfigPath(filepath.Join("home", "u")); got != want {
			t.Errorf("write dir must follow HOME > XDG; CONFIG/CONFIG_DIR are read-only layers: got %q, want %q", got, want)
		}
	})
	t.Run("write target is ALWAYS the fixed seed mimocode.json — NEVER floats to .jsonc", func(t *testing.T) {
		// bot PR #420 finding 5: the write target must NOT float to mimocode.jsonc
		// when that file exists. A floating target would make a later
		// backup/demigrate miss the layer the hub actually wrote (mimocode.json).
		// Even with an existing mimocode.jsonc, the write target stays mimocode.json.
		isolateMimoCodeEnv(t)
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		dir := filepath.Join(xdg, "mimocode")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mimocode.jsonc"), []byte("{\n  // comment\n  \"mcp\": {}\n}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(dir, "mimocode.json")
		if got != want {
			t.Errorf("write target must stay the fixed seed mimocode.json even when mimocode.jsonc exists: got %q, want %q", got, want)
		}
	})
	t.Run("no files present: write target is mimocode.json", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(xdg, "mimocode", "mimocode.json")
		if got != want {
			t.Errorf("on a fresh host the write target = mimocode.json: got %q, want %q", got, want)
		}
	})
	t.Run("config.json is never a WRITE target (only mimocode.json)", func(t *testing.T) {
		// Spec #1: config.json IS a real READ merge layer, but it is a legacy
		// migration sink that the hub never WRITES — the write target is the fixed
		// seed mimocode.json. A lone config.json must NOT become the write target.
		isolateMimoCodeEnv(t)
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		dir := filepath.Join(xdg, "mimocode")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got := defaultMimoCodeConfigPath(filepath.Join("home", "u"))
		want := filepath.Join(dir, "mimocode.json")
		if got != want {
			t.Errorf("config.json must not be the write target: got %q, want %q", got, want)
		}
	})
}

// TestMimoCode_ConfigJSONIsRealReadLayer pins spec #1: config.json is a real,
// LOWEST-precedence READ merge layer (config.ts `loadGlobal` merges config.json
// → mimocode.json → mimocode.jsonc). A server defined ONLY in config.json must
// be visible through the merged read; on a same-key conflict mimocode.json /
// mimocode.jsonc (higher layers) win.
func TestMimoCode_ConfigJSONIsRealReadLayer(t *testing.T) {
	t.Run("entry in config.json visible via merge", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"),
			[]byte(`{"mcp":{"from-config":{"type":"remote","url":"http://localhost:9009/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// Write target is mimocode.json (top of write), but the MERGE must surface
		// the config.json-layer entry. NOTE: probe the merged read directly (NOT
		// GetEntry) — GetEntry deliberately suppresses a config.json-ONLY entry to
		// keep the install/register rollback from copying it up into the write
		// target (bot PR #420 finding 1; see TestMimoCode_GetEntry_LowerLayerOnly...).
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		merged, err := o.readMergedLayers()
		if err != nil {
			t.Fatalf("readMergedLayers: %v", err)
		}
		servers, _ := merged[mimoCodeMCPKey].(map[string]any)
		entry, _ := servers["from-config"].(map[string]any)
		if entry == nil || entry["url"] != "http://localhost:9009/mcp" {
			t.Errorf("config.json-layer entry not visible through merged read: %+v", merged)
		}
	})
	t.Run("precedence config.json < mimocode.json < mimocode.jsonc", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		// Same key in all three layers; the .jsonc value must win.
		if err := os.WriteFile(filepath.Join(dir, "config.json"),
			[]byte(`{"mcp":{"srv":{"type":"remote","url":"http://config-json/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mimocode.json"),
			[]byte(`{"mcp":{"srv":{"type":"remote","url":"http://mimocode-json/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mimocode.jsonc"),
			[]byte("{\n  \"mcp\": {\"srv\": {\"type\":\"remote\",\"url\":\"http://mimocode-jsonc/mcp\",\"enabled\":true}}\n}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.jsonc")}
		e, err := o.GetEntry("srv")
		if err != nil {
			t.Fatalf("GetEntry: %v", err)
		}
		if e == nil || e.URL != "http://mimocode-jsonc/mcp" {
			t.Errorf("layer precedence wrong: want .jsonc to win, got %+v", e)
		}
	})
	t.Run("config.json-only entry survives a mimocode.json that lacks it", func(t *testing.T) {
		// The lowest layer's entry must NOT be shadowed away when a higher layer
		// merely carries an unrelated setting (deep-merge by key, not replace).
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"),
			[]byte(`{"mcp":{"low":{"type":"remote","url":"http://low/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mimocode.json"),
			[]byte(`{"theme":"dark"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		// Probe the MERGE directly — a config.json-only entry must survive an
		// unrelated higher layer (deep-merge by key). GetEntry suppresses a
		// config.json-only entry by design (finding 1), so it is not the probe here.
		merged, err := o.readMergedLayers()
		if err != nil {
			t.Fatalf("readMergedLayers: %v", err)
		}
		servers, _ := merged[mimoCodeMCPKey].(map[string]any)
		entry, _ := servers["low"].(map[string]any)
		if entry == nil || entry["url"] != "http://low/mcp" {
			t.Errorf("config.json entry shadowed by an unrelated higher layer: %+v", merged)
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

// TestMimoCode_RemoveEntry_TopLayerOnly_PreservesLowerOriginal pins the P1
// data-safety rule (spec #4 / VERDICT B1): RemoveEntry deletes ONLY from the
// top WRITE layer (o.path). A lower-layer (config.json / mimocode.json) entry of
// the same name is the OPERATOR's original — the hub physically cannot have
// written there (setMember only touches o.path) — so deleting it would destroy
// operator data the hub never owned. Removing only the top hub entry lets the
// merge REVEAL the operator's lower-layer original again, which is exactly the
// restore-to-prior-state the rollback/demigrate contract provides.
func TestMimoCode_RemoveEntry_TopLayerOnly_PreservesLowerOriginal(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	configJSON := filepath.Join(dir, "config.json")
	jsoncPath := filepath.Join(dir, "mimocode.jsonc")
	// The OPERATOR's own server lives in the LOWER config.json layer; the hub's
	// entry of the same name was written into the TOP mimocode.jsonc layer.
	if err := os.WriteFile(configJSON, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://operator-original/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsoncPath, []byte("{\n  \"mcp\": {\"serena\": {\"type\":\"remote\",\"url\":\"http://localhost:9121/mcp\",\"enabled\":true}}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: jsoncPath}
	if err := o.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	// The operator's lower-layer original MUST survive on disk untouched.
	lowData, _ := os.ReadFile(configJSON)
	lowM, err := parseJSONCBytes(lowData)
	if err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	lowServers, _ := lowM[mimoCodeMCPKey].(map[string]any)
	if _, present := lowServers["serena"]; !present {
		t.Fatal("DATA LOSS: operator's lower-layer config.json serena was deleted by RemoveEntry")
	}
	// The hub's top-layer entry MUST be gone from mimocode.jsonc.
	topData, _ := os.ReadFile(jsoncPath)
	topM, _ := parseJSONCBytes(topData)
	topServers, _ := topM[mimoCodeMCPKey].(map[string]any)
	if _, present := topServers["serena"]; present {
		t.Error("hub's top-layer serena entry should have been removed")
	}
	// And the merged read now surfaces the operator's original (restored view).
	// Probe readMergedLayers directly: after removal the entry lives ONLY in the
	// lower config.json layer, which GetEntry deliberately suppresses (finding 1)
	// so a rollback never copies it up; the MERGE still reveals it.
	merged, err := o.readMergedLayers()
	if err != nil {
		t.Fatalf("readMergedLayers: %v", err)
	}
	mergedServers, _ := merged[mimoCodeMCPKey].(map[string]any)
	mergedEntry, _ := mergedServers["serena"].(map[string]any)
	if mergedEntry == nil || mergedEntry["url"] != "http://operator-original/mcp" {
		t.Errorf("merged read should reveal the operator's lower-layer original after removal: %+v", merged)
	}
	// And GetEntry now reports it ABSENT (the rollback-safety guard): a same-named
	// server living ONLY below the write target must NOT be returned as a prior the
	// rollback would AddEntry(*prior) up into the write target.
	if e, _ := o.GetEntry("serena"); e != nil {
		t.Errorf("GetEntry must suppress a config.json-only entry (rollback safety), got %+v", e)
	}
}

// TestMimoCode_GetEntry_LowerLayerOnly_RollbackRemovesNotCopiesUp pins bot PR
// #420 finding 1: when the prior entry the install/register rollback snapshots
// lives ONLY in the config.json layer BELOW the write target, GetEntry must
// report it ABSENT (nil) so the rollback takes the nil-prior branch (RemoveEntry
// the hub's write-target key) — NOT AddEntry(*prior), which would copy the
// operator's lower-layer entry UP into the write target (mimocode.json) and
// SHADOW their config.json forever. This test drives the EXACT rollback contract
// from install.go:2573 / register.go:786 (snapshot prior → AddEntry hub entry →
// on failure: prior==nil ? RemoveEntry : AddEntry(*prior)).
func TestMimoCode_GetEntry_LowerLayerOnly_RollbackRemovesNotCopiesUp(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	configJSON := filepath.Join(dir, "config.json")
	writeTarget := filepath.Join(dir, "mimocode.json")
	// The operator's ONLY definition of "serena" lives in the LOWER config.json
	// layer. The write target does not (yet) carry it.
	if err := os.WriteFile(configJSON,
		[]byte(`{"mcp":{"serena":{"type":"remote","url":"http://operator-original/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: writeTarget}

	// 1. Rollback snapshots the prior BEFORE the hub write. A config.json-only
	//    prior must read as ABSENT so the rollback removes (not copies up).
	priorEntry, err := o.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if priorEntry != nil {
		t.Fatalf("config.json-only prior must read ABSENT for rollback safety, got %+v", priorEntry)
	}

	// 2. The hub installs its own entry into the write target (mimocode.json).
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://127.0.0.1:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	// 3. Downstream install failure → rollback. With priorEntry == nil the
	//    rollback closure (install.go:2607 / register.go:802) calls RemoveEntry.
	if priorEntry != nil {
		t.Fatal("unreachable: prior was nil")
	}
	if err := o.RemoveEntry("serena"); err != nil {
		t.Fatalf("rollback RemoveEntry: %v", err)
	}

	// 4a. The write target must NOT carry serena — the hub's key was removed and
	//     the lower-layer entry was never copied up.
	wtData, _ := os.ReadFile(writeTarget)
	wtM, _ := parseJSONCBytes(wtData)
	wtServers, _ := wtM[mimoCodeMCPKey].(map[string]any)
	if _, present := wtServers["serena"]; present {
		t.Errorf("SHADOW BUG: the operator's config.json serena was copied up into the write target by the rollback: %s", wtData)
	}

	// 4b. The operator's config.json original is byte-untouched.
	lowData, _ := os.ReadFile(configJSON)
	lowM, _ := parseJSONCBytes(lowData)
	lowServers, _ := lowM[mimoCodeMCPKey].(map[string]any)
	lowEntry, _ := lowServers["serena"].(map[string]any)
	if lowEntry == nil || lowEntry["url"] != "http://operator-original/mcp" {
		t.Errorf("operator's config.json original was mutated/lost: %s", lowData)
	}

	// 4c. The merged read once again surfaces the operator's original — config.json
	//     re-emerged, which is the restore-to-prior-state the rollback promises.
	merged, err := o.readMergedLayers()
	if err != nil {
		t.Fatalf("readMergedLayers: %v", err)
	}
	mServers, _ := merged[mimoCodeMCPKey].(map[string]any)
	mEntry, _ := mServers["serena"].(map[string]any)
	if mEntry == nil || mEntry["url"] != "http://operator-original/mcp" {
		t.Errorf("config.json original did not re-emerge via the merge after rollback: %+v", merged)
	}
}

// TestMimoCode_Exists_InlineOnlyProfile pins bot PR #420 finding 3: an
// INLINE-ONLY MIMOCODE_CONFIG_CONTENT profile (no config file on disk, no config
// dir) must report Exists()==true so every write path gating on client.Exists()
// (Apply / Register) proceeds — consistent with the scan promotion that already
// shows such a profile as present and discovers its servers. A MALFORMED inline
// string must NOT assert presence (it is surfaced as a loud merged-read parse
// error elsewhere, not a silent skip).
func TestMimoCode_Exists_InlineOnlyProfile(t *testing.T) {
	isolateMimoCodeEnv(t)
	// A directory that does NOT contain any mimo config files, and whose write
	// target dir we delete so the dir-stat fallback also misses — isolating the
	// inline-content branch as the ONLY presence signal.
	parent := t.TempDir()
	absentDir := filepath.Join(parent, "no-such-config-dir")
	writeTarget := filepath.Join(absentDir, "mimocode.json")

	t.Run("parseable inline content makes Exists true with no files and no dir", func(t *testing.T) {
		o := &mimoCodeClient{
			path:          writeTarget,
			inlineContent: `{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`,
		}
		// Sanity: no read-layer file and no config dir exist.
		for _, f := range o.readLayerFiles() {
			if _, err := os.Stat(f); err == nil {
				t.Fatalf("test setup leaked a real read-layer file %q", f)
			}
		}
		if _, err := os.Stat(filepath.Dir(o.path)); err == nil {
			t.Fatalf("test setup leaked a real config dir %q", filepath.Dir(o.path))
		}
		if !o.Exists() {
			t.Error("inline-only profile must report Exists()==true so Apply/Register can act on it")
		}
	})

	t.Run("malformed inline content does NOT assert presence", func(t *testing.T) {
		o := &mimoCodeClient{
			path:          writeTarget,
			inlineContent: `{"mcp": { not valid json`,
		}
		if o.Exists() {
			t.Error("malformed inline content must NOT assert Exists() (no silent presence on unparseable bytes)")
		}
	})

	t.Run("empty inline content with no files is not present", func(t *testing.T) {
		o := &mimoCodeClient{path: writeTarget, inlineContent: ""}
		if o.Exists() {
			t.Error("no inline content, no file, no dir → Exists() must be false")
		}
	})
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
	layers := mimoCodeReadLayerFiles(explicit, "", "")
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
	// Delete-side: RemoveEntry on an explicit path touches ONLY that file; the
	// sibling mimocode.json is never opened/mutated.
	siblingBefore, _ := os.ReadFile(filepath.Join(dir, "mimocode.json"))
	if err := o.RemoveEntry("chosen"); err != nil {
		t.Fatalf("RemoveEntry on explicit path: %v", err)
	}
	siblingAfter, _ := os.ReadFile(filepath.Join(dir, "mimocode.json"))
	if string(siblingBefore) != string(siblingAfter) {
		t.Errorf("explicit-path RemoveEntry mutated the sibling mimocode.json (state-safety breach)")
	}
}

// TestMimoCode_ConfigDirOverlay_MergesGlobalPlusCustom pins spec #3:
// MIMOCODE_CONFIG_DIR is an ADDITIONAL overlay read ON TOP of the global dir
// (NOT a replacement). Global-dir entries stay visible, the overlay's entries
// are merged in, and on a same-key conflict the overlay (higher precedence)
// wins. The construction mirrors NewMimoCode (write target in the global dir,
// overlayDir = the custom dir).
func TestMimoCode_ConfigDirOverlay_MergesGlobalPlusCustom(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	overlayDir := t.TempDir()
	// Global dir: an operator server only the global layer defines.
	if err := os.WriteFile(filepath.Join(globalDir, "mimocode.json"),
		[]byte(`{"mcp":{"global-only":{"type":"remote","url":"http://global/mcp","enabled":true},"shared":{"type":"remote","url":"http://global-shared/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Overlay dir: a custom-only server + a conflicting `shared` the overlay wins.
	if err := os.WriteFile(filepath.Join(overlayDir, "mimocode.json"),
		[]byte(`{"mcp":{"overlay-only":{"type":"remote","url":"http://overlay/mcp","enabled":true},"shared":{"type":"remote","url":"http://overlay-shared/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), overlayDir: overlayDir}

	// Global-only entry still visible (overlay did not REPLACE the global dir).
	if e, _ := o.GetEntry("global-only"); e == nil || e.URL != "http://global/mcp" {
		t.Errorf("global-dir entry must stay visible under an overlay: %+v", e)
	}
	// Overlay-only entry is merged in.
	if e, _ := o.GetEntry("overlay-only"); e == nil || e.URL != "http://overlay/mcp" {
		t.Errorf("overlay-dir entry not merged: %+v", e)
	}
	// On conflict the overlay (higher precedence) wins.
	if e, _ := o.GetEntry("shared"); e == nil || e.URL != "http://overlay-shared/mcp" {
		t.Errorf("on a same-key conflict the MIMOCODE_CONFIG_DIR overlay must win: %+v", e)
	}
}

// TestMimoCode_ConfigFile_MergesAboveGlobal pins bot PR #420 finding 1:
// MIMOCODE_CONFIG is a READ LAYER merged ABOVE the global dir layers, NOT a
// single-file replacement. A server defined only in the global mimocode.json
// stays visible; the MIMOCODE_CONFIG file's entries merge in and win a same-key
// conflict; and the WRITE target stays the global seed mimocode.json (the hub
// never writes the operator's MIMOCODE_CONFIG file). Mirrors NewMimoCode
// (configFile + write target in the global dir).
func TestMimoCode_ConfigFile_MergesAboveGlobal(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	// Global dir: a global-only server + a conflicting `shared` the config file wins.
	if err := os.WriteFile(globalPath,
		[]byte(`{"mcp":{"global-only":{"type":"remote","url":"http://global/mcp","enabled":true},"shared":{"type":"remote","url":"http://global-shared/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// MIMOCODE_CONFIG file: a config-only server + the conflicting `shared`.
	configFile := filepath.Join(t.TempDir(), "my-custom.json")
	if err := os.WriteFile(configFile,
		[]byte(`{"mcp":{"config-only":{"type":"remote","url":"http://config/mcp","enabled":true},"shared":{"type":"remote","url":"http://config-shared/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath, configFile: configFile}

	// The MIMOCODE_CONFIG file is a read layer; the read set carries BOTH the
	// global layers and the config file (config file appended above global).
	layers := o.readLayerFiles()
	if len(layers) == 0 || layers[len(layers)-1] != configFile {
		t.Fatalf("MIMOCODE_CONFIG file must be the top FILE read layer, got %v", layers)
	}
	// Global-only entry still visible (config file did not REPLACE the global dir).
	if e, _ := o.GetEntry("global-only"); e == nil || e.URL != "http://global/mcp" {
		t.Errorf("global-dir entry must stay visible under a MIMOCODE_CONFIG file: %+v", e)
	}
	// Config-file-only entry is merged in.
	if e, _ := o.GetEntry("config-only"); e == nil || e.URL != "http://config/mcp" {
		t.Errorf("MIMOCODE_CONFIG file entry not merged: %+v", e)
	}
	// On conflict the MIMOCODE_CONFIG file (higher precedence) wins.
	if e, _ := o.GetEntry("shared"); e == nil || e.URL != "http://config-shared/mcp" {
		t.Errorf("on a same-key conflict the MIMOCODE_CONFIG file must win: %+v", e)
	}

	// NewMimoCode-equivalent resolution: write target = the GLOBAL seed (NOT the
	// config file); the config file is recorded as a read layer.
	t.Setenv("MIMOCODE_CONFIG", configFile)
	t.Setenv("XDG_CONFIG_HOME", globalDir) // global dir = $XDG/mimocode below
	r := resolveMimoCodeConfig(filepath.Join("home", "u"))
	wantWrite := filepath.Join(globalDir, "mimocode", "mimocode.json")
	if r.writeTarget != wantWrite {
		t.Errorf("write target must be the global seed, not the MIMOCODE_CONFIG file: got %q, want %q", r.writeTarget, wantWrite)
	}
	if r.configFile != configFile {
		t.Errorf("MIMOCODE_CONFIG must be recorded as the configFile read layer: got %q, want %q", r.configFile, configFile)
	}
}

// TestMimoCode_FindStdioLanguageServerEntries_CommandArray pins spec #7: a
// MiMoCode LOCAL mcp-language-server entry stores `command` as an ARRAY
// (["mcp-language-server","--lsp","go"]). The LSP scan must normalize the array
// before delegating to the shared string-keyed matcher, so the entry is found.
// A string-command form (already covered elsewhere) still works.
func TestMimoCode_FindStdioLanguageServerEntries_CommandArray(t *testing.T) {
	o := newMimoCodeForTest(t, `{
  "mcp": {
    "serena": {"type": "remote", "url": "http://localhost:9121/mcp", "enabled": true},
    "go-ls": {"type": "local", "command": ["mcp-language-server", "--lsp", "go"], "enabled": true},
    "rust-ls": {"type": "local", "command": ["mcp-language-server"], "args": ["--lsp=rust"], "enabled": true}
  }
}`)
	ls, err := o.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	got := map[string]string{}
	for _, e := range ls {
		got[e.Name] = e.Language
	}
	if got["go-ls"] != "go" {
		t.Errorf("array-command LSP entry not found: command-array --lsp go missed, got %v", got)
	}
	// The two-array-element form (array command + separate args array, --lsp=rust)
	// must also resolve, proving array-args are prepended to existing args.
	if got["rust-ls"] != "rust" {
		t.Errorf("array-command + args-array LSP entry not found: got %v", got)
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

// TestMimoCode_ConfigFile_NamedLikeGlobalLayer pins the corrected bot PR #420
// finding 1 model: a MIMOCODE_CONFIG file whose basename is a known global layer
// name (mimocode.json/.jsonc/config.json) is STILL just a read layer ABOVE the
// global write target — it never becomes a single-file replacement. The global
// dir's own layers AND the MIMOCODE_CONFIG file are all in the read set, the
// config file merges on top (wins conflicts), and the WRITE target stays the
// global seed mimocode.json. (Pre-fix, such a file wrongly collapsed the whole
// read+write to the one file via the deleted fileOverride flag.)
func TestMimoCode_ConfigFile_NamedLikeGlobalLayer(t *testing.T) {
	for _, base := range []string{"mimocode.json", "mimocode.jsonc", "config.json"} {
		t.Run(base, func(t *testing.T) {
			isolateMimoCodeEnv(t)
			globalDir := t.TempDir()
			globalPath := filepath.Join(globalDir, "mimocode.json")
			// Global dir owns a server the merge must keep visible.
			if err := os.WriteFile(globalPath,
				[]byte(`{"mcp":{"global-srv":{"type":"remote","url":"http://global/mcp","enabled":true}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			// The MIMOCODE_CONFIG file lives in a SEPARATE dir and is named like a
			// global layer; it owns its own server.
			configFile := filepath.Join(t.TempDir(), base)
			if err := os.WriteFile(configFile,
				[]byte(`{"mcp":{"config-srv":{"type":"remote","url":"http://config/mcp","enabled":true}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			o := &mimoCodeClient{path: globalPath, configFile: configFile}

			// Read set carries the global layers AND the config file (config above).
			layers := o.readLayerFiles()
			if len(layers) < 2 || layers[len(layers)-1] != configFile {
				t.Fatalf("config file (basename=%s) must be the top FILE read layer above global, got %v", base, layers)
			}
			if e, _ := o.GetEntry("global-srv"); e == nil {
				t.Errorf("global-dir server must stay visible (basename=%s): %+v", base, e)
			}
			if e, _ := o.GetEntry("config-srv"); e == nil {
				t.Errorf("MIMOCODE_CONFIG file server must be merged in (basename=%s): %+v", base, e)
			}
		})
	}
}

// TestMimoCode_FindStdioLanguageServerEntries_SkipsDisabled pins bot PR #420
// finding 3: a disabled (enabled:false) mcp-language-server entry must NOT be
// reported by the LSP-cleanup scan — MiMoCode never spawns it, so
// `language-server cleanup` must not remove it. An active entry alongside it is
// still found (proving the filter is per-entry, not all-or-nothing).
func TestMimoCode_FindStdioLanguageServerEntries_SkipsDisabled(t *testing.T) {
	o := newMimoCodeForTest(t, `{
  "mcp": {
    "go-ls": {"type": "local", "command": ["mcp-language-server", "--lsp", "go"], "enabled": true},
    "rust-ls-off": {"type": "local", "command": ["mcp-language-server", "--lsp", "rust"], "enabled": false}
  }
}`)
	ls, err := o.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	names := map[string]bool{}
	for _, e := range ls {
		names[e.Name] = true
	}
	if !names["go-ls"] {
		t.Errorf("active LSP entry must still be found: got %v", names)
	}
	if names["rust-ls-off"] {
		t.Errorf("disabled (enabled:false) LSP entry must NOT be reported for cleanup: got %v", names)
	}
}

// TestMimoCode_DisabledURLEntry_RollbackStaysDisabled pins bot PR #420 finding
// 5: a pre-existing URL entry with enabled:false must round-trip through
// GetEntry → AddEntry(*prior) WITHOUT being re-enabled. GetEntry carries the
// verbatim entry in Raw (URL left empty) so the rollback writes it back
// byte-shaped (enabled:false preserved), instead of the normal install path
// hardcoding enabled:true.
func TestMimoCode_DisabledURLEntry_RollbackStaysDisabled(t *testing.T) {
	o := newMimoCodeForTest(t, `{"mcp":{"disabled-remote":{"type":"remote","url":"https://api.example.com/mcp","enabled":false}}}`)

	prior, err := o.GetEntry("disabled-remote")
	if err != nil || prior == nil {
		t.Fatalf("GetEntry: prior=%+v err=%v", prior, err)
	}
	// A disabled URL entry must be carried via Raw (URL left empty) so AddEntry
	// writes it verbatim rather than re-projecting it to enabled:true.
	if prior.Raw == nil {
		t.Fatalf("disabled URL entry must carry Raw for verbatim rollback, got %+v", prior)
	}
	// Simulate the install-rollback restore.
	if err := o.AddEntry(*prior); err != nil {
		t.Fatalf("AddEntry(*prior): %v", err)
	}
	raw, _ := os.ReadFile(o.path)
	m, err := parseJSONCBytes(raw)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	ent, _ := servers["disabled-remote"].(map[string]any)
	if ent == nil {
		t.Fatalf("entry missing after rollback: %s", raw)
	}
	if enabled, _ := ent["enabled"].(bool); enabled {
		t.Errorf("rollback RE-ENABLED a disabled URL entry: enabled=%v, raw=%s", ent["enabled"], raw)
	}
	if got, _ := ent["url"].(string); got != "https://api.example.com/mcp" {
		t.Errorf("url not preserved through rollback: got %q", got)
	}
	// An ENABLED URL entry still takes the lean {URL} path (Raw nil) — polarity unchanged.
	o2 := newMimoCodeForTest(t, `{"mcp":{"on-remote":{"type":"remote","url":"https://api.example.com/mcp","enabled":true}}}`)
	if e, _ := o2.GetEntry("on-remote"); e == nil || e.Raw != nil || e.URL != "https://api.example.com/mcp" {
		t.Errorf("enabled URL entry must stay on the lean {URL} path with Raw nil: %+v", e)
	}
}

// TestMimoCode_OverlayShadow_AddEntryRefuses pins bot PR #420 finding 7: when the
// MIMOCODE_CONFIG_DIR overlay already defines mcp.<server>, the hub write to the
// lower-precedence global target would be shadowed (overlay wins the merge). The
// adapter REFUSES the write with ErrMimoCodeOverlayShadowsServer rather than
// silently reporting success — and touches neither the global target nor the
// operator-owned overlay file.
func TestMimoCode_OverlayShadow_AddEntryRefuses(t *testing.T) {
	isolateMimoCodeEnv(t)
	globalDir := t.TempDir()
	overlayDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")
	if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The overlay (in its top .jsonc layer) already defines `serena`.
	overlayFile := filepath.Join(overlayDir, "mimocode.jsonc")
	overlayBytes := []byte(`{"mcp":{"serena":{"type":"remote","url":"http://overlay-serena/mcp","enabled":true}}}`)
	if err := os.WriteFile(overlayFile, overlayBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: globalPath, overlayDir: overlayDir}

	globalBefore, _ := os.ReadFile(globalPath)
	err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
	var shadowErr *ErrMimoCodeOverlayShadowsServer
	if !errors.As(err, &shadowErr) {
		t.Fatalf("AddEntry over a shadowing overlay must return ErrMimoCodeOverlayShadowsServer, got %v", err)
	}
	if shadowErr.OverlayFile != overlayFile {
		t.Errorf("error must name the shadowing overlay file: got %q, want %q", shadowErr.OverlayFile, overlayFile)
	}
	// Neither the global target nor the operator-owned overlay was written.
	if after, _ := os.ReadFile(globalPath); string(after) != string(globalBefore) {
		t.Errorf("refused AddEntry must NOT write the global target: before=%s after=%s", globalBefore, after)
	}
	if after, _ := os.ReadFile(overlayFile); string(after) != string(overlayBytes) {
		t.Errorf("refused AddEntry must NOT touch the operator-owned overlay file")
	}
}

// TestMimoCode_OverlayNonShadow_AddEntryWrites confirms the finding-7 guard does
// NOT false-refuse: when the overlay defines a DIFFERENT server (or is absent),
// AddEntry writes the global target normally. Also pins the common no-overlay
// path (overlayDir == "") as byte-coherent (writes succeed).
func TestMimoCode_OverlayNonShadow_AddEntryWrites(t *testing.T) {
	t.Run("overlay defines a different server", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		globalDir := t.TempDir()
		overlayDir := t.TempDir()
		globalPath := filepath.Join(globalDir, "mimocode.json")
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(overlayDir, "mimocode.json"),
			[]byte(`{"mcp":{"other":{"type":"remote","url":"http://other/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: globalPath, overlayDir: overlayDir}
		if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
			t.Fatalf("AddEntry must succeed when the overlay does not shadow the server: %v", err)
		}
		if e, _ := o.GetEntry("serena"); e == nil || e.URL != "http://localhost:9121/mcp" {
			t.Errorf("serena entry not written to the global target: %+v", e)
		}
	})
	t.Run("no overlay (common path) writes normally", func(t *testing.T) {
		o := newMimoCodeForTest(t, `{"mcp":{}}`)
		if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
			t.Fatalf("AddEntry on the common no-overlay path: %v", err)
		}
		if e, _ := o.GetEntry("serena"); e == nil || e.URL != "http://localhost:9121/mcp" {
			t.Errorf("serena entry not written on the no-overlay path: %+v", e)
		}
	})
}

// TestMimoCode_InlineContent_MergesAndShadows pins bot PR #420 finding 4:
// MIMOCODE_CONFIG_CONTENT is an INLINE JSONC config string merged as the TOP
// read layer. Its servers are visible via the merged read and win same-key
// conflicts; and a same-name server in the inline content SHADOWS a hub write to
// the global target, so AddEntry refuses with the distinct inline-shadow error.
func TestMimoCode_InlineContent_MergesAndShadows(t *testing.T) {
	t.Run("inline content is read as the top layer", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		globalDir := t.TempDir()
		globalPath := filepath.Join(globalDir, "mimocode.json")
		if err := os.WriteFile(globalPath,
			[]byte(`{"mcp":{"global-srv":{"type":"remote","url":"http://global/mcp","enabled":true},"shared":{"type":"remote","url":"http://global-shared/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// Inline (JSONC: comment + trailing comma) adds a server and wins `shared`.
		o := &mimoCodeClient{path: globalPath, inlineContent: "{\n  // inline overlay\n  \"mcp\": {\n    \"inline-srv\": {\"type\":\"remote\",\"url\":\"http://inline/mcp\",\"enabled\":true},\n    \"shared\": {\"type\":\"remote\",\"url\":\"http://inline-shared/mcp\",\"enabled\":true},\n  },\n}"}
		if e, _ := o.GetEntry("global-srv"); e == nil || e.URL != "http://global/mcp" {
			t.Errorf("global server must stay visible under inline content: %+v", e)
		}
		if e, _ := o.GetEntry("inline-srv"); e == nil || e.URL != "http://inline/mcp" {
			t.Errorf("MIMOCODE_CONFIG_CONTENT server not merged: %+v", e)
		}
		if e, _ := o.GetEntry("shared"); e == nil || e.URL != "http://inline-shared/mcp" {
			t.Errorf("inline content must win a same-key conflict (top layer): %+v", e)
		}
	})

	t.Run("inline content shadowing the hub server refuses AddEntry with the inline error", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		globalDir := t.TempDir()
		globalPath := filepath.Join(globalDir, "mimocode.json")
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: globalPath, inlineContent: `{"mcp":{"serena":{"type":"remote","url":"http://inline-serena/mcp","enabled":true}}}`}
		globalBefore, _ := os.ReadFile(globalPath)
		err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
		var inlineErr *ErrMimoCodeInlineContentShadowsServer
		if !errors.As(err, &inlineErr) {
			t.Fatalf("inline-shadow AddEntry must return ErrMimoCodeInlineContentShadowsServer, got %v", err)
		}
		// Must NOT be misclassified as a file-shadow error (no file to name).
		var fileErr *ErrMimoCodeOverlayShadowsServer
		if errors.As(err, &fileErr) {
			t.Errorf("inline shadow must NOT be the file-shadow error type: %v", err)
		}
		if after, _ := os.ReadFile(globalPath); string(after) != string(globalBefore) {
			t.Errorf("refused AddEntry must NOT write the global target")
		}
	})

	t.Run("inline content scanned via MimoCodeMergedConfig (known global layer path)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		globalDir := t.TempDir()
		globalPath := filepath.Join(globalDir, "mimocode.json")
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{"mcp":{"inline-srv":{"type":"remote","url":"http://inline/mcp","enabled":true}}}`)
		merged, err := MimoCodeMergedConfig(globalPath)
		if err != nil {
			t.Fatalf("MimoCodeMergedConfig: %v", err)
		}
		servers, _ := merged["mcp"].(map[string]any)
		if _, ok := servers["inline-srv"]; !ok {
			t.Errorf("scan must see MIMOCODE_CONFIG_CONTENT servers via the merged read: %v", servers)
		}
	})
}

// TestMimoCode_HigherLayerShadow_Sources pins bot PR #420 findings 4+7
// (generalized): the AddEntry shadow guard refuses a hub install when ANY read
// layer above the global write target defines mcp.<name>, and the typed error
// names the correct highest-precedence source.
func TestMimoCode_HigherLayerShadow_Sources(t *testing.T) {
	t.Run("global mimocode.jsonc shadows the mimocode.json write target", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "mimocode.json")
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		jsoncPath := filepath.Join(dir, "mimocode.jsonc")
		if err := os.WriteFile(jsoncPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://jsonc-serena/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: globalPath}
		err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
		var shadowErr *ErrMimoCodeOverlayShadowsServer
		if !errors.As(err, &shadowErr) {
			t.Fatalf("global .jsonc shadow must refuse with the file-shadow error, got %v", err)
		}
		if shadowErr.SourceFile != jsoncPath {
			t.Errorf("error must name the global mimocode.jsonc: got %q want %q", shadowErr.SourceFile, jsoncPath)
		}
		if !strings.Contains(shadowErr.SourceLabel, "global") {
			t.Errorf("source label must describe the global higher layer: %q", shadowErr.SourceLabel)
		}
	})

	t.Run("MIMOCODE_CONFIG file shadows the write target", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "mimocode.json")
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		configFile := filepath.Join(t.TempDir(), "custom.json")
		if err := os.WriteFile(configFile, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://config-serena/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: globalPath, configFile: configFile}
		err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
		var shadowErr *ErrMimoCodeOverlayShadowsServer
		if !errors.As(err, &shadowErr) {
			t.Fatalf("MIMOCODE_CONFIG file shadow must refuse, got %v", err)
		}
		if shadowErr.SourceFile != configFile {
			t.Errorf("error must name the MIMOCODE_CONFIG file: got %q want %q", shadowErr.SourceFile, configFile)
		}
		if !strings.Contains(shadowErr.SourceLabel, "MIMOCODE_CONFIG file") {
			t.Errorf("source label must name the MIMOCODE_CONFIG file: %q", shadowErr.SourceLabel)
		}
	})

	t.Run("highest-precedence shadow wins: inline over overlay over config over global jsonc", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		overlayDir := t.TempDir()
		globalPath := filepath.Join(dir, "mimocode.json")
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// ALL higher layers define serena; the inline (top) must be the named source.
		if err := os.WriteFile(filepath.Join(dir, "mimocode.jsonc"), []byte(`{"mcp":{"serena":{"type":"remote","url":"http://g/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		configFile := filepath.Join(t.TempDir(), "custom.json")
		if err := os.WriteFile(configFile, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://c/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(overlayDir, "mimocode.jsonc"), []byte(`{"mcp":{"serena":{"type":"remote","url":"http://o/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{
			path:          globalPath,
			configFile:    configFile,
			overlayDir:    overlayDir,
			inlineContent: `{"mcp":{"serena":{"type":"remote","url":"http://i/mcp","enabled":true}}}`,
		}
		err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
		var inlineErr *ErrMimoCodeInlineContentShadowsServer
		if !errors.As(err, &inlineErr) {
			t.Fatalf("the highest-precedence (inline) shadow must be reported, got %v", err)
		}
	})
}

// TestMimoCode_RemoteExtraFields_RollbackRoundTrip pins bot PR #420 finding 3: a
// user-authored REMOTE entry with fields beyond the hub shape (oauth, timeout)
// must round-trip GetEntry -> AddEntry(*prior) byte-shaped — GetEntry carries
// Raw so AddEntry restores the verbatim entry (extra fields preserved). A clean
// hub-shaped remote entry stays on the lean {URL,Headers} path (Raw nil).
func TestMimoCode_RemoteExtraFields_RollbackRoundTrip(t *testing.T) {
	o := newMimoCodeForTest(t, `{"mcp":{"rich-remote":{"type":"remote","url":"https://api.example.com/mcp","enabled":true,"headers":{"X-Api-Key":"k"},"oauth":{"client_id":"abc"},"timeout":30000}}}`)

	prior, err := o.GetEntry("rich-remote")
	if err != nil || prior == nil {
		t.Fatalf("GetEntry: prior=%+v err=%v", prior, err)
	}
	if prior.Raw == nil {
		t.Fatalf("remote entry with extra fields must carry Raw for verbatim rollback, got %+v", prior)
	}
	if err := o.AddEntry(*prior); err != nil {
		t.Fatalf("AddEntry(*prior): %v", err)
	}
	raw, _ := os.ReadFile(o.path)
	m, err := parseJSONCBytes(raw)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	ent, _ := servers["rich-remote"].(map[string]any)
	if ent == nil {
		t.Fatalf("entry missing after rollback: %s", raw)
	}
	if _, ok := ent["oauth"]; !ok {
		t.Errorf("rollback dropped the user-authored oauth field: raw=%s", raw)
	}
	if _, ok := ent["timeout"]; !ok {
		t.Errorf("rollback dropped the user-authored timeout field: raw=%s", raw)
	}
	if got, _ := ent["url"].(string); got != "https://api.example.com/mcp" {
		t.Errorf("url not preserved through rollback: got %q", got)
	}
	// A CLEAN hub-shaped enabled remote entry stays on the lean {URL} path (Raw nil).
	o2 := newMimoCodeForTest(t, `{"mcp":{"clean-remote":{"type":"remote","url":"https://api.example.com/mcp","enabled":true,"headers":{"X-Api-Key":"k"}}}}`)
	if e, _ := o2.GetEntry("clean-remote"); e == nil || e.Raw != nil || e.URL != "https://api.example.com/mcp" {
		t.Errorf("clean hub-shaped remote entry must stay on the lean {URL} path with Raw nil: %+v", e)
	}
}

// TestMimoCode_ConfigEqualsWriteTarget_NotAShadow pins bot PR #420 (finding 2):
// when MIMOCODE_CONFIG points at the SAME file as the global write target
// (o.path), it is the SAME file the write lands in, not a higher layer — editing
// o.path is exactly what takes effect, so re-installing/updating a server already
// present in that file must SUCCEED, not be refused as a shadow. A genuine
// MIMOCODE_CONFIG at a DIFFERENT path still shadows.
func TestMimoCode_ConfigEqualsWriteTarget_NotAShadow(t *testing.T) {
	t.Run("MIMOCODE_CONFIG == write target: re-install of an existing entry succeeds", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "mimocode.json")
		// The write target ALREADY contains `serena` (an updated/re-installed entry).
		if err := os.WriteFile(globalPath,
			[]byte(`{"mcp":{"serena":{"type":"remote","url":"http://old/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// MIMOCODE_CONFIG resolves to the SAME file as the write target.
		o := &mimoCodeClient{path: globalPath, configFile: globalPath}
		if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://new/mcp"}); err != nil {
			t.Fatalf("re-install when MIMOCODE_CONFIG == write target must succeed, got shadow refusal: %v", err)
		}
		if e, _ := o.GetEntry("serena"); e == nil || e.URL != "http://new/mcp" {
			t.Errorf("the entry must be updated in place on the write target: %+v", e)
		}
	})

	t.Run("MIMOCODE_CONFIG == write target via a non-clean spelling is still not a shadow", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "mimocode.json")
		if err := os.WriteFile(globalPath,
			[]byte(`{"mcp":{"serena":{"type":"remote","url":"http://old/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// Same file, spelled with a redundant ./x/.. segment — filepath.Clean must
		// collapse it so the equality holds and the write is not refused.
		spelled := filepath.Join(dir, "x", "..", "mimocode.json")
		o := &mimoCodeClient{path: globalPath, configFile: spelled}
		if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://new/mcp"}); err != nil {
			t.Fatalf("a non-clean spelling of the write target must not be a shadow, got: %v", err)
		}
	})

	t.Run("MIMOCODE_CONFIG at a DIFFERENT path still shadows", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "mimocode.json")
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		configFile := filepath.Join(t.TempDir(), "custom.json")
		if err := os.WriteFile(configFile,
			[]byte(`{"mcp":{"serena":{"type":"remote","url":"http://config/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: globalPath, configFile: configFile}
		err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
		var shadowErr *ErrMimoCodeOverlayShadowsServer
		if !errors.As(err, &shadowErr) {
			t.Fatalf("a different-path MIMOCODE_CONFIG must still shadow, got %v", err)
		}
		if shadowErr.SourceFile != configFile {
			t.Errorf("error must name the different-path MIMOCODE_CONFIG file: got %q want %q", shadowErr.SourceFile, configFile)
		}
	})
}

// TestMimoCode_RelativeConfigEnv_ResolvedFromCwd pins bot PR #420 (finding 3):
// MiMoCode resolves a RELATIVE MIMOCODE_CONFIG / MIMOCODE_CONFIG_DIR from the
// process cwd, so the hub must too (absoluteEnv used to silently DROP relative
// values, ignoring an active overlay). MIMOCODE_HOME stays absolute-only.
// State-safe: t.Chdir into a temp dir, all paths under t.TempDir.
func TestMimoCode_RelativeConfigEnv_ResolvedFromCwd(t *testing.T) {
	t.Run("relative MIMOCODE_CONFIG resolves from cwd (overlay active)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		cwd := t.TempDir()
		t.Chdir(cwd)
		// A relative MIMOCODE_CONFIG file sitting in the cwd.
		if err := os.WriteFile(filepath.Join(cwd, "custom.json"),
			[]byte(`{"mcp":{"cfg-srv":{"type":"remote","url":"http://cfg/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MIMOCODE_CONFIG", "custom.json") // RELATIVE

		got := cwdResolvedEnv("MIMOCODE_CONFIG")
		want := filepath.Join(cwd, "custom.json")
		if got != want {
			t.Fatalf("relative MIMOCODE_CONFIG must resolve from cwd: got %q want %q", got, want)
		}
		// And the resolved layer is actually read: build the scan client for a
		// global-layer path and confirm the overlay server is merged in.
		globalDir := t.TempDir()
		globalPath := filepath.Join(globalDir, "mimocode.json")
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		merged, err := MimoCodeMergedConfig(globalPath)
		if err != nil {
			t.Fatalf("MimoCodeMergedConfig: %v", err)
		}
		servers, _ := merged["mcp"].(map[string]any)
		if _, ok := servers["cfg-srv"]; !ok {
			t.Errorf("a relative MIMOCODE_CONFIG overlay must be active (server merged): %v", servers)
		}
	})

	t.Run("relative MIMOCODE_CONFIG_DIR resolves from cwd (overlay active)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		cwd := t.TempDir()
		t.Chdir(cwd)
		// A relative overlay DIR under the cwd, with its own mimocode.json layer.
		if err := os.Mkdir(filepath.Join(cwd, ".mimocode"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cwd, ".mimocode", "mimocode.json"),
			[]byte(`{"mcp":{"ovl-srv":{"type":"remote","url":"http://ovl/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MIMOCODE_CONFIG_DIR", ".mimocode") // RELATIVE

		got := cwdResolvedEnv("MIMOCODE_CONFIG_DIR")
		want := filepath.Join(cwd, ".mimocode")
		if got != want {
			t.Fatalf("relative MIMOCODE_CONFIG_DIR must resolve from cwd: got %q want %q", got, want)
		}
		globalDir := t.TempDir()
		globalPath := filepath.Join(globalDir, "mimocode.json")
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		merged, err := MimoCodeMergedConfig(globalPath)
		if err != nil {
			t.Fatalf("MimoCodeMergedConfig: %v", err)
		}
		servers, _ := merged["mcp"].(map[string]any)
		if _, ok := servers["ovl-srv"]; !ok {
			t.Errorf("a relative MIMOCODE_CONFIG_DIR overlay must be active (server merged): %v", servers)
		}
	})

	t.Run("absolute MIMOCODE_CONFIG is returned cleaned, unchanged", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		abs := filepath.Join(t.TempDir(), "x", "..", "custom.json")
		t.Setenv("MIMOCODE_CONFIG", abs)
		if got, want := cwdResolvedEnv("MIMOCODE_CONFIG"), filepath.Clean(abs); got != want {
			t.Errorf("absolute MIMOCODE_CONFIG must be returned cleaned: got %q want %q", got, want)
		}
	})

	t.Run("MIMOCODE_HOME stays absolute-only (relative ignored)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Chdir(t.TempDir())
		t.Setenv("MIMOCODE_HOME", "rel-home") // RELATIVE — must be ignored
		if got := absoluteEnv("MIMOCODE_HOME"); got != "" {
			t.Errorf("relative MIMOCODE_HOME must be ignored (absolute-only): got %q", got)
		}
	})
}

// TestMimoCode_WriteTargetStable_DemigrateHitsActualLayer pins bot PR #420
// finding 5: an install that wrote the hub entry to mimocode.json (the only file
// at install time), followed by the operator LATER creating mimocode.jsonc, must
// still Backup() and RemoveEntry() against mimocode.json — the file that holds
// the managed entry — NOT float to mimocode.jsonc and miss it. The write target
// is fixed at the global seed, so this holds by construction.
func TestMimoCode_WriteTargetStable_DemigrateHitsActualLayer(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "mimocode.json")
	// Install-time: only mimocode.json exists; the hub writes the entry there.
	o := &mimoCodeClient{path: jsonPath}
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("install AddEntry: %v", err)
	}
	// Operator LATER hand-creates mimocode.jsonc (a higher read layer).
	jsoncPath := filepath.Join(dir, "mimocode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte("{\n  \"mcp\": {}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jsoncBefore, _ := os.ReadFile(jsoncPath)

	// A fresh write target resolved AFTER .jsonc exists must STILL be mimocode.json
	// (fixed seed — does not float).
	o2 := &mimoCodeClient{path: mimoCodeWriteTargetInDir(dir)}
	if o2.ConfigPath() != jsonPath {
		t.Fatalf("write target floated to .jsonc after it was created: got %q, want %q", o2.ConfigPath(), jsonPath)
	}

	// Backup() must back up mimocode.json (the entry-holding file).
	bak, err := o2.Backup()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(bak), "mimocode.json") {
		t.Errorf("backup must be of mimocode.json (the entry-holding layer), got %q", bak)
	}
	// LatestBackupPath must find that mimocode.json backup.
	latest, ok, err := o2.LatestBackupPath()
	if err != nil || !ok {
		t.Fatalf("LatestBackupPath: latest=%q ok=%v err=%v", latest, ok, err)
	}
	if !strings.HasPrefix(filepath.Base(latest), "mimocode.json") {
		t.Errorf("LatestBackupPath must point at a mimocode.json backup, got %q", latest)
	}
	// RemoveEntry (demigrate) must delete from mimocode.json.
	if err := o2.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	jsonAfter, _ := os.ReadFile(jsonPath)
	mj, _ := parseJSONCBytes(jsonAfter)
	if servers, _ := mj[mimoCodeMCPKey].(map[string]any); servers != nil {
		if _, present := servers["serena"]; present {
			t.Errorf("RemoveEntry did not remove serena from mimocode.json: %s", jsonAfter)
		}
	}
	// The operator's mimocode.jsonc must be UNTOUCHED throughout.
	if after, _ := os.ReadFile(jsoncPath); string(after) != string(jsoncBefore) {
		t.Errorf("backup/remove must NOT touch the operator's mimocode.jsonc")
	}
}

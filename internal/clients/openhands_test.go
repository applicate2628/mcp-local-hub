package clients

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func setupOpenHandsConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// parseOpenHandsTOML re-reads + parses the live config and returns the [mcp]
// section so tests can assert on the array shapes directly (more robust than
// substring matching against go-toml's chosen quoting).
func parseOpenHandsTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	sec, _ := m["mcp"].(map[string]any)
	if sec == nil {
		sec = map[string]any{}
	}
	return sec
}

func shttpByName(t *testing.T, sec map[string]any, name string) map[string]any {
	t.Helper()
	arr, _ := sec["shttp_servers"].([]any)
	for _, member := range arr {
		tbl, ok := member.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := tbl["name"].(string); n == name {
			return tbl
		}
	}
	return nil
}

func stdioByName(sec map[string]any, name string) map[string]any {
	arr, _ := sec["stdio_servers"].([]any)
	for _, member := range arr {
		tbl, ok := member.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := tbl["name"].(string); n == name {
			return tbl
		}
	}
	return nil
}

func TestOpenHands_AddEntry_WritesShttpInlineTable(t *testing.T) {
	initial := `[mcp]
shttp_servers = ["https://remote.example.com/mcp/shttp"]
stdio_servers = [
    {name = "fetch", command = "uvx", args = ["mcp-server-fetch"]},
]
`
	path := setupOpenHandsConfig(t, initial)
	o := &openHands{path: path}

	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://127.0.0.1:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	sec := parseOpenHandsTOML(t, path)

	got := shttpByName(t, sec, "serena")
	if got == nil {
		t.Fatalf("serena shttp entry not written; section: %#v", sec)
	}
	if url, _ := got["url"].(string); url != "http://127.0.0.1:9121/mcp" {
		t.Errorf("url = %q, want loopback hub url", url)
	}

	// The user's bare-string shttp entry must survive untouched.
	arr, _ := sec["shttp_servers"].([]any)
	var sawRemote bool
	for _, m := range arr {
		if s, ok := m.(string); ok && s == "https://remote.example.com/mcp/shttp" {
			sawRemote = true
		}
	}
	if !sawRemote {
		t.Errorf("user's bare-string shttp entry dropped; got %#v", arr)
	}

	// The unrelated stdio entry must survive.
	if stdioByName(sec, "fetch") == nil {
		t.Errorf("unrelated stdio 'fetch' entry dropped; section: %#v", sec)
	}
}

func TestOpenHands_AddEntry_ReplacesSameNameAndDropsStdio(t *testing.T) {
	// `serena` exists BOTH as a stdio entry (pre-hub) and we add it as shttp.
	// AddEntry must drop the stdio form (wholesale migrate to HTTP) and not
	// duplicate the shttp form.
	initial := `[mcp]
stdio_servers = [
    {name = "serena", command = "uvx", args = ["--from", "serena", "start"]},
    {name = "keep", command = "echo", args = ["hi"]},
]
shttp_servers = [
    {name = "serena", url = "http://127.0.0.1:9000/mcp"},
]
`
	path := setupOpenHandsConfig(t, initial)
	o := &openHands{path: path}

	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://127.0.0.1:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	sec := parseOpenHandsTOML(t, path)

	// Exactly one shttp serena entry, pointing at the NEW url.
	shttpArr, _ := sec["shttp_servers"].([]any)
	count := 0
	for _, m := range shttpArr {
		if tbl, ok := m.(map[string]any); ok {
			if n, _ := tbl["name"].(string); n == "serena" {
				count++
				if url, _ := tbl["url"].(string); url != "http://127.0.0.1:9121/mcp" {
					t.Errorf("serena url = %q, want updated hub url", url)
				}
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 serena shttp entry, got %d (%#v)", count, shttpArr)
	}

	// The stdio serena entry must be GONE (migrated to HTTP).
	if stdioByName(sec, "serena") != nil {
		t.Errorf("stdio serena entry should be dropped after migrate to HTTP; section: %#v", sec)
	}
	// The unrelated stdio entry survives.
	if stdioByName(sec, "keep") == nil {
		t.Errorf("unrelated stdio 'keep' dropped; section: %#v", sec)
	}
}

func TestOpenHands_AddEntry_FreshFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	o := &openHands{path: path}

	if err := o.AddEntry(MCPEntry{Name: "memory", URL: "http://127.0.0.1:9140/mcp"}); err != nil {
		t.Fatalf("AddEntry on fresh file: %v", err)
	}
	sec := parseOpenHandsTOML(t, path)
	got := shttpByName(t, sec, "memory")
	if got == nil {
		t.Fatalf("memory entry not written to fresh file; section: %#v", sec)
	}
	if url, _ := got["url"].(string); url != "http://127.0.0.1:9140/mcp" {
		t.Errorf("url = %q", url)
	}
}

func TestOpenHands_GetEntry(t *testing.T) {
	initial := `[mcp]
shttp_servers = [
    "https://remote.example.com/mcp/shttp",
    {name = "serena", url = "http://127.0.0.1:9121/mcp"},
]
`
	path := setupOpenHandsConfig(t, initial)
	o := &openHands{path: path}

	got, err := o.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil {
		t.Fatal("GetEntry returned nil for present entry")
	}
	if got.Name != "serena" || got.URL != "http://127.0.0.1:9121/mcp" {
		t.Errorf("GetEntry = %+v", got)
	}

	// Absent entry.
	missing, err := o.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry(absent): %v", err)
	}
	if missing != nil {
		t.Errorf("expected nil for absent entry, got %+v", missing)
	}
}

func TestOpenHands_RemoveEntry(t *testing.T) {
	initial := `[mcp]
shttp_servers = [
    "https://remote.example.com/mcp/shttp",
    {name = "serena", url = "http://127.0.0.1:9121/mcp"},
    {name = "memory", url = "http://127.0.0.1:9140/mcp"},
]
`
	path := setupOpenHandsConfig(t, initial)
	o := &openHands{path: path}

	if err := o.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	sec := parseOpenHandsTOML(t, path)

	if shttpByName(t, sec, "serena") != nil {
		t.Errorf("serena not removed; section: %#v", sec)
	}
	if shttpByName(t, sec, "memory") == nil {
		t.Errorf("memory wrongly removed; section: %#v", sec)
	}
	// Bare-string remote entry survives.
	arr, _ := sec["shttp_servers"].([]any)
	var sawRemote bool
	for _, m := range arr {
		if s, ok := m.(string); ok && s == "https://remote.example.com/mcp/shttp" {
			sawRemote = true
		}
	}
	if !sawRemote {
		t.Errorf("user's bare-string shttp entry dropped on RemoveEntry; got %#v", arr)
	}
}

func TestOpenHands_RemoveEntry_AlsoDropsStdio(t *testing.T) {
	initial := `[mcp]
stdio_servers = [
    {name = "serena", command = "uvx", args = ["start"]},
    {name = "keep", command = "echo"},
]
`
	path := setupOpenHandsConfig(t, initial)
	o := &openHands{path: path}

	if err := o.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	sec := parseOpenHandsTOML(t, path)
	if stdioByName(sec, "serena") != nil {
		t.Errorf("stdio serena not removed; section: %#v", sec)
	}
	if stdioByName(sec, "keep") == nil {
		t.Errorf("stdio keep wrongly removed; section: %#v", sec)
	}
}

func TestOpenHands_RemoveEntry_IdempotentAbsent(t *testing.T) {
	initial := `[mcp]
shttp_servers = [
    {name = "memory", url = "http://127.0.0.1:9140/mcp"},
]
`
	path := setupOpenHandsConfig(t, initial)
	o := &openHands{path: path}

	if err := o.RemoveEntry("ghost"); err != nil {
		t.Fatalf("RemoveEntry(absent): %v", err)
	}
	sec := parseOpenHandsTOML(t, path)
	if shttpByName(t, sec, "memory") == nil {
		t.Errorf("memory entry wrongly removed by no-op RemoveEntry; section: %#v", sec)
	}
}

func TestOpenHands_RemoveEntry_LastEntryDeletesArrayKey(t *testing.T) {
	initial := `[mcp]
shttp_servers = [
    {name = "memory", url = "http://127.0.0.1:9140/mcp"},
]
`
	path := setupOpenHandsConfig(t, initial)
	o := &openHands{path: path}

	if err := o.RemoveEntry("memory"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	data, _ := os.ReadFile(path)
	// No empty `shttp_servers = []` leftover.
	if strings.Contains(string(data), "shttp_servers") {
		t.Errorf("expected shttp_servers key to be deleted when empty, got:\n%s", data)
	}
}

func TestOpenHands_Name_IsRelayStdio(t *testing.T) {
	o := &openHands{path: "x"}
	if o.Name() != "openhands" {
		t.Errorf("Name() = %q, want openhands", o.Name())
	}
	if o.IsRelayStdio() {
		t.Errorf("IsRelayStdio() = true, want false (shttp is HTTP-native)")
	}
}

func TestOpenHands_InitEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	o := &openHands{path: path}

	created, err := o.InitEmpty()
	if err != nil {
		t.Fatalf("InitEmpty: %v", err)
	}
	if !created {
		t.Error("InitEmpty created=false on absent file")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "[mcp]") {
		t.Errorf("stub missing [mcp] header: %q", data)
	}

	// Second call is a no-op.
	created2, err := o.InitEmpty()
	if err != nil {
		t.Fatalf("InitEmpty (2nd): %v", err)
	}
	if created2 {
		t.Error("InitEmpty created=true on existing file (should be idempotent)")
	}
}

func TestOpenHands_AllStdioEntries(t *testing.T) {
	initial := `[mcp]
stdio_servers = [
    {name = "fetch", command = "uvx", args = ["mcp-server-fetch"]},
    {name = "fs", command = "npx", args = ["@mcp/fs", "/"]},
]
shttp_servers = [
    {name = "serena", url = "http://127.0.0.1:9121/mcp"},
]
`
	path := setupOpenHandsConfig(t, initial)
	o := &openHands{path: path}

	got, err := o.AllStdioEntries()
	if err != nil {
		t.Fatalf("AllStdioEntries: %v", err)
	}
	// Sorted by name: fetch, fs. shttp entries excluded.
	if len(got) != 2 {
		t.Fatalf("expected 2 stdio entries, got %d: %+v", len(got), got)
	}
	if got[0].Name != "fetch" || got[0].Command != "uvx" {
		t.Errorf("entry[0] = %+v", got[0])
	}
	if got[1].Name != "fs" || got[1].Command != "npx" {
		t.Errorf("entry[1] = %+v", got[1])
	}
	if len(got[0].Args) != 1 || got[0].Args[0] != "mcp-server-fetch" {
		t.Errorf("entry[0].Args = %v", got[0].Args)
	}
}

func TestOpenHands_FindStdioLanguageServerEntries(t *testing.T) {
	initial := `[mcp]
stdio_servers = [
    {name = "go-ls", command = "mcp-language-server", args = ["--lsp", "go"]},
    {name = "fetch", command = "uvx", args = ["mcp-server-fetch"]},
]
`
	path := setupOpenHandsConfig(t, initial)
	o := &openHands{path: path}

	got, err := o.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 LSP entry, got %d: %+v", len(got), got)
	}
	if got[0].Name != "go-ls" || got[0].Language != "go" {
		t.Errorf("LSP entry = %+v", got[0])
	}
}

func TestOpenHands_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// Live config has the migrated hub-HTTP form.
	live := `[mcp]
shttp_servers = [
    {name = "memory", url = "http://127.0.0.1:9140/mcp"},
]
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	// Backup holds the pre-hub stdio form.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	backupBody := `[mcp]
stdio_servers = [
    {name = "memory", command = "npx", args = ["-y", "mem"]},
]
`
	if err := os.WriteFile(backup, []byte(backupBody), 0600); err != nil {
		t.Fatal(err)
	}
	o := &openHands{path: path}
	if err := o.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	sec := parseOpenHandsTOML(t, path)

	// The hub-HTTP shttp form must be gone.
	if shttpByName(t, sec, "memory") != nil {
		t.Errorf("hub-HTTP memory should be removed after restore; section: %#v", sec)
	}
	// The pre-hub stdio form must be restored.
	st := stdioByName(sec, "memory")
	if st == nil {
		t.Fatalf("stdio memory not restored; section: %#v", sec)
	}
	if cmd, _ := st["command"].(string); cmd != "npx" {
		t.Errorf("restored stdio command = %q, want npx", cmd)
	}
}

func TestOpenHands_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	live := `[mcp]
shttp_servers = [
    {name = "newserver", url = "http://127.0.0.1:9999/mcp"},
]
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte("[mcp]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	o := &openHands{path: path}
	if err := o.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	sec := parseOpenHandsTOML(t, path)
	if shttpByName(t, sec, "newserver") != nil {
		t.Errorf("newserver should be removed when backup lacks it; section: %#v", sec)
	}
}

func TestOpenHands_RestoreEntryFromBackup_RefusesHubHTTPBackupEntry(t *testing.T) {
	// Backup was taken AFTER an earlier migrate already rewrote this entry to
	// hub-HTTP form. Defensive refuse.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`[mcp]
shttp_servers = [
    {name = "memory", url = "http://127.0.0.1:9200/mcp"},
]
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`[mcp]
shttp_servers = [
    {name = "memory", url = "http://127.0.0.1:9200/mcp"},
]
`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &openHands{path: path}
	err := o.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

func TestOpenHands_RestoreEntryFromBackupForRollback_RestoresHubEntryVerbatim(t *testing.T) {
	// Rollback path: the backup's hub-HTTP entry IS what must be restored.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`[mcp]
shttp_servers = [
    {name = "serena", url = "http://127.0.0.1:9333/serena/mcp"},
]
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`[mcp]
shttp_servers = [
    {name = "serena", url = "http://127.0.0.1:9121/mcp"},
]
`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &openHands{path: path}
	if err := o.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
	}
	got, err := o.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://127.0.0.1:9121/mcp" {
		t.Errorf("rollback should restore the backup's legacy hub url verbatim, got %+v", got)
	}
}

func TestOpenHands_BackupContainsEntry(t *testing.T) {
	dir := t.TempDir()
	backup := filepath.Join(dir, "config.toml.bak-mcp-local-hub-20260101-000000")
	if err := os.WriteFile(backup, []byte(`[mcp]
shttp_servers = [
    {name = "serena", url = "http://127.0.0.1:9121/mcp"},
]
stdio_servers = [
    {name = "fetch", command = "uvx"},
]
`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &openHands{path: filepath.Join(dir, "config.toml")}

	cases := []struct {
		name string
		want bool
	}{
		{"serena", true},
		{"fetch", true},
		{"absent", false},
	}
	for _, c := range cases {
		has, err := o.BackupContainsEntry(backup, c.name)
		if err != nil {
			t.Fatalf("BackupContainsEntry(%q): %v", c.name, err)
		}
		if has != c.want {
			t.Errorf("BackupContainsEntry(%q) = %v, want %v", c.name, has, c.want)
		}
	}
}

func TestOpenHands_BackupEntryIsHubManaged(t *testing.T) {
	dir := t.TempDir()
	backup := filepath.Join(dir, "config.toml.bak-mcp-local-hub-20260101-000000")
	if err := os.WriteFile(backup, []byte(`[mcp]
shttp_servers = [
    {name = "hub", url = "http://127.0.0.1:9121/mcp"},
    {name = "remote", url = "https://api.example.com/mcp/shttp"},
]
stdio_servers = [
    {name = "fetch", command = "uvx"},
]
`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &openHands{path: filepath.Join(dir, "config.toml")}

	cases := []struct {
		name string
		want bool
	}{
		{"hub", true},      // loopback url, no command -> hub-managed
		{"remote", false},  // remote url -> user-configured, not hub
		{"fetch", false},   // stdio entry -> never hub-managed
		{"missing", false}, // absent
	}
	for _, c := range cases {
		got, err := o.BackupEntryIsHubManaged(backup, c.name)
		if err != nil {
			t.Fatalf("BackupEntryIsHubManaged(%q): %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("BackupEntryIsHubManaged(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOpenHands_AddEntry_PreservesUnknownSections(t *testing.T) {
	// OpenHands config.toml carries many non-MCP sections; AddEntry must not
	// drop them.
	initial := `[core]
workspace_base = "/work"

[llm]
model = "claude-3"

[mcp]
shttp_servers = ["https://remote.example.com/mcp/shttp"]
`
	path := setupOpenHandsConfig(t, initial)
	o := &openHands{path: path}

	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://127.0.0.1:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	data, _ := os.ReadFile(path)
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	core, _ := m["core"].(map[string]any)
	if core == nil || core["workspace_base"] != "/work" {
		t.Errorf("[core] section dropped or mangled: %#v", m["core"])
	}
	llm, _ := m["llm"].(map[string]any)
	if llm == nil || llm["model"] != "claude-3" {
		t.Errorf("[llm] section dropped or mangled: %#v", m["llm"])
	}
}

// TestOpenHands_NewOpenHands_PathShape verifies the constructor binds the
// documented per-user OpenHands config path. (The constructor is exported so
// the central client registry can reference it.)
func TestOpenHands_NewOpenHands_PathShape(t *testing.T) {
	c, err := NewOpenHands()
	if err != nil {
		t.Fatalf("NewOpenHands: %v", err)
	}
	if c.Name() != "openhands" {
		t.Errorf("Name() = %q", c.Name())
	}
	p := c.ConfigPath()
	if !strings.HasSuffix(filepath.ToSlash(p), ".openhands/config.toml") {
		t.Errorf("ConfigPath() = %q, want it to end with .openhands/config.toml", p)
	}
}

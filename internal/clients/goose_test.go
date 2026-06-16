package clients

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func setupGooseConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readGooseExtensions parses the config at path and returns the extensions
// map, normalized to map[string]any via asStringMap.
func readGooseExtensions(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	exts := asStringMap(m["extensions"])
	if exts == nil {
		return map[string]any{}
	}
	return exts
}

func TestGoose_NameAndConfigPath(t *testing.T) {
	c := &goose{path: filepath.Join("home", ".config", "goose", "config.yaml")}
	if c.Name() != "goose" {
		t.Errorf("Name = %q, want goose", c.Name())
	}
	if !strings.HasSuffix(c.ConfigPath(), filepath.Join(".config", "goose", "config.yaml")) {
		t.Errorf("ConfigPath = %q, want suffix .config/goose/config.yaml", c.ConfigPath())
	}
}

func TestGoose_IsRelayStdio(t *testing.T) {
	c := &goose{path: "x"}
	if c.IsRelayStdio() {
		t.Error("goose IsRelayStdio = true, want false (HTTP-direct streamable_http)")
	}
}

func TestGoose_DefaultConfigPath_XDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join("xdg", "root"))
	got := defaultGooseConfigPath(filepath.Join("home", "user"))
	want := filepath.Join("xdg", "root", "goose", "config.yaml")
	if got != want {
		t.Errorf("defaultGooseConfigPath with XDG = %q, want %q", got, want)
	}
}

func TestGoose_DefaultConfigPath_NoXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	got := defaultGooseConfigPath(filepath.Join("home", "user"))
	want := filepath.Join("home", "user", ".config", "goose", "config.yaml")
	if got != want {
		t.Errorf("defaultGooseConfigPath without XDG = %q, want %q", got, want)
	}
}

// TestGoose_AddEntry_StreamableHTTPShape pins the exact on-disk shape the hub
// writes, verified against the Goose serde schema (extensions.rs /
// extension.rs): top-level `extensions`, entry carries `enabled: true` +
// flattened streamable_http config (`type`, `name`, `uri`, `timeout`,
// `description`).
func TestGoose_AddEntry_StreamableHTTPShape(t *testing.T) {
	path := setupGooseConfig(t, "extensions: {}\n")
	c := &goose{path: path}
	if err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://127.0.0.1:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	exts := readGooseExtensions(t, path)
	serena := asStringMap(exts["serena"])
	if serena == nil {
		t.Fatalf("serena extension missing: %+v", exts)
	}
	if got := serena["type"]; got != "streamable_http" {
		t.Errorf("type = %v, want streamable_http", got)
	}
	if got := serena["uri"]; got != "http://127.0.0.1:9121/mcp" {
		t.Errorf("uri = %v, want http://127.0.0.1:9121/mcp", got)
	}
	if got := serena["name"]; got != "serena" {
		t.Errorf("name = %v, want serena", got)
	}
	if got, ok := serena["enabled"].(bool); !ok || !got {
		t.Errorf("enabled = %v, want true", serena["enabled"])
	}
	// No `url` field — Goose uses `uri`.
	if _, hasURL := serena["url"]; hasURL {
		t.Errorf("entry should not carry a `url` field (goose uses `uri`): %+v", serena)
	}
	// timeout present (the struct doc asks new configs to include it).
	if _, hasTimeout := serena["timeout"]; !hasTimeout {
		t.Errorf("entry missing timeout field: %+v", serena)
	}
}

func TestGoose_AddEntry_ReplacesStdioBlock(t *testing.T) {
	initial := `extensions:
  serena:
    enabled: true
    type: stdio
    name: serena
    cmd: uvx
    args: ["--from", "git+...", "serena", "start-mcp-server"]
    envs:
      FOO: bar
  other:
    enabled: true
    type: stdio
    name: other
    cmd: echo
    args: ["hi"]
`
	path := setupGooseConfig(t, initial)
	c := &goose{path: path}
	if err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://127.0.0.1:9122/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	exts := readGooseExtensions(t, path)
	serena := asStringMap(exts["serena"])
	if serena == nil {
		t.Fatalf("serena entry missing after AddEntry")
	}
	// Old stdio fields dropped (wholesale replace).
	for _, k := range []string{"cmd", "args", "envs"} {
		if _, has := serena[k]; has {
			t.Errorf("old stdio field %q not removed: %+v", k, serena)
		}
	}
	if serena["type"] != "streamable_http" {
		t.Errorf("type not switched to streamable_http: %+v", serena)
	}
	// Other extension preserved.
	if _, ok := exts["other"]; !ok {
		t.Error("other extension dropped")
	}
}

func TestGoose_AddEntry_WithHeaders(t *testing.T) {
	path := setupGooseConfig(t, "extensions: {}\n")
	c := &goose{path: path}
	hdrs := map[string]string{"Authorization": "Bearer xyz"}
	if err := c.AddEntry(MCPEntry{Name: "memory", URL: "http://127.0.0.1:9140/mcp", Headers: hdrs}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	got, err := c.GetEntry("memory")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://127.0.0.1:9140/mcp" {
		t.Fatalf("memory URL not set: %+v", got)
	}
	if got.Headers["Authorization"] != "Bearer xyz" {
		t.Errorf("headers not round-tripped: %+v", got.Headers)
	}
}

func TestGoose_AddEntry_NoHeaders_OmitsField(t *testing.T) {
	path := setupGooseConfig(t, "extensions: {}\n")
	c := &goose{path: path}
	if err := c.AddEntry(MCPEntry{Name: "time", URL: "http://127.0.0.1:9150/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	exts := readGooseExtensions(t, path)
	entry := asStringMap(exts["time"])
	if _, hasHeaders := entry["headers"]; hasHeaders {
		t.Errorf("headers field should be omitted when none supplied: %+v", entry)
	}
}

func TestGoose_AddEntry_CreatesFromMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml") // does not exist yet
	c := &goose{path: path}
	if err := c.AddEntry(MCPEntry{Name: "time", URL: "http://127.0.0.1:9150/mcp"}); err != nil {
		t.Fatalf("AddEntry on missing file: %v", err)
	}
	got, err := c.GetEntry("time")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://127.0.0.1:9150/mcp" {
		t.Fatalf("time URL not set: %+v", got)
	}
}

func TestGoose_RemoveEntry(t *testing.T) {
	initial := `extensions:
  serena:
    enabled: true
    type: streamable_http
    name: serena
    uri: "http://127.0.0.1:9122/mcp"
  memory:
    enabled: true
    type: streamable_http
    name: memory
    uri: "http://127.0.0.1:9140/mcp"
`
	path := setupGooseConfig(t, initial)
	c := &goose{path: path}
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	exts := readGooseExtensions(t, path)
	if _, ok := exts["serena"]; ok {
		t.Errorf("serena not removed: %+v", exts)
	}
	if _, ok := exts["memory"]; !ok {
		t.Error("memory also removed (should be preserved)")
	}
}

func TestGoose_RemoveEntry_Idempotent(t *testing.T) {
	path := setupGooseConfig(t, "extensions: {}\n")
	c := &goose{path: path}
	if err := c.RemoveEntry("nonexistent"); err != nil {
		t.Fatalf("RemoveEntry on absent entry should be nil: %v", err)
	}
}

func TestGoose_GetEntry_Absent(t *testing.T) {
	path := setupGooseConfig(t, "extensions: {}\n")
	c := &goose{path: path}
	got, err := c.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for absent entry, got %+v", got)
	}
}

func TestGoose_InitEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := &goose{path: path}

	created, err := c.InitEmpty()
	if err != nil {
		t.Fatalf("InitEmpty: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on fresh file")
	}
	data, _ := os.ReadFile(path)
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("stub is not valid YAML: %v", err)
	}
	if _, ok := m["extensions"]; !ok {
		t.Errorf("stub missing extensions key: %s", data)
	}

	created2, err := c.InitEmpty()
	if err != nil {
		t.Fatalf("InitEmpty (2nd): %v", err)
	}
	if created2 {
		t.Error("expected created=false on second call")
	}
}

// TestGoose_Backup_SeedsOnFreshHost mirrors the opencode/cursor seed-then-
// backup pattern: BackupKeep MkdirAll's the nested parent and InitEmpty-seeds
// an `extensions: {}` stub when the config is absent, so Backup SUCCEEDS on a
// clean install (it does NOT return ErrClientNotInstalled). The seeded stub
// and the timestamped backup both exist afterward.
func TestGoose_Backup_SeedsOnFreshHost(t *testing.T) {
	dir := t.TempDir()
	// Nested parent does not exist yet — exercises the load-bearing MkdirAll.
	path := filepath.Join(dir, "nested", "goose", "config.yaml")
	c := &goose{path: path}
	backup, err := c.Backup()
	if err != nil {
		t.Fatalf("Backup on fresh host should seed+succeed, got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config stub not seeded at %s: %v", path, err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("backup file not written at %s: %v", backup, err)
	}
}

func TestGoose_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &goose{path: path}
	got, ok, err := c.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestGoose_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	live := `extensions:
  memory:
    enabled: true
    type: streamable_http
    name: memory
    uri: "http://127.0.0.1:9123/mcp"
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	backupBody := `extensions:
  memory:
    enabled: true
    type: stdio
    name: memory
    cmd: npx
    args: ["-y", "mem"]
`
	if err := os.WriteFile(backup, []byte(backupBody), 0600); err != nil {
		t.Fatal(err)
	}
	c := &goose{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	exts := readGooseExtensions(t, path)
	mem := asStringMap(exts["memory"])
	if mem == nil {
		t.Fatalf("memory entry missing after restore: %+v", exts)
	}
	if cmd, _ := mem["cmd"].(string); cmd != "npx" {
		t.Errorf("expected restored stdio cmd npx, got %+v", mem)
	}
	if _, hasURI := mem["uri"]; hasURI {
		t.Errorf("hub uri should be gone after restore, got %+v", mem)
	}
}

func TestGoose_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	live := `extensions:
  newserver:
    enabled: true
    type: streamable_http
    name: newserver
    uri: "http://127.0.0.1:9999/mcp"
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &goose{path: path}
	if err := c.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	exts := readGooseExtensions(t, path)
	if _, ok := exts["newserver"]; ok {
		t.Errorf("newserver should have been removed, got %+v", exts)
	}
}

func TestGoose_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	// Backup was taken AFTER an earlier migrate already rewrote this entry to
	// hub-managed (loopback uri) form. Defensive refuse.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`extensions:
  memory:
    enabled: true
    type: streamable_http
    name: memory
    uri: "http://127.0.0.1:9200/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`extensions:
  memory:
    enabled: true
    type: streamable_http
    name: memory
    uri: "http://127.0.0.1:9200/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &goose{path: path}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

func TestGoose_RestoreEntryFromBackupForRollback_BypassesGuard(t *testing.T) {
	// Rollback variant writes the hub backup entry verbatim despite the
	// demigrate guard.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`extensions:
  serena:
    enabled: true
    type: streamable_http
    name: serena
    uri: "http://127.0.0.1:9300/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`extensions:
  serena:
    enabled: true
    type: streamable_http
    name: serena
    uri: "http://127.0.0.1:9121/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &goose{path: path}
	if err := c.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
	}
	got, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://127.0.0.1:9121/mcp" {
		t.Errorf("expected verbatim legacy hub uri after rollback, got %+v", got)
	}
}

func TestGoose_BackupContainsEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	backup := path + ".bak"
	if err := os.WriteFile(backup, []byte(`extensions:
  present:
    enabled: true
    type: streamable_http
    name: present
    uri: "http://127.0.0.1:9400/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &goose{path: path}

	has, err := c.BackupContainsEntry(backup, "present")
	if err != nil || !has {
		t.Errorf("BackupContainsEntry(present) = %v, %v; want true, nil", has, err)
	}
	missing, err := c.BackupContainsEntry(backup, "absent")
	if err != nil || missing {
		t.Errorf("BackupContainsEntry(absent) = %v, %v; want false, nil", missing, err)
	}
}

func TestGoose_BackupEntryIsHubManaged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	backup := path + ".bak"
	if err := os.WriteFile(backup, []byte(`extensions:
  hubbed:
    enabled: true
    type: streamable_http
    name: hubbed
    uri: "http://127.0.0.1:9500/mcp"
  remote:
    enabled: true
    type: streamable_http
    name: remote
    uri: "https://api.example.com/mcp"
  stdio:
    enabled: true
    type: stdio
    name: stdio
    cmd: npx
    args: ["-y", "mem"]
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &goose{path: path}

	cases := []struct {
		name string
		want bool
	}{
		{"hubbed", true},  // loopback uri, no cmd
		{"remote", false}, // non-loopback uri -> user-configured remote
		{"stdio", false},  // has cmd
		{"absent", false}, // missing
	}
	for _, tc := range cases {
		got, err := c.BackupEntryIsHubManaged(backup, tc.name)
		if err != nil {
			t.Errorf("%s: unexpected err %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("BackupEntryIsHubManaged(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGoose_AllStdioEntries(t *testing.T) {
	path := setupGooseConfig(t, `extensions:
  stdio1:
    enabled: true
    type: stdio
    name: stdio1
    cmd: npx
    args: ["-y", "pkg"]
  httponly:
    enabled: true
    type: streamable_http
    name: httponly
    uri: "http://127.0.0.1:9600/mcp"
  disabledStdio:
    enabled: false
    type: stdio
    name: disabledStdio
    cmd: echo
    args: ["x"]
`)
	c := &goose{path: path}
	entries, err := c.AllStdioEntries()
	if err != nil {
		t.Fatalf("AllStdioEntries: %v", err)
	}
	// Only stdio1 qualifies: httponly has no cmd; disabledStdio is disabled.
	if len(entries) != 1 || entries[0].Name != "stdio1" {
		t.Fatalf("expected [stdio1], got %+v", entries)
	}
	if entries[0].Command != "npx" {
		t.Errorf("command = %q, want npx", entries[0].Command)
	}
	if len(entries[0].Args) != 2 || entries[0].Args[0] != "-y" {
		t.Errorf("args = %+v, want [-y pkg]", entries[0].Args)
	}
}

func TestGoose_FindStdioLanguageServerEntries(t *testing.T) {
	path := setupGooseConfig(t, `extensions:
  go-ls:
    enabled: true
    type: stdio
    name: go-ls
    cmd: mcp-language-server
    args: ["--lsp", "gopls"]
  notls:
    enabled: true
    type: stdio
    name: notls
    cmd: npx
    args: ["-y", "mem"]
  httpls:
    enabled: true
    type: streamable_http
    name: httpls
    uri: "http://127.0.0.1:9700/mcp"
`)
	c := &goose{path: path}
	entries, err := c.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "go-ls" {
		t.Fatalf("expected [go-ls], got %+v", entries)
	}
	if entries[0].Language != "gopls" {
		t.Errorf("language = %q, want gopls", entries[0].Language)
	}
}

func TestGoose_PreservesUnknownTopLevelKeys(t *testing.T) {
	// Goose config.yaml holds many non-MCP settings (provider/model/etc.);
	// AddEntry/RemoveEntry must not clobber them.
	path := setupGooseConfig(t, `GOOSE_PROVIDER: anthropic
GOOSE_MODEL: claude-sonnet
extensions:
  existing:
    enabled: true
    type: streamable_http
    name: existing
    uri: "http://127.0.0.1:9800/mcp"
`)
	c := &goose{path: path}
	if err := c.AddEntry(MCPEntry{Name: "new", URL: "http://127.0.0.1:9801/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	data, _ := os.ReadFile(path)
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if m["GOOSE_PROVIDER"] != "anthropic" {
		t.Errorf("top-level GOOSE_PROVIDER clobbered: %+v", m["GOOSE_PROVIDER"])
	}
	if _, ok := m["GOOSE_MODEL"]; !ok {
		t.Error("top-level GOOSE_MODEL dropped")
	}
	// Both the existing and new extensions survive.
	exts := asStringMap(m["extensions"])
	if _, ok := exts["existing"]; !ok {
		t.Error("existing extension dropped")
	}
	if _, ok := exts["new"]; !ok {
		t.Error("new extension missing")
	}
}

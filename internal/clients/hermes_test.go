package clients

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func setupHermesConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHermes_NameAndConfigPath(t *testing.T) {
	c := &hermes{path: filepath.Join("home", ".hermes", "config.yaml")}
	if c.Name() != "hermes" {
		t.Errorf("Name = %q, want hermes", c.Name())
	}
	if !strings.HasSuffix(c.ConfigPath(), filepath.Join(".hermes", "config.yaml")) {
		t.Errorf("ConfigPath = %q, want suffix .hermes/config.yaml", c.ConfigPath())
	}
}

func TestHermes_AddEntry_ReplaceStdioBlock(t *testing.T) {
	initial := `mcp_servers:
  serena:
    command: uvx
    args: ["--from", "git+...", "serena", "start-mcp-server"]
    env:
      FOO: bar
  other:
    command: echo
    args: ["hi"]
`
	path := setupHermesConfig(t, initial)
	c := &hermes{path: path}

	if err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9122/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	// Re-parse to assert structure (avoids brittle string matching on
	// yaml.v3 quoting/ordering).
	got, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://localhost:9122/mcp" {
		t.Fatalf("serena URL not set: %+v", got)
	}

	// Verify old stdio fields were dropped from the serena block (wholesale replace).
	m := readHermesServers(t, path)
	serena, _ := m["serena"].(map[string]any)
	if serena == nil {
		// yaml.v3 may decode nested maps as map[string]any; handle both
		serena = asStringMap(m["serena"])
	}
	if serena == nil {
		t.Fatalf("serena entry missing after AddEntry")
	}
	if _, hasCmd := serena["command"]; hasCmd {
		t.Errorf("old command field not removed from serena block: %+v", serena)
	}
	if _, hasArgs := serena["args"]; hasArgs {
		t.Errorf("old args field not removed from serena block: %+v", serena)
	}
	if _, hasEnv := serena["env"]; hasEnv {
		t.Errorf("old env field not removed from serena block: %+v", serena)
	}
	// Other section preserved.
	if _, ok := m["other"]; !ok {
		t.Error("other section dropped")
	}
}

func TestHermes_AddEntry_WithHeaders(t *testing.T) {
	path := setupHermesConfig(t, "mcp_servers: {}\n")
	c := &hermes{path: path}
	hdrs := map[string]string{"Authorization": "Bearer xyz"}
	if err := c.AddEntry(MCPEntry{Name: "memory", URL: "http://localhost:9140/mcp", Headers: hdrs}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	got, err := c.GetEntry("memory")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://localhost:9140/mcp" {
		t.Fatalf("memory URL not set: %+v", got)
	}
	if got.Headers["Authorization"] != "Bearer xyz" {
		t.Errorf("headers not round-tripped: %+v", got.Headers)
	}
}

func TestHermes_AddEntry_CreatesFromMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml") // does not exist yet
	c := &hermes{path: path}
	if err := c.AddEntry(MCPEntry{Name: "time", URL: "http://localhost:9150/mcp"}); err != nil {
		t.Fatalf("AddEntry on missing file: %v", err)
	}
	got, err := c.GetEntry("time")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://localhost:9150/mcp" {
		t.Fatalf("time URL not set: %+v", got)
	}
}

func TestHermes_RemoveEntry(t *testing.T) {
	initial := `mcp_servers:
  serena:
    url: "http://localhost:9122/mcp"
  memory:
    url: "http://localhost:9140/mcp"
`
	path := setupHermesConfig(t, initial)
	c := &hermes{path: path}
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	m := readHermesServers(t, path)
	if _, ok := m["serena"]; ok {
		t.Errorf("serena not removed: %+v", m)
	}
	if _, ok := m["memory"]; !ok {
		t.Error("memory also removed (should be preserved)")
	}
}

func TestHermes_RemoveEntry_Idempotent(t *testing.T) {
	path := setupHermesConfig(t, "mcp_servers: {}\n")
	c := &hermes{path: path}
	if err := c.RemoveEntry("nonexistent"); err != nil {
		t.Fatalf("RemoveEntry on absent entry should be nil: %v", err)
	}
}

func TestHermes_GetEntry_Absent(t *testing.T) {
	path := setupHermesConfig(t, "mcp_servers: {}\n")
	c := &hermes{path: path}
	got, err := c.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for absent entry, got %+v", got)
	}
}

func TestHermes_InitEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := &hermes{path: path}

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
	if _, ok := m["mcp_servers"]; !ok {
		t.Errorf("stub missing mcp_servers key: %s", data)
	}

	// Second call is idempotent.
	created2, err := c.InitEmpty()
	if err != nil {
		t.Fatalf("InitEmpty (2nd): %v", err)
	}
	if created2 {
		t.Error("expected created=false on second call")
	}
}

func TestHermes_Backup_NotInstalled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml") // absent
	c := &hermes{path: path}
	_, err := c.Backup()
	var notInstalled *ErrClientNotInstalled
	if !errors.As(err, &notInstalled) {
		t.Fatalf("expected ErrClientNotInstalled, got %v", err)
	}
}

func TestHermes_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &hermes{path: path}
	got, ok, err := c.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestHermes_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	live := `mcp_servers:
  memory:
    url: "http://localhost:9123/mcp"
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	backupBody := `mcp_servers:
  memory:
    command: npx
    args: ["-y", "mem"]
`
	if err := os.WriteFile(backup, []byte(backupBody), 0600); err != nil {
		t.Fatal(err)
	}
	c := &hermes{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	m := readHermesServers(t, path)
	mem := asStringMap(m["memory"])
	if mem == nil {
		t.Fatalf("memory entry missing after restore: %+v", m)
	}
	if cmd, _ := mem["command"].(string); cmd != "npx" {
		t.Errorf("expected restored stdio command npx, got %+v", mem)
	}
	if _, hasURL := mem["url"]; hasURL {
		t.Errorf("hub-HTTP url should be gone after restore, got %+v", mem)
	}
}

func TestHermes_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	live := `mcp_servers:
  newserver:
    url: "http://localhost:9999/mcp"
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &hermes{path: path}
	if err := c.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	m := readHermesServers(t, path)
	if _, ok := m["newserver"]; ok {
		t.Errorf("newserver should have been removed, got %+v", m)
	}
}

func TestHermes_RestoreEntryFromBackup_RefusesHubHTTPBackupEntry(t *testing.T) {
	// Backup was taken AFTER an earlier migrate already rewrote this
	// entry to hub-HTTP form. Defensive refuse.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`mcp_servers:
  memory:
    url: "http://localhost:9200/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`mcp_servers:
  memory:
    url: "http://localhost:9200/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &hermes{path: path}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

func TestHermes_RestoreEntryFromBackupForRollback_BypassesGuard(t *testing.T) {
	// Rollback variant writes the hub-HTTP backup entry verbatim despite
	// the demigrate guard.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`mcp_servers:
  serena:
    url: "http://localhost:9300/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`mcp_servers:
  serena:
    url: "http://localhost:9121/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &hermes{path: path}
	if err := c.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
	}
	got, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://localhost:9121/mcp" {
		t.Errorf("expected verbatim legacy hub URL after rollback, got %+v", got)
	}
}

func TestHermes_BackupContainsEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	backup := path + ".bak"
	if err := os.WriteFile(backup, []byte(`mcp_servers:
  present:
    url: "http://localhost:9400/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &hermes{path: path}

	has, err := c.BackupContainsEntry(backup, "present")
	if err != nil || !has {
		t.Errorf("BackupContainsEntry(present) = %v, %v; want true, nil", has, err)
	}
	missing, err := c.BackupContainsEntry(backup, "absent")
	if err != nil || missing {
		t.Errorf("BackupContainsEntry(absent) = %v, %v; want false, nil", missing, err)
	}
}

func TestHermes_BackupEntryIsHubManaged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	backup := path + ".bak"
	if err := os.WriteFile(backup, []byte(`mcp_servers:
  hubbed:
    url: "http://localhost:9500/mcp"
  remote:
    url: "https://api.example.com/mcp"
  stdio:
    command: npx
    args: ["-y", "mem"]
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &hermes{path: path}

	cases := []struct {
		name string
		want bool
	}{
		{"hubbed", true},  // loopback url, no command
		{"remote", false}, // non-loopback url -> user-configured remote
		{"stdio", false},  // has command
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

func TestHermes_AllStdioEntries(t *testing.T) {
	path := setupHermesConfig(t, `mcp_servers:
  stdio1:
    command: npx
    args: ["-y", "pkg"]
  httponly:
    url: "http://localhost:9600/mcp"
  disabledStdio:
    command: echo
    args: ["x"]
    disabled: true
`)
	c := &hermes{path: path}
	entries, err := c.AllStdioEntries()
	if err != nil {
		t.Fatalf("AllStdioEntries: %v", err)
	}
	// Only stdio1 qualifies: httponly has no command; disabledStdio is disabled.
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

func TestHermes_FindStdioLanguageServerEntries(t *testing.T) {
	path := setupHermesConfig(t, `mcp_servers:
  go-ls:
    command: mcp-language-server
    args: ["--lsp", "gopls"]
  notls:
    command: npx
    args: ["-y", "mem"]
  httpls:
    url: "http://localhost:9700/mcp"
`)
	c := &hermes{path: path}
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

func TestHermes_PreservesUnknownTopLevelKeys(t *testing.T) {
	// Hermes config.yaml holds many non-MCP settings; AddEntry/RemoveEntry
	// must not clobber them.
	path := setupHermesConfig(t, `model: hermes-4
temperature: 0.7
mcp_servers:
  existing:
    url: "http://localhost:9800/mcp"
`)
	c := &hermes{path: path}
	if err := c.AddEntry(MCPEntry{Name: "new", URL: "http://localhost:9801/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	data, _ := os.ReadFile(path)
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if m["model"] != "hermes-4" {
		t.Errorf("top-level model key clobbered: %+v", m["model"])
	}
	if _, ok := m["temperature"]; !ok {
		t.Error("top-level temperature key dropped")
	}
}

// readHermesServers parses the config at path and returns the mcp_servers
// map, normalized to map[string]any via asStringMap.
func readHermesServers(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	servers := asStringMap(m["mcp_servers"])
	if servers == nil {
		return map[string]any{}
	}
	return servers
}

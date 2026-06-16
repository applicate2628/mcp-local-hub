package clients

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func setupContinueConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readContinueList parses the config at path and returns the mcpServers list
// (a slice of map[string]any normalized via asStringMap).
func readContinueList(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	rawList, _ := m["mcpServers"].([]any)
	out := make([]map[string]any, 0, len(rawList))
	for _, item := range rawList {
		out = append(out, asStringMap(item))
	}
	return out
}

// findContinueEntry returns the list item whose inner name == name, or nil.
func findContinueEntry(list []map[string]any, name string) map[string]any {
	for _, item := range list {
		if item == nil {
			continue
		}
		if n, _ := item["name"].(string); n == name {
			return item
		}
	}
	return nil
}

func TestContinue_NameAndConfigPath(t *testing.T) {
	c := &continueClient{path: filepath.Join("home", ".continue", "config.yaml")}
	if c.Name() != "continue" {
		t.Errorf("Name = %q, want continue", c.Name())
	}
	if !strings.HasSuffix(c.ConfigPath(), filepath.Join(".continue", "config.yaml")) {
		t.Errorf("ConfigPath = %q, want suffix .continue/config.yaml", c.ConfigPath())
	}
	if c.IsRelayStdio() {
		t.Error("continue must report IsRelayStdio=false (URL-native)")
	}
}

func TestContinue_AddEntry_AppendsNewServer(t *testing.T) {
	// List with one pre-existing operator server; AddEntry must append, not
	// replace, and preserve the existing item.
	initial := `mcpServers:
  - name: Existing
    type: stdio
    command: npx
    args: ["-y", "other"]
`
	path := setupContinueConfig(t, initial)
	c := &continueClient{path: path}

	if err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://127.0.0.1:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	got, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://127.0.0.1:9121/mcp" {
		t.Fatalf("serena URL not set: %+v", got)
	}

	list := readContinueList(t, path)
	serena := findContinueEntry(list, "serena")
	if serena == nil {
		t.Fatalf("serena item missing after AddEntry: %+v", list)
	}
	// Hub writes the streamable-http remote shape.
	if typ, _ := serena["type"].(string); typ != "streamable-http" {
		t.Errorf("type = %v, want streamable-http", serena["type"])
	}
	// Existing operator entry preserved.
	if findContinueEntry(list, "Existing") == nil {
		t.Error("pre-existing 'Existing' item dropped by AddEntry")
	}
}

func TestContinue_AddEntry_ReplaceDropsStdioFields(t *testing.T) {
	initial := `mcpServers:
  - name: serena
    type: stdio
    command: uvx
    args: ["--from", "git+...", "serena"]
    env:
      FOO: bar
`
	path := setupContinueConfig(t, initial)
	c := &continueClient{path: path}

	if err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://127.0.0.1:9122/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	list := readContinueList(t, path)
	// Exactly one serena entry (replaced in place, not duplicated).
	count := 0
	for _, item := range list {
		if n, _ := item["name"].(string); n == "serena" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 serena item, got %d: %+v", count, list)
	}
	serena := findContinueEntry(list, "serena")
	for _, dropped := range []string{"command", "args", "env"} {
		if _, has := serena[dropped]; has {
			t.Errorf("old stdio field %q not removed from serena block: %+v", dropped, serena)
		}
	}
}

func TestContinue_AddEntry_WithHeaders(t *testing.T) {
	path := setupContinueConfig(t, "mcpServers: []\n")
	c := &continueClient{path: path}
	hdrs := map[string]string{"Authorization": "Bearer xyz"}
	if err := c.AddEntry(MCPEntry{Name: "memory", URL: "http://127.0.0.1:9140/mcp", Headers: hdrs}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}

	// Headers round-trip through requestOptions.headers.
	got, err := c.GetEntry("memory")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.Headers["Authorization"] != "Bearer xyz" {
		t.Fatalf("headers not round-tripped: %+v", got)
	}

	// Assert the on-disk nesting is requestOptions.headers (Continue-canonical).
	list := readContinueList(t, path)
	mem := findContinueEntry(list, "memory")
	ro := asStringMap(mem["requestOptions"])
	if ro == nil {
		t.Fatalf("requestOptions missing: %+v", mem)
	}
	h := asStringMap(ro["headers"])
	if h == nil || h["Authorization"] != "Bearer xyz" {
		t.Errorf("requestOptions.headers not written correctly: %+v", ro)
	}
}

func TestContinue_AddEntry_CreatesFromMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml") // does not exist yet
	c := &continueClient{path: path}
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

func TestContinue_RemoveEntry(t *testing.T) {
	initial := `mcpServers:
  - name: serena
    type: streamable-http
    url: "http://127.0.0.1:9122/mcp"
  - name: memory
    type: streamable-http
    url: "http://127.0.0.1:9140/mcp"
`
	path := setupContinueConfig(t, initial)
	c := &continueClient{path: path}
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	list := readContinueList(t, path)
	if findContinueEntry(list, "serena") != nil {
		t.Errorf("serena not removed: %+v", list)
	}
	if findContinueEntry(list, "memory") == nil {
		t.Error("memory also removed (should be preserved)")
	}
}

func TestContinue_RemoveEntry_Idempotent(t *testing.T) {
	path := setupContinueConfig(t, "mcpServers: []\n")
	c := &continueClient{path: path}
	if err := c.RemoveEntry("nonexistent"); err != nil {
		t.Fatalf("RemoveEntry on absent entry should be nil: %v", err)
	}
}

func TestContinue_GetEntry_Absent(t *testing.T) {
	path := setupContinueConfig(t, "mcpServers: []\n")
	c := &continueClient{path: path}
	got, err := c.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for absent entry, got %+v", got)
	}
}

func TestContinue_InitEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := &continueClient{path: path}

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
	// Stub declares mcpServers as a (possibly empty) list.
	if _, ok := m["mcpServers"]; !ok {
		t.Errorf("stub missing mcpServers key: %s", data)
	}
	if _, ok := m["mcpServers"].([]any); !ok && m["mcpServers"] != nil {
		t.Errorf("stub mcpServers should be a list, got %T: %s", m["mcpServers"], data)
	}

	// Second call is idempotent.
	created2, err := c.InitEmpty()
	if err != nil {
		t.Fatalf("InitEmpty (2nd): %v", err)
	}
	if created2 {
		t.Error("expected created=false on second call")
	}

	// AddEntry works on the freshly-init'd stub.
	if err := c.AddEntry(MCPEntry{Name: "x", URL: "http://127.0.0.1:9000/mcp"}); err != nil {
		t.Fatalf("AddEntry after InitEmpty: %v", err)
	}
	if got, _ := c.GetEntry("x"); got == nil {
		t.Error("AddEntry after InitEmpty did not persist entry")
	}
}

func TestContinue_Backup_NotInstalled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml") // absent
	c := &continueClient{path: path}
	_, err := c.Backup()
	var notInstalled *ErrClientNotInstalled
	if !errors.As(err, &notInstalled) {
		t.Fatalf("expected ErrClientNotInstalled, got %v", err)
	}
}

func TestContinue_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &continueClient{path: path}
	got, ok, err := c.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestContinue_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	live := `mcpServers:
  - name: memory
    type: streamable-http
    url: "http://127.0.0.1:9123/mcp"
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	backupBody := `mcpServers:
  - name: memory
    type: stdio
    command: npx
    args: ["-y", "mem"]
`
	if err := os.WriteFile(backup, []byte(backupBody), 0600); err != nil {
		t.Fatal(err)
	}
	c := &continueClient{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	list := readContinueList(t, path)
	mem := findContinueEntry(list, "memory")
	if mem == nil {
		t.Fatalf("memory entry missing after restore: %+v", list)
	}
	if cmd, _ := mem["command"].(string); cmd != "npx" {
		t.Errorf("expected restored stdio command npx, got %+v", mem)
	}
	if _, hasURL := mem["url"]; hasURL {
		t.Errorf("hub-HTTP url should be gone after restore, got %+v", mem)
	}
}

func TestContinue_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	live := `mcpServers:
  - name: newserver
    type: streamable-http
    url: "http://127.0.0.1:9999/mcp"
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &continueClient{path: path}
	if err := c.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	list := readContinueList(t, path)
	if findContinueEntry(list, "newserver") != nil {
		t.Errorf("newserver should have been removed, got %+v", list)
	}
}

func TestContinue_RestoreEntryFromBackup_RefusesHubHTTPBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`mcpServers:
  - name: memory
    type: streamable-http
    url: "http://127.0.0.1:9200/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`mcpServers:
  - name: memory
    type: streamable-http
    url: "http://127.0.0.1:9200/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &continueClient{path: path}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

func TestContinue_RestoreEntryFromBackupForRollback_BypassesGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`mcpServers:
  - name: serena
    type: streamable-http
    url: "http://127.0.0.1:9300/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`mcpServers:
  - name: serena
    type: streamable-http
    url: "http://127.0.0.1:9121/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &continueClient{path: path}
	if err := c.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
	}
	got, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://127.0.0.1:9121/mcp" {
		t.Errorf("expected verbatim legacy hub URL after rollback, got %+v", got)
	}
}

func TestContinue_BackupContainsEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	backup := path + ".bak"
	if err := os.WriteFile(backup, []byte(`mcpServers:
  - name: present
    type: streamable-http
    url: "http://127.0.0.1:9400/mcp"
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &continueClient{path: path}

	has, err := c.BackupContainsEntry(backup, "present")
	if err != nil || !has {
		t.Errorf("BackupContainsEntry(present) = %v, %v; want true, nil", has, err)
	}
	missing, err := c.BackupContainsEntry(backup, "absent")
	if err != nil || missing {
		t.Errorf("BackupContainsEntry(absent) = %v, %v; want false, nil", missing, err)
	}
}

func TestContinue_BackupEntryIsHubManaged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	backup := path + ".bak"
	if err := os.WriteFile(backup, []byte(`mcpServers:
  - name: hubbed
    type: streamable-http
    url: "http://127.0.0.1:9500/mcp"
  - name: remote
    type: streamable-http
    url: "https://api.example.com/mcp"
  - name: stdio
    type: stdio
    command: npx
    args: ["-y", "mem"]
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &continueClient{path: path}

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

func TestContinue_AllStdioEntries(t *testing.T) {
	path := setupContinueConfig(t, `mcpServers:
  - name: stdio1
    type: stdio
    command: npx
    args: ["-y", "pkg"]
  - name: httponly
    type: streamable-http
    url: "http://127.0.0.1:9600/mcp"
  - name: disabledStdio
    type: stdio
    command: echo
    args: ["x"]
    disabled: true
`)
	c := &continueClient{path: path}
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

func TestContinue_FindStdioLanguageServerEntries(t *testing.T) {
	path := setupContinueConfig(t, `mcpServers:
  - name: go-ls
    type: stdio
    command: mcp-language-server
    args: ["--lsp", "gopls"]
  - name: notls
    type: stdio
    command: npx
    args: ["-y", "mem"]
  - name: httpls
    type: streamable-http
    url: "http://127.0.0.1:9700/mcp"
`)
	c := &continueClient{path: path}
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

func TestContinue_PreservesUnknownTopLevelKeys(t *testing.T) {
	// Continue config.yaml holds many non-MCP settings (models, context,
	// rules); AddEntry/RemoveEntry must not clobber them.
	path := setupContinueConfig(t, `name: My Assistant
models:
  - name: GPT-4o
    provider: openai
mcpServers:
  - name: existing
    type: streamable-http
    url: "http://127.0.0.1:9800/mcp"
`)
	c := &continueClient{path: path}
	if err := c.AddEntry(MCPEntry{Name: "new", URL: "http://127.0.0.1:9801/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	data, _ := os.ReadFile(path)
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if m["name"] != "My Assistant" {
		t.Errorf("top-level name key clobbered: %+v", m["name"])
	}
	if _, ok := m["models"]; !ok {
		t.Error("top-level models key dropped")
	}
	// Both the existing and the new MCP server are present.
	list := readContinueList(t, path)
	if findContinueEntry(list, "existing") == nil {
		t.Error("existing mcp server dropped")
	}
	if findContinueEntry(list, "new") == nil {
		t.Error("new mcp server not added")
	}
}

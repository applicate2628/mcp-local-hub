package clients

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func setupAiderConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readAiderList parses the config at path and returns the mcp-server list as
// a slice of name-keyed maps, so tests can assert structure without brittle
// string matching on yaml.v3 quoting/ordering.
func readAiderList(t *testing.T, path string) []any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	list, _ := m["mcp-server"].([]any)
	return list
}

// findAiderEntry returns the element of the mcp-server list whose `name` ==
// name, normalized via asStringMap, or nil.
func findAiderEntry(t *testing.T, path, name string) map[string]any {
	t.Helper()
	for _, raw := range readAiderList(t, path) {
		if aiderEntryName(raw) == name {
			return asStringMap(raw)
		}
	}
	return nil
}

func TestAider_NameAndConfigPath(t *testing.T) {
	c := &aider{path: filepath.Join("home", ".aider.conf.yml")}
	if c.Name() != "aider" {
		t.Errorf("Name = %q, want aider", c.Name())
	}
	if !strings.HasSuffix(c.ConfigPath(), ".aider.conf.yml") {
		t.Errorf("ConfigPath = %q, want suffix .aider.conf.yml", c.ConfigPath())
	}
}

func TestAider_IsRelayStdio(t *testing.T) {
	c := &aider{}
	if !c.IsRelayStdio() {
		t.Error("aider must be a relay-stdio adapter (IsRelayStdio=true): its documented MCP schema is stdio-only with no verifiable url/http server form")
	}
}

// TestAider_AddEntry_WritesStdioRelayShape verifies aider entries are written
// as stdio invocations of the local mcphub.exe relay subcommand, inside the
// `mcp-server` LIST with the `name` field carried INSIDE each object.
func TestAider_AddEntry_WritesStdioRelayShape(t *testing.T) {
	path := setupAiderConfig(t, "mcp-server: []\n")
	c := &aider{path: path}
	exePath := filepath.Join(t.TempDir(), "mcphub.exe")
	err := c.AddEntry(MCPEntry{
		Name:         "serena",
		URL:          "http://localhost:9121/mcp", // ignored by adapter; relay args take over
		RelayServer:  "serena",
		RelayDaemon:  "claude",
		RelayExePath: exePath,
	})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	serena := findAiderEntry(t, path, "serena")
	if serena == nil {
		t.Fatalf("serena entry missing after AddEntry")
	}
	if n, _ := serena["name"].(string); n != "serena" {
		t.Errorf("name field = %q, want serena (must live inside the list object)", n)
	}
	if cmd, _ := serena["command"].(string); cmd != exePath {
		t.Errorf("command = %q, want absolute mcphub.exe path", cmd)
	}
	argsAny, ok := serena["args"].([]any)
	if !ok || len(argsAny) != 5 {
		t.Fatalf("args must be 5-element array [relay, --server, <s>, --daemon, <d>], got %v", serena["args"])
	}
	want := []string{"relay", "--server", "serena", "--daemon", "claude"}
	for i, v := range want {
		got, _ := argsAny[i].(string)
		if got != v {
			t.Errorf("args[%d] = %q, want %q", i, got, v)
		}
	}
	// Must NOT write any HTTP shape fields — the stdio schema has no url.
	for _, bad := range []string{"url", "serverUrl", "httpUrl", "type", "timeout"} {
		if _, has := serena[bad]; has {
			t.Errorf("unexpected HTTP-shape field %q present in stdio-relay entry: %v", bad, serena)
		}
	}
}

// TestAider_AddEntry_RelayURLForm verifies the --url escape-hatch shape used
// by the serena dynamic-pool client-reconcile.
func TestAider_AddEntry_RelayURLForm(t *testing.T) {
	path := setupAiderConfig(t, "mcp-server: []\n")
	c := &aider{path: path}
	exePath := filepath.Join(t.TempDir(), "mcphub.exe")
	err := c.AddEntry(MCPEntry{
		Name:         "serena",
		RelayURL:     "http://127.0.0.1:9121/serena/mcp",
		RelayExePath: exePath,
	})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	serena := findAiderEntry(t, path, "serena")
	argsAny, ok := serena["args"].([]any)
	if !ok || len(argsAny) != 3 {
		t.Fatalf("args must be 3-element array [relay, --url, <u>], got %v", serena["args"])
	}
	want := []string{"relay", "--url", "http://127.0.0.1:9121/serena/mcp"}
	for i, v := range want {
		got, _ := argsAny[i].(string)
		if got != v {
			t.Errorf("args[%d] = %q, want %q", i, got, v)
		}
	}
}

// TestAider_AddEntry_RejectsMissingRelayFields ensures the adapter fails loudly
// when install.go forgets to populate the relay identifiers — a URL-only entry
// has no valid stdio representation in aider's schema.
func TestAider_AddEntry_RejectsMissingRelayFields(t *testing.T) {
	path := setupAiderConfig(t, "mcp-server: []\n")
	c := &aider{path: path}
	cases := []struct {
		name string
		e    MCPEntry
	}{
		{"no relay server", MCPEntry{Name: "x", URL: "http://x", RelayDaemon: "d", RelayExePath: filepath.Join(t.TempDir(), "mcphub.exe")}},
		{"no relay daemon", MCPEntry{Name: "x", URL: "http://x", RelayServer: "s", RelayExePath: filepath.Join(t.TempDir(), "mcphub.exe")}},
		{"no exe path", MCPEntry{Name: "x", URL: "http://x", RelayServer: "s", RelayDaemon: "d"}},
		{"relative exe path", MCPEntry{Name: "x", URL: "http://x", RelayServer: "s", RelayDaemon: "d", RelayExePath: "mcphub"}},
	}
	for _, tc := range cases {
		err := c.AddEntry(tc.e)
		if err == nil {
			t.Errorf("case %q: expected error, got nil", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "aider adapter requires") {
			t.Errorf("case %q: error should reference required fields: %v", tc.name, err)
		}
	}
}

// TestAider_AddEntry_ReplaceInPlace verifies a second AddEntry for the same
// name overwrites the existing list element rather than appending a duplicate,
// and preserves order + unrelated elements.
func TestAider_AddEntry_ReplaceInPlace(t *testing.T) {
	path := setupAiderConfig(t, `mcp-server:
  - name: serena
    command: uvx
    args: ['serena', 'start-mcp-server']
    env:
      FOO: bar
  - name: other
    command: echo
    args: ['hi']
`)
	c := &aider{path: path}
	exePath := filepath.Join(t.TempDir(), "mcphub.exe")
	if err := c.AddEntry(MCPEntry{Name: "serena", RelayServer: "serena", RelayDaemon: "claude", RelayExePath: exePath}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	list := readAiderList(t, path)
	if len(list) != 2 {
		t.Fatalf("expected 2 list elements (no duplicate), got %d: %+v", len(list), list)
	}
	serena := findAiderEntry(t, path, "serena")
	if cmd, _ := serena["command"].(string); cmd != exePath {
		t.Errorf("serena command not replaced: %+v", serena)
	}
	// Old stdio-era fields dropped (wholesale replace of the element body).
	if _, hasEnv := serena["env"]; hasEnv {
		t.Errorf("old env field not removed from replaced serena element: %+v", serena)
	}
	// Other element preserved.
	if findAiderEntry(t, path, "other") == nil {
		t.Error("unrelated 'other' element dropped")
	}
}

func TestAider_AddEntry_CreatesFromMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml") // does not exist yet
	c := &aider{path: path}
	exePath := filepath.Join(t.TempDir(), "mcphub.exe")
	if err := c.AddEntry(MCPEntry{Name: "time", RelayServer: "time", RelayDaemon: "claude", RelayExePath: exePath}); err != nil {
		t.Fatalf("AddEntry on missing file: %v", err)
	}
	if findAiderEntry(t, path, "time") == nil {
		t.Fatalf("time entry not written to a freshly-created file")
	}
}

func TestAider_RemoveEntry(t *testing.T) {
	path := setupAiderConfig(t, `mcp-server:
  - name: serena
    command: uvx
    args: ['serena']
  - name: memory
    command: npx
    args: ['-y', 'mem']
`)
	c := &aider{path: path}
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if findAiderEntry(t, path, "serena") != nil {
		t.Errorf("serena not removed")
	}
	if findAiderEntry(t, path, "memory") == nil {
		t.Error("memory also removed (should be preserved)")
	}
}

func TestAider_RemoveEntry_Idempotent(t *testing.T) {
	path := setupAiderConfig(t, "mcp-server: []\n")
	c := &aider{path: path}
	if err := c.RemoveEntry("nonexistent"); err != nil {
		t.Fatalf("RemoveEntry on absent entry should be nil: %v", err)
	}
}

func TestAider_GetEntry_ReconstructsRelayArgs(t *testing.T) {
	path := setupAiderConfig(t, `mcp-server:
  - name: serena
    command: D:\dev\mcp-local-hub\mcphub.exe
    args: ['relay', '--server', 'serena', '--daemon', 'claude']
`)
	c := &aider{path: path}
	e, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry returned nil")
	}
	if e.RelayServer != "serena" {
		t.Errorf("RelayServer = %q", e.RelayServer)
	}
	if e.RelayDaemon != "claude" {
		t.Errorf("RelayDaemon = %q", e.RelayDaemon)
	}
	if e.RelayExePath == "" {
		t.Error("RelayExePath should be populated from 'command' field")
	}
}

func TestAider_GetEntry_Absent(t *testing.T) {
	path := setupAiderConfig(t, "mcp-server: []\n")
	c := &aider{path: path}
	got, err := c.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for absent entry, got %+v", got)
	}
}

func TestAider_InitEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml")
	c := &aider{path: path}

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
	// mcp-server must be a LIST (sequence), not a map.
	if _, ok := m["mcp-server"].([]any); !ok {
		t.Errorf("stub mcp-server must be a list, got %T: %s", m["mcp-server"], data)
	}

	created2, err := c.InitEmpty()
	if err != nil {
		t.Fatalf("InitEmpty (2nd): %v", err)
	}
	if created2 {
		t.Error("expected created=false on second call")
	}
}

func TestAider_Backup_NotInstalled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml") // absent
	c := &aider{path: path}
	_, err := c.Backup()
	var notInstalled *ErrClientNotInstalled
	if !errors.As(err, &notInstalled) {
		t.Fatalf("expected ErrClientNotInstalled, got %v", err)
	}
}

func TestAider_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml")
	if err := os.WriteFile(path, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &aider{path: path}
	got, ok, err := c.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestAider_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml")
	// Live holds the hub-relay entry written by migrate.
	live := `mcp-server:
  - name: memory
    command: C:/mcphub.exe
    args: ['relay', '--server', 'memory', '--daemon', 'claude']
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	// Backup holds the user's pre-hub stdio entry.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	backupBody := `mcp-server:
  - name: memory
    command: npx
    args: ['-y', 'mem']
`
	if err := os.WriteFile(backup, []byte(backupBody), 0600); err != nil {
		t.Fatal(err)
	}
	c := &aider{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	mem := findAiderEntry(t, path, "memory")
	if mem == nil {
		t.Fatalf("memory entry missing after restore")
	}
	if cmd, _ := mem["command"].(string); cmd != "npx" {
		t.Errorf("expected restored stdio command npx, got %+v", mem)
	}
	args, _ := mem["args"].([]any)
	if len(args) == 0 || args[0] != "-y" {
		t.Errorf("hub-relay args should be gone after restore, got %+v", mem)
	}
}

func TestAider_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml")
	live := `mcp-server:
  - name: newserver
    command: C:/mcphub.exe
    args: ['relay', '--server', 'newserver', '--daemon', 'claude']
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte("mcp-server: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &aider{path: path}
	if err := c.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	if findAiderEntry(t, path, "newserver") != nil {
		t.Errorf("newserver should have been removed")
	}
}

func TestAider_RestoreEntryFromBackup_RefusesHubRelayBackupEntry(t *testing.T) {
	// Backup was taken AFTER an earlier migrate already rewrote this entry to
	// hub-relay form. Defensive refuse (aider's hub-managed form is relay,
	// like Antigravity — NOT a url entry).
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml")
	body := `mcp-server:
  - name: memory
    command: C:/mcphub.exe
    args: ['relay', '--server', 'memory', '--daemon', 'claude']
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c := &aider{path: path}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

func TestAider_RestoreEntryFromBackupForRollback_BypassesGuard(t *testing.T) {
	// Rollback variant writes the hub-relay backup entry verbatim despite the
	// demigrate guard.
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml")
	if err := os.WriteFile(path, []byte(`mcp-server:
  - name: serena
    command: C:/mcphub.exe
    args: ['relay', '--url', 'http://127.0.0.1:9300/serena/mcp']
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`mcp-server:
  - name: serena
    command: C:/mcphub.exe
    args: ['relay', '--server', 'serena', '--daemon', 'claude']
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &aider{path: path}
	if err := c.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
	}
	got, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.RelayServer != "serena" || got.RelayDaemon != "claude" {
		t.Errorf("expected verbatim legacy manifest-lookup relay entry after rollback, got %+v", got)
	}
}

func TestAider_BackupContainsEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml")
	backup := path + ".bak"
	if err := os.WriteFile(backup, []byte(`mcp-server:
  - name: present
    command: npx
    args: ['-y', 'pkg']
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &aider{path: path}

	has, err := c.BackupContainsEntry(backup, "present")
	if err != nil || !has {
		t.Errorf("BackupContainsEntry(present) = %v, %v; want true, nil", has, err)
	}
	missing, err := c.BackupContainsEntry(backup, "absent")
	if err != nil || missing {
		t.Errorf("BackupContainsEntry(absent) = %v, %v; want false, nil", missing, err)
	}
}

func TestAider_BackupEntryIsHubManaged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".aider.conf.yml")
	backup := path + ".bak"
	if err := os.WriteFile(backup, []byte(`mcp-server:
  - name: hubbed
    command: C:/mcphub.exe
    args: ['relay', '--server', 'hubbed', '--daemon', 'claude']
  - name: stdio
    command: npx
    args: ['-y', 'mem']
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &aider{path: path}

	cases := []struct {
		name string
		want bool
	}{
		{"hubbed", true},  // relay shape: mcphub command + args[0]==relay
		{"stdio", false},  // user stdio entry (other command)
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

func TestAider_AllStdioEntries(t *testing.T) {
	path := setupAiderConfig(t, `mcp-server:
  - name: stdio1
    command: npx
    args: ['-y', 'pkg']
  - name: disabledStdio
    command: echo
    args: ['x']
    disabled: true
`)
	c := &aider{path: path}
	entries, err := c.AllStdioEntries()
	if err != nil {
		t.Fatalf("AllStdioEntries: %v", err)
	}
	// Only stdio1 qualifies: disabledStdio is disabled.
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

func TestAider_FindStdioLanguageServerEntries(t *testing.T) {
	path := setupAiderConfig(t, `mcp-server:
  - name: go-ls
    command: mcp-language-server
    args: ['--lsp', 'gopls']
  - name: notls
    command: npx
    args: ['-y', 'mem']
`)
	c := &aider{path: path}
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

func TestAider_PreservesUnknownTopLevelKeys(t *testing.T) {
	// .aider.conf.yml holds many non-MCP settings; AddEntry/RemoveEntry must
	// not clobber them.
	path := setupAiderConfig(t, `model: claude-sonnet-4-6
auto-commits: true
mcp-server:
  - name: existing
    command: npx
    args: ['-y', 'pkg']
`)
	c := &aider{path: path}
	exePath := filepath.Join(t.TempDir(), "mcphub.exe")
	if err := c.AddEntry(MCPEntry{Name: "new", RelayServer: "new", RelayDaemon: "claude", RelayExePath: exePath}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	data, _ := os.ReadFile(path)
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if m["model"] != "claude-sonnet-4-6" {
		t.Errorf("top-level model key clobbered: %+v", m["model"])
	}
	if _, ok := m["auto-commits"]; !ok {
		t.Error("top-level auto-commits key dropped")
	}
	// Both the existing and new entries present.
	if findAiderEntry(t, path, "existing") == nil {
		t.Error("existing entry dropped")
	}
	if findAiderEntry(t, path, "new") == nil {
		t.Error("new entry missing")
	}
}

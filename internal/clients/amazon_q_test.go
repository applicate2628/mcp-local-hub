package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func setupAmazonQConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAmazonQ_NameAndRelay(t *testing.T) {
	c := &amazonQ{path: filepath.Join(t.TempDir(), "mcp.json")}
	if c.Name() != "amazon-q" {
		t.Errorf("Name() = %q, want amazon-q", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (amazon-q is URL-native HTTP)")
	}
}

func TestAmazonQ_AddEntry_CreatesFieldWithHTTPType(t *testing.T) {
	path := setupAmazonQConfig(t, `{"other":"field"}`)
	c := &amazonQ{path: path}

	err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong type: %v", parsed["mcpServers"])
	}
	serena, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if serena["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want http://localhost:9121/mcp", serena["url"])
	}
	// Amazon Q's documented remote shape requires "type":"http" alongside url.
	if serena["type"] != "http" {
		t.Errorf("type = %v, want http (Amazon Q remote MCP server requires type+url)", serena["type"])
	}
	// Original field preserved.
	if parsed["other"] != "field" {
		t.Error("original field dropped")
	}
}

func TestAmazonQ_AddEntry_WithHeaders(t *testing.T) {
	path := setupAmazonQConfig(t, `{}`)
	c := &amazonQ{path: path}
	if err := c.AddEntry(MCPEntry{
		Name:    "remote",
		URL:     "https://api.example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer xyz"},
	}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	entry := parsed["mcpServers"].(map[string]any)["remote"].(map[string]any)
	hdrs, ok := entry["headers"].(map[string]any)
	if !ok || hdrs["Authorization"] != "Bearer xyz" {
		t.Errorf("headers not written: %v", entry["headers"])
	}
}

func TestAmazonQ_AddEntry_Replaces(t *testing.T) {
	path := setupAmazonQConfig(t, `{"mcpServers":{"serena":{"type":"http","url":"http://old"}}}`)
	c := &amazonQ{path: path}
	_ = c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})

	entry, _ := c.GetEntry("serena")
	if entry == nil || entry.URL != "http://localhost:9121/mcp" {
		t.Errorf("entry not replaced: %v", entry)
	}
}

func TestAmazonQ_RemoveEntry(t *testing.T) {
	path := setupAmazonQConfig(t, `{"mcpServers":{"serena":{"url":"http://x"},"other":{"url":"http://y"}}}`)
	c := &amazonQ{path: path}
	_ = c.RemoveEntry("serena")

	entry, _ := c.GetEntry("serena")
	if entry != nil {
		t.Errorf("serena still present: %v", entry)
	}
	other, _ := c.GetEntry("other")
	if other == nil {
		t.Error("other entry should still be present")
	}
}

func TestAmazonQ_RemoveEntry_AbsentIsNoOp(t *testing.T) {
	path := setupAmazonQConfig(t, `{"mcpServers":{}}`)
	c := &amazonQ{path: path}
	if err := c.RemoveEntry("nope"); err != nil {
		t.Errorf("RemoveEntry on absent entry should be no-op, got %v", err)
	}
}

func TestAmazonQ_GetEntry_Absent(t *testing.T) {
	path := setupAmazonQConfig(t, `{"mcpServers":{}}`)
	c := &amazonQ{path: path}
	entry, err := c.GetEntry("missing")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if entry != nil {
		t.Errorf("GetEntry on absent = %v, want nil", entry)
	}
}

func TestAmazonQ_BackupRestore(t *testing.T) {
	original := `{"mcpServers":{"serena":{"type":"http","url":"http://old"}}}`
	path := setupAmazonQConfig(t, original)
	c := &amazonQ{path: path}

	bak, err := c.Backup()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// Mutate live file.
	_ = c.AddEntry(MCPEntry{Name: "serena", URL: "http://new"})
	// Restore.
	if err := c.Restore(bak); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("after restore = %q, want %q", got, original)
	}
}

// TestAmazonQ_Backup_FreshHost verifies the nested-directory bootstrap: on a
// clean host the ~/.aws/amazonq parent does not exist, so Backup must MkdirAll
// it + seed an empty stub before backing up (mirrors the kiro adapter).
func TestAmazonQ_Backup_FreshHost(t *testing.T) {
	dir := t.TempDir()
	// Path lives two levels deep, neither level present yet.
	path := filepath.Join(dir, ".aws", "amazonq", "mcp.json")
	c := &amazonQ{path: path}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("precondition: parent dir should not exist, stat err = %v", err)
	}
	bak, err := c.Backup()
	if err != nil {
		t.Fatalf("Backup on fresh host: %v", err)
	}
	if _, err := os.Stat(bak); err != nil {
		t.Errorf("backup file not written: %v", err)
	}
	// The live stub must now exist with an empty mcpServers map.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("seeded config not present: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("seeded config parse: %v", err)
	}
	if _, ok := parsed["mcpServers"].(map[string]any); !ok {
		t.Errorf("seeded stub missing mcpServers: %v", parsed)
	}
}

// TestAmazonQ_Exists_DirectoryHeuristic verifies Exists reports true when the
// parent dir exists even without the config file (the "installed but no MCP
// config yet" affordance).
func TestAmazonQ_Exists_DirectoryHeuristic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".aws", "amazonq", "mcp.json")
	c := &amazonQ{path: path}
	if c.Exists() {
		t.Error("Exists() = true before parent dir created")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if !c.Exists() {
		t.Error("Exists() = false although parent dir exists (directory heuristic)")
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !c.Exists() {
		t.Error("Exists() = false although config file exists")
	}
}

func TestAmazonQ_InitEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".aws", "amazonq"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".aws", "amazonq", "mcp.json")
	c := &amazonQ{path: path}
	created, err := c.InitEmpty()
	if err != nil || !created {
		t.Fatalf("InitEmpty: created=%v err=%v", created, err)
	}
	// Second call is idempotent: file already exists.
	created, err = c.InitEmpty()
	if err != nil || created {
		t.Fatalf("InitEmpty second call: created=%v err=%v (want created=false)", created, err)
	}
}

func TestAmazonQ_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &amazonQ{path: path}
	got, ok, err := c.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestAmazonQ_RestoreEntryFromBackup_RestoresStdioShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	// Live config is in post-migrate hub-HTTP state.
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9123/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Backup has pre-migrate stdio.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &amazonQ{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["memory"].(map[string]any)
	if entry["command"] != "npx" {
		t.Errorf("command=%v, want npx", entry["command"])
	}
}

func TestAmazonQ_RestoreEntryFromBackup_RemovesEntryIfBackupLacksIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"newserver":{"type":"http","url":"x"},"memory":{"type":"http","url":"y"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &amazonQ{path: path}
	if err := c.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	servers := m["mcpServers"].(map[string]any)
	if _, present := servers["newserver"]; present {
		t.Error("newserver should have been removed — backup predates it")
	}
	if _, present := servers["memory"]; !present {
		t.Error("memory was touched but should be untouched — call targeted newserver")
	}
}

func TestAmazonQ_RestoreEntryFromBackup_AcceptsRemoteHTTPBackup(t *testing.T) {
	// A legit remote HTTP MCP server (non-loopback URL) must NOT be rejected
	// as hub-managed — the defensive check only fires on loopback urls.
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"remote":{"type":"http","url":"https://api.example.com/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &amazonQ{path: path}
	if err := c.RestoreEntryFromBackup(backup, "remote"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v (remote HTTP url must not be rejected as hub-managed)", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["remote"].(map[string]any)
	if entry["url"] != "https://api.example.com/mcp" {
		t.Errorf("url=%v, want https://api.example.com/mcp", entry["url"])
	}
}

func TestAmazonQ_RestoreEntryFromBackup_RefusesHubHTTPBackupEntry(t *testing.T) {
	// Backup was taken AFTER an earlier migrate already rewrote this entry to
	// hub-HTTP (loopback) form. Restoring would silently re-write the hub
	// entry — defensive refuse.
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &amazonQ{path: path}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

func TestAmazonQ_RestoreEntryFromBackupForRollback_AllowsHubEntry(t *testing.T) {
	// The rollback variant must restore a loopback hub-HTTP backup entry
	// verbatim, bypassing the demigrate guard.
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"serena":{"type":"http","url":"http://127.0.0.1:9999/serena/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{"mcpServers":{"serena":{"type":"http","url":"http://localhost:9121/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &amazonQ{path: path}
	if err := c.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
	}
	entry, _ := c.GetEntry("serena")
	if entry == nil || entry.URL != "http://localhost:9121/mcp" {
		t.Errorf("rollback did not restore loopback hub entry verbatim: %v", entry)
	}
}

func TestAmazonQ_AllStdioEntries(t *testing.T) {
	path := setupAmazonQConfig(t, `{"mcpServers":{
		"local":{"command":"npx","args":["-y","srv"]},
		"remote":{"type":"http","url":"http://localhost:9000/mcp"}
	}}`)
	c := &amazonQ{path: path}
	entries, err := c.AllStdioEntries()
	if err != nil {
		t.Fatalf("AllStdioEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d stdio entries, want 1 (HTTP entry must be skipped): %v", len(entries), entries)
	}
	if entries[0].Name != "local" || entries[0].Command != "npx" {
		t.Errorf("stdio entry = %+v, want local/npx", entries[0])
	}
}

func TestAmazonQ_FindStdioLanguageServerEntries(t *testing.T) {
	path := setupAmazonQConfig(t, `{"mcpServers":{
		"go-ls":{"command":"mcp-language-server","args":["--lsp","gopls"]},
		"remote":{"type":"http","url":"http://localhost:9000/mcp"}
	}}`)
	c := &amazonQ{path: path}
	entries, err := c.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Language != "gopls" {
		t.Fatalf("got %v, want one entry with language gopls", entries)
	}
}

func TestAmazonQ_BackupContainsEntry_RejectsNonObjectValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	badBackup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(badBackup, []byte(
		`{"mcpServers":{"memory":"bad","other":null}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &amazonQ{path: path}
	for _, name := range []string{"memory", "other"} {
		has, err := c.BackupContainsEntry(badBackup, name)
		if err != nil {
			t.Errorf("%s: unexpected err: %v", name, err)
		}
		if has {
			t.Errorf("%s: expected BackupContainsEntry=false for non-object value", name)
		}
	}
}

func TestAmazonQ_BackupEntryIsHubManaged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"hub":{"type":"http","url":"http://localhost:9123/mcp"},"remote":{"type":"http","url":"https://api.example.com/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &amazonQ{path: path}
	managed, err := c.BackupEntryIsHubManaged(backup, "hub")
	if err != nil || !managed {
		t.Errorf("hub: managed=%v err=%v, want true", managed, err)
	}
	managed, err = c.BackupEntryIsHubManaged(backup, "remote")
	if err != nil || managed {
		t.Errorf("remote: managed=%v err=%v, want false (non-loopback)", managed, err)
	}
}

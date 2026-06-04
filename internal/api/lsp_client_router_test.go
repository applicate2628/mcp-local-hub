package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

type lspRouterFakeClient struct {
	name        string
	path        string
	exists      bool
	entries     map[string]clients.MCPEntry
	snapshots   map[string]map[string]clients.MCPEntry
	backupPaths []string
	addErr      error
	addCalls    int
	removeCalls int
}

func newLSPRouterFakeClient(t *testing.T, name string, exists bool) *lspRouterFakeClient {
	t.Helper()
	return &lspRouterFakeClient{
		name:      name,
		path:      filepath.Join(t.TempDir(), name+".json"),
		exists:    exists,
		entries:   map[string]clients.MCPEntry{},
		snapshots: map[string]map[string]clients.MCPEntry{},
	}
}

func (f *lspRouterFakeClient) Name() string       { return f.name }
func (f *lspRouterFakeClient) ConfigPath() string { return f.path }
func (f *lspRouterFakeClient) Exists() bool       { return f.exists }
func (f *lspRouterFakeClient) InitEmpty() (bool, error) {
	return false, nil
}
func (f *lspRouterFakeClient) Backup() (string, error) { return f.BackupKeep(0) }
func (f *lspRouterFakeClient) BackupKeep(int) (string, error) {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return "", err
	}
	backupPath := fmt.Sprintf("%s.bak-mcp-local-hub-test-%02d", f.path, len(f.backupPaths)+1)
	if err := os.WriteFile(backupPath, []byte("backup\n"), 0o600); err != nil {
		return "", err
	}
	snapshot := map[string]clients.MCPEntry{}
	for name, entry := range f.entries {
		snapshot[name] = entry
	}
	f.snapshots[backupPath] = snapshot
	f.backupPaths = append(f.backupPaths, backupPath)
	return backupPath, nil
}
func (f *lspRouterFakeClient) Restore(string) error { return nil }
func (f *lspRouterFakeClient) AddEntry(e clients.MCPEntry) error {
	f.addCalls++
	if f.addErr != nil {
		return f.addErr
	}
	f.entries[e.Name] = e
	return nil
}
func (f *lspRouterFakeClient) RemoveEntry(name string) error {
	f.removeCalls++
	delete(f.entries, name)
	return nil
}
func (f *lspRouterFakeClient) GetEntry(name string) (*clients.MCPEntry, error) {
	entry, ok := f.entries[name]
	if !ok {
		return nil, nil
	}
	cp := entry
	return &cp, nil
}
func (f *lspRouterFakeClient) LatestBackupPath() (string, bool, error) {
	if len(f.backupPaths) == 0 {
		return "", false, nil
	}
	return f.backupPaths[len(f.backupPaths)-1], true, nil
}
func (f *lspRouterFakeClient) RestoreEntryFromBackup(backupPath, name string) error {
	return f.restoreEntryFromSnapshot(backupPath, name)
}
func (f *lspRouterFakeClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return f.restoreEntryFromSnapshot(backupPath, name)
}
func (f *lspRouterFakeClient) restoreEntryFromSnapshot(backupPath, name string) error {
	snapshot, ok := f.snapshots[backupPath]
	if !ok {
		return fmt.Errorf("unknown backup %s", backupPath)
	}
	entry, present := snapshot[name]
	if !present {
		delete(f.entries, name)
		return nil
	}
	f.entries[name] = entry
	return nil
}
func (f *lspRouterFakeClient) BackupContainsEntry(backupPath, name string) (bool, error) {
	snapshot, ok := f.snapshots[backupPath]
	if !ok {
		return false, fmt.Errorf("unknown backup %s", backupPath)
	}
	_, present := snapshot[name]
	return present, nil
}
func (f *lspRouterFakeClient) BackupEntryIsHubManaged(string, string) (bool, error) {
	return false, nil
}
func (f *lspRouterFakeClient) AllStdioEntries() ([]clients.StdioEntry, error) {
	return nil, nil
}
func (f *lspRouterFakeClient) FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error) {
	return nil, nil
}

var _ clients.Client = (*lspRouterFakeClient)(nil)

func TestEnsureLSPRouterClientEntries_WiresManifestLanguagesToPresentClientsAndIsIdempotent(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go", "python"})

	claude := newLSPRouterFakeClient(t, "claude-code", true)
	codex := newLSPRouterFakeClient(t, "codex-cli", true)
	missing := newLSPRouterFakeClient(t, "cursor", false)
	clientMap := map[string]clients.Client{
		"claude-code": claude,
		"codex-cli":   codex,
		"cursor":      missing,
	}

	report, err := NewAPI().EnsureLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 7777,
		Clients: clientMap,
	})
	if err != nil {
		t.Fatalf("EnsureLSPRouterClientEntries: %v", err)
	}
	if len(report.Backups) != 2 {
		t.Fatalf("Backups len = %d, want 2 present clients", len(report.Backups))
	}

	for _, client := range []*lspRouterFakeClient{claude, codex} {
		for _, lang := range []string{"go", "python"} {
			name := "mcp-language-server-" + lang
			got, err := client.GetEntry(name)
			if err != nil {
				t.Fatalf("%s GetEntry(%s): %v", client.name, name, err)
			}
			wantURL := "http://localhost:7777/lsp/" + lang + "/mcp"
			if got == nil || got.URL != wantURL {
				t.Fatalf("%s %s URL = %+v, want %s", client.name, name, got, wantURL)
			}
		}
		if len(client.backupPaths) != 1 {
			t.Fatalf("%s backup count = %d, want 1", client.name, len(client.backupPaths))
		}
	}
	if len(missing.entries) != 0 || len(missing.backupPaths) != 0 {
		t.Fatalf("missing client was mutated: entries=%v backups=%v", missing.entries, missing.backupPaths)
	}

	second, err := NewAPI().EnsureLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 7777,
		Clients: clientMap,
	})
	if err != nil {
		t.Fatalf("second EnsureLSPRouterClientEntries: %v", err)
	}
	if len(second.Backups) != 0 || len(second.Applied) != 0 || len(second.Removed) != 0 {
		t.Fatalf("idempotent rerun changed config: %+v", second)
	}
	if len(claude.backupPaths) != 1 || len(codex.backupPaths) != 1 {
		t.Fatalf("idempotent rerun wrote backups: claude=%d codex=%d", len(claude.backupPaths), len(codex.backupPaths))
	}
}

func TestEnsureLSPRouterClientEntries_MigratesPerPairEntryToRouterAndKeepsRegistry(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	restoreRegistry := overrideLSPRouterRegistry(t)
	defer restoreRegistry()

	reg := NewRegistry(testRegistryPathOverride)
	err := reg.PutLSP(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "D:/dev/project",
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9200,
		TaskName:      "mcp-local-hub-lsp-abcd1234-go",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-go"},
	})
	if err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}

	codex := newLSPRouterFakeClient(t, "codex-cli", true)
	codex.entries["mcp-language-server-go"] = clients.MCPEntry{
		Name: "mcp-language-server-go",
		URL:  "http://localhost:9200/mcp",
	}

	report, err := NewAPI().EnsureLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 9126,
		Clients: map[string]clients.Client{"codex-cli": codex},
	})
	if err != nil {
		t.Fatalf("EnsureLSPRouterClientEntries: %v", err)
	}
	if len(report.Backups) != 1 {
		t.Fatalf("Backups len = %d, want 1", len(report.Backups))
	}
	if _, err := os.Stat(report.Backups[0].Path); err != nil {
		t.Fatalf("backup file was not created at %s: %v", report.Backups[0].Path, err)
	}
	got := codex.entries["mcp-language-server-go"]
	if got.URL != "http://localhost:9126/lsp/go/mcp" {
		t.Fatalf("migrated URL = %q, want router URL", got.URL)
	}

	after := NewRegistry(testRegistryPathOverride)
	if err := after.Load(); err != nil {
		t.Fatalf("Load registry after migration: %v", err)
	}
	kept, ok := after.Get("abcd1234", "go")
	if !ok || kept.Port != 9200 {
		t.Fatalf("registry row was not preserved: ok=%v row=%+v", ok, kept)
	}
}

func TestRollbackLSPRouterClientEntries_RestoresPerPairEntryFromBackup(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	restoreRegistry := overrideLSPRouterRegistry(t)
	defer restoreRegistry()

	reg := NewRegistry(testRegistryPathOverride)
	if err := reg.PutLSP(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "D:/dev/project",
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9200,
		TaskName:      "mcp-local-hub-lsp-abcd1234-go",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-go"},
	}); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}

	codex := newLSPRouterFakeClient(t, "codex-cli", true)
	codex.entries["mcp-language-server-go"] = clients.MCPEntry{
		Name: "mcp-language-server-go",
		URL:  "http://localhost:9200/mcp",
	}
	opts := LSPClientRouterOpts{
		GUIPort: 9126,
		Clients: map[string]clients.Client{"codex-cli": codex},
	}
	if _, err := NewAPI().EnsureLSPRouterClientEntries(opts); err != nil {
		t.Fatalf("EnsureLSPRouterClientEntries: %v", err)
	}
	if got := codex.entries["mcp-language-server-go"].URL; got != "http://localhost:9126/lsp/go/mcp" {
		t.Fatalf("setup precondition URL = %q", got)
	}

	report, err := NewAPI().RollbackLSPRouterClientEntries(opts)
	if err != nil {
		t.Fatalf("RollbackLSPRouterClientEntries: %v", err)
	}
	if len(report.Backups) != 1 {
		t.Fatalf("rollback backups len = %d, want 1 current-state backup", len(report.Backups))
	}
	if got := codex.entries["mcp-language-server-go"].URL; got != "http://localhost:9200/mcp" {
		t.Fatalf("rollback URL = %q, want restored per-pair URL", got)
	}
	if len(codex.backupPaths) != 2 {
		t.Fatalf("total backups = %d, want setup backup + rollback backup", len(codex.backupPaths))
	}
}

func TestRollbackLSPRouterClientEntries_ReconstructsPerPairEntryInsteadOfLatestRouterBackup(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	restoreRegistry := overrideLSPRouterRegistry(t)
	defer restoreRegistry()

	reg := NewRegistry(testRegistryPathOverride)
	if err := reg.PutLSP(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "D:/dev/project",
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9200,
		TaskName:      "mcp-local-hub-lsp-abcd1234-go",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-go"},
	}); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}

	codex := newLSPRouterFakeClient(t, "codex-cli", true)
	codex.entries["mcp-language-server-go"] = clients.MCPEntry{
		Name: "mcp-language-server-go",
		URL:  "http://localhost:9200/mcp",
	}
	opts := LSPClientRouterOpts{
		GUIPort: 9126,
		Clients: map[string]clients.Client{"codex-cli": codex},
	}
	if _, err := NewAPI().EnsureLSPRouterClientEntries(opts); err != nil {
		t.Fatalf("EnsureLSPRouterClientEntries: %v", err)
	}
	if got := codex.entries["mcp-language-server-go"].URL; got != "http://localhost:9126/lsp/go/mcp" {
		t.Fatalf("setup precondition URL = %q", got)
	}
	if _, err := codex.BackupKeep(0); err != nil {
		t.Fatalf("newer router-containing backup: %v", err)
	}

	report, err := NewAPI().RollbackLSPRouterClientEntries(opts)
	if err != nil {
		t.Fatalf("RollbackLSPRouterClientEntries: %v", err)
	}
	if len(report.Backups) != 1 {
		t.Fatalf("rollback backups len = %d, want 1 current-state backup", len(report.Backups))
	}
	if got := codex.entries["mcp-language-server-go"].URL; got != "http://localhost:9200/mcp" {
		t.Fatalf("rollback restored latest router-containing backup; URL = %q, want reconstructed per-pair URL", got)
	}
	if len(codex.backupPaths) != 3 {
		t.Fatalf("total backups = %d, want setup + newer + rollback backup", len(codex.backupPaths))
	}
}

func TestEnsureLSPRouterClientEntries_AddFailureSkipsLegacyRemove(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	restoreRegistry := overrideLSPRouterRegistry(t)
	defer restoreRegistry()

	reg := NewRegistry(testRegistryPathOverride)
	if err := reg.PutLSP(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "D:/dev/project",
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9200,
		TaskName:      "mcp-local-hub-lsp-abcd1234-go",
		ClientEntries: map[string]string{"codex-cli": "mcp-language-server-go-abcd"},
	}); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}

	codex := newLSPRouterFakeClient(t, "codex-cli", true)
	codex.entries["mcp-language-server-go-abcd"] = clients.MCPEntry{
		Name: "mcp-language-server-go-abcd",
		URL:  "http://localhost:9200/mcp",
	}
	codex.addErr = errors.New("induced add failure")

	report, err := NewAPI().EnsureLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 9126,
		Clients: map[string]clients.Client{"codex-cli": codex},
	})
	if err == nil {
		t.Fatal("expected router add failure")
	}
	if len(report.Failed) != 1 || report.Failed[0].Op != "add" || report.Failed[0].EntryName != "mcp-language-server-go" {
		t.Fatalf("Failed = %+v, want one router add failure", report.Failed)
	}
	if _, ok := codex.entries["mcp-language-server-go-abcd"]; !ok {
		t.Fatalf("legacy entry was removed after router add failed; entries=%+v", codex.entries)
	}
	if got := codex.removeCalls; got != 0 {
		t.Fatalf("RemoveEntry calls = %d, want 0 after add failure", got)
	}
}

func seedLSPRouterManifest(t *testing.T, languages []string) {
	t.Helper()
	dir := t.TempDir()
	serverDir := filepath.Join(dir, "mcp-language-server")
	if err := os.MkdirAll(serverDir, 0o700); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	body := "name: mcp-language-server\n" +
		"kind: workspace-scoped\n" +
		"transport: stdio-bridge\n" +
		"command: mcp-language-server\n" +
		"port_pool:\n  start: 9200\n  end: 9299\n" +
		"weekly_refresh: false\n" +
		"languages:\n"
	for _, lang := range languages {
		backend := "mcp-language-server"
		command := lang + "-language-server"
		marker := lang + ".marker"
		if lang == "go" {
			backend = "gopls-mcp"
			command = "gopls"
			marker = "go.mod"
		}
		body += fmt.Sprintf(
			"  - name: %s\n    backend: %s\n    transport: stdio\n    lsp_command: %s\n    project_markers: [%s]\n    required_binaries: [%s]\n",
			lang, backend, command, marker, command,
		)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
}

func overrideLSPRouterRegistry(t *testing.T) func() {
	t.Helper()
	prior := testRegistryPathOverride
	testRegistryPathOverride = filepath.Join(t.TempDir(), "workspaces.yaml")
	return func() { testRegistryPathOverride = prior }
}

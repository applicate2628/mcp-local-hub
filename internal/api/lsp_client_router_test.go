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
	name                string
	path                string
	exists              bool
	entries             map[string]clients.MCPEntry
	snapshots           map[string]map[string]clients.MCPEntry
	backupPaths         []string
	addErr              error
	addAppliesOnErr     bool
	removeErr           error
	removeAppliesOnErr  bool
	restoreErr          error
	restoreAppliesOnErr bool
	addCalls            int
	removeCalls         int
	restoreCalls        int
	getCalls            int
	latestBackupErr     error
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

// IsRelayStdio mirrors the real per-name relay-stdio classification so a
// fake named "antigravity"/"zed" behaves like the real relay-stdio adapter
// (true) and every URL-native fake reports false.
func (f *lspRouterFakeClient) IsRelayStdio() bool { return clients.IsRelayStdio(f.name) }
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
		if f.addAppliesOnErr {
			f.entries[e.Name] = e
		}
		return f.addErr
	}
	f.entries[e.Name] = e
	return nil
}
func (f *lspRouterFakeClient) RemoveEntry(name string) error {
	f.removeCalls++
	if f.removeErr != nil {
		if f.removeAppliesOnErr {
			delete(f.entries, name)
		}
		return f.removeErr
	}
	delete(f.entries, name)
	return nil
}
func (f *lspRouterFakeClient) GetEntry(name string) (*clients.MCPEntry, error) {
	f.getCalls++
	entry, ok := f.entries[name]
	if !ok {
		return nil, nil
	}
	cp := entry
	return &cp, nil
}
func (f *lspRouterFakeClient) LatestBackupPath() (string, bool, error) {
	if f.latestBackupErr != nil {
		return "", false, f.latestBackupErr
	}
	if len(f.backupPaths) == 0 {
		return "", false, nil
	}
	return f.backupPaths[len(f.backupPaths)-1], true, nil
}
func (f *lspRouterFakeClient) RestoreEntryFromBackup(backupPath, name string) error {
	f.restoreCalls++
	if f.restoreErr != nil {
		if f.restoreAppliesOnErr {
			_ = f.restoreEntryFromSnapshot(backupPath, name)
		}
		return f.restoreErr
	}
	return f.restoreEntryFromSnapshot(backupPath, name)
}
func (f *lspRouterFakeClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	f.restoreCalls++
	if f.restoreErr != nil {
		if f.restoreAppliesOnErr {
			_ = f.restoreEntryFromSnapshot(backupPath, name)
		}
		return f.restoreErr
	}
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
func (f *lspRouterFakeClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	snapshot, ok := f.snapshots[backupPath]
	if !ok {
		return false, fmt.Errorf("unknown backup %s", backupPath)
	}
	entry, present := snapshot[name]
	if !present {
		return false, nil
	}
	for _, raw := range []string{entry.URL, entry.RelayURL} {
		if _, ok := lspRouterURLLanguage(raw); ok {
			return true, nil
		}
	}
	return false, nil
}
func (f *lspRouterFakeClient) AllStdioEntries() ([]clients.StdioEntry, error) {
	return nil, nil
}
func (f *lspRouterFakeClient) FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error) {
	return nil, nil
}

var _ clients.Client = (*lspRouterFakeClient)(nil)

func TestLSPRouterURLUsesIPv4Loopback(t *testing.T) {
	got := LSPRouterURL(7777, "go")
	if got != "http://127.0.0.1:7777/lsp/go/mcp" {
		t.Fatalf("LSPRouterURL = %q, want IPv4 loopback URL", got)
	}
}

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
			wantURL := "http://127.0.0.1:7777/lsp/" + lang + "/mcp"
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

func TestEnsureLSPRouterClientEntries_HonorsEffectiveClientEnablement(t *testing.T) {
	t.Run("default install set writes but fresh opt-in client without evidence is skipped", func(t *testing.T) {
		seedLSPRouterManifest(t, []string{"go"})
		if err := NewAPI().SetDefaultInstallClientNames([]string{"claude-code"}); err != nil {
			t.Fatalf("set default-install clients: %v", err)
		}

		defaultClient := newLSPRouterFakeClient(t, "claude-code", true)
		freshOptIn := newLSPRouterFakeClient(t, "antigravity", true)
		clientMap := map[string]clients.Client{
			"claude-code": defaultClient,
			"antigravity": freshOptIn,
		}
		opts := LSPClientRouterOpts{
			GUIPort:       7777,
			Clients:       clientMap,
			McphubExePath: filepath.Join(t.TempDir(), "mcphub.exe"),
		}

		report, err := NewAPI().EnsureLSPRouterClientEntries(opts)
		if err != nil {
			t.Fatalf("EnsureLSPRouterClientEntries: %v", err)
		}
		name := LSPRouterEntryName("go")
		if got, err := defaultClient.GetEntry(name); err != nil || got == nil || got.URL != LSPRouterURL(7777, "go") {
			t.Fatalf("default client entry = %+v err=%v, want router URL", got, err)
		}
		if got, err := freshOptIn.GetEntry(name); err != nil || got != nil {
			t.Fatalf("fresh opt-in entry = %+v err=%v, want no router entry", got, err)
		}
		if len(report.Backups) != 1 || report.Backups[0].Client != "claude-code" {
			t.Fatalf("Backups = %+v, want only default-install client", report.Backups)
		}
		if freshOptIn.addCalls != 0 || len(freshOptIn.backupPaths) != 0 {
			t.Fatalf("fresh opt-in client was mutated: addCalls=%d backups=%v entries=%v", freshOptIn.addCalls, freshOptIn.backupPaths, freshOptIn.entries)
		}

		second, err := NewAPI().EnsureLSPRouterClientEntries(opts)
		if err != nil {
			t.Fatalf("second EnsureLSPRouterClientEntries: %v", err)
		}
		if len(second.Backups) != 0 || len(second.Applied) != 0 || len(second.Removed) != 0 {
			t.Fatalf("idempotent rerun changed config: %+v", second)
		}
		if len(defaultClient.backupPaths) != 1 || len(freshOptIn.backupPaths) != 0 {
			t.Fatalf("idempotent rerun wrote unexpected backups: default=%d fresh=%d", len(defaultClient.backupPaths), len(freshOptIn.backupPaths))
		}
	})

	t.Run("explicit install evidence keeps opt-in client eligible", func(t *testing.T) {
		seedLSPRouterManifest(t, []string{"go"})
		seedManifestWithClientBinding(t, "memory", "antigravity", 9123)
		if err := NewAPI().SetDefaultInstallClientNames([]string{"claude-code"}); err != nil {
			t.Fatalf("set default-install clients: %v", err)
		}

		explicitClient := newLSPRouterFakeClient(t, "antigravity", true)
		explicitClient.entries["memory"] = clients.MCPEntry{
			Name:         "memory",
			RelayServer:  "memory",
			RelayDaemon:  "default",
			RelayExePath: filepath.Join(t.TempDir(), "mcphub.exe"),
		}
		opts := LSPClientRouterOpts{
			GUIPort:       7777,
			Clients:       map[string]clients.Client{"antigravity": explicitClient},
			McphubExePath: filepath.Join(t.TempDir(), "mcphub.exe"),
		}

		report, err := NewAPI().EnsureLSPRouterClientEntries(opts)
		if err != nil {
			t.Fatalf("EnsureLSPRouterClientEntries: %v", err)
		}
		name := LSPRouterEntryName("go")
		if got, err := explicitClient.GetEntry(name); err != nil || got == nil || got.RelayURL != LSPRouterURL(7777, "go") {
			t.Fatalf("explicit client router entry = %+v err=%v, want relay router URL", got, err)
		}
		if len(report.Backups) != 1 || report.Backups[0].Client != "antigravity" {
			t.Fatalf("Backups = %+v, want explicit Antigravity backup", report.Backups)
		}
	})

	t.Run("disabled hub evidence does not opt in client", func(t *testing.T) {
		seedLSPRouterManifest(t, []string{"go", "python"})
		seedManifestWithClientBinding(t, "memory", "antigravity", 9123)
		if err := NewAPI().SetDefaultInstallClientNames([]string{"claude-code"}); err != nil {
			t.Fatalf("set default-install clients: %v", err)
		}

		disabledOnly := newLSPRouterFakeClient(t, "antigravity", true)
		mcphub := filepath.Join(t.TempDir(), "mcphub.exe")
		disabledOnly.entries[LSPRouterEntryName("go")] = clients.MCPEntry{
			Name:         LSPRouterEntryName("go"),
			RelayURL:     LSPRouterURL(7777, "go"),
			RelayExePath: mcphub,
			Disabled:     true,
		}
		disabledOnly.entries["memory"] = clients.MCPEntry{
			Name:         "memory",
			RelayServer:  "memory",
			RelayDaemon:  "default",
			RelayExePath: mcphub,
			Disabled:     true,
		}
		opts := LSPClientRouterOpts{
			GUIPort:       7777,
			Clients:       map[string]clients.Client{"antigravity": disabledOnly},
			McphubExePath: mcphub,
		}

		report, err := NewAPI().EnsureLSPRouterClientEntries(opts)
		if err != nil {
			t.Fatalf("EnsureLSPRouterClientEntries: %v", err)
		}
		if len(report.Backups) != 0 || len(report.Applied) != 0 || len(report.Removed) != 0 {
			t.Fatalf("disabled-only evidence mutated config: %+v", report)
		}
		if got, err := disabledOnly.GetEntry(LSPRouterEntryName("python")); err != nil || got != nil {
			t.Fatalf("disabled-only evidence added python entry = %+v err=%v, want nil", got, err)
		}
		if disabledOnly.addCalls != 0 || len(disabledOnly.backupPaths) != 0 {
			t.Fatalf("disabled-only client was mutated: addCalls=%d backups=%v entries=%v", disabledOnly.addCalls, disabledOnly.backupPaths, disabledOnly.entries)
		}
	})

	t.Run("pre-existing router entry keeps upgrade client eligible", func(t *testing.T) {
		seedLSPRouterManifest(t, []string{"go", "python"})
		if err := NewAPI().SetDefaultInstallClientNames([]string{"claude-code"}); err != nil {
			t.Fatalf("set default-install clients: %v", err)
		}

		upgradeClient := newLSPRouterFakeClient(t, "antigravity", true)
		mcphub := filepath.Join(t.TempDir(), "mcphub.exe")
		upgradeClient.entries[LSPRouterEntryName("go")] = clients.MCPEntry{
			Name:         LSPRouterEntryName("go"),
			RelayURL:     LSPRouterURL(7777, "go"),
			RelayExePath: mcphub,
		}
		opts := LSPClientRouterOpts{
			GUIPort:       7777,
			Clients:       map[string]clients.Client{"antigravity": upgradeClient},
			McphubExePath: mcphub,
		}

		report, err := NewAPI().EnsureLSPRouterClientEntries(opts)
		if err != nil {
			t.Fatalf("EnsureLSPRouterClientEntries: %v", err)
		}
		name := LSPRouterEntryName("python")
		if got, err := upgradeClient.GetEntry(name); err != nil || got == nil || got.RelayURL != LSPRouterURL(7777, "python") {
			t.Fatalf("upgrade client new router entry = %+v err=%v, want python relay router URL", got, err)
		}
		if len(report.Backups) != 1 || report.Backups[0].Client != "antigravity" {
			t.Fatalf("Backups = %+v, want upgrade Antigravity backup", report.Backups)
		}
	})

	t.Run("aggregate hub entry keeps opt-in client eligible", func(t *testing.T) {
		seedLSPRouterManifest(t, []string{"go"})
		if err := NewAPI().SetDefaultInstallClientNames([]string{"claude-code"}); err != nil {
			t.Fatalf("set default-install clients: %v", err)
		}

		aggregateClient := newLSPRouterFakeClient(t, "gemini-cli", true)
		aggregateClient.entries[hubReconcileAggregateEntryName] = clients.MCPEntry{
			Name: hubReconcileAggregateEntryName,
			URL:  clients.HubLoopbackURL(3439, "/clients/gemini-cli/mcp"),
		}
		opts := LSPClientRouterOpts{
			GUIPort: 7777,
			Clients: map[string]clients.Client{"gemini-cli": aggregateClient},
		}

		report, err := NewAPI().EnsureLSPRouterClientEntries(opts)
		if err != nil {
			t.Fatalf("EnsureLSPRouterClientEntries: %v", err)
		}
		name := LSPRouterEntryName("go")
		if got, err := aggregateClient.GetEntry(name); err != nil || got == nil || got.URL != LSPRouterURL(7777, "go") {
			t.Fatalf("aggregate evidence router entry = %+v err=%v, want router URL", got, err)
		}
		if len(report.Backups) != 1 || report.Backups[0].Client != "gemini-cli" {
			t.Fatalf("Backups = %+v, want aggregate-evidence client backup", report.Backups)
		}
	})

	t.Run("explicit lsp router opt-out skips only listed clients", func(t *testing.T) {
		seedLSPRouterManifest(t, []string{"go"})
		prefs := []byte("clients.default_install: claude-code,antigravity\nclients.lsp_router_disabled: antigravity\n")
		if err := WriteStateFileBytesAtomic(SettingsPath(), prefs); err != nil {
			t.Fatalf("write lsp-router disabled prefs: %v", err)
		}

		enabled := newLSPRouterFakeClient(t, "claude-code", true)
		disabled := newLSPRouterFakeClient(t, "antigravity", true)
		clientMap := map[string]clients.Client{
			"claude-code": enabled,
			"antigravity": disabled,
		}
		opts := LSPClientRouterOpts{
			GUIPort:       7777,
			Clients:       clientMap,
			McphubExePath: filepath.Join(t.TempDir(), "mcphub.exe"),
		}

		report, err := NewAPI().EnsureLSPRouterClientEntries(opts)
		if err != nil {
			t.Fatalf("EnsureLSPRouterClientEntries: %v", err)
		}
		name := LSPRouterEntryName("go")
		if got, err := enabled.GetEntry(name); err != nil || got == nil || got.URL != LSPRouterURL(7777, "go") {
			t.Fatalf("enabled client entry = %+v err=%v, want router URL", got, err)
		}
		if got, err := disabled.GetEntry(name); err != nil || got != nil {
			t.Fatalf("disabled client entry = %+v err=%v, want no entry", got, err)
		}
		if len(report.Backups) != 1 || report.Backups[0].Client != "claude-code" {
			t.Fatalf("Backups = %+v, want only enabled client backup", report.Backups)
		}
		if disabled.addCalls != 0 || len(disabled.backupPaths) != 0 {
			t.Fatalf("disabled client was mutated: addCalls=%d backups=%v entries=%v", disabled.addCalls, disabled.backupPaths, disabled.entries)
		}

		second, err := NewAPI().EnsureLSPRouterClientEntries(opts)
		if err != nil {
			t.Fatalf("second EnsureLSPRouterClientEntries: %v", err)
		}
		if len(second.Backups) != 0 || len(second.Applied) != 0 || len(second.Removed) != 0 {
			t.Fatalf("idempotent rerun changed config: %+v", second)
		}
		if disabled.addCalls != 0 || len(disabled.entries) != 0 {
			t.Fatalf("disabled client was re-added on rerun: addCalls=%d entries=%v", disabled.addCalls, disabled.entries)
		}
	})
}

func TestLSPRouterDisableThenSetupDoesNotReaddPersistedClient(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})

	a := NewAPI()
	codex := newLSPRouterFakeClient(t, "codex-cli", true)
	opts := LSPClientRouterOpts{
		GUIPort: 7777,
		Clients: map[string]clients.Client{"codex-cli": codex},
	}
	name := LSPRouterEntryName("go")

	if _, err := a.EnsureLSPRouterClientEntries(opts); err != nil {
		t.Fatalf("initial EnsureLSPRouterClientEntries: %v", err)
	}
	if got, err := codex.GetEntry(name); err != nil || got == nil || got.URL != LSPRouterURL(7777, "go") {
		t.Fatalf("initial router entry = %+v err=%v, want router URL", got, err)
	}

	if err := a.SetLSPRouterDisabledClients([]string{"codex-cli"}); err != nil {
		t.Fatalf("persist disabled client: %v", err)
	}
	rollback, err := a.RollbackLSPRouterClientEntriesForClient("codex-cli", opts)
	if err != nil {
		t.Fatalf("RollbackLSPRouterClientEntriesForClient: %v", err)
	}
	if len(rollback.Removed) != 1 || rollback.Removed[0].Client != "codex-cli" || rollback.Removed[0].EntryName != name {
		t.Fatalf("rollback removed = %+v, want codex-cli %s", rollback.Removed, name)
	}
	if got, err := codex.GetEntry(name); err != nil || got != nil {
		t.Fatalf("entry after disable rollback = %+v err=%v, want removed", got, err)
	}

	second, err := a.EnsureLSPRouterClientEntries(opts)
	if err != nil {
		t.Fatalf("second EnsureLSPRouterClientEntries: %v", err)
	}
	if len(second.Backups) != 0 || len(second.Applied) != 0 || len(second.Removed) != 0 {
		t.Fatalf("disabled client was mutated by setup rerun: %+v", second)
	}
	if got, err := codex.GetEntry(name); err != nil || got != nil {
		t.Fatalf("disabled client was re-added by setup: entry=%+v err=%v", got, err)
	}
	if len(codex.backupPaths) != 2 {
		t.Fatalf("backup count = %d, want initial ensure + immediate rollback only", len(codex.backupPaths))
	}
}

func TestLSPRouterEnableThenEnsureReaddsPersistedClient(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})

	a := NewAPI()
	if err := a.SetLSPRouterDisabledClients([]string{"codex-cli"}); err != nil {
		t.Fatalf("persist disabled client: %v", err)
	}
	codex := newLSPRouterFakeClient(t, "codex-cli", true)
	opts := LSPClientRouterOpts{
		GUIPort: 7777,
		Clients: map[string]clients.Client{"codex-cli": codex},
	}
	name := LSPRouterEntryName("go")

	skipped, err := a.EnsureLSPRouterClientEntries(opts)
	if err != nil {
		t.Fatalf("Ensure while disabled: %v", err)
	}
	if len(skipped.Backups) != 0 || len(skipped.Applied) != 0 {
		t.Fatalf("disabled ensure mutated config: %+v", skipped)
	}
	if got, err := codex.GetEntry(name); err != nil || got != nil {
		t.Fatalf("entry while disabled = %+v err=%v, want nil", got, err)
	}

	if err := a.SetLSPRouterDisabledClients(nil); err != nil {
		t.Fatalf("clear disabled client: %v", err)
	}
	report, err := a.EnsureLSPRouterClientEntries(opts)
	if err != nil {
		t.Fatalf("Ensure after enable: %v", err)
	}
	if len(report.Applied) != 1 || report.Applied[0].Client != "codex-cli" || report.Applied[0].EntryName != name {
		t.Fatalf("applied = %+v, want codex-cli %s", report.Applied, name)
	}
	if got, err := codex.GetEntry(name); err != nil || got == nil || got.URL != LSPRouterURL(7777, "go") {
		t.Fatalf("entry after enable = %+v err=%v, want router URL", got, err)
	}
}

func TestEnsureLSPRouterClientEntries_ForceClientReaddsOptInAfterDisable(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	if err := NewAPI().SetDefaultInstallClientNames([]string{"claude-code"}); err != nil {
		t.Fatalf("set default-install clients: %v", err)
	}

	a := NewAPI()
	if err := a.SetLSPRouterDisabledClients([]string{"antigravity"}); err != nil {
		t.Fatalf("persist disabled client: %v", err)
	}
	antigravity := newLSPRouterFakeClient(t, "antigravity", true)
	opts := LSPClientRouterOpts{
		GUIPort:       7777,
		Clients:       map[string]clients.Client{"antigravity": antigravity},
		McphubExePath: filepath.Join(t.TempDir(), "mcphub.exe"),
	}
	name := LSPRouterEntryName("go")

	skipped, err := a.EnsureLSPRouterClientEntries(opts)
	if err != nil {
		t.Fatalf("Ensure while disabled: %v", err)
	}
	if len(skipped.Backups) != 0 || len(skipped.Applied) != 0 {
		t.Fatalf("disabled ensure mutated config: %+v", skipped)
	}
	if got, err := antigravity.GetEntry(name); err != nil || got != nil {
		t.Fatalf("entry while disabled = %+v err=%v, want nil", got, err)
	}

	if err := a.SetLSPRouterDisabledClients(nil); err != nil {
		t.Fatalf("clear disabled client: %v", err)
	}
	opts.ForceClientName = "antigravity"
	report, err := a.EnsureLSPRouterClientEntries(opts)
	if err != nil {
		t.Fatalf("Ensure after explicit enable: %v", err)
	}
	if len(report.Applied) != 1 || report.Applied[0].Client != "antigravity" || report.Applied[0].EntryName != name {
		t.Fatalf("applied = %+v, want antigravity %s", report.Applied, name)
	}
	if got, err := antigravity.GetEntry(name); err != nil || got == nil || got.RelayURL != LSPRouterURL(7777, "go") {
		t.Fatalf("entry after explicit enable = %+v err=%v, want relay router URL", got, err)
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
	if got.URL != "http://127.0.0.1:9126/lsp/go/mcp" {
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

func TestEnsureLSPRouterClientEntries_SkipsNonHubOwnedLiveEntry(t *testing.T) {
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
	codex.entries["mcp-language-server-go"] = clients.MCPEntry{
		Name: "mcp-language-server-go",
		URL:  "https://example.invalid/custom-mcp",
	}

	report, err := NewAPI().EnsureLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 9126,
		Clients: map[string]clients.Client{"codex-cli": codex},
	})
	if err == nil {
		t.Fatal("expected non-hub-owned live entry to produce a failure")
	}
	if len(report.Failed) != 1 || report.Failed[0].Op != "ownership" || report.Failed[0].EntryName != "mcp-language-server-go" {
		t.Fatalf("Failed = %+v, want one ownership failure for router entry", report.Failed)
	}
	if got := codex.entries["mcp-language-server-go"].URL; got != "https://example.invalid/custom-mcp" {
		t.Fatalf("custom live entry was overwritten: %q", got)
	}
	if len(report.Backups) != 0 || codex.addCalls != 0 {
		t.Fatalf("non-hub-owned skip should not backup or add; report=%+v addCalls=%d", report, codex.addCalls)
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
	if got := codex.entries["mcp-language-server-go"].URL; got != "http://127.0.0.1:9126/lsp/go/mcp" {
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
		t.Fatalf("rollback URL = %q, want exact backed-up per-pair URL", got)
	}
	if codex.restoreCalls != 1 {
		t.Fatalf("rollback restore calls = %d, want 1 backup restore", codex.restoreCalls)
	}
	if len(codex.backupPaths) != 2 {
		t.Fatalf("total backups = %d, want setup backup + rollback backup", len(codex.backupPaths))
	}
}

func TestRollbackLSPRouterClientEntriesForClient_RemovesOnlyTargetRouterEntries(t *testing.T) {
	languages := []string{"clangd", "fortran", "go", "javascript", "python", "rust", "typescript", "vscode-css", "vscode-html"}
	seedLSPRouterManifest(t, languages)

	target := newLSPRouterFakeClient(t, "antigravity", true)
	sibling := newLSPRouterFakeClient(t, "codex-cli", true)
	mcphub := filepath.Join(t.TempDir(), "mcphub.exe")
	for _, language := range languages {
		name := LSPRouterEntryName(language)
		target.entries[name] = clients.MCPEntry{
			Name:         name,
			RelayURL:     LSPRouterURL(7777, language),
			RelayExePath: mcphub,
		}
		sibling.entries[name] = clients.MCPEntry{
			Name: name,
			URL:  LSPRouterURL(7777, language),
		}
	}
	target.entries["operator-entry"] = clients.MCPEntry{Name: "operator-entry", URL: "https://example.invalid/mcp"}
	opts := LSPClientRouterOpts{
		GUIPort: 7777,
		Clients: map[string]clients.Client{
			"antigravity": target,
			"codex-cli":   sibling,
		},
	}

	report, err := NewAPI().RollbackLSPRouterClientEntriesForClient("antigravity", opts)
	if err != nil {
		t.Fatalf("RollbackLSPRouterClientEntriesForClient: %v", err)
	}
	if len(report.Removed) != len(languages) {
		t.Fatalf("Removed len = %d, want %d: %+v", len(report.Removed), len(languages), report.Removed)
	}
	if len(report.Backups) != 1 || report.Backups[0].Client != "antigravity" {
		t.Fatalf("Backups = %+v, want only target client backup", report.Backups)
	}
	for _, language := range languages {
		name := LSPRouterEntryName(language)
		if _, ok := target.entries[name]; ok {
			t.Fatalf("target router entry %s survived: entries=%v", name, target.entries)
		}
		if _, ok := sibling.entries[name]; !ok {
			t.Fatalf("sibling router entry %s was removed: entries=%v", name, sibling.entries)
		}
	}
	if _, ok := target.entries["operator-entry"]; !ok {
		t.Fatalf("non-router target entry was removed: entries=%v", target.entries)
	}
	if len(sibling.backupPaths) != 0 || sibling.removeCalls != 0 {
		t.Fatalf("sibling was mutated: backups=%v removeCalls=%d", sibling.backupPaths, sibling.removeCalls)
	}
}

func TestRollbackLSPRouterClientEntriesForClient_LeavesForeignLSPRouterLikeEntries(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go", "python", "rust", "typescript"})

	target := newLSPRouterFakeClient(t, "antigravity", true)
	stalePort := 6666
	mcphub := filepath.Join(t.TempDir(), "mcphub.exe")
	target.entries[LSPRouterEntryName("go")] = clients.MCPEntry{
		Name: LSPRouterEntryName("go"),
		URL:  LSPRouterURL(7777, "go"),
	}
	target.entries[LSPRouterEntryName("python")] = clients.MCPEntry{
		Name: LSPRouterEntryName("python"),
		URL:  LSPRouterURL(stalePort, "python"),
	}
	target.entries[LSPRouterEntryName("rust")] = clients.MCPEntry{
		Name:         LSPRouterEntryName("rust"),
		RelayURL:     LSPRouterURL(7777, "rust"),
		RelayExePath: mcphub,
	}
	target.entries[LSPRouterEntryName("typescript")] = clients.MCPEntry{
		Name:         LSPRouterEntryName("typescript"),
		RelayURL:     LSPRouterURL(7777, "typescript"),
		RelayExePath: filepath.Join(t.TempDir(), "mcp.exe"),
	}
	target.entries["operator-lsp-go"] = clients.MCPEntry{
		Name: "operator-lsp-go",
		URL:  LSPRouterURL(stalePort, "go"),
	}

	report, err := NewAPI().RollbackLSPRouterClientEntriesForClient("antigravity", LSPClientRouterOpts{
		GUIPort: 7777,
		Clients: map[string]clients.Client{"antigravity": target},
	})
	if err != nil {
		t.Fatalf("RollbackLSPRouterClientEntriesForClient: %v", err)
	}

	removed := map[string]bool{}
	for _, change := range report.Removed {
		removed[change.EntryName] = true
	}
	for _, language := range []string{"go", "python", "rust"} {
		name := LSPRouterEntryName(language)
		if !removed[name] {
			t.Fatalf("removed entries = %+v, want %s removed", report.Removed, name)
		}
		if _, ok := target.entries[name]; ok {
			t.Fatalf("owned router entry %s survived: entries=%v", name, target.entries)
		}
	}
	if _, ok := target.entries[LSPRouterEntryName("typescript")]; !ok {
		t.Fatalf("foreign relay entry was removed: entries=%v", target.entries)
	}
	if _, ok := target.entries["operator-lsp-go"]; !ok {
		t.Fatalf("foreign non-reserved LSP-like entry was removed: entries=%v", target.entries)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("foreign LSP-like entries should be skipped, not failed: %+v", report.Failed)
	}
	if len(report.Skipped) != 1 || report.Skipped[0].EntryName != LSPRouterEntryName("typescript") {
		t.Fatalf("Skipped = %+v, want one skipped foreign typescript entry", report.Skipped)
	}
	if target.removeCalls != 3 {
		t.Fatalf("removeCalls = %d, want only 3 owned router removals", target.removeCalls)
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
	if got := codex.entries["mcp-language-server-go"].URL; got != "http://127.0.0.1:9126/lsp/go/mcp" {
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
	if got := codex.entries["mcp-language-server-go"].URL; got != "http://127.0.0.1:9200/mcp" {
		t.Fatalf("rollback restored latest router-containing backup; URL = %q, want reconstructed per-pair URL", got)
	}
	if codex.restoreCalls != 1 {
		t.Fatalf("rollback restore calls = %d, want 1 router-backup probe before reconstruction", codex.restoreCalls)
	}
	if len(codex.backupPaths) != 3 {
		t.Fatalf("total backups = %d, want setup + newer + rollback backup", len(codex.backupPaths))
	}
}

func TestRollbackLSPRouterClientEntries_BackupReadFailureSkipsReconstruct(t *testing.T) {
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
	routerURL := "http://127.0.0.1:9126/lsp/go/mcp"
	codex.entries["mcp-language-server-go"] = clients.MCPEntry{
		Name: "mcp-language-server-go",
		URL:  routerURL,
	}
	codex.latestBackupErr = errors.New("backup index unreadable")

	report, err := NewAPI().RollbackLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 9126,
		Clients: map[string]clients.Client{"codex-cli": codex},
	})
	if err == nil {
		t.Fatal("expected backup-read failure to make rollback fail")
	}
	if len(report.Failed) != 1 || report.Failed[0].Op != "backup-read" {
		t.Fatalf("Failed = %+v, want one backup-read failure", report.Failed)
	}
	if got := codex.entries["mcp-language-server-go"].URL; got != routerURL {
		t.Fatalf("rollback mutated entry after backup-read failure: %q", got)
	}
	if codex.addCalls != 0 || codex.restoreCalls != 0 || len(report.Backups) != 0 {
		t.Fatalf("backup-read failure should not mutate; add=%d restore=%d backups=%+v", codex.addCalls, codex.restoreCalls, report.Backups)
	}
}

func TestRollbackLSPRouterClientEntries_SkipsNonHubOwnedLiveEntry(t *testing.T) {
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
		URL:  "https://example.invalid/custom-mcp",
	}

	report, err := NewAPI().RollbackLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 9126,
		Clients: map[string]clients.Client{"codex-cli": codex},
	})
	if err == nil {
		t.Fatal("expected non-hub-owned rollback target to produce a failure")
	}
	if len(report.Failed) != 1 || report.Failed[0].Op != "ownership" || report.Failed[0].EntryName != "mcp-language-server-go" {
		t.Fatalf("Failed = %+v, want one ownership failure for legacy entry", report.Failed)
	}
	if got := codex.entries["mcp-language-server-go"].URL; got != "https://example.invalid/custom-mcp" {
		t.Fatalf("custom live entry was overwritten during rollback: %q", got)
	}
	if len(report.Backups) != 0 || codex.addCalls != 0 || codex.restoreCalls != 0 {
		t.Fatalf("non-hub-owned rollback skip should not mutate; report=%+v add=%d restore=%d",
			report, codex.addCalls, codex.restoreCalls)
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
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
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

func classifyAPITestAppliedRelease(induced error) func(error) clients.ClientMutationSettlement {
	return func(err error) clients.ClientMutationSettlement {
		if err == induced {
			return clients.ClientMutationAppliedReleaseUnconfirmed
		}
		return clients.ClassifyClientMutation(err)
	}
}

func TestApplyLSPRouterOpsAppliedReleaseUnconfirmedStopsSameLeaf(t *testing.T) {
	induced := errors.New("induced applied mutation with unconfirmed release")
	classify := classifyAPITestAppliedRelease(induced)

	t.Run("add records applied and failure before dependent remove", func(t *testing.T) {
		client := newLSPRouterFakeClient(t, "codex-cli", true)
		client.addErr = induced
		client.addAppliesOnErr = true
		report := &LSPClientRouterReport{}
		applyLSPRouterOps(LSPClientRouterOpts{classifyClientMutation: classify}, client, client.name, 1, []lspClientRouterOp{
			{kind: "add", language: "go", entryName: "router-go", entry: clients.MCPEntry{Name: "router-go", URL: "http://127.0.0.1:9125/lsp/go/mcp"}},
			{kind: "remove", language: "go", entryName: "legacy-go"},
		}, report)

		if client.addCalls != 1 || client.removeCalls != 0 {
			t.Fatalf("calls add=%d remove=%d, want 1/0", client.addCalls, client.removeCalls)
		}
		if len(report.Applied) != 1 || len(report.Failed) != 1 || report.Failed[0].Op != "add" {
			t.Fatalf("report = %+v, want one applied plus one add lifecycle failure", report)
		}
	})

	t.Run("remove records removal and stops later add", func(t *testing.T) {
		client := newLSPRouterFakeClient(t, "codex-cli", true)
		client.entries["legacy-go"] = clients.MCPEntry{Name: "legacy-go"}
		client.removeErr = induced
		client.removeAppliesOnErr = true
		report := &LSPClientRouterReport{}
		applyLSPRouterOps(LSPClientRouterOpts{classifyClientMutation: classify}, client, client.name, 1, []lspClientRouterOp{
			{kind: "remove", language: "go", entryName: "legacy-go"},
			{kind: "add", language: "python", entryName: "router-python", entry: clients.MCPEntry{Name: "router-python", URL: "http://127.0.0.1:9125/lsp/python/mcp"}},
		}, report)

		if client.removeCalls != 1 || client.addCalls != 0 {
			t.Fatalf("calls remove=%d add=%d, want 1/0", client.removeCalls, client.addCalls)
		}
		if len(report.Removed) != 1 || len(report.Failed) != 1 || report.Failed[0].Op != "remove" {
			t.Fatalf("report = %+v, want one removed plus one remove lifecycle failure", report)
		}
	})

	t.Run("restore records restored and never reads or falls back", func(t *testing.T) {
		client := newLSPRouterFakeClient(t, "codex-cli", true)
		client.entries["router-go"] = clients.MCPEntry{Name: "router-go", URL: "http://localhost:9200/mcp"}
		backup, err := client.BackupKeep(1)
		if err != nil {
			t.Fatal(err)
		}
		client.restoreErr = induced
		client.restoreAppliesOnErr = true
		report := &LSPClientRouterReport{}
		applyLSPRouterOps(LSPClientRouterOpts{classifyClientMutation: classify}, client, client.name, 1, []lspClientRouterOp{
			{kind: "restore", language: "go", entryName: "router-go", backup: backup, entry: clients.MCPEntry{Name: "router-go", URL: "http://localhost:9200/mcp"}},
		}, report)

		if client.restoreCalls != 1 || client.getCalls != 0 || client.addCalls != 0 {
			t.Fatalf("calls restore=%d get=%d add=%d, want 1/0/0", client.restoreCalls, client.getCalls, client.addCalls)
		}
		if len(report.Restored) != 1 || len(report.Failed) != 1 || report.Failed[0].Op != "restore" {
			t.Fatalf("report = %+v, want one restored plus one restore lifecycle failure", report)
		}
	})

	t.Run("fallback add records applied and stops", func(t *testing.T) {
		client := newLSPRouterFakeClient(t, "codex-cli", true)
		client.entries["router-go"] = clients.MCPEntry{Name: "router-go", URL: "http://127.0.0.1:9125/lsp/go/mcp"}
		backup, err := client.BackupKeep(1)
		if err != nil {
			t.Fatal(err)
		}
		client.addErr = induced
		client.addAppliesOnErr = true
		report := &LSPClientRouterReport{}
		applyLSPRouterOps(LSPClientRouterOpts{classifyClientMutation: classify}, client, client.name, 1, []lspClientRouterOp{
			{kind: "restore", language: "go", entryName: "router-go", backup: backup, entry: clients.MCPEntry{Name: "router-go", URL: "http://localhost:9200/mcp"}},
			{kind: "remove", language: "python", entryName: "later"},
		}, report)

		if client.restoreCalls != 1 || client.addCalls != 1 || client.removeCalls != 0 {
			t.Fatalf("calls restore=%d add=%d remove=%d, want 1/1/0", client.restoreCalls, client.addCalls, client.removeCalls)
		}
		if len(report.Applied) != 1 || len(report.Failed) != 1 || report.Failed[0].Op != "add" {
			t.Fatalf("report = %+v, want one applied fallback plus one add lifecycle failure", report)
		}
	})
}

func seedManifestWithClientBinding(t *testing.T, name, client string, port int) {
	t.Helper()
	dir := os.Getenv("MCPHUB_MANIFEST_DIR_OVERRIDE")
	if dir == "" {
		dir = t.TempDir()
		t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	}
	serverDir := filepath.Join(dir, name)
	if err := os.MkdirAll(serverDir, 0o700); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	body := fmt.Sprintf(`name: %s
kind: global
transport: stdio-bridge
command: %s
daemons:
  - name: default
    port: %d
client_bindings:
  - client: %s
    daemon: default
    url_path: /mcp
`, name, name, port, client)
	if err := os.WriteFile(filepath.Join(serverDir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func overrideLSPRouterRegistry(t *testing.T) func() {
	t.Helper()
	prior := testRegistryPathOverride
	testRegistryPathOverride = filepath.Join(t.TempDir(), "workspaces.yaml")
	return func() { testRegistryPathOverride = prior }
}

// TestLSPRouterEntryRelayContextForRelayStdioClients pins the relay-stdio
// predicate migration on the LSP-router entry-builder path. Both
// lspRouterMCPEntryForClient and lspLegacyMCPEntryForClient must populate
// RelayExePath for EVERY relay-stdio adapter (antigravity AND zed) and leave
// it empty for URL-native adapters (claude-code as control). Before the
// migration only antigravity was handled (`adapter.Name() == "antigravity"`),
// so a zed LSP entry lacked RelayExePath and zed.AddEntry would reject it —
// this test closes that latent gap. The relay forward target (RelayURL) must
// also be the router/legacy URL so the relay takes its --url branch.
func TestLSPRouterEntryRelayContextForRelayStdioClients(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "mcphub.exe") // absolute on the host OS so the relay-stdio adapters accept it
	const targetURL = "http://localhost:9201/lsp/go/mcp"
	opts := LSPClientRouterOpts{McphubExePath: exe}

	mkAdapter := func(name string, factory func() (clients.Client, error)) clients.Client {
		t.Helper()
		c, err := factory()
		if err != nil {
			t.Fatalf("construct %s adapter: %v", name, err)
		}
		return c
	}

	relayAdapters := map[string]clients.Client{
		"antigravity": mkAdapter("antigravity", clients.NewAntigravity),
		"zed":         mkAdapter("zed", clients.NewZed),
	}

	for name, adapter := range relayAdapters {
		if !adapter.IsRelayStdio() {
			t.Fatalf("%s adapter IsRelayStdio() = false, want true (test premise)", name)
		}

		// Router path: RelayURL set unconditionally, RelayExePath for relay clients.
		entry, err := lspRouterMCPEntryForClient(opts, adapter, "mcp-language-server-go", targetURL)
		if err != nil {
			t.Fatalf("%s lspRouterMCPEntryForClient: %v", name, err)
		}
		if entry.RelayExePath != exe {
			t.Errorf("%s router entry RelayExePath = %q, want %q (relay-stdio client must carry relay exe)", name, entry.RelayExePath, exe)
		}
		if entry.RelayURL != targetURL {
			t.Errorf("%s router entry RelayURL = %q, want %q (relay forwards to target via --url branch)", name, entry.RelayURL, targetURL)
		}
		// Prove the built field set is SUFFICIENT for the relay-stdio
		// adapter's AddEntry preconditions, purely in-memory (no disk I/O
		// against a live config): RelayExePath must be a non-empty absolute
		// path and a non-empty relay forward target (RelayURL or URL) must
		// be present — exactly the gates antigravity.AddEntry / zed.AddEntry
		// enforce before writing.
		assertRelayEntryFieldsSufficient(t, name+" router", entry)

		// Legacy path: RelayURL + RelayExePath for relay clients.
		legacy, err := lspLegacyMCPEntryForClient(opts, adapter, "mcp-language-server-go", targetURL)
		if err != nil {
			t.Fatalf("%s lspLegacyMCPEntryForClient: %v", name, err)
		}
		if legacy.RelayExePath != exe {
			t.Errorf("%s legacy entry RelayExePath = %q, want %q", name, legacy.RelayExePath, exe)
		}
		if legacy.RelayURL != targetURL {
			t.Errorf("%s legacy entry RelayURL = %q, want %q", name, legacy.RelayURL, targetURL)
		}
		assertRelayEntryFieldsSufficient(t, name+" legacy", legacy)
	}

	// Control: a URL-native adapter gets NO relay context on either path.
	urlNative := mkAdapter("claude-code", clients.NewClaudeCode)
	if urlNative.IsRelayStdio() {
		t.Fatalf("claude-code adapter IsRelayStdio() = true, want false (control premise)")
	}
	router, err := lspRouterMCPEntryForClient(opts, urlNative, "mcp-language-server-go", targetURL)
	if err != nil {
		t.Fatalf("claude-code lspRouterMCPEntryForClient: %v", err)
	}
	if router.RelayExePath != "" {
		t.Errorf("claude-code router entry RelayExePath = %q, want empty (URL-native client needs no relay context)", router.RelayExePath)
	}
	if router.URL != targetURL {
		t.Errorf("claude-code router entry URL = %q, want %q", router.URL, targetURL)
	}
	legacy, err := lspLegacyMCPEntryForClient(opts, urlNative, "mcp-language-server-go", targetURL)
	if err != nil {
		t.Fatalf("claude-code lspLegacyMCPEntryForClient: %v", err)
	}
	if legacy.RelayExePath != "" {
		t.Errorf("claude-code legacy entry RelayExePath = %q, want empty", legacy.RelayExePath)
	}
}

// assertRelayEntryFieldsSufficient checks, purely in-memory, that the
// lsp-router-built MCPEntry carries the fields a relay-stdio adapter's
// AddEntry requires before it will write the stdio-bridge entry:
//
//   - RelayExePath non-empty AND absolute (the `command` field), and
//   - a non-empty relay forward target via RelayURL or URL (the `--url` arg).
//
// These are exactly the preconditions antigravity.AddEntry and zed.AddEntry
// enforce (and reject on). Asserting them directly proves field-sufficiency
// without invoking AddEntry against any config file — so the test never
// reads or writes a live client config.
func assertRelayEntryFieldsSufficient(t *testing.T, label string, entry clients.MCPEntry) {
	t.Helper()
	if entry.RelayExePath == "" {
		t.Errorf("%s: RelayExePath empty — relay-stdio AddEntry requires it for the 'command' field", label)
	} else if !filepath.IsAbs(entry.RelayExePath) {
		t.Errorf("%s: RelayExePath %q not absolute — relay-stdio AddEntry rejects a relative command", label, entry.RelayExePath)
	}
	if entry.RelayURL == "" && entry.URL == "" {
		t.Errorf("%s: both RelayURL and URL empty — relay-stdio AddEntry has no forward target to relay to", label)
	}
}

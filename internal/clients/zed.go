package clients

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// NewZed returns a Client bound to Zed editor's user settings.json.
//
// Zed is a real, current MCP client. It reads MCP server definitions from
// the top-level `context_servers` key of its user settings file:
//
//   - Windows: %APPDATA%\Zed\settings.json
//   - Linux:   $XDG_CONFIG_HOME/zed/settings.json or ~/.config/zed/settings.json
//   - macOS:   ~/.config/zed/settings.json
//
// (Official source: github.com/zed-industries/zed/blob/main/docs/src/
// configuring-zed.md and .../ai/mcp.md.)
//
// Transport choice — relay-stdio (NOT HTTP-direct). Zed's official MCP
// docs DO show a `url` form under `context_servers`, but it is unreliable
// for mcp-local-hub's use case (a loopback streamable-HTTP daemon with no
// OAuth):
//
//   - The documented `{"url": "custom", ...}` example is a placeholder,
//     not a working loopback pattern.
//   - Zed's own external-agents troubleshooting docs state: "Local
//     stdio-based MCP servers work reliably, but remote MCP servers ...
//     may have issues", and recommend stdio servers when remote MCP
//     tools do not appear.
//   - The community-standard path for remote/HTTP MCP in Zed (issue
//     zed-industries/zed#37770 and multiple 2025-2026 setup guides) is a
//     local stdio proxy (`mcp-remote` / equivalent), not a native `url`
//     entry.
//
// So this adapter writes a STDIO entry that spawns our own `mcphub relay`
// subcommand, exactly like the Antigravity adapter. Zed's agent launches
// the relay as a child process; the relay connects to the shared HTTP
// daemon and Zed transparently benefits from the shared-daemon
// architecture like every other client.
//
// Entry shape written under context_servers.<server-name>:
//
//	{
//	  "command": "<abs-path>/mcphub.exe",
//	  "args": ["relay", "--url", "http://localhost:<port>/mcp"]
//	}
//
// The relay target is MCPEntry.RelayURL when set (the serena dynamic-pool
// /serena/mcp router case), otherwise MCPEntry.URL — which install.go
// populates for every adapter. Requires MCPEntry.RelayExePath (absolute
// path to mcphub.exe, from os.Executable() at install time).
func NewZed() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &zedClient{path: defaultZedConfigPath(home)}, nil
}

func defaultZedConfigPath(home string) string {
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Zed", "settings.json")
		}
		return filepath.Join(home, "AppData", "Roaming", "Zed", "settings.json")
	default:
		// macOS and Linux both use ~/.config/zed; Linux additionally
		// honors XDG_CONFIG_HOME.
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "zed", "settings.json")
		}
		return filepath.Join(home, ".config", "zed", "settings.json")
	}
}

// zedClient is a standalone adapter (NOT an embedding of jsonMCPClient)
// because Zed uses the top-level `context_servers` key rather than the
// JSON family's `mcpServers`, AND a relay-stdio entry shape rather than a
// URL shape. It mirrors VS Code's standalone-struct pattern (different
// top-level key) crossed with Antigravity's relay-stdio AddEntry/GetEntry
// shape.
type zedClient struct {
	path string
}

// contextServersKey is the single owner of Zed's top-level MCP section
// name. Every method that reaches into the parsed settings map uses it.
const contextServersKey = "context_servers"

func (z *zedClient) Name() string       { return "zed" }
func (z *zedClient) ConfigPath() string { return z.path }

// IsRelayStdio reports true: Zed is treated as stdio-only for mcp-local-hub
// (see NewZed doc), so AddEntry requires relay context (RelayExePath + the
// relay forward target via RelayURL/URL) and rejects a URL-only entry.
func (z *zedClient) IsRelayStdio() bool { return true }

func (z *zedClient) Exists() bool {
	if _, err := os.Stat(z.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(z.path))
	return err == nil && st.IsDir()
}

func (z *zedClient) Backup() (string, error) {
	return z.BackupKeep(0)
}

func (z *zedClient) BackupKeep(keepN int) (string, error) {
	// Explicit MkdirAll before InitEmpty: EnsureClientConfigStub no
	// longer creates parents (v0.4.5 deep-sec Lane A #1), so this
	// seed-then-backup path must ensure the parent exists for fresh
	// hosts — matching the cursor/vscode/qwen BackupKeep wrappers.
	if dir := filepath.Dir(z.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := z.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(z.path, z.Name(), keepN)
}

// InitEmpty seeds settings.json with `{"context_servers": {}}` if the
// file is absent. AddEntry's later merge writes into the same map. Zed's
// settings.json supports `//` comments, but an empty stub is plain JSON,
// and the merge path round-trips through encoding/json which does not
// preserve comments — same limitation as every JSON-family adapter here.
func (z *zedClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(z.path, []byte("{\n  \"context_servers\": {}\n}\n"))
}

func (z *zedClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(z.path, data)
}

func (z *zedClient) readJSON() (map[string]any, error) {
	data, err := os.ReadFile(z.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", z.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (z *zedClient) writeJSON(m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline; tests get the os.WriteFile
	// fallback.
	return WriteConfigFile(z.path, append(out, '\n'))
}

// AddEntry writes a stdio relay entry under context_servers.<name>. Zed
// is treated as stdio-only for mcp-local-hub (see NewZed doc), so the
// entry spawns `mcphub relay --url <hub-url>` rather than declaring an
// HTTP url. The relay target is RelayURL when set (serena /serena/mcp
// router), otherwise the universally-populated URL.
func (z *zedClient) AddEntry(entry MCPEntry) error {
	if entry.RelayExePath == "" {
		return fmt.Errorf("zed adapter requires MCPEntry.RelayExePath (absolute path to mcphub.exe for the 'command' field)")
	}
	if !filepath.IsAbs(entry.RelayExePath) {
		return fmt.Errorf("zed adapter requires MCPEntry.RelayExePath to be absolute (got %q)", entry.RelayExePath)
	}
	target := entry.RelayURL
	if target == "" {
		target = entry.URL
	}
	if target == "" {
		return fmt.Errorf("zed adapter requires MCPEntry.URL or MCPEntry.RelayURL (the HTTP endpoint the stdio relay forwards to; Zed does not reliably support native loopback-HTTP MCP)")
	}
	m, err := z.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m[contextServersKey].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[entry.Name] = map[string]any{
		"command": entry.RelayExePath,
		"args":    []string{"relay", "--url", target},
	}
	m[contextServersKey] = servers
	return z.writeJSON(m)
}

func (z *zedClient) RemoveEntry(name string) error {
	m, err := z.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m[contextServersKey].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m[contextServersKey] = servers
	return z.writeJSON(m)
}

// GetEntry returns a minimal MCPEntry reconstructed from the stored
// relay-stdio shape. The `command` field maps to RelayExePath and the
// `--url <target>` arg maps back to RelayURL, for install-idempotency
// diagnostics. The URL field is NOT reconstructed because the stored arg
// is the relay forward target, which RelayURL already carries.
func (z *zedClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := z.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[contextServersKey].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	e := &MCPEntry{Name: name}
	if cmd, _ := raw["command"].(string); cmd != "" {
		e.RelayExePath = cmd
	}
	if argsAny, ok := raw["args"].([]any); ok {
		for i, v := range argsAny {
			if s, _ := v.(string); s == "--url" && i+1 < len(argsAny) {
				e.RelayURL, _ = argsAny[i+1].(string)
			}
		}
	}
	return e, nil
}

func (z *zedClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(z.path, z.Name())
}

func (z *zedClient) RestoreEntryFromBackup(backupPath, name string) error {
	return z.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface
// doc on Client.RestoreEntryFromBackupForRollback). Used only by the
// serena dynamic-pool migrate abort-rollback.
func (z *zedClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return z.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a backup entry already in hub-managed shape (an
// mcphub `relay` invocation) with ErrBackupEntryAlreadyMigrated; when
// true (migrate rollback) it writes the backup bytes verbatim regardless
// of shape.
func (z *zedClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	var backupMap map[string]any
	if len(backupData) == 0 {
		backupMap = map[string]any{}
	} else if err := json.Unmarshal(backupData, &backupMap); err != nil {
		return fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	backupServers, _ := backupMap[contextServersKey].(map[string]any)
	liveMap, err := z.readJSON()
	if err != nil {
		return err
	}
	liveServers, _ := liveMap[contextServersKey].(map[string]any)
	if liveServers == nil {
		liveServers = map[string]any{}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive guard (demigrate flow only — the rollback caller
			// passes allowHubEntry=true to restore the pre-reconcile
			// legacy hub entry verbatim).
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubRelayShapeEntry(rawMap) {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap[contextServersKey] = liveServers
			return z.writeJSON(liveMap)
		}
	}
	delete(liveServers, name)
	liveMap[contextServersKey] = liveServers
	return z.writeJSON(liveMap)
}

// AllStdioEntries returns every stdio entry from Zed's top-level
// `context_servers` key. Zed-written hub entries (command='mcphub')
// surface here, but the cleanup pipeline filters them out via
// isOurOwnProcess — same as Antigravity.
func (z *zedClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := z.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[contextServersKey].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans `context_servers` for stdio
// entries matching the mcp-language-server invocation pattern.
func (z *zedClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := z.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[contextServersKey].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

func (z *zedClient) BackupContainsEntry(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[contextServersKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether context_servers[name] in the
// backup at backupPath is in Zed's hub-managed shape (an mcphub `relay`
// invocation: command is the mcphub binary AND args[0] == "relay"). See
// Client.BackupEntryIsHubManaged.
func (z *zedClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[contextServersKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubRelayShapeEntry(entry), nil
}

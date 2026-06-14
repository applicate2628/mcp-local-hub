package clients

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// NewOpenCode returns a Client bound to OpenCode's global config file.
//
// OpenCode (https://opencode.ai, github.com/sst/opencode) is a real,
// current terminal-based AI coding agent with first-class MCP support. It
// reads MCP server definitions from the top-level `mcp` object of its JSON
// config. Two config scopes exist:
//
//   - Global: ~/.config/opencode/opencode.json (also accepts a `.jsonc`
//     variant). On every OS OpenCode resolves the global config from
//     ~/.config/opencode/ — it does NOT follow the Windows %APPDATA% /
//     macOS ~/Library convention. (Verified against the official config
//     resolution docs.)
//   - Project: opencode.json in the repository root (highest precedence,
//     merged over global at load time).
//
// The hub writes the GLOBAL file so a single per-user hub entry is visible
// in every project, matching every other adapter's user-scoped posture.
//
// Transport choice — HTTP-direct (NOT relay-stdio). OpenCode supports
// remote MCP servers natively over Streamable HTTP. A remote entry is
// keyed by server name under `mcp` and discriminated by `"type":"remote"`
// with a `url` endpoint and an `enabled` flag:
//
//	{
//	  "mcp": {
//	    "<server-name>": {
//	      "type": "remote",
//	      "url": "http://localhost:9121/mcp",
//	      "enabled": true
//	    }
//	  }
//	}
//
// (Local stdio servers use "type":"local" with a `command` ARRAY instead;
// the hub never writes that shape because the daemon is already an HTTP
// endpoint.) Optional `headers` is emitted when MCPEntry.Headers is
// non-empty.
//
// Sources (verified 2026-06):
//   - https://opencode.ai/docs/mcp-servers/ — `mcp` top-level key; local
//     ("type":"local", `command` array, `environment`, `enabled`) and
//     remote ("type":"remote", `url`, `headers`, `enabled`) entry shapes.
//   - https://opencode.ai/docs/config/ — global config at
//     ~/.config/opencode/opencode.json, project config opencode.json in
//     repo root; remote `type` discriminator value is "remote".
//
// IMPORTANT divergences from the JSON family (mcpServers + disabled:false):
//   - top-level key is `mcp`, NOT `mcpServers` — so this is a standalone
//     struct (like vscode/zed), not an embedding of jsonMCPClient.
//   - the active flag is `enabled` (true = on), NOT `disabled` (false =
//     on). The hub writes `enabled:true`.
//   - remote entries carry `"type":"remote"`.
//
// The hub-shape guard (isHubURLShapeEntry) keys off the `url` field and
// the absence of a `command` key, which holds for OpenCode's remote entry
// shape, so demigrate/rollback recognize a hub-managed OpenCode entry.
func NewOpenCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &openCodeClient{path: defaultOpenCodeConfigPath(home)}, nil
}

// defaultOpenCodeConfigPath returns the global OpenCode config path.
// OpenCode uses the XDG-style ~/.config/opencode/ location on every OS
// (Windows included — it does not switch to %APPDATA%). XDG_CONFIG_HOME is
// honored when set, matching OpenCode's own config resolution.
func defaultOpenCodeConfigPath(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "opencode.json")
	}
	// Path is OS-independent by design — OpenCode uses ~/.config/opencode/
	// on every OS, not %APPDATA% / ~/Library (see NewOpenCode doc).
	return filepath.Join(home, ".config", "opencode", "opencode.json")
}

// openCodeClient is a standalone adapter (NOT an embedding of jsonMCPClient)
// because OpenCode uses the top-level `mcp` key rather than the JSON
// family's `mcpServers`, AND a distinct entry shape (`type:"remote"` +
// `enabled:true` rather than `disabled:false`). It mirrors VS Code's
// standalone-struct + HTTP-direct pattern with OpenCode's key/field set.
type openCodeClient struct {
	path string
}

// openCodeMCPKey is the single owner of OpenCode's top-level MCP section
// name. Every method that reaches into the parsed config map uses it.
const openCodeMCPKey = "mcp"

func (o *openCodeClient) Name() string       { return "opencode" }
func (o *openCodeClient) ConfigPath() string { return o.path }

// Exists treats OpenCode as installed when EITHER the config file is
// present OR its parent directory (~/.config/opencode/) exists, mirroring
// the cursor/vscode/kiro "directory means installed" heuristic so an
// operator who has OpenCode installed but no MCP config yet still gets the
// Initialize / install affordance.
func (o *openCodeClient) Exists() bool {
	if _, err := os.Stat(o.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(o.path))
	return err == nil && st.IsDir()
}

func (o *openCodeClient) Backup() (string, error) {
	return o.BackupKeep(0)
}

// BackupKeep ensures the nested ~/.config/opencode parent directory exists,
// seeds an empty `{"mcp": {}}` stub if the config is absent, then writes
// the timestamped backup (pruning to keepN). The parent dir does not exist
// on a clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host. Mirrors the
// cursor/vscode/kiro/windsurf BackupKeep wrappers.
func (o *openCodeClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(o.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := o.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(o.path, o.Name(), keepN)
}

// InitEmpty seeds ~/.config/opencode/opencode.json with `{"mcp": {}}` if
// the file is absent. AddEntry's later merge writes into the same `mcp`
// map. OpenCode also accepts a top-level `$schema` field and many other
// keys; the merge path round-trips through encoding/json which preserves
// every unknown top-level key already present in the file (only the `mcp`
// map is touched), so seeding a minimal stub does not clobber a
// hand-authored config — but on a truly fresh host this minimal stub is
// all that is needed.
func (o *openCodeClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(o.path, []byte("{\n  \"mcp\": {}\n}\n"))
}

func (o *openCodeClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(o.path, data)
}

func (o *openCodeClient) readJSON() (map[string]any, error) {
	data, err := os.ReadFile(o.path)
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
		return nil, fmt.Errorf("parse %s: %w", o.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (o *openCodeClient) writeJSON(m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative + DACL-bound) for
	// token-bearing rewrites; tests get the os.WriteFile fallback.
	return WriteConfigFile(o.path, append(out, '\n'))
}

// AddEntry writes the hub-managed remote-HTTP entry under mcp.<name>.
// OpenCode's remote entry shape is `{"type":"remote","url":...,
// "enabled":true}`; an optional `headers` object is emitted when
// MCPEntry.Headers is non-empty.
func (o *openCodeClient) AddEntry(entry MCPEntry) error {
	m, err := o.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m[openCodeMCPKey].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	serverEntry := map[string]any{
		"type":    "remote",
		"url":     entry.URL,
		"enabled": true,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	servers[entry.Name] = serverEntry
	m[openCodeMCPKey] = servers
	return o.writeJSON(m)
}

func (o *openCodeClient) RemoveEntry(name string) error {
	m, err := o.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m[openCodeMCPKey].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m[openCodeMCPKey] = servers
	return o.writeJSON(m)
}

func (o *openCodeClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[openCodeMCPKey].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	url, _ := raw["url"].(string)
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers")}, nil
}

func (o *openCodeClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(o.path, o.Name())
}

func (o *openCodeClient) RestoreEntryFromBackup(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc
// on Client.RestoreEntryFromBackupForRollback). Used only by the serena
// dynamic-pool migrate abort-rollback.
func (o *openCodeClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a backup entry already in hub-HTTP shape (a hub
// loopback URL under `url` with no `command`) with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes
// the backup bytes verbatim regardless of shape.
func (o *openCodeClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
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
	backupServers, _ := backupMap[openCodeMCPKey].(map[string]any)
	liveMap, err := o.readJSON()
	if err != nil {
		return err
	}
	liveServers, _ := liveMap[openCodeMCPKey].(map[string]any)
	if liveServers == nil {
		liveServers = map[string]any{}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive guard (demigrate flow only — the rollback caller
			// passes allowHubEntry=true to restore the pre-reconcile legacy
			// hub entry verbatim).
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubURLShapeEntry(rawMap, "url") {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap[openCodeMCPKey] = liveServers
			return o.writeJSON(liveMap)
		}
	}
	delete(liveServers, name)
	liveMap[openCodeMCPKey] = liveServers
	return o.writeJSON(liveMap)
}

// AllStdioEntries returns every stdio entry from OpenCode's top-level `mcp`
// key. The hub writes only HTTP-direct ("type":"remote") entries, which
// have no `command` field and so are correctly skipped by
// collectStdioEntries. Operator-authored local ("type":"local") entries
// store `command` as an ARRAY (["npx","-y",...]) rather than a string;
// collectStdioEntries reads `command` as a string and therefore does not
// surface them — an accepted limitation of the cross-format cleanup scan
// (these helpers are best-effort stdio-leak detection, and the hub never
// writes the local shape).
func (o *openCodeClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[openCodeMCPKey].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans `mcp` for stdio entries matching the
// mcp-language-server invocation pattern. As with AllStdioEntries, OpenCode
// local entries use a `command` ARRAY which the string-keyed matcher does
// not recognize; the hub-written HTTP entries never match either way.
func (o *openCodeClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[openCodeMCPKey].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

func (o *openCodeClient) BackupContainsEntry(backupPath, name string) (bool, error) {
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
	servers, _ := m[openCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether mcp[name] in the backup at
// backupPath is in OpenCode's hub-managed shape (a hub loopback `url` with
// no `command`). See Client.BackupEntryIsHubManaged.
func (o *openCodeClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
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
	servers, _ := m[openCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}

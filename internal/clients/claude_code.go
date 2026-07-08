package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewClaudeCode returns a Client bound to the current user's ~/.claude.json.
// Note: this is the single-file Claude Code user config at $HOME/.claude.json —
// NOT the .claude/ directory's settings.json, which stores UI preferences and
// is not read for MCP server entries.
func NewClaudeCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&claudeCode{path: filepath.Join(home, ".claude.json")}), nil
}

type claudeCode struct {
	path string
}

// claudeCodeMCPServersKey is the single owner of claude-code's top-level MCP
// section name (the canonical JSON family `mcpServers` key). Every method that
// reaches into the parsed config map uses it.
const claudeCodeMCPServersKey = "mcpServers"

func (c *claudeCode) Name() string       { return "claude-code" }
func (c *claudeCode) ConfigPath() string { return c.path }

// IsRelayStdio reports false: claude-code is a URL-native HTTP MCP client.
func (c *claudeCode) IsRelayStdio() bool { return false }

func (c *claudeCode) Exists() bool {
	_, err := os.Stat(c.path)
	return err == nil
}

func (c *claudeCode) Backup() (string, error) {
	return writeBackup(c.path, c.Name(), 0)
}

func (c *claudeCode) BackupKeep(keepN int) (string, error) {
	return writeBackup(c.path, c.Name(), keepN)
}

// InitEmpty seeds ~/.claude.json with `{"mcpServers": {}}` if the
// file is absent. claude-code's single-file user config uses the
// canonical JSON family `mcpServers` key; AddEntry's later merge
// writes into the same map.
//
// ~/.claude.json is parsed JSONC-tolerantly and rewritten
// comment-preservingly: the read path strips `//` + `/* */` comments and
// trailing commas via the shared JSONC helper, and AddEntry/RemoveEntry
// patch through hujson so any comments and unrelated top-level keys are
// PRESERVED on every write — no longer the lossy encoding/json round-trip.
func (c *claudeCode) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(c.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

func (c *claudeCode) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so
	// production restores inherit the SecureWriteClientConfig
	// pipeline (handle-relative + DACL-bound). The backup file is
	// read in full, then handed to the writer as a byte slice.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(c.path, data)
}

// readJSON keeps unknown top-level fields untouched by parsing through
// map[string]any. The bytes are parsed JSONC-tolerantly so a `//` comment or
// trailing comma in a hand-edited ~/.claude.json does not break migrate / Init.
func (c *claudeCode) readJSON() (map[string]any, error) {
	data, err := readRawConfig(c.path)
	if err != nil {
		return nil, err
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.path, err)
	}
	return m, nil
}

// setMember sets mcpServers.<name> = value, and deleteMember removes it, both
// preserving the operator's comments + unrelated top-level keys when the file
// already has JSONC content. An empty/absent file falls back to a clean
// indented marshal. The bytes route through the UNCHANGED WriteConfigFile
// pipeline.
func (c *claudeCode) setMember(name string, value any) error {
	return mutateJSONObjectMember(c.path, claudeCodeMCPServersKey, name, value, false)
}

func (c *claudeCode) deleteMember(name string) error {
	return mutateJSONObjectMember(c.path, claudeCodeMCPServersKey, name, nil, true)
}

func (c *claudeCode) AddEntry(entry MCPEntry) error {
	// Claude Code's per-transport schema requires an explicit `type` field.
	// For HTTP-transport servers the correct value is "http"; stdio servers use
	// "stdio" and include command/args/env instead. This adapter only produces
	// URL-backed entries, so type is hardcoded here.
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set: patches mcpServers.<name> into the original
	// on-disk bytes via hujson so any comments and unrelated keys survive (a
	// full map re-marshal would drop both).
	return c.setMember(entry.Name, serverEntry)
}

func (c *claudeCode) RemoveEntry(name string) error {
	// Comment-preserving delete; absence is a no-op.
	return c.deleteMember(name)
}

func (c *claudeCode) GetEntry(name string) (*MCPEntry, error) {
	m, err := c.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[claudeCodeMCPServersKey].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	url, _ := raw["url"].(string)
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers"), Disabled: mcpEntryDisabled(raw)}, nil
}

// LatestBackupPath delegates to the shared helper.
func (c *claudeCode) LatestBackupPath() (string, bool, error) {
	return latestBackup(c.path, c.Name())
}

// RestoreEntryFromBackup reads the raw per-name entry from the backup
// at backupPath and writes it (or removes the current live entry, if
// the backup had none) into the live config. Other entries in the
// live config are untouched.
//
// Defensively refuses if the backup's copy of the named entry is
// already in hub-HTTP form (has a `url` field but no `command`). That
// situation arises when the backup was taken AFTER an earlier migrate
// of the same client already rewrote this entry — restoring would
// silently re-apply hub-HTTP data. See ErrBackupEntryAlreadyMigrated.
func (c *claudeCode) RestoreEntryFromBackup(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface
// doc on Client.RestoreEntryFromBackupForRollback). Install rollback and
// Serena migrate rollback use it when the timestamped backup is the source of
// truth.
func (c *claudeCode) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (the demigrate flow) it refuses a hub-HTTP-shaped backup entry with
// ErrBackupEntryAlreadyMigrated; when true (the migrate rollback) it
// writes the backup bytes verbatim regardless of shape.
func (c *claudeCode) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	// os.ReadFile (NOT readRawConfig): a named backup that is missing is a
	// genuine read error the demigrate caller must see, not a silent
	// treat-as-empty. Empty / comment-only / malformed bytes are then
	// classified by parseJSONCBytes (empty map vs parse error).
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	backupMap, err := parseJSONCBytes(backupData)
	if err != nil {
		return fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	backupServers, _ := backupMap[claudeCodeMCPServersKey].(map[string]any)
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive: refuse hub-HTTP-shaped backup entries. The
			// canonical hub-HTTP shape in .claude.json has a loopback
			// `url` field (http://localhost:<port>/... or 127.0.0.1)
			// and no `command` field. User-configured remote HTTP MCP
			// servers (url pointing at a non-loopback host) pass
			// through to the normal restore path. The rollback caller
			// (allowHubEntry=true) bypasses this guard to restore the
			// pre-reconcile legacy hub entry verbatim.
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubURLShapeEntry(rawMap, "url") {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			// Comment-preserving set into the LIVE config (its comments +
			// unrelated keys survive; the backup's entry VALUE is written).
			return c.setMember(name, backupEntry)
		}
	}
	return c.deleteMember(name)
}

// AllStdioEntries returns every stdio entry from mcpServers.
func (c *claudeCode) AllStdioEntries() ([]StdioEntry, error) {
	m, err := c.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[claudeCodeMCPServersKey].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans mcpServers for stdio entries
// matching the mcp-language-server invocation pattern.
func (c *claudeCode) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := c.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[claudeCodeMCPServersKey].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

// BackupContainsEntry reports whether the backup file at backupPath
// has an mcpServers[name] entry.
func (c *claudeCode) BackupContainsEntry(backupPath, name string) (bool, error) {
	// os.ReadFile (NOT readRawConfig): a missing named backup is a read error,
	// not a silent (false, nil); empty bytes parse to an empty map below.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[claudeCodeMCPServersKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	// Require the entry to be an object. A scalar (string, number,
	// bool) or null passes the "key present" check but would
	// corrupt the live config if fed back through
	// RestoreEntryFromBackup — treat as absent so sentinel fallback
	// refuses rather than silently writes malformed data.
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether mcpServers[name] in the
// .claude.json backup at backupPath is in claude-code's hub-HTTP shape
// (loopback `url` present, `command` absent). See
// Client.BackupEntryIsHubManaged.
func (c *claudeCode) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	// os.ReadFile (NOT readRawConfig): a missing named backup is a read error
	// the demigrate caller must see, not a silent (false, nil); empty bytes
	// parse to an empty map below.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[claudeCodeMCPServersKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}

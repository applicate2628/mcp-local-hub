package clients

import (
	"encoding/json"
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
	return &claudeCode{path: filepath.Join(home, ".claude.json")}, nil
}

type claudeCode struct {
	path string
}

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

// readJSON / writeJSON keep unknown top-level fields untouched by round-tripping
// through map[string]any.
func (c *claudeCode) readJSON() (map[string]any, error) {
	data, err := os.ReadFile(c.path)
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
		return nil, fmt.Errorf("parse %s: %w", c.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (c *claudeCode) writeJSON(m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Append trailing newline to match Claude Code's own formatting preference.
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative, DACL-bound)
	// for token-bearing rewrites; tests get the os.WriteFile fallback.
	return WriteConfigFile(c.path, append(out, '\n'))
}

func (c *claudeCode) AddEntry(entry MCPEntry) error {
	m, err := c.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
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
	servers[entry.Name] = serverEntry
	m["mcpServers"] = servers
	return c.writeJSON(m)
}

func (c *claudeCode) RemoveEntry(name string) error {
	m, err := c.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m["mcpServers"] = servers
	return c.writeJSON(m)
}

func (c *claudeCode) GetEntry(name string) (*MCPEntry, error) {
	m, err := c.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
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
// doc on Client.RestoreEntryFromBackupForRollback). Used only by the
// serena dynamic-pool migrate abort-rollback, whose backups ARE the
// legacy hub entry it must put back.
func (c *claudeCode) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (the demigrate flow) it refuses a hub-HTTP-shaped backup entry with
// ErrBackupEntryAlreadyMigrated; when true (the migrate rollback) it
// writes the backup bytes verbatim regardless of shape.
func (c *claudeCode) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
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
	backupServers, _ := backupMap["mcpServers"].(map[string]any)
	liveMap, err := c.readJSON()
	if err != nil {
		return err
	}
	liveServers, _ := liveMap["mcpServers"].(map[string]any)
	if liveServers == nil {
		liveServers = map[string]any{}
	}
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
			liveServers[name] = backupEntry
			liveMap["mcpServers"] = liveServers
			return c.writeJSON(liveMap)
		}
	}
	delete(liveServers, name)
	liveMap["mcpServers"] = liveServers
	return c.writeJSON(liveMap)
}

// AllStdioEntries returns every stdio entry from mcpServers.
func (c *claudeCode) AllStdioEntries() ([]StdioEntry, error) {
	m, err := c.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans mcpServers for stdio entries
// matching the mcp-language-server invocation pattern.
func (c *claudeCode) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := c.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

// BackupContainsEntry reports whether the backup file at backupPath
// has an mcpServers[name] entry.
func (c *claudeCode) BackupContainsEntry(backupPath, name string) (bool, error) {
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
	servers, _ := m["mcpServers"].(map[string]any)
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
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}

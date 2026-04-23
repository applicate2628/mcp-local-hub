package clients

import (
	"encoding/json"
	"fmt"
	"io"
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

func (c *claudeCode) Restore(backupPath string) error {
	in, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
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
	return os.WriteFile(c.path, append(out, '\n'), 0600)
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
	return &MCPEntry{Name: name, URL: url}, nil
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
			// through to the normal restore path.
			if rawMap, ok := backupEntry.(map[string]any); ok {
				if urlStr, _ := rawMap["url"].(string); isHubHTTPURL(urlStr) {
					if _, hasCmd := rawMap["command"]; !hasCmd {
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

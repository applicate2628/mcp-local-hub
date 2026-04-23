package clients

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// NewCodexCLI returns a Client bound to ~/.codex/config.toml.
func NewCodexCLI() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &codexCLI{path: filepath.Join(home, ".codex", "config.toml")}, nil
}

type codexCLI struct {
	path string
}

func (c *codexCLI) Name() string       { return "codex-cli" }
func (c *codexCLI) ConfigPath() string { return c.path }

func (c *codexCLI) Exists() bool {
	_, err := os.Stat(c.path)
	return err == nil
}

func (c *codexCLI) Backup() (string, error) {
	return writeBackup(c.path, c.Name(), 0)
}

func (c *codexCLI) BackupKeep(keepN int) (string, error) {
	return writeBackup(c.path, c.Name(), keepN)
}

func (c *codexCLI) Restore(backupPath string) error {
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

// readTOML / writeTOML round-trip through map[string]any so unknown sections survive.
func (c *codexCLI) readTOML() (map[string]any, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (c *codexCLI) writeTOML(m map[string]any) error {
	out, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, out, 0600)
}

func (c *codexCLI) AddEntry(entry MCPEntry) error {
	m, err := c.readTOML()
	if err != nil {
		return err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	// Replace the entry wholesale — this drops any stdio-era fields like `command`/`args`.
	entryMap := map[string]any{
		"url":                 entry.URL,
		"startup_timeout_sec": 10.0,
	}
	if len(entry.Headers) > 0 {
		entryMap["http_headers"] = entry.Headers
	}
	servers[entry.Name] = entryMap
	m["mcp_servers"] = servers
	return c.writeTOML(m)
}

func (c *codexCLI) RemoveEntry(name string) error {
	m, err := c.readTOML()
	if err != nil {
		return err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m["mcp_servers"] = servers
	return c.writeTOML(m)
}

func (c *codexCLI) GetEntry(name string) (*MCPEntry, error) {
	m, err := c.readTOML()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
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
func (c *codexCLI) LatestBackupPath() (string, bool, error) {
	return latestBackup(c.path, c.Name())
}

// RestoreEntryFromBackup reads the TOML backup, extracts the
// [mcp_servers.<name>] table (if present), and writes it over the live
// config's corresponding entry. Other [mcp_servers.*] tables are left
// untouched.
//
// Defensively refuses if the backup's copy of the named entry is
// already in hub-HTTP form (has a `url` key and no `command` key) —
// see ErrBackupEntryAlreadyMigrated.
func (c *codexCLI) RestoreEntryFromBackup(backupPath, name string) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	var backupMap map[string]any
	if len(backupData) > 0 {
		if err := toml.Unmarshal(backupData, &backupMap); err != nil {
			return fmt.Errorf("parse backup %s: %w", backupPath, err)
		}
	}
	if backupMap == nil {
		backupMap = map[string]any{}
	}
	backupServers, _ := backupMap["mcp_servers"].(map[string]any)
	liveMap, err := c.readTOML()
	if err != nil {
		return err
	}
	liveServers, _ := liveMap["mcp_servers"].(map[string]any)
	if liveServers == nil {
		liveServers = map[string]any{}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive: refuse hub-HTTP-shaped backup entries for
			// Codex CLI (loopback `url` present, `command` absent).
			// User-configured remote HTTP entries (non-loopback url)
			// pass through.
			if rawMap, ok := backupEntry.(map[string]any); ok {
				if urlStr, _ := rawMap["url"].(string); isHubHTTPURL(urlStr) {
					if _, hasCmd := rawMap["command"]; !hasCmd {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap["mcp_servers"] = liveServers
			return c.writeTOML(liveMap)
		}
	}
	delete(liveServers, name)
	liveMap["mcp_servers"] = liveServers
	return c.writeTOML(liveMap)
}

// BackupContainsEntry reports whether the backup file at backupPath
// has an [mcp_servers.<name>] table.
func (c *codexCLI) BackupContainsEntry(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	_, present := servers[name]
	return present, nil
}

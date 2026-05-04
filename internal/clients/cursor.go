package clients

import (
	"os"
	"path/filepath"
)

// NewCursor returns a Client bound to ~/.cursor/mcp.json.
func NewCursor() (Client, error) {
	path, err := ConfigPathForName("cursor")
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       path,
		clientName: "cursor",
		urlField:   "url",
	}
	return &cursorClient{jsonMCPClient: base}, nil
}

type cursorClient struct {
	*jsonMCPClient
}

func (c *cursorClient) Exists() bool {
	if _, err := os.Stat(c.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(c.path))
	return err == nil && st.IsDir()
}

func (c *cursorClient) Backup() (string, error) {
	return c.BackupKeep(0)
}

func (c *cursorClient) BackupKeep(keepN int) (string, error) {
	if _, err := os.Stat(c.path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(c.path), 0755); err != nil {
			return "", err
		}
		if err := os.WriteFile(c.path, []byte("{\n  \"mcpServers\": {}\n}\n"), 0600); err != nil {
			return "", err
		}
	}
	return writeBackup(c.path, c.Name(), keepN)
}

func (c *cursorClient) AddEntry(entry MCPEntry) error {
	m, err := c.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
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

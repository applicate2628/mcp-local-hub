package clients

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// NewClaudeCode returns a Client bound to the current user's ~/.claude/settings.json.
func NewClaudeCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &claudeCode{path: filepath.Join(home, ".claude", "settings.json")}, nil
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
	if !c.Exists() {
		return "", &ErrClientNotInstalled{Client: c.Name()}
	}
	ts := time.Now().Format("20060102-150405")
	bak := c.path + ".bak-mcp-local-hub-" + ts
	in, err := os.Open(c.path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return "", err
	}
	return bak, nil
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
	serverEntry := map[string]any{"url": entry.URL}
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

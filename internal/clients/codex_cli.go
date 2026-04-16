package clients

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

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
	_, err = io.Copy(out, in)
	return bak, err
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

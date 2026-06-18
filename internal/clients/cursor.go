package clients

import (
	"os"
	"path/filepath"
)

// NewCursor returns a Client bound to ~/.cursor/mcp.json.
func NewCursor() (Client, error) {
	path, err := defaultCursorConfigPath()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       path,
		clientName: "cursor",
		urlField:   "url",
	}
	return newLockingClient(&cursorClient{jsonMCPClient: base}), nil
}

// defaultCursorConfigPath returns ~/.cursor/mcp.json. This is the SINGLE
// owner of cursor's config-path derivation: NewCursor calls it directly (the
// adapter's ConfigPath() then surfaces the same value), and ConfigPathForName
// resolves cursor through that adapter — so there is exactly one literal. The
// factory does its own home lookup here (rather than via ConfigPathForName) to
// keep cursor out of the registry-resolver init cycle: ConfigPathForName builds
// the adapter via the factory, so a factory that called ConfigPathForName("cursor")
// would recurse.
func defaultCursorConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "mcp.json"), nil
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
	if dir := filepath.Dir(c.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := c.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(c.path, c.Name(), keepN)
}

// InitEmpty seeds ~/.cursor/mcp.json with `{"mcpServers": {}}` if the
// file is absent. Cursor shares the canonical JSON family schema;
// AddEntry's later merge writes into the same `mcpServers` map.
func (c *cursorClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(c.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

func (c *cursorClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam: Cursor's mcp.json is
	// JSONC (operators hand-edit it), so patch mcpServers.<name> into the
	// original bytes instead of a lossy full-map re-marshal.
	return c.setMember(entry.Name, serverEntry)
}

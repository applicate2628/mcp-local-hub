package clients

import (
	"os"
	"path/filepath"
)

// NewCrush returns a Client bound to Charmbracelet Crush's user-level (global)
// MCP config at ~/.config/crush/crush.json (XDG_CONFIG_HOME-aware).
//
// Crush (a terminal coding agent) reads MCP servers from a JSON file, but under
// the top-level key `mcp` (NOT the JSON family's `mcpServers`) — an object map
// of named entries. Each entry carries a `type` discriminator in
// {stdio, http, sse}: http/sse entries use `url` (+ optional `headers`), stdio
// entries use `command`/`args`/`env`. Crush speaks HTTP MCP natively, so the
// hub writes an HTTP-direct entry (the loopback daemon URL) and IsRelayStdio()
// is false (inherited from jsonMCPClient).
//
// Hub-managed entry shape written:
//
//	"mcp": {
//	  "<server-name>": {
//	    "type": "http",
//	    "url": "http://localhost:9121/mcp"
//	  }
//	}
//
// (plus an optional `headers` object when MCPEntry.Headers is non-empty).
//
// The embedded jsonMCPClient is parameterized with serversKey "mcp", so all the
// inherited read/write seams (GetEntry, RemoveEntry, the comment-preserving
// setMember/deleteMember, AllStdioEntries, the backup/demigrate helpers) operate
// on the `mcp` object map. Only AddEntry (the `type:"http"` discriminator) and
// the filesystem-bootstrap overrides are dedicated.
//
// Config search order (Crush): ./.crush.json, ./crush.json, then the global
// ~/.config/crush/crush.json. This adapter targets the global file — the stable
// home-anchored default the registry's configPath(home) contract requires.
//
// Security note (from Crush docs): crush.json is trusted code — a `$(...)`
// command-substitution in a value runs at config-load time. The hub only ever
// writes a static loopback `url` string, never a substitution, so this adapter
// introduces no such risk; operators hand-editing the file should be aware of
// it independently.
//
// Source (verified 2026-06-17):
//   - https://github.com/charmbracelet/crush — config search order; top-level
//     key `mcp` (object map); per-entry `type` stdio|http|sse with url/headers
//     for http/sse and command/args/env for stdio.
func NewCrush() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultCrushConfigPath(home),
		clientName: "crush",
		serversKey: "mcp",
		urlField:   "url",
	}
	return newLockingClient(&crushClient{jsonMCPClient: base}), nil
}

// defaultCrushConfigPath returns the global ~/.config/crush/crush.json,
// honoring XDG_CONFIG_HOME. Takes `home` (the registry's configPath(home)
// contract) so ConfigPathForName and the adapter resolve identically.
func defaultCrushConfigPath(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "crush", "crush.json")
	}
	return filepath.Join(home, ".config", "crush", "crush.json")
}

// crushClient overrides AddEntry (the `type:"http"` discriminator) and the
// filesystem-bootstrap methods (Exists, Backup, BackupKeep). All other methods
// are promoted from the embedded jsonMCPClient (parameterized with serversKey
// "mcp") unchanged.
type crushClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.config/crush) does.
func (c *crushClient) Exists() bool {
	if _, err := os.Stat(c.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(c.path))
	return err == nil && st.IsDir()
}

func (c *crushClient) Backup() (string, error) {
	return c.BackupKeep(0)
}

// BackupKeep ensures the parent directory exists, seeds an empty `{"mcp": {}}`
// stub (via the parameterized InitEmpty) if the config is absent, then writes
// the timestamped backup (pruning to keepN).
func (c *crushClient) BackupKeep(keepN int) (string, error) {
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

// AddEntry writes the hub-managed remote entry under `mcp` with Crush's HTTP
// shape (`type:"http"` + `url`, plus optional `headers`).
func (c *crushClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	return c.setMember(entry.Name, serverEntry)
}

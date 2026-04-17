package clients

import (
	"os"
	"path/filepath"
)

// NewAntigravity returns a Client bound to ~/.gemini/antigravity/mcp_config.json.
//
// Antigravity is a Gemini-CLI fork shipped inside Google's Antigravity IDE
// (Cascade agent). Its HTTP MCP schema diverges from Gemini CLI 0.38+:
// Antigravity uses `serverUrl` (NOT `url`), auto-detects HTTP transport by
// the presence of that field (no `type` field), and does NOT store a
// per-entry `timeout` for HTTP servers. Only `disabled` and optional
// `headers` accompany `serverUrl`.
//
// Verified empirically against an existing HTTP entry (context7) in a live
// ~/.gemini/antigravity/mcp_config.json. An entry using the Gemini-style
// {url, type:"http", timeout} is silently dropped by the Cascade agent's
// RefreshMcpServers loader even though the file parses as valid JSON.
func NewAntigravity() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		clientName: "antigravity",
		// urlField is set to "serverUrl" so that any fallthrough to the base
		// reader/writer stays consistent with the overridden methods.
		urlField: "serverUrl",
	}
	return &antigravityClient{jsonMCPClient: base}, nil
}

// antigravityClient overrides AddEntry/GetEntry to emit Antigravity's
// specific HTTP schema. Backup, Restore, RemoveEntry, Name, ConfigPath,
// Exists are promoted from the embedded jsonMCPClient unchanged.
type antigravityClient struct {
	*jsonMCPClient
}

func (a *antigravityClient) AddEntry(entry MCPEntry) error {
	m, err := a.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	// Antigravity HTTP schema: serverUrl + disabled, with optional headers.
	// No `type` (transport inferred from serverUrl presence), no `timeout`.
	serverEntry := map[string]any{
		"serverUrl": entry.URL,
		"disabled":  false,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	servers[entry.Name] = serverEntry
	m["mcpServers"] = servers
	return a.writeJSON(m)
}

func (a *antigravityClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := a.readJSON()
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
	url, _ := raw["serverUrl"].(string)
	return &MCPEntry{Name: name, URL: url}, nil
}

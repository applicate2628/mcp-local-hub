package clients

import (
	"os"
	"path/filepath"
)

// NewAntigravity returns a Client bound to ~/.gemini/antigravity/mcp_config.json.
//
// Antigravity is a Gemini-CLI derivative shipped inside Google's Antigravity IDE.
// For HTTP servers we write Gemini's modern schema (url + type:"http" + timeout)
// plus Antigravity's traditional `disabled` flag — their stdio entries have
// always carried `disabled`, so including it here keeps the entry shape
// consistent with the rest of mcp_config.json. Surplus fields are harmless
// for a reader that only cares about url/type.
func NewAntigravity() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		clientName: "antigravity",
		urlField:   "url",
	}
	return &antigravityClient{jsonMCPClient: base}, nil
}

// antigravityClient overrides AddEntry/GetEntry to emit the modern
// HTTP schema. Backup, Restore, RemoveEntry, Name, ConfigPath, Exists
// are promoted from the embedded jsonMCPClient unchanged.
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
	serverEntry := map[string]any{
		"url":      entry.URL,
		"type":     "http",
		"timeout":  defaultGeminiHTTPTimeoutMs,
		"disabled": false,
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
	url, _ := raw["url"].(string)
	return &MCPEntry{Name: name, URL: url}, nil
}

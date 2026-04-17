package clients

import (
	"os"
	"path/filepath"
)

// NewGeminiCLI returns a Client bound to ~/.gemini/settings.json using the
// modern HTTP entry schema that Gemini CLI 0.38+ writes and reads.
//
// The modern schema stores HTTP servers as:
//
//	{ "url": "http://...", "type": "http", "timeout": <ms> }
//
// The legacy schema (httpUrl + disabled) shown in older docs is NOT what
// `gemini mcp add --transport http` emits — verified empirically by
// round-tripping a test entry through the CLI.
func NewGeminiCLI() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".gemini", "settings.json"),
		clientName: "gemini-cli",
		// urlField is set to "url" so that if the base readers/writers are ever
		// invoked directly they stay consistent with the overridden methods.
		urlField: "url",
	}
	return &geminiCLI{jsonMCPClient: base}, nil
}

// geminiCLI overrides AddEntry and GetEntry to use Gemini CLI 0.38+'s HTTP
// schema. Backup/Restore/RemoveEntry/Name/ConfigPath/Exists are promoted
// from the embedded jsonMCPClient unchanged.
type geminiCLI struct {
	*jsonMCPClient
}

// defaultGeminiHTTPTimeoutMs matches what `gemini mcp add --transport http`
// writes when --timeout is not specified (CLI default).
const defaultGeminiHTTPTimeoutMs = 10000

func (g *geminiCLI) AddEntry(entry MCPEntry) error {
	m, err := g.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	serverEntry := map[string]any{
		"url":     entry.URL,
		"type":    "http",
		"timeout": defaultGeminiHTTPTimeoutMs,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	servers[entry.Name] = serverEntry
	m["mcpServers"] = servers
	return g.writeJSON(m)
}

func (g *geminiCLI) GetEntry(name string) (*MCPEntry, error) {
	m, err := g.readJSON()
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

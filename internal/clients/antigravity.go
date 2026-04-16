package clients

import (
	"os"
	"path/filepath"
)

// NewAntigravity returns a Client bound to ~/.gemini/antigravity/mcp_config.json.
func NewAntigravity() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &jsonMCPClient{
		path:       filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		clientName: "antigravity",
		urlField:   "httpUrl",
	}, nil
}

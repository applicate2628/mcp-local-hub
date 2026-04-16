package clients

import (
	"os"
	"path/filepath"
)

// NewGeminiCLI returns a Client bound to ~/.gemini/settings.json.
func NewGeminiCLI() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &jsonMCPClient{
		path:       filepath.Join(home, ".gemini", "settings.json"),
		clientName: "gemini-cli",
		urlField:   "httpUrl",
	}, nil
}

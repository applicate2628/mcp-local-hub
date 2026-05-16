package clients

import (
	"os"
	"path/filepath"
)

// NewQwenCLI returns a Client bound to ~/.qwen/settings.json.
func NewQwenCLI() (Client, error) {
	path, err := ConfigPathForName("qwen-cli")
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       path,
		clientName: "qwen-cli",
		urlField:   "httpUrl",
	}
	return &qwenCLI{jsonMCPClient: base}, nil
}

type qwenCLI struct {
	*jsonMCPClient
}

const defaultQwenHTTPTimeoutMs = 10000

func (q *qwenCLI) Exists() bool {
	if _, err := os.Stat(q.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(q.path))
	return err == nil && st.IsDir()
}

func (q *qwenCLI) Backup() (string, error) {
	return q.BackupKeep(0)
}

func (q *qwenCLI) BackupKeep(keepN int) (string, error) {
	if err := q.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(q.path, q.Name(), keepN)
}

// InitEmpty seeds ~/.qwen/settings.json with `{"mcpServers": {}}` if
// the file is absent. Qwen CLI shares the canonical JSON family
// schema; AddEntry's later merge writes into the same `mcpServers`
// map.
func (q *qwenCLI) InitEmpty() error {
	return EnsureClientConfigStub(q.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

func (q *qwenCLI) AddEntry(entry MCPEntry) error {
	m, err := q.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	serverEntry := map[string]any{
		"httpUrl": entry.URL,
		"timeout": defaultQwenHTTPTimeoutMs,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	servers[entry.Name] = serverEntry
	m["mcpServers"] = servers
	return q.writeJSON(m)
}

func (q *qwenCLI) GetEntry(name string) (*MCPEntry, error) {
	m, err := q.readJSON()
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
	url, _ := raw["httpUrl"].(string)
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers")}, nil
}

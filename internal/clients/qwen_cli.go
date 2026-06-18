package clients

import (
	"os"
	"path/filepath"
)

// NewQwenCLI returns a Client bound to ~/.qwen/settings.json.
func NewQwenCLI() (Client, error) {
	path, err := defaultQwenCLIConfigPath()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       path,
		clientName: "qwen-cli",
		urlField:   "httpUrl",
	}
	return newLockingClient(&qwenCLI{jsonMCPClient: base}), nil
}

// defaultQwenCLIConfigPath returns ~/.qwen/settings.json. Single owner of
// qwen-cli's config-path derivation; see defaultCursorConfigPath for why the
// factory resolves home here directly instead of via ConfigPathForName (init
// cycle: the resolver builds the adapter via this factory).
func defaultQwenCLIConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".qwen", "settings.json"), nil
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
	if dir := filepath.Dir(q.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := q.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(q.path, q.Name(), keepN)
}

// InitEmpty seeds ~/.qwen/settings.json with `{"mcpServers": {}}` if
// the file is absent. Qwen CLI shares the canonical JSON family
// schema; AddEntry's later merge writes into the same `mcpServers`
// map.
func (q *qwenCLI) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(q.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

func (q *qwenCLI) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"httpUrl": entry.URL,
		"timeout": defaultQwenHTTPTimeoutMs,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return q.setMember(entry.Name, serverEntry)
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

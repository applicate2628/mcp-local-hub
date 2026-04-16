package clients

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// jsonMCPClient is a reusable struct that handles JSON-format MCP configs
// with the `mcpServers.<name>.httpUrl` schema shared by Gemini CLI and Antigravity.
// clientName and urlField distinguish the two (field name is "httpUrl" for both,
// kept parameterized in case a future client uses a different field).
type jsonMCPClient struct {
	path       string
	clientName string
	urlField   string // "httpUrl" for both known cases
}

func (j *jsonMCPClient) Name() string       { return j.clientName }
func (j *jsonMCPClient) ConfigPath() string { return j.path }

func (j *jsonMCPClient) Exists() bool {
	_, err := os.Stat(j.path)
	return err == nil
}

func (j *jsonMCPClient) Backup() (string, error) {
	if !j.Exists() {
		return "", &ErrClientNotInstalled{Client: j.clientName}
	}
	ts := time.Now().Format("20060102-150405")
	bak := j.path + ".bak-mcp-local-hub-" + ts
	in, err := os.Open(j.path)
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

func (j *jsonMCPClient) Restore(backupPath string) error {
	in, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(j.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func (j *jsonMCPClient) readJSON() (map[string]any, error) {
	data, err := os.ReadFile(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", j.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (j *jsonMCPClient) writeJSON(m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(j.path, append(out, '\n'), 0600)
}

func (j *jsonMCPClient) AddEntry(entry MCPEntry) error {
	m, err := j.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	serverEntry := map[string]any{
		j.urlField: entry.URL,
		"disabled": false,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	servers[entry.Name] = serverEntry
	m["mcpServers"] = servers
	return j.writeJSON(m)
}

func (j *jsonMCPClient) RemoveEntry(name string) error {
	m, err := j.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m["mcpServers"] = servers
	return j.writeJSON(m)
}

func (j *jsonMCPClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := j.readJSON()
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
	url, _ := raw[j.urlField].(string)
	return &MCPEntry{Name: name, URL: url}, nil
}

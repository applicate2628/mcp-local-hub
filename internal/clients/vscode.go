package clients

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// NewVSCode returns a Client bound to VS Code's default user-profile mcp.json.
func NewVSCode() (Client, error) {
	path, err := ConfigPathForName("vscode")
	if err != nil {
		return nil, err
	}
	return &vscodeClient{path: path}, nil
}

type vscodeClient struct {
	path string
}

func (v *vscodeClient) Name() string       { return "vscode" }
func (v *vscodeClient) ConfigPath() string { return v.path }

func (v *vscodeClient) Exists() bool {
	if _, err := os.Stat(v.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(v.path))
	return err == nil && st.IsDir()
}

func (v *vscodeClient) Backup() (string, error) {
	return writeBackup(v.path, v.Name(), 0)
}

func (v *vscodeClient) BackupKeep(keepN int) (string, error) {
	// Explicit MkdirAll before InitEmpty: EnsureClientConfigStub no
	// longer creates parents (v0.4.5 deep-sec Lane A #1) so the
	// adapter's seed-then-backup contract must ensure the parent
	// exists for fresh hosts.
	if dir := filepath.Dir(v.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := v.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(v.path, v.Name(), keepN)
}

// InitEmpty seeds %APPDATA%\Code\User\mcp.json with VS Code's
// `{"servers": {}}` top-level shape if the file is absent. VS Code
// 1.103+ migrated MCP entries to a top-level `servers` key (NOT
// `mcpServers`); the stub matches that schema so AddEntry's later
// merge writes into the right map.
func (v *vscodeClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(v.path, []byte("{\n  \"servers\": {}\n}\n"))
}

func (v *vscodeClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so
	// production restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(v.path, data)
}

func (v *vscodeClient) readJSON() (map[string]any, error) {
	data, err := os.ReadFile(v.path)
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
		return nil, fmt.Errorf("parse %s: %w", v.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (v *vscodeClient) writeJSON(m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative + DACL-bound)
	// for token-bearing rewrites; tests get the os.WriteFile fallback.
	return WriteConfigFile(v.path, append(out, '\n'))
}

func (v *vscodeClient) AddEntry(entry MCPEntry) error {
	m, err := v.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	servers[entry.Name] = serverEntry
	m["servers"] = servers
	return v.writeJSON(m)
}

func (v *vscodeClient) RemoveEntry(name string) error {
	m, err := v.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["servers"].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m["servers"] = servers
	return v.writeJSON(m)
}

func (v *vscodeClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := v.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["servers"].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	url, _ := raw["url"].(string)
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers")}, nil
}

func (v *vscodeClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(v.path, v.Name())
}

func (v *vscodeClient) RestoreEntryFromBackup(backupPath, name string) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	var backupMap map[string]any
	if len(backupData) == 0 {
		backupMap = map[string]any{}
	} else if err := json.Unmarshal(backupData, &backupMap); err != nil {
		return fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	backupServers, _ := backupMap["servers"].(map[string]any)
	liveMap, err := v.readJSON()
	if err != nil {
		return err
	}
	liveServers, _ := liveMap["servers"].(map[string]any)
	if liveServers == nil {
		liveServers = map[string]any{}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			if rawMap, ok := backupEntry.(map[string]any); ok {
				if urlStr, _ := rawMap["url"].(string); IsHubHTTPURL(urlStr) {
					if _, hasCmd := rawMap["command"]; !hasCmd {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap["servers"] = liveServers
			return v.writeJSON(liveMap)
		}
	}
	delete(liveServers, name)
	liveMap["servers"] = liveServers
	return v.writeJSON(liveMap)
}

// AllStdioEntries returns every stdio entry from VS Code's
// top-level `servers` key (different from the JSON family's
// `mcpServers`).
func (v *vscodeClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := v.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["servers"].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans `servers` for stdio entries
// matching the mcp-language-server invocation pattern. VS Code uses
// the top-level `servers` key (NOT `mcpServers`) and supports stdio
// entries with `command`/`args`.
func (v *vscodeClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := v.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["servers"].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

func (v *vscodeClient) BackupContainsEntry(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m["servers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

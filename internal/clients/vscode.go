package clients

import (
	"encoding/json"
	"fmt"
	"io"
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
	if _, err := os.Stat(v.path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(v.path), 0755); err != nil {
			return "", err
		}
		if err := os.WriteFile(v.path, []byte("{\n  \"servers\": {}\n}\n"), 0600); err != nil {
			return "", err
		}
	}
	return writeBackup(v.path, v.Name(), keepN)
}

func (v *vscodeClient) Restore(backupPath string) error {
	in, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(v.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
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
	if err := os.MkdirAll(filepath.Dir(v.path), 0755); err != nil {
		return err
	}
	return os.WriteFile(v.path, append(out, '\n'), 0600)
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
	return &MCPEntry{Name: name, URL: url}, nil
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

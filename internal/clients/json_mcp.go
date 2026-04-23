package clients

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	return writeBackup(j.path, j.clientName, 0)
}

func (j *jsonMCPClient) BackupKeep(keepN int) (string, error) {
	return writeBackup(j.path, j.clientName, keepN)
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

// LatestBackupPath delegates to the shared helper.
func (j *jsonMCPClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(j.path, j.clientName)
}

// RestoreEntryFromBackup reads the JSON backup, extracts mcpServers[name]
// (if present), and writes it (or removes the current live entry) to
// the live config. Other entries in mcpServers are untouched.
// Inherited by geminiCLI and antigravityClient via struct embedding.
//
// Defensively refuses if the backup's copy of the named entry is
// already in hub-managed shape. Shape detection is adapter-specific:
//   - For Gemini CLI (urlField = "url"): entry has `url` and no `command`.
//   - For Antigravity (urlField = "command"): entry's `command` is the
//     mcphub binary AND args[0] == "relay".
//
// Both paths return ErrBackupEntryAlreadyMigrated so Demigrate can
// surface a clear operator-facing failure row.
func (j *jsonMCPClient) RestoreEntryFromBackup(backupPath, name string) error {
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
	backupServers, _ := backupMap["mcpServers"].(map[string]any)
	liveMap, err := j.readJSON()
	if err != nil {
		return err
	}
	liveServers, _ := liveMap["mcpServers"].(map[string]any)
	if liveServers == nil {
		liveServers = map[string]any{}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			if rawMap, ok := backupEntry.(map[string]any); ok {
				if j.urlField == "url" {
					// Gemini CLI hub-HTTP shape: loopback `url`
					// (http://localhost:<port>/) present, `command`
					// absent. User-configured remote HTTP entries
					// pass through.
					if urlStr, _ := rawMap["url"].(string); isHubHTTPURL(urlStr) {
						if _, hasCmd := rawMap["command"]; !hasCmd {
							return ErrBackupEntryAlreadyMigrated
						}
					}
				} else {
					// Antigravity hub-relay shape: command is mcphub,
					// args[0] == "relay".
					if cmd, _ := rawMap["command"].(string); IsMcphubBinary(cmd) {
						if args, ok := rawMap["args"].([]any); ok && len(args) > 0 {
							if first, _ := args[0].(string); first == "relay" {
								return ErrBackupEntryAlreadyMigrated
							}
						}
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap["mcpServers"] = liveServers
			return j.writeJSON(liveMap)
		}
	}
	delete(liveServers, name)
	liveMap["mcpServers"] = liveServers
	return j.writeJSON(liveMap)
}

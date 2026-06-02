package clients

import (
	"encoding/json"
	"fmt"
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

// InitEmpty seeds the JSON family config file with `{"mcpServers": {}}`
// if it is absent. Inherited by gemini-cli (~/.gemini/settings.json)
// and the Antigravity adapter (~/.gemini/antigravity/mcp_config.json).
// Both write subsequent entries into the same top-level `mcpServers`
// map that AddEntry merges into.
func (j *jsonMCPClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(j.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

func (j *jsonMCPClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so
	// production restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(j.path, data)
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
	// WriteConfigFile creates parent dirs as needed (the test fallback
	// does its own MkdirAll; production's SecureWriteClientConfig
	// requires the parent dir to exist + pass the DACL allowlist gate,
	// which install paths arrange via `clients.ConfigPathForName` +
	// per-adapter directory bootstrap before calling AddEntry).
	return WriteConfigFile(j.path, append(out, '\n'))
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
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers")}, nil
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
//   - For URL-native JSON clients (urlField = "url" or "httpUrl"): entry
//     has a hub loopback URL and no `command`.
//   - For Antigravity (urlField = "command"): entry's `command` is the
//     mcphub binary AND args[0] == "relay".
//
// Both paths return ErrBackupEntryAlreadyMigrated so Demigrate can
// surface a clear operator-facing failure row.
func (j *jsonMCPClient) RestoreEntryFromBackup(backupPath, name string) error {
	return j.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface
// doc on Client.RestoreEntryFromBackupForRollback). Used only by the
// serena dynamic-pool migrate abort-rollback. Inherited by geminiCLI,
// qwenCLI, cursorClient, and antigravityClient via struct embedding —
// which is exactly why the serena reconcile (which rewrites all of those)
// can roll each one back through this one method.
func (j *jsonMCPClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return j.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a backup entry already in hub-managed shape
// (hub-HTTP loopback URL for URL clients; mcphub `relay` invocation for
// Antigravity) with ErrBackupEntryAlreadyMigrated; when true (migrate
// rollback) it writes the backup bytes verbatim regardless of shape.
func (j *jsonMCPClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
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
			// Defensive guard (demigrate flow only — the rollback
			// caller passes allowHubEntry=true to restore the
			// pre-reconcile legacy hub entry verbatim).
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if j.backupEntryMapIsHubManaged(rawMap) {
						return ErrBackupEntryAlreadyMigrated
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

// AllStdioEntries returns every stdio entry from mcpServers.
// Inherited by geminiCLI, qwenCLI, cursorClient, and antigravityClient
// via struct embedding. Antigravity entries DO surface here because
// they have `command='mcphub'`; the cleanup pipeline filters them
// out via isOurOwnProcess, so the inheritance is safe.
func (j *jsonMCPClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := j.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans mcpServers for stdio entries
// matching the mcp-language-server invocation pattern. Inherited by
// geminiCLI, qwenCLI, cursorClient, and antigravityClient via struct
// embedding. Antigravity entries never match because their `command`
// resolves to mcphub, not mcp-language-server.
func (j *jsonMCPClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := j.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

// BackupContainsEntry reports whether the backup file at backupPath
// has an mcpServers[name] entry. Inherited by both geminiCLI and
// antigravityClient via struct embedding.
func (j *jsonMCPClient) BackupContainsEntry(backupPath, name string) (bool, error) {
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
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	// Require the entry to be an object — a scalar value at this
	// key would be malformed and, if fed to RestoreEntryFromBackup,
	// would corrupt the live config. Treat non-object values as
	// absent so the sentinel fallback refuses with a clear error.
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// backupEntryMapIsHubManaged classifies one parsed entry map by this
// adapter's hub shape. URL-native variants (urlField "url"/"httpUrl",
// inherited by geminiCLI, qwenCLI, cursorClient) use the hub-HTTP shape;
// the Antigravity variant (urlField "command") uses the hub-relay shape.
// Single owner of the per-adapter shape decision shared by
// restoreEntryFromBackup and BackupEntryIsHubManaged.
func (j *jsonMCPClient) backupEntryMapIsHubManaged(rawMap map[string]any) bool {
	if j.urlField == "url" || j.urlField == "httpUrl" {
		return isHubURLShapeEntry(rawMap, j.urlField)
	}
	return isHubRelayShapeEntry(rawMap)
}

// BackupEntryIsHubManaged reports whether mcpServers[name] in the backup
// at backupPath is in this adapter's hub-managed shape. Inherited by
// geminiCLI, qwenCLI, cursorClient, and antigravityClient via struct
// embedding (the antigravity variant routes to the relay-shape branch
// because its urlField is "command"). See Client.BackupEntryIsHubManaged.
func (j *jsonMCPClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
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
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return j.backupEntryMapIsHubManaged(entry), nil
}

package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewVSCode returns a Client bound to VS Code's default user-profile mcp.json.
//
// The factory resolves home and calls defaultVSCodeConfigPath directly (rather
// than via ConfigPathForName) to stay out of the registry-resolver init cycle:
// ConfigPathForName builds the adapter through this factory, so a factory that
// re-entered ConfigPathForName("vscode") would recurse. defaultVSCodeConfigPath
// is the single owner of the path derivation, shared by the factory and (via
// the constructed adapter's ConfigPath()) the resolver.
func NewVSCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&vscodeClient{path: defaultVSCodeConfigPath(home)}), nil
}

type vscodeClient struct {
	path string
}

// vscodeServersKey is the single owner of VS Code's top-level MCP section name
// (the 1.103+ `servers` key, NOT the JSON family's `mcpServers`). Every method
// that reaches into the parsed config map uses it.
const vscodeServersKey = "servers"

func (v *vscodeClient) Name() string       { return "vscode" }
func (v *vscodeClient) ConfigPath() string { return v.path }

// IsRelayStdio reports false: vscode is a URL-native HTTP MCP client.
func (v *vscodeClient) IsRelayStdio() bool { return false }

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
//
// VS Code's mcp.json is JSONC (it allows `//` + `/* */` comments and
// trailing commas, and operators hand-edit it): the read path parses
// comments via the shared JSONC helper, and AddEntry/RemoveEntry patch
// through hujson so the operator's comments and unrelated top-level keys
// are PRESERVED on every write — no longer the lossy encoding/json
// round-trip.
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
	data, err := readRawConfig(v.path)
	if err != nil {
		return nil, err
	}
	// VS Code's mcp.json is JSONC (it allows `//` + `/* */` comments and
	// trailing commas, and operators hand-edit it) — parse via the
	// comment-tolerant shared helper so a comment in the file does not break
	// migrate / Init.
	m, err := parseJSONCBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", v.path, err)
	}
	return m, nil
}

// setMember sets servers.<name> = value, and deleteMember removes it, both
// preserving the operator's comments + unrelated top-level keys when the file
// already has JSONC content. An empty/absent file falls back to a clean
// indented marshal. The bytes route through the UNCHANGED WriteConfigFile
// pipeline.
func (v *vscodeClient) setMember(name string, value any) error {
	return mutateJSONObjectMember(v.path, vscodeServersKey, name, value, false)
}

func (v *vscodeClient) deleteMember(name string) error {
	return mutateJSONObjectMember(v.path, vscodeServersKey, name, nil, true)
}

func (v *vscodeClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set: patches servers.<name> into the original on-disk
	// bytes via hujson so the operator's comments and unrelated keys survive (a
	// full map re-marshal would drop both).
	return v.setMember(entry.Name, serverEntry)
}

func (v *vscodeClient) RemoveEntry(name string) error {
	// Comment-preserving delete; absence is a no-op.
	return v.deleteMember(name)
}

func (v *vscodeClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := v.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[vscodeServersKey].(map[string]any)
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
	return v.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface
// doc on Client.RestoreEntryFromBackupForRollback). Install rollback and
// Serena migrate rollback use it when the timestamped backup is the source of
// truth.
func (v *vscodeClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return v.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a hub-HTTP-shaped backup entry with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes
// the backup bytes verbatim regardless of shape.
func (v *vscodeClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	// os.ReadFile (NOT readRawConfig): a named backup that is missing is a
	// genuine read error the demigrate caller must see, not a silent
	// treat-as-empty. Empty / comment-only / malformed bytes are then
	// classified by parseJSONCBytes (empty map vs parse error).
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	backupMap, err := parseJSONCBytes(backupData)
	if err != nil {
		return fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	backupServers, _ := backupMap[vscodeServersKey].(map[string]any)
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive guard (demigrate flow only — the rollback caller
			// passes allowHubEntry=true to restore the pre-reconcile
			// legacy hub entry verbatim).
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubURLShapeEntry(rawMap, "url") {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			// Comment-preserving set into the LIVE config (its comments +
			// unrelated keys survive; the backup's entry VALUE is written).
			return v.setMember(name, backupEntry)
		}
	}
	return v.deleteMember(name)
}

// AllStdioEntries returns every stdio entry from VS Code's
// top-level `servers` key (different from the JSON family's
// `mcpServers`).
func (v *vscodeClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := v.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[vscodeServersKey].(map[string]any)
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
	servers, _ := m[vscodeServersKey].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

func (v *vscodeClient) BackupContainsEntry(backupPath, name string) (bool, error) {
	// os.ReadFile (NOT readRawConfig): a missing named backup is a read error,
	// not a silent (false, nil); empty bytes parse to an empty map below.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[vscodeServersKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether servers[name] in VS Code's
// mcp.json backup at backupPath is in the hub-HTTP shape (loopback
// `url` present, `command` absent). VS Code uses the top-level `servers`
// key (NOT `mcpServers`). See Client.BackupEntryIsHubManaged.
func (v *vscodeClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	// os.ReadFile (NOT readRawConfig): a missing named backup is a read error
	// the demigrate caller must see, not a silent (false, nil); empty bytes
	// parse to an empty map below.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[vscodeServersKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}

package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewAmazonQ returns a Client bound to Amazon Q Developer CLI's global MCP
// config at ~/.aws/amazonq/mcp.json.
//
// Amazon Q Developer reads MCP servers from a small set of JSON files that
// share one schema. The hub writes the GLOBAL ~/.aws/amazonq/mcp.json file
// (servers available across every project), matching every other adapter's
// user-scoped posture; Q merges the global file with the workspace-level
// .amazonq/mcp.json at load time (workspace wins on conflict), so a global
// hub entry is visible in all workspaces. Legacy mcp.json support is enabled
// by the `useLegacyMcpJson` field in the global default.json config, which
// defaults to true, and the Q CLI reads mcp.json directly.
//
// The file uses the canonical `{"mcpServers": {...}}` JSON family schema.
// Amazon Q speaks HTTP MCP natively (HTTP-direct, not relay-stdio): a remote
// server entry is `{"type": "http", "url": "https://...", "headers": {...}}`.
// This is the SAME shape claude-code writes (explicit `type: "http"` plus
// `url`), NOT the no-`type` shape Kiro uses — so this adapter mirrors
// claude_code.go's AddEntry rather than embedding the base jsonMCPClient
// (whose AddEntry writes `{url, disabled:false}` with no `type` and would not
// match Amazon Q's documented remote entry).
//
// The parent directory ~/.aws/amazonq is two levels deep and does not exist
// on a clean install, so the Backup/BackupKeep wrappers MkdirAll it first,
// mirroring the Kiro adapter's nested-directory bootstrap.
//
// Source: AWS docs, "MCP configuration in the CLI"
// (https://docs.aws.amazon.com/amazonq/latest/qdeveloper-ug/command-line-mcp-config-CLI.html)
// — remote MCP servers configured with the `type` and `url` fields under
// `mcpServers`; and "MCP configuration for Q Developer in the IDE"
// (https://docs.aws.amazon.com/amazonq/latest/qdeveloper-ug/mcp-ide.html) —
// global scope ~/.aws/amazonq/mcp.json (legacy, still read by the Q CLI).
func NewAmazonQ() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&amazonQ{path: filepath.Join(home, ".aws", "amazonq", "mcp.json")}), nil
}

type amazonQ struct {
	path string
}

// amazonQMCPServersKey is the single owner of amazon-q's top-level MCP
// section name (the canonical JSON family `mcpServers` key). Every method
// that reaches into the parsed config map uses it.
const amazonQMCPServersKey = "mcpServers"

func (c *amazonQ) Name() string       { return "amazon-q" }
func (c *amazonQ) ConfigPath() string { return c.path }

// IsRelayStdio reports false: amazon-q is a URL-native HTTP MCP client.
func (c *amazonQ) IsRelayStdio() bool { return false }

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.aws/amazonq) does — mirroring the kiro/cursor/qwen
// "directory means installed" heuristic so an operator who has Amazon Q
// installed but no MCP config yet still gets the Initialize / install
// affordance.
func (c *amazonQ) Exists() bool {
	if _, err := os.Stat(c.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(c.path))
	return err == nil && st.IsDir()
}

func (c *amazonQ) Backup() (string, error) {
	return c.BackupKeep(0)
}

// BackupKeep ensures the nested ~/.aws/amazonq parent directory exists, seeds
// an empty mcpServers stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir is two levels deep
// (.aws/amazonq) and does not exist on a clean install, so the MkdirAll here
// is load-bearing — without it writeBackup/InitEmpty would fail on a fresh
// host. Mirrors the kiro adapter's nested-directory bootstrap.
func (c *amazonQ) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(c.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := c.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(c.path, c.Name(), keepN)
}

// InitEmpty seeds ~/.aws/amazonq/mcp.json with `{"mcpServers": {}}` if the
// file is absent. amazon-q shares the canonical JSON family `mcpServers` key;
// AddEntry's later merge writes into the same map.
func (c *amazonQ) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(c.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

func (c *amazonQ) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline (handle-relative
	// + DACL-bound). The backup file is read in full, then handed to the
	// writer as a byte slice.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(c.path, data)
}

// readJSON keeps unknown top-level fields untouched by parsing through
// map[string]any. The bytes are parsed JSONC-tolerantly so a `//` comment or
// trailing comma in a hand-edited mcp.json does not break migrate / Init.
func (c *amazonQ) readJSON() (map[string]any, error) {
	data, err := readRawConfig(c.path)
	if err != nil {
		return nil, err
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.path, err)
	}
	return m, nil
}

// setMember sets mcpServers.<name> = value, and deleteMember removes it, both
// preserving the operator's comments + unrelated top-level keys when the file
// already has JSONC content. An empty/absent file falls back to a clean
// indented marshal. The bytes route through the UNCHANGED WriteConfigFile
// pipeline.
func (c *amazonQ) setMember(name string, value any) error {
	return mutateJSONObjectMember(c.path, amazonQMCPServersKey, name, value, false)
}

func (c *amazonQ) deleteMember(name string) error {
	return mutateJSONObjectMember(c.path, amazonQMCPServersKey, name, nil, true)
}

func (c *amazonQ) AddEntry(entry MCPEntry) error {
	// Amazon Q's remote-server schema requires an explicit `type` field; the
	// documented HTTP form is `{"type": "http", "url": ...}` (with optional
	// `headers`). This adapter only produces URL-backed entries, so type is
	// hardcoded "http" here — identical to claude-code's shape.
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set: patches mcpServers.<name> into the original
	// on-disk bytes via hujson so any comments and unrelated keys survive (a
	// full map re-marshal would drop both).
	return c.setMember(entry.Name, serverEntry)
}

func (c *amazonQ) RemoveEntry(name string) error {
	// Comment-preserving delete; absence is a no-op.
	return c.deleteMember(name)
}

func (c *amazonQ) GetEntry(name string) (*MCPEntry, error) {
	m, err := c.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[amazonQMCPServersKey].(map[string]any)
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

// LatestBackupPath delegates to the shared helper.
func (c *amazonQ) LatestBackupPath() (string, bool, error) {
	return latestBackup(c.path, c.Name())
}

// RestoreEntryFromBackup reads the raw per-name entry from the backup at
// backupPath and writes it (or removes the current live entry, if the backup
// had none) into the live config. Other entries in the live config are
// untouched.
//
// Defensively refuses if the backup's copy of the named entry is already in
// hub-HTTP form (has a loopback `url` field but no `command`). That situation
// arises when the backup was taken AFTER an earlier migrate of the same
// client already rewrote this entry — restoring would silently re-apply
// hub-HTTP data. See ErrBackupEntryAlreadyMigrated.
func (c *amazonQ) RestoreEntryFromBackup(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc on
// Client.RestoreEntryFromBackupForRollback). Install rollback and Serena
// migrate rollback use it when the timestamped backup is the source of truth.
func (c *amazonQ) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false (the
// demigrate flow) it refuses a hub-HTTP-shaped backup entry with
// ErrBackupEntryAlreadyMigrated; when true (the migrate rollback) it writes
// the backup bytes verbatim regardless of shape.
func (c *amazonQ) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
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
	backupServers, _ := backupMap[amazonQMCPServersKey].(map[string]any)
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive: refuse hub-HTTP-shaped backup entries. The canonical
			// hub-HTTP shape in mcp.json has a loopback `url` field
			// (http://localhost:<port>/... or 127.0.0.1) and no `command`
			// field. User-configured remote HTTP MCP servers (url pointing at
			// a non-loopback host) pass through to the normal restore path.
			// The rollback caller (allowHubEntry=true) bypasses this guard to
			// restore the pre-reconcile legacy hub entry verbatim.
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubURLShapeEntry(rawMap, "url") {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			// Comment-preserving set into the LIVE config (its comments +
			// unrelated keys survive; the backup's entry VALUE is written).
			return c.setMember(name, backupEntry)
		}
	}
	return c.deleteMember(name)
}

// AllStdioEntries returns every stdio entry from mcpServers.
func (c *amazonQ) AllStdioEntries() ([]StdioEntry, error) {
	m, err := c.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[amazonQMCPServersKey].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans mcpServers for stdio entries matching
// the mcp-language-server invocation pattern.
func (c *amazonQ) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := c.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[amazonQMCPServersKey].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

// BackupContainsEntry reports whether the backup file at backupPath has an
// mcpServers[name] entry.
func (c *amazonQ) BackupContainsEntry(backupPath, name string) (bool, error) {
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
	servers, _ := m[amazonQMCPServersKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	// Require the entry to be an object. A scalar (string, number, bool) or
	// null passes the "key present" check but would corrupt the live config
	// if fed back through RestoreEntryFromBackup — treat as absent so sentinel
	// fallback refuses rather than silently writes malformed data.
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether mcpServers[name] in the mcp.json
// backup at backupPath is in amazon-q's hub-HTTP shape (loopback `url`
// present, `command` absent). See Client.BackupEntryIsHubManaged.
func (c *amazonQ) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
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
	servers, _ := m[amazonQMCPServersKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}

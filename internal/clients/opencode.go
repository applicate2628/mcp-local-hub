package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewOpenCode returns a Client bound to OpenCode's global config file.
//
// OpenCode (https://opencode.ai, github.com/sst/opencode) is a real,
// current terminal-based AI coding agent with first-class MCP support. It
// reads MCP server definitions from the top-level `mcp` object of its JSON
// config. Two config scopes exist:
//
//   - Global: ~/.config/opencode/opencode.json (also accepts a `.jsonc`
//     variant). On every OS OpenCode resolves the global config from
//     ~/.config/opencode/ — it does NOT follow the Windows %APPDATA% /
//     macOS ~/Library convention. (Verified against the official config
//     resolution docs.)
//   - Project: opencode.json in the repository root (highest precedence,
//     merged over global at load time).
//
// The hub writes the GLOBAL file so a single per-user hub entry is visible
// in every project, matching every other adapter's user-scoped posture.
//
// Transport choice — HTTP-direct (NOT relay-stdio). OpenCode supports
// remote MCP servers natively over Streamable HTTP. A remote entry is
// keyed by server name under `mcp` and discriminated by `"type":"remote"`
// with a `url` endpoint and an `enabled` flag:
//
//	{
//	  "mcp": {
//	    "<server-name>": {
//	      "type": "remote",
//	      "url": "http://localhost:9121/mcp",
//	      "enabled": true
//	    }
//	  }
//	}
//
// (Local stdio servers use "type":"local" with a `command` ARRAY instead;
// the hub never writes that shape because the daemon is already an HTTP
// endpoint.) Optional `headers` is emitted when MCPEntry.Headers is
// non-empty.
//
// Sources (verified 2026-06):
//   - https://opencode.ai/docs/mcp-servers/ — `mcp` top-level key; local
//     ("type":"local", `command` array, `environment`, `enabled`) and
//     remote ("type":"remote", `url`, `headers`, `enabled`) entry shapes.
//   - https://opencode.ai/docs/config/ — global config at
//     ~/.config/opencode/opencode.json, project config opencode.json in
//     repo root; remote `type` discriminator value is "remote".
//
// IMPORTANT divergences from the JSON family (mcpServers + disabled:false):
//   - top-level key is `mcp`, NOT `mcpServers` — so this is a standalone
//     struct (like vscode/zed), not an embedding of jsonMCPClient.
//   - the active flag is `enabled` (true = on), NOT `disabled` (false =
//     on). The hub writes `enabled:true`.
//   - remote entries carry `"type":"remote"`.
//
// The hub-shape guard (isHubURLShapeEntry) keys off the `url` field and
// the absence of a `command` key, which holds for OpenCode's remote entry
// shape, so demigrate/rollback recognize a hub-managed OpenCode entry.
func NewOpenCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&openCodeClient{path: defaultOpenCodeConfigPath(home)}), nil
}

// defaultOpenCodeConfigPath returns the global OpenCode config path.
// OpenCode uses the XDG-style ~/.config/opencode/ location on every OS
// (Windows included — it does not switch to %APPDATA%). XDG_CONFIG_HOME is
// honored when set, matching OpenCode's own config resolution.
func defaultOpenCodeConfigPath(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "opencode.json")
	}
	// Path is OS-independent by design — OpenCode uses ~/.config/opencode/
	// on every OS, not %APPDATA% / ~/Library (see NewOpenCode doc).
	return filepath.Join(home, ".config", "opencode", "opencode.json")
}

// openCodeClient is a standalone adapter (NOT an embedding of jsonMCPClient)
// because OpenCode uses the top-level `mcp` key rather than the JSON
// family's `mcpServers`, AND a distinct entry shape (`type:"remote"` +
// `enabled:true` rather than `disabled:false`). It mirrors VS Code's
// standalone-struct + HTTP-direct pattern with OpenCode's key/field set.
type openCodeClient struct {
	path string
}

// openCodeMCPKey is the single owner of OpenCode's top-level MCP section
// name. Every method that reaches into the parsed config map uses it.
const openCodeMCPKey = "mcp"

func (o *openCodeClient) Name() string       { return "opencode" }
func (o *openCodeClient) ConfigPath() string { return o.path }

// IsRelayStdio reports false: opencode is a URL-native HTTP MCP client.
func (o *openCodeClient) IsRelayStdio() bool { return false }

// Exists treats OpenCode as installed when EITHER the config file is
// present OR its parent directory (~/.config/opencode/) exists, mirroring
// the cursor/vscode/kiro "directory means installed" heuristic so an
// operator who has OpenCode installed but no MCP config yet still gets the
// Initialize / install affordance.
func (o *openCodeClient) Exists() bool {
	if _, err := os.Stat(o.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(o.path))
	return err == nil && st.IsDir()
}

func (o *openCodeClient) Backup() (string, error) {
	return o.BackupKeep(0)
}

// BackupKeep ensures the nested ~/.config/opencode parent directory exists,
// seeds an empty `{"mcp": {}}` stub if the config is absent, then writes
// the timestamped backup (pruning to keepN). The parent dir does not exist
// on a clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host. Mirrors the
// cursor/vscode/kiro/windsurf BackupKeep wrappers.
func (o *openCodeClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(o.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := o.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(o.path, o.Name(), keepN)
}

// InitEmpty seeds ~/.config/opencode/opencode.json with `{"mcp": {}}` if
// the file is absent. AddEntry's later merge writes into the same `mcp`
// map. OpenCode also accepts a top-level `$schema` field and many other
// keys, plus JSONC comments / trailing commas (it explicitly supports a
// `.jsonc` variant and operators hand-edit it): the read path parses
// comments via the shared JSONC helper, and AddEntry/RemoveEntry patch
// through hujson so the operator's comments and every unknown top-level key
// already present in the file are PRESERVED on every write (only the `mcp`
// map is touched) — so seeding a minimal stub does not clobber a
// hand-authored config, and on a truly fresh host this minimal stub is all
// that is needed.
func (o *openCodeClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(o.path, []byte("{\n  \"mcp\": {}\n}\n"))
}

func (o *openCodeClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(o.path, data)
}

func (o *openCodeClient) readJSON() (map[string]any, error) {
	data, err := readRawConfig(o.path)
	if err != nil {
		return nil, err
	}
	// OpenCode's config is JSONC (it supports a `.jsonc` variant, comments,
	// and trailing commas, and operators hand-edit it) — parse via the
	// comment-tolerant shared helper so a comment in the file does not break
	// migrate / Init.
	m, err := parseJSONCBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", o.path, err)
	}
	return m, nil
}

// setMember sets mcp.<name> = value, and deleteMember removes it, both
// preserving the operator's comments + unrelated top-level keys (e.g.
// `$schema`) when the file already has JSONC content. An empty/absent file
// falls back to a clean indented marshal. The bytes route through the
// UNCHANGED WriteConfigFile pipeline.
func (o *openCodeClient) setMember(name string, value any) error {
	return mutateJSONObjectMember(o.path, openCodeMCPKey, name, value, false)
}

func (o *openCodeClient) deleteMember(name string) error {
	return mutateJSONObjectMember(o.path, openCodeMCPKey, name, nil, true)
}

// AddEntry writes the hub-managed remote-HTTP entry under mcp.<name>.
// OpenCode's remote entry shape is `{"type":"remote","url":...,
// "enabled":true}`; an optional `headers` object is emitted when
// MCPEntry.Headers is non-empty.
func (o *openCodeClient) AddEntry(entry MCPEntry) error {
	if entry.Raw != nil {
		return o.setMember(entry.Name, entry.Raw)
	}
	serverEntry := map[string]any{
		"type":    "remote",
		"url":     entry.URL,
		"enabled": true,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set: patches mcp.<name> into the original on-disk
	// bytes via hujson so the operator's comments and unrelated keys survive (a
	// full map re-marshal would drop both).
	return o.setMember(entry.Name, serverEntry)
}

func (o *openCodeClient) RemoveEntry(name string) error {
	// Comment-preserving delete; absence is a no-op.
	return o.deleteMember(name)
}

func (o *openCodeClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[openCodeMCPKey].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	url, _ := raw["url"].(string)
	disabled := openCodeEntryDisabled(raw)
	if url == "" {
		return &MCPEntry{Name: name, Raw: raw, Disabled: disabled}, nil
	}
	if disabled {
		return &MCPEntry{Name: name, Raw: raw, Disabled: true}, nil
	}
	if openCodeRemoteHasExtraFields(raw) {
		return &MCPEntry{Name: name, Raw: raw}, nil
	}
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers")}, nil
}

func openCodeEntryDisabled(raw map[string]any) bool {
	if enabled, present := raw["enabled"]; present {
		if b, ok := enabled.(bool); ok && !b {
			return true
		}
	}
	return false
}

func openCodeRemoteHasExtraFields(raw map[string]any) bool {
	for k := range raw {
		if k != "type" && k != "url" && k != "enabled" && k != "headers" {
			return true
		}
	}
	return false
}

func (o *openCodeClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(o.path, o.Name())
}

func (o *openCodeClient) RestoreEntryFromBackup(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc
// on Client.RestoreEntryFromBackupForRollback). Used only by the serena
// dynamic-pool migrate abort-rollback.
func (o *openCodeClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a backup entry already in hub-HTTP shape (a hub
// loopback URL under `url` with no `command`) with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes
// the backup bytes verbatim regardless of shape.
func (o *openCodeClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
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
	backupServers, _ := backupMap[openCodeMCPKey].(map[string]any)
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive guard (demigrate flow only — the rollback caller
			// passes allowHubEntry=true to restore the pre-reconcile legacy
			// hub entry verbatim).
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubURLShapeEntry(rawMap, "url") {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			// Comment-preserving set into the LIVE config (its comments +
			// unrelated keys survive; the backup's entry VALUE is written).
			return o.setMember(name, backupEntry)
		}
	}
	return o.deleteMember(name)
}

// AllStdioEntries returns every stdio entry from OpenCode's top-level `mcp`
// key. The hub writes only HTTP-direct ("type":"remote") entries, which
// have no `command` field and so are correctly skipped by
// collectStdioEntries. Operator-authored local ("type":"local") entries
// store `command` as an ARRAY (["npx","-y",...]) rather than a string;
// collectStdioEntries reads `command` as a string and therefore does not
// surface them — an accepted limitation of the cross-format cleanup scan
// (these helpers are best-effort stdio-leak detection, and the hub never
// writes the local shape).
func (o *openCodeClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[openCodeMCPKey].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans `mcp` for stdio entries matching the
// mcp-language-server invocation pattern. As with AllStdioEntries, OpenCode
// local entries use a `command` ARRAY which the string-keyed matcher does
// not recognize; the hub-written HTTP entries never match either way.
func (o *openCodeClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[openCodeMCPKey].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

func (o *openCodeClient) BackupContainsEntry(backupPath, name string) (bool, error) {
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
	servers, _ := m[openCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether mcp[name] in the backup at
// backupPath is in OpenCode's hub-managed shape (a hub loopback `url` with
// no `command`). See Client.BackupEntryIsHubManaged.
func (o *openCodeClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
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
	servers, _ := m[openCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}

package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewMimoCode returns a Client bound to MiMoCode's global config file.
//
// MiMoCode (Xiaomi MiMo Code, github.com/XiaomiMiMo/MiMo-Code) is built as a
// FORK of OpenCode and keeps OpenCode's MCP config: MCP server definitions
// live in the top-level `mcp` object of its JSON config, with the IDENTICAL
// local/remote entry shapes. This adapter is a 1:1 structural mirror of the
// OpenCode adapter (see opencode.go); only the client id and the config path
// segment differ ("opencode" -> "mimocode", "opencode.json" ->
// "mimocode.json"), plus a single addition: when an operator already keeps a
// `mimocode.jsonc`, the path owner targets that existing file (MiMoCode reads
// both `.json` and `.jsonc`).
//
// Two config scopes exist (same as OpenCode):
//
//   - Global: ~/.config/mimocode/mimocode.json (also accepts a `.jsonc`
//     variant). On every OS MiMoCode resolves the global config from
//     ~/.config/mimocode/ — like OpenCode it does NOT follow the Windows
//     %APPDATA% / macOS ~/Library convention. (Verified 2026-06: the user's
//     real Windows install is at C:\Users\<user>\.config\mimocode\.)
//   - Project: a per-repo config in the repository root (highest precedence,
//     merged over global at load time).
//
// The hub writes the GLOBAL file so a single per-user hub entry is visible in
// every project, matching every other adapter's user-scoped posture (and
// matching OpenCode's adapter, which also returns the global path). The
// project-root overlay is a documented limitation SHARED with the OpenCode
// adapter, not a MiMoCode-specific concern.
//
// Transport choice — HTTP-direct (NOT relay-stdio). Like OpenCode, MiMoCode
// supports remote MCP servers natively over Streamable HTTP. A remote entry
// is keyed by server name under `mcp` and discriminated by `"type":"remote"`
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
//     remote ("type":"remote", `url`, `headers`, `enabled`) entry shapes,
//     inherited verbatim by the MiMoCode fork.
//   - https://github.com/XiaomiMiMo/MiMo-Code (paths.ts) — the global config
//     dir is ~/.config/mimocode/, loading `mimocode.json` / `mimocode.jsonc`.
//
// IMPORTANT divergences from the JSON family (mcpServers + disabled:false),
// inherited from OpenCode:
//   - top-level key is `mcp`, NOT `mcpServers` — so this is a standalone
//     struct (like vscode/zed/opencode), not an embedding of jsonMCPClient.
//   - the active flag is `enabled` (true = on), NOT `disabled` (false =
//     on). The hub writes `enabled:true`.
//   - remote entries carry `"type":"remote"`.
//
// The hub-shape guard (isHubURLShapeEntry) keys off the `url` field and the
// absence of a `command` key, which holds for MiMoCode's remote entry shape,
// so demigrate/rollback recognize a hub-managed MiMoCode entry.
func NewMimoCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&mimoCodeClient{path: defaultMimoCodeConfigPath(home)}), nil
}

// defaultMimoCodeConfigPath returns the global MiMoCode config path.
// MiMoCode uses the XDG-style ~/.config/mimocode/ location on every OS
// (Windows included — it does not switch to %APPDATA%). XDG_CONFIG_HOME is
// honored when set, matching MiMoCode's own config resolution.
//
// One addition over the OpenCode adapter: when the operator already keeps a
// `mimocode.jsonc` in the resolved directory, that existing file is the
// target (MiMoCode reads both `mimocode.json` and `mimocode.jsonc`, so a hub
// entry written into a separate `.json` while a `.jsonc` exists could be
// shadowed). Otherwise the path is `mimocode.json` — the file the adapter
// seeds on a fresh host.
func defaultMimoCodeConfigPath(home string) string {
	dir := mimoCodeGlobalConfigDir(home)
	// Prefer an existing mimocode.jsonc; otherwise target mimocode.json.
	if jsonc := filepath.Join(dir, "mimocode.jsonc"); isRegularFile(jsonc) {
		return jsonc
	}
	return filepath.Join(dir, "mimocode.json")
}

// mimoCodeGlobalConfigDir resolves the global config directory. XDG_CONFIG_HOME
// is honored when set, else ~/.config/mimocode. The path is OS-independent by
// design — MiMoCode uses ~/.config/mimocode/ on every OS, not %APPDATA% /
// ~/Library (see NewMimoCode doc).
func mimoCodeGlobalConfigDir(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mimocode")
	}
	return filepath.Join(home, ".config", "mimocode")
}

// isRegularFile reports whether path is an existing regular file. A stat error
// other than not-exist (e.g. a permission failure) is treated as "absent" so
// resolution never fails closed on a transient probe error.
func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// mimoCodeClient is a standalone adapter (NOT an embedding of jsonMCPClient)
// because MiMoCode uses the top-level `mcp` key rather than the JSON family's
// `mcpServers`, AND a distinct entry shape (`type:"remote"` + `enabled:true`
// rather than `disabled:false`). It mirrors the OpenCode adapter's
// standalone-struct + HTTP-direct pattern with the same key/field set.
type mimoCodeClient struct {
	path string
}

// mimoCodeMCPKey is the single owner of MiMoCode's top-level MCP section
// name. Every method that reaches into the parsed config map uses it.
const mimoCodeMCPKey = "mcp"

func (o *mimoCodeClient) Name() string       { return "mimocode" }
func (o *mimoCodeClient) ConfigPath() string { return o.path }

// IsRelayStdio reports false: mimocode is a URL-native HTTP MCP client.
func (o *mimoCodeClient) IsRelayStdio() bool { return false }

// Exists treats MiMoCode as installed when EITHER the config file is present
// OR its parent directory (~/.config/mimocode/) exists, mirroring the
// opencode/cursor/vscode/kiro "directory means installed" heuristic so an
// operator who has MiMoCode installed but no MCP config yet still gets the
// Initialize / install affordance.
func (o *mimoCodeClient) Exists() bool {
	if _, err := os.Stat(o.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(o.path))
	return err == nil && st.IsDir()
}

func (o *mimoCodeClient) Backup() (string, error) {
	return o.BackupKeep(0)
}

// BackupKeep ensures the nested ~/.config/mimocode parent directory exists,
// seeds an empty `{"mcp": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir does not exist on a
// clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host. Mirrors the
// opencode/cursor/vscode/kiro/windsurf BackupKeep wrappers.
func (o *mimoCodeClient) BackupKeep(keepN int) (string, error) {
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

// InitEmpty seeds ~/.config/mimocode/mimocode.json with `{"mcp": {}}` if the
// file is absent. AddEntry's later merge writes into the same `mcp` map.
// MiMoCode also accepts a top-level `$schema` field and many other keys, plus
// JSONC comments / trailing commas (it accepts a `.jsonc` variant and
// operators hand-edit it): the read path parses comments via the shared
// JSONC helper, and AddEntry/RemoveEntry patch through hujson so the
// operator's comments and every unknown top-level key already present in the
// file are PRESERVED on every write (only the `mcp` map is touched) — so
// seeding a minimal stub does not clobber a hand-authored config, and on a
// truly fresh host this minimal stub is all that is needed.
func (o *mimoCodeClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(o.path, []byte("{\n  \"mcp\": {}\n}\n"))
}

func (o *mimoCodeClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(o.path, data)
}

func (o *mimoCodeClient) readJSON() (map[string]any, error) {
	data, err := readRawConfig(o.path)
	if err != nil {
		return nil, err
	}
	// MiMoCode's config is JSONC (it accepts a `.jsonc` variant, comments, and
	// trailing commas, and operators hand-edit it) — parse via the
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
func (o *mimoCodeClient) setMember(name string, value any) error {
	return mutateJSONObjectMember(o.path, mimoCodeMCPKey, name, value, false)
}

func (o *mimoCodeClient) deleteMember(name string) error {
	return mutateJSONObjectMember(o.path, mimoCodeMCPKey, name, nil, true)
}

// AddEntry writes the hub-managed remote-HTTP entry under mcp.<name>.
// MiMoCode's remote entry shape is `{"type":"remote","url":...,
// "enabled":true}`; an optional `headers` object is emitted when
// MCPEntry.Headers is non-empty.
func (o *mimoCodeClient) AddEntry(entry MCPEntry) error {
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

func (o *mimoCodeClient) RemoveEntry(name string) error {
	// Comment-preserving delete; absence is a no-op.
	return o.deleteMember(name)
}

func (o *mimoCodeClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
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

func (o *mimoCodeClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(o.path, o.Name())
}

func (o *mimoCodeClient) RestoreEntryFromBackup(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc
// on Client.RestoreEntryFromBackupForRollback). Used only by the serena
// dynamic-pool migrate abort-rollback.
func (o *mimoCodeClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a backup entry already in hub-HTTP shape (a hub
// loopback URL under `url` with no `command`) with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes the
// backup bytes verbatim regardless of shape.
func (o *mimoCodeClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
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
	backupServers, _ := backupMap[mimoCodeMCPKey].(map[string]any)
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

// AllStdioEntries returns every stdio entry from MiMoCode's top-level `mcp`
// key. The hub writes only HTTP-direct ("type":"remote") entries, which have
// no `command` field and so are correctly skipped by collectStdioEntries.
// Operator-authored local ("type":"local") entries store `command` as an
// ARRAY (["npx","-y",...]) rather than a string; collectStdioEntries reads
// `command` as a string and therefore does not surface them — an accepted
// limitation of the cross-format cleanup scan (these helpers are best-effort
// stdio-leak detection, and the hub never writes the local shape).
func (o *mimoCodeClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans `mcp` for stdio entries matching the
// mcp-language-server invocation pattern. As with AllStdioEntries, MiMoCode
// local entries use a `command` ARRAY which the string-keyed matcher does not
// recognize; the hub-written HTTP entries never match either way.
func (o *mimoCodeClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

func (o *mimoCodeClient) BackupContainsEntry(backupPath, name string) (bool, error) {
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
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether mcp[name] in the backup at
// backupPath is in MiMoCode's hub-managed shape (a hub loopback `url` with no
// `command`). See Client.BackupEntryIsHubManaged.
func (o *mimoCodeClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
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
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}

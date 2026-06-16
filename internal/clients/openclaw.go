package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewOpenClaw returns a Client bound to OpenClaw's user config file.
//
// OpenClaw (https://openclaw.ai, github.com/openclaw/openclaw) is a real,
// current personal-AI-assistant runtime with a first-class MCP client-side
// registry. The `openclaw mcp add | set | configure | ...` commands manage
// "OpenClaw-owned outbound MCP server definitions" that OpenClaw projects
// into the agent runtimes it launches (embedded OpenClaw, Codex app-server,
// Claude Code / Gemini adapters). Those saved definitions live under the
// NESTED `mcp.servers` object of OpenClaw's JSON config:
//
//	~/.openclaw/openclaw.json
//
// Path resolution (OpenClaw's own precedence, env docs/help/environment.md):
// OPENCLAW_CONFIG_PATH > (OPENCLAW_STATE_DIR | OPENCLAW_HOME)/openclaw.json >
// ~/.openclaw/openclaw.json. This adapter resolves the default
// ~/.openclaw/openclaw.json form, matching every other adapter's
// home-relative posture. OPENCLAW_HOME / OPENCLAW_STATE_DIR /
// OPENCLAW_CONFIG_PATH overrides are an operator concern handled the same
// way the other XDG-aware adapters defer non-default overrides.
//
// Transport choice — HTTP-direct (NOT relay-stdio). OpenClaw supports remote
// MCP servers natively over Streamable HTTP. A saved HTTP server definition
// is keyed by server name under `mcp.servers` and carries a `url` endpoint
// plus `transport: "streamable-http"`:
//
//	{
//	  "mcp": {
//	    "servers": {
//	      "<server-name>": {
//	        "url": "http://localhost:9121/mcp",
//	        "transport": "streamable-http",
//	        "enabled": true
//	      }
//	    }
//	  }
//	}
//
// (Stdio servers use `command` + `args` instead; the hub never writes that
// shape because the daemon is already an HTTP endpoint.) Optional `headers`
// is emitted when MCPEntry.Headers is non-empty.
//
// Sources (verified 2026-06, primary repo + source-of-truth type):
//   - github.com/openclaw/openclaw/blob/master/docs/cli/mcp.md — the
//     `openclaw mcp` client-side registry stores definitions "under
//     mcp.servers in OpenClaw config"; "Use transport: \"streamable-http\"
//     for Streamable HTTP MCP servers"; `enabled: false` keeps a saved
//     definition while excluding it from runtime discovery.
//   - github.com/openclaw/openclaw/blob/master/src/config/types.mcp.ts —
//     the authoritative TS type: McpConfig.servers is
//     Record<string, McpServerConfig>; McpServerConfig has url?,
//     transport?: "sse" | "streamable-http", enabled?, headers?, command?,
//     args? (so a hub HTTP entry has `url`+`transport`, no `command`).
//   - github.com/openclaw/openclaw/blob/master/docs/help/environment.md —
//     OPENCLAW_CONFIG_PATH default ~/.openclaw/openclaw.json.
//
// IMPORTANT divergences from the JSON family (top-level mcpServers + the
// `disabled` flag):
//   - the MCP section is NESTED two levels: `mcp` -> `servers`, NOT a single
//     top-level `mcpServers` map. So this is a standalone struct (like
//     vscode/zed/opencode), not an embedding of jsonMCPClient, AND it uses a
//     two-level accessor (readServers / writeServers) so the rest of the
//     parsed `mcp` object and every unrelated top-level field survive merges.
//   - the active flag is `enabled` (true = on, default true when omitted),
//     NOT `disabled` (false = on). The hub writes `enabled:true`.
//   - HTTP entries carry `transport: "streamable-http"`.
//
// The hub-shape guard (isHubURLShapeEntry) keys off the `url` field and the
// absence of a `command` key, which holds for OpenClaw's HTTP entry shape, so
// demigrate/rollback recognize a hub-managed OpenClaw entry.
//
// Default-vs-opt-in — OPT-IN. OpenClaw is a niche/new client; it is not in
// DefaultInstallClientNames, so a fresh install never silently mutates an
// OpenClaw config. The operator targets it explicitly.
func NewOpenClaw() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&openClawClient{path: defaultOpenClawConfigPath(home)}), nil
}

// defaultOpenClawConfigPath returns the default OpenClaw config path,
// ~/.openclaw/openclaw.json. OPENCLAW_CONFIG_PATH overrides this in
// OpenClaw itself, but mcp-local-hub's adapters resolve the canonical
// home-relative default (matching the claude-code/cursor/gemini posture);
// non-default OpenClaw home/state/config overrides remain an operator
// concern, the same way the XDG-aware adapters defer their non-default
// overrides. The path is OS-independent by design — OpenClaw uses
// ~/.openclaw on every OS, not %APPDATA% / ~/Library.
func defaultOpenClawConfigPath(home string) string {
	return filepath.Join(home, ".openclaw", "openclaw.json")
}

// openClawClient is a standalone adapter (NOT an embedding of jsonMCPClient)
// because OpenClaw nests its MCP server map under `mcp.servers` (two levels)
// rather than a single top-level `mcpServers`, AND uses an `enabled:true`
// flag + a `transport` discriminator rather than `disabled:false`. It mirrors
// OpenCode's standalone-struct + HTTP-direct pattern, adapted for OpenClaw's
// nested key path and field set.
type openClawClient struct {
	path string
}

// openClawMCPKey and openClawServersKey are the single owners of OpenClaw's
// nested MCP section path (`mcp` -> `servers`). Every method that reaches
// into the parsed config map uses them.
const (
	openClawMCPKey     = "mcp"
	openClawServersKey = "servers"
)

func (o *openClawClient) Name() string       { return "openclaw" }
func (o *openClawClient) ConfigPath() string { return o.path }

// IsRelayStdio reports false: openclaw is a URL-native HTTP MCP client.
func (o *openClawClient) IsRelayStdio() bool { return false }

// Exists treats OpenClaw as installed when EITHER the config file is present
// OR its parent directory (~/.openclaw/) exists, mirroring the
// cursor/vscode/kiro/opencode "directory means installed" heuristic so an
// operator who has OpenClaw installed but no MCP config yet still gets the
// Initialize / install affordance.
func (o *openClawClient) Exists() bool {
	if _, err := os.Stat(o.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(o.path))
	return err == nil && st.IsDir()
}

func (o *openClawClient) Backup() (string, error) {
	return o.BackupKeep(0)
}

// BackupKeep ensures the ~/.openclaw parent directory exists, seeds an empty
// `{"mcp":{"servers":{}}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir does not exist on a
// clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host. Mirrors the
// cursor/vscode/kiro/opencode BackupKeep wrappers.
func (o *openClawClient) BackupKeep(keepN int) (string, error) {
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

// InitEmpty seeds ~/.openclaw/openclaw.json with `{"mcp":{"servers":{}}}` if
// the file is absent. AddEntry's later merge writes into the same nested
// `mcp.servers` map. OpenClaw config also accepts a top-level `$schema` field
// and many other keys, plus JSONC comments / trailing commas (operators
// hand-edit it): the read path parses comments via the shared JSONC helper,
// and AddEntry/RemoveEntry patch through hujson so the operator's comments,
// every unknown top-level key, and every sibling key on the `mcp` object
// (sessionIdleTtlMs, etc.) are PRESERVED on every write (only the nested
// `mcp.servers.<name>` member is touched) — so seeding a minimal stub does
// not clobber a hand-authored config, and on a truly fresh host this minimal
// stub is all that is needed.
func (o *openClawClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(o.path, []byte("{\n  \"mcp\": {\n    \"servers\": {}\n  }\n}\n"))
}

func (o *openClawClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(o.path, data)
}

func (o *openClawClient) readJSON() (map[string]any, error) {
	data, err := readRawConfig(o.path)
	if err != nil {
		return nil, err
	}
	// OpenClaw's config is JSONC (operators hand-edit it; it carries `//` +
	// `/* */` comments and trailing commas) — parse via the comment-tolerant
	// shared helper so a comment in the file does not break migrate / Init.
	m, err := parseJSONCBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", o.path, err)
	}
	return m, nil
}

// openClawServerPath is the nested object key chain (`mcp` -> `servers`) under
// which OpenClaw's server members live. Single owner of the path passed to the
// nested-path JSONC mutate helper so the writer stays in sync with the
// serversFromMap read accessor.
var openClawServerPath = []string{openClawMCPKey, openClawServersKey}

// serversFromMap returns the nested `mcp.servers` map from a parsed config
// root, or nil when the `mcp` object or its `servers` child is missing or the
// wrong type. Read-only; the write seams (setMember / deleteMember) patch the
// nested member through hujson via the openClawServerPath helper rather than
// rebuilding the map, so the rest of the `mcp` object and every unrelated
// top-level key survive untouched.
func serversFromMap(root map[string]any) map[string]any {
	mcp, _ := root[openClawMCPKey].(map[string]any)
	if mcp == nil {
		return nil
	}
	servers, _ := mcp[openClawServersKey].(map[string]any)
	return servers
}

// setMember sets mcp.servers.<name> = value, and deleteMember removes it, both
// preserving the operator's comments, every sibling key on the `mcp` object,
// and every unrelated top-level key when the file already has JSONC content.
// An empty/absent file falls back to a clean indented marshal of the full
// `{"mcp":{"servers":{...}}}` nesting. The bytes route through the UNCHANGED
// WriteConfigFile pipeline.
func (o *openClawClient) setMember(name string, value any) error {
	return mutateJSONObjectMemberPath(o.path, openClawServerPath, name, value, false)
}

func (o *openClawClient) deleteMember(name string) error {
	return mutateJSONObjectMemberPath(o.path, openClawServerPath, name, nil, true)
}

// AddEntry writes the hub-managed HTTP server definition under
// mcp.servers.<name>. OpenClaw's HTTP entry shape is
// `{"url":...,"transport":"streamable-http","enabled":true}`; an optional
// `headers` object is emitted when MCPEntry.Headers is non-empty.
func (o *openClawClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"url":       entry.URL,
		"transport": "streamable-http",
		"enabled":   true,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set: patches mcp.servers.<name> into the original
	// on-disk bytes via hujson so the operator's comments, the `mcp` object's
	// sibling keys, and unrelated top-level keys all survive (a full map
	// re-marshal would drop the comments).
	return o.setMember(entry.Name, serverEntry)
}

func (o *openClawClient) RemoveEntry(name string) error {
	// Comment-preserving delete; absence is a no-op.
	return o.deleteMember(name)
}

func (o *openClawClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers := serversFromMap(m)
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

func (o *openClawClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(o.path, o.Name())
}

func (o *openClawClient) RestoreEntryFromBackup(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc
// on Client.RestoreEntryFromBackupForRollback). Used only by the serena
// dynamic-pool migrate abort-rollback.
func (o *openClawClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a backup entry already in hub-HTTP shape (a hub
// loopback URL under `url` with no `command`) with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes the
// backup bytes verbatim regardless of shape. Both the backup read and the
// live write go through the nested `mcp.servers` accessor.
func (o *openClawClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
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
	backupServers := serversFromMap(backupMap)
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
			// Comment-preserving set into the LIVE config (its comments,
			// `mcp` siblings + unrelated keys survive; the backup's entry
			// VALUE is written).
			return o.setMember(name, backupEntry)
		}
	}
	return o.deleteMember(name)
}

// AllStdioEntries returns every stdio entry from OpenClaw's nested
// `mcp.servers` key. The hub writes only HTTP-direct entries (url +
// transport), which have no `command` field and so are correctly skipped by
// collectStdioEntries. Operator-authored stdio entries (`command` string +
// `args`) surface here so the cross-format cleanup scan can derive their
// kill-patterns.
func (o *openClawClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	return collectStdioEntries(serversFromMap(m)), nil
}

// FindStdioLanguageServerEntries scans `mcp.servers` for stdio entries
// matching the mcp-language-server invocation pattern. The hub-written HTTP
// entries never match (no `command`).
func (o *openClawClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	return findLanguageServerStdioInMap(serversFromMap(m)), nil
}

func (o *openClawClient) BackupContainsEntry(backupPath, name string) (bool, error) {
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
	servers := serversFromMap(m)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether mcp.servers[name] in the backup at
// backupPath is in OpenClaw's hub-managed shape (a hub loopback `url` with no
// `command`). See Client.BackupEntryIsHubManaged.
func (o *openClawClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
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
	servers := serversFromMap(m)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}

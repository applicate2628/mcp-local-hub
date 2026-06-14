package clients

import (
	"encoding/json"
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
	return &openClawClient{path: defaultOpenClawConfigPath(home)}, nil
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
// and many other keys; the merge path round-trips through encoding/json which
// preserves every unknown top-level key already present in the file (only the
// nested `mcp.servers` map is touched), so seeding a minimal stub does not
// clobber a hand-authored config — but on a truly fresh host this minimal
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
	data, err := os.ReadFile(o.path)
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
		return nil, fmt.Errorf("parse %s: %w", o.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (o *openClawClient) writeJSON(m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative + DACL-bound) for
	// token-bearing rewrites; tests get the os.WriteFile fallback.
	return WriteConfigFile(o.path, append(out, '\n'))
}

// serversFromMap returns the nested `mcp.servers` map from a parsed config
// root, or nil when the `mcp` object or its `servers` child is missing or the
// wrong type. Read-only; callers that need to mutate use writeServers to put
// the (possibly newly-created) map back under the right nested key while
// preserving the rest of the `mcp` object.
func serversFromMap(root map[string]any) map[string]any {
	mcp, _ := root[openClawMCPKey].(map[string]any)
	if mcp == nil {
		return nil
	}
	servers, _ := mcp[openClawServersKey].(map[string]any)
	return servers
}

// writeServers stores `servers` under root["mcp"]["servers"], creating (and
// preserving) the intermediate `mcp` object. Any other keys already present
// on the `mcp` object (sessionIdleTtlMs, etc.) and every unrelated top-level
// field survive untouched.
func writeServers(root, servers map[string]any) {
	mcp, _ := root[openClawMCPKey].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	mcp[openClawServersKey] = servers
	root[openClawMCPKey] = mcp
}

// AddEntry writes the hub-managed HTTP server definition under
// mcp.servers.<name>. OpenClaw's HTTP entry shape is
// `{"url":...,"transport":"streamable-http","enabled":true}`; an optional
// `headers` object is emitted when MCPEntry.Headers is non-empty.
func (o *openClawClient) AddEntry(entry MCPEntry) error {
	m, err := o.readJSON()
	if err != nil {
		return err
	}
	servers := serversFromMap(m)
	if servers == nil {
		servers = map[string]any{}
	}
	serverEntry := map[string]any{
		"url":       entry.URL,
		"transport": "streamable-http",
		"enabled":   true,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	servers[entry.Name] = serverEntry
	writeServers(m, servers)
	return o.writeJSON(m)
}

func (o *openClawClient) RemoveEntry(name string) error {
	m, err := o.readJSON()
	if err != nil {
		return err
	}
	servers := serversFromMap(m)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	writeServers(m, servers)
	return o.writeJSON(m)
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
	backupServers := serversFromMap(backupMap)
	liveMap, err := o.readJSON()
	if err != nil {
		return err
	}
	liveServers := serversFromMap(liveMap)
	if liveServers == nil {
		liveServers = map[string]any{}
	}
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
			liveServers[name] = backupEntry
			writeServers(liveMap, liveServers)
			return o.writeJSON(liveMap)
		}
	}
	delete(liveServers, name)
	writeServers(liveMap, liveServers)
	return o.writeJSON(liveMap)
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

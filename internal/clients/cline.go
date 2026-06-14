package clients

import (
	"os"
	"path/filepath"
	"runtime"
)

// NewCline returns a Client bound to Cline's cline_mcp_settings.json under the
// VS Code globalStorage directory.
//
// Cline (the VS Code extension, publisher id saoudrizwan.claude-dev) reads MCP
// servers from a JSON file under the top-level `mcpServers` key — the canonical
// JSON family schema shared by Cursor / Gemini CLI / Antigravity. Cline supports
// stdio, SSE, and Streamable HTTP transports, so the hub writes an HTTP-direct
// entry (no relay shim needed).
//
// The quirk that forces a dedicated adapter rather than reusing the bare
// jsonMCPClient is the transport-type discriminator. Cline's server-config Zod
// schema is a discriminated union over type ∈ {"stdio", "sse", "streamableHttp"}
// — there is NO "http" literal (the value Cursor/VS Code write). Critically, when
// `type` is OMITTED the union lists SSE first, so an entry with only a `url` and
// no `type` is parsed as SSE, NOT Streamable HTTP. mcp-local-hub serves
// Streamable HTTP, so the hub-managed entry MUST carry `type: "streamableHttp"`
// explicitly — both omitting it (mis-routed as SSE) and writing Cursor's
// `type: "http"` (fails Cline's `.refine()` and shows the
// "Server type must be one of: 'stdio', 'sse', or 'streamableHttp'" error)
// would break the connection. The hub-managed entry shape written is therefore:
//
//	"<server-name>": {
//	  "type": "streamableHttp",
//	  "url": "http://localhost:9121/mcp"
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty.
//
// Because the endpoint field is the standard `url` key, the embedded
// jsonMCPClient's hub-shape detection (isHubURLShapeEntry(_, "url")) and the
// inherited GetEntry/RemoveEntry/RestoreEntryFromBackup/BackupEntryIsHubManaged
// helpers all work unchanged with urlField "url" — only AddEntry (the
// streamableHttp discriminator) and the Cursor-style seed/Exists overrides need
// dedicated implementations.
//
// Config path (VS Code globalStorage, publisher saoudrizwan.claude-dev):
//   - Windows: %APPDATA%\Code\User\globalStorage\saoudrizwan.claude-dev\settings\cline_mcp_settings.json
//   - macOS:   ~/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json
//   - Linux:   $XDG_CONFIG_HOME (or ~/.config)/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json
//
// Sources (verified 2026-06):
//   - https://docs.cline.bot/mcp/configuring-mcp-servers : top-level key
//     `mcpServers`, remote entry uses `url` + `headers` (+ `disabled`,
//     `autoApprove`).
//   - cline/cline apps/vscode/src/services/mcp/schemas.ts (ServerConfigSchema):
//     discriminated union type ∈ {"stdio","sse","streamableHttp"}; the SSE
//     variant is listed first so an absent `type` defaults to SSE; the
//     Streamable HTTP variant takes `type: "streamableHttp"` + `url`. The
//     TYPE_ERROR_MESSAGE constant confirms the accepted literals.
//   - landicefu/mcp-client-configuration-server (get_configuration_path):
//     Windows/macOS globalStorage paths under saoudrizwan.claude-dev/settings.
func NewCline() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultClineConfigPath(home),
		clientName: "cline",
		// Standard `url` endpoint key: the embedded base helpers
		// (GetEntry, hub-shape detection in backupEntryMapIsHubManaged,
		// the restore guard) all key off this and need no override.
		urlField: "url",
	}
	return &clineClient{jsonMCPClient: base}, nil
}

// defaultClineConfigPath returns the cline_mcp_settings.json path for the
// current OS. It mirrors the VS Code globalStorage layout used by
// defaultVSCodeConfigPath but descends into the Cline extension's settings
// folder (publisher id saoudrizwan.claude-dev).
func defaultClineConfigPath(home string) string {
	const (
		publisher = "saoudrizwan.claude-dev"
		fileName  = "cline_mcp_settings.json"
	)
	switch runtime.GOOS {
	case "windows":
		root := os.Getenv("APPDATA")
		if root == "" {
			root = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(root, "Code", "User", "globalStorage", publisher, "settings", fileName)
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", publisher, "settings", fileName)
	default:
		root := os.Getenv("XDG_CONFIG_HOME")
		if root == "" {
			root = filepath.Join(home, ".config")
		}
		return filepath.Join(root, "Code", "User", "globalStorage", publisher, "settings", fileName)
	}
}

// clineClient overrides AddEntry to emit Cline's Streamable HTTP discriminator
// (`type: "streamableHttp"`) and overrides Exists/Backup/BackupKeep/InitEmpty so
// a fresh Cline install (the extension's settings directory present, but
// cline_mcp_settings.json not yet written) can be routed through the hub without
// the operator hand-creating the file — the same posture as the Cursor adapter.
// Restore, RemoveEntry, GetEntry, Name, ConfigPath, LatestBackupPath,
// RestoreEntryFromBackup, RestoreEntryFromBackupForRollback, BackupContainsEntry,
// BackupEntryIsHubManaged, AllStdioEntries, and FindStdioLanguageServerEntries
// are promoted from the embedded jsonMCPClient unchanged (all key off the
// standard `url` field via urlField "url").
type clineClient struct {
	*jsonMCPClient
}

// Exists treats Cline as installed when EITHER the config file is present OR its
// parent directory (…/saoudrizwan.claude-dev/settings/) exists. Cline writes
// cline_mcp_settings.json lazily — only once the user first adds an MCP server —
// so a dir-based probe lets the operator migrate from a fresh install. Mirrors
// cursorClient.Exists.
func (c *clineClient) Exists() bool {
	if _, err := os.Stat(c.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(c.path))
	return err == nil && st.IsDir()
}

// Backup delegates to BackupKeep(0) (no pruning) while routing through the
// MkdirAll+InitEmpty seed path so a fresh install backs up cleanly.
func (c *clineClient) Backup() (string, error) {
	return c.BackupKeep(0)
}

// BackupKeep ensures the parent dir exists and seeds an empty `{"mcpServers":{}}`
// stub before backing up, so a migrate that runs against a fresh install (no
// cline_mcp_settings.json yet) does not fail with ErrClientNotInstalled. Mirrors
// cursorClient.BackupKeep.
func (c *clineClient) BackupKeep(keepN int) (string, error) {
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

// InitEmpty seeds cline_mcp_settings.json with `{"mcpServers": {}}` if the file
// is absent. AddEntry's later merge writes into the same `mcpServers` map.
// Identical stub to the Cursor/JSON-family adapters.
func (c *clineClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(c.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

// AddEntry writes the hub-managed Streamable HTTP entry under the standard `url`
// key with the Cline-specific `type: "streamableHttp"` discriminator. Emitting
// `type: "streamableHttp"` (rather than Cursor's `type: "http"` or omitting
// `type`, which Cline parses as SSE) is the single behavioral difference from
// the JSON-family adapters — see the NewCline doc comment for the schema
// rationale.
func (c *clineClient) AddEntry(entry MCPEntry) error {
	m, err := c.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	serverEntry := map[string]any{
		"type": "streamableHttp",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	servers[entry.Name] = serverEntry
	m["mcpServers"] = servers
	return c.writeJSON(m)
}

package clients

import (
	"os"
	"path/filepath"
)

// NewCopilotCLI returns a Client bound to GitHub Copilot CLI's user-level
// MCP config at <COPILOT_HOME or ~/.copilot>/mcp-config.json.
//
// GitHub Copilot CLI reads MCP servers from a JSON file using the canonical
// `{"mcpServers": {...}}` family schema, keyed by server name. Each entry is
// discriminated by a `type` field:
//
//   - local (a.k.a. "stdio"): `{"type":"local","command":...,"args":[...],
//     "env":{...},"tools":["*"]}` — a stdio subprocess.
//   - http (a.k.a. "sse"): `{"type":"http","url":...,"headers":{...},
//     "tools":["*"]}` — a remote HTTP MCP server.
//
// Because Copilot CLI speaks HTTP MCP natively, this adapter is HTTP-direct
// (NOT relay-stdio): a hub daemon's loopback URL is written as an http entry.
// IsRelayStdio() therefore returns false (inherited from jsonMCPClient).
//
// COPILOT_HOME override — when the COPILOT_HOME environment variable is set,
// it replaces the default ~/.copilot directory (the config file then lives at
// $COPILOT_HOME/mcp-config.json). This mirrors how Copilot CLI itself locates
// its home directory, so a user who relocates ~/.copilot has the hub write to
// the same place the CLI reads.
//
// Source (verified 2026-06-17):
//   - https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers
//     — config at ~/.copilot/mcp-config.json, top-level `mcpServers` key,
//     per-server `type` discriminator (local/http), http entry `{url, headers,
//     tools}`, local entry `{command, args, env, tools}`.
//   - .scratch/adapter-schemas-openhands-aider.md — COPILOT_HOME overrides the
//     ~/.copilot dir; http entries carry `type:"http"`, `url`, and a `tools`
//     filter (["*"] = all tools).
func NewCopilotCLI() (Client, error) {
	base := &jsonMCPClient{
		path:       defaultCopilotCLIConfigPath(),
		clientName: "copilot-cli",
		// Copilot CLI's remote endpoint key is "url". The base GetEntry /
		// demigrate helpers key off this field; AddEntry is overridden below
		// to add the `type:"http"` discriminator and the `tools` filter.
		urlField: "url",
	}
	return newLockingClient(&copilotCLIClient{jsonMCPClient: base}), nil
}

// defaultCopilotCLIConfigPath returns <COPILOT_HOME or ~/.copilot>/mcp-config.json.
// COPILOT_HOME, when set, replaces the ~/.copilot directory entirely. When it
// is unset, the path falls back to ~/.copilot under the user's home dir; a
// home-dir lookup failure degrades to a bare "mcp-config.json" relative path
// rather than panicking (matching the fail-safe posture AllClients uses to
// drop unconstructable adapters — Exists() on such a path simply reports
// absent).
func defaultCopilotCLIConfigPath() string {
	if h := os.Getenv("COPILOT_HOME"); h != "" {
		return filepath.Join(h, "mcp-config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "mcp-config.json"
	}
	return filepath.Join(home, ".copilot", "mcp-config.json")
}

// copilotCLIClient overrides only the filesystem-bootstrap methods (Exists,
// Backup, BackupKeep) and AddEntry (to emit the http-transport shape with the
// `type` discriminator + `tools` filter). GetEntry, RemoveEntry, Restore,
// InitEmpty, and every backup/demigrate helper are promoted from the embedded
// jsonMCPClient unchanged.
type copilotCLIClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.copilot or $COPILOT_HOME) does — mirroring the
// cursor/kiro "directory means installed" heuristic so an operator who has
// Copilot CLI installed but no MCP config yet still gets the Initialize /
// install affordance.
func (c *copilotCLIClient) Exists() bool {
	if _, err := os.Stat(c.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(c.path))
	return err == nil && st.IsDir()
}

func (c *copilotCLIClient) Backup() (string, error) {
	return c.BackupKeep(0)
}

// BackupKeep ensures the ~/.copilot ($COPILOT_HOME) parent directory exists,
// seeds an empty `{"mcpServers": {}}` stub if the config is absent, then writes
// the timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host. Mirrors the cursor/kiro
// BackupKeep wrappers.
func (c *copilotCLIClient) BackupKeep(keepN int) (string, error) {
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

// AddEntry writes the hub-managed http-transport entry under
// mcpServers.<name>. Copilot CLI's remote entry shape is
// `{"type":"http","url":...,"tools":["*"]}`; an optional `headers` object is
// emitted when MCPEntry.Headers is non-empty. The `tools:["*"]` filter opts
// the entry into every tool the server exposes (Copilot requires the field to
// route any tools at all).
func (c *copilotCLIClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type":  "http",
		"url":   entry.URL,
		"tools": []any{"*"},
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam: mcp-config.json may be
	// hand-edited, so patch mcpServers.<name> into the original bytes instead
	// of a lossy full-map re-marshal.
	return c.setMember(entry.Name, serverEntry)
}

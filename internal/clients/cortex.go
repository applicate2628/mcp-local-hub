package clients

import (
	"os"
	"path/filepath"
)

// NewCortex returns a Client bound to Snowflake Cortex Code's global MCP config:
//
//	~/.snowflake/cortex/mcp.json
//
// Snowflake Cortex Code (a CLI coding agent) reads MCP servers from a JSON file
// under the top-level `mcpServers` key — the canonical JSON family schema
// shared by Claude Code / Cursor / Gemini CLI. Each entry is discriminated by a
// `type` field ∈ {"stdio","http","sse"}. Cortex Code speaks HTTP MCP natively,
// so this adapter is HTTP-direct (NOT relay-stdio). The documented remote (http)
// entry shape is:
//
//	"<server-name>": {
//	  "type": "http",
//	  "url": "https://api.example.com/mcp"
//	}
//
// (http/sse entries carry `url` + optional `headers`; stdio entries use
// command/args. Secrets/tokens are stored in the OS keychain, never in the
// file.) The `type:"http"` discriminator is REQUIRED — Cortex distinguishes a
// remote HTTP server from a local stdio one by this field — so AddEntry is
// overridden to emit it (the base jsonMCPClient.AddEntry would omit `type` and
// add a `disabled:false` flag the documented http shape does not carry). An
// optional `headers` object is emitted when MCPEntry.Headers is non-empty.
//
// Scope note: Cortex resolves config across scopes — global
// `~/.snowflake/cortex/mcp.json`, workspace `<ws>/.snowflake/cortex/mcp.json`
// or `<ws>/.cortex/mcp.json`. The hub writes the canonical GLOBAL
// `~/.snowflake/cortex/mcp.json`.
//
// Because the endpoint field is the standard `url` key and the hub entry has
// no `command`, the base jsonMCPClient hub-shape detection (isHubURLShapeEntry
// keyed on urlField "url") recognizes a hub Cortex entry for demigrate/rollback
// unchanged — so the restore/predicate methods are inherited verbatim (same
// posture as the copilot-cli adapter).
//
// Source (verified 2026-06-17):
//   - https://docs.snowflake.com/en/user-guide/cortex-code/cortex-code-mcp —
//     "MCP servers are configured in: `~/.snowflake/cortex/mcp.json`";
//     top-level `mcpServers`; http entry `{"type":"http","url":...}` (also an
//     optional `oauth` object); SSE via `type:"sse"`; tokens in OS keychain.
func NewCortex() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultCortexConfigPath(home),
		clientName: "cortex",
		// Cortex's remote endpoint key is the standard "url". The base
		// GetEntry / demigrate helpers key off this field; AddEntry is
		// overridden below to add the `type:"http"` discriminator.
		urlField: "url",
	}
	return newLockingClient(&cortexClient{jsonMCPClient: base}), nil
}

// defaultCortexConfigPath returns the default Snowflake Cortex Code global MCP
// config path, ~/.snowflake/cortex/mcp.json. The path is OS-independent
// (Cortex uses ~/.snowflake/cortex on every OS), mirroring the
// copilot-cli/openclaw home-relative posture.
func defaultCortexConfigPath(home string) string {
	return filepath.Join(home, ".snowflake", "cortex", "mcp.json")
}

// cortexClient overrides only AddEntry (to emit the `type:"http"`
// discriminator) and the filesystem-bootstrap methods (Exists, Backup,
// BackupKeep) so a fresh host with no ~/.snowflake/cortex directory still
// installs cleanly. GetEntry, RemoveEntry, Restore, InitEmpty, and every
// backup/demigrate helper are promoted from the embedded jsonMCPClient
// unchanged — the hub entry's loopback `url` + absent `command` makes the base
// hub-shape detection correct without override (same posture as the copilot-cli
// adapter).
type cortexClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.snowflake/cortex) does — mirroring the
// cursor/kiro/copilot-cli "directory means installed" heuristic so an operator
// who has Cortex Code installed but no MCP config yet still gets the
// Initialize / install affordance.
func (c *cortexClient) Exists() bool {
	if _, err := os.Stat(c.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(c.path))
	return err == nil && st.IsDir()
}

func (c *cortexClient) Backup() (string, error) {
	return c.BackupKeep(0)
}

// BackupKeep ensures the ~/.snowflake/cortex parent directory exists, seeds an
// empty `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir is two levels deep and
// may not exist on a clean install, so the MkdirAll here is load-bearing.
// Mirrors the cursor/kiro/copilot-cli BackupKeep wrappers.
func (c *cortexClient) BackupKeep(keepN int) (string, error) {
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

// AddEntry writes the hub-managed http-transport entry under mcpServers.<name>.
// Cortex's remote entry shape is `{"type":"http","url":...}`; an optional
// `headers` object is emitted when MCPEntry.Headers is non-empty. The base
// jsonMCPClient.AddEntry would omit the required `type` discriminator (and add
// a `disabled:false` flag the documented http shape does not carry), so this
// override is required.
func (c *cortexClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return c.setMember(entry.Name, serverEntry)
}

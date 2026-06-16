package clients

import (
	"os"
	"path/filepath"
)

// NewCommandCode returns a Client bound to Command Code's user-level MCP config:
//
//	~/.commandcode/mcp.json
//
// Command Code (commandcode.ai; npm `command-code`, run via `cmd`) reads MCP
// servers from a JSON file under the top-level `mcpServers` key — the canonical
// JSON family schema shared by Claude Code / Cursor / Gemini CLI. Servers are
// added via `cmd mcp add` / `cmd mcp add-json` and editable on disk. Each entry
// is discriminated by a `type` field; the documented `add-json` form for a
// remote HTTP server is:
//
//	cmd mcp add-json github '{"type":"http","url":"https://..."}'
//
// which stores under mcpServers.<name>:
//
//	"<server-name>": {
//	  "type": "http",
//	  "url": "https://.../mcp"
//	}
//
// Command Code speaks HTTP MCP natively (also `cmd mcp add --transport http`),
// so this adapter is HTTP-direct (NOT relay-stdio). The `type:"http"`
// discriminator is REQUIRED, so AddEntry is overridden to emit it (the base
// jsonMCPClient.AddEntry would omit `type` and add a `disabled:false` flag the
// documented http shape does not carry). An optional `headers` object is
// emitted when MCPEntry.Headers is non-empty.
//
// Scope note: Command Code resolves config across three scopes — user/global
// `~/.commandcode/mcp.json`, project `./.mcp.json`, local
// `~/.commandcode/projects/<slug>/mcp.json` (local overrides project overrides
// user). The hub writes the canonical USER/GLOBAL `~/.commandcode/mcp.json`.
//
// Because the endpoint field is the standard `url` key and the hub entry has
// no `command`, the base jsonMCPClient hub-shape detection (isHubURLShapeEntry
// keyed on urlField "url") recognizes a hub Command Code entry for
// demigrate/rollback unchanged — so the restore/predicate methods are inherited
// verbatim (same posture as the copilot-cli adapter).
//
// Source (verified 2026-06-17):
//   - https://commandcode.ai/docs/mcp — three scopes (user
//     `~/.commandcode/mcp.json`, project `.mcp.json`, local
//     `~/.commandcode/projects/<slug>/mcp.json`); `cmd mcp add-json github
//     '{"type":"http","url":"https://..."}'`; `cmd mcp add --transport http`.
//     Top-level `mcpServers` key per the JSON family.
func NewCommandCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultCommandCodeConfigPath(home),
		clientName: "command-code",
		// Command Code's remote endpoint key is the standard "url". The base
		// GetEntry / demigrate helpers key off this field; AddEntry is
		// overridden below to add the `type:"http"` discriminator.
		urlField: "url",
	}
	return newLockingClient(&commandCodeClient{jsonMCPClient: base}), nil
}

// defaultCommandCodeConfigPath returns the default Command Code user/global MCP
// config path, ~/.commandcode/mcp.json. The path is OS-independent (Command
// Code uses ~/.commandcode on every OS), mirroring the copilot-cli/openclaw
// home-relative posture.
func defaultCommandCodeConfigPath(home string) string {
	return filepath.Join(home, ".commandcode", "mcp.json")
}

// commandCodeClient overrides only AddEntry (to emit the `type:"http"`
// discriminator) and the filesystem-bootstrap methods (Exists, Backup,
// BackupKeep) so a fresh host with no ~/.commandcode directory still installs
// cleanly. GetEntry, RemoveEntry, Restore, InitEmpty, and every
// backup/demigrate helper are promoted from the embedded jsonMCPClient
// unchanged — the hub entry's loopback `url` + absent `command` makes the base
// hub-shape detection correct without override (same posture as the copilot-cli
// adapter).
type commandCodeClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.commandcode) does — mirroring the
// cursor/kiro/copilot-cli "directory means installed" heuristic so an operator
// who has Command Code installed but no MCP config yet still gets the
// Initialize / install affordance.
func (c *commandCodeClient) Exists() bool {
	if _, err := os.Stat(c.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(c.path))
	return err == nil && st.IsDir()
}

func (c *commandCodeClient) Backup() (string, error) {
	return c.BackupKeep(0)
}

// BackupKeep ensures the ~/.commandcode parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing. Mirrors the
// cursor/kiro/copilot-cli BackupKeep wrappers.
func (c *commandCodeClient) BackupKeep(keepN int) (string, error) {
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
// Command Code's remote entry shape is `{"type":"http","url":...}`; an optional
// `headers` object is emitted when MCPEntry.Headers is non-empty. The base
// jsonMCPClient.AddEntry would omit the required `type` discriminator (and add
// a `disabled:false` flag the documented http shape does not carry), so this
// override is required.
func (c *commandCodeClient) AddEntry(entry MCPEntry) error {
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

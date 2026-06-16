package clients

import (
	"os"
	"path/filepath"
)

// NewDeepAgents returns a Client bound to LangChain Deep Agents CLI's
// user-level MCP config:
//
//	~/.deepagents/.mcp.json
//
// LangChain Deep Agents CLI (the `deepagents_cli` / `deepagents_code` Python
// packages) reads MCP servers from a JSON file under the top-level `mcpServers`
// key — the canonical Claude-Desktop-style JSON family schema. The user-level
// file at `~/.deepagents/.mcp.json` is "available in every project on this
// machine" (project-level `<project>/.mcp.json` merges with project
// precedence). Each entry is discriminated by a `type` field; the documented
// remote (http) entry shape is:
//
//	"<server-name>": {
//	  "type": "http",
//	  "url": "https://docs.langchain.com/mcp"
//	}
//
// Deep Agents speaks HTTP MCP natively, so this adapter is HTTP-direct (NOT
// relay-stdio). The `type:"http"` discriminator is REQUIRED for an HTTP server
// (the docs note `type` may alternatively be written as `transport` for
// cross-client compatibility), so AddEntry is overridden to emit it (the base
// jsonMCPClient.AddEntry would omit `type` and add a `disabled:false` flag the
// documented http shape does not carry). An optional `headers` object is
// emitted when MCPEntry.Headers is non-empty.
//
// Because the endpoint field is the standard `url` key and the hub entry has
// no `command`, the base jsonMCPClient hub-shape detection (isHubURLShapeEntry
// keyed on urlField "url") recognizes a hub Deep Agents entry for
// demigrate/rollback unchanged — so the restore/predicate methods are inherited
// verbatim (same posture as the copilot-cli adapter).
//
// Source (verified 2026-06-17):
//   - https://docs.langchain.com/oss/python/deepagents/code/mcp-tools —
//     user-level `~/.deepagents/.mcp.json` ("available in every project on this
//     machine"), project-level `<project>/.mcp.json`; top-level `mcpServers`;
//     http entry `{"type":"http","url":...}` (+ optional `headers`,
//     `auth:"oauth"`); `type` may also be written as `transport`.
func NewDeepAgents() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultDeepAgentsConfigPath(home),
		clientName: "deepagents",
		// Deep Agents' remote endpoint key is the standard "url". The base
		// GetEntry / demigrate helpers key off this field; AddEntry is
		// overridden below to add the `type:"http"` discriminator.
		urlField: "url",
	}
	return newLockingClient(&deepAgentsClient{jsonMCPClient: base}), nil
}

// defaultDeepAgentsConfigPath returns the default Deep Agents CLI user-level MCP
// config path, ~/.deepagents/.mcp.json. The path is OS-independent (Deep Agents
// uses ~/.deepagents on every OS), mirroring the copilot-cli/openclaw
// home-relative posture. Note the filename is dot-prefixed (`.mcp.json`), not
// `mcp.json`.
func defaultDeepAgentsConfigPath(home string) string {
	return filepath.Join(home, ".deepagents", ".mcp.json")
}

// deepAgentsClient overrides only AddEntry (to emit the `type:"http"`
// discriminator) and the filesystem-bootstrap methods (Exists, Backup,
// BackupKeep) so a fresh host with no ~/.deepagents directory still installs
// cleanly. GetEntry, RemoveEntry, Restore, InitEmpty, and every
// backup/demigrate helper are promoted from the embedded jsonMCPClient
// unchanged — the hub entry's loopback `url` + absent `command` makes the base
// hub-shape detection correct without override (same posture as the copilot-cli
// adapter).
type deepAgentsClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.deepagents) does — mirroring the
// cursor/kiro/copilot-cli "directory means installed" heuristic so an operator
// who has Deep Agents installed but no MCP config yet still gets the
// Initialize / install affordance.
func (d *deepAgentsClient) Exists() bool {
	if _, err := os.Stat(d.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(d.path))
	return err == nil && st.IsDir()
}

func (d *deepAgentsClient) Backup() (string, error) {
	return d.BackupKeep(0)
}

// BackupKeep ensures the ~/.deepagents parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing. Mirrors the
// cursor/kiro/copilot-cli BackupKeep wrappers.
func (d *deepAgentsClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(d.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := d.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(d.path, d.Name(), keepN)
}

// AddEntry writes the hub-managed http-transport entry under mcpServers.<name>.
// Deep Agents' remote entry shape is `{"type":"http","url":...}`; an optional
// `headers` object is emitted when MCPEntry.Headers is non-empty. The base
// jsonMCPClient.AddEntry would omit the required `type` discriminator (and add
// a `disabled:false` flag the documented http shape does not carry), so this
// override is required.
func (d *deepAgentsClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return d.setMember(entry.Name, serverEntry)
}

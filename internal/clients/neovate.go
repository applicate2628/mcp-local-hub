package clients

import (
	"os"
	"path/filepath"
)

// NewNeovate returns a Client bound to Neovate Code's user-level (global) MCP
// config at ~/.neovate/config.json.
//
// Neovate Code (a terminal AI coding agent) reads MCP servers from a JSON file
// under the canonical top-level `mcpServers` key — the standard object-map
// family schema (each server keyed by a unique name, NOT an array). Neovate's
// config is HIERARCHICAL: a global ~/.neovate/config.json plus project
// .neovate/config.json and local .neovate/config.local.json, all merged. This
// adapter targets the global user file; because every write goes through the
// comment-preserving JSONC seam, Neovate's other config keys (model, provider,
// etc.) in the same file survive untouched.
//
// Each entry is a per-server object with either `command`/`args`/`env` (stdio)
// or `url`/`headers` (http/sse). Neovate speaks HTTP/SSE MCP natively
// (src/config.ts carries url+headers fields, and `neovate mcp add --transport
// http|sse <url>` corroborates url support), so the hub writes an HTTP-direct
// entry — the loopback daemon URL — and IsRelayStdio() is false (inherited from
// jsonMCPClient).
//
// Hub-managed entry shape written:
//
//	"<server-name>": {
//	  "type": "http",
//	  "url": "http://localhost:9121/mcp"
//	}
//
// (plus an optional `headers` object when MCPEntry.Headers is non-empty). The
// `type:"http"` discriminator mirrors the other HTTP-native adapters (vscode /
// warp / crush); ASSUMPTION (UNVERIFIED): Neovate infers transport from the
// `url` presence and tolerates the explicit `type` key — confirm against
// src/config.ts on a host with Neovate installed. The endpoint field is the
// standard `url` key, so the embedded jsonMCPClient's hub-shape detection
// (isHubURLShapeEntry(_, "url")) and the inherited GetEntry / RemoveEntry /
// RestoreEntryFromBackup / BackupEntryIsHubManaged helpers all work unchanged.
//
// Source (verified 2026-06-17):
//   - https://deepwiki.com/neovateai/neovate-code/5.3-mcp-server-integration —
//     hierarchical config (~/.neovate/config.json global), top-level
//     `mcpServers` object map, stdio + http + sse transports (url+headers).
func NewNeovate() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".neovate", "config.json"),
		clientName: "neovate",
		// Standard `url` endpoint key; serversKey left empty -> "mcpServers".
		urlField: "url",
	}
	return newLockingClient(&neovateClient{jsonMCPClient: base}), nil
}

// neovateClient overrides AddEntry (to emit the `type:"http"` discriminator)
// and the filesystem-bootstrap methods (Exists, Backup, BackupKeep) so a fresh
// host with no ~/.neovate directory still installs cleanly. Every other method
// is promoted from the embedded jsonMCPClient unchanged.
type neovateClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.neovate) does — mirroring the warp/droid "directory
// means installed" heuristic so an operator who has Neovate installed but no
// MCP config yet still gets the Initialize / install affordance.
func (n *neovateClient) Exists() bool {
	if _, err := os.Stat(n.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(n.path))
	return err == nil && st.IsDir()
}

func (n *neovateClient) Backup() (string, error) {
	return n.BackupKeep(0)
}

// BackupKeep ensures the ~/.neovate parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). Mirrors the warp BackupKeep.
func (n *neovateClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(n.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := n.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(n.path, n.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under `mcpServers` with
// Neovate's HTTP shape (`type:"http"` + `url`, plus optional `headers`). The
// base jsonMCPClient.AddEntry would omit the `type` discriminator, so this
// override is required.
func (n *neovateClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	return n.setMember(entry.Name, serverEntry)
}

package clients

import (
	"os"
	"path/filepath"
)

// NewTabnineCLI returns a Client bound to the Tabnine CLI / Tabnine Agent's
// user-level MCP config at ~/.tabnine/mcp_servers.json.
//
// Tabnine CLI (Tabnine Agent) reads MCP servers from a JSON file under the
// canonical top-level `mcpServers` key. Tabnine auto-detects the transport
// from the entry's fields: an entry with a `command` field is stdio, an
// entry with a `url` field is a remote HTTP server (defaulting to Streamable
// HTTP), and an explicit `transport:"sse"` selects legacy SSE. There is NO
// `type` discriminator key — the presence of `url` is the HTTP signal.
// Because Tabnine speaks HTTP MCP natively, this adapter is HTTP-direct (NOT
// relay-stdio): a hub daemon's loopback URL is written as a `url` entry.
// IsRelayStdio() returns false (inherited from jsonMCPClient).
//
// The hub-managed remote entry shape is:
//
//	"<server-name>": {
//	  "url": "http://localhost:9121/mcp"
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty.
// Tabnine's docs surface auth headers under a `requestInit.headers` nest;
// the hub writes a top-level `headers` object instead, matching the rest of
// the JSON-family adapters so the base GetEntry / hub-shape predicate
// (which read `headers`) round-trip correctly. mcphub's loopback hub URL
// needs no auth headers, so this is purely a forward-compat nicety; operators
// who later add a header can move it under requestInit by hand if Tabnine
// requires it. Because the hub entry carries a loopback `url` and NO
// `command`, the base jsonMCPClient hub-shape detection (isHubURLShapeEntry
// keyed on urlField "url") recognizes a hub Tabnine entry for demigrate/
// rollback unchanged.
//
// This bare-`url` shape (no `type`, no `disabled`) is the base
// jsonMCPClient.AddEntry shape MINUS the `disabled:false` field — Tabnine's
// docs do not show a `disabled` key on remote entries, so AddEntry is
// overridden below to omit it and keep the entry minimal.
//
// Sources (verified 2026-06-17):
//   - https://docs.tabnine.com/main/getting-started/tabnine-agent/mcp-intro-and-setup
//     — config at ~/.tabnine/mcp_servers.json (also project-relative
//     .tabnine/mcp_servers.json); top-level `mcpServers`; remote entry
//     detected by the presence of a `url` field (defaults to Streamable
//     HTTP); legacy SSE via `transport:"sse"`; auth via `requestInit.headers`.
func NewTabnineCLI() (Client, error) {
	base := &jsonMCPClient{
		path:       defaultTabnineCLIConfigPath(),
		clientName: "tabnine-cli",
		// Tabnine's remote endpoint key is "url"; the base helpers (GetEntry,
		// hub-shape detection, restore guards) key off urlField. AddEntry is
		// overridden below only to emit the minimal bare-`url` shape (no
		// `disabled` field) Tabnine documents.
		urlField: "url",
	}
	return newLockingClient(&tabnineCLIClient{jsonMCPClient: base}), nil
}

// defaultTabnineCLIConfigPath returns ~/.tabnine/mcp_servers.json. A home-dir
// lookup failure degrades to a bare "mcp_servers.json" relative path rather
// than panicking (matching the fail-safe posture AllClients uses).
func defaultTabnineCLIConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mcp_servers.json"
	}
	return filepath.Join(home, ".tabnine", "mcp_servers.json")
}

// tabnineCLIClient overrides only AddEntry (to emit the minimal bare-`url`
// shape without the base's `disabled:false` field) and the filesystem-
// bootstrap methods (Exists, Backup, BackupKeep) so a fresh host with no
// ~/.tabnine directory still installs cleanly. GetEntry, RemoveEntry,
// Restore, InitEmpty, and every backup/demigrate helper are promoted from
// the embedded jsonMCPClient unchanged.
type tabnineCLIClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists
// OR its parent directory (~/.tabnine) does — mirroring the cursor/copilot
// "directory means installed" heuristic.
func (t *tabnineCLIClient) Exists() bool {
	if _, err := os.Stat(t.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(t.path))
	return err == nil && st.IsDir()
}

func (t *tabnineCLIClient) Backup() (string, error) {
	return t.BackupKeep(0)
}

// BackupKeep ensures the ~/.tabnine parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing.
func (t *tabnineCLIClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(t.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := t.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(t.path, t.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map
// with Tabnine's minimal bare-`url` shape (transport auto-detected from the
// presence of `url`), plus an optional top-level `headers` object. The base
// jsonMCPClient.AddEntry writes a `disabled:false` field Tabnine does not
// document on remote entries, so this override emits the entry without it.
func (t *tabnineCLIClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"url": entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return t.setMember(entry.Name, serverEntry)
}

package clients

import (
	"os"
	"path/filepath"
)

// NewQoder returns a Client bound to the Qoder agentic IDE's user-level
// MCP config at ~/.qoder/mcp-settings.json.
//
// Qoder reads MCP servers from a JSON file under the canonical top-level
// `mcpServers` key (the shape edited via Qoder Settings > MCP's JSON
// editor). Qoder speaks HTTP MCP natively via the Streamable HTTP transport
// (its newer, more capable successor to SSE), so the hub writes an
// HTTP-direct entry (no relay shim needed). IsRelayStdio() returns false
// (inherited from jsonMCPClient).
//
// The hub-managed remote entry shape is:
//
//	"<server-name>": {
//	  "type": "streamable-http",
//	  "url": "http://localhost:9121/mcp",
//	  "disabled": false
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty.
// Qoder distinguishes a remote HTTP server from a local stdio one by the
// `type` value: a remote entry uses `type:"streamable-http"` (or `sse`) +
// `url`; a local entry uses `command`/`args`. Because the hub entry carries
// a loopback `url` and NO `command`, the base jsonMCPClient hub-shape
// detection (isHubURLShapeEntry keyed on urlField "url") recognizes a hub
// Qoder entry for demigrate/rollback unchanged — so the restore/predicate
// methods are inherited verbatim. This mirrors the Kilo Code adapter
// exactly (same Streamable HTTP family, same `type` value).
//
// Path note: the home-relative user file ~/.qoder/mcp-settings.json is the
// canonical location. On Windows the settings UI also surfaces it at
// %APPDATA%\Qoder\mcp-settings.json; the home-relative form is used here as
// the cross-platform user file (the GUI Initialize/install affordance and
// Exists probe target this path).
//
// Sources (verified 2026-06-17):
//   - https://docs.qoder.com/user-guide/chat/model-context-protocol — MCP
//     managed via Qoder Settings > MCP (JSON editor); remote entry uses
//     `type` ("sse" / streamable-http) + `url`; top-level `mcpServers`.
//   - https://docs.qoder.com/qoderwork/mcp — exact remote-entry shape
//     `{"type":"streamable-http","url":...,"headers":{...}}` under the
//     top-level `mcpServers` key; "Streamable HTTP is the newer, more
//     capable successor to SSE."
func NewQoder() (Client, error) {
	base := &jsonMCPClient{
		path:       defaultQoderConfigPath(),
		clientName: "qoder",
		// Qoder's remote-server endpoint key is "url"; the base helpers
		// (GetEntry fallback, hub-shape detection, restore guards) key off
		// urlField. AddEntry is overridden below only to add the
		// `type:"streamable-http"` discriminator the base does not write.
		urlField: "url",
	}
	return newLockingClient(&qoderClient{jsonMCPClient: base}), nil
}

// qoderMCPType is Qoder's remote-HTTP transport discriminator. Single owner
// of the "streamable-http" literal so the writer stays in sync with the
// documented shape.
const qoderMCPType = "streamable-http"

// defaultQoderConfigPath returns ~/.qoder/mcp-settings.json. A home-dir
// lookup failure degrades to a bare "mcp-settings.json" relative path rather
// than panicking (matching the fail-safe posture AllClients uses).
func defaultQoderConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mcp-settings.json"
	}
	return filepath.Join(home, ".qoder", "mcp-settings.json")
}

// qoderClient overrides only AddEntry (to emit the `type:"streamable-http"`
// discriminator) and the filesystem-bootstrap methods (Exists, Backup,
// BackupKeep) so a fresh host with no ~/.qoder directory still installs
// cleanly. GetEntry, RemoveEntry, Restore, InitEmpty, and every backup/
// demigrate helper are promoted from the embedded jsonMCPClient unchanged.
type qoderClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists
// OR its parent directory (~/.qoder) does — mirroring the cursor/kilocode
// "directory means installed" heuristic.
func (q *qoderClient) Exists() bool {
	if _, err := os.Stat(q.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(q.path))
	return err == nil && st.IsDir()
}

func (q *qoderClient) Backup() (string, error) {
	return q.BackupKeep(0)
}

// BackupKeep ensures the ~/.qoder parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). Mirrors the kilocode BackupKeep.
func (q *qoderClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(q.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := q.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(q.path, q.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map
// with Qoder's Streamable HTTP shape: `type:"streamable-http"` + `url` +
// `disabled:false`, plus an optional `headers` object. The base
// jsonMCPClient.AddEntry would omit the `type` discriminator, so this
// override is required.
func (q *qoderClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type":     qoderMCPType,
		"url":      entry.URL,
		"disabled": false,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return q.setMember(entry.Name, serverEntry)
}

package clients

import (
	"os"
	"path/filepath"
)

// NewRovoDev returns a Client bound to the Atlassian Rovo Dev CLI's
// user-level MCP config at ~/.rovodev/mcp.json.
//
// Rovo Dev CLI (`acli rovodev`) is a shipping agentic coding CLI. It reads
// MCP servers from a JSON file under the canonical top-level `mcpServers`
// key (openable via `acli rovodev mcp`). Rovo Dev distinguishes transports
// by a `transport` field on each entry: a remote HTTP server uses
// `transport:"http"` (or `transport:"sse"`) + `url`; a local stdio server
// uses `command`/`args`. Because Rovo Dev speaks HTTP MCP natively, this
// adapter is HTTP-direct (NOT relay-stdio): a hub daemon's loopback URL is
// written as an http entry. IsRelayStdio() returns false (inherited from
// jsonMCPClient).
//
// The hub-managed remote entry shape is:
//
//	"<server-name>": {
//	  "url": "http://localhost:9121/mcp",
//	  "transport": "http"
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty.
// Note Rovo Dev's transport discriminator is the `transport` key (value
// "http"), NOT the `type` key used by the Roo/Cursor/Kilo family — hence the
// bespoke AddEntry override below. Because the hub entry carries a loopback
// `url` and NO `command`, the base jsonMCPClient hub-shape detection
// (isHubURLShapeEntry keyed on urlField "url") recognizes a hub Rovo Dev
// entry for demigrate/rollback unchanged, so the restore/predicate methods
// are inherited verbatim.
//
// Sources (verified 2026-06-17):
//   - https://support.atlassian.com/rovo/docs/connect-to-an-mcp-server-in-rovo-dev-cli/
//     — global config ~/.rovodev/mcp.json; top-level `mcpServers`; HTTP
//     entry `{"url":...,"headers":{...},"transport":"http","enable_instructions":true}`;
//     SSE entry `{"url":...,"transport":"sse"}`. Remote servers use a `url`
//     field (NOT `httpUrl`); `transport` explicitly names the protocol.
func NewRovoDev() (Client, error) {
	base := &jsonMCPClient{
		path:       defaultRovoDevConfigPath(),
		clientName: "rovodev",
		// Rovo Dev's remote endpoint key is "url"; the base helpers (GetEntry
		// fallback, hub-shape detection, restore guards) key off urlField.
		// AddEntry is overridden below to add the `transport:"http"`
		// discriminator (Rovo Dev uses `transport`, not `type`).
		urlField: "url",
	}
	return newLockingClient(&rovoDevClient{jsonMCPClient: base}), nil
}

// rovoDevTransport is Rovo Dev's remote-HTTP transport discriminator value.
// Single owner of the "http" literal so the writer stays in sync with the
// documented shape (the KEY is "transport", distinct from the Roo family's
// "type").
const rovoDevTransport = "http"

// defaultRovoDevConfigPath returns ~/.rovodev/mcp.json. A home-dir lookup
// failure degrades to a bare "mcp.json" relative path rather than panicking
// (matching the fail-safe posture AllClients uses).
func defaultRovoDevConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mcp.json"
	}
	return filepath.Join(home, ".rovodev", "mcp.json")
}

// rovoDevClient overrides only AddEntry (to emit the `transport:"http"`
// discriminator) and the filesystem-bootstrap methods (Exists, Backup,
// BackupKeep) so a fresh host with no ~/.rovodev directory still installs
// cleanly. GetEntry, RemoveEntry, Restore, InitEmpty, and every backup/
// demigrate helper are promoted from the embedded jsonMCPClient unchanged.
type rovoDevClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists
// OR its parent directory (~/.rovodev) does — mirroring the cursor/copilot
// "directory means installed" heuristic.
func (r *rovoDevClient) Exists() bool {
	if _, err := os.Stat(r.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(r.path))
	return err == nil && st.IsDir()
}

func (r *rovoDevClient) Backup() (string, error) {
	return r.BackupKeep(0)
}

// BackupKeep ensures the ~/.rovodev parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing.
func (r *rovoDevClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(r.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := r.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(r.path, r.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map
// with Rovo Dev's HTTP shape: `url` + `transport:"http"`, plus an optional
// `headers` object. The base jsonMCPClient.AddEntry writes `disabled:false`
// and no `transport`, so this override is required to produce the documented
// Rovo Dev shape.
func (r *rovoDevClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"url":       entry.URL,
		"transport": rovoDevTransport,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return r.setMember(entry.Name, serverEntry)
}

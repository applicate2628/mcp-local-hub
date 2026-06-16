package clients

import (
	"os"
	"path/filepath"
)

// NewQoderCN returns a Client bound to Qoder CN's user-level MCP config at
// ~/.qoder-cn.json.
//
// Qoder CN (Aliyun Lingma's China edition of Qoder; CLI `qoderclicn`) reads
// MCP servers from a flat JSON file in the user's home directory. Servers
// are added via `qoderclicn mcp add -t {stdio|sse|streamable-http} -s {user|
// project}`; the user/global scope writes ~/.qoder-cn.json (the project
// scope writes ${project}/.mcp.json, which the hub does NOT touch). The file
// uses the canonical top-level `mcpServers` key. Qoder CN speaks HTTP MCP
// natively via the Streamable HTTP transport, so the hub writes an
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
// Qoder CN distinguishes a remote HTTP server from a local stdio one by the
// `type` value: a remote entry uses `type:"streamable-http"` (or `sse`) +
// `url`; a local entry uses `command`/`args`. Because the hub entry carries
// a loopback `url` and NO `command`, the base jsonMCPClient hub-shape
// detection (isHubURLShapeEntry keyed on urlField "url") recognizes a hub
// Qoder CN entry for demigrate/rollback unchanged — restore/predicate
// methods are inherited verbatim. Same Streamable HTTP family / `type`
// value as the Qoder and Kilo Code adapters.
//
// Sources (verified 2026-06-17):
//   - https://help.aliyun.com/zh/lingma/qoder-cn/user-guide/guide-for-using-mcp
//     — `qoderclicn mcp add -t {stdio|sse|streamable-http} -s {user|project}`;
//     user scope → ~/.qoder-cn.json, project scope → ${project}/.mcp.json.
//   - Qoder family shape (https://docs.qoder.com/qoderwork/mcp + forum
//     thread forum.qoder.com/t/.../5553): remote entry
//     `{"type":"streamable-http","url":...}` under top-level `mcpServers`.
func NewQoderCN() (Client, error) {
	base := &jsonMCPClient{
		path:       defaultQoderCNConfigPath(),
		clientName: "qoder-cn",
		// Qoder CN's remote-server endpoint key is "url"; the base helpers
		// key off urlField. AddEntry is overridden below only to add the
		// `type:"streamable-http"` discriminator.
		urlField: "url",
	}
	return newLockingClient(&qoderCNClient{jsonMCPClient: base}), nil
}

// defaultQoderCNConfigPath returns ~/.qoder-cn.json. A home-dir lookup
// failure degrades to a bare ".qoder-cn.json" relative path rather than
// panicking (matching the fail-safe posture AllClients uses).
func defaultQoderCNConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".qoder-cn.json"
	}
	return filepath.Join(home, ".qoder-cn.json")
}

// qoderCNClient overrides only AddEntry (to emit the
// `type:"streamable-http"` discriminator) and Backup/BackupKeep (to seed an
// empty stub on a fresh host). The config is a flat dotfile directly in the
// home directory, so no parent-dir MkdirAll beyond the home dir is needed,
// and Exists is inherited from the base (a flat dotfile has no meaningful
// parent-dir "installed" signal beyond the home dir itself). GetEntry,
// RemoveEntry, Restore, InitEmpty, and every backup/demigrate helper are
// promoted from the embedded jsonMCPClient unchanged.
type qoderCNClient struct {
	*jsonMCPClient
}

func (q *qoderCNClient) Backup() (string, error) {
	return q.BackupKeep(0)
}

// BackupKeep seeds an empty `{"mcpServers": {}}` stub if the config is
// absent, then writes the timestamped backup (pruning to keepN). The parent
// is the user's home directory (always present), so no MkdirAll is needed.
func (q *qoderCNClient) BackupKeep(keepN int) (string, error) {
	if _, err := q.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(q.path, q.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map
// with Qoder CN's Streamable HTTP shape: `type:"streamable-http"` + `url` +
// `disabled:false`, plus an optional `headers` object. The base
// jsonMCPClient.AddEntry would omit the `type` discriminator, so this
// override is required.
func (q *qoderCNClient) AddEntry(entry MCPEntry) error {
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

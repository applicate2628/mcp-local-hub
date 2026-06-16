package clients

import (
	"os"
	"path/filepath"
)

// NewOna returns a Client bound to Ona's user-level MCP config at
// <~/.ona>/mcp-config.json.
//
// Ona (formerly Gitpod) is an AI software-engineer agent. It reads MCP
// servers from a JSON file using the canonical `{"mcpServers": {...}}`
// family schema, keyed by server name. Each entry sets EITHER `command`
// (stdio transport) OR `url` (HTTP transport) — a server cannot have both.
// Because Ona speaks HTTP MCP natively, this adapter is HTTP-direct (NOT
// relay-stdio): a hub daemon's loopback URL is written as a `url` entry.
// IsRelayStdio() therefore returns false (inherited from jsonMCPClient).
//
// The hub-managed remote entry shape is:
//
//	"<server-name>": {
//	  "url": "http://localhost:9121/mcp",
//	  "disabled": false
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty.
// This is exactly the base jsonMCPClient.AddEntry shape (bare `url` +
// `disabled:false` + optional `headers`), so AddEntry is inherited
// unchanged — Ona uses NO `type` discriminator for HTTP entries (the
// presence of `url` is the transport signal). The loopback `url` + absent
// `command` makes the base hub-shape detection correct, so the demigrate/
// rollback predicate methods are inherited verbatim too.
//
// Path note: Ona's documentation shows the config at the env/repo-relative
// path `.ona/mcp-config.json`; no separate home-relative GLOBAL location is
// documented. mcp-local-hub installs GLOBALLY (a single user-level file),
// so this adapter targets the most-global documented location: the same
// `.ona/mcp-config.json` resolved under the user's home directory
// (~/.ona/mcp-config.json). An ONA_HOME-style override is NOT documented,
// so none is implemented.
//
// Sources (verified 2026-06-17):
//   - https://gitpod-13c83c2b.mintlify.dev/docs/ona/mcp — config at
//     `.ona/mcp-config.json`; top-level `mcpServers` key; HTTP entry uses
//     a `url` field (must start with `http://`/`https://`) with optional
//     `headers` + `timeout`; stdio entry uses `command` (a server cannot
//     have both `command` and `url`). No `type` discriminator for HTTP.
func NewOna() (Client, error) {
	base := &jsonMCPClient{
		path:       defaultOnaConfigPath(),
		clientName: "ona",
		// Ona's remote endpoint key is "url"; the base AddEntry / GetEntry /
		// hub-shape detection all key off this field. No `type` override is
		// needed because Ona uses a bare `url` entry (no discriminator).
		urlField: "url",
	}
	return newLockingClient(&onaClient{jsonMCPClient: base}), nil
}

// defaultOnaConfigPath returns ~/.ona/mcp-config.json. A home-dir lookup
// failure degrades to a bare "mcp-config.json" relative path rather than
// panicking (matching the fail-safe posture AllClients uses to drop
// unconstructable adapters — Exists() on such a path simply reports absent).
func defaultOnaConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mcp-config.json"
	}
	return filepath.Join(home, ".ona", "mcp-config.json")
}

// onaClient overrides only the filesystem-bootstrap methods (Exists,
// Backup, BackupKeep) so a fresh host with no ~/.ona directory still
// installs cleanly. AddEntry, GetEntry, RemoveEntry, Restore, InitEmpty,
// and every backup/demigrate helper are promoted from the embedded
// jsonMCPClient unchanged — Ona's bare-`url` shape matches the base
// AddEntry exactly.
type onaClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists
// OR its parent directory (~/.ona) does — mirroring the cursor/copilot
// "directory means installed" heuristic so an operator who has Ona
// installed but no MCP config yet still gets the Initialize / install
// affordance.
func (o *onaClient) Exists() bool {
	if _, err := os.Stat(o.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(o.path))
	return err == nil && st.IsDir()
}

func (o *onaClient) Backup() (string, error) {
	return o.BackupKeep(0)
}

// BackupKeep ensures the ~/.ona parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing. Mirrors the
// copilot/cursor BackupKeep wrappers.
func (o *onaClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(o.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := o.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(o.path, o.Name(), keepN)
}

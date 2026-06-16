package clients

import (
	"os"
	"path/filepath"
)

// NewBob returns a Client bound to IBM Bob's global MCP settings file:
//
//	~/.bob/mcp_settings.json
//
// IBM Bob (an agentic IDE) reads MCP servers from a JSON file under the
// top-level `mcpServers` key — the canonical JSON family schema shared by
// Cursor / Gemini CLI / Cline. Bob supports remote MCP servers (SSE /
// Streamable HTTP) natively, so the hub writes an HTTP-direct entry (no relay
// shim needed). A remote entry's transport is inferred from the presence of a
// `url` key — Bob does NOT require (or document) a `type`/`transport`
// discriminator field for remote servers. The documented remote-entry shape is:
//
//	"<server-name>": {
//	  "url": "https://your-server-url.com/mcp",
//	  "headers": { "Authorization": "Bearer ***" },
//	  "alwaysAllow": ["tool3"],
//	  "disabled": false
//	}
//
// This is exactly the bare jsonMCPClient hub shape (`{"url":..., "disabled":
// false}` + optional `headers`), so no AddEntry override is needed — the base
// `urlField:"url"` adapter writes the correct entry and every backup/demigrate
// helper keys off the standard `url` field unchanged.
//
// Scope note: Bob exposes BOTH a global file (`~/.bob/mcp_settings.json`,
// "applies across all workspaces") and a project file (`.bob/mcp.json`). The
// hub writes the GLOBAL file, matching every other adapter's user-scoped
// posture; the project file is auto-detected per repo by Bob and the hub does
// NOT touch it. The path is OS-independent by design — Bob uses ~/.bob on every
// OS, not %APPDATA% / ~/Library.
//
// Source (verified 2026-06-17):
//   - https://bob.ibm.com/docs/ide/configuration/mcp/mcp-in-bob — global file
//     `~/.bob/mcp_settings.json` ("applies across all workspaces"), project file
//     `.bob/mcp.json`, top-level `mcpServers` key, remote entry
//     `{"url":..., "headers":..., "alwaysAllow":..., "disabled":...}` (transport
//     inferred from `url`, no `type` field shown).
func NewBob() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultBobConfigPath(home),
		clientName: "bob",
		// Bob's remote endpoint key is the standard "url". The base
		// AddEntry already writes `{"url":..., "disabled":false}` (+ optional
		// headers), which is exactly Bob's documented remote shape — no
		// override required; every promoted helper keys off urlField "url".
		urlField: "url",
	}
	return newLockingClient(&bobClient{jsonMCPClient: base}), nil
}

// defaultBobConfigPath returns the default IBM Bob global MCP settings path,
// ~/.bob/mcp_settings.json. The path is OS-independent (Bob uses ~/.bob on
// every OS), mirroring the openclaw/hermes home-relative posture.
func defaultBobConfigPath(home string) string {
	return filepath.Join(home, ".bob", "mcp_settings.json")
}

// bobClient overrides only the filesystem-bootstrap methods (Exists, Backup,
// BackupKeep) so a fresh host with no ~/.bob directory still installs cleanly.
// AddEntry, GetEntry, RemoveEntry, Restore, InitEmpty, and every
// backup/demigrate helper are promoted from the embedded jsonMCPClient
// unchanged — Bob's documented remote shape is the base hub shape and its
// endpoint key is the standard `url`.
type bobClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.bob) does — mirroring the cursor/kiro/copilot-cli
// "directory means installed" heuristic so an operator who has Bob installed
// but no MCP config yet still gets the Initialize / install affordance.
func (b *bobClient) Exists() bool {
	if _, err := os.Stat(b.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(b.path))
	return err == nil && st.IsDir()
}

func (b *bobClient) Backup() (string, error) {
	return b.BackupKeep(0)
}

// BackupKeep ensures the ~/.bob parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host. Mirrors the
// cursor/kiro/copilot-cli BackupKeep wrappers.
func (b *bobClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(b.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := b.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(b.path, b.Name(), keepN)
}

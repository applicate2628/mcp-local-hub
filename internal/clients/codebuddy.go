package clients

import (
	"os"
	"path/filepath"
)

// NewCodeBuddy returns a Client bound to CodeBuddy Code's global MCP config:
//
//	~/.codebuddy/mcp.json
//
// CodeBuddy (Tencent Cloud Code Assistant; CLI `codebuddy`) reads MCP servers
// from a JSON/JSONC file under the top-level `mcpServers` key — the canonical
// JSON family schema shared by Claude Code / Cursor / Gemini CLI. Each entry is
// discriminated by a `type` field ∈ {"stdio","sse","http"}. CodeBuddy speaks
// HTTP MCP natively, so this adapter is HTTP-direct (NOT relay-stdio): a hub
// daemon's loopback URL is written as an http entry. The documented remote
// (http) entry shape is:
//
//	"<server-name>": {
//	  "type": "http",
//	  "url": "https://api.example.com/mcp",
//	  "headers": { "Authorization": "Bearer ***" }
//	}
//
// (stdio servers use `command`/`args`/`env` instead; the hub never writes that
// shape because the daemon is already an HTTP endpoint.) An optional `headers`
// object is emitted when MCPEntry.Headers is non-empty. The `type:"http"`
// discriminator is REQUIRED — CodeBuddy distinguishes a remote HTTP server from
// a local stdio one by this field — so AddEntry is overridden to emit it (the
// base jsonMCPClient.AddEntry would omit `type` and add a `disabled:false` flag
// CodeBuddy's documented http shape does not carry).
//
// Scope note: CodeBuddy resolves config across scopes — user/global
// `~/.codebuddy/mcp.json` (also `~/.codebuddy/.mcp.json`), project
// `./.codebuddy/...`. CODEBUDDY_CONFIG_DIR overrides the `~/.codebuddy`
// directory and `codebuddy --mcp-config <file>` overrides per-invocation; both
// are operator concerns the adapter defers the same way the other env-aware
// adapters do. The hub writes the canonical GLOBAL `~/.codebuddy/mcp.json`.
//
// Because the endpoint field is the standard `url` key and the hub entry has
// no `command`, the base jsonMCPClient hub-shape detection (isHubURLShapeEntry
// keyed on urlField "url") recognizes a hub CodeBuddy entry for
// demigrate/rollback unchanged — so the restore/predicate methods are inherited
// verbatim (same posture as the copilot-cli adapter).
//
// Source (verified 2026-06-17):
//   - https://www.codebuddy.ai/docs/cli/mcp — config dir `~/.codebuddy/`, MCP
//     file `mcp.json` (also `.mcp.json`), top-level `mcpServers` key, per-server
//     `type` ∈ {stdio,sse,http}, http entry `{"type":"http","url":...,
//     "headers":...}` with `${VAR}`/`${VAR:-default}` env substitution.
//     (codebuddy.ai was DNS-unreachable at fetch time; the documented http
//     shape + global path were confirmed via the cached search index of the
//     same vendor doc page and corroborated by the tier-1 vendor research note.)
func NewCodeBuddy() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultCodeBuddyConfigPath(home),
		clientName: "codebuddy",
		// CodeBuddy's remote endpoint key is the standard "url". The base
		// GetEntry / demigrate helpers key off this field; AddEntry is
		// overridden below to add the `type:"http"` discriminator.
		urlField: "url",
	}
	return newLockingClient(&codeBuddyClient{jsonMCPClient: base}), nil
}

// defaultCodeBuddyConfigPath returns the default CodeBuddy global MCP config
// path, ~/.codebuddy/mcp.json. The path is OS-independent (CodeBuddy uses
// ~/.codebuddy on every OS), mirroring the copilot-cli/openclaw home-relative
// posture. CODEBUDDY_CONFIG_DIR overrides the ~/.codebuddy directory in
// CodeBuddy itself; non-default overrides remain an operator concern.
func defaultCodeBuddyConfigPath(home string) string {
	return filepath.Join(home, ".codebuddy", "mcp.json")
}

// codeBuddyClient overrides only AddEntry (to emit the `type:"http"`
// discriminator) and the filesystem-bootstrap methods (Exists, Backup,
// BackupKeep) so a fresh host with no ~/.codebuddy directory still installs
// cleanly. GetEntry, RemoveEntry, Restore, InitEmpty, and every
// backup/demigrate helper are promoted from the embedded jsonMCPClient
// unchanged — the hub entry's loopback `url` + absent `command` makes the base
// hub-shape detection correct without override (same posture as the copilot-cli
// adapter).
type codeBuddyClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.codebuddy) does — mirroring the cursor/kiro/copilot-cli
// "directory means installed" heuristic so an operator who has CodeBuddy
// installed but no MCP config yet still gets the Initialize / install affordance.
func (c *codeBuddyClient) Exists() bool {
	if _, err := os.Stat(c.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(c.path))
	return err == nil && st.IsDir()
}

func (c *codeBuddyClient) Backup() (string, error) {
	return c.BackupKeep(0)
}

// BackupKeep ensures the ~/.codebuddy parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing. Mirrors the
// cursor/kiro/copilot-cli BackupKeep wrappers.
func (c *codeBuddyClient) BackupKeep(keepN int) (string, error) {
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
// CodeBuddy's remote entry shape is `{"type":"http","url":...}`; an optional
// `headers` object is emitted when MCPEntry.Headers is non-empty. The base
// jsonMCPClient.AddEntry would omit the required `type` discriminator (and add
// a `disabled:false` flag CodeBuddy's documented http shape does not carry), so
// this override is required.
func (c *codeBuddyClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam: mcp.json may be hand-edited
	// JSONC, so patch mcpServers.<name> into the original bytes instead of a
	// lossy full-map re-marshal.
	return c.setMember(entry.Name, serverEntry)
}

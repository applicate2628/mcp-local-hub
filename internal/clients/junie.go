package clients

import (
	"os"
	"path/filepath"
)

// NewJunie returns a Client bound to JetBrains Junie's user-scope (global)
// MCP config at ~/.junie/mcp/mcp.json.
//
// Junie (the JetBrains AI coding agent) reads MCP servers from a JSON file
// under the top-level `mcpServers` key — the canonical JSON family schema. A
// remote server is distinguished from a stdio one purely by the presence of a
// `url` key (stdio entries carry `command`/`args`/`env` instead); there is NO
// `type` discriminator. Junie speaks HTTP/HTTPS MCP natively, so the hub
// writes an HTTP-direct entry (no relay shim needed).
//
// The hub-managed remote entry shape written is therefore:
//
//	"<server-name>": {
//	  "url": "http://localhost:9121/mcp"
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty. The
// docs show neither a `type` nor a `disabled` field for remote entries, so
// AddEntry emits the minimal `{url}` (+ headers) shape rather than the base
// jsonMCPClient.AddEntry's `{url, disabled:false}`.
//
// Because the endpoint field is the standard `url` key, the embedded
// jsonMCPClient's hub-shape detection (isHubURLShapeEntry(_, "url")) and the
// inherited GetEntry / RemoveEntry / RestoreEntryFromBackup /
// BackupEntryIsHubManaged helpers all work unchanged with urlField "url" —
// only AddEntry (the minimal docs shape) and the seed/Exists overrides need
// dedicated implementations.
//
// Scope note: the hub writes the GLOBAL (user-scope) ~/.junie/mcp/mcp.json.
// Junie also reads a project-scope <project>/.junie/mcp/mcp.json which the hub
// does NOT touch.
//
// Source (verified 2026-06-17):
//   - https://junie.jetbrains.com/docs/junie-cli-mcp-configuration.html —
//     user-scope config at ~/.junie/mcp/mcp.json; top-level `mcpServers` key;
//     remote entry `{"url": "https://...", "headers": {...}}` with no `type`
//     field; accepts stdio (command/args/env) and remote HTTP/HTTPS url
//     entries.
func NewJunie() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".junie", "mcp", "mcp.json"),
		clientName: "junie",
		// Standard `url` endpoint key: the embedded base helpers (GetEntry,
		// hub-shape detection, restore guards) all key off this and need no
		// override.
		urlField: "url",
	}
	return newLockingClient(&junieClient{jsonMCPClient: base}), nil
}

// junieClient overrides AddEntry (to emit the minimal documented `{url}` shape
// without the base's `disabled:false`) and the filesystem-bootstrap methods
// (Exists, Backup, BackupKeep) so a fresh host with no ~/.junie/mcp directory
// still installs cleanly. GetEntry, RemoveEntry, Restore, InitEmpty, and every
// backup/demigrate helper are promoted from the embedded jsonMCPClient
// unchanged.
type junieClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.junie/mcp) does — mirroring the cursor/kiro
// "directory means installed" heuristic so an operator who has Junie installed
// but no MCP config yet still gets the Initialize / install affordance.
func (j *junieClient) Exists() bool {
	if _, err := os.Stat(j.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(j.path))
	return err == nil && st.IsDir()
}

func (j *junieClient) Backup() (string, error) {
	return j.BackupKeep(0)
}

// BackupKeep ensures the nested ~/.junie/mcp parent directory exists, seeds an
// empty `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The two-level-deep parent dir does
// not exist on a clean install, so the MkdirAll here is load-bearing —
// without it writeBackup/InitEmpty would fail on a fresh host.
func (j *junieClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(j.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := j.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(j.path, j.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map with
// Junie's minimal documented shape: just `url` (+ optional `headers`). The
// base jsonMCPClient.AddEntry would add a `disabled:false` field the Junie
// docs never show, so this override is required.
func (j *junieClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"url": entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return j.setMember(entry.Name, serverEntry)
}

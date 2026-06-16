package clients

import (
	"os"
	"path/filepath"
)

// NewWarp returns a Client bound to Warp (the agentic terminal)'s user-level
// (global) MCP config at ~/.warp/.mcp.json.
//
// Warp reads file-based MCP servers from a JSON file under the canonical
// top-level `mcpServers` key — the standard object-map family schema (each
// server is keyed by a unique name, NOT an array of objects). Each entry has
// exactly one transport type:
//
//   - URL-based (HTTP/SSE): `{"type":"http","url":...,"headers":{...}}` — the
//     `url` is a streamable-HTTP or SSE endpoint; `headers` is only valid
//     alongside `url`.
//   - CLI/stdio: `{"command":...,"args":[...],"env":{...}}`.
//
// Warp speaks HTTP MCP natively (streamable HTTP + SSE), so the hub writes an
// HTTP-direct entry (no relay shim needed): a hub daemon's loopback URL is
// written as an http entry. IsRelayStdio() therefore returns false (inherited
// from jsonMCPClient).
//
// The hub-managed remote entry shape written is therefore:
//
//	"<server-name>": {
//	  "type": "http",
//	  "url": "http://localhost:9121/mcp",
//	  "disabled": false
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty.
//
// Because the endpoint field is the standard `url` key, the embedded
// jsonMCPClient's hub-shape detection (isHubURLShapeEntry(_, "url")) and the
// inherited GetEntry / RemoveEntry / RestoreEntryFromBackup /
// BackupEntryIsHubManaged helpers all work unchanged with urlField "url" —
// only AddEntry (the `type:"http"` discriminator) and the seed/Exists
// overrides need dedicated implementations. Mirrors the Droid adapter (same
// object-map `mcpServers`, same `type:"http"` + `url` remote shape).
//
// Path note: the home-relative user file ~/.warp/.mcp.json (note the leading
// dot on the filename) is Warp's cross-platform location — the same file on
// Windows, macOS, and Linux, and the same for both Stable and Preview builds.
// Warp detects and (with an approval gate) spawns the file-based servers
// defined there; it also surfaces the same config through its in-app UI's
// "+ Add" JSON-paste affordance.
//
// Sources (verified 2026-06-17):
//   - https://docs.warp.dev/terminal/settings/file-locations/ — MCP server
//     config is stored at ~/.warp/.mcp.json; cross-platform (always in the
//     home directory), shared by Stable and Preview.
//   - https://docs.warp.dev/agent-platform/capabilities/mcp/ — top-level
//     `mcpServers` object map; a URL-based entry uses `type:"http"` (or `sse`)
//     + `url` + optional `headers` (headers only valid with `url`); a server
//     config must have exactly one transport type; servers are added by
//     pasting a JSON snippet into the "+ Add" UI (same on-disk shape).
func NewWarp() (Client, error) {
	base := &jsonMCPClient{
		path:       defaultWarpConfigPath(),
		clientName: "warp",
		// Standard `url` endpoint key: the embedded base helpers (GetEntry,
		// hub-shape detection, restore guards) all key off this. AddEntry is
		// overridden below only to add the `type:"http"` discriminator.
		urlField: "url",
	}
	return newLockingClient(&warpClient{jsonMCPClient: base}), nil
}

// defaultWarpConfigPath returns ~/.warp/.mcp.json. A home-dir lookup failure
// degrades to a bare ".mcp.json" relative path rather than panicking (matching
// the fail-safe posture AllClients uses to drop unconstructable adapters —
// Exists() on such a path simply reports absent).
func defaultWarpConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".mcp.json"
	}
	return filepath.Join(home, ".warp", ".mcp.json")
}

// warpClient overrides AddEntry (to emit the `type:"http"` discriminator) and
// the filesystem-bootstrap methods (Exists, Backup, BackupKeep) so a fresh
// host with no ~/.warp directory still installs cleanly. GetEntry,
// RemoveEntry, Restore, InitEmpty, and every backup/demigrate helper are
// promoted from the embedded jsonMCPClient unchanged.
type warpClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.warp) does — mirroring the cursor/droid "directory
// means installed" heuristic so an operator who has Warp installed but no MCP
// config yet still gets the Initialize / install affordance.
func (w *warpClient) Exists() bool {
	if _, err := os.Stat(w.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(w.path))
	return err == nil && st.IsDir()
}

func (w *warpClient) Backup() (string, error) {
	return w.BackupKeep(0)
}

// BackupKeep ensures the ~/.warp parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host. Mirrors the droid
// BackupKeep.
func (w *warpClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(w.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := w.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(w.path, w.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map with
// Warp's HTTP shape: `type:"http"` + `url` + `disabled:false`, plus an optional
// `headers` object. The base jsonMCPClient.AddEntry would omit the `type`
// discriminator, so this override is required.
func (w *warpClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type":     "http",
		"url":      entry.URL,
		"disabled": false,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return w.setMember(entry.Name, serverEntry)
}

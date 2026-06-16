package clients

import (
	"os"
	"path/filepath"
)

// NewDroid returns a Client bound to Factory.ai Droid CLI's user-level
// (global) MCP config at ~/.factory/mcp.json.
//
// Droid (the Factory.ai CLI agent) reads MCP servers from a JSON file under
// the top-level `mcpServers` key — the canonical JSON family schema. Each
// entry is discriminated by a `type` field: a remote server uses
// `type:"http"` (or `sse`) + `url`; a stdio server uses
// `command`/`args`/`env`. Droid speaks HTTP MCP natively, so the hub writes an
// HTTP-direct entry (no relay shim needed).
//
// The hub-managed remote entry shape written is therefore:
//
//	"<server-name>": {
//	  "type": "http",
//	  "url": "http://localhost:9121/mcp",
//	  "disabled": false
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty. The
// docs show `disabled` on the http entry shape, so it is emitted explicitly.
//
// Because the endpoint field is the standard `url` key, the embedded
// jsonMCPClient's hub-shape detection (isHubURLShapeEntry(_, "url")) and the
// inherited GetEntry / RemoveEntry / RestoreEntryFromBackup /
// BackupEntryIsHubManaged helpers all work unchanged with urlField "url" —
// only AddEntry (the `type:"http"` discriminator) and the seed/Exists
// overrides need dedicated implementations.
//
// Scope note: the hub writes the GLOBAL (user-level) ~/.factory/mcp.json.
// Droid also reads folder-level and project-level <project>/.factory/mcp.json
// which the hub does NOT touch.
//
// Source (verified 2026-06-17):
//   - https://docs.factory.ai/cli/configuration/mcp — user-level config at
//     ~/.factory/mcp.json ("User | ~/.factory/mcp.json | Your personal
//     servers, available in all projects"); top-level `mcpServers` key; HTTP
//     entry `{"type":"http","url":...,"headers":{...},"disabled":false,
//     "enabledTools":[...],"disabledTools":[...],"timeoutMs":...}`; URL key is
//     `url` (not `httpUrl`), type is `http` (not `streamable-http`).
func NewDroid() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".factory", "mcp.json"),
		clientName: "droid",
		// Standard `url` endpoint key: the embedded base helpers (GetEntry,
		// hub-shape detection, restore guards) all key off this. AddEntry is
		// overridden below only to add the `type:"http"` discriminator.
		urlField: "url",
	}
	return newLockingClient(&droidClient{jsonMCPClient: base}), nil
}

// droidClient overrides AddEntry (to emit the `type:"http"` discriminator) and
// the filesystem-bootstrap methods (Exists, Backup, BackupKeep) so a fresh
// host with no ~/.factory directory still installs cleanly. GetEntry,
// RemoveEntry, Restore, InitEmpty, and every backup/demigrate helper are
// promoted from the embedded jsonMCPClient unchanged.
type droidClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.factory) does — mirroring the cursor/kiro
// "directory means installed" heuristic so an operator who has Droid installed
// but no MCP config yet still gets the Initialize / install affordance.
func (d *droidClient) Exists() bool {
	if _, err := os.Stat(d.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(d.path))
	return err == nil && st.IsDir()
}

func (d *droidClient) Backup() (string, error) {
	return d.BackupKeep(0)
}

// BackupKeep ensures the ~/.factory parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host.
func (d *droidClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(d.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := d.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(d.path, d.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map with
// Droid's HTTP shape: `type:"http"` + `url` + `disabled:false`, plus an
// optional `headers` object. The base jsonMCPClient.AddEntry would omit the
// `type` discriminator, so this override is required.
func (d *droidClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type":     "http",
		"url":      entry.URL,
		"disabled": false,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return d.setMember(entry.Name, serverEntry)
}

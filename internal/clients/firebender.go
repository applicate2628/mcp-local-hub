package clients

import (
	"os"
	"path/filepath"
)

// NewFirebender returns a Client bound to Firebender's user-level (global)
// MCP config at ~/.firebender/firebender.json.
//
// Firebender (the JetBrains IDE coding-agent plugin) reads MCP servers from a
// JSON file under the top-level `mcpServers` key — the canonical JSON family
// schema. A remote server is distinguished from a stdio one purely by the
// presence of a `url` key (stdio entries carry `command`/`args`/`env`
// instead); there is NO `type` discriminator. Firebender speaks HTTP MCP
// natively (Streamable HTTP / SSE inferred from the `url`), so the hub writes
// an HTTP-direct entry (no relay shim needed).
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
// Scope note: the hub writes the GLOBAL (user-level) ~/.firebender/firebender.json.
// Firebender also reads a project-level <project>/firebender.json which the hub
// does NOT touch.
//
// Source (verified 2026-06-17):
//   - https://docs.firebender.com/context/mcp/overview — global config at
//     ~/.firebender/firebender.json ("Create ~/.firebender/firebender.json in
//     your home directory for tools available everywhere"); top-level
//     `mcpServers` key; remote entry `{"url": ..., "headers": {...}}` with no
//     `type` field; references Streamable HTTP as the remote transport.
func NewFirebender() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".firebender", "firebender.json"),
		clientName: "firebender",
		// Standard `url` endpoint key: the embedded base helpers (GetEntry,
		// hub-shape detection, restore guards) all key off this and need no
		// override.
		urlField: "url",
	}
	return newLockingClient(&firebenderClient{jsonMCPClient: base}), nil
}

// firebenderClient overrides AddEntry (to emit the minimal documented `{url}`
// shape without the base's `disabled:false`) and the filesystem-bootstrap
// methods (Exists, Backup, BackupKeep) so a fresh host with no ~/.firebender
// directory still installs cleanly. GetEntry, RemoveEntry, Restore, InitEmpty,
// and every backup/demigrate helper are promoted from the embedded
// jsonMCPClient unchanged.
type firebenderClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.firebender) does — mirroring the cursor/kiro
// "directory means installed" heuristic so an operator who has Firebender
// installed but no MCP config yet still gets the Initialize / install
// affordance.
func (f *firebenderClient) Exists() bool {
	if _, err := os.Stat(f.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(f.path))
	return err == nil && st.IsDir()
}

func (f *firebenderClient) Backup() (string, error) {
	return f.BackupKeep(0)
}

// BackupKeep ensures the ~/.firebender parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host.
func (f *firebenderClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(f.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := f.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(f.path, f.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map with
// Firebender's minimal documented shape: just `url` (+ optional `headers`).
// The base jsonMCPClient.AddEntry would add a `disabled:false` field the
// Firebender docs never show, so this override is required.
func (f *firebenderClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"url": entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return f.setMember(entry.Name, serverEntry)
}

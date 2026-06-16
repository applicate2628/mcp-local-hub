package clients

import (
	"os"
	"path/filepath"
)

// NewIFlowCLI returns a Client bound to iFlow CLI's user-level (global) MCP
// config at ~/.iflow/settings.json.
//
// iFlow CLI (心流; the terminal AI coding CLI by iFlow) is a Gemini-CLI-style
// agent: it reads MCP servers from a settings.json file under the top-level
// `mcpServers` key — the canonical JSON family schema. HTTP servers are added
// via `iflow mcp add --transport http <name> <url>`. Matching its Gemini-CLI
// lineage (see the gemini-cli adapter), an HTTP server entry carries a `url`
// plus an explicit `type: "http"` discriminator. iFlow speaks HTTP MCP
// natively, so the hub writes an HTTP-direct entry (no relay shim needed).
//
// The hub-managed remote entry shape written is therefore:
//
//	"<server-name>": {
//	  "url": "http://localhost:9121/mcp",
//	  "type": "http"
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty.
//
// Because the endpoint field is the standard `url` key, the embedded
// jsonMCPClient's hub-shape detection (isHubURLShapeEntry(_, "url")) and the
// inherited GetEntry / RemoveEntry / RestoreEntryFromBackup /
// BackupEntryIsHubManaged helpers all work unchanged with urlField "url" —
// only AddEntry (the `type:"http"` discriminator) and the seed/Exists
// overrides need dedicated implementations.
//
// Scope note: the hub writes the GLOBAL (user-level) ~/.iflow/settings.json.
// iFlow also reads a project-level <project>/.iflow/settings.json which the
// hub does NOT touch.
//
// Source (verified 2026-06-17):
//   - https://platform.iflow.cn/en/cli/examples/mcp — user/global config at
//     ~/.iflow/settings.json; top-level `mcpServers` key; stdio
//     (command/args/env/cwd/timeout/...) and HTTP via
//     `iflow mcp add --transport http <name> <url>`.
//   - The HTTP entry shape (`url` + `type:"http"`) mirrors iFlow's Gemini-CLI
//     lineage; see the gemini-cli adapter in this package for the verified
//     `gemini mcp add --transport http` round-trip shape.
func NewIFlowCLI() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".iflow", "settings.json"),
		clientName: "iflow-cli",
		// Standard `url` endpoint key: the embedded base helpers (GetEntry,
		// hub-shape detection, restore guards) all key off this. AddEntry is
		// overridden below only to add the `type:"http"` discriminator.
		urlField: "url",
	}
	return newLockingClient(&iflowCLIClient{jsonMCPClient: base}), nil
}

// iflowCLIClient overrides AddEntry (to emit the `type:"http"` discriminator)
// and the filesystem-bootstrap methods (Exists, Backup, BackupKeep) so a fresh
// host with no ~/.iflow directory still installs cleanly. GetEntry,
// RemoveEntry, Restore, InitEmpty, and every backup/demigrate helper are
// promoted from the embedded jsonMCPClient unchanged.
type iflowCLIClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.iflow) does — mirroring the cursor/kiro "directory
// means installed" heuristic so an operator who has iFlow CLI installed but no
// MCP config yet still gets the Initialize / install affordance.
func (i *iflowCLIClient) Exists() bool {
	if _, err := os.Stat(i.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(i.path))
	return err == nil && st.IsDir()
}

func (i *iflowCLIClient) Backup() (string, error) {
	return i.BackupKeep(0)
}

// BackupKeep ensures the ~/.iflow parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host.
//
// Note: iFlow reads many other settings from settings.json, but because
// InitEmpty fires only when the file is missing, no user-authored
// configuration can be clobbered by the seed.
func (i *iflowCLIClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(i.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := i.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(i.path, i.Name(), keepN)
}

// AddEntry writes the hub-managed HTTP entry under the `mcpServers` map with
// iFlow's Gemini-CLI-style shape: `url` + `type:"http"` (+ optional `headers`).
// The base jsonMCPClient.AddEntry would omit the `type` discriminator, so this
// override is required.
func (i *iflowCLIClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"url":  entry.URL,
		"type": "http",
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return i.setMember(entry.Name, serverEntry)
}

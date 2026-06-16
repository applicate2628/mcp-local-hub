package clients

import (
	"os"
	"path/filepath"
	"runtime"
)

// NewRoo returns a Client bound to the Roo Code VS Code extension's GLOBAL
// MCP settings file:
//
//	Windows: %APPDATA%\Code\User\globalStorage\rooveterinaryinc.roo-cline\settings\cline_mcp_settings.json
//	macOS:   ~/Library/Application Support/Code/User/globalStorage/rooveterinaryinc.roo-cline/settings/cline_mcp_settings.json
//	Linux:   ~/.config/Code/User/globalStorage/rooveterinaryinc.roo-cline/settings/cline_mcp_settings.json
//	         (or $XDG_CONFIG_HOME/Code/User/... when XDG_CONFIG_HOME is set)
//
// Roo Code (the actively-shipping VS Code AI agent extension, formerly Roo
// Cline; publisher id rooveterinaryinc.roo-cline) reads MCP servers from a
// JSON file under the top-level `mcpServers` key — the canonical JSON family
// schema. Roo distinguishes a remote HTTP server from a local stdio one by
// the `type` value: a remote entry uses `type:"streamable-http"` (modern) or
// `type:"sse"` (legacy) + `url`; a local entry uses `command`/`args`. Roo
// speaks HTTP MCP natively via Streamable HTTP, so the hub writes an
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
// Because the hub entry carries a loopback `url` and NO `command`, the base
// jsonMCPClient hub-shape detection (isHubURLShapeEntry keyed on urlField
// "url") recognizes a hub Roo entry for demigrate/rollback unchanged — so
// the restore/predicate methods are inherited verbatim. This mirrors the
// Kilo Code adapter (both are Roo/Cline-family Streamable HTTP clients
// living under VS Code globalStorage); the only differences are the
// publisher dir (rooveterinaryinc.roo-cline) and the file name
// (cline_mcp_settings.json — the global config Roo Code surfaces as "Edit
// Global MCP").
//
// Scope note: the hub writes the GLOBAL (user-level) extension-managed
// settings file ("Edit Global MCP" in the Roo UI), the same user-scoped
// posture as every other adapter. Roo also supports a project-scoped
// `.roo/mcp.json` (commit-to-VCS) which the hub does NOT touch.
//
// Sources (verified 2026-06-17):
//   - https://docs.roocode.com/features/mcp/using-mcp-in-roo (→
//     roocodeinc.github.io/Roo-Code/...) — top-level `mcpServers` key;
//     remote/streamable-http entry shape
//     `{"type":"streamable-http","url":...,"headers":{...},"disabled":false,
//     "alwaysAllow":[...]}`; legacy `type:"sse"`; project-scope
//     `.roo/mcp.json`; global config is the extension-managed
//     cline_mcp_settings.json ("Edit Global MCP").
//   - Roo Code publisher id rooveterinaryinc.roo-cline + VS Code
//     globalStorage layout mirror the Cline / Kilo Code adapter siblings.
func NewRoo() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultRooConfigPath(home),
		clientName: "roo",
		// Roo's remote-server endpoint key is "url"; the base helpers
		// (GetEntry fallback, hub-shape detection, restore guards) key off
		// urlField. AddEntry is overridden below only to add the
		// `type:"streamable-http"` discriminator.
		urlField: "url",
	}
	return newLockingClient(&rooClient{jsonMCPClient: base}), nil
}

// rooMCPType is Roo Code's remote-HTTP transport discriminator. Single owner
// of the "streamable-http" literal so the writer stays in sync with the
// documented shape.
const rooMCPType = "streamable-http"

// defaultRooConfigPath builds the per-OS absolute path to Roo Code's global
// MCP settings file under the VS Code user globalStorage root. The root
// computation mirrors defaultVSCodeConfigPath / defaultKiloCodeConfigPath
// (APPDATA on Windows, Application Support on macOS, XDG_CONFIG_HOME/.config
// on Linux); the extension-specific tail is
// globalStorage/rooveterinaryinc.roo-cline/settings/cline_mcp_settings.json.
func defaultRooConfigPath(home string) string {
	const (
		extDir   = "rooveterinaryinc.roo-cline"
		fileName = "cline_mcp_settings.json"
	)
	var userDir string
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			userDir = filepath.Join(appData, "Code", "User")
		} else {
			userDir = filepath.Join(home, "AppData", "Roaming", "Code", "User")
		}
	case "darwin":
		userDir = filepath.Join(home, "Library", "Application Support", "Code", "User")
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			userDir = filepath.Join(xdg, "Code", "User")
		} else {
			userDir = filepath.Join(home, ".config", "Code", "User")
		}
	}
	return filepath.Join(userDir, "globalStorage", extDir, "settings", fileName)
}

// rooClient overrides only AddEntry (to emit the `type:"streamable-http"`
// discriminator) and the filesystem-bootstrap methods (Exists, Backup,
// BackupKeep, InitEmpty) so a fresh host with no
// globalStorage/rooveterinaryinc.roo-cline/settings directory still installs
// cleanly. GetEntry, RemoveEntry, Restore, and every backup/demigrate helper
// are promoted from the embedded jsonMCPClient unchanged — the hub entry's
// loopback `url` + absent `command` makes the base hub-shape detection
// correct without override (same posture as the Kilo Code adapter).
type rooClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists
// OR its parent directory (…/settings) does — mirroring the kilocode/kiro
// "directory means installed" heuristic so an operator who has Roo Code
// installed but no MCP config yet still gets the Initialize / install
// affordance.
func (r *rooClient) Exists() bool {
	if _, err := os.Stat(r.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(r.path))
	return err == nil && st.IsDir()
}

func (r *rooClient) Backup() (string, error) {
	return r.BackupKeep(0)
}

// BackupKeep ensures the deeply-nested
// …/globalStorage/rooveterinaryinc.roo-cline/settings parent directory
// exists, seeds an empty `{"mcpServers": {}}` stub if the config is absent,
// then writes the timestamped backup (pruning to keepN). The parent dir is
// several levels deep and does not exist on a clean install, so the MkdirAll
// here is load-bearing. Mirrors kiloCodeClient.BackupKeep.
func (r *rooClient) BackupKeep(keepN int) (string, error) {
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

// InitEmpty seeds the Roo Code global cline_mcp_settings.json with
// `{"mcpServers": {}}` if the file is absent. Roo shares the canonical JSON
// family schema; AddEntry's later merge writes into the same `mcpServers`
// map.
func (r *rooClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(r.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map
// with Roo Code's Streamable HTTP shape: `type:"streamable-http"` + `url` +
// `disabled:false`, plus an optional `headers` object. The base
// jsonMCPClient.AddEntry would omit the `type` discriminator, so this
// override is required.
func (r *rooClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type":     rooMCPType,
		"url":      entry.URL,
		"disabled": false,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return r.setMember(entry.Name, serverEntry)
}

package clients

import (
	"os"
	"path/filepath"
	"runtime"
)

// NewKiloCode returns a Client bound to the Kilo Code VS Code extension's
// global MCP settings file:
//
//	Windows: %APPDATA%\Code\User\globalStorage\kilo-code.kilo-code\settings\mcp_settings.json
//	macOS:   ~/Library/Application Support/Code/User/globalStorage/kilo-code.kilo-code/settings/mcp_settings.json
//	Linux:   ~/.config/Code/User/globalStorage/kilo-code.kilo-code/settings/mcp_settings.json
//	         (or $XDG_CONFIG_HOME/Code/User/... when XDG_CONFIG_HOME is set)
//
// Kilo Code (the agentic VS Code coding extension — a fork of Roo Code /
// Cline) reads MCP servers from that JSON file under the top-level
// `mcpServers` key — the canonical JSON family schema shared by Cursor /
// Gemini CLI / Antigravity. Kilo Code speaks HTTP MCP natively and
// supports the Streamable HTTP transport (per the MCP 2025-03-26 spec),
// so the hub writes an HTTP-direct entry (no relay shim needed).
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
// This mirrors the Cursor adapter exactly except the `type` value is
// "streamable-http" (Cursor writes "http"). Kilo distinguishes a remote
// HTTP server from a local stdio one by the `type` value: a remote entry
// uses `type:"streamable-http"` (or `sse`) + `url`; a local entry uses
// `command`/`args`. Because the hub entry carries `url` and NO `command`,
// the base jsonMCPClient hub-shape detection (isHubURLShapeEntry keyed on
// urlField "url") recognizes a hub Kilo entry for demigrate/rollback
// unchanged — so the restore/predicate methods are inherited verbatim,
// matching the Cursor adapter's posture.
//
// Scope note: the hub writes the GLOBAL (user-level) settings file, the
// same user-scoped posture as every other adapter. Kilo Code also supports
// a project-scoped `.kilocode/mcp.json` (auto-detected per repo) which the
// hub does NOT touch.
//
// Sources (verified 2026-06):
//   - Kilo Code docs, "Using MCP in Kilo Code"
//     (https://kilo.ai/docs/automate/mcp/using-in-kilo-code): top-level
//     `mcpServers` key; remote/streamable-http entry shape
//     `{"type": "streamable-http", "url": ..., "headers": ..., "disabled": ...,
//     "alwaysAllow": [...]}`; project-scope `.kilocode/mcp.json`.
//   - Kilo Code GitHub issue #6481 (v7.0.33 MCP settings migration):
//     global VS Code extension settings path
//     `%APPDATA%/Code/User/globalStorage/kilo-code.kilo-code/settings/mcp_settings.json`.
//   - VS Code globalStorage roots per OS (macOS
//     `~/Library/Application Support/Code/User/globalStorage`, Linux
//     `~/.config/Code/User/globalStorage`) mirror this adapter's
//     defaultVSCodeConfigPath sibling.
func NewKiloCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultKiloCodeConfigPath(home),
		clientName: "kilocode",
		// Kilo Code's remote-server endpoint key is "url". The base
		// helpers (GetEntry fallback, hub-shape detection, restore guards)
		// key off urlField; AddEntry is overridden below only to add the
		// `type:"streamable-http"` discriminator the base does not write.
		urlField: "url",
	}
	return newLockingClient(&kiloCodeClient{jsonMCPClient: base}), nil
}

// kiloCodeMCPType is Kilo Code's remote-HTTP transport discriminator.
// Single owner of the "streamable-http" literal so the writer stays in
// sync with the documented shape.
const kiloCodeMCPType = "streamable-http"

// defaultKiloCodeConfigPath builds the per-OS absolute path to Kilo Code's
// global MCP settings file under the VS Code user globalStorage root. The
// root computation mirrors defaultVSCodeConfigPath (APPDATA on Windows,
// Application Support on macOS, XDG_CONFIG_HOME/.config on Linux); the
// extension-specific tail is
// globalStorage/kilo-code.kilo-code/settings/mcp_settings.json.
func defaultKiloCodeConfigPath(home string) string {
	const (
		extDir   = "kilo-code.kilo-code"
		fileName = "mcp_settings.json"
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

// kiloCodeClient overrides only AddEntry (to emit the
// `type:"streamable-http"` discriminator) and the filesystem-bootstrap
// methods (Exists, Backup, BackupKeep, InitEmpty) so a fresh host with no
// globalStorage/kilo-code.kilo-code/settings directory still installs
// cleanly. GetEntry, RemoveEntry, Restore, and every backup/demigrate
// helper are promoted from the embedded jsonMCPClient unchanged — the hub
// entry's loopback `url` + absent `command` makes the base hub-shape
// detection correct without override (same posture as the Cursor adapter).
type kiloCodeClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists
// OR its parent directory (…/settings) does — mirroring the cursor/kiro
// "directory means installed" heuristic so an operator who has Kilo Code
// installed but no MCP config yet still gets the Initialize / install
// affordance.
func (k *kiloCodeClient) Exists() bool {
	if _, err := os.Stat(k.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(k.path))
	return err == nil && st.IsDir()
}

func (k *kiloCodeClient) Backup() (string, error) {
	return k.BackupKeep(0)
}

// BackupKeep ensures the deeply-nested
// …/globalStorage/kilo-code.kilo-code/settings parent directory exists,
// seeds an empty `{"mcpServers": {}}` stub if the config is absent, then
// writes the timestamped backup (pruning to keepN). The parent dir is
// several levels deep and does not exist on a clean install, so the
// MkdirAll here is load-bearing — without it writeBackup/InitEmpty would
// fail on a fresh host. Mirrors kiroClient.BackupKeep.
func (k *kiloCodeClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(k.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := k.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(k.path, k.Name(), keepN)
}

// InitEmpty seeds the Kilo Code global mcp_settings.json with
// `{"mcpServers": {}}` if the file is absent. Kilo Code shares the
// canonical JSON family schema; AddEntry's later merge writes into the
// same `mcpServers` map.
func (k *kiloCodeClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(k.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map
// with Kilo Code's Streamable HTTP shape: `type:"streamable-http"` + `url`
// + `disabled:false`, plus an optional `headers` object. The base
// jsonMCPClient.AddEntry would omit the `type` discriminator, so this
// override is required.
func (k *kiloCodeClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type":     kiloCodeMCPType,
		"url":      entry.URL,
		"disabled": false,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return k.setMember(entry.Name, serverEntry)
}

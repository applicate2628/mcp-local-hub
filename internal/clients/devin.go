package clients

import (
	"os"
	"path/filepath"
	"runtime"
)

// NewDevin returns a Client bound to the Devin CLI's user-level MCP config:
//
//	POSIX (Linux/macOS): ~/.config/devin/config.json
//	Windows:             %APPDATA%\devin\config.json
//
// The Devin CLI (Cognition Devin; docs.devin.ai/cli) has a real file-based
// config that reads MCP servers from a JSON file under the top-level
// `mcpServers` key — the canonical JSON family schema shared by Claude Code /
// Cursor / Gemini CLI. Devin CLI speaks HTTP MCP natively, so this adapter is
// HTTP-direct (NOT relay-stdio): a remote server's transport is inferred from
// the presence of a `url` key (http is the default for URL-based servers; a
// `transport` field is optional and may be set to "sse" for legacy fallback).
// The documented remote-entry shape is:
//
//	"<server-name>": {
//	  "url": "https://mcp.notion.com/mcp",
//	  "transport": "http"
//	}
//
// Because `transport` is OPTIONAL (inferred from `url`), the hub writes the
// minimal documented form `{"url":...}` (+ optional `headers`). AddEntry is
// overridden — not to add a discriminator, but to AVOID the base
// jsonMCPClient.AddEntry's `disabled:false` flag, which Devin's documented
// remote shape does not carry.
//
// Surface note — this adapter targets the Devin CLI's file config, NOT the
// Devin CLOUD product (which configures MCP through a web-UI Marketplace form,
// not a user file). DEVIN_MCP_CONFIG can relocate the file in Devin itself;
// non-default overrides remain an operator concern, the same way the other
// env-aware adapters defer them. The hub writes the canonical default user file.
//
// Because the endpoint field is the standard `url` key and the hub entry has
// no `command`, the base jsonMCPClient hub-shape detection (isHubURLShapeEntry
// keyed on urlField "url") recognizes a hub Devin entry for demigrate/rollback
// unchanged — so the restore/predicate methods are inherited verbatim (same
// posture as the copilot-cli adapter).
//
// Source (verified 2026-06-17):
//   - https://docs.devin.ai/cli/extensibility/mcp/configuration — user config
//     `~/.config/devin/config.json` (Windows `%APPDATA%\devin\config.json`),
//     top-level `mcpServers`, remote entry `{"url":..., "transport":"http"}`
//     with `transport` optional (http inferred from `url`; "sse" for legacy);
//     DEVIN_MCP_CONFIG relocates the file.
func NewDevin() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultDevinConfigPath(home),
		clientName: "devin",
		// Devin's remote endpoint key is the standard "url". The base
		// GetEntry / demigrate helpers key off this field; AddEntry is
		// overridden below only to drop the base's `disabled:false` flag.
		urlField: "url",
	}
	return newLockingClient(&devinClient{jsonMCPClient: base}), nil
}

// defaultDevinConfigPath returns the per-OS absolute path to the Devin CLI's
// user config file. Devin uses the XDG-style `~/.config/devin/config.json` on
// POSIX and `%APPDATA%\devin\config.json` on Windows. The XDG_CONFIG_HOME env
// var, when set, replaces the `~/.config` root on POSIX (matching how Devin's
// own config resolver and other XDG-aware tools locate it); on Windows the
// APPDATA root is used (falling back to `<home>\AppData\Roaming` when APPDATA
// is unset).
func defaultDevinConfigPath(home string) string {
	const (
		appDir   = "devin"
		fileName = "config.json"
	)
	if runtime.GOOS == "windows" {
		root := os.Getenv("APPDATA")
		if root == "" {
			root = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(root, appDir, fileName)
	}
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, appDir, fileName)
}

// devinClient overrides only AddEntry (to write the minimal `{"url":...}` shape
// WITHOUT the base's `disabled:false` flag) and the filesystem-bootstrap
// methods (Exists, Backup, BackupKeep) so a fresh host with no devin config
// directory still installs cleanly. GetEntry, RemoveEntry, Restore, InitEmpty,
// and every backup/demigrate helper are promoted from the embedded
// jsonMCPClient unchanged — the hub entry's loopback `url` + absent `command`
// makes the base hub-shape detection correct without override (same posture as
// the copilot-cli adapter).
type devinClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (…/devin) does — mirroring the cursor/kiro/copilot-cli
// "directory means installed" heuristic so an operator who has the Devin CLI
// installed but no MCP config yet still gets the Initialize / install affordance.
func (d *devinClient) Exists() bool {
	if _, err := os.Stat(d.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(d.path))
	return err == nil && st.IsDir()
}

func (d *devinClient) Backup() (string, error) {
	return d.BackupKeep(0)
}

// BackupKeep ensures the …/devin parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir may not exist on a
// clean install, so the MkdirAll here is load-bearing. Mirrors the
// cursor/kiro/copilot-cli BackupKeep wrappers.
func (d *devinClient) BackupKeep(keepN int) (string, error) {
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

// AddEntry writes the hub-managed remote entry under mcpServers.<name>. Devin's
// remote entry shape is the minimal `{"url":...}` (transport is inferred from
// `url`; http is the default). An optional `headers` object is emitted when
// MCPEntry.Headers is non-empty. The base jsonMCPClient.AddEntry would add a
// `disabled:false` flag the documented Devin shape does not carry, so this
// override writes the minimal documented shape instead.
func (d *devinClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"url": entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return d.setMember(entry.Name, serverEntry)
}

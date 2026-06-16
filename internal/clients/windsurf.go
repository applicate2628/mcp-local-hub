package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewWindsurf returns a Client bound to ~/.codeium/windsurf/mcp_config.json
// (Windows: %USERPROFILE%\.codeium\windsurf\mcp_config.json).
//
// Windsurf (the Codeium / Cognition editor, Cascade agent) reads MCP servers
// from a JSON file under the top-level `mcpServers` key — the canonical JSON
// family schema shared by Cursor / Gemini CLI / Antigravity. Windsurf supports
// stdio, Streamable HTTP, and SSE transports, so the hub writes an HTTP-direct
// entry (no relay shim needed).
//
// The one quirk that forces a dedicated adapter rather than reusing the bare
// jsonMCPClient: Windsurf names the remote-HTTP endpoint field `serverUrl`,
// NOT `url` (it is the only major MCP client to do so). The hub-managed entry
// shape written is therefore:
//
//	"<server-name>": {
//	  "serverUrl": "http://localhost:9121/mcp",
//	  "disabled": false
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty.
//
// Sources (verified 2026-06):
//   - https://docs.windsurf.com/windsurf/cascade/mcp (redirects to
//     https://docs.devin.ai/desktop/cascade/mcp): config path
//     ~/.codeium/windsurf/mcp_config.json, top-level key `mcpServers`,
//     remote-HTTP entry uses `serverUrl` (or `url`) + `headers`; transports
//     stdio / Streamable HTTP / SSE.
//   - Multiple install guides (GitHub github-mcp-server, Microsoft Learn
//     Azure MCP, mcp.run) confirm the same path + `serverUrl` field.
func NewWindsurf() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"),
		clientName: "windsurf",
		// urlField is set to windsurfURLField so the inherited base helpers
		// (GetEntry fallback, hub-shape detection) reference the same key the
		// overrides below write. The base backupEntryMapIsHubManaged routes
		// URL-shape detection only for urlField "url"/"httpUrl", so the
		// shape-sensitive restore/predicate methods are overridden below to
		// run serverUrl-aware detection.
		urlField: windsurfURLField,
	}
	return newLockingClient(&windsurfClient{jsonMCPClient: base}), nil
}

// windsurfURLField is Windsurf's remote-HTTP endpoint key. Single owner of the
// "serverUrl" literal so the writer, reader, and hub-shape guard stay in sync.
const windsurfURLField = "serverUrl"

// windsurfClient overrides AddEntry/GetEntry to use Windsurf's `serverUrl`
// HTTP schema, overrides the hub-shape-detecting restore/predicate methods so
// demigrate + rollback recognize a serverUrl hub entry, and overrides
// Exists/Backup/BackupKeep/InitEmpty so a fresh Windsurf install (the
// ~/.codeium/windsurf/ directory present but mcp_config.json not yet written)
// can be routed through the hub without the operator hand-creating the file —
// the same posture as the Cursor adapter. Restore, RemoveEntry, Name,
// ConfigPath, LatestBackupPath, BackupContainsEntry, AllStdioEntries, and
// FindStdioLanguageServerEntries are promoted from the embedded jsonMCPClient
// unchanged.
type windsurfClient struct {
	*jsonMCPClient
}

// Exists treats Windsurf as installed when EITHER the config file is present
// OR its parent directory (~/.codeium/windsurf/) exists. Windsurf is a GUI
// editor that creates the directory on install but writes mcp_config.json
// only when the user first configures an MCP server, so a dir-based probe
// lets the operator migrate from a fresh install. Mirrors cursorClient.Exists.
func (w *windsurfClient) Exists() bool {
	if _, err := os.Stat(w.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(w.path))
	return err == nil && st.IsDir()
}

// Backup delegates to BackupKeep(0) (no pruning), matching the embedded
// contract while routing through the MkdirAll+InitEmpty seed path.
func (w *windsurfClient) Backup() (string, error) {
	return w.BackupKeep(0)
}

// BackupKeep ensures the parent dir exists and seeds an empty `{"mcpServers":{}}`
// stub before backing up, so a migrate that runs against a fresh install (no
// mcp_config.json yet) does not fail with ErrClientNotInstalled. Mirrors
// cursorClient.BackupKeep.
func (w *windsurfClient) BackupKeep(keepN int) (string, error) {
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

// InitEmpty seeds ~/.codeium/windsurf/mcp_config.json with `{"mcpServers": {}}`
// if the file is absent. AddEntry's later merge writes into the same
// `mcpServers` map. Identical stub to the Cursor/JSON-family adapters.
func (w *windsurfClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(w.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

// AddEntry writes the hub-managed HTTP entry under the `serverUrl` key.
func (w *windsurfClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		windsurfURLField: entry.URL,
		"disabled":       false,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return w.setMember(entry.Name, serverEntry)
}

// GetEntry reads the endpoint from the `serverUrl` key (the base reader keys
// off urlField, which is windsurfURLField here, so this override is for
// clarity + symmetry with the writer; both reference windsurfURLField).
func (w *windsurfClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := w.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	url, _ := raw[windsurfURLField].(string)
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers")}, nil
}

// RestoreEntryFromBackup restores mcpServers[name] from the backup, refusing
// (ErrBackupEntryAlreadyMigrated) when the backup's copy is already in
// hub-managed serverUrl-HTTP shape. The base body keys the hub-shape guard off
// urlField via backupEntryMapIsHubManaged, which only routes URL-shape
// detection for "url"/"httpUrl"; this override runs serverUrl-aware detection
// so a backup of a Windsurf hub entry is correctly recognized.
func (w *windsurfClient) RestoreEntryFromBackup(backupPath, name string) error {
	return w.restoreEntryFromBackupServerURL(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the hub-managed guard (serena dynamic-pool migrate abort-rollback).
func (w *windsurfClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return w.restoreEntryFromBackupServerURL(backupPath, name, true)
}

// BackupEntryIsHubManaged reports whether mcpServers[name] in the backup is in
// Windsurf's hub-managed serverUrl-HTTP shape (hub loopback URL under
// `serverUrl` and no `command`). Overrides the base predicate so it keys off
// `serverUrl` instead of the base urlField routing.
func (w *windsurfClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	data, err := readRawConfig(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, windsurfURLField), nil
}

// restoreEntryFromBackupServerURL is the shared restore body for Windsurf. It
// mirrors jsonMCPClient.restoreEntryFromBackup but performs serverUrl-aware
// hub-shape detection via isHubURLShapeEntry(_, windsurfURLField). When
// allowHubEntry is false (demigrate) it refuses a backup entry already in
// hub-managed shape with ErrBackupEntryAlreadyMigrated; when true (migrate
// rollback) it writes the backup bytes verbatim regardless of shape.
func (w *windsurfClient) restoreEntryFromBackupServerURL(backupPath, name string, allowHubEntry bool) error {
	backupData, err := readRawConfig(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	backupMap, err := parseJSONCBytes(backupData)
	if err != nil {
		return fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	backupServers, _ := backupMap["mcpServers"].(map[string]any)
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubURLShapeEntry(rawMap, windsurfURLField) {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			// Comment-preserving set into the LIVE config.
			return w.setMember(name, backupEntry)
		}
	}
	return w.deleteMember(name)
}

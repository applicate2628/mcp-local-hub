package clients

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// NewHermes returns a Client bound to ~/.hermes/config.yaml.
//
// Hermes Agent (Nous Research, MIT-licensed) reads MCP servers from
// `~/.hermes/config.yaml` under the top-level `mcp_servers` key. It speaks
// HTTP MCP natively: an entry whose body carries a `url` key is treated as
// an HTTP/streamable-HTTP server (the transport is inferred from `url` — no
// explicit `type`/`transport` field is required or written). stdio entries
// carry `command`/`args`/`env` instead. mcp-local-hub writes the HTTP shape.
//
// Source (official docs, verified 2026-06):
//   - https://hermes-agent.nousresearch.com/docs/reference/mcp-config-reference
//   - https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp
//   - https://github.com/NousResearch/hermes-agent (use-mcp-with-hermes guide)
//
// Example HTTP server entry Hermes accepts:
//
//	mcp_servers:
//	  company_api:
//	    url: "https://mcp.internal.example.com"
//	    headers:
//	      Authorization: "Bearer ***"
//
// Hermes has no documented workspace/project-level MCP config file; the
// single user-level ~/.hermes/config.yaml is canonical.
func NewHermes() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&hermes{path: filepath.Join(home, ".hermes", "config.yaml")}), nil
}

type hermes struct {
	path string
}

func (h *hermes) Name() string       { return "hermes" }
func (h *hermes) ConfigPath() string { return h.path }

// IsRelayStdio reports false: hermes is a URL-native HTTP MCP client.
func (h *hermes) IsRelayStdio() bool { return false }

func (h *hermes) Exists() bool {
	_, err := os.Stat(h.path)
	return err == nil
}

func (h *hermes) Backup() (string, error) {
	return writeBackup(h.path, h.Name(), 0)
}

func (h *hermes) BackupKeep(keepN int) (string, error) {
	return writeBackup(h.path, h.Name(), keepN)
}

// InitEmpty seeds ~/.hermes/config.yaml with an empty `mcp_servers:` map
// if the file is absent. The key is declared (rather than dropping an
// empty file) so a user inspecting the stub sees exactly where AddEntry
// will append new servers. Hermes reads many other settings from the same
// config.yaml, but because InitEmpty fires only when the file is missing,
// no user-authored configuration can be clobbered.
func (h *hermes) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(h.path, []byte("mcp_servers: {}\n"))
}

func (h *hermes) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so
	// production restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(h.path, data)
}

// readYAML / writeYAML round-trip through map[string]any so unknown keys survive.
func (h *hermes) readYAML() (map[string]any, error) {
	data, err := os.ReadFile(h.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", h.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (h *hermes) writeYAML(m map[string]any) error {
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative + DACL-bound)
	// for token-bearing rewrites; tests get the os.WriteFile fallback.
	return WriteConfigFile(h.path, out)
}

func (h *hermes) AddEntry(entry MCPEntry) error {
	m, err := h.readYAML()
	if err != nil {
		return err
	}
	servers := asStringMap(m["mcp_servers"])
	if servers == nil {
		servers = map[string]any{}
	}
	// Replace the entry wholesale — this drops any stdio-era fields like
	// `command`/`args`/`env`. Transport is inferred from the `url` key;
	// Hermes needs no explicit type field.
	entryMap := map[string]any{
		"url": entry.URL,
	}
	if len(entry.Headers) > 0 {
		entryMap["headers"] = entry.Headers
	}
	servers[entry.Name] = entryMap
	m["mcp_servers"] = servers
	return h.writeYAML(m)
}

func (h *hermes) RemoveEntry(name string) error {
	m, err := h.readYAML()
	if err != nil {
		return err
	}
	servers := asStringMap(m["mcp_servers"])
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m["mcp_servers"] = servers
	return h.writeYAML(m)
}

func (h *hermes) GetEntry(name string) (*MCPEntry, error) {
	m, err := h.readYAML()
	if err != nil {
		return nil, err
	}
	servers := asStringMap(m["mcp_servers"])
	if servers == nil {
		return nil, nil
	}
	raw := asStringMap(servers[name])
	if raw == nil {
		return nil, nil
	}
	url, _ := raw["url"].(string)
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers")}, nil
}

// LatestBackupPath delegates to the shared helper.
func (h *hermes) LatestBackupPath() (string, bool, error) {
	return latestBackup(h.path, h.Name())
}

// RestoreEntryFromBackup reads the YAML backup, extracts the
// mcp_servers.<name> mapping (if present), and writes it over the live
// config's corresponding entry. Other mcp_servers.* entries are left
// untouched.
//
// Defensively refuses if the backup's copy of the named entry is
// already in hub-HTTP form (has a loopback `url` key and no `command`
// key) — see ErrBackupEntryAlreadyMigrated.
func (h *hermes) RestoreEntryFromBackup(backupPath, name string) error {
	return h.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface
// doc on Client.RestoreEntryFromBackupForRollback). Used only by the
// serena dynamic-pool migrate abort-rollback.
func (h *hermes) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return h.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a hub-HTTP-shaped backup entry with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes
// the backup bytes verbatim regardless of shape.
func (h *hermes) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	var backupMap map[string]any
	if len(backupData) > 0 {
		if err := yaml.Unmarshal(backupData, &backupMap); err != nil {
			return fmt.Errorf("parse backup %s: %w", backupPath, err)
		}
	}
	if backupMap == nil {
		backupMap = map[string]any{}
	}
	backupServers := asStringMap(backupMap["mcp_servers"])
	liveMap, err := h.readYAML()
	if err != nil {
		return err
	}
	liveServers := asStringMap(liveMap["mcp_servers"])
	if liveServers == nil {
		liveServers = map[string]any{}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive: refuse hub-HTTP-shaped backup entries for
			// Hermes (loopback `url` present, `command` absent).
			// User-configured remote HTTP entries (non-loopback url)
			// pass through. The rollback caller (allowHubEntry=true)
			// bypasses this guard to restore the pre-reconcile legacy
			// hub entry verbatim.
			if !allowHubEntry {
				if rawMap := asStringMap(backupEntry); rawMap != nil {
					if isHubURLShapeEntry(rawMap, "url") {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap["mcp_servers"] = liveServers
			return h.writeYAML(liveMap)
		}
	}
	delete(liveServers, name)
	liveMap["mcp_servers"] = liveServers
	return h.writeYAML(liveMap)
}

// AllStdioEntries returns every stdio entry from mcp_servers.*.
func (h *hermes) AllStdioEntries() ([]StdioEntry, error) {
	m, err := h.readYAML()
	if err != nil {
		return nil, err
	}
	servers := asStringMap(m["mcp_servers"])
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans mcp_servers.* for stdio entries
// matching the mcp-language-server invocation pattern.
func (h *hermes) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := h.readYAML()
	if err != nil {
		return nil, err
	}
	servers := asStringMap(m["mcp_servers"])
	return findLanguageServerStdioInMap(servers), nil
}

// BackupContainsEntry reports whether the backup file at backupPath
// has an mcp_servers.<name> mapping.
func (h *hermes) BackupContainsEntry(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers := asStringMap(m["mcp_servers"])
	if servers == nil {
		return false, nil
	}
	// Require the entry to be a mapping. A scalar value would be
	// malformed at this path; treat as absent so sentinel fallback
	// refuses rather than silently writes corrupted data via
	// RestoreEntryFromBackup.
	entry := asStringMap(servers[name])
	return entry != nil, nil
}

// BackupEntryIsHubManaged reports whether mcp_servers.<name> in the YAML
// backup at backupPath is in Hermes's hub-HTTP shape (loopback `url`
// present, `command` absent). See Client.BackupEntryIsHubManaged.
func (h *hermes) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers := asStringMap(m["mcp_servers"])
	if servers == nil {
		return false, nil
	}
	entry := asStringMap(servers[name])
	if entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}

// asStringMap normalizes a parsed YAML node into a map[string]any.
// gopkg.in/yaml.v3 decodes mappings into map[string]any when every key
// is a string scalar, so the common case is a direct type assertion.
// The map[any]any fallback covers the rare case of a non-string key
// (defensive — a hub MCP config never has one), re-keying string keys
// and dropping non-string keys so the shared helpers (collectStdioEntries,
// findLanguageServerStdioInMap, isHubURLShapeEntry, extractHeaders) that
// all expect map[string]any keep working. Returns nil for any non-mapping
// node (scalar, sequence, or absent), matching the `, _ := raw.(map[string]any)`
// idiom every other adapter uses.
func asStringMap(raw any) map[string]any {
	switch v := raw.(type) {
	case map[string]any:
		return v
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			if ks, ok := k.(string); ok {
				out[ks] = val
			}
		}
		return out
	default:
		return nil
	}
}

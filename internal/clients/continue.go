package clients

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// NewContinue returns a Client bound to ~/.continue/config.yaml.
//
// Continue.dev (https://continue.dev, github.com/continuedev/continue) is a
// popular open-source AI code assistant for VS Code and JetBrains. Its global
// configuration is the YAML file ~/.continue/config.yaml ("config.yaml is the
// primary configuration format" — Continue Hub config-yaml reference), and MCP
// servers live under the top-level `mcpServers` key.
//
// IMPORTANT shape divergence from the rest of this package: Continue's
// `mcpServers` is a YAML SEQUENCE (a list), NOT a map keyed by server name.
// Every other YAML/JSON adapter here keys entries by name under an object;
// Continue keys each list ITEM by an inner `name` field:
//
//	mcpServers:
//	  - name: My Server
//	    type: streamable-http
//	    url: https://...
//
// Source (official docs, verified 2026-06):
//   - https://docs.continue.dev/customize/deep-dives/mcp — `mcpServers` list;
//     stdio (`type: stdio`, `command`, `args`, `env`), remote SSE
//     (`type: sse`, `url`), and remote streamable-http
//     (`type: streamable-http`, `url`) entry shapes.
//   - https://docs.continue.dev/customize/deep-dives/mcp-examples — verbatim
//     `type: streamable-http` + `url` remote examples (Supabase, Netlify,
//     PostHog) and `type: sse` + `url` (Sentry).
//   - https://docs.continue.dev/reference — `requestOptions.headers` is the
//     canonical custom-HTTP-header mechanism for remote servers.
//
// Transport choice — HTTP-direct (NOT relay-stdio). Continue speaks remote
// HTTP MCP natively, so the hub writes the streamable-http shape:
//
//	mcpServers:
//	  - name: serena
//	    type: streamable-http
//	    url: http://127.0.0.1:9121/mcp
//
// Optional auth headers (the G4 unified-hub per-client token) are emitted
// under `requestOptions.headers` — the Continue-canonical nesting — when
// MCPEntry.Headers is non-empty. Loopback daemon entries carry no headers.
//
// The hub-shape guard (isHubURLShapeEntry) keys off the `url` field and the
// absence of a `command` key, which holds for Continue's streamable-http
// entry shape, so demigrate/rollback recognize a hub-managed Continue entry.
//
// Continue reads many other settings (models, context, rules, ...) from the
// same config.yaml; AddEntry/RemoveEntry round-trip through map[string]any so
// every unrelated top-level key survives each write.
func NewContinue() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&continueClient{path: filepath.Join(home, ".continue", "config.yaml")}), nil
}

type continueClient struct {
	path string
}

// continueMCPKey is the single owner of Continue's top-level MCP section name.
const continueMCPKey = "mcpServers"

func (c *continueClient) Name() string       { return "continue" }
func (c *continueClient) ConfigPath() string { return c.path }

// IsRelayStdio reports false: continue is a URL-native HTTP MCP client.
func (c *continueClient) IsRelayStdio() bool { return false }

func (c *continueClient) Exists() bool {
	_, err := os.Stat(c.path)
	return err == nil
}

func (c *continueClient) Backup() (string, error) {
	return writeBackup(c.path, c.Name(), 0)
}

func (c *continueClient) BackupKeep(keepN int) (string, error) {
	return writeBackup(c.path, c.Name(), keepN)
}

// InitEmpty seeds ~/.continue/config.yaml with an empty `mcpServers: []` list
// if the file is absent. The key is declared (as an empty sequence — matching
// Continue's list shape) so a user inspecting the stub sees exactly where
// AddEntry will append servers. Continue reads many other settings from the
// same config.yaml, but because InitEmpty fires only when the file is missing,
// no user-authored configuration can be clobbered.
func (c *continueClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(c.path, []byte("mcpServers: []\n"))
}

func (c *continueClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(c.path, data)
}

// readYAML round-trips through map[string]any so unknown top-level keys
// (models, context, rules, ...) survive every write. A missing file yields an
// empty map.
func (c *continueClient) readYAML() (map[string]any, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (c *continueClient) writeYAML(m map[string]any) error {
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative + DACL-bound) for
	// token-bearing rewrites; tests get the os.WriteFile fallback.
	return WriteConfigFile(c.path, out)
}

// continueServerList normalizes the value at m[continueMCPKey] (which yaml.v3
// decodes as []any) into a slice. A non-list value (absent, scalar, or map)
// yields nil — the hub then starts a fresh list. Entries that are not maps are
// preserved verbatim by the caller (they're left untouched in the slice).
func continueServerList(m map[string]any) []any {
	list, _ := m[continueMCPKey].([]any)
	return list
}

// continueEntryName extracts the `name` field of a Continue list item.
// Returns "" for a non-map item or one with no string `name`.
func continueEntryName(item any) string {
	em := asStringMap(item)
	if em == nil {
		return ""
	}
	name, _ := em["name"].(string)
	return name
}

// continueServersAsMap projects the Continue server LIST into a
// name -> entry-map shape so the shared scan helpers (collectStdioEntries,
// findLanguageServerStdioInMap, isHubURLShapeEntry) — all written against the
// object-keyed format every other adapter uses — work unchanged. Items without
// a usable string `name` or that are not maps are dropped (they cannot
// participate in name-keyed scans). Later items with a duplicate name win,
// matching last-write-wins list semantics.
func continueServersAsMap(list []any) map[string]any {
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]any, len(list))
	for _, item := range list {
		em := asStringMap(item)
		if em == nil {
			continue
		}
		name, _ := em["name"].(string)
		if name == "" {
			continue
		}
		out[name] = em
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AddEntry adds or replaces the list item whose inner `name` equals
// entry.Name. The hub writes the streamable-http remote shape
// (`type: streamable-http`, `url`); optional auth headers go under
// `requestOptions.headers`. Any prior item with the same name is replaced
// wholesale (dropping stdio-era `command`/`args`/`env`); a brand-new server is
// appended to the list. Other list items and all unrelated top-level keys are
// preserved.
func (c *continueClient) AddEntry(entry MCPEntry) error {
	m, err := c.readYAML()
	if err != nil {
		return err
	}
	list := continueServerList(m)

	newItem := map[string]any{
		"name": entry.Name,
		"type": "streamable-http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		// Continue's canonical custom-header mechanism for HTTP servers is
		// requestOptions.headers (see NewContinue doc). Headers are a
		// string->string map.
		hdrs := make(map[string]any, len(entry.Headers))
		for k, v := range entry.Headers {
			hdrs[k] = v
		}
		newItem["requestOptions"] = map[string]any{"headers": hdrs}
	}

	replaced := false
	for i, item := range list {
		if continueEntryName(item) == entry.Name {
			list[i] = newItem
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, newItem)
	}
	m[continueMCPKey] = list
	return c.writeYAML(m)
}

// RemoveEntry drops the list item whose inner `name` equals name. Idempotent:
// absence is a no-op. Other list items and unrelated top-level keys survive.
func (c *continueClient) RemoveEntry(name string) error {
	m, err := c.readYAML()
	if err != nil {
		return err
	}
	list := continueServerList(m)
	if list == nil {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		if continueEntryName(item) == name {
			continue
		}
		out = append(out, item)
	}
	m[continueMCPKey] = out
	return c.writeYAML(m)
}

func (c *continueClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := c.readYAML()
	if err != nil {
		return nil, err
	}
	for _, item := range continueServerList(m) {
		raw := asStringMap(item)
		if raw == nil {
			continue
		}
		if n, _ := raw["name"].(string); n != name {
			continue
		}
		url, _ := raw["url"].(string)
		return &MCPEntry{Name: name, URL: url, Headers: continueEntryHeaders(raw), Disabled: mcpEntryDisabled(raw)}, nil
	}
	return nil, nil
}

// continueEntryHeaders extracts requestOptions.headers from a parsed Continue
// entry map as a string->string map. Returns nil when absent or empty.
func continueEntryHeaders(raw map[string]any) map[string]string {
	ro := asStringMap(raw["requestOptions"])
	if ro == nil {
		return nil
	}
	return extractHeaders(ro, "headers")
}

// LatestBackupPath delegates to the shared helper.
func (c *continueClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(c.path, c.Name())
}

// RestoreEntryFromBackup reads the YAML backup, finds the mcpServers list item
// whose inner `name` equals name (if present), and writes it over the live
// config's corresponding item. Other list items are left untouched.
//
// Defensively refuses if the backup's copy of the named entry is already in
// hub-HTTP form (loopback `url`, no `command`) — see
// ErrBackupEntryAlreadyMigrated.
func (c *continueClient) RestoreEntryFromBackup(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc on
// Client.RestoreEntryFromBackupForRollback). Install rollback and Serena
// migrate rollback use it when the timestamped backup is the source of truth.
func (c *continueClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a hub-HTTP-shaped backup entry with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes the
// backup item verbatim regardless of shape. When the backup lacks the named
// entry, the live item is removed (matching the every-adapter contract).
func (c *continueClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
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

	// Find the backup's copy of the named list item.
	var backupItem any
	found := false
	for _, item := range continueServerList(backupMap) {
		if continueEntryName(item) == name {
			backupItem = item
			found = true
			break
		}
	}

	liveMap, err := c.readYAML()
	if err != nil {
		return err
	}
	liveList := continueServerList(liveMap)

	if found {
		// Defensive: refuse hub-HTTP-shaped backup entries (loopback `url`
		// present, `command` absent). The rollback caller
		// (allowHubEntry=true) bypasses this guard to restore the
		// pre-reconcile legacy hub entry verbatim.
		if !allowHubEntry {
			if rawMap := asStringMap(backupItem); rawMap != nil {
				if isHubURLShapeEntry(rawMap, "url") {
					return ErrBackupEntryAlreadyMigrated
				}
			}
		}
		// Replace the live item in place, or append when the live config
		// lacks it.
		replaced := false
		for i, item := range liveList {
			if continueEntryName(item) == name {
				liveList[i] = backupItem
				replaced = true
				break
			}
		}
		if !replaced {
			liveList = append(liveList, backupItem)
		}
		liveMap[continueMCPKey] = liveList
		return c.writeYAML(liveMap)
	}

	// Backup lacks the entry — remove it from the live config.
	out := make([]any, 0, len(liveList))
	for _, item := range liveList {
		if continueEntryName(item) == name {
			continue
		}
		out = append(out, item)
	}
	liveMap[continueMCPKey] = out
	return c.writeYAML(liveMap)
}

// AllStdioEntries returns every stdio entry from the mcpServers list, projected
// through the shared name-keyed scan helper.
func (c *continueClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := c.readYAML()
	if err != nil {
		return nil, err
	}
	servers := continueServersAsMap(continueServerList(m))
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans the mcpServers list for stdio entries
// matching the mcp-language-server invocation pattern.
func (c *continueClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := c.readYAML()
	if err != nil {
		return nil, err
	}
	servers := continueServersAsMap(continueServerList(m))
	return findLanguageServerStdioInMap(servers), nil
}

// BackupContainsEntry reports whether the backup file at backupPath has an
// mcpServers list item whose inner `name` equals name.
func (c *continueClient) BackupContainsEntry(backupPath, name string) (bool, error) {
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
	for _, item := range continueServerList(m) {
		// Require the item to be a mapping with the name. A scalar/malformed
		// item is treated as absent so sentinel fallback refuses rather than
		// silently writing corrupted data.
		if entry := asStringMap(item); entry != nil {
			if n, _ := entry["name"].(string); n == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// BackupEntryIsHubManaged reports whether the mcpServers list item named name
// in the YAML backup at backupPath is in Continue's hub-HTTP shape (loopback
// `url` present, `command` absent). See Client.BackupEntryIsHubManaged.
func (c *continueClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
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
	for _, item := range continueServerList(m) {
		entry := asStringMap(item)
		if entry == nil {
			continue
		}
		if n, _ := entry["name"].(string); n != name {
			continue
		}
		return isHubURLShapeEntry(entry, "url"), nil
	}
	return false, nil
}

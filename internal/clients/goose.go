package clients

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// NewGoose returns a Client bound to Goose's global config at
// ~/.config/goose/config.yaml.
//
// Goose (Block, Apache-2.0, https://block.github.io/goose,
// github.com/block/goose) is a real, current extensible AI agent. It reads
// MCP servers from the top-level `extensions` key of its single global YAML
// config — NOT `mcpServers`. Each MCP server is one "extension" entry.
//
// On-disk shape (verified 2026-06 against the Goose source — the serde
// definitions are the authoritative schema, the docs page is secondary):
//
//   - crates/goose/src/config/extensions.rs — `EXTENSIONS_CONFIG_KEY =
//     "extensions"`; each entry is an `ExtensionEntry { enabled: bool,
//     #[serde(flatten)] config: ExtensionConfig }`, so `enabled` sits at the
//     SAME level as the flattened `ExtensionConfig` fields (there is no
//     nested `config:` wrapper on disk).
//   - crates/goose/src/agents/extension.rs — `ExtensionConfig` is
//     `#[serde(tag = "type")]`, so a `type:` discriminator selects the
//     variant. The `streamable_http` variant (`#[serde(rename =
//     "streamable_http")]`) carries `name` (required), `uri` (required —
//     the endpoint, spelled `uri` NOT `url`), optional `headers`
//     (map<string,string>), `envs`, `env_keys`, `timeout`, `socket`,
//     `bundled`, `available_tools`. The stdio variant uses `cmd`/`args`/
//     `envs`. SSE is deprecated ("migrate to streamable_http").
//
// Transport choice — HTTP-direct (NOT relay-stdio). Goose supports remote
// MCP servers natively via its `streamable_http` extension type with a `uri`
// endpoint, so the hub writes that shape and never needs a stdio relay (the
// daemon is already an HTTP endpoint). IsRelayStdio() is therefore false,
// mirroring hermes/opencode.
//
// A hub-managed Goose extension looks like:
//
//	extensions:
//	  serena:
//	    enabled: true
//	    type: streamable_http
//	    name: serena
//	    uri: http://127.0.0.1:9121/mcp
//	    timeout: 300
//	    description: ""
//
// `timeout: 300` is Goose's documented DEFAULT_EXTENSION_TIMEOUT, written
// because the struct comment says "new configurations should include this
// field". `description: ""` matches what Goose itself emits (the field is
// `#[serde(default)]` so its absence is harmless, but writing it keeps the
// entry byte-aligned with a Goose-authored one). `headers` is emitted only
// when MCPEntry.Headers is non-empty.
//
// Goose entry KEYS are normalized by Goose's `name_to_key` (lowercase,
// whitespace stripped, non-[A-Za-z0-9_-] → '_'). mcphub server names
// ("serena", "memory", "mcp-language-server-go", …) are already valid keys,
// so the hub uses MCPEntry.Name verbatim as the map key; the inner `name`
// field also carries MCPEntry.Name (Goose matches extensions by that inner
// `name`, see get_extension_by_name).
//
// Goose's Windows-native config lives at %APPDATA%\Block\goose\config\
// config.yaml, but mcphub binds the XDG-style ~/.config/goose/config.yaml on
// every OS ($XDG_CONFIG_HOME-aware) for a single per-user posture matching
// every other user-scoped adapter; this is the canonical global config Goose
// also honors on macOS/Linux.
func NewGoose() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&goose{path: defaultGooseConfigPath(home)}), nil
}

// defaultGooseConfigPath returns the global Goose config path
// (~/.config/goose/config.yaml). $XDG_CONFIG_HOME is honored when set. The
// path is OS-independent by design — see the NewGoose doc.
func defaultGooseConfigPath(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "goose", "config.yaml")
	}
	return filepath.Join(home, ".config", "goose", "config.yaml")
}

type goose struct {
	path string
}

// gooseExtensionsKey is the single owner of Goose's top-level MCP section
// name. Every method that reaches into the parsed config map uses it.
const gooseExtensionsKey = "extensions"

// gooseDefaultTimeout is Goose's DEFAULT_EXTENSION_TIMEOUT (seconds). The
// streamable_http struct doc says new configurations should include the
// timeout field, so the hub writes this default.
const gooseDefaultTimeout = 300

func (g *goose) Name() string       { return "goose" }
func (g *goose) ConfigPath() string { return g.path }

// IsRelayStdio reports false: goose is a URL-native HTTP MCP client (it has a
// native streamable_http extension type).
func (g *goose) IsRelayStdio() bool { return false }

// Exists treats Goose as installed when EITHER the config file is present OR
// its parent directory (~/.config/goose/) exists, mirroring the
// opencode/cursor/vscode "directory means installed" heuristic so an operator
// who has Goose installed but no MCP config yet still gets the Initialize /
// install affordance.
func (g *goose) Exists() bool {
	if _, err := os.Stat(g.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(g.path))
	return err == nil && st.IsDir()
}

func (g *goose) Backup() (string, error) {
	return g.BackupKeep(0)
}

// BackupKeep ensures the nested ~/.config/goose parent directory exists,
// seeds an empty `extensions: {}` stub if the config is absent, then writes
// the timestamped backup (pruning to keepN). The parent dir does not exist on
// a clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host. Mirrors the
// opencode/cursor/vscode BackupKeep wrappers.
func (g *goose) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(g.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := g.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(g.path, g.Name(), keepN)
}

// InitEmpty seeds ~/.config/goose/config.yaml with an empty `extensions: {}`
// map if the file is absent. The key is declared (rather than dropping an
// empty file) so a user inspecting the stub sees exactly where AddEntry will
// append new extensions. Goose reads many other settings from the same
// config.yaml, but because InitEmpty fires only when the file is missing, no
// user-authored configuration can be clobbered.
func (g *goose) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(g.path, []byte("extensions: {}\n"))
}

func (g *goose) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(g.path, data)
}

// readYAML / writeYAML round-trip through map[string]any so unknown
// top-level keys (Goose stores provider/model/etc. settings here) survive.
func (g *goose) readYAML() (map[string]any, error) {
	data, err := os.ReadFile(g.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", g.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (g *goose) writeYAML(m map[string]any) error {
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative + DACL-bound) for
	// token-bearing rewrites; tests get the os.WriteFile fallback.
	return WriteConfigFile(g.path, out)
}

// AddEntry writes the hub-managed streamable_http extension under
// extensions.<name>. The entry carries `enabled: true` and the flattened
// streamable_http config (`type`, `name`, `uri`, `timeout`, `description`).
// An optional `headers` map is emitted when MCPEntry.Headers is non-empty.
// The entry is replaced wholesale, dropping any stdio-era fields (cmd/args/
// envs) — transport is selected by the `type` discriminator.
func (g *goose) AddEntry(entry MCPEntry) error {
	m, err := g.readYAML()
	if err != nil {
		return err
	}
	extensions := asStringMap(m[gooseExtensionsKey])
	if extensions == nil {
		extensions = map[string]any{}
	}
	entryMap := map[string]any{
		"enabled":     true,
		"type":        "streamable_http",
		"name":        entry.Name,
		"uri":         entry.URL,
		"timeout":     gooseDefaultTimeout,
		"description": "",
	}
	if len(entry.Headers) > 0 {
		entryMap["headers"] = entry.Headers
	}
	extensions[entry.Name] = entryMap
	m[gooseExtensionsKey] = extensions
	return g.writeYAML(m)
}

func (g *goose) RemoveEntry(name string) error {
	m, err := g.readYAML()
	if err != nil {
		return err
	}
	extensions := asStringMap(m[gooseExtensionsKey])
	if extensions == nil {
		return nil
	}
	delete(extensions, name)
	m[gooseExtensionsKey] = extensions
	return g.writeYAML(m)
}

func (g *goose) GetEntry(name string) (*MCPEntry, error) {
	m, err := g.readYAML()
	if err != nil {
		return nil, err
	}
	extensions := asStringMap(m[gooseExtensionsKey])
	if extensions == nil {
		return nil, nil
	}
	raw := asStringMap(extensions[name])
	if raw == nil {
		return nil, nil
	}
	// Goose's endpoint field is `uri`, not `url`.
	uri, _ := raw["uri"].(string)
	return &MCPEntry{Name: name, URL: uri, Headers: extractHeaders(raw, "headers"), Disabled: mcpEntryDisabled(raw)}, nil
}

// LatestBackupPath delegates to the shared helper.
func (g *goose) LatestBackupPath() (string, bool, error) {
	return latestBackup(g.path, g.Name())
}

// RestoreEntryFromBackup reads the YAML backup, extracts the
// extensions.<name> mapping (if present), and writes it over the live
// config's corresponding entry. Other extensions.* entries are left
// untouched.
//
// Defensively refuses if the backup's copy of the named entry is already in
// hub-managed shape (a loopback `uri` and no `cmd`) — see
// ErrBackupEntryAlreadyMigrated.
func (g *goose) RestoreEntryFromBackup(backupPath, name string) error {
	return g.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc on
// Client.RestoreEntryFromBackupForRollback). Install rollback and Serena
// migrate rollback use it when the timestamped backup is the source of truth.
func (g *goose) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return g.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a hub-managed backup entry with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes the
// backup bytes verbatim regardless of shape.
func (g *goose) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
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
	backupExtensions := asStringMap(backupMap[gooseExtensionsKey])
	liveMap, err := g.readYAML()
	if err != nil {
		return err
	}
	liveExtensions := asStringMap(liveMap[gooseExtensionsKey])
	if liveExtensions == nil {
		liveExtensions = map[string]any{}
	}
	if backupExtensions != nil {
		if backupEntry, present := backupExtensions[name]; present {
			// Defensive: refuse hub-managed backup entries (loopback `uri`,
			// no `cmd`). User-configured remote streamable_http entries
			// (non-loopback uri) pass through. The rollback caller
			// (allowHubEntry=true) bypasses this guard to restore the
			// pre-reconcile legacy hub entry verbatim.
			if !allowHubEntry {
				if rawMap := asStringMap(backupEntry); rawMap != nil {
					if isHubGooseEntry(rawMap) {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			liveExtensions[name] = backupEntry
			liveMap[gooseExtensionsKey] = liveExtensions
			return g.writeYAML(liveMap)
		}
	}
	delete(liveExtensions, name)
	liveMap[gooseExtensionsKey] = liveExtensions
	return g.writeYAML(liveMap)
}

// AllStdioEntries returns every stdio extension from extensions.*. Goose
// stdio extensions store the executable under `cmd` (not `command`), so this
// adapter normalizes each entry to the shared `command`-keyed shape (see
// gooseStdioView) and delegates to the single-owner collectStdioEntries —
// keeping the stdio-detection + disabled-skip + args-extraction logic in one
// place. The hub writes only streamable_http (no `cmd`) entries, which are
// correctly skipped.
func (g *goose) AllStdioEntries() ([]StdioEntry, error) {
	m, err := g.readYAML()
	if err != nil {
		return nil, err
	}
	extensions := asStringMap(m[gooseExtensionsKey])
	return collectStdioEntries(gooseStdioView(extensions)), nil
}

// FindStdioLanguageServerEntries scans extensions.* for stdio entries
// matching the mcp-language-server invocation pattern. As with
// AllStdioEntries, Goose entries are normalized to the shared `command`-keyed
// shape so the single-owner findLanguageServerStdioInMap classifier runs
// unchanged.
func (g *goose) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := g.readYAML()
	if err != nil {
		return nil, err
	}
	extensions := asStringMap(m[gooseExtensionsKey])
	return findLanguageServerStdioInMap(gooseStdioView(extensions)), nil
}

// gooseStdioView re-keys every Goose extension entry's `cmd` field to the
// shared `command` field name expected by collectStdioEntries /
// findLanguageServerStdioInMap, copying `args` through verbatim and
// preserving the `enabled` flag (those helpers already skip `enabled: false`).
// `type`/`uri`/`headers`-only streamable_http entries (no `cmd`) come through
// with no `command` and are skipped downstream. The returned map values are
// fresh map[string]any so the live config is never mutated. nil/empty input
// yields nil so the downstream helpers short-circuit to a nil slice.
func gooseStdioView(extensions map[string]any) map[string]any {
	if len(extensions) == 0 {
		return nil
	}
	view := make(map[string]any, len(extensions))
	for name, raw := range extensions {
		entry := asStringMap(raw)
		if entry == nil {
			continue
		}
		shared := map[string]any{}
		if cmd, ok := entry["cmd"]; ok {
			shared["command"] = cmd
		}
		if args, ok := entry["args"]; ok {
			shared["args"] = args
		}
		if enabled, ok := entry["enabled"]; ok {
			shared["enabled"] = enabled
		}
		view[name] = shared
	}
	return view
}

// BackupContainsEntry reports whether the backup file at backupPath has an
// extensions.<name> mapping.
func (g *goose) BackupContainsEntry(backupPath, name string) (bool, error) {
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
	extensions := asStringMap(m[gooseExtensionsKey])
	if extensions == nil {
		return false, nil
	}
	// Require the entry to be a mapping. A scalar value would be malformed at
	// this path; treat as absent so sentinel fallback refuses rather than
	// silently writes corrupted data via RestoreEntryFromBackup.
	entry := asStringMap(extensions[name])
	return entry != nil, nil
}

// BackupEntryIsHubManaged reports whether extensions.<name> in the YAML
// backup at backupPath is in Goose's hub-managed shape (loopback `uri`, no
// `cmd`). See Client.BackupEntryIsHubManaged.
func (g *goose) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
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
	extensions := asStringMap(m[gooseExtensionsKey])
	if extensions == nil {
		return false, nil
	}
	entry := asStringMap(extensions[name])
	if entry == nil {
		return false, nil
	}
	return isHubGooseEntry(entry), nil
}

// isHubGooseEntry reports whether a parsed Goose extension entry is in
// mcphub's hub-managed shape: the entry's `uri` value is a hub loopback URL
// (IsHubHTTPURL) AND the entry has no `cmd` key (a stdio extension). This is
// the Goose analogue of the shared isHubURLShapeEntry, which keys off the
// `url`/`httpUrl` fields and `command`; Goose's field names differ (`uri`
// for the endpoint, `cmd` for the stdio executable), so the shared helper
// cannot be reused here. User-configured remote streamable_http extensions
// (non-loopback uri) and stdio extensions (have `cmd`) are NOT hub-managed.
func isHubGooseEntry(rawMap map[string]any) bool {
	uri, _ := rawMap["uri"].(string)
	if !IsHubHTTPURL(uri) {
		return false
	}
	if _, hasCmd := rawMap["cmd"]; hasCmd {
		return false
	}
	return true
}

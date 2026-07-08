package clients

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// OpenHands stores its MCP-server configuration in a `config.toml` file under a
// `[mcp]` section. Unlike codex-cli (which uses per-name `[mcp_servers.<name>]`
// TOML tables), OpenHands uses TOML ARRAYS of inline tables:
//
//	[mcp]
//	stdio_servers = [
//	    {name="fetch", command="uvx", args=["mcp-server-fetch"]},
//	    {name="filesystem", command="npx", args=["@mcp/fs", "/"], env={"DEBUG"="true"}},
//	]
//	shttp_servers = [
//	    "https://api.example.com/mcp/shttp",
//	    {url="https://files.example.com/mcp/shttp", api_key="...", timeout=1800},
//	]
//
// Verified against the OpenHands source (openhands/core/config/mcp_config.py,
// MCPStdioServerConfig / MCPSHTTPServerConfig) and the docs at
// https://docs.openhands.dev/openhands/usage/settings/mcp-settings :
//
//   - stdio_servers entries are inline tables with required `name`+`command`,
//     optional `args` (list) and `env` (dict). The `name` lives INSIDE the
//     table, not as a TOML table key.
//   - shttp_servers entries are EITHER a bare URL string OR an inline table
//     with required `url`, optional `api_key`, optional `timeout` (1-3600).
//     The documented schema has NO `name` field on shttp entries.
//
// mcp-local-hub maps stdio-bridge daemons -> stdio_servers and http daemons ->
// shttp_servers. This adapter is URL-native (IsRelayStdio() == false): every
// hub-managed entry carries the loopback HTTP URL (MCPEntry.URL,
// http://127.0.0.1:<port>/...), so AddEntry writes an shttp_servers inline
// table. To give mcphub a stable per-name identity for idempotent
// AddEntry/RemoveEntry/GetEntry — which shttp's documented schema does not
// provide — the inline table also carries a `name` key. OpenHands'
// MCPSHTTPServerConfig is a plain pydantic BaseModel (default extra='ignore',
// NOT extra='forbid' — confirmed in the source), so the extra `name` key is
// silently ignored by OpenHands while serving as mcphub's lookup key. This
// mirrors how stdio_servers already carries `name` inline.

// NewOpenHands returns a Client bound to <home>/.openhands/config.toml.
//
// OpenHands' default config-search behavior loads `config.toml` from the
// current working directory (the project/workspace root), overridable via the
// `--config-file` arg (see OpenHands issue #3947 / PR #4168). There is no fixed
// home-anchored path for the TOML form, so the location is ambiguous. This
// adapter targets the per-user OpenHands config directory `~/.openhands/` (the
// documented user-level config home — see OpenHands issue #10947 and the
// common `~/.openhands/config.toml` volume-mount workaround in issue #5957),
// which is the stable home-anchored default the client registry's
// `configPath(home)` contract requires. Operators who run OpenHands with a
// project-root or `--config-file` config should point this adapter there (or
// copy the written `[mcp]` section across).
func NewOpenHands() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&openHands{path: filepath.Join(home, ".openhands", "config.toml")}), nil
}

type openHands struct {
	path string
}

func (o *openHands) Name() string       { return "openhands" }
func (o *openHands) ConfigPath() string { return o.path }

// IsRelayStdio reports false: OpenHands speaks Streamable HTTP (shttp) MCP
// natively, so mcphub writes the loopback hub URL directly rather than a
// relay-stdio bridge.
func (o *openHands) IsRelayStdio() bool { return false }

func (o *openHands) Exists() bool {
	_, err := os.Stat(o.path)
	return err == nil
}

func (o *openHands) Backup() (string, error) {
	return writeBackup(o.path, o.Name(), 0)
}

func (o *openHands) BackupKeep(keepN int) (string, error) {
	return writeBackup(o.path, o.Name(), keepN)
}

// InitEmpty seeds <home>/.openhands/config.toml with an empty `[mcp]` table if
// the file is absent. The header is declared (rather than dropping an empty
// file) so a user inspecting the stub sees exactly where AddEntry will append
// the stdio_servers / shttp_servers arrays. OpenHands reads many other settings
// from the same config.toml, but because InitEmpty fires only when the file is
// missing, no user-authored configuration can be clobbered.
func (o *openHands) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(o.path, []byte("[mcp]\n"))
}

func (o *openHands) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(o.path, data)
}

// readTOML / writeTOML round-trip through map[string]any so unknown sections
// (and any `[core]`/`[llm]`/etc. OpenHands config) survive.
func (o *openHands) readTOML() (map[string]any, error) {
	data, err := os.ReadFile(o.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", o.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (o *openHands) writeTOML(m map[string]any) error {
	out, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative + DACL-bound) for
	// token-bearing rewrites; tests get the os.WriteFile fallback.
	return WriteConfigFile(o.path, out)
}

// mcpSection returns the `[mcp]` table from the parsed config (a fresh empty
// map when absent). go-toml/v2 decodes a `[mcp]` table to map[string]any.
func mcpSection(m map[string]any) map[string]any {
	sec, _ := m["mcp"].(map[string]any)
	if sec == nil {
		sec = map[string]any{}
	}
	return sec
}

// serverArray returns the named `[mcp]` array (stdio_servers / shttp_servers)
// as a []any. go-toml/v2 decodes an array-of-inline-tables to []any whose
// members are map[string]any, and bare strings to string members. Returns nil
// when the key is absent or not an array.
func serverArray(sec map[string]any, key string) []any {
	arr, _ := sec[key].([]any)
	return arr
}

// shttpEntryName extracts the mcphub identity `name` key from one shttp_servers
// member. Bare-string members (user-authored URL-only entries) and inline
// tables without a `name` key return "". Only entries this adapter wrote carry
// a `name`.
func shttpEntryName(member any) string {
	tbl, ok := member.(map[string]any)
	if !ok {
		return ""
	}
	name, _ := tbl["name"].(string)
	return name
}

// stdioEntryName extracts the `name` field from one stdio_servers inline-table
// member (OpenHands stores the entry name INSIDE the table). Non-table members
// or tables without a string `name` return "".
func stdioEntryName(member any) string {
	tbl, ok := member.(map[string]any)
	if !ok {
		return ""
	}
	name, _ := tbl["name"].(string)
	return name
}

// AddEntry adds or replaces the hub-managed shttp_servers entry named
// entry.Name. The entry is written as an inline table {name=<n>, url=<u>}. Any
// existing shttp entry with the same `name`, and any stdio_servers entry with
// the same `name` (a pre-hub stdio form being migrated to HTTP), are removed
// first — wholesale replace, matching the codex-cli/hermes URL adapters which
// drop stdio-era fields on migrate.
//
// OpenHands' shttp schema has no `headers` field (only url/api_key/timeout), so
// MCPEntry.Headers cannot be represented here and is intentionally dropped; the
// hub's loopback daemons need no auth header anyway.
func (o *openHands) AddEntry(entry MCPEntry) error {
	m, err := o.readTOML()
	if err != nil {
		return err
	}
	sec := mcpSection(m)

	// Build the replacement shttp inline table. The `name` key is mcphub's
	// identity anchor (ignored by OpenHands); `url` is the connection target.
	newEntry := map[string]any{
		"name": entry.Name,
		"url":  entry.URL,
	}

	// Rewrite shttp_servers: drop any prior same-named hub entry, keep
	// everything else (bare URL strings + other operators' tables) verbatim,
	// then append the fresh entry.
	shttp := serverArray(sec, "shttp_servers")
	rebuilt := make([]any, 0, len(shttp)+1)
	for _, member := range shttp {
		if shttpEntryName(member) == entry.Name {
			continue // replaced below
		}
		rebuilt = append(rebuilt, member)
	}
	rebuilt = append(rebuilt, newEntry)
	sec["shttp_servers"] = rebuilt

	// Wholesale-replace semantics: if this name previously existed as a
	// stdio_servers entry (the pre-hub stdio form being migrated to HTTP),
	// drop it so the daemon is not double-registered.
	if stdio := serverArray(sec, "stdio_servers"); len(stdio) > 0 {
		kept := make([]any, 0, len(stdio))
		for _, member := range stdio {
			if stdioEntryName(member) == entry.Name {
				continue
			}
			kept = append(kept, member)
		}
		setOrDeleteArray(sec, "stdio_servers", kept)
	}

	m["mcp"] = sec
	return o.writeTOML(m)
}

// RemoveEntry removes the entry named `name` from BOTH shttp_servers and
// stdio_servers (idempotent — absence is a no-op). Bare-string shttp members
// (no `name`) and entries owned by other operators are left untouched.
func (o *openHands) RemoveEntry(name string) error {
	m, err := o.readTOML()
	if err != nil {
		return err
	}
	sec, ok := m["mcp"].(map[string]any)
	if !ok || sec == nil {
		return nil // no [mcp] section: nothing to remove
	}

	changed := false
	if shttp := serverArray(sec, "shttp_servers"); len(shttp) > 0 {
		kept := make([]any, 0, len(shttp))
		for _, member := range shttp {
			if shttpEntryName(member) == name {
				changed = true
				continue
			}
			kept = append(kept, member)
		}
		if changed {
			setOrDeleteArray(sec, "shttp_servers", kept)
		}
	}
	if stdio := serverArray(sec, "stdio_servers"); len(stdio) > 0 {
		kept := make([]any, 0, len(stdio))
		removed := false
		for _, member := range stdio {
			if stdioEntryName(member) == name {
				removed = true
				continue
			}
			kept = append(kept, member)
		}
		if removed {
			changed = true
			setOrDeleteArray(sec, "stdio_servers", kept)
		}
	}
	if !changed {
		return nil
	}
	m["mcp"] = sec
	return o.writeTOML(m)
}

// setOrDeleteArray writes `kept` to sec[key], or deletes the key entirely when
// `kept` is empty — so the config never carries an empty `shttp_servers = []`
// leftover after the last hub entry is removed.
func setOrDeleteArray(sec map[string]any, key string, kept []any) {
	if len(kept) == 0 {
		delete(sec, key)
		return
	}
	sec[key] = kept
}

// GetEntry returns the hub-managed shttp_servers entry named `name`, or nil if
// absent. Only entries carrying mcphub's `name` identity key are returned;
// bare-string URL entries and other operators' tables are invisible to GetEntry
// (they have no name to key on).
func (o *openHands) GetEntry(name string) (*MCPEntry, error) {
	m, err := o.readTOML()
	if err != nil {
		return nil, err
	}
	sec, ok := m["mcp"].(map[string]any)
	if !ok || sec == nil {
		return nil, nil
	}
	for _, member := range serverArray(sec, "shttp_servers") {
		if shttpEntryName(member) != name {
			continue
		}
		tbl, _ := member.(map[string]any)
		url, _ := tbl["url"].(string)
		return &MCPEntry{Name: name, URL: url, Disabled: mcpEntryDisabled(tbl)}, nil
	}
	return nil, nil
}

// LatestBackupPath delegates to the shared helper.
func (o *openHands) LatestBackupPath() (string, bool, error) {
	return latestBackup(o.path, o.Name())
}

// RestoreEntryFromBackup reads the TOML backup, extracts the entry named `name`
// from the backup's `[mcp]` arrays (shttp_servers OR stdio_servers), and writes
// that pre-migrate shape over the live config's corresponding entry. Other
// entries in the live config are left untouched.
//
// Defensively refuses if the backup's copy of the named entry is already in
// hub-HTTP form (an shttp_servers inline table with a loopback `url` and no
// `command`) — see ErrBackupEntryAlreadyMigrated.
func (o *openHands) RestoreEntryFromBackup(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc on
// Client.RestoreEntryFromBackupForRollback). Install rollback and Serena
// migrate rollback use it when the timestamped backup is the source of truth.
func (o *openHands) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a hub-HTTP-shaped backup entry with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes the
// backup's entry verbatim regardless of shape.
//
// Both array kinds are scanned because the pre-hub form of an entry may live in
// EITHER stdio_servers (a user's direct stdio config) or shttp_servers (a
// user's remote-HTTP config). The restored member is written back into whichever
// array it came from in the backup; the live config's same-named entry is first
// removed from BOTH arrays so the restore cannot duplicate it.
func (o *openHands) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	var backupMap map[string]any
	if len(backupData) > 0 {
		if err := toml.Unmarshal(backupData, &backupMap); err != nil {
			return fmt.Errorf("parse backup %s: %w", backupPath, err)
		}
	}
	if backupMap == nil {
		backupMap = map[string]any{}
	}
	backupSec, _ := backupMap["mcp"].(map[string]any)

	// Find the backup's copy of this entry, in either array.
	var (
		backupMember any
		backupArray  string // "shttp_servers" or "stdio_servers"
	)
	if backupSec != nil {
		for _, member := range serverArray(backupSec, "shttp_servers") {
			if shttpEntryName(member) == name {
				backupMember, backupArray = member, "shttp_servers"
				break
			}
		}
		if backupMember == nil {
			for _, member := range serverArray(backupSec, "stdio_servers") {
				if stdioEntryName(member) == name {
					backupMember, backupArray = member, "stdio_servers"
					break
				}
			}
		}
	}

	// Load the live config and strip the live copy of this name from both
	// arrays first (so a restore never duplicates and an absent backup entry
	// degrades to a removal).
	liveMap, err := o.readTOML()
	if err != nil {
		return err
	}
	liveSec := mcpSection(liveMap)
	stripNamedFromArray(liveSec, "shttp_servers", name, shttpEntryName)
	stripNamedFromArray(liveSec, "stdio_servers", name, stdioEntryName)

	if backupMember == nil {
		// Backup had no copy of this entry: migrate added it from scratch, so
		// demigrate must remove the live entry (already stripped above).
		liveMap["mcp"] = liveSec
		return o.writeTOML(liveMap)
	}

	// Defensive: refuse a backup entry that is itself in hub-managed shape
	// (an shttp inline table with a loopback url and no command). The rollback
	// caller (allowHubEntry=true) bypasses this guard to restore the
	// pre-reconcile legacy hub entry verbatim.
	if !allowHubEntry {
		if tbl, ok := backupMember.(map[string]any); ok {
			if isHubURLShapeEntry(tbl, "url") {
				return ErrBackupEntryAlreadyMigrated
			}
		}
	}

	sec := liveSec
	arr := serverArray(sec, backupArray)
	arr = append(arr, backupMember)
	sec[backupArray] = arr
	liveMap["mcp"] = sec
	return o.writeTOML(liveMap)
}

// stripNamedFromArray removes every member of sec[key] whose name (via
// nameFn) equals `name`, deleting the key entirely when nothing remains.
func stripNamedFromArray(sec map[string]any, key, name string, nameFn func(any) string) {
	arr := serverArray(sec, key)
	if len(arr) == 0 {
		return
	}
	kept := make([]any, 0, len(arr))
	for _, member := range arr {
		if nameFn(member) == name {
			continue
		}
		kept = append(kept, member)
	}
	setOrDeleteArray(sec, key, kept)
}

// AllStdioEntries returns every stdio entry from `[mcp].stdio_servers`. Each
// inline table already carries its `name` field, so it is rekeyed into the
// map[string]any shape the shared collectStdioEntries helper expects (entry
// name -> entry table).
func (o *openHands) AllStdioEntries() ([]StdioEntry, error) {
	servers, err := o.stdioServersByName()
	if err != nil {
		return nil, err
	}
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans `[mcp].stdio_servers` for stdio entries
// matching the mcp-language-server invocation pattern.
func (o *openHands) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	servers, err := o.stdioServersByName()
	if err != nil {
		return nil, err
	}
	return findLanguageServerStdioInMap(servers), nil
}

// stdioServersByName reads `[mcp].stdio_servers` and returns a name->table map
// in the shape the shared map-based helpers (collectStdioEntries,
// findLanguageServerStdioInMap) consume. Members without a `name` field are
// skipped (they cannot be addressed by mcphub). On a duplicate name, the last
// wins — matching map semantics; OpenHands' own loader would also be
// ambiguous, but mcphub never writes duplicates.
func (o *openHands) stdioServersByName() (map[string]any, error) {
	m, err := o.readTOML()
	if err != nil {
		return nil, err
	}
	sec, ok := m["mcp"].(map[string]any)
	if !ok || sec == nil {
		return nil, nil
	}
	stdio := serverArray(sec, "stdio_servers")
	if len(stdio) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(stdio))
	for _, member := range stdio {
		tbl, ok := member.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tbl["name"].(string)
		if name == "" {
			continue
		}
		out[name] = tbl
	}
	return out, nil
}

// BackupContainsEntry reports whether the backup file at backupPath has an
// `[mcp]` entry named `name` in EITHER stdio_servers or shttp_servers.
func (o *openHands) BackupContainsEntry(backupPath, name string) (bool, error) {
	sec, err := readBackupMCPSection(backupPath)
	if err != nil {
		return false, err
	}
	if sec == nil {
		return false, nil
	}
	for _, member := range serverArray(sec, "shttp_servers") {
		if shttpEntryName(member) == name {
			return true, nil
		}
	}
	for _, member := range serverArray(sec, "stdio_servers") {
		if stdioEntryName(member) == name {
			return true, nil
		}
	}
	return false, nil
}

// BackupEntryIsHubManaged reports whether the entry named `name` in the TOML
// backup at backupPath is in OpenHands' hub-HTTP shape (an shttp_servers inline
// table with a loopback `url` and no `command`). A stdio_servers member is
// never hub-managed (the hub writes shttp, not stdio). See
// Client.BackupEntryIsHubManaged.
func (o *openHands) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	sec, err := readBackupMCPSection(backupPath)
	if err != nil {
		return false, err
	}
	if sec == nil {
		return false, nil
	}
	for _, member := range serverArray(sec, "shttp_servers") {
		if shttpEntryName(member) != name {
			continue
		}
		tbl, ok := member.(map[string]any)
		if !ok {
			return false, nil
		}
		return isHubURLShapeEntry(tbl, "url"), nil
	}
	return false, nil
}

// readBackupMCPSection reads + parses the TOML backup and returns its `[mcp]`
// section (nil when the file is empty or has no `[mcp]` table). Read/parse
// errors propagate so callers distinguish "absent" from "broken".
func readBackupMCPSection(backupPath string) (map[string]any, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return nil, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	sec, _ := m["mcp"].(map[string]any)
	return sec, nil
}

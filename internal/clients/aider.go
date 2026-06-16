package clients

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// NewAider returns a Client bound to ~/.aider.conf.yml.
//
// Aider (aider-ai/aider) is a terminal AI pair-programming tool. Its MCP
// client support reads MCP servers from a YAML config under the top-level
// `mcp-server` key (SINGULAR), whose value is a LIST of server objects —
// NOT a name-keyed map like the other YAML adapter (hermes). Each list
// element carries {name, command, args, env}:
//
//	mcp-server:
//	  - name: filesystem
//	    command: npx
//	    args: ['-y', '@modelcontextprotocol/server-filesystem', '/path']
//	  - name: github
//	    command: npx
//	    args: ['-y', '@modelcontextprotocol/server-github']
//	    env:
//	      GITHUB_PERSONAL_ACCESS_TOKEN: ${GH_TOKEN}
//
// Config search order is `~/.aider.conf.yml`, `<git-root>/.aider.conf.yml`,
// then the cwd; the home-dir file is the natural GLOBAL location, which is
// what mcphub writes (verified against .scratch — no live config on host).
//
// ── IsRelayStdio decision: TRUE (relay-stdio, like Antigravity) ──
//
// Aider's documented MCP-server schema is STDIO-ONLY: every captured example
// (.scratch/adapter-schemas-openhands-aider.md) and the source-doc shape is
// `{name, command, args, env}`. There is NO documented or verifiable
// `url`/`http`/`serverUrl` server-entry form:
//
//   - The official aider docs indexed by Context7 (/aider-ai/aider and
//     /websites/aider_chat) contain NO `mcp-server` configuration reference at
//     all — MCP support is recent/PR-based and absent from the documented
//     config surface; querying for an http/url MCP entry returned nothing.
//   - The aider GitHub source has no `aider/mcp/server.py` (HTTP 404 on
//     raw.githubusercontent.com/aider-ai/aider/main/aider/mcp/server.py), so a
//     mainline url/http server shape could not be confirmed.
//   - The captured schema doc itself flags the http/url form as UNVERIFIED.
//
// Per the evidence-citation discipline, an unverified url-entry form is an
// ASSUMPTION (UNVERIFIED) and must not be written. The only schema we can
// emit correctly is the stdio shape — `{name, command, args}` — which cannot
// carry a raw hub HTTP URL. So, exactly like the Antigravity adapter, mcphub
// writes a STDIO entry invoking our own `mcphub relay` subcommand; aider
// spawns the relay as its child, the relay connects to the shared HTTP daemon,
// and aider transparently joins the shared-daemon architecture.
//
// Entry shape written (stdio relay, mirroring antigravity.go):
//
//	- name: <server-name>
//	  command: <abs-path>/mcphub.exe
//	  args: ['relay', '--server', '<s>', '--daemon', '<d>']   # or ['relay','--url',<u>]
//
// Requires MCPEntry.RelayExePath to be set (absolute), plus RelayServer +
// RelayDaemon for the manifest-lookup form (or RelayURL for the direct form).
// install.go populates them from manifest + os.Executable().
func NewAider() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&aider{path: filepath.Join(home, ".aider.conf.yml")}), nil
}

type aider struct {
	path string
}

func (a *aider) Name() string       { return "aider" }
func (a *aider) ConfigPath() string { return a.path }

// IsRelayStdio reports true: aider's documented MCP schema is stdio-only
// ({name, command, args, env}) with no verifiable url/http server form, so
// AddEntry requires relay context (RelayExePath, plus RelayServer/RelayDaemon
// for the manifest-lookup form) and rejects a URL-only entry — same contract
// as Antigravity. See the NewAider doc comment for the full evidence trail.
func (a *aider) IsRelayStdio() bool { return true }

func (a *aider) Exists() bool {
	_, err := os.Stat(a.path)
	return err == nil
}

func (a *aider) Backup() (string, error) {
	return writeBackup(a.path, a.Name(), 0)
}

func (a *aider) BackupKeep(keepN int) (string, error) {
	return writeBackup(a.path, a.Name(), keepN)
}

// InitEmpty seeds ~/.aider.conf.yml with an empty `mcp-server: []` list if
// the file is absent. The key is declared with an empty SEQUENCE (not a map)
// because aider's `mcp-server` is a list — a user inspecting the stub sees
// exactly the list shape AddEntry appends to. Aider reads many other settings
// from the same file, but because InitEmpty fires only when the file is
// missing, no user-authored configuration can be clobbered.
func (a *aider) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(a.path, []byte("mcp-server: []\n"))
}

func (a *aider) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(a.path, data)
}

// readYAML / writeYAML round-trip through map[string]any so unknown top-level
// keys (model, auto-commits, etc.) survive.
func (a *aider) readYAML() (map[string]any, error) {
	data, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", a.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (a *aider) writeYAML(m map[string]any) error {
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative + DACL-bound) for
	// token-bearing rewrites; tests get the os.WriteFile fallback.
	return WriteConfigFile(a.path, out)
}

// aiderServerList reads the top-level `mcp-server` value and returns it as the
// raw slice of parsed list elements. Non-list values (absent, scalar, mapping)
// yield a nil slice. yaml.v3 decodes a sequence into []any.
func aiderServerList(m map[string]any) []any {
	list, ok := m["mcp-server"].([]any)
	if !ok {
		return nil
	}
	return list
}

// aiderEntryName returns the `name` field of a parsed list element, or "" when
// the element is not a mapping or has no string name. This is the identity key
// for an aider mcp-server list entry (the list has no map key — name lives
// INSIDE each object).
func aiderEntryName(raw any) string {
	em := asStringMap(raw)
	if em == nil {
		return ""
	}
	name, _ := em["name"].(string)
	return name
}

// aiderListToNameMap projects the aider `mcp-server` LIST into the name-keyed
// map[string]any shape every shared helper (collectStdioEntries,
// findLanguageServerStdioInMap, isHubRelayShapeEntry) expects. Each element's
// `name` field becomes the map key; the element object itself becomes the
// value. Elements without a string name are skipped. On a duplicate name the
// last wins — matching the map semantics the other adapters' on-disk format
// enforces structurally.
func aiderListToNameMap(list []any) map[string]any {
	out := make(map[string]any, len(list))
	for _, raw := range list {
		name := aiderEntryName(raw)
		if name == "" {
			continue
		}
		em := asStringMap(raw)
		if em == nil {
			continue
		}
		out[name] = em
	}
	return out
}

func (a *aider) AddEntry(entry MCPEntry) error {
	if entry.RelayExePath == "" {
		return fmt.Errorf("aider adapter requires MCPEntry.RelayExePath (absolute path to mcphub.exe for the 'command' field)")
	}
	if !filepath.IsAbs(entry.RelayExePath) {
		return fmt.Errorf("aider adapter requires MCPEntry.RelayExePath to be absolute (got %q)", entry.RelayExePath)
	}
	// Two relay invocation shapes — identical contract to the Antigravity
	// adapter. When RelayURL is set (serena dynamic-pool client-reconcile),
	// the relay forwards to a fixed URL via its --url escape hatch (mutually
	// exclusive with --server/--daemon). Otherwise the manifest-lookup form
	// requires RelayServer + RelayDaemon.
	var relayArgs []any
	if entry.RelayURL != "" {
		relayArgs = []any{"relay", "--url", entry.RelayURL}
	} else {
		if entry.RelayServer == "" || entry.RelayDaemon == "" {
			return fmt.Errorf("aider adapter requires MCPEntry.RelayServer and RelayDaemon (aider's documented MCP schema is stdio-only; relay spawner bridges to the shared HTTP daemon), or set MCPEntry.RelayURL for a direct relay target")
		}
		relayArgs = []any{"relay", "--server", entry.RelayServer, "--daemon", entry.RelayDaemon}
	}
	serverEntry := map[string]any{
		"name":    entry.Name,
		"command": entry.RelayExePath,
		"args":    relayArgs,
	}

	m, err := a.readYAML()
	if err != nil {
		return err
	}
	list := aiderServerList(m)
	// Replace in place if an element with this name already exists; else
	// append. Other list elements (and their order) are preserved.
	replaced := false
	for i, raw := range list {
		if aiderEntryName(raw) == entry.Name {
			list[i] = serverEntry
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, serverEntry)
	}
	m["mcp-server"] = list
	return a.writeYAML(m)
}

func (a *aider) RemoveEntry(name string) error {
	m, err := a.readYAML()
	if err != nil {
		return err
	}
	list := aiderServerList(m)
	if list == nil {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, raw := range list {
		if aiderEntryName(raw) == name {
			continue
		}
		out = append(out, raw)
	}
	m["mcp-server"] = out
	return a.writeYAML(m)
}

func (a *aider) GetEntry(name string) (*MCPEntry, error) {
	m, err := a.readYAML()
	if err != nil {
		return nil, err
	}
	for _, raw := range aiderServerList(m) {
		if aiderEntryName(raw) != name {
			continue
		}
		em := asStringMap(raw)
		if em == nil {
			return nil, nil
		}
		// The stdio entry stores `command`/`args`, not a URL. Reconstruct
		// the relay identifiers for diagnostic parity with antigravity.
		e := &MCPEntry{Name: name}
		if cmd, _ := em["command"].(string); cmd != "" {
			e.RelayExePath = cmd
		}
		if argsAny, ok := em["args"].([]any); ok {
			for i, v := range argsAny {
				s, _ := v.(string)
				switch s {
				case "--server":
					if i+1 < len(argsAny) {
						e.RelayServer, _ = argsAny[i+1].(string)
					}
				case "--daemon":
					if i+1 < len(argsAny) {
						e.RelayDaemon, _ = argsAny[i+1].(string)
					}
				case "--url":
					if i+1 < len(argsAny) {
						e.RelayURL, _ = argsAny[i+1].(string)
					}
				}
			}
		}
		return e, nil
	}
	return nil, nil
}

// LatestBackupPath delegates to the shared helper.
func (a *aider) LatestBackupPath() (string, bool, error) {
	return latestBackup(a.path, a.Name())
}

// RestoreEntryFromBackup reads the YAML backup, extracts the mcp-server list
// element whose `name` == name (if present), and writes it over the live
// config's corresponding element. Other list elements are left untouched.
//
// Defensively refuses if the backup's copy of the named entry is already in
// hub-managed (relay) shape — `command` is the mcphub binary AND args[0] ==
// "relay" — see ErrBackupEntryAlreadyMigrated. Aider's hub-managed form is the
// RELAY entry (like Antigravity), NOT a url entry.
func (a *aider) RestoreEntryFromBackup(backupPath, name string) error {
	return a.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc on
// Client.RestoreEntryFromBackupForRollback). Used only by the serena
// dynamic-pool migrate abort-rollback.
func (a *aider) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return a.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a hub-relay-shaped backup entry with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes the
// backup element verbatim regardless of shape.
func (a *aider) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
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

	// Locate the named element in the backup list.
	var backupEntry any
	backupHas := false
	for _, raw := range aiderServerList(backupMap) {
		if aiderEntryName(raw) == name {
			backupEntry = raw
			backupHas = true
			break
		}
	}

	liveMap, err := a.readYAML()
	if err != nil {
		return err
	}
	liveList := aiderServerList(liveMap)

	if backupHas {
		// Defensive: refuse hub-relay-shaped backup entries for aider
		// (command == mcphub binary, args[0] == "relay"). Pre-hub stdio
		// entries (other commands) pass through. The rollback caller
		// (allowHubEntry=true) bypasses this guard to restore the
		// pre-reconcile legacy hub entry verbatim.
		if !allowHubEntry {
			if rawMap := asStringMap(backupEntry); rawMap != nil {
				if isHubRelayShapeEntry(rawMap) {
					return ErrBackupEntryAlreadyMigrated
				}
			}
		}
		// Replace in place if present in live, else append. Preserve order.
		replaced := false
		for i, raw := range liveList {
			if aiderEntryName(raw) == name {
				liveList[i] = backupEntry
				replaced = true
				break
			}
		}
		if !replaced {
			liveList = append(liveList, backupEntry)
		}
		liveMap["mcp-server"] = liveList
		return a.writeYAML(liveMap)
	}

	// Backup lacks the entry — remove it from live (migrate added it from
	// scratch and there was no prior entry).
	out := make([]any, 0, len(liveList))
	for _, raw := range liveList {
		if aiderEntryName(raw) == name {
			continue
		}
		out = append(out, raw)
	}
	liveMap["mcp-server"] = out
	return a.writeYAML(liveMap)
}

// AllStdioEntries returns every stdio entry from the mcp-server list.
func (a *aider) AllStdioEntries() ([]StdioEntry, error) {
	m, err := a.readYAML()
	if err != nil {
		return nil, err
	}
	return collectStdioEntries(aiderListToNameMap(aiderServerList(m))), nil
}

// FindStdioLanguageServerEntries scans the mcp-server list for stdio entries
// matching the mcp-language-server invocation pattern.
func (a *aider) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := a.readYAML()
	if err != nil {
		return nil, err
	}
	return findLanguageServerStdioInMap(aiderListToNameMap(aiderServerList(m))), nil
}

// BackupContainsEntry reports whether the backup file at backupPath has an
// mcp-server list element named `name`.
func (a *aider) BackupContainsEntry(backupPath, name string) (bool, error) {
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
	for _, raw := range aiderServerList(m) {
		if aiderEntryName(raw) != name {
			continue
		}
		// Require the element to be a mapping. A scalar value would be
		// malformed at this path; treat as absent so sentinel fallback
		// refuses rather than silently writes corrupted data via
		// RestoreEntryFromBackup.
		return asStringMap(raw) != nil, nil
	}
	return false, nil
}

// BackupEntryIsHubManaged reports whether the mcp-server list element named
// `name` in the YAML backup at backupPath is in aider's hub-managed (relay)
// shape: `command` is the mcphub binary AND args[0] == "relay". See
// Client.BackupEntryIsHubManaged.
func (a *aider) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
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
	for _, raw := range aiderServerList(m) {
		if aiderEntryName(raw) != name {
			continue
		}
		em := asStringMap(raw)
		if em == nil {
			return false, nil
		}
		return isHubRelayShapeEntry(em), nil
	}
	return false, nil
}

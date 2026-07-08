package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewPochi returns a Client bound to Pochi's user-level MCP config at
// ~/config.json.
//
// Pochi (getpochi — a VS Code extension + CLI AI assistant) stores MCP servers
// in a JSON file edited via its "Pochi: Open MCP Server Settings" command, under
// the top-level key `mcp` (NOT the JSON family's `mcpServers`) — an object map
// of named entries. Pochi's documented entry form is STDIO ONLY: each server has
// `command` (with optional `args`/`cwd`); the docs show no `url`/HTTP entry form
// for the hand-edited config. So Pochi cannot consume a loopback-HTTP entry the
// way the URL-native adapters do.
//
// Workaround (same as the Antigravity / Pi adapters): mcp-local-hub writes a
// STDIO entry that invokes our own `mcphub relay` subcommand. Pochi spawns the
// relay as its child, the relay connects to the shared HTTP daemon, and Pochi
// transparently benefits from the shared-daemon architecture. This makes Pochi a
// RELAY-STDIO adapter — IsRelayStdio() returns true and AddEntry REQUIRES relay
// context (RelayExePath, plus RelayServer/RelayDaemon, or RelayURL).
//
// Entry shape written:
//
//	"mcp": {
//	  "<server-name>": {
//	    "command": "<abs-path>/mcphub.exe",
//	    "args": ["relay", "--server", "<s>", "--daemon", "<d>"],
//	    "disabled": false
//	  }
//	}
//
// (or `args: ["relay", "--url", "<url>"]` when MCPEntry.RelayURL is set). This
// is the Antigravity/Pi relay-stdio shape under Pochi's `mcp` key. The embedded
// jsonMCPClient is parameterized with serversKey "mcp" and urlField "command",
// so the relay-shape hub-managed detection (isHubRelayShapeEntry) and the
// inherited RemoveEntry/restore/predicate helpers behave exactly as they do for
// Pi — only AddEntry, GetEntry, IsRelayStdio, and the bootstrap overrides are
// dedicated.
//
// Path note: Pochi's docs say the MCP-settings command modifies `~/config.json`
// (the bare home config.json, not a dot-dir) — that home-anchored path is what
// the registry's configPath(home) contract targets.
//
// Source (verified 2026-06-17):
//   - https://docs.getpochi.com/tutorials/supabase-mcp-server/ — MCP config
//     edited via "Pochi: Open MCP Server Settings" -> ~/config.json; top-level
//     `mcp` object map; documented entry shape is stdio command/args/cwd.
func NewPochi() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, "config.json"),
		clientName: "pochi",
		serversKey: "mcp",
		// urlField "command" routes the hub-shape detection to the relay-shape
		// branch (isHubRelayShapeEntry), matching Antigravity/Pi.
		urlField: "command",
	}
	return newLockingClient(&pochiClient{jsonMCPClient: base}), nil
}

// pochiClient overrides AddEntry/GetEntry/IsRelayStdio/Exists/Backup to emit and
// read stdio-relay entries under the `mcp` key. Restore, RemoveEntry, InitEmpty,
// and every backup/demigrate helper are promoted from the embedded jsonMCPClient
// (serversKey "mcp") unchanged.
type pochiClient struct {
	*jsonMCPClient
}

// IsRelayStdio reports true: Pochi only documents stdio entries, so AddEntry
// requires relay context and rejects a URL-only entry.
func (p *pochiClient) IsRelayStdio() bool { return true }

// Exists reports Pochi as present only when the generic ~/config.json file
// already has Pochi's top-level `mcp` object. The path itself is too generic to
// prove ownership: unrelated tools can also use ~/config.json, and treating mere
// file existence as installation would let auto-targeting flows mutate another
// application's config.
func (p *pochiClient) Exists() bool {
	ok, err := p.hasPochiMCPSection()
	return err == nil && ok
}

func (p *pochiClient) hasPochiMCPSection() (bool, error) {
	if _, err := os.Stat(p.path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	m, err := p.readJSON()
	if err != nil {
		return false, err
	}
	_, ok := m[p.sectionKey()].(map[string]any)
	return ok, nil
}

func (p *pochiClient) requirePochiMCPSectionIfFileExists() error {
	ok, err := p.hasPochiMCPSection()
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if _, err := os.Stat(p.path); err == nil {
		return fmt.Errorf("refusing to modify %s as Pochi config: existing file lacks top-level %q object", p.path, p.sectionKey())
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (p *pochiClient) Backup() (string, error) {
	return p.BackupKeep(0)
}

// BackupKeep seeds an empty `{"mcp": {}}` stub (via the parameterized
// InitEmpty) if the config is absent, then writes the timestamped backup. The
// parent (home dir) always exists, so no MkdirAll is needed.
func (p *pochiClient) BackupKeep(keepN int) (string, error) {
	if err := p.requirePochiMCPSectionIfFileExists(); err != nil {
		return "", err
	}
	if _, err := p.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(p.path, p.Name(), keepN)
}

// AddEntry writes the hub-managed stdio-relay entry under mcp.<name>. Mirrors
// piClient.AddEntry: requires an absolute RelayExePath and either RelayURL
// (direct form) or RelayServer+RelayDaemon (manifest-lookup form).
func (p *pochiClient) AddEntry(entry MCPEntry) error {
	if err := p.requirePochiMCPSectionIfFileExists(); err != nil {
		return err
	}
	if entry.RelayExePath == "" {
		return fmt.Errorf("pochi adapter requires MCPEntry.RelayExePath (absolute path to mcphub.exe for the 'command' field)")
	}
	if !filepath.IsAbs(entry.RelayExePath) {
		return fmt.Errorf("pochi adapter requires MCPEntry.RelayExePath to be absolute (got %q)", entry.RelayExePath)
	}
	var relayArgs []string
	if entry.RelayURL != "" {
		relayArgs = []string{"relay", "--url", entry.RelayURL}
	} else {
		if entry.RelayServer == "" || entry.RelayDaemon == "" {
			return fmt.Errorf("pochi adapter requires MCPEntry.RelayServer and RelayDaemon (Pochi only accepts stdio entries; the relay spawner bridges to the shared HTTP daemon), or set MCPEntry.RelayURL for a direct relay target")
		}
		relayArgs = []string{"relay", "--server", entry.RelayServer, "--daemon", entry.RelayDaemon}
	}
	serverEntry := map[string]any{
		"command":  entry.RelayExePath,
		"args":     relayArgs,
		"disabled": false,
	}
	return p.setMember(entry.Name, serverEntry)
}

// GetEntry returns a minimal MCPEntry reconstructing the relay args from the
// stored `command`/`args` (for diagnostics). Mirrors piClient.GetEntry, reading
// from the parameterized `mcp` section key.
func (p *pochiClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := p.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[p.sectionKey()].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	e := &MCPEntry{Name: name, Disabled: mcpEntryDisabled(raw)}
	if cmd, _ := raw["command"].(string); cmd != "" {
		e.RelayExePath = cmd
	}
	if argsAny, ok := raw["args"].([]any); ok {
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

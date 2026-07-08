package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewPi returns a Client bound to the Pi coding agent's user-level MCP
// config at ~/.pi/agent/mcp.json.
//
// Pi (pi-coding-agent on PyPI; mariozechner/pi) reads MCP servers from a
// JSON file under the canonical top-level `mcpServers` key. Pi's documented
// MCP entry form is STDIO ONLY — every example uses a `command` (with
// optional `args`/`env`); the docs show NO `url`/HTTP entry form. So Pi
// cannot consume a loopback-HTTP entry the way the URL-native adapters do.
//
// Workaround (same as the Antigravity adapter): mcp-local-hub writes a STDIO
// entry that invokes our own `mcphub relay` subcommand. Pi spawns the relay
// as its child process, the relay connects to the shared HTTP daemon, and Pi
// transparently benefits from the shared-daemon architecture. This makes Pi
// a RELAY-STDIO adapter — IsRelayStdio() returns true and AddEntry REQUIRES
// relay context (RelayExePath, plus RelayServer/RelayDaemon for the
// manifest-lookup form, or RelayURL for the direct form).
//
// Entry shape written:
//
//	"<server-name>": {
//	  "command": "<abs-path>/mcphub.exe",
//	  "args": ["relay", "--server", "<s>", "--daemon", "<d>"],
//	  "disabled": false
//	}
//
// (or `args: ["relay", "--url", "<url>"]` when MCPEntry.RelayURL is set, for
// the serena dynamic-pool router endpoint). This is byte-for-byte the
// Antigravity relay-stdio shape; the only difference is the config path
// (~/.pi/agent/mcp.json vs ~/.gemini/antigravity/mcp_config.json) and the
// client name. The embedded jsonMCPClient with urlField "command" makes the
// relay-shape hub-managed detection (isHubRelayShapeEntry) and the inherited
// GetEntry/restore/predicate helpers behave exactly as they do for
// Antigravity.
//
// Sources (verified 2026-06-17):
//   - https://pi.dev/packages/context-mode — user config ~/.pi/agent/mcp.json;
//     top-level `mcpServers`; documented entry shape is stdio
//     `{"command": ...}` only (no `url`/HTTP entry form shown), so the hub
//     bridges via the `mcphub relay` stdio subcommand.
func NewPi() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".pi", "agent", "mcp.json"),
		clientName: "pi",
		// urlField stays nominal — Pi stores `command`/`args`, not a URL, so
		// the base readers/writers that reference urlField are never
		// exercised for the URL path; "command" routes the hub-shape
		// detection to the relay-shape branch (isHubRelayShapeEntry), matching
		// Antigravity.
		urlField: "command",
	}
	return newLockingClient(&piClient{jsonMCPClient: base}), nil
}

// piClient overrides AddEntry/GetEntry/Exists/Backup/BackupKeep to emit and
// read stdio-relay entries (and to seed a fresh ~/.pi/agent dir). Restore,
// RemoveEntry, Name, ConfigPath, InitEmpty, and every backup/demigrate helper
// are promoted from the embedded jsonMCPClient unchanged.
type piClient struct {
	*jsonMCPClient
}

// IsRelayStdio reports true: Pi only documents stdio entries, so AddEntry
// requires relay context (RelayExePath, plus RelayServer/RelayDaemon for the
// manifest-lookup form) and rejects a URL-only entry. Overrides the embedded
// jsonMCPClient's default false.
func (p *piClient) IsRelayStdio() bool { return true }

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.pi/agent) does — mirroring the cursor/copilot
// "directory means installed" heuristic.
func (p *piClient) Exists() bool {
	if _, err := os.Stat(p.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(p.path))
	return err == nil && st.IsDir()
}

func (p *piClient) Backup() (string, error) {
	return p.BackupKeep(0)
}

// BackupKeep ensures the ~/.pi/agent parent directory exists, seeds an empty
// `{"mcpServers": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The nested parent dir may not exist
// on a clean install, so the MkdirAll here is load-bearing.
func (p *piClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(p.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := p.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(p.path, p.Name(), keepN)
}

// AddEntry writes the hub-managed stdio-relay entry under mcpServers.<name>.
// Mirrors antigravityClient.AddEntry exactly: it requires an absolute
// RelayExePath and either RelayURL (direct form) or RelayServer+RelayDaemon
// (manifest-lookup form), and rejects a URL-only entry (Pi cannot consume a
// loopback-HTTP entry).
func (p *piClient) AddEntry(entry MCPEntry) error {
	if entry.RelayExePath == "" {
		return fmt.Errorf("pi adapter requires MCPEntry.RelayExePath (absolute path to mcphub.exe for the 'command' field)")
	}
	if !filepath.IsAbs(entry.RelayExePath) {
		return fmt.Errorf("pi adapter requires MCPEntry.RelayExePath to be absolute (got %q)", entry.RelayExePath)
	}
	var relayArgs []string
	if entry.RelayURL != "" {
		relayArgs = []string{"relay", "--url", entry.RelayURL}
	} else {
		if entry.RelayServer == "" || entry.RelayDaemon == "" {
			return fmt.Errorf("pi adapter requires MCPEntry.RelayServer and RelayDaemon (Pi only accepts stdio entries; the relay spawner bridges to the shared HTTP daemon), or set MCPEntry.RelayURL for a direct relay target")
		}
		relayArgs = []string{"relay", "--server", entry.RelayServer, "--daemon", entry.RelayDaemon}
	}
	serverEntry := map[string]any{
		"command":  entry.RelayExePath,
		"args":     relayArgs,
		"disabled": false,
	}
	// Comment-preserving set via the embedded jsonMCPClient seam: patches
	// mcpServers.<name> into the original bytes so any operator comments +
	// unrelated keys survive. RemoveEntry/Restore are promoted unchanged.
	return p.setMember(entry.Name, serverEntry)
}

// GetEntry returns a minimal MCPEntry reconstructing the relay args from the
// stored `command`/`args` (for diagnostics). Mirrors antigravityClient.GetEntry.
func (p *piClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := p.readJSON()
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

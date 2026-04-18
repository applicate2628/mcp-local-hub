package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewAntigravity returns a Client bound to ~/.gemini/antigravity/mcp_config.json.
//
// Antigravity is a Gemini-CLI fork shipped inside Google's Antigravity IDE
// (Cascade agent). As of April 2026 its RefreshMcpServers loader silently
// drops any loopback-HTTP MCP entry regardless of schema — both
// {url,type:"http",timeout} (Gemini-CLI shape) and {serverUrl,disabled}
// (Antigravity-native shape, confirmed via the working context7 entry
// pointing at remote HTTPS). Only remote HTTPS is accepted over HTTP
// transport; localhost is rejected.
//
// Workaround: mcp-local-hub writes a STDIO entry invoking our own
// `mcp relay` subcommand. Antigravity's agent spawns the relay as its
// child process, relay connects to the shared HTTP daemon on 9121, and
// Antigravity transparently benefits from the shared-daemon architecture
// like Claude Code / Codex CLI / Gemini CLI.
//
// Entry shape written:
//
//	"<server-name>": {
//	  "command": "<abs-path>/mcphub.exe",
//	  "args": ["relay", "--server", "<s>", "--daemon", "<d>"],
//	  "disabled": false
//	}
//
// Requires MCPEntry.RelayServer, RelayDaemon, and RelayExePath to be set
// by the caller (install.go populates them from manifest + os.Executable()).
func NewAntigravity() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		clientName: "antigravity",
		// urlField stays nominal — Antigravity stores `command`/`args`, not a URL,
		// so base readers/writers that reference urlField are never exercised.
		urlField: "command",
	}
	return &antigravityClient{jsonMCPClient: base}, nil
}

// antigravityClient overrides AddEntry/GetEntry to emit stdio-relay
// entries. Backup, Restore, RemoveEntry, Name, ConfigPath, Exists are
// promoted from the embedded jsonMCPClient unchanged.
type antigravityClient struct {
	*jsonMCPClient
}

func (a *antigravityClient) AddEntry(entry MCPEntry) error {
	if entry.RelayServer == "" || entry.RelayDaemon == "" {
		return fmt.Errorf("antigravity adapter requires MCPEntry.RelayServer and RelayDaemon (Cascade only accepts stdio entries for localhost MCP; relay spawner is used to bridge to the shared HTTP daemon)")
	}
	if entry.RelayExePath == "" {
		return fmt.Errorf("antigravity adapter requires MCPEntry.RelayExePath (absolute path to mcphub.exe for the 'command' field)")
	}
	m, err := a.readJSON()
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	serverEntry := map[string]any{
		"command":  entry.RelayExePath,
		"args":     []string{"relay", "--server", entry.RelayServer, "--daemon", entry.RelayDaemon},
		"disabled": false,
	}
	servers[entry.Name] = serverEntry
	m["mcpServers"] = servers
	return a.writeJSON(m)
}

// GetEntry returns a minimal MCPEntry with just Name populated. The
// stdio entry shape stores `command`/`args` rather than a URL, so the
// URL field cannot be reconstructed without re-reading the manifest.
// Callers that need URL diagnostics should consult the manifest or the
// running daemon status directly.
func (a *antigravityClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := a.readJSON()
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
	// Reconstruct relay args if present, for debugging convenience.
	e := &MCPEntry{Name: name}
	if cmd, _ := raw["command"].(string); cmd != "" {
		e.RelayExePath = cmd
	}
	if argsAny, ok := raw["args"].([]any); ok {
		// Pull RelayServer/RelayDaemon back out by position — our writer
		// always produces [relay, --server, <s>, --daemon, <d>].
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
			}
		}
	}
	return e, nil
}

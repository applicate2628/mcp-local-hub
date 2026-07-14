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
	return newLockingClient(&antigravityClient{jsonMCPClient: base}), nil
}

// antigravityClient overrides AddEntry/GetEntry to emit stdio-relay
// entries. Backup, Restore, RemoveEntry, Name, ConfigPath, Exists are
// promoted from the embedded jsonMCPClient unchanged.
type antigravityClient struct {
	*jsonMCPClient
}

// IsRelayStdio reports true: Antigravity (Gemini Cascade) only accepts
// stdio entries for localhost MCP, so AddEntry below requires relay context
// (RelayExePath, plus RelayServer/RelayDaemon for the manifest-lookup form)
// and rejects a URL-only entry. Overrides the embedded jsonMCPClient's
// default false.
func (a *antigravityClient) IsRelayStdio() bool { return true }

func (a *antigravityClient) AddEntry(entry MCPEntry) error {
	return a.AddEntryWithConfigWriter(entry, nil)
}

func (a *antigravityClient) AddEntryWithConfigWriter(entry MCPEntry, writer WriteConfigFileFunc) error {
	if entry.RelayExePath == "" {
		return fmt.Errorf("antigravity adapter requires MCPEntry.RelayExePath (absolute path to mcphub.exe for the 'command' field)")
	}
	if !filepath.IsAbs(entry.RelayExePath) {
		return fmt.Errorf("antigravity adapter requires MCPEntry.RelayExePath to be absolute (got %q)", entry.RelayExePath)
	}
	// Two relay invocation shapes. When RelayURL is set (serena
	// dynamic-pool client-reconcile — design §5), the relay forwards to
	// a fixed URL via its --url escape hatch; the constant /serena/mcp
	// router has no per-daemon manifest port to resolve, so the
	// manifest-lookup --server/--daemon form cannot be used (and is
	// mutually exclusive with --url anyway — see resolveRelayURL). When
	// RelayURL is empty, preserve the legacy manifest-lookup form, which
	// requires RelayServer + RelayDaemon.
	var relayArgs []string
	if entry.RelayURL != "" {
		relayArgs = []string{"relay", "--url", entry.RelayURL}
	} else {
		if entry.RelayServer == "" || entry.RelayDaemon == "" {
			return fmt.Errorf("antigravity adapter requires MCPEntry.RelayServer and RelayDaemon (Cascade only accepts stdio entries for localhost MCP; relay spawner is used to bridge to the shared HTTP daemon), or set MCPEntry.RelayURL for a direct relay target")
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
	// unrelated keys in mcp_config.json survive (Antigravity's config is the
	// same JSONC-tolerant family). RemoveEntry/Restore are promoted unchanged.
	return a.setMemberWithWriter(entry.Name, serverEntry, writer)
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
	return classifyAntigravityRawEntry(name, raw), nil
}

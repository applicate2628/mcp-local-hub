package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewZencoder returns a Client bound to Zencoder's MCP config, which lives in
// the host editor's VS Code User settings.json under the namespaced flat key
// `zencoder.mcpServers`.
//
// Zencoder (an AI coding agent; VS Code + JetBrains plugin with a 100+ server
// MCP Library) is primarily managed through its in-app Agent Tools UI, but it IS
// file-configurable in VS Code via the dotted key `zencoder.mcpServers` inside
// settings.json. (Zenflow — Zencoder's multi-agent orchestration product — is
// NOT a separate vendor: it shares this exact config surface and MCP Library, so
// there is no distinct `zenflow` adapter.)
//
// VS Code treats dotted setting keys as FLAT top-level string keys; the embedded
// jsonMCPClient parameterized with serversKey "zencoder.mcpServers" lands the
// entry under that flat dotted key via the comment-preserving JSONC seam,
// preserving the operator's other editor settings.
//
// Zencoder's documented hand-edited entry shape is STDIO ONLY — each server has
// `command`/`args`/`env`; HTTP-url support exists in the in-app Library but is
// NOT authoritatively documented for the hand-written settings.json form. So,
// conservatively, this is a RELAY-STDIO adapter: the hub writes a STDIO entry
// invoking our `mcphub relay` subcommand (which the documented stdio shape
// definitely accepts), rather than a raw url the hand-edit form may not consume.
// IsRelayStdio() returns true and AddEntry REQUIRES relay context.
//
// Entry shape written:
//
//	"zencoder.mcpServers": {
//	  "<server-name>": {
//	    "command": "<abs-path>/mcphub.exe",
//	    "args": ["relay", "--server", "<s>", "--daemon", "<d>"],
//	    "disabled": false
//	  }
//	}
//
// The file is shared with the `vscode` and `amp` adapters' host, but all three
// write DIFFERENT keys (vscode -> mcp.json `servers`; amp -> settings.json
// `amp.mcpServers`; zencoder -> settings.json `zencoder.mcpServers`), so none
// collide.
//
// Source (verified 2026-06-17):
//   - https://docs.zencoder.ai/features/mcp-deep-dive — `zencoder.mcpServers`
//     key in VS Code settings.json; documented hand-edit entries are stdio
//     command/args/env; HTTP support is via the in-app Library (not the
//     hand-written form), so the hub bridges via the `mcphub relay` stdio
//     subcommand.
func NewZencoder() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultVSCodeUserSettingsPath(home),
		clientName: "zencoder",
		serversKey: "zencoder.mcpServers",
		// urlField "command" routes hub-shape detection to the relay-shape
		// branch (isHubRelayShapeEntry), matching Antigravity/Pi/Pochi.
		urlField: "command",
	}
	return newLockingClient(&zencoderClient{jsonMCPClient: base}), nil
}

// zencoderClient overrides AddEntry/GetEntry/IsRelayStdio/Exists/Backup to emit
// and read stdio-relay entries under the `zencoder.mcpServers` key. Restore,
// RemoveEntry, InitEmpty, and every backup/demigrate helper are promoted from
// the embedded jsonMCPClient (serversKey "zencoder.mcpServers") unchanged.
type zencoderClient struct {
	*jsonMCPClient
}

// IsRelayStdio reports true: Zencoder's documented hand-edit form is stdio-only,
// so AddEntry requires relay context and rejects a URL-only entry.
func (z *zencoderClient) IsRelayStdio() bool { return true }

// Exists reports the client as present when the VS Code User settings.json file
// exists — Zencoder is a VS Code-hosted plugin, so the editor's settings file is
// the detectable signal.
func (z *zencoderClient) Exists() bool {
	_, err := os.Stat(z.path)
	return err == nil
}

func (z *zencoderClient) Backup() (string, error) {
	return z.BackupKeep(0)
}

// BackupKeep ensures the Code/User parent directory exists, seeds an empty
// `{"zencoder.mcpServers": {}}` stub (via the parameterized InitEmpty) only if
// settings.json is absent — so an existing editor settings.json is never
// clobbered — then writes the timestamped backup.
func (z *zencoderClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(z.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := z.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(z.path, z.Name(), keepN)
}

// AddEntry writes the hub-managed stdio-relay entry under
// zencoder.mcpServers.<name>. Mirrors piClient.AddEntry.
func (z *zencoderClient) AddEntry(entry MCPEntry) error {
	if entry.RelayExePath == "" {
		return fmt.Errorf("zencoder adapter requires MCPEntry.RelayExePath (absolute path to mcphub.exe for the 'command' field)")
	}
	if !filepath.IsAbs(entry.RelayExePath) {
		return fmt.Errorf("zencoder adapter requires MCPEntry.RelayExePath to be absolute (got %q)", entry.RelayExePath)
	}
	var relayArgs []string
	if entry.RelayURL != "" {
		relayArgs = []string{"relay", "--url", entry.RelayURL}
	} else {
		if entry.RelayServer == "" || entry.RelayDaemon == "" {
			return fmt.Errorf("zencoder adapter requires MCPEntry.RelayServer and RelayDaemon (Zencoder's hand-edit form accepts stdio entries; the relay spawner bridges to the shared HTTP daemon), or set MCPEntry.RelayURL for a direct relay target")
		}
		relayArgs = []string{"relay", "--server", entry.RelayServer, "--daemon", entry.RelayDaemon}
	}
	serverEntry := map[string]any{
		"command":  entry.RelayExePath,
		"args":     relayArgs,
		"disabled": false,
	}
	return z.setMember(entry.Name, serverEntry)
}

// GetEntry reconstructs a minimal MCPEntry from the stored `command`/`args`.
// Mirrors piClient.GetEntry, reading from the parameterized
// `zencoder.mcpServers` section key.
func (z *zencoderClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := z.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[z.sectionKey()].(map[string]any)
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

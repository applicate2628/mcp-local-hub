package clients

import (
	"os"
	"path/filepath"
	"runtime"
)

// defaultVSCodeUserSettingsPath returns the VS Code User settings.json path —
// the file Sourcegraph Amp and Zencoder store their namespaced MCP keys in
// (`amp.mcpServers` / `zencoder.mcpServers`). It is the SIBLING of VS Code's
// dedicated mcp.json (defaultVSCodeConfigPath, which the `vscode` adapter
// targets under its `servers` key): same `Code/User/` directory, different
// leaf. A home-dir lookup failure degrades to a bare "settings.json" relative
// path (fail-safe; Exists() on such a path simply reports absent). Takes `home`
// (the registry's configPath(home) contract) so ConfigPathForName and the amp /
// zencoder adapters resolve the same file.
func defaultVSCodeUserSettingsPath(home string) string {
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Code", "User", "settings.json")
		}
		if home == "" {
			return "settings.json"
		}
		return filepath.Join(home, "AppData", "Roaming", "Code", "User", "settings.json")
	case "darwin":
		if home == "" {
			return "settings.json"
		}
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "settings.json")
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "Code", "User", "settings.json")
		}
		if home == "" {
			return "settings.json"
		}
		return filepath.Join(home, ".config", "Code", "User", "settings.json")
	}
}

// NewAmp returns a Client bound to Sourcegraph Amp's MCP config, which lives in
// the host editor's VS Code User settings.json under the namespaced flat key
// `amp.mcpServers`.
//
// Amp (Sourcegraph's agentic coding tool) does not use a standalone
// {mcpServers:{...}} mcp.json; instead its MCP servers are configured in VS Code
// settings.json under the dotted key `amp.mcpServers` (or via `amp mcp add`).
// VS Code treats dotted setting keys as FLAT top-level string keys, and the
// comment-preserving JSONC write seam (jsonc.go) builds a JSON Pointer where the
// dot is one literal key character (only `/` separates pointer tokens) — so the
// embedded jsonMCPClient parameterized with serversKey "amp.mcpServers" lands
// the entry under the flat dotted key exactly as VS Code requires, while
// preserving the rest of the operator's editor settings + comments.
//
// Each Amp entry is either `{command,args,env}` (stdio) OR `{url:...}` (HTTP).
// Amp speaks HTTP MCP, so the hub writes a URL-direct entry (the loopback
// daemon URL) and IsRelayStdio() is false (inherited). The entry is written
// minimally as `{"url": "..."}` (+ optional `headers`) to match Amp's
// documented shape — no `type`/`disabled` keys, which Amp's stdio-or-url schema
// does not list.
//
// Path note: Amp's MCP settings are GLOBAL (editor User settings.json); Amp does
// not yet support a project-local mcp.json. The file is shared with the `vscode`
// adapter's host — but they write DIFFERENT keys (vscode -> a dedicated mcp.json
// `servers`; amp -> settings.json `amp.mcpServers`), so the two never collide.
//
// Source (verified 2026-06-17):
//   - https://github.com/sourcegraph/amp-examples-and-guides/blob/main/guides/amp-mcp-setup-guide.md
//     — MCP servers configured in VS Code settings.json under `amp.mcpServers`
//     (or `amp mcp add`); each entry is stdio {command,args,env} or HTTP {url}.
func NewAmp() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       defaultVSCodeUserSettingsPath(home),
		clientName: "amp",
		serversKey: "amp.mcpServers",
		urlField:   "url",
	}
	return newLockingClient(&ampClient{jsonMCPClient: base}), nil
}

// ampClient overrides AddEntry (to write Amp's minimal `{url}` shape — the base
// would add a `disabled` key Amp's schema does not document) and Exists (the
// config file is VS Code's shared settings.json). Every other method is promoted
// from the embedded jsonMCPClient (serversKey "amp.mcpServers") unchanged — the
// comment-preserving setMember/deleteMember are exactly what a shared editor
// settings.json needs.
type ampClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when the VS Code User settings.json file
// exists — Amp is a VS Code-hosted tool, so the presence of the editor's
// settings file is the detectable signal. (Unlike the dot-dir adapters, there is
// no Amp-specific parent directory to probe; the file is the host editor's.)
func (a *ampClient) Exists() bool {
	_, err := os.Stat(a.path)
	return err == nil
}

func (a *ampClient) Backup() (string, error) {
	return a.BackupKeep(0)
}

// BackupKeep ensures the Code/User parent directory exists, seeds an empty
// `{"amp.mcpServers": {}}` stub (via the parameterized InitEmpty) only if the
// settings.json is absent — so an existing editor settings.json is never
// clobbered — then writes the timestamped backup.
func (a *ampClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(a.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := a.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(a.path, a.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under `amp.mcpServers` as Amp's
// minimal HTTP shape `{"url": "..."}` (plus optional `headers`). The base
// jsonMCPClient.AddEntry would add a `disabled:false` key that Amp's documented
// stdio-or-url entry schema does not list, so this override is required.
func (a *ampClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"url": entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	return a.setMember(entry.Name, serverEntry)
}

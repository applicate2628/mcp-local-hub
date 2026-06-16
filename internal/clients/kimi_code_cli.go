package clients

import (
	"os"
	"path/filepath"
)

// NewKimiCodeCLI returns a Client bound to Moonshot AI's Kimi Code CLI
// user-level (global) MCP config at <KIMI_CODE_HOME or ~/.kimi-code>/mcp.json.
//
// Kimi Code CLI (Moonshot AI; pypi kimi-code / kimi-cli) reads MCP servers
// from a JSON file under the top-level `mcpServers` key — the canonical JSON
// family schema. Per the docs the transport is inferred from the entry's
// fields: an entry with a `command` field is a stdio server; an entry with a
// `url` field and no `transport` is an HTTP server (a legacy SSE server adds
// `"transport": "sse"`). There is NO `type` discriminator. Kimi Code speaks
// HTTP MCP natively, so the hub writes an HTTP-direct entry (no relay shim
// needed).
//
// The hub-managed remote entry shape written is therefore:
//
//	"<server-name>": {
//	  "url": "http://localhost:9121/mcp"
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty. The
// docs show neither a `type` nor a `disabled` field for HTTP entries, so
// AddEntry emits the minimal `{url}` (+ headers) shape rather than the base
// jsonMCPClient.AddEntry's `{url, disabled:false}`.
//
// Because the endpoint field is the standard `url` key, the embedded
// jsonMCPClient's hub-shape detection (isHubURLShapeEntry(_, "url")) and the
// inherited GetEntry / RemoveEntry / RestoreEntryFromBackup /
// BackupEntryIsHubManaged helpers all work unchanged with urlField "url" —
// only AddEntry (the minimal docs shape) and the seed/Exists overrides need
// dedicated implementations.
//
// KIMI_CODE_HOME override — when the KIMI_CODE_HOME environment variable is
// set, it replaces the default ~/.kimi-code directory (the config file then
// lives at $KIMI_CODE_HOME/mcp.json). This mirrors how Kimi Code CLI itself
// locates its home directory.
//
// Scope note: the hub writes the GLOBAL (user-level) file. Kimi Code also reads
// a project-level <project>/.kimi-code/mcp.json which the hub does NOT touch.
//
// Source (verified 2026-06-17):
//   - https://www.kimi.com/code/docs/en/kimi-code-cli/customization/mcp.html —
//     user config at ~/.kimi-code/mcp.json (or $KIMI_CODE_HOME/mcp.json);
//     top-level `mcpServers` key; HTTP entry `{"url": "https://..."}` ("entries
//     with a url field and no transport are HTTP servers"); legacy SSE adds
//     `"transport": "sse"`.
func NewKimiCodeCLI() (Client, error) {
	base := &jsonMCPClient{
		path:       defaultKimiCodeCLIConfigPath(),
		clientName: "kimi-code-cli",
		// Standard `url` endpoint key: the embedded base helpers (GetEntry,
		// hub-shape detection, restore guards) all key off this and need no
		// override.
		urlField: "url",
	}
	return newLockingClient(&kimiCodeCLIClient{jsonMCPClient: base}), nil
}

// defaultKimiCodeCLIConfigPath returns <KIMI_CODE_HOME or ~/.kimi-code>/mcp.json.
// KIMI_CODE_HOME, when set, replaces the ~/.kimi-code directory entirely. When
// it is unset, the path falls back to ~/.kimi-code under the user's home dir;
// a home-dir lookup failure degrades to a bare "mcp.json" relative path rather
// than panicking (matching the fail-safe posture AllClients uses to drop
// unconstructable adapters — Exists() on such a path simply reports absent).
func defaultKimiCodeCLIConfigPath() string {
	if h := os.Getenv("KIMI_CODE_HOME"); h != "" {
		return filepath.Join(h, "mcp.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "mcp.json"
	}
	return filepath.Join(home, ".kimi-code", "mcp.json")
}

// kimiCodeCLIClient overrides AddEntry (to emit the minimal documented `{url}`
// shape without the base's `disabled:false`) and the filesystem-bootstrap
// methods (Exists, Backup, BackupKeep) so a fresh host with no ~/.kimi-code
// directory still installs cleanly. GetEntry, RemoveEntry, Restore, InitEmpty,
// and every backup/demigrate helper are promoted from the embedded
// jsonMCPClient unchanged.
type kimiCodeCLIClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file exists OR
// its parent directory (~/.kimi-code or $KIMI_CODE_HOME) does — mirroring the
// cursor/kiro "directory means installed" heuristic so an operator who has
// Kimi Code CLI installed but no MCP config yet still gets the Initialize /
// install affordance.
func (k *kimiCodeCLIClient) Exists() bool {
	if _, err := os.Stat(k.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(k.path))
	return err == nil && st.IsDir()
}

func (k *kimiCodeCLIClient) Backup() (string, error) {
	return k.BackupKeep(0)
}

// BackupKeep ensures the ~/.kimi-code ($KIMI_CODE_HOME) parent directory
// exists, seeds an empty `{"mcpServers": {}}` stub if the config is absent,
// then writes the timestamped backup (pruning to keepN). The parent dir may
// not exist on a clean install, so the MkdirAll here is load-bearing —
// without it writeBackup/InitEmpty would fail on a fresh host.
func (k *kimiCodeCLIClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(k.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := k.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(k.path, k.Name(), keepN)
}

// AddEntry writes the hub-managed HTTP entry under the `mcpServers` map with
// Kimi Code's minimal documented shape: just `url` (+ optional `headers`).
// Omitting `transport` makes Kimi Code treat the entry as a (streamable) HTTP
// server. The base jsonMCPClient.AddEntry would add a `disabled:false` field
// the Kimi Code docs never show, so this override is required.
func (k *kimiCodeCLIClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"url": entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return k.setMember(entry.Name, serverEntry)
}

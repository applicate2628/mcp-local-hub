package clients

import (
	"os"
	"path/filepath"
)

// NewKiro returns a Client bound to Kiro's user-level MCP config at
// ~/.kiro/settings/mcp.json.
//
// Kiro (the AWS agentic IDE) reads MCP servers from two JSON files that
// share one schema: a user-level ~/.kiro/settings/mcp.json (servers
// available in every workspace) and a workspace-level
// <repo>/.kiro/settings/mcp.json (scoped to one project). The hub writes
// the user-level file, matching every other adapter's user-scoped
// posture; Kiro merges both at load time (workspace overrides user), so
// a per-user hub entry is visible in all workspaces. The file uses the
// canonical `{"mcpServers": {...}}` JSON family schema.
//
// Kiro speaks HTTP MCP natively (HTTP-direct, not relay-stdio): a remote
// server entry is `{"url": "https://...", ...}` with optional `headers`,
// `disabled`, `autoApprove`, `disabledTools`, `oauth`. Crucially the
// remote-server entry has NO `type` field (unlike Cursor/Gemini/VS Code,
// which write `"type": "http"`) — Kiro distinguishes remote from local
// by the presence of `url` vs `command`. So the plain base
// jsonMCPClient.AddEntry — which writes `{url, disabled:false}` under
// urlField — produces exactly Kiro's documented remote shape, and Kiro
// inherits AddEntry/GetEntry/RemoveEntry from the base unchanged.
//
// Source: Kiro docs, "Model Context Protocol (MCP) — Configuration"
// (https://kiro.dev/docs/mcp/configuration/): config paths
// ~/.kiro/settings/mcp.json + .kiro/settings/mcp.json, `mcpServers`
// top-level key, remote-server entry `{"url": ..., "headers": ...,
// "disabled": ..., "autoApprove": ..., "disabledTools": ...}` with no
// `type` field.
func NewKiro() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".kiro", "settings", "mcp.json"),
		clientName: "kiro",
		// Kiro's remote-server endpoint key is "url". The base
		// AddEntry writes {url, disabled:false}, which matches Kiro's
		// documented remote entry shape (no `type` field), so no
		// AddEntry/GetEntry override is needed.
		urlField: "url",
	}
	return &kiroClient{jsonMCPClient: base}, nil
}

// kiroClient overrides only the filesystem-bootstrap methods (Exists,
// Backup, BackupKeep, InitEmpty) so a fresh host with no ~/.kiro/settings
// directory still installs cleanly. AddEntry, GetEntry, RemoveEntry,
// Restore, and every backup/demigrate helper are promoted from the
// embedded jsonMCPClient unchanged.
type kiroClient struct {
	*jsonMCPClient
}

// Exists reports the client as present when either the config file
// exists OR its parent directory (~/.kiro/settings) does — mirroring the
// cursor/qwen "directory means installed" heuristic so an operator who
// has Kiro installed but no MCP config yet still gets the Initialize /
// install affordance.
func (k *kiroClient) Exists() bool {
	if _, err := os.Stat(k.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(k.path))
	return err == nil && st.IsDir()
}

func (k *kiroClient) Backup() (string, error) {
	return k.BackupKeep(0)
}

// BackupKeep ensures the nested ~/.kiro/settings parent directory exists,
// seeds an empty mcpServers stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir is two levels
// deep (.kiro/settings) and does not exist on a clean install, so the
// MkdirAll here is load-bearing — without it writeBackup/InitEmpty would
// fail on a fresh host.
func (k *kiroClient) BackupKeep(keepN int) (string, error) {
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

// InitEmpty seeds ~/.kiro/settings/mcp.json with `{"mcpServers": {}}` if
// the file is absent. Kiro shares the canonical JSON family schema;
// AddEntry's later merge writes into the same `mcpServers` map.
func (k *kiroClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(k.path, []byte("{\n  \"mcpServers\": {}\n}\n"))
}

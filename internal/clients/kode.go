package clients

import (
	"os"
	"path/filepath"
)

// NewKode returns a Client bound to Kode's global MCP config at ~/.kode.json.
//
// Kode (the terminal AI coding agent; npm @shareai-lab/kode, command `kode`)
// reads MCP servers from ~/.kode.json under the top-level `mcpServers` key —
// the canonical JSON family schema (Record<string, McpServerConfig>). The
// McpServerConfig is a discriminated union keyed on `type`: a remote server
// uses `type:"http"` (or `sse`/`sse-ide`/`ws`/`ws-ide`) + `url` + optional
// `headers`; a stdio server omits `type` (default) and uses
// `command`/`args`/`env`. Kode speaks HTTP MCP natively, so the hub writes an
// HTTP-direct entry (no relay shim needed).
//
// The hub-managed remote entry shape written is therefore:
//
//	"<server-name>": {
//	  "type": "http",
//	  "url": "http://localhost:9121/mcp"
//	}
//
// with an optional `headers` object when MCPEntry.Headers is non-empty. The
// discriminated-union docs show no `disabled` field on the HTTP variant, so
// AddEntry emits just `type:"http"` + `url` (+ headers).
//
// Because the endpoint field is the standard `url` key, the embedded
// jsonMCPClient's hub-shape detection (isHubURLShapeEntry(_, "url")) and the
// inherited GetEntry / RemoveEntry / RestoreEntryFromBackup /
// BackupEntryIsHubManaged helpers all work unchanged with urlField "url" —
// only AddEntry (the `type:"http"` discriminator) needs a dedicated
// implementation.
//
// Path note: the config is a single dotfile directly under the home directory
// (~/.kode.json), not a file inside a per-tool subdirectory. The parent dir is
// therefore always the home dir, so this adapter uses a plain file-existence
// Exists (a dir-based "parent means installed" heuristic would be vacuously
// true on every host) and needs no MkdirAll in BackupKeep — the home dir
// already exists.
//
// Source (verified 2026-06-17):
//   - https://deepwiki.com/shareAI-lab/Kode-Agent/4.2-mcp-integration — global
//     config at ~/.kode.json; top-level `mcpServers` key
//     (Record<string, McpServerConfig>); McpServerConfig is a discriminated
//     union over `type` ∈ {stdio (default, type optional), sse, http, sse-ide,
//     ws, ws-ide}; HTTP variant `{"type":"http","url":...,"headers":{...}}`;
//     stdio variant `{"command":...,"args":[...],"env":{...}}`.
func NewKode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := &jsonMCPClient{
		path:       filepath.Join(home, ".kode.json"),
		clientName: "kode",
		// Standard `url` endpoint key: the embedded base helpers (GetEntry,
		// hub-shape detection, restore guards) all key off this. AddEntry is
		// overridden below only to add the `type:"http"` discriminator.
		urlField: "url",
	}
	return newLockingClient(&kodeClient{jsonMCPClient: base}), nil
}

// kodeClient overrides only AddEntry (to emit the `type:"http"` discriminator)
// and Backup/BackupKeep (to seed an empty stub on a fresh install). Exists,
// InitEmpty, GetEntry, RemoveEntry, Restore, and every backup/demigrate helper
// are promoted from the embedded jsonMCPClient unchanged. The base Exists
// (plain file-existence) is correct here because the config is a dotfile
// directly under the always-present home directory.
type kodeClient struct {
	*jsonMCPClient
}

func (k *kodeClient) Backup() (string, error) {
	return k.BackupKeep(0)
}

// BackupKeep seeds an empty `{"mcpServers": {}}` stub if the config is absent,
// then writes the timestamped backup (pruning to keepN). No MkdirAll is needed
// because the parent (the home directory) always exists; seeding via InitEmpty
// lets a migrate run against a fresh install (no ~/.kode.json yet) without
// failing with ErrClientNotInstalled.
func (k *kodeClient) BackupKeep(keepN int) (string, error) {
	if _, err := k.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(k.path, k.Name(), keepN)
}

// AddEntry writes the hub-managed remote entry under the `mcpServers` map with
// Kode's discriminated-union HTTP shape: `type:"http"` + `url` (+ optional
// `headers`). The base jsonMCPClient.AddEntry would omit the `type`
// discriminator (and add a `disabled:false` the union variant does not
// document), so this override is required.
func (k *kodeClient) AddEntry(entry MCPEntry) error {
	serverEntry := map[string]any{
		"type": "http",
		"url":  entry.URL,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set via the embedded seam.
	return k.setMember(entry.Name, serverEntry)
}

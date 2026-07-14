package clients

import (
	"fmt"

	toml "github.com/pelletier/go-toml/v2"
)

// EntryBytesChecker is the read-only capability of confirming whether a named MCP
// server entry is PHYSICALLY present in a given config-file byte blob — WITHOUT any
// disk read or mutation. adopt-provenance capture uses it to verify the EXACT
// snapshotted write-target bytes actually contain the adopted entry before pinning
// them, so a TOCTOU where the entry is deleted (and possibly re-created) mid-capture
// cannot pin a snapshot that a later de-adopt would restore as an ABSENCE (codex bot
// PR #528 r4 finding 3).
//
// It is implemented by the adopt-supported client adapters (single-file clients pin
// their own bytes; MiMoCode pins its write target) and forwarded by the
// lockingClient wrapper. "Present in bytes" mirrors each adapter's own GetEntry
// parse + section lookup. NOTE (MiMoCode): "present in the write-target bytes" is
// PHYSICAL presence in the write target (mimocode.json's "mcp"); an entry that
// resolves only from a LOWER/import layer never reaches this check — it is routed to
// present-merged-lower by SourceBelowWriteTarget before any snapshot.
type EntryBytesChecker interface {
	EntryPresentInBytes(configBytes []byte, name string) (bool, error)
}

// Compile-time proof that every adopt-supported adapter (and the wrapper every
// AllClients() adapter is wrapped in) satisfies the capability. jsonMCPClient covers
// its embedders (cursor / gemini-cli / qwen-cli / antigravity / relay clients).
var (
	_ EntryBytesChecker = (*jsonMCPClient)(nil)
	_ EntryBytesChecker = (*claudeCode)(nil)
	_ EntryBytesChecker = (*vscodeClient)(nil)
	_ EntryBytesChecker = (*openCodeClient)(nil)
	_ EntryBytesChecker = (*codexCLI)(nil)
	_ EntryBytesChecker = (*mimoCodeClient)(nil)
	_ EntryBytesChecker = (*lockingClient)(nil)
)

// jsoncEntryPresentInBytes parses jsonc config bytes and reports whether
// <section>[name] is a JSON object — the same parse + lookup the JSON-family
// adapters' GetEntry uses. Pure/read-only (no disk access).
func jsoncEntryPresentInBytes(configBytes []byte, section, name string) (bool, error) {
	_, present, err := jsoncEntryRawSubtree(configBytes, section, name)
	return present, err
}

// jsoncEntryRawSubtree is the single JSONC section extractor used by both the
// physical-presence check and Phase-4 classification. It returns the parsed
// on-disk entry value without projecting it onto the intentionally lean
// MCPEntry shape.
func jsoncEntryRawSubtree(configBytes []byte, section, name string) (any, bool, error) {
	m, err := parseJSONCBytes(configBytes)
	if err != nil {
		return nil, false, err
	}
	servers, _ := m[section].(map[string]any)
	if servers == nil {
		return nil, false, nil
	}
	subtree, ok := servers[name].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	return subtree, true, nil
}

// tomlEntryPresentInBytes is the TOML analogue (codex-cli's readTOML parse).
func tomlEntryPresentInBytes(configBytes []byte, section, name string) (bool, error) {
	_, present, err := tomlEntryRawSubtree(configBytes, section, name)
	return present, err
}

// tomlEntryRawSubtree is the TOML analogue of jsoncEntryRawSubtree.
func tomlEntryRawSubtree(configBytes []byte, section, name string) (any, bool, error) {
	var m map[string]any
	if err := toml.Unmarshal(configBytes, &m); err != nil {
		return nil, false, fmt.Errorf("parse toml config bytes: %w", err)
	}
	servers, _ := m[section].(map[string]any)
	if servers == nil {
		return nil, false, nil
	}
	subtree, ok := servers[name].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	return subtree, true, nil
}

// ---- adapter implementations (methods may live in any file of package clients) ----

// jsonMCPClient covers every embedder (cursor, gemini-cli, qwen-cli, antigravity,
// and the rest) via its sectionKey.
func (j *jsonMCPClient) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return jsoncEntryPresentInBytes(configBytes, j.sectionKey(), name)
}

func (c *claudeCode) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return jsoncEntryPresentInBytes(configBytes, claudeCodeMCPServersKey, name)
}

func (v *vscodeClient) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return jsoncEntryPresentInBytes(configBytes, vscodeServersKey, name)
}

func (o *openCodeClient) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return jsoncEntryPresentInBytes(configBytes, openCodeMCPKey, name)
}

func (c *codexCLI) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return tomlEntryPresentInBytes(configBytes, "mcp_servers", name)
}

// mimoCodeClient checks PHYSICAL presence in the write-target bytes (mimocode.json's
// "mcp"). An entry that resolves only from a lower/import layer is routed to
// present-merged-lower before this check (SourceBelowWriteTarget); an entry defined
// only in a layer ABOVE the write target (mimocode.jsonc etc.) is not physically in
// the write-target bytes and so reads absent here (conservative fail-closed at the
// capture caller — see the r4 report flag).
func (o *mimoCodeClient) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	return jsoncEntryPresentInBytes(configBytes, mimoCodeMCPKey, name)
}

// lockingClient forwards to the wrapped adapter (read-only, no config lock needed).
// Every AllClients() adapter is lockingClient-wrapped, so this is how capture reaches
// the concrete adapter's EntryPresentInBytes. A wrapped client that does not
// implement the capability (a non-adopt client, never reached by capture) errors.
func (l *lockingClient) EntryPresentInBytes(configBytes []byte, name string) (bool, error) {
	if c, ok := l.Client.(EntryBytesChecker); ok {
		return c.EntryPresentInBytes(configBytes, name)
	}
	return false, fmt.Errorf("client %q does not support byte-level entry validation", l.Name())
}

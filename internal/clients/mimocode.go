package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewMimoCode returns a Client bound to MiMoCode's global config file.
//
// MiMoCode (Xiaomi MiMo Code, github.com/XiaomiMiMo/MiMo-Code) is built as
// a FORK of OpenCode and keeps all of OpenCode's core capabilities — TUI,
// LSP, MCP, plugins, multiple providers. Crucially it inherits OpenCode's
// MCP config: MCP server definitions live in the top-level `mcp` object of
// its JSON config, with the IDENTICAL local/remote entry shapes. Two config
// scopes exist:
//
//   - Global: the global config DIRECTORY is $MIMOCODE_HOME/config/ when
//     MIMOCODE_HOME is set (absolute path required), else
//     $XDG_CONFIG_HOME/mimocode/, else ~/.config/mimocode/. On every OS
//     MiMoCode resolves the default global config from ~/.config/mimocode/ —
//     like OpenCode it does NOT follow the Windows %APPDATA% / macOS
//     ~/Library convention (the user's real Windows install is at
//     C:\Users\<user>\.config\mimocode\, confirmed 2026-06).
//   - Project: .mimocode/mimocode.json in the repository root.
//
// Within the global directory MiMoCode reads `config.json` / `mimocode.json`
// / `mimocode.jsonc`, "merged in that order (later overrides earlier)", so
// `mimocode.jsonc` takes FINAL precedence, then `mimocode.json`, then
// `config.json`. So if the operator already keeps a `mimocode.jsonc`, an entry
// written into a separate `mimocode.json` would be silently overridden by the
// `.jsonc` at load time. To make the hub write win AND so scan/backup read the
// file that actually holds the entries, this adapter targets the
// HIGHEST-precedence EXISTING file (`mimocode.jsonc` > `mimocode.json` >
// `config.json`) and otherwise falls back to `mimocode.json` (the file it
// creates on a fresh host). `config.json` is in the set because an install
// whose MCP entries live solely there would otherwise be invisible to
// scan/backup and a migrate would write without a backup.
//
// Config LOCATION is further overridable by two documented env vars, honored
// AHEAD of the MIMOCODE_HOME/XDG/default fallback: MIMOCODE_CONFIG names a
// custom config FILE (highest precedence; bypasses all file probing) and
// MIMOCODE_CONFIG_DIR names a custom config DIR. Full documented precedence:
// MIMOCODE_CONFIG > MIMOCODE_CONFIG_DIR > MIMOCODE_HOME > XDG_CONFIG_HOME >
// default ~/.config/mimocode/ (see defaultMimoCodeConfigPath /
// mimoCodeGlobalConfigDir). These divergences from the OpenCode adapter
// (MIMOCODE_CONFIG/_DIR/_HOME env vars + the multi-file preference) come
// straight from MiMoCode's own config docs — OpenCode's docs leave them
// under-specified, but MiMoCode's fork documents them explicitly, so the
// verified MiMoCode behavior governs. (Sources verified 2026-06:
// https://mimo.xiaomi.com/mimocode/config-overrides — "$MIMOCODE_HOME/config/"
// global dir, MIMOCODE_HOME must be absolute; the global directory accepts
// "config.json / mimocode.json / mimocode.jsonc, merged in that order (later
// overrides earlier)"; MIMOCODE_CONFIG / MIMOCODE_CONFIG_DIR location
// overrides.)
//
// The hub writes the GLOBAL file so a single per-user hub entry is visible
// in every project, matching every other adapter's user-scoped posture (and
// matching OpenCode's adapter, which also returns the global path).
//
// Transport choice — HTTP-direct (NOT relay-stdio). Like OpenCode, MiMoCode
// supports remote MCP servers natively over Streamable HTTP. A remote entry
// is keyed by server name under `mcp` and discriminated by `"type":"remote"`
// with a `url` endpoint and an `enabled` flag:
//
//	{
//	  "mcp": {
//	    "<server-name>": {
//	      "type": "remote",
//	      "url": "http://localhost:9121/mcp",
//	      "enabled": true
//	    }
//	  }
//	}
//
// (Local stdio servers use "type":"local" with a `command` ARRAY instead;
// the hub never writes that shape because the daemon is already an HTTP
// endpoint.) Optional `headers` is emitted when MCPEntry.Headers is
// non-empty.
//
// This adapter is a 1:1 structural mirror of the OpenCode adapter (see
// opencode.go) — MiMoCode is OpenCode's MCP format. Only the client id and
// the config path segment differ ("opencode" -> "mimocode"); the `mcp`-key
// object schema, the local/remote `type` discrimination, the JSONC-tolerant
// read path, and the comment-preserving secure-write routing are byte-
// identical and reuse the same shared helpers.
//
// IMPORTANT divergences from the JSON family (mcpServers + disabled:false),
// inherited from OpenCode:
//   - top-level key is `mcp`, NOT `mcpServers` — so this is a standalone
//     struct (like vscode/zed/opencode), not an embedding of jsonMCPClient.
//   - the active flag is `enabled` (true = on), NOT `disabled` (false =
//     on). The hub writes `enabled:true`.
//   - remote entries carry `"type":"remote"`.
//
// The hub-shape guard (isHubURLShapeEntry) keys off the `url` field and the
// absence of a `command` key, which holds for MiMoCode's remote entry shape,
// so demigrate/rollback recognize a hub-managed MiMoCode entry.
func NewMimoCode() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&mimoCodeClient{path: defaultMimoCodeConfigPath(home), home: home}), nil
}

// defaultMimoCodeConfigPath returns the global MiMoCode config-file path.
//
// It first honors the two explicit config-location env vars MiMoCode
// documents (MIMOCODE_CONFIG → a custom config FILE; MIMOCODE_CONFIG_DIR → a
// custom config DIR), AHEAD of the MIMOCODE_HOME/XDG/default directory
// fallback (the documented precedence is MIMOCODE_CONFIG > MIMOCODE_CONFIG_DIR
// > MIMOCODE_HOME > XDG_CONFIG_HOME > default ~/.config/mimocode/; sources:
// https://mimo.xiaomi.com/mimocode/config-overrides). When MIMOCODE_CONFIG
// points at an absolute file, that file IS the config and no dir/file probing
// happens. Otherwise it resolves the global config DIRECTORY then picks the
// file within it (see mimoCodePreferredConfigFile).
//
// Resolving the env + file choice HERE (the single path owner) — not in
// AddEntry — is load-bearing: scan, probe, backup, and write all read the path
// through ConfigPath(), so they must agree on which file holds the hub entry.
// The choice is a pure function of (env, on-disk state) at construction time,
// which keeps ConfigPath() stable for the lifetime of the adapter instance
// (the file does not move under it mid-run).
func defaultMimoCodeConfigPath(home string) string {
	// MIMOCODE_CONFIG names a config FILE directly and is the highest-precedence
	// location override. Like MIMOCODE_HOME it must be absolute (a relative value
	// is ignored so resolution never depends on the process cwd). When set, the
	// named file is the config — no .jsonc/.json/config.json probing applies.
	if mc := os.Getenv("MIMOCODE_CONFIG"); mc != "" && filepath.IsAbs(mc) {
		return mc
	}
	dir, isGlobalDefault := mimoCodeGlobalConfigDir(home)
	return mimoCodePreferredConfigFile(dir, isGlobalDefault)
}

// mimoCodeGlobalConfigDir resolves the global config directory in MiMoCode's
// documented precedence order: MIMOCODE_CONFIG_DIR (absolute-path required) →
// MIMOCODE_HOME/config (absolute-path required; a relative value is ignored,
// mirroring the docs' "must be an absolute path") → XDG_CONFIG_HOME/mimocode →
// ~/.config/mimocode. The MIMOCODE_CONFIG (file) override is handled one level
// up in defaultMimoCodeConfigPath because it bypasses dir resolution entirely.
// The path is OS-independent by design — MiMoCode uses ~/.config/mimocode/ on
// every OS, not %APPDATA% / ~/Library.
//
// The second return value reports whether the resolved dir is the
// GLOBAL-DEFAULT directory (MIMOCODE_HOME/config, XDG_CONFIG_HOME/mimocode, or
// ~/.config/mimocode) as opposed to a custom location named by
// MIMOCODE_CONFIG_DIR. The distinction governs whether `config.json` is an
// accepted layer: MiMoCode reads config.json/mimocode.json/mimocode.jsonc in
// the GLOBAL directory, but "`.mimocode/` and MIMOCODE_CONFIG_DIR likewise use
// mimocode.json(c)" — config.json is a global-default-dir-ONLY layer that MiMo
// does NOT load from a custom config dir (verified 2026-06:
// https://mimo.xiaomi.com/mimocode/config-overrides). So a custom dir holding
// only config.json must NOT be selected/written, because MiMo won't load it.
func mimoCodeGlobalConfigDir(home string) (dir string, isGlobalDefault bool) {
	if cd := os.Getenv("MIMOCODE_CONFIG_DIR"); cd != "" && filepath.IsAbs(cd) {
		return cd, false
	}
	if mh := os.Getenv("MIMOCODE_HOME"); mh != "" && filepath.IsAbs(mh) {
		return filepath.Join(mh, "config"), true
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mimocode"), true
	}
	return filepath.Join(home, ".config", "mimocode"), true
}

// mimoCodeAcceptedFileNames returns the global config file names MiMoCode reads
// in this dir, in LOWEST→HIGHEST merge-precedence order ("merged in that order,
// later overrides earlier" → mimocode.jsonc has the final say). `config.json`
// is included ONLY for the global-default directory; for a custom
// MIMOCODE_CONFIG_DIR it is excluded because MiMo loads only mimocode.json(c)
// there (Finding 2). Single owner of the accepted-file set so the path picker
// and the merge-read layer agree on which files exist.
func mimoCodeAcceptedFileNames(isGlobalDefault bool) []string {
	if isGlobalDefault {
		return []string{"config.json", "mimocode.json", "mimocode.jsonc"}
	}
	return []string{"mimocode.json", "mimocode.jsonc"}
}

// mimoCodePreferredConfigFile picks the WRITE-target config file inside dir
// following MiMoCode's documented merge order. MiMoCode reads the accepted
// global files "merged in that order (later overrides earlier)", so
// `mimocode.jsonc` has the FINAL say, then `mimocode.json`, then (global dir
// only) `config.json`. The hub write must land in whichever of those the
// operator already keeps so its entry survives the merge.
//
// Resolution: return the HIGHEST-precedence EXISTING accepted file
// (`mimocode.jsonc` > `mimocode.json` > `config.json` [global dir only]); if
// none exists, fall back to creating `mimocode.json` — the file the adapter
// seeds on a fresh host and the one every existing test/fixture expects. A
// stat error other than not-exist (e.g. a permission failure) is treated as
// "absent" for that candidate so resolution never fails closed on a transient
// probe error.
//
// NOTE — the WRITE target is the highest layer, but the READ/SCAN path now
// deep-merges ALL accepted layers (see mimoCodeReadLayerFiles): an MCP server
// living only in config.json stays visible to scan/extract/backup even when a
// separate mimocode.jsonc holds only unrelated settings (Finding 1). Writes
// still target the top layer here — that is fine because the deep merge means
// the top layer overrides per key, so a hub entry written there wins.
func mimoCodePreferredConfigFile(dir string, isGlobalDefault bool) string {
	names := mimoCodeAcceptedFileNames(isGlobalDefault)
	// Highest precedence first: walk the accepted names in reverse.
	for i := len(names) - 1; i >= 0; i-- {
		p := filepath.Join(dir, names[i])
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return filepath.Join(dir, "mimocode.json")
}

// mimoCodeReadLayerFiles returns the EXISTING accepted config-layer files to
// deep-merge for the READ/SCAN/EXTRACT path, in LOWEST→HIGHEST precedence
// order, given the adapter's resolved config path.
//
// MiMoCode's global config is a DEEP MERGE: it reads all of
// config.json/mimocode.json/mimocode.jsonc (global dir) — "merged in that
// order (later overrides earlier)", recursing per key — so an `mcp` server
// defined only in config.json stays ACTIVE even when mimocode.jsonc holds only
// unrelated settings (Finding 1). A merge-read here keeps mcphub's view in
// lockstep with what MiMoCode actually loads, so a split-file profile (the MCP
// server in config.json, settings in mimocode.jsonc) is not invisible.
//
// The resolution CONTEXT is re-derived from the SAME env-precedence owner the
// path picker uses (defaultMimoCodeConfigPath → mimoCodeGlobalConfigDir), not
// guessed from the path string, so the accepted-file set (in particular
// whether config.json is a layer — Finding 2) matches exactly what
// ConfigPath() was resolved against:
//   - MIMOCODE_CONFIG direct-file override: MiMo reads ONLY that named file
//     (no probing). The layer set is just `path`.
//   - MIMOCODE_CONFIG_DIR custom dir: accepted layers are mimocode.json(c)
//     only (config.json excluded — MiMo does not load it from a custom dir).
//   - global-default dir: accepted layers are
//     config.json/mimocode.json/mimocode.jsonc.
//
// Only EXISTING regular files are returned; a stat error other than not-exist
// is treated as "absent" so a transient probe error never drops a real layer
// silently into a hard failure. When no accepted layer exists the resolved
// `path` itself is returned as the sole (possibly-absent) layer so the caller's
// own not-exist handling still fires. `home` is the os.UserHomeDir() the
// adapter was constructed against; passing it explicitly keeps the resolver a
// pure function of (env, home, on-disk state) and test-friendly.
func mimoCodeReadLayerFiles(path, home string) []string {
	// MIMOCODE_CONFIG names a single config FILE; MiMo reads only it.
	if mc := os.Getenv("MIMOCODE_CONFIG"); mc != "" && filepath.IsAbs(mc) {
		return []string{path}
	}
	dir, isGlobalDefault := mimoCodeGlobalConfigDir(home)
	names := mimoCodeAcceptedFileNames(isGlobalDefault)
	var layers []string
	for _, n := range names {
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			layers = append(layers, p)
		}
	}
	if len(layers) == 0 {
		// No accepted layer on disk yet — return the resolved path so the
		// caller's not-exist handling runs against the file it would create.
		return []string{path}
	}
	return layers
}

// MimoCodeMergedMCP deep-merges MiMoCode's accepted config layers (Finding 1)
// and returns the resulting top-level `mcp` map keyed by server name, given the
// adapter's RESOLVED config path (the same path clients.ConfigPathForName /
// defaultMimoCodeConfigPath produced). It is the single exported seam the scan
// and extract code paths in the api package use so they see exactly what
// MiMoCode loads — a server defined only in config.json stays visible even when
// the resolved top-layer file (mimocode.jsonc) holds only unrelated settings.
//
// home is resolved best-effort via os.UserHomeDir(); the env-precedence layer
// resolution (MIMOCODE_CONFIG/_DIR/_HOME, XDG) does not need it except for the
// last-resort ~/.config/mimocode default, so a home-resolution error degrades
// gracefully to env-only resolution rather than failing the read. A missing
// config file yields an empty map + nil error (the caller treats absence as "no
// entries"); a present-but-unparseable layer is a hard error.
func MimoCodeMergedMCP(path string) (map[string]map[string]any, error) {
	home, _ := os.UserHomeDir()
	merged, err := mimoCodeMergedConfig(path, home)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	servers, _ := merged[mimoCodeMCPKey].(map[string]any)
	for name, v := range servers {
		if entry, ok := v.(map[string]any); ok {
			out[name] = entry
		}
	}
	return out, nil
}

// mimoCodeClient is a standalone adapter (NOT an embedding of jsonMCPClient)
// because MiMoCode uses the top-level `mcp` key rather than the JSON family's
// `mcpServers`, AND a distinct entry shape (`type:"remote"` + `enabled:true`
// rather than `disabled:false`). It mirrors the OpenCode adapter's
// standalone-struct + HTTP-direct pattern with the same key/field set.
type mimoCodeClient struct {
	path string
	// home is the os.UserHomeDir() the adapter was constructed against. It is
	// threaded into the deep-merge read-layer resolver (mimoCodeReadLayerFiles)
	// so the read path re-derives the SAME env-precedence config context the
	// path picker used, instead of guessing it from the path string. Empty when
	// the adapter is constructed in a context without a resolvable home (the
	// resolver then falls back to env-only dir resolution, which still honors
	// MIMOCODE_CONFIG/_DIR/_HOME/XDG — the home is only the last-resort default).
	home string
}

// mimoCodeMCPKey is the single owner of MiMoCode's top-level MCP section
// name. Every method that reaches into the parsed config map uses it.
const mimoCodeMCPKey = "mcp"

func (o *mimoCodeClient) Name() string       { return "mimocode" }
func (o *mimoCodeClient) ConfigPath() string { return o.path }

// IsRelayStdio reports false: mimocode is a URL-native HTTP MCP client.
func (o *mimoCodeClient) IsRelayStdio() bool { return false }

// Exists treats MiMoCode as installed when EITHER the config file is present
// OR its parent directory (~/.config/mimocode/) exists, mirroring the
// opencode/cursor/vscode/kiro "directory means installed" heuristic so an
// operator who has MiMoCode installed but no MCP config yet still gets the
// Initialize / install affordance.
func (o *mimoCodeClient) Exists() bool {
	if _, err := os.Stat(o.path); err == nil {
		return true
	}
	st, err := os.Stat(filepath.Dir(o.path))
	return err == nil && st.IsDir()
}

func (o *mimoCodeClient) Backup() (string, error) {
	return o.BackupKeep(0)
}

// BackupKeep ensures the nested ~/.config/mimocode parent directory exists,
// seeds an empty `{"mcp": {}}` stub if the config is absent, then writes the
// timestamped backup (pruning to keepN). The parent dir does not exist on a
// clean install, so the MkdirAll here is load-bearing — without it
// writeBackup/InitEmpty would fail on a fresh host. Mirrors the
// opencode/cursor/vscode/kiro/windsurf BackupKeep wrappers.
func (o *mimoCodeClient) BackupKeep(keepN int) (string, error) {
	if dir := filepath.Dir(o.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := o.InitEmpty(); err != nil {
		return "", err
	}
	return writeBackup(o.path, o.Name(), keepN)
}

// InitEmpty seeds ~/.config/mimocode/mimocode.json with `{"mcp": {}}` if the
// file is absent. AddEntry's later merge writes into the same `mcp` map.
// MiMoCode also accepts a top-level `$schema` field and many other keys, plus
// JSONC comments / trailing commas (it explicitly supports a `.jsonc` variant
// and operators hand-edit it): the read path parses comments via the shared
// JSONC helper, and AddEntry/RemoveEntry patch through hujson so the
// operator's comments and every unknown top-level key already present in the
// file are PRESERVED on every write (only the `mcp` map is touched) — so
// seeding a minimal stub does not clobber a hand-authored config, and on a
// truly fresh host this minimal stub is all that is needed.
func (o *mimoCodeClient) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(o.path, []byte("{\n  \"mcp\": {}\n}\n"))
}

func (o *mimoCodeClient) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so production
	// restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(o.path, data)
}

// readJSON returns the DEEP-MERGED view of MiMoCode's global config across all
// accepted layers (config.json → mimocode.json → mimocode.jsonc, later wins),
// mirroring what MiMoCode itself loads (Finding 1). Every read/scan consumer
// (GetEntry, AllStdioEntries, FindStdioLanguageServerEntries) goes through here
// so a server defined only in config.json — while mimocode.jsonc holds only
// unrelated settings — stays visible to mcphub instead of being hidden behind a
// partial top-layer read. WRITES still target the top layer (setMember /
// deleteMember operate on o.path); the deep merge means the top layer overrides
// per key, so a hub entry written there wins the merge.
func (o *mimoCodeClient) readJSON() (map[string]any, error) {
	return mimoCodeMergedConfig(o.path, o.home)
}

// mimoCodeMergedConfig reads each accepted config layer (lowest→highest
// precedence) and deep-merges them. MiMoCode's config is JSONC (it supports a
// `.jsonc` variant, comments, and trailing commas, and operators hand-edit it)
// — each layer parses via the comment-tolerant shared helper. A missing layer
// is skipped (treated as an empty contribution); a present-but-unparseable
// layer is a hard error the caller must see (the same posture the single-file
// read had). The merge is a deep, per-key recursion (mimoCodeDeepMergeInto), so
// the `mcp` object accumulates servers across layers and a later layer's entry
// of the same name overrides the earlier one ("overridden by the later
// writer").
func mimoCodeMergedConfig(path, home string) (map[string]any, error) {
	layers := mimoCodeReadLayerFiles(path, home)
	merged := map[string]any{}
	for _, layer := range layers {
		data, err := readRawConfig(layer)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		m, err := parseJSONCBytes(data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", layer, err)
		}
		mimoCodeDeepMergeInto(merged, m)
	}
	return merged, nil
}

// mimoCodeDeepMergeInto deep-merges src into dst in place: for each key, when
// BOTH sides hold a JSON object the merge recurses per key (so e.g. two layers'
// `mcp` objects union their server entries), otherwise src's value REPLACES
// dst's ("later overrides earlier"). This matches MiMoCode's documented
// "general merge is a deep merge, recursing per key on objects", with the
// same-name `mcp` entry being "overridden by the later writer" (a server entry
// is a leaf object whose whole value the later layer replaces — its inner
// fields are NOT field-merged across layers, which would invent a hybrid entry
// MiMo never sees).
func mimoCodeDeepMergeInto(dst, src map[string]any) {
	for k, sv := range src {
		if svMap, ok := sv.(map[string]any); ok {
			// When both layers carry an object under the same key, deep-merge
			// recursively (per-key) so unrelated nested settings from both
			// layers survive. The `mcp` container follows the same rule — its
			// per-server-name entries union, and a same-name entry in the later
			// (src) layer replaces the earlier one whole ("overridden by the
			// later writer"), because the recursion's leaf assignment sets the
			// entry VALUE directly rather than field-merging two entries (which
			// would invent a hybrid entry MiMo never sees). Ensure dst[k] is a
			// FRESH object map so we never alias and then mutate a throwaway
			// per-layer parse result.
			dvMap, ok := dst[k].(map[string]any)
			if !ok {
				dvMap = map[string]any{}
				dst[k] = dvMap
			}
			if k == mimoCodeMCPKey {
				for name, entry := range svMap {
					dvMap[name] = entry
				}
			} else {
				mimoCodeDeepMergeInto(dvMap, svMap)
			}
			continue
		}
		dst[k] = sv
	}
}

// setMember sets mcp.<name> = value, and deleteMember removes it, both
// preserving the operator's comments + unrelated top-level keys (e.g.
// `$schema`) when the file already has JSONC content. An empty/absent file
// falls back to a clean indented marshal. The bytes route through the
// UNCHANGED WriteConfigFile pipeline.
func (o *mimoCodeClient) setMember(name string, value any) error {
	return mutateJSONObjectMember(o.path, mimoCodeMCPKey, name, value, false)
}

func (o *mimoCodeClient) deleteMember(name string) error {
	return mutateJSONObjectMember(o.path, mimoCodeMCPKey, name, nil, true)
}

// AddEntry writes the hub-managed remote-HTTP entry under mcp.<name>.
// MiMoCode's remote entry shape is `{"type":"remote","url":...,
// "enabled":true}`; an optional `headers` object is emitted when
// MCPEntry.Headers is non-empty.
//
// FAITHFUL-RESTORE path (Finding 5): when entry.Raw is non-nil the entry is a
// VERBATIM prior on-disk shape captured by GetEntry that the URL fields cannot
// represent (a LOCAL command-array entry). Write it back unchanged so an
// install/register rollback restores the user's original local entry instead
// of clobbering it with a broken `url:""` remote entry. Raw is never set for a
// hub-managed install (the installer builds MCPEntry from URL/Headers), so this
// branch fires only on the snapshot-rollback round-trip.
func (o *mimoCodeClient) AddEntry(entry MCPEntry) error {
	if entry.Raw != nil {
		return o.setMember(entry.Name, entry.Raw)
	}
	serverEntry := map[string]any{
		"type":    "remote",
		"url":     entry.URL,
		"enabled": true,
	}
	if len(entry.Headers) > 0 {
		serverEntry["headers"] = entry.Headers
	}
	// Comment-preserving set: patches mcp.<name> into the original on-disk
	// bytes via hujson so the operator's comments and unrelated keys survive (a
	// full map re-marshal would drop both).
	return o.setMember(entry.Name, serverEntry)
}

func (o *mimoCodeClient) RemoveEntry(name string) error {
	// Comment-preserving delete; absence is a no-op.
	return o.deleteMember(name)
}

func (o *mimoCodeClient) GetEntry(name string) (*MCPEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	// A LOCAL entry (`{"type":"local","command":[...],"environment":{...}}`) has
	// no `url`; the URL/Headers fields cannot represent it. Carry the verbatim
	// shape in Raw so the install/register snapshot-rollback round-trips it
	// faithfully (AddEntry writes Raw back unchanged) instead of corrupting it
	// into a broken `url:""` remote entry (Finding 5). A REMOTE/hub entry (has a
	// `url`) keeps the lean URL/Headers representation so every existing
	// caller (demigrate hub-shape checks, idempotency diffs) is unchanged.
	url, hasURL := raw["url"].(string)
	if !hasURL {
		return &MCPEntry{Name: name, Raw: raw}, nil
	}
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers")}, nil
}

func (o *mimoCodeClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(o.path, o.Name())
}

func (o *mimoCodeClient) RestoreEntryFromBackup(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc
// on Client.RestoreEntryFromBackupForRollback). Used only by the serena
// dynamic-pool migrate abort-rollback.
func (o *mimoCodeClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, true)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a backup entry already in hub-HTTP shape (a hub
// loopback URL under `url` with no `command`) with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes the
// backup bytes verbatim regardless of shape.
func (o *mimoCodeClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	// os.ReadFile (NOT readRawConfig): a named backup that is missing is a
	// genuine read error the demigrate caller must see, not a silent
	// treat-as-empty. Empty / comment-only / malformed bytes are then
	// classified by parseJSONCBytes (empty map vs parse error).
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	backupMap, err := parseJSONCBytes(backupData)
	if err != nil {
		return fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	backupServers, _ := backupMap[mimoCodeMCPKey].(map[string]any)
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive guard (demigrate flow only — the rollback caller
			// passes allowHubEntry=true to restore the pre-reconcile legacy
			// hub entry verbatim).
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubURLShapeEntry(rawMap, "url") {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			// Comment-preserving set into the LIVE config (its comments +
			// unrelated keys survive; the backup's entry VALUE is written).
			return o.setMember(name, backupEntry)
		}
	}
	return o.deleteMember(name)
}

// AllStdioEntries returns every stdio entry from MiMoCode's top-level `mcp`
// key. The hub writes only HTTP-direct ("type":"remote") entries, which have
// no `command` field and so are correctly skipped by collectStdioEntries.
// Operator-authored local ("type":"local") entries store `command` as an
// ARRAY (["npx","-y",...]) rather than a string; collectStdioEntries reads
// `command` as a string and therefore does not surface them — an accepted
// limitation of the cross-format cleanup scan (these helpers are best-effort
// stdio-leak detection, and the hub never writes the local shape).
func (o *mimoCodeClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans `mcp` for stdio entries matching the
// mcp-language-server invocation pattern. As with AllStdioEntries, MiMoCode
// local entries use a `command` ARRAY which the string-keyed matcher does not
// recognize; the hub-written HTTP entries never match either way.
func (o *mimoCodeClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

func (o *mimoCodeClient) BackupContainsEntry(backupPath, name string) (bool, error) {
	// os.ReadFile (NOT readRawConfig): a missing named backup is a read error,
	// not a silent (false, nil); empty bytes parse to an empty map below.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether mcp[name] in the backup at
// backupPath is in MiMoCode's hub-managed shape (a hub loopback `url` with no
// `command`). See Client.BackupEntryIsHubManaged.
func (o *mimoCodeClient) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	// os.ReadFile (NOT readRawConfig): a missing named backup is a read error
	// the demigrate caller must see, not a silent (false, nil); empty bytes
	// parse to an empty map below.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}

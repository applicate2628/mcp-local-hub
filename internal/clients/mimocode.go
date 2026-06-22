package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewMimoCode returns a Client bound to MiMoCode's global config file.
//
// MiMoCode (Xiaomi MiMo Code, github.com/XiaomiMiMo/MiMo-Code) is built as a
// FORK of OpenCode and keeps OpenCode's MCP config: MCP server definitions
// live in the top-level `mcp` object of its JSON config, with the IDENTICAL
// local/remote entry shapes. Where this adapter diverges from the OpenCode
// adapter it is to stay FAITHFUL to MiMoCode's own multi-layer config
// resolution (packages/opencode/src/config/config.ts + paths.ts +
// packages/shared/src/global.ts), NOT to OpenCode's simpler single-file model:
//
//   - MiMoCode's global loader deep-merges THREE files in the resolved global
//     dir, lowest precedence first: config.json < mimocode.json <
//     mimocode.jsonc (config.ts `loadGlobal` pipes three successive mergeDeep
//     calls). config.json IS a real merge layer — see mimoCodeGlobalLayerNames.
//   - MiMoCode honors a richer env-override chain for the global config
//     location (MIMOCODE_HOME / XDG_CONFIG_HOME) — see
//     resolveMimoCodeGlobalDir — and two custom-config flags layered ON TOP of
//     the global dir, not replacing it: MIMOCODE_CONFIG (a single config FILE,
//     loaded verbatim) and MIMOCODE_CONFIG_DIR (an ADDITIONAL overlay dir whose
//     mimocode.json/.jsonc merge on top of the global dir, custom winning) —
//     see resolveMimoCodeConfig.
//   - MiMoCode local entries store `command` as an ARRAY (and env under
//     `environment`) — handled by the scanner + extractor + the LSP scan, and
//     the reason GetEntry returns a Raw-carrying MCPEntry for a URL-less local
//     entry (so rollback restores it verbatim).
//
// Two config scopes exist (same as OpenCode):
//
//   - Global: the resolved global dir's config.json / mimocode.json /
//     mimocode.jsonc deep-merged layers (see resolveMimoCodeGlobalDir). On
//     every OS MiMoCode resolves the default global config from
//     ~/.config/mimocode/ — like OpenCode it does NOT follow the Windows
//     %APPDATA% / macOS ~/Library convention (global.ts uses the xdg-basedir
//     config dir joined with the "mimocode" app name).
//   - Project: per-repo + per-project `.mimocode` directories (highest
//     precedence, merged over global at load time).
//
// The hub WRITES the GLOBAL dir's top layer so a single per-user hub entry is
// visible in every project, matching every other adapter's user-scoped posture.
// READ (and the rollback restore) merge every active layer; WRITE and DELETE
// touch the single top write-target file ONLY (o.path) — never a lower layer —
// so an operator's lower-layer (config.json / mimocode.json) original is never
// destroyed and re-emerges via the merge after the hub's top-layer entry is
// removed. The project-root / cross-DIR `.mimocode` overlay (project + home) is
// a documented LIMITATION shared with the OpenCode adapter — this adapter
// targets the resolved global dir plus the MIMOCODE_CONFIG_DIR overlay only.
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
// (Local stdio servers use "type":"local" with a `command` ARRAY plus an
// optional `environment` object instead; the hub never WRITES that shape
// because the daemon is already an HTTP endpoint, but the scanner/extractor/LSP
// scan READ it faithfully and rollback RESTORES it verbatim via MCPEntry.Raw.)
// Optional `headers` is emitted when MCPEntry.Headers is non-empty.
//
// Sources (verified 2026-06 against the live source):
//   - config.ts (packages/opencode/src/config/config.ts) — `loadGlobal` deep-
//     merges config.json → mimocode.json → mimocode.jsonc (three mergeDeep
//     calls). Flag.MIMOCODE_CONFIG is a single config FILE loaded verbatim;
//     Flag.MIMOCODE_CONFIG_DIR is appended to the directory list and its
//     mimocode.json/.jsonc are merged ON TOP of the global dir (overlay).
//   - paths.ts (packages/opencode/src/config/paths.ts) — `directories()`
//     returns [globalConfig, ...project .mimocode, ...home .mimocode,
//     MIMOCODE_CONFIG_DIR]; the global dir always loads even when
//     MIMOCODE_CONFIG_DIR is set.
//   - global.ts (packages/shared/src/global.ts) — resolveMimocodeHome:
//     MIMOCODE_HOME (absolute, → $HOME/config) else the xdg-basedir config
//     dir joined with APP="mimocode" (→ XDG_CONFIG_HOME/mimocode or
//     ~/.config/mimocode). A relative MIMOCODE_HOME is a hard error there;
//     this adapter conservatively IGNORES relative env values.
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
	r := resolveMimoCodeConfig(home)
	return newLockingClient(&mimoCodeClient{path: r.writeTarget, overlayDir: r.overlayDir}), nil
}

// mimoCodeGlobalLayerNames are the config files MiMoCode's GLOBAL loader merges
// from the resolved global directory, LOWEST precedence first. This matches
// config.ts `loadGlobal`:
//
//	pipe({}, mergeDeep(loadFile(config.json)),
//	        mergeDeep(loadFile(mimocode.json)),
//	        mergeDeep(loadFile(mimocode.jsonc)))
//
// config.json is the base layer, mimocode.json deep-merges over it, and
// mimocode.jsonc wins per key. config.json IS a real merge layer — an earlier
// revision of this adapter wrongly excluded it; the codex bot's config.ts
// citation (PR #420) corrected that.
var mimoCodeGlobalLayerNames = []string{"config.json", "mimocode.json", "mimocode.jsonc"}

// mimoCodeOverlayLayerNames are the config files MiMoCode loads from a
// MIMOCODE_CONFIG_DIR overlay (and from a project/home `.mimocode` dir). Per
// config.ts the per-directory loop reads only `["mimocode.json",
// "mimocode.jsonc"]` (NOT config.json — config.json is a GLOBAL-dir-only
// migration sink). The overlay merges ON TOP of the global layers (custom
// wins).
var mimoCodeOverlayLayerNames = []string{"mimocode.json", "mimocode.jsonc"}

// mimoCodeResolution is the fully resolved config surface: the single
// write/delete target file, the optional MIMOCODE_CONFIG_DIR overlay
// (read-merged on top of the write-target's own global-dir layers), and a
// single-file-override flag set when MIMOCODE_CONFIG points at a verbatim file
// (in which case the read set, the write target, and the delete target are all
// that one file — no dir probing, no overlay, no siblings).
type mimoCodeResolution struct {
	writeTarget  string // the single file WRITE + DELETE touch (o.path)
	overlayDir   string // MIMOCODE_CONFIG_DIR overlay read on top of global ("" when unset / file-override mode)
	fileOverride bool   // true when MIMOCODE_CONFIG pinned a single verbatim file
}

// resolveMimoCodeConfig resolves MiMoCode's config surface honoring its
// documented env precedence:
//
//   - MIMOCODE_CONFIG (absolute FILE) → file-override mode: the read set, the
//     write target, and the delete target are ALL that one file. No dir
//     probing, no sibling layers, no overlay. (config.ts loads
//     Flag.MIMOCODE_CONFIG verbatim; the adapter, which writes ONE user-scoped
//     entry, operates on exactly that file.)
//   - otherwise the GLOBAL dir is resolved (MIMOCODE_HOME > XDG_CONFIG_HOME >
//     ~/.config/mimocode) and:
//   - write target = the global dir's TOP existing layer (.jsonc when
//     present, else mimocode.json — the hub seeds mimocode.json on a fresh
//     host). config.json is NEVER a write target (it is a legacy migration
//     sink; paths.ts loads it only as the lowest READ layer).
//   - MIMOCODE_CONFIG_DIR (absolute DIR), when set, is recorded as the
//     overlay read on TOP of the global layers (custom wins). It does NOT
//     become the write target — the hub writes the canonical per-user
//     global file, which MiMoCode still loads.
//
// Relative env values are IGNORED (global.ts rejects a relative MIMOCODE_HOME
// outright and the XDG spec ignores a relative XDG_CONFIG_HOME).
func resolveMimoCodeConfig(home string) mimoCodeResolution {
	if f := absoluteEnv("MIMOCODE_CONFIG"); f != "" {
		return mimoCodeResolution{writeTarget: f, fileOverride: true}
	}
	globalDir := resolveMimoCodeGlobalDir(home)
	return mimoCodeResolution{
		writeTarget: mimoCodeWriteTargetInDir(globalDir),
		overlayDir:  absoluteEnv("MIMOCODE_CONFIG_DIR"),
	}
}

// mimoCodeWriteTargetInDir returns the WRITE-target file in a config dir: the
// existing higher layer (mimocode.jsonc) when present so a hub entry is not
// written into a separate lower-priority file the client merges below it;
// otherwise mimocode.json — the file the adapter seeds on a fresh host. Never
// config.json (a read-only legacy migration layer).
func mimoCodeWriteTargetInDir(dir string) string {
	if jsonc := filepath.Join(dir, "mimocode.jsonc"); isRegularFile(jsonc) {
		return jsonc
	}
	return filepath.Join(dir, "mimocode.json")
}

// resolveMimoCodeGlobalDir resolves the GLOBAL config DIRECTORY honoring
// MiMoCode's documented env precedence (highest first):
//
//	MIMOCODE_HOME    (absolute DIR → $MIMOCODE_HOME/config)
//	  > XDG_CONFIG_HOME  (absolute DIR → $XDG_CONFIG_HOME/mimocode)
//	  > ~/.config/mimocode
//
// (MIMOCODE_CONFIG — the absolute FILE override — and MIMOCODE_CONFIG_DIR — the
// overlay — are handled one level up in resolveMimoCodeConfig; neither replaces
// the global dir.) Relative env values are IGNORED.
func resolveMimoCodeGlobalDir(home string) string {
	if h := absoluteEnv("MIMOCODE_HOME"); h != "" {
		return filepath.Join(h, "config")
	}
	if xdg := absoluteEnv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mimocode")
	}
	return filepath.Join(home, ".config", "mimocode")
}

// defaultMimoCodeConfigPath returns the WRITE-target path for the resolved
// config surface (the single file every WRITE + DELETE touches). Thin wrapper
// over resolveMimoCodeConfig kept for the config-path test surface and any
// caller that only needs the write target.
func defaultMimoCodeConfigPath(home string) string {
	return resolveMimoCodeConfig(home).writeTarget
}

// absoluteEnv returns the named env var's value only when it is set AND an
// absolute path; otherwise "". A relative value is treated as unset, matching
// MiMoCode's own absolute-only handling (global.ts throws on a relative
// MIMOCODE_HOME; XDG ignores a relative XDG_CONFIG_HOME).
func absoluteEnv(name string) string {
	v := os.Getenv(name)
	if v == "" || !filepath.IsAbs(v) {
		return ""
	}
	return v
}

// isRegularFile reports whether path is an existing regular file. A stat error
// other than not-exist (e.g. a permission failure) is treated as "absent" so
// resolution never fails closed on a transient probe error.
func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// mimoCodeClient is a standalone adapter (NOT an embedding of jsonMCPClient)
// because MiMoCode uses the top-level `mcp` key rather than the JSON family's
// `mcpServers`, AND a distinct entry shape (`type:"remote"` + `enabled:true`
// rather than `disabled:false`). It mirrors the OpenCode adapter's
// standalone-struct + HTTP-direct pattern with the same key/field set, plus
// MiMoCode-faithful multi-layer reads (config.json/mimocode.json/.jsonc + an
// optional MIMOCODE_CONFIG_DIR overlay) and single-top-layer writes/deletes.
//
// path is the single WRITE + DELETE target. overlayDir, when non-empty, is the
// MIMOCODE_CONFIG_DIR overlay whose mimocode.json/.jsonc are read-merged on top
// of path's own directory layers. Direct construction (`&mimoCodeClient{path}`
// in tests / scan / extract) leaves overlayDir empty, so the read set is path's
// own directory layers (or just path for an explicit override) — the state-safe
// default that never reaches the developer's real ~/.config/mimocode.
type mimoCodeClient struct {
	path       string
	overlayDir string
}

// mimoCodeMCPKey is the single owner of MiMoCode's top-level MCP section
// name. Every method that reaches into the parsed config map uses it.
const mimoCodeMCPKey = "mcp"

func (o *mimoCodeClient) Name() string       { return "mimocode" }
func (o *mimoCodeClient) ConfigPath() string { return o.path }

// IsRelayStdio reports false: mimocode is a URL-native HTTP MCP client.
func (o *mimoCodeClient) IsRelayStdio() bool { return false }

// Exists treats MiMoCode as installed when EITHER any resolved READ layer file
// is present OR the write-target's config directory exists, mirroring the
// opencode/cursor/vscode/kiro "directory means installed" heuristic so an
// operator who has MiMoCode installed but no MCP config yet still gets the
// Initialize / install affordance.
func (o *mimoCodeClient) Exists() bool {
	for _, f := range o.readLayerFiles() {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	st, err := os.Stat(filepath.Dir(o.path))
	return err == nil && st.IsDir()
}

func (o *mimoCodeClient) Backup() (string, error) {
	return o.BackupKeep(0)
}

// BackupKeep ensures the write-target's parent directory exists, seeds an empty
// `{"mcp": {}}` stub at the write target if absent, then writes the timestamped
// backup (pruning to keepN). It backs up the WRITE-target file ONLY — the
// single file the hub mutates. A lower-layer (config.json / mimocode.json)
// operator original is never mutated (writes + deletes are top-layer-only), so
// it needs no backup: after a demigrate removes the hub's top-layer entry, the
// lower-layer original re-emerges via the merged read. The parent dir does not
// exist on a clean install, so the MkdirAll here is load-bearing. Mirrors the
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

// InitEmpty seeds the write-target config file with `{"mcp": {}}` if the file
// is absent. AddEntry's later merge writes into the same `mcp` map. MiMoCode
// also accepts a top-level `$schema` field and many other keys, plus JSONC
// comments / trailing commas (it accepts a `.jsonc` variant and operators
// hand-edit it): the read path parses comments via the shared JSONC helper,
// and AddEntry/RemoveEntry patch through hujson so the operator's comments and
// every unknown top-level key already present in the file are PRESERVED on
// every write (only the `mcp` map is touched) — so seeding a minimal stub does
// not clobber a hand-authored config, and on a truly fresh host this minimal
// stub is all that is needed.
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

// readLayerFiles returns every config file the READ merge consumes, LOWEST
// precedence first. This is deliberately BROADER than the single write/delete
// target (o.path): a lower-layer entry must be VISIBLE so the merge reveals an
// operator's original after the hub's top-layer entry is removed.
//
// Resolution (state-safe + explicit-path honoring, bot PR #420):
//
//   - Explicit override / file-override (o.path's basename is NOT a known
//     global layer name — a temp/test path, or a MIMOCODE_CONFIG verbatim
//     file): return ONLY [o.path]. No dir probing, no siblings, no overlay —
//     so a temp/test scan never reaches the real ~/.config/mimocode, and a
//     pinned MIMOCODE_CONFIG file is operated on verbatim.
//   - Known global layer name: return config.json + mimocode.json +
//     mimocode.jsonc in o.path's own directory (the global layers, .jsonc
//     winning), THEN — when overlayDir is set (MIMOCODE_CONFIG_DIR) — the
//     overlay's mimocode.json + mimocode.jsonc appended LAST so the overlay
//     wins per key (faithful to config.ts's directory-order merge).
//
// It never recomputes a directory from env/home beyond the overlayDir already
// captured at construction; it stays inside dir(o.path) plus the explicit
// overlayDir.
func (o *mimoCodeClient) readLayerFiles() []string {
	return mimoCodeReadLayerFiles(o.path, o.overlayDir)
}

// mimoCodeReadLayerFiles is the pure resolver behind
// (*mimoCodeClient).readLayerFiles. See that method's doc for the rules.
func mimoCodeReadLayerFiles(path, overlayDir string) []string {
	base := filepath.Base(path)
	if !mimoCodeIsGlobalLayerName(base) {
		// Explicit override or MIMOCODE_CONFIG file: operate only on the supplied
		// file; do NOT recompute the dir, pull siblings, or apply an overlay.
		return []string{path}
	}
	dir := filepath.Dir(path)
	files := make([]string, 0, len(mimoCodeGlobalLayerNames)+len(mimoCodeOverlayLayerNames))
	for _, n := range mimoCodeGlobalLayerNames {
		files = append(files, filepath.Join(dir, n))
	}
	if overlayDir != "" {
		for _, n := range mimoCodeOverlayLayerNames {
			files = append(files, filepath.Join(overlayDir, n))
		}
	}
	return files
}

// mimoCodeIsGlobalLayerName reports whether base is one of the global-dir layer
// file names (config.json / mimocode.json / mimocode.jsonc). Used to decide
// whether a path is a default-resolved global layer (sibling merge applies) or
// an explicit override / MIMOCODE_CONFIG file (single-file mode).
func mimoCodeIsGlobalLayerName(base string) bool {
	for _, n := range mimoCodeGlobalLayerNames {
		if base == n {
			return true
		}
	}
	return false
}

// readMergedLayers reads every READ layer file and DEEP-MERGES them
// lowest-first (config.json base, mimocode.json over it, mimocode.jsonc over
// that, then the MIMOCODE_CONFIG_DIR overlay's mimocode.json/.jsonc on top) so
// an entry present in any layer is visible and the highest layer wins per key.
// Missing layers are skipped (treated as empty); a parse error on a present
// layer propagates. JSONC (comments + trailing commas) is tolerated via the
// shared parseJSONCBytes helper.
func (o *mimoCodeClient) readMergedLayers() (map[string]any, error) {
	merged := map[string]any{}
	for _, f := range o.readLayerFiles() {
		data, err := readRawConfig(f)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			continue
		}
		m, err := parseJSONCBytes(data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		merged = mimoCodeDeepMerge(merged, m)
	}
	return merged, nil
}

// mimoCodeDeepMerge recursively merges src over dst (src wins per key) and
// returns dst. Nested objects are merged key-by-key; every other value
// (including arrays and scalars) is replaced wholesale by src's. This mirrors
// MiMoCode's `mergeDeep` (remeda) layer semantics — remeda's mergeDeep merges
// objects by key and REPLACES arrays/scalars (the lone exception in config.ts
// is the `instructions` array, irrelevant to the keyed `mcp` server map) — so a
// server defined in a lower layer survives when a higher layer carries only
// unrelated settings, while a same-named server in a higher layer wins. Kept
// LOCAL to the mimocode adapter (no shared-type change).
func mimoCodeDeepMerge(dst, src map[string]any) map[string]any {
	for k, sv := range src {
		if svMap, ok := sv.(map[string]any); ok {
			if dvMap, ok := dst[k].(map[string]any); ok {
				dst[k] = mimoCodeDeepMerge(dvMap, svMap)
				continue
			}
		}
		dst[k] = sv
	}
	return dst
}

// readJSON returns the deep-merged view across the resolved READ layer files.
// Used by GetEntry / AllStdioEntries / FindStdioLanguageServerEntries.
func (o *mimoCodeClient) readJSON() (map[string]any, error) {
	return o.readMergedLayers()
}

// MimoCodeMergedConfig is the SCAN-side entry point into the adapter's
// multi-layer merge: given a config path it returns the deep-merged top-level
// map across the resolved READ layer files (config.json + mimocode.json +
// mimocode.jsonc in the path's own directory, .jsonc winning per key) — or just
// the single supplied file for an explicit override path. Exported so
// internal/api/scan.go reuses the EXACT same layer resolution + JSONC-tolerant
// decode as the adapter, keeping the merge a single owner (no parallel
// reimplementation in scan.go) and keeping the shared clients types untouched.
//
// The MIMOCODE_CONFIG_DIR overlay is resolved from the live environment when
// the supplied path is a default-resolved global layer (so a scan/extract sees
// the same overlay the adapter does); an explicit/temp override path stays
// single-file (state-safe — never reaches the real config dir or the overlay).
// Returns an empty (non-nil) map when no layer file exists; a parse error on a
// present file propagates.
func MimoCodeMergedConfig(path string) (map[string]any, error) {
	overlay := ""
	if mimoCodeIsGlobalLayerName(filepath.Base(path)) {
		overlay = absoluteEnv("MIMOCODE_CONFIG_DIR")
	}
	return (&mimoCodeClient{path: path, overlayDir: overlay}).readMergedLayers()
}

// setMember sets mcp.<name> = value on the WRITE-target file (o.path),
// preserving the operator's comments + unrelated top-level keys when the file
// already has JSONC content. Writes always target the top layer; the bytes
// route through the UNCHANGED WriteConfigFile pipeline.
func (o *mimoCodeClient) setMember(name string, value any) error {
	return mutateJSONObjectMember(o.path, mimoCodeMCPKey, name, value, false)
}

// deleteMember removes mcp.<name> from the WRITE-target file (o.path) ONLY —
// the single top layer the hub writes (bot PR #420). It must NOT delete from a
// lower READ layer (config.json / mimocode.json): the hub physically cannot
// have written there (setMember only ever touches o.path), so a same-named
// lower-layer entry is the OPERATOR's, not the hub's. Removing it would destroy
// operator data the hub never owned; leaving it lets the merged read reveal the
// operator's original after the hub's top-layer entry is gone — exactly the
// restore-to-prior-state the rollback/demigrate contract provides.
// Delete-of-absent is a no-op (mutateJSONObjectMember returns nil on an
// empty/absent file), so this is idempotent.
func (o *mimoCodeClient) deleteMember(name string) error {
	return mutateJSONObjectMember(o.path, mimoCodeMCPKey, name, nil, true)
}

// AddEntry writes either the operator's verbatim prior entry (rollback restore
// path: entry.Raw != nil — Raw WINS, URL/Headers ignored, so a MiMoCode LOCAL
// entry with a `command` ARRAY round-trips byte-identical) or the hub-managed
// remote-HTTP entry (normal install path: entry.Raw == nil). MiMoCode's remote
// entry shape is `{"type":"remote","url":...,"enabled":true}`; an optional
// `headers` object is emitted when MCPEntry.Headers is non-empty.
func (o *mimoCodeClient) AddEntry(entry MCPEntry) error {
	// Raw-restore path FIRST: when GetEntry captured a non-representable prior
	// (a local command-array entry, or any url-less entry), the rollback calls
	// AddEntry(*prior). Write that raw value verbatim so the operator's original
	// is restored exactly — never re-projected onto a lossy {url:""} remote.
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
	// Comment-preserving delete on the top write layer only; absence is a no-op.
	return o.deleteMember(name)
}

// GetEntry reads mcp.<name> from the merged layers and projects it onto MCPEntry.
//
//   - A remote/URL entry → {Name, URL, Headers}, Raw left nil (the normal
//     hub-managed / user-remote shape; rollback restores it via the URL path).
//   - A URL-less LOCAL entry (type:"local", a `command` ARRAY, NO `url`) →
//     {Name, Raw: <the raw entry map>}, URL left empty. The lean MCPEntry has
//     no way to carry a command array, so Raw preserves the verbatim value. The
//     install/register rollback snapshots GetEntry before AddEntry and on a
//     downstream failure either AddEntry(*prior) or — when prior is nil —
//     RemoveEntry. Returning nil for a local entry would make the nil-prior
//     else-branch DELETE the operator's original; returning {Raw} makes
//     AddEntry(*prior) restore it verbatim instead (bot PR #420 P1).
//
// A missing entry returns (nil, nil).
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
	url, _ := raw["url"].(string)
	if url == "" {
		// URL-less local entry (or a url-less remote) — not representable as a
		// URL MCPEntry. Carry the verbatim value in Raw so rollback restores it
		// exactly rather than deleting it or corrupting it to {type:remote,url:""}.
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
// backup bytes verbatim regardless of shape. The restore/delete both touch the
// top write layer only (setMember/deleteMember on o.path), so a lower-layer
// operator original is preserved and re-emerges via the merge.
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

// AllStdioEntries returns every stdio entry from MiMoCode's merged top-level
// `mcp` key. The hub writes only HTTP-direct ("type":"remote") entries, which
// have no string `command` field and so are correctly skipped by
// collectStdioEntries. Operator-authored local ("type":"local") entries store
// `command` as an ARRAY (["npx","-y",...]) rather than a string;
// collectStdioEntries reads `command` as a string and therefore does not
// surface them — an accepted limitation of the shared cross-format cleanup
// scan (these helpers are best-effort stdio-leak detection, and the hub never
// writes the local shape). The Discovery/extract path DOES parse the array
// (see scanMimoCode + ExtractManifestFromClient), and the LSP scan below
// normalizes the array before delegating.
func (o *mimoCodeClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans the merged `mcp` for stdio entries
// matching the mcp-language-server invocation pattern. MiMoCode local entries
// store `command` as an ARRAY (["mcp-language-server","--lsp","go"]); the
// shared string-keyed matcher (findLanguageServerStdioInMap) reads `command` as
// a STRING and so would miss them. mimoCodeNormalizeCommandArray rewrites each
// entry's array `command` into the (command-string, prepended-args) shape the
// shared matcher understands — the same normalization the scan/extract paths
// already apply — so a hand-authored local mcp-language-server entry is found.
// The hub-written HTTP entries (no `command`) never match either way.
func (o *mimoCodeClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return findLanguageServerStdioInMap(mimoCodeNormalizeCommandArrays(servers)), nil
}

// mimoCodeNormalizeCommandArrays returns a shallow copy of servers in which any
// entry whose `command` is a MiMoCode ARRAY (["exe","arg0","arg1"]) is rewritten
// to the string-command shape the shared stdio/LSP matchers expect: `command` =
// the executable (first element) and `args` = the remaining array elements
// PREPENDED to any existing `args`. Entries with a string `command`, or no
// `command`, pass through unchanged. The per-entry maps are shallow-copied so
// the live merged config is not mutated. Returns nil for nil/empty input.
func mimoCodeNormalizeCommandArrays(servers map[string]any) map[string]any {
	if len(servers) == 0 {
		return servers
	}
	out := make(map[string]any, len(servers))
	for name, rawAny := range servers {
		raw, ok := rawAny.(map[string]any)
		if !ok {
			out[name] = rawAny
			continue
		}
		arr, isArr := raw["command"].([]any)
		if !isArr || len(arr) == 0 {
			out[name] = raw // string command or none — leave as-is
			continue
		}
		cmd, cmdArgs := mimoCodeSplitCommandArray(arr)
		if cmd == "" {
			out[name] = raw
			continue
		}
		// Shallow copy so the live merged map is untouched; rewrite command +
		// args. Array args ([...]any) are preserved/prepended in the same
		// element type the shared extractStringSlice/extractLspLanguageArg read.
		copied := make(map[string]any, len(raw))
		for k, v := range raw {
			copied[k] = v
		}
		copied["command"] = cmd
		merged := make([]any, 0, len(cmdArgs)+len(arr))
		for _, a := range cmdArgs {
			merged = append(merged, a)
		}
		if existing, ok := raw["args"].([]any); ok {
			merged = append(merged, existing...)
		}
		copied["args"] = merged
		out[name] = copied
	}
	return out
}

// mimoCodeSplitCommandArray splits a MiMoCode `command` ARRAY ([]any of
// strings) into (executable, trailing-args). Non-string elements are skipped.
// Returns ("", nil) when no string executable is present.
func mimoCodeSplitCommandArray(arr []any) (string, []string) {
	var parts []string
	for _, v := range arr {
		if s, ok := v.(string); ok {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
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

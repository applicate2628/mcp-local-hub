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
//     resolveMimoCodeGlobalDir — plus THREE custom-config sources layered ON TOP
//     of the global dir (config.ts loadInstanceState merges them AFTER
//     getGlobal(), each winning over what is below; none REPLACES the global
//     layers): MIMOCODE_CONFIG (a single config FILE merged above the global
//     layers), MIMOCODE_CONFIG_DIR (an overlay dir whose mimocode.json/.jsonc
//     merge above the MIMOCODE_CONFIG file), and MIMOCODE_CONFIG_CONTENT (an
//     INLINE JSONC config string merged as the TOP layer) — see
//     resolveMimoCodeConfig.
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
// The hub WRITES the GLOBAL dir's FIXED seed mimocode.json so a single per-user
// hub entry is visible in every project, matching every other adapter's
// user-scoped posture. READ (and the rollback restore) merge every active layer;
// WRITE, DELETE, and BACKUP touch the single FIXED write-target file ONLY
// (o.path = mimocode.json) — never a lower or higher layer. The write target
// does NOT float to mimocode.jsonc when that file exists (bot PR #420 finding 5:
// a floating target makes a later backup/demigrate miss the file the hub
// actually wrote). An operator's OTHER-layer original (config.json,
// mimocode.jsonc, MIMOCODE_CONFIG file, overlay, inline content) is never
// mutated and re-emerges via the merge after the hub's entry is removed; a
// same-named server in a HIGHER layer is refused at AddEntry by the shadow guard
// (it would otherwise win the merge and silently mask the hub entry). The
// project-root / cross-DIR `.mimocode` overlay (project + home) is a documented
// LIMITATION shared with the OpenCode adapter — this adapter targets the
// resolved global dir plus the MIMOCODE_CONFIG file, the MIMOCODE_CONFIG_DIR
// overlay, and MIMOCODE_CONFIG_CONTENT inline only.
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
//   - config.ts (packages/opencode/src/config/config.ts) — `Config.layer` →
//     `loadInstanceState` merges, lowest→highest (later merges win via
//     mergeDeep): `getGlobal()` (= loadGlobal: config.json → mimocode.json →
//     mimocode.jsonc) then `if (Flag.MIMOCODE_CONFIG) merge(loadFile(...))` then
//     the per-directory loop (`.mimocode` dirs + MIMOCODE_CONFIG_DIR:
//     mimocode.json → mimocode.jsonc) then
//     `if (process.env.MIMOCODE_CONFIG_CONTENT) merge(loadConfig(...))`. So the
//     MIMOCODE_CONFIG file is a LAYER above global (NOT a replacement),
//     MIMOCODE_CONFIG_DIR is above it, and MIMOCODE_CONFIG_CONTENT (inline JSONC
//     parsed via loadConfig → ConfigParse.jsonc) is the TOP in-scope layer.
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
	return newLockingClient(&mimoCodeClient{
		path:          r.writeTarget,
		configFile:    r.configFile,
		overlayDir:    r.overlayDir,
		inlineContent: r.inlineContent,
	}), nil
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

// mimoCodeResolution is the fully resolved config surface, faithful to
// MiMoCode's `Config.layer` → `loadInstanceState` merge order (verified against
// the live source, see the package doc). All env-resolved READ layers above the
// global write target are recorded; the write target itself is the FIXED global
// seed (never floats — bot PR #420 finding 5).
//
//   - writeTarget — the single global-dir file every WRITE + DELETE + BACKUP
//     touches (o.path). It is ALWAYS mimocode.json in the resolved global dir
//     (the deterministic seed); it does NOT float to mimocode.jsonc when that
//     exists, so a backup/demigrate always hits the exact file the hub wrote.
//   - configFile — the MIMOCODE_CONFIG absolute FILE, a READ LAYER merged ABOVE
//     the global layers (NOT a replacement — bot PR #420 finding 1). "" when
//     unset / relative. The hub never WRITES it (operator-owned).
//   - overlayDir — the MIMOCODE_CONFIG_DIR absolute overlay DIR whose
//     mimocode.json/.jsonc merge above configFile. "" when unset / relative.
//   - inlineContent — the MIMOCODE_CONFIG_CONTENT raw JSONC STRING merged as the
//     TOP layer (highest in-scope precedence — bot PR #420 finding 4). "" when
//     unset. It is content, not a path, so it threads into the merge separately
//     from the file layers.
type mimoCodeResolution struct {
	writeTarget   string // FIXED global seed file WRITE + DELETE + BACKUP touch (o.path)
	configFile    string // MIMOCODE_CONFIG file — a READ LAYER above global ("" when unset)
	overlayDir    string // MIMOCODE_CONFIG_DIR overlay read above configFile ("" when unset)
	inlineContent string // MIMOCODE_CONFIG_CONTENT inline JSONC string — TOP read layer ("" when unset)
}

// resolveMimoCodeConfig resolves MiMoCode's config surface honoring its
// documented merge order (config.ts `loadInstanceState`, verified source):
//
//   - The GLOBAL dir is resolved (MIMOCODE_HOME > XDG_CONFIG_HOME >
//     ~/.config/mimocode). The write target is the FIXED seed mimocode.json in
//     that dir — it does NOT float to mimocode.jsonc even when that file
//     exists (bot PR #420 finding 5: a floating write target makes a later
//     backup/demigrate miss the layer the hub actually wrote). config.json is
//     never a write target (a read-only legacy migration sink).
//   - MIMOCODE_CONFIG (absolute FILE) is a READ LAYER merged ABOVE the global
//     layers (bot PR #420 finding 1). MiMoCode merges it ON TOP of getGlobal()
//     — `if (Flag.MIMOCODE_CONFIG) merge(loadFile(Flag.MIMOCODE_CONFIG))` runs
//     AFTER `merge(getGlobal())` — it is NOT a replacement. The hub never
//     WRITES it (operator-owned); it only contributes to READS + shadow checks.
//   - MIMOCODE_CONFIG_DIR (absolute DIR) is the overlay read ABOVE configFile
//     (config.ts's per-directory loop runs after the MIMOCODE_CONFIG merge);
//     custom wins per key. It does NOT become the write target.
//   - MIMOCODE_CONFIG_CONTENT is an INLINE JSONC config string merged as the
//     TOP layer (config.ts merges `process.env.MIMOCODE_CONFIG_CONTENT` last of
//     the in-scope sources — bot PR #420 finding 4). It is content, not a path,
//     so it is carried as a raw string.
//
// Relative env values are IGNORED for the PATH vars (global.ts rejects a
// relative MIMOCODE_HOME outright and the XDG spec ignores a relative
// XDG_CONFIG_HOME). MIMOCODE_CONFIG_CONTENT is read raw (it is content, not a
// path).
func resolveMimoCodeConfig(home string) mimoCodeResolution {
	globalDir := resolveMimoCodeGlobalDir(home)
	return mimoCodeResolution{
		writeTarget:   mimoCodeWriteTargetInDir(globalDir),
		configFile:    absoluteEnv("MIMOCODE_CONFIG"),
		overlayDir:    absoluteEnv("MIMOCODE_CONFIG_DIR"),
		inlineContent: os.Getenv("MIMOCODE_CONFIG_CONTENT"),
	}
}

// mimoCodeWriteTargetInDir returns the WRITE-target file in a config dir: ALWAYS
// the fixed seed mimocode.json. It deliberately does NOT float to mimocode.jsonc
// when that file exists.
//
// Why a fixed seed (bot PR #420 finding 5). Backup() / BackupKeep() /
// LatestBackupPath() / ConfigPath() are NAME-LESS in the Client interface and
// key off o.path alone; the demigrate/install sequence calls Backup() (name-
// less) then RemoveEntry(server) (named) against the SAME adapter, and both must
// resolve to the one file the hub actually wrote. The old floating target
// (.jsonc when present) broke that: a hub install while only mimocode.json
// existed wrote the entry to mimocode.json, but if the operator LATER created
// mimocode.jsonc the write target floated to .jsonc — so backup looked for
// .jsonc backups and RemoveEntry deleted from .jsonc, MISSING the .json backups
// and leaving the hub entry stranded in mimocode.json. A fixed seed keeps
// write/delete/backup mutually consistent forever.
//
// The shadowing concern the float was (wrongly) meant to address — a same-named
// server in the higher mimocode.jsonc layer winning the merge over the hub's
// mimocode.json entry — is handled by the shadow guard (AddEntry refuses when a
// higher layer defines mcp.<name>), NOT by writing into the higher layer.
//
// config.json is never a write target (a read-only legacy migration layer).
func mimoCodeWriteTargetInDir(dir string) string {
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

// mimoCodeClient is a standalone adapter (NOT an embedding of jsonMCPClient)
// because MiMoCode uses the top-level `mcp` key rather than the JSON family's
// `mcpServers`, AND a distinct entry shape (`type:"remote"` + `enabled:true`
// rather than `disabled:false`). It mirrors the OpenCode adapter's
// standalone-struct + HTTP-direct pattern with the same key/field set, plus
// MiMoCode-faithful multi-layer reads (config.json/mimocode.json/.jsonc + the
// MIMOCODE_CONFIG file + an optional MIMOCODE_CONFIG_DIR overlay +
// MIMOCODE_CONFIG_CONTENT inline) and fixed-seed single-target writes/deletes.
//
// path is the single FIXED WRITE + DELETE + BACKUP target (always the global
// seed mimocode.json — never floats; bot PR #420 finding 5). The three
// env-resolved READ layers above it merge in MiMoCode's source order (config.ts
// loadInstanceState): configFile (MIMOCODE_CONFIG) above the global layers, then
// the overlayDir (MIMOCODE_CONFIG_DIR) above that, then inlineContent
// (MIMOCODE_CONFIG_CONTENT) as the TOP layer. The hub WRITES only path; the
// three extra layers are operator-owned READ + shadow-check inputs.
//
// Direct construction (`&mimoCodeClient{path}` in tests / scan / extract) leaves
// configFile / overlayDir / inlineContent empty, so the read set collapses to
// path's own directory layers (or just path for an explicit non-layer-named
// override) with no env-resolved sources — the state-safe default that never
// reaches the developer's real ~/.config/mimocode.
//
//   - configFile (MIMOCODE_CONFIG) — an absolute FILE read as a LAYER ABOVE the
//     global dir layers (bot PR #420 finding 1). It is NOT a replacement: the
//     global layers stay in the read set. The hub never writes it.
//   - overlayDir (MIMOCODE_CONFIG_DIR) — an absolute DIR whose
//     mimocode.json/.jsonc merge ABOVE configFile.
//   - inlineContent (MIMOCODE_CONFIG_CONTENT) — a raw JSONC STRING merged as the
//     TOP read layer (bot PR #420 finding 4). It is content, not a path, so it
//     is parsed and merged after the file layers in readMergedLayers.
type mimoCodeClient struct {
	path          string
	configFile    string
	overlayDir    string
	inlineContent string
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

// readLayerFiles returns every config FILE the READ merge consumes, LOWEST
// precedence first. This is deliberately BROADER than the single write/delete
// target (o.path): a lower-layer entry must be VISIBLE so the merge reveals an
// operator's original after the hub's top-layer entry is removed.
//
// NOTE: this returns FILE paths only. The MIMOCODE_CONFIG_CONTENT inline layer
// (o.inlineContent) is a STRING, not a path, so it does NOT appear here — it is
// merged as the TOP layer separately inside readMergedLayers.
//
// Resolution (state-safe + explicit-path honoring, faithful to config.ts
// loadInstanceState order):
//
//   - Explicit override (o.path's basename is NOT a known global layer name — a
//     temp/test path): return ONLY [o.path]. No dir probing, no siblings, no
//     MIMOCODE_CONFIG file, no overlay — so a temp/test scan never reaches the
//     real ~/.config/mimocode. (Direct construction also leaves configFile /
//     overlayDir empty, so this branch is doubly state-safe.)
//   - Known global layer name (the live NewMimoCode write target, basename
//     mimocode.json): return, lowest→highest —
//       1. config.json + mimocode.json + mimocode.jsonc in o.path's own dir
//          (the global getGlobal() layers, .jsonc winning);
//       2. configFile (MIMOCODE_CONFIG), when set, ABOVE the global layers
//          (bot PR #420 finding 1 — it merges on top, NOT a replacement);
//       3. when overlayDir (MIMOCODE_CONFIG_DIR) is set, its mimocode.json +
//          mimocode.jsonc appended LAST so the overlay wins per key.
//
// It never recomputes a directory from env/home beyond the configFile /
// overlayDir already captured at construction; it stays inside dir(o.path) plus
// those explicit paths.
func (o *mimoCodeClient) readLayerFiles() []string {
	return mimoCodeReadLayerFiles(o.path, o.configFile, o.overlayDir)
}

// mimoCodeReadLayerFiles is the pure resolver behind
// (*mimoCodeClient).readLayerFiles. See that method's doc for the rules. The
// basename heuristic is the state-safe single-file collapse for direct
// (scan/extract/test) construction whose path is not a known global layer name.
func mimoCodeReadLayerFiles(path, configFile, overlayDir string) []string {
	base := filepath.Base(path)
	if !mimoCodeIsGlobalLayerName(base) {
		// Explicit override (temp/test path, basename not a global layer name):
		// operate only on the supplied file; do NOT recompute the dir, pull
		// siblings, the MIMOCODE_CONFIG file, or the overlay.
		return []string{path}
	}
	dir := filepath.Dir(path)
	files := make([]string, 0, len(mimoCodeGlobalLayerNames)+1+len(mimoCodeOverlayLayerNames))
	for _, n := range mimoCodeGlobalLayerNames {
		files = append(files, filepath.Join(dir, n))
	}
	// MIMOCODE_CONFIG file: a single FILE merged ABOVE the global layers.
	if configFile != "" {
		files = append(files, configFile)
	}
	// MIMOCODE_CONFIG_DIR overlay: ABOVE the MIMOCODE_CONFIG file.
	if overlayDir != "" {
		for _, n := range mimoCodeOverlayLayerNames {
			files = append(files, filepath.Join(overlayDir, n))
		}
	}
	return files
}

// mimoCodeIsGlobalLayerName reports whether base is one of the global-dir layer
// file names (config.json / mimocode.json / mimocode.jsonc). Used to decide
// whether a supplied path is a default-resolved global layer (the full merge —
// global siblings + the env-resolved MIMOCODE_CONFIG file / overlay / inline
// content — applies) or a state-safe explicit override (a temp/test path whose
// basename is not a global layer name → single-file read, no env-resolved
// sources).
func mimoCodeIsGlobalLayerName(base string) bool {
	for _, n := range mimoCodeGlobalLayerNames {
		if base == n {
			return true
		}
	}
	return false
}

// ErrMimoCodeOverlayShadowsServer is returned by (*mimoCodeClient).AddEntry when
// a FILE-based read layer ABOVE the hub's global write target already defines
// mcp.<Server>. The hub writes only the lowest writable global layer
// (WriteTarget = mimocode.json); any higher layer merges on top and WINS, so the
// hub entry would never take effect. The shadowing layer can be (highest→lowest
// precedence): the MIMOCODE_CONFIG_DIR overlay, the MIMOCODE_CONFIG file, or the
// global higher layer mimocode.jsonc. SourceLabel names which kind it is (for
// the actionable message); SourceFile is the offending file path. Rather than
// silently report success while MiMoCode keeps using the shadowing definition,
// the adapter refuses the write (bot PR #420 findings 4+7). Writing into the
// higher layer instead is rejected by design — Backup / ConfigPath / demigrate
// are all anchored to the single global write target, so a per-layer target
// would orphan the backup and break the demigrate round-trip — and editing the
// operator-owned higher layer is out of the hub's write ownership.
//
// The inline MIMOCODE_CONFIG_CONTENT layer shadows too, but it has no file path;
// that case uses the distinct ErrMimoCodeInlineContentShadowsServer below.
type ErrMimoCodeOverlayShadowsServer struct {
	Server      string
	WriteTarget string
	SourceLabel string // human label of the shadowing layer, e.g. "MIMOCODE_CONFIG_DIR overlay"
	SourceFile  string // the offending file path
	// OverlayFile is retained as an alias of SourceFile for backward
	// compatibility with callers that named the overlay file directly.
	OverlayFile string
}

func (e *ErrMimoCodeOverlayShadowsServer) Error() string {
	return fmt.Sprintf("mimocode: server %q is already defined in the %s %q, which MiMoCode merges on top of the hub's write target %q and wins — the hub entry would have no effect; remove or rename mcp.%s there (or unset the relevant env var), then retry",
		e.Server, e.SourceLabel, e.SourceFile, e.WriteTarget, e.Server)
}

// ErrMimoCodeInlineContentShadowsServer is returned by AddEntry when the inline
// MIMOCODE_CONFIG_CONTENT layer already defines mcp.<Server>. Distinct from the
// file-based shadow error because there is NO file to name — conflating it into
// the file error would force a misleading empty/sentinel file path. The operator
// must edit the env var, not a file.
type ErrMimoCodeInlineContentShadowsServer struct {
	Server      string
	WriteTarget string
}

func (e *ErrMimoCodeInlineContentShadowsServer) Error() string {
	return fmt.Sprintf("mimocode: server %q is already defined in MIMOCODE_CONFIG_CONTENT (the inline config string), which MiMoCode merges on top of the hub's write target %q and wins — the hub entry would have no effect; remove mcp.%s from MIMOCODE_CONFIG_CONTENT, or unset that env var, then retry",
		e.Server, e.WriteTarget, e.Server)
}

// mimoCodeShadowSource identifies the single highest-precedence READ layer above
// the hub's write target that defines mcp.<name>. Kind is "" when nothing
// shadows; "inline" for the MIMOCODE_CONFIG_CONTENT layer (File is ""); "file"
// for any file layer (File names it, Label describes it).
type mimoCodeShadowSource struct {
	Kind  string // "", "inline", or "file"
	Label string // human label for the "file" kind
	File  string // file path for the "file" kind
}

// mimoCodeHigherLayerDefining walks every READ layer ABOVE the hub's global
// write target (mimocode.json) from HIGHEST precedence DOWN and returns the
// FIRST that defines mcp.<name> — i.e. the layer that actually wins the merge,
// the one the operator must edit to un-shadow. Order (highest→lowest):
//
//  1. MIMOCODE_CONFIG_CONTENT inline string
//  2. MIMOCODE_CONFIG_DIR overlay: mimocode.jsonc then mimocode.json
//  3. MIMOCODE_CONFIG file
//  4. global higher layer mimocode.jsonc (sibling of the write target)
//
// config.json and the write-target mimocode.json itself are BELOW the write
// target / are the write target, so they cannot shadow and are not checked. It
// reads ONLY the explicit configFile / overlayDir / inlineContent captured at
// construction plus the write target's own dir (state-safe — never recomputes a
// dir from env/home). A parse error on a present layer propagates (a malformed
// higher layer must not be silently treated as "no shadow" — that would
// re-introduce the silent-false-success this guards).
func (o *mimoCodeClient) mimoCodeHigherLayerDefining(name string) (mimoCodeShadowSource, error) {
	// 1. Inline content (highest).
	if o.inlineContent != "" {
		defined, err := mimoCodeInlineDefines(o.inlineContent, name)
		if err != nil {
			return mimoCodeShadowSource{}, err
		}
		if defined {
			return mimoCodeShadowSource{Kind: "inline"}, nil
		}
	}
	// 2. MIMOCODE_CONFIG_DIR overlay (mimocode.jsonc preferred over mimocode.json).
	if o.overlayDir != "" {
		for i := len(mimoCodeOverlayLayerNames) - 1; i >= 0; i-- {
			f := filepath.Join(o.overlayDir, mimoCodeOverlayLayerNames[i])
			defined, err := mimoCodeFileDefines(f, name)
			if err != nil {
				return mimoCodeShadowSource{}, err
			}
			if defined {
				return mimoCodeShadowSource{Kind: "file", Label: "MIMOCODE_CONFIG_DIR overlay", File: f}, nil
			}
		}
	}
	// 3. MIMOCODE_CONFIG file.
	if o.configFile != "" {
		defined, err := mimoCodeFileDefines(o.configFile, name)
		if err != nil {
			return mimoCodeShadowSource{}, err
		}
		if defined {
			return mimoCodeShadowSource{Kind: "file", Label: "MIMOCODE_CONFIG file", File: o.configFile}, nil
		}
	}
	// 4. global higher layer mimocode.jsonc (sibling of the write target).
	jsonc := filepath.Join(filepath.Dir(o.path), "mimocode.jsonc")
	if jsonc != o.path { // never treat the write target itself as a shadow
		defined, err := mimoCodeFileDefines(jsonc, name)
		if err != nil {
			return mimoCodeShadowSource{}, err
		}
		if defined {
			return mimoCodeShadowSource{Kind: "file", Label: "global higher layer", File: jsonc}, nil
		}
	}
	return mimoCodeShadowSource{}, nil
}

// mimoCodeFileDefines reports whether the JSONC file at path defines mcp.<name>.
// A missing/empty file → false; a parse error on present bytes propagates.
func mimoCodeFileDefines(path, name string) (bool, error) {
	data, err := readRawConfig(path)
	if err != nil {
		return false, err
	}
	if len(data) == 0 {
		return false, nil
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	return mimoCodeMapDefines(m, name), nil
}

// mimoCodeInlineDefines reports whether the inline JSONC string defines
// mcp.<name>. A parse error propagates.
func mimoCodeInlineDefines(content, name string) (bool, error) {
	m, err := parseJSONCBytes([]byte(content))
	if err != nil {
		return false, fmt.Errorf("parse MIMOCODE_CONFIG_CONTENT: %w", err)
	}
	return mimoCodeMapDefines(m, name), nil
}

// mimoCodeMapDefines reports whether a parsed config map carries mcp.<name>.
func mimoCodeMapDefines(m map[string]any, name string) bool {
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false
	}
	_, present := servers[name]
	return present
}

// readMergedLayers reads every READ layer and DEEP-MERGES them lowest-first
// (config.json base, mimocode.json over it, mimocode.jsonc over that, then the
// MIMOCODE_CONFIG file, then the MIMOCODE_CONFIG_DIR overlay's
// mimocode.json/.jsonc, and finally the MIMOCODE_CONFIG_CONTENT inline string as
// the TOP layer) so an entry present in any layer is visible and the highest
// layer wins per key. This is the verified config.ts loadInstanceState order.
// Missing file layers are skipped (treated as empty); a parse error on a present
// layer (file OR inline) propagates. JSONC (comments + trailing commas) is
// tolerated via the shared parseJSONCBytes helper.
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
	// MIMOCODE_CONFIG_CONTENT inline layer — a JSONC STRING, not a file path, so
	// it merges AFTER (above) every file layer (bot PR #420 finding 4). Empty
	// when unset OR in the state-safe single-file mode (direct/test construction
	// leaves o.inlineContent empty). A parse error on a present inline string
	// propagates — a malformed MIMOCODE_CONFIG_CONTENT must not be silently
	// dropped (that would re-introduce a silent-wrong-merge).
	if o.inlineContent != "" {
		m, err := parseJSONCBytes([]byte(o.inlineContent))
		if err != nil {
			return nil, fmt.Errorf("parse MIMOCODE_CONFIG_CONTENT: %w", err)
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
// The MIMOCODE_CONFIG file, MIMOCODE_CONFIG_DIR overlay, and
// MIMOCODE_CONFIG_CONTENT inline layer are resolved from the live environment
// when the supplied path is a default-resolved global layer (so a scan/extract
// sees the same merged view the adapter does); an explicit/temp override path
// stays single-file (state-safe — never reaches the real config dir, the
// MIMOCODE_CONFIG file, the overlay, or the inline content). Returns an empty
// (non-nil) map when no layer exists; a parse error on a present layer
// propagates.
func MimoCodeMergedConfig(path string) (map[string]any, error) {
	return mimoCodeClientForScanPath(path).readMergedLayers()
}

// MimoCodeReadLayerPaths returns the resolved READ-layer FILE paths for a
// scan-supplied config path, using the SAME layer resolution as
// MimoCodeMergedConfig (config.json + mimocode.json + mimocode.jsonc in the
// path's own dir, plus the env-resolved MIMOCODE_CONFIG file and
// MIMOCODE_CONFIG_DIR overlay; or just the single file for an explicit/temp
// override). Exported so internal/api/scan.go can decide MiMoCode's config
// PRESENCE from the actual layer files rather than only the (possibly absent)
// write target — a profile whose servers live only in a lower config.json layer,
// the MIMOCODE_CONFIG file, or the overlay must still be scanned (bot PR #420
// finding 2). The returned paths are candidates; the caller stats them. No file
// is read here. NOTE: the MIMOCODE_CONFIG_CONTENT inline layer has no file path,
// so it does not appear here — a host whose ONLY config is inline content has no
// presence file to stat, which is correct (there is no file to scan).
func MimoCodeReadLayerPaths(path string) []string {
	return mimoCodeClientForScanPath(path).readLayerFiles()
}

// mimoCodeClientForScanPath builds the read-only client a scan/extract entry
// point uses for a supplied config path, resolving the MIMOCODE_CONFIG file, the
// MIMOCODE_CONFIG_DIR overlay, and the MIMOCODE_CONFIG_CONTENT inline content
// from the live environment — but ONLY when the path is a known global layer
// name (so a temp/test path stays state-safe single-file). Single owner of the
// scan-side path→resolution mapping so the merged-read and the layer-path probe
// never diverge.
func mimoCodeClientForScanPath(path string) *mimoCodeClient {
	if !mimoCodeIsGlobalLayerName(filepath.Base(path)) {
		// Explicit/temp override — state-safe single-file, no env-resolved layers.
		return &mimoCodeClient{path: path}
	}
	return &mimoCodeClient{
		path:          path,
		configFile:    absoluteEnv("MIMOCODE_CONFIG"),
		overlayDir:    absoluteEnv("MIMOCODE_CONFIG_DIR"),
		inlineContent: os.Getenv("MIMOCODE_CONFIG_CONTENT"),
	}
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
	// is restored exactly — never re-projected onto a lossy {url:""} remote. The
	// rollback restore is exempt from the overlay-shadow guard below: it
	// re-asserts a prior state on the write target (o.path), not a new
	// hub-managed install, and must never be blocked.
	if entry.Raw != nil {
		return o.setMember(entry.Name, entry.Raw)
	}
	// Higher-layer-shadow guard (normal install path only, bot PR #420 findings
	// 4+7). The hub writes ONLY the global write target (o.path = mimocode.json);
	// EVERY read layer above it merges ON TOP and WINS per key: the global
	// mimocode.jsonc, the MIMOCODE_CONFIG file, the MIMOCODE_CONFIG_DIR overlay,
	// and the MIMOCODE_CONFIG_CONTENT inline content. So when ANY of them already
	// defines mcp.<name>, a global write would "succeed" yet MiMoCode would keep
	// resolving the server from the shadowing layer — a silent false success. We
	// cannot redirect the write into the higher layer (Backup/ConfigPath/demigrate
	// are anchored to the single o.path; a per-layer target would orphan the
	// backup and break the demigrate round-trip), and we must not edit the
	// operator-owned higher layer. So we FAIL LOUD with an actionable typed error
	// naming the highest-precedence shadowing source — converting a silent
	// wrong-state into a visible install failure the operator can fix.
	if shadow, err := o.mimoCodeHigherLayerDefining(entry.Name); err != nil {
		return err
	} else if shadow.Kind == "inline" {
		return &ErrMimoCodeInlineContentShadowsServer{Server: entry.Name, WriteTarget: o.path}
	} else if shadow.Kind == "file" {
		return &ErrMimoCodeOverlayShadowsServer{
			Server:      entry.Name,
			WriteTarget: o.path,
			SourceLabel: shadow.Label,
			SourceFile:  shadow.File,
			OverlayFile: shadow.File,
		}
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

// mimoCodeHubRemoteShapeKeys are the ONLY keys the hub's AddEntry remote-HTTP
// shape ever writes: `type`, `url`, `enabled`, and the optional `headers`. A
// remote entry whose key set is a subset of these is faithfully representable as
// a lean {URL,Headers} MCPEntry (so GetEntry can leave Raw nil and the normal
// URL round-trip applies). A remote entry carrying ANY other key (oauth, timeout,
// or any MiMoCode MCP-schema field beyond this set) is NOT — see GetEntry.
var mimoCodeHubRemoteShapeKeys = map[string]bool{
	"type": true, "url": true, "enabled": true, "headers": true,
}

// GetEntry reads mcp.<name> from the merged layers and projects it onto MCPEntry.
//
//   - A clean hub-shaped ENABLED remote entry (only type/url/enabled/headers,
//     enabled not false) → {Name, URL, Headers}, Raw left nil (the normal
//     hub-managed / user-remote shape; rollback restores it via the URL path).
//   - A URL-less LOCAL entry (type:"local", a `command` ARRAY, NO `url`) →
//     {Name, Raw: <the raw entry map>}, URL left empty. The lean MCPEntry has
//     no way to carry a command array, so Raw preserves the verbatim value. The
//     install/register rollback snapshots GetEntry before AddEntry and on a
//     downstream failure either AddEntry(*prior) or — when prior is nil —
//     RemoveEntry. Returning nil for a local entry would make the nil-prior
//     else-branch DELETE the operator's original; returning {Raw} makes
//     AddEntry(*prior) restore it verbatim instead (bot PR #420 P1).
//   - A DISABLED remote entry (enabled:false) → {Name, Raw}, URL empty (bot PR
//     #420 finding 5 — the lean path hardcodes enabled:true, which would
//     re-enable it on rollback).
//   - A user-authored remote entry carrying EXTRA fields beyond the hub shape
//     (oauth, timeout, or any other MiMoCode MCP-schema key) → {Name, Raw}, URL
//     empty (bot PR #420 finding 3). The lean {URL,Headers} snapshot is LOSSY
//     for those fields, so a GetEntry→AddEntry(*prior) rollback would rewrite
//     the entry as the bare {type:remote,url,enabled:true,headers} shape and
//     drop oauth/timeout. Carrying Raw makes the rollback restore the verbatim
//     shape (Raw wins in AddEntry).
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
	// A DISABLED URL entry (enabled:false) is also not faithfully representable
	// as a {URL,Headers} MCPEntry: AddEntry's normal install path hardcodes
	// enabled:true, so a GetEntry→AddEntry(*prior) rollback would silently
	// RE-ENABLE a server the operator had disabled (bot PR #420 finding 5).
	// Carry the verbatim entry in Raw so the rollback writes it back byte-shaped
	// (Raw wins in AddEntry), preserving enabled:false.
	if enabled, present := raw["enabled"]; present {
		if b, ok := enabled.(bool); ok && !b {
			return &MCPEntry{Name: name, Raw: raw}, nil
		}
	}
	// A user-authored remote entry carrying EXTRA fields beyond the hub-written
	// shape (type/url/enabled/headers) — e.g. oauth, timeout (accepted by
	// MiMoCode's MCP schema) — is ALSO not faithfully representable by the lean
	// {URL,Headers} MCPEntry: a GetEntry→AddEntry(*prior) rollback would re-emit
	// only the bare remote shape and DROP those fields (bot PR #420 finding 3).
	// Carry the verbatim entry in Raw so the rollback restores it byte-shaped. A
	// clean hub-shaped remote entry (key set ⊆ {type,url,enabled,headers}) stays
	// on the lean {URL,Headers} path (Raw nil) so the normal hub-install/restore
	// polarity and every existing URL round-trip are unchanged.
	if mimoCodeRemoteHasExtraFields(raw) {
		return &MCPEntry{Name: name, Raw: raw}, nil
	}
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(raw, "headers")}, nil
}

// mimoCodeRemoteHasExtraFields reports whether a remote entry map carries any
// key beyond the clean hub-written remote shape (type/url/enabled/headers). Such
// an entry is user-authored with fields the lean {URL,Headers} MCPEntry cannot
// represent, so GetEntry must snapshot it via Raw for a verbatim rollback.
func mimoCodeRemoteHasExtraFields(raw map[string]any) bool {
	for k := range raw {
		if !mimoCodeHubRemoteShapeKeys[k] {
			return true
		}
	}
	return false
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
//
// enabled:false entries are dropped FIRST (bot PR #420 finding 3): MiMoCode uses
// `enabled` (default true) as the active flag and never spawns a disabled entry,
// so the `language-server cleanup` / post-register cleanup path must not
// report/remove a disabled mcp-language-server entry. This mirrors the scan
// path's enabled-handling (shapeMimoCodeEntry classifies enabled:false as an
// absent presence), keeping the cleanup view consistent with what MiMoCode
// actually loads. The shared findLanguageServerStdioInMap does not itself
// inspect `enabled` (it serves every adapter format), so the filter is applied
// here in the mimo-local path.
func (o *mimoCodeClient) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return findLanguageServerStdioInMap(mimoCodeNormalizeCommandArrays(mimoCodeDropDisabled(servers))), nil
}

// mimoCodeDropDisabled returns a shallow copy of servers with every entry whose
// `enabled` is explicitly false removed. A missing or non-bool `enabled` is
// treated as active (MiMoCode's default). Non-map entries pass through (the
// downstream matcher ignores them). Returns servers unchanged for nil/empty
// input. The map is shallow-copied so the live merged config is not mutated.
func mimoCodeDropDisabled(servers map[string]any) map[string]any {
	if len(servers) == 0 {
		return servers
	}
	out := make(map[string]any, len(servers))
	for name, rawAny := range servers {
		if raw, ok := rawAny.(map[string]any); ok {
			if enabled, present := raw["enabled"]; present {
				if b, ok := enabled.(bool); ok && !b {
					continue // disabled: MiMoCode never spawns it
				}
			}
		}
		out[name] = rawAny
	}
	return out
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

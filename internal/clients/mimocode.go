package clients

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// ## Scope — which of MiMoCode's 13 config layers this adapter reads, merges,
// shadow-detects, and writes (the AGREED PR #420 scope, not an omission).
//
// MiMoCode resolves MCP entries from up to 13 layers (config.ts loadInstanceState,
// lowest→highest): remote well-known < global(config.json<mimocode.json<.jsonc) <
// MIMOCODE_CONFIG < project .mimocode/ < MIMOCODE_CONFIG_DIR < Claude commands <
// .mimocode dir < MIMOCODE_CONFIG_CONTENT < org account < managed config dir <
// macOS Managed Preferences < ~/.claude.json + <dir>/.claude.json MCP import
// (skip-if-name-exists, gated by MIMOCODE_DISABLE_CLAUDE_CODE_MCP) < legacy
// migration. The hub splits these into THREE buckets:
//
//   - WRITABLE layers — FULLY MERGED for read + shadow, and the hub WRITES exactly
//     ONE of them (the global mimocode.json write target). These are: the global
//     dir layers (config.json / mimocode.json / mimocode.jsonc), the MIMOCODE_CONFIG
//     file, the MIMOCODE_CONFIG_DIR overlay, and MIMOCODE_CONFIG_CONTENT inline.
//     A same-name entry in a writable layer ABOVE the write target is refused at
//     AddEntry (it would win the merge and mask the hub entry) — fail loud, never
//     silent success.
//   - The ~/.claude.json MCP import (writable-ADJACENT) — READ-ONLY into the
//     effective merge so a Claude-imported server is discoverable / extractable /
//     shadow-detected, honoring skip-if-name-exists (an explicit mimo entry wins)
//     and MIMOCODE_DISABLE_CLAUDE_CODE_MCP. The hub NEVER writes .claude.json. Only
//     the home ~/.claude.json is imported (the single-target model has no project
//     ctx for the <dir>/.claude.json sibling — a documented narrowing of
//     config.ts:889, justified because the hub has no project directory concept).
//   - Read-only MANAGED layers — DETECT-AND-FAIL-LOUD ONLY, NEVER merged into the
//     writable path and NEVER written. macOS Managed Preferences (MDM) is the one
//     implemented here: on macOS, if the managed plist defines the server being
//     installed, AddEntry returns a typed loud error rather than reporting a
//     successful write to mimocode.json (MiMoCode would keep resolving the MDM
//     entry — a silent false success). The not-yet-implemented managed layers (org
//     account, remote well-known, managed config dir) follow the SAME
//     detect-and-fail-loud principle when added; their absence here is a scoped
//     deferral, not a contract gap.
//
// This three-bucket split is the agreed PR #420 scope: read/merge the writable
// layers + the home ~/.claude.json import; detect-and-fail-loud the managed
// layers; write only the single writable global write target.
//
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
	r, err := resolveMimoCodeConfig(home)
	if err != nil {
		// A relative / ~/-prefixed MIMOCODE_HOME is an operator error MiMoCode
		// itself rejects at startup (global.ts:26-50 THROWS); surface it loud
		// here so the client never silently constructs against the wrong
		// ~/.config/mimocode profile and reports a false-success install (bot PR
		// #420 finding 5). The matrix renders the failed client as not-installed.
		return nil, err
	}
	return newLockingClient(&mimoCodeClient{
		path:          r.writeTarget,
		configFile:    r.configFile,
		overlayDir:    r.overlayDir,
		inlineContent: r.inlineContent,
		claudeHome:    home,
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
//   - configFile — the MIMOCODE_CONFIG FILE, a READ LAYER merged ABOVE the
//     global layers (NOT a replacement — bot PR #420 finding 1). Absolute, with a
//     relative value resolved from the process cwd (bot PR #420 finding 3). ""
//     when unset. The hub never WRITES it (operator-owned).
//   - overlayDir — the MIMOCODE_CONFIG_DIR overlay DIR whose mimocode.json/.jsonc
//     merge above configFile. Absolute, with a relative value resolved from the
//     process cwd (bot PR #420 finding 3). "" when unset.
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
//   - MIMOCODE_CONFIG (a FILE, absolute or cwd-relative — bot PR #420 finding 3)
//     is a READ LAYER merged ABOVE the global layers (bot PR #420 finding 1).
//     MiMoCode merges it ON TOP of getGlobal() — `if (Flag.MIMOCODE_CONFIG)
//     merge(loadFile(Flag.MIMOCODE_CONFIG))` runs AFTER `merge(getGlobal())` — it
//     is NOT a replacement. The hub never WRITES it (operator-owned); it only
//     contributes to READS + shadow checks.
//   - MIMOCODE_CONFIG_DIR (a DIR, absolute or cwd-relative — bot PR #420 finding
//     3) is the overlay read ABOVE configFile (config.ts's per-directory loop runs
//     after the MIMOCODE_CONFIG merge); custom wins per key. It does NOT become
//     the write target.
//   - MIMOCODE_CONFIG_CONTENT is an INLINE JSONC config string merged as the
//     TOP layer (config.ts merges `process.env.MIMOCODE_CONFIG_CONTENT` last of
//     the in-scope sources — bot PR #420 finding 4). It is content, not a path,
//     so it is carried as a raw string.
//
// Relative env values: MIMOCODE_CONFIG and MIMOCODE_CONFIG_DIR are resolved
// from the process cwd (bot PR #420 finding 3 — MiMoCode treats them as ordinary
// path strings and resolves a relative value from cwd, so the hub must too).
// MIMOCODE_HOME and XDG_CONFIG_HOME stay absolute-only (global.ts rejects a
// relative MIMOCODE_HOME outright and the XDG spec ignores a relative
// XDG_CONFIG_HOME). MIMOCODE_CONFIG_CONTENT is read raw (it is content, not a
// path).
func resolveMimoCodeConfig(home string) (mimoCodeResolution, error) {
	globalDir, err := resolveMimoCodeGlobalDir(home)
	if err != nil {
		return mimoCodeResolution{}, err
	}
	return mimoCodeResolution{
		writeTarget:   mimoCodeWriteTargetInDir(globalDir),
		configFile:    cwdResolvedEnv("MIMOCODE_CONFIG"),
		overlayDir:    cwdResolvedEnv("MIMOCODE_CONFIG_DIR"),
		inlineContent: os.Getenv("MIMOCODE_CONFIG_CONTENT"),
	}, nil
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

// mimoCodePathsSamePhysical reports whether a and b name the SAME physical file,
// the single owner of every write-target self-exemption in the shadow walk (bot
// PR #420 finding 2). The hub writes only the global write target (o.path); a
// READ layer (the MIMOCODE_CONFIG_DIR overlay, the MIMOCODE_CONFIG file, the
// global mimocode.jsonc) that resolves to the SAME physical file as o.path is NOT
// a higher layer that shadows — editing o.path is exactly what takes effect — so
// AddEntry must NOT refuse an existing entry there as a shadow on re-install.
//
// It degrades safely across the failure modes a plain filepath.Clean compare
// misses:
//  1. os.SameFile(stat(a), stat(b)) when BOTH exist — the kernel's own identity,
//     so a symlink / hardlink / bind alias to the write target is caught for free
//     and OS-correctly (no manual case handling needed when both files exist).
//  2. EvalSymlinks both then SameFile (the resolved targets both exist) —
//     authoritative; covers a symlink whose target spells the write target
//     differently, when a raw Stat in branch 1 missed it.
//  3. exact filepath.Clean compare of the raw inputs — covers the common
//     re-install case where the overlay file does NOT yet exist (Stat and
//     EvalSymlinks both error on a missing path), so a redundant `./x/..`
//     spelling of the write target is still recognized.
//  4. case-FOLDED Clean compare — ONLY when the paths differ purely by case AND a
//     PROBE of the actual directory proves the volume is case-insensitive.
//
// Case sensitivity (bot PR #420 finding 4). GOOS is NOT a reliable
// case-sensitivity proxy: NTFS supports per-directory case sensitivity
// (Win10 1803+) and APFS can be formatted case-sensitive, so a blanket
// "fold on windows/darwin" would over-exempt — wrongly treating Foo and foo
// as the SAME physical file and SUPPRESSING a real shadow. So the case-FOLD
// branch (4) fires only after mimoCodeDirCaseInsensitive PROBES the relevant
// existing directory and confirms case-insensitivity. When the probe is
// inconclusive (neither directory exists, the probe errors) the function is
// CONSERVATIVE: it does NOT exempt, so a genuine shadow still fires. os.SameFile
// (branches 1,2) is always OS-correct without any folding.
func mimoCodePathsSamePhysical(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	// 1. Authoritative identity when both files exist.
	if sa, err := os.Stat(a); err == nil {
		if sb, err := os.Stat(b); err == nil {
			return os.SameFile(sa, sb)
		}
	}
	// 2. Resolve symlinks then authoritative SameFile (both resolved targets
	// exist — EvalSymlinks errors on a missing path, so success means present).
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		if rb, err := filepath.EvalSymlinks(b); err == nil {
			if sa, err := os.Stat(ra); err == nil {
				if sb, err := os.Stat(rb); err == nil {
					return os.SameFile(sa, sb)
				}
			}
			// Both resolved but a Stat raced/failed: fall through to the
			// exact/probe compare on the resolved spellings.
			a, b = ra, rb
		}
	}
	// 3. Exact Clean compare (handles a redundant `./x/..` spelling and the
	// common non-existent-overlay re-install case without any case assumption).
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	// 4. The cleaned paths differ. They may differ purely by case (the same
	// physical file on a case-insensitive volume) OR be genuinely distinct files
	// on a case-sensitive volume. Fold ONLY when they differ purely by case AND a
	// probe of the actual directory proves case-insensitivity; otherwise do NOT
	// exempt (conservative — a genuine shadow fires).
	if !strings.EqualFold(ca, cb) {
		return false // differ by more than case → genuinely distinct
	}
	return mimoCodeCaseFoldedPathsSamePhysical(ca, cb)
}

// mimoCodeCaseFoldedPathsSamePhysical reports whether two cleaned paths that
// differ ONLY by case name the same physical file, by PROBING the actual case
// behavior of the relevant directory rather than assuming it from GOOS (bot PR
// #420 finding 4). It returns true only when the probe positively confirms the
// volume is case-insensitive; an inconclusive probe returns false (conservative
// — do NOT exempt, so a genuine shadow still fires).
func mimoCodeCaseFoldedPathsSamePhysical(a, b string) bool {
	// Probe the parent dir of whichever path exists. If the dirs themselves are
	// case variants of each other, the dir's own existence resolves it; prefer a
	// dir that actually exists on disk as the probe anchor.
	for _, dir := range []string{filepath.Dir(a), filepath.Dir(b)} {
		if ci, ok := mimoCodeDirCaseInsensitive(dir); ok {
			return ci
		}
	}
	return false // probe inconclusive → conservative: do not exempt
}

// mimoCodeDirCaseInsensitive probes whether dir lives on a case-insensitive
// volume by stat-ing an existing entry under a case-flipped basename and asking
// the kernel (os.SameFile) whether the flipped spelling resolves to the SAME
// file. Returns (result, true) when the probe was conclusive and (false, false)
// when it could not run (dir missing, empty, no case-flippable entry, stat
// error). Non-mutating: it never creates or writes anything.
func mimoCodeDirCaseInsensitive(dir string) (caseInsensitive bool, ok bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		name := e.Name()
		flipped := mimoCodeFlipASCIICase(name)
		if flipped == name {
			continue // no ASCII letter to flip — try the next entry
		}
		orig, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		alt, err := os.Stat(filepath.Join(dir, flipped))
		if err != nil {
			// The flipped spelling does not resolve → case-SENSITIVE (the only
			// reason an existing entry's case-flip would not stat). Conclusive.
			return false, true
		}
		// The flipped spelling resolved: case-insensitive iff it is the SAME file.
		return os.SameFile(orig, alt), true
	}
	return false, false // no case-flippable entry to probe → inconclusive
}

// mimoCodeFlipASCIICase returns s with the case of its first ASCII letter
// flipped (the rest unchanged) — enough to make a distinct case-variant spelling
// for the case-sensitivity probe. A string with no ASCII letter returns s
// unchanged (the probe caller then skips it).
func mimoCodeFlipASCIICase(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			return s[:i] + string(c-('a'-'A')) + s[i+1:]
		}
		if c >= 'A' && c <= 'Z' {
			return s[:i] + string(c+('a'-'A')) + s[i+1:]
		}
	}
	return s
}

// resolveMimoCodeGlobalDir resolves the GLOBAL config DIRECTORY honoring
// MiMoCode's documented env precedence (highest first):
//
//	MIMOCODE_HOME    (absolute DIR → $MIMOCODE_HOME/config)
//	  > XDG_CONFIG_HOME  (absolute DIR → $XDG_CONFIG_HOME/mimocode)
//	  > ~/.config/mimocode
//
// (MIMOCODE_CONFIG — the FILE override — and MIMOCODE_CONFIG_DIR — the overlay —
// are handled one level up in resolveMimoCodeConfig; neither replaces the global
// dir, and both accept a cwd-relative value per bot PR #420 finding 3.)
//
// MIMOCODE_HOME and XDG_CONFIG_HOME diverge on a relative value (bot PR #420
// finding 5):
//   - A relative or ~/-prefixed MIMOCODE_HOME is a HARD ERROR — MiMoCode's own
//     global.ts:26-50 THROWS ("MIMOCODE_HOME must be an absolute path"; ~/ is not
//     absolute, so it throws too — global.test.ts:34-47). Falling back to
//     ~/.config/mimocode here would write the install to a profile MiMoCode never
//     reads with that env set, then report success. So mimoCodeHomeDir surfaces
//     the error and we propagate it (NewMimoCode fails loud → not-installed).
//   - A relative XDG_CONFIG_HOME is correctly IGNORED (the XDG spec says relative
//     values must be ignored), so absoluteEnv stays for it.
func resolveMimoCodeGlobalDir(home string) (string, error) {
	h, err := mimoCodeHomeDir()
	if err != nil {
		return "", err
	}
	if h != "" {
		return filepath.Join(h, "config"), nil
	}
	if xdg := absoluteEnv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mimocode"), nil
	}
	return filepath.Join(home, ".config", "mimocode"), nil
}

// ErrMimoCodeHomeNotAbsolute is returned by mimoCodeHomeDir (and propagated
// through NewMimoCode) when MIMOCODE_HOME is set to a relative or ~/-prefixed
// value. MiMoCode's global.ts rejects exactly this at startup, so the hub must
// not silently fall back to ~/.config/mimocode and report a false-success
// install against a profile MiMoCode will never read (bot PR #420 finding 5).
type ErrMimoCodeHomeNotAbsolute struct {
	Value string
}

func (e *ErrMimoCodeHomeNotAbsolute) Error() string {
	return fmt.Sprintf("mimocode: MIMOCODE_HOME must be an absolute path, got %q — MiMoCode rejects a relative or ~/-prefixed value at startup, so the hub cannot resolve a config profile; set MIMOCODE_HOME to an absolute path (or unset it to use ~/.config/mimocode)", e.Value)
}

// mimoCodeHomeDir resolves MIMOCODE_HOME faithfully to MiMoCode's global.ts:26-50:
//   - unset / empty            → "", nil  (caller falls back to XDG / ~/.config)
//   - absolute path            → <value>, nil
//   - relative OR ~/-prefixed  → "", *ErrMimoCodeHomeNotAbsolute  (LOUD)
//
// `~/x` is NOT absolute (filepath.IsAbs is false on every OS for a leading ~),
// so a tilde path routes to the error arm — matching global.test.ts:34-47 where
// both relative and ~/ throw. An empty string is treated as unset (the XDG
// fallback applies), mirroring global.test.ts:29-32.
func mimoCodeHomeDir() (string, error) {
	v := os.Getenv("MIMOCODE_HOME")
	if v == "" {
		return "", nil
	}
	if !filepath.IsAbs(v) {
		return "", &ErrMimoCodeHomeNotAbsolute{Value: v}
	}
	return v, nil
}

// defaultMimoCodeConfigPath returns the WRITE-target path for the resolved
// config surface (the single file every WRITE + DELETE touches). Thin wrapper
// over resolveMimoCodeConfig kept for the config-path test surface and any
// caller that only needs the write target.
//
// On a resolution error (a relative / ~/ MIMOCODE_HOME — bot PR #420 finding 5)
// it returns "" (the sentinel write target): mimoCodeReadLayerFiles("") yields
// [""], os.Stat("") fails everywhere, so Exists() is false and every write path
// fails loud against the empty path — never a silent fallback to the real
// ~/.config/mimocode. The string signature is preserved so the config-path test
// surface and any string-only caller are not forced to handle the error.
func defaultMimoCodeConfigPath(home string) string {
	r, err := resolveMimoCodeConfig(home)
	if err != nil {
		return ""
	}
	return r.writeTarget
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

// cwdResolvedEnv reads env var `name` and resolves it to an ABSOLUTE path,
// joining a RELATIVE value against the process cwd (bot PR #420 finding 3).
// MiMoCode treats MIMOCODE_CONFIG / MIMOCODE_CONFIG_DIR as ordinary path strings
// and resolves a relative value from the process cwd, so the hub must too —
// absoluteEnv silently DROPPED relative values (MIMOCODE_CONFIG=custom.json,
// MIMOCODE_CONFIG_DIR=.mimocode), making the hub ignore an active overlay and
// miss servers + same-name shadows. An absolute value is returned cleaned and
// unchanged. Unset → "". A relative value that filepath.Abs cannot resolve
// (cwd unreadable — vanishingly rare) is dropped ("") rather than guessed.
//
// This is ONLY for MIMOCODE_CONFIG / MIMOCODE_CONFIG_DIR. MIMOCODE_HOME and
// XDG_CONFIG_HOME stay absolute-only (absoluteEnv): MiMoCode's global.ts rejects
// a relative MIMOCODE_HOME outright and the XDG spec ignores a relative
// XDG_CONFIG_HOME, so resolving those from cwd would diverge from the runtime.
func cwdResolvedEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		return ""
	}
	if filepath.IsAbs(v) {
		return filepath.Clean(v)
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return ""
	}
	return abs
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
//   - configFile (MIMOCODE_CONFIG) — a FILE (absolute or cwd-relative — bot PR
//     #420 finding 3) read as a LAYER ABOVE the global dir layers (bot PR #420
//     finding 1). It is NOT a replacement: the global layers stay in the read
//     set. The hub never writes it.
//   - overlayDir (MIMOCODE_CONFIG_DIR) — a DIR (absolute or cwd-relative — bot PR
//     #420 finding 3) whose mimocode.json/.jsonc merge ABOVE configFile.
//   - inlineContent (MIMOCODE_CONFIG_CONTENT) — a raw JSONC STRING merged as the
//     TOP read layer (bot PR #420 finding 4). It is content, not a path, so it
//     is parsed and merged after the file layers in readMergedLayers.
type mimoCodeClient struct {
	path          string
	configFile    string
	overlayDir    string
	inlineContent string
	// claudeHome is the OS home dir ($HOME / $USERPROFILE) under which MiMoCode
	// imports ~/.claude.json mcpServers into the effective `mcp` view
	// (config.ts:887-890; Global.Path.home = process.env.HOME || USERPROFILE).
	// Non-empty ONLY for the live NewMimoCode resolution and the scan-side
	// resolver when the path is a known global layer name — direct/test
	// construction leaves it "" so the read set never imports the developer's
	// real ~/.claude.json (state-safe, same gate as inlineContent). Read-only:
	// the hub never WRITES .claude.json. See mergeClaudeImport (bot PR #420
	// finding 3).
	claudeHome string
}

// mimoCodeMCPKey is the single owner of MiMoCode's top-level MCP section
// name. Every method that reaches into the parsed config map uses it.
const mimoCodeMCPKey = "mcp"

func (o *mimoCodeClient) Name() string       { return "mimocode" }
func (o *mimoCodeClient) ConfigPath() string { return o.path }

// IsRelayStdio reports false: mimocode is a URL-native HTTP MCP client.
func (o *mimoCodeClient) IsRelayStdio() bool { return false }

// Exists treats MiMoCode as installed when ANY of: a resolved READ layer FILE is
// present; a PARSEABLE MIMOCODE_CONFIG_CONTENT inline layer is set; a PARSEABLE
// ~/.claude.json mcpServers import yields at least one importable entry; OR the
// write-target's config directory exists — mirroring the
// opencode/cursor/vscode/kiro "directory means installed" heuristic so an
// operator who has MiMoCode installed but no MCP config yet still gets the
// Initialize / install affordance.
//
// The inline-content branch (bot PR #420 finding 3) keeps Exists() consistent
// with the SCAN promotion: an INLINE-ONLY profile (MIMOCODE_CONFIG_CONTENT set,
// no config file on disk, possibly no config dir) is promoted to "ok" and has its
// servers discovered by the scan path (MimoCodeHasInlineContent /
// MimoCodeMergedConfig). Without this branch readLayerFiles() (FILE paths only,
// never the inline string) plus the dir stat would BOTH miss it, Exists() would
// return false, and every write path gating on client.Exists() (Apply / Register)
// would silently skip mimo — unable to act on the inline-only setup the matrix
// shows as present. "Parseable" matches MimoCodeHasInlineContent: a malformed
// inline string is NOT treated as present (a write path acting on it would
// surface the inline-shadow / merged-parse error from the existing guards, not a
// silent skip). The merged-read parse error on a malformed inline string remains
// the loud signal; Exists() simply does not assert presence on unparseable bytes.
//
// The claude-import branch (bot PR #420 finding 2, r16) closes the SAME gap for a
// CLAUDE-IMPORT-ONLY profile: one whose only active mimo MCP source is the
// ~/.claude.json mcpServers import (no mimo file layer, no inline string, possibly
// no config dir). The scan path already promotes such a profile to "ok" via
// MimoCodeHasClaudeImport; without this branch Exists() falls through to the
// parent-dir stat → false → install/register/Apply gate on Exists() and SKIP mimo
// even though the matrix shows the imported servers as present. claudeImportEntries
// is the single owner of the read + conversion and carries the SAME state-safe gate
// the scan path uses: it returns no entries when claudeHome is "" (direct/test
// construction) or when MIMOCODE_DISABLE_CLAUDE_CODE_MCP is set, so a test/temp
// client never reaches the developer's real ~/.claude.json, and it is best-effort
// (a malformed/unreadable claude.json imports nothing → no false presence). The
// live client (NewMimoCode) sets claudeHome to the OS home, so an operator with a
// real ~/.claude.json import is correctly seen as present.
func (o *mimoCodeClient) Exists() bool {
	for _, f := range o.readLayerFiles() {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	if o.inlineContent != "" {
		if _, err := parseJSONCBytes([]byte(o.inlineContent)); err == nil {
			return true
		}
	}
	if imported, err := o.claudeImportEntries(); err == nil && len(imported) > 0 {
		return true
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
// for any file layer (File names it, Label describes it); "managed" for the
// macOS Managed Preferences (MDM) layer (PlistFile names the plist).
type mimoCodeShadowSource struct {
	Kind  string // "", "inline", "file", or "managed"
	Label string // human label for the "file" kind
	File  string // file path for the "file" kind

	// PlistFile is the plist path for the "managed" kind. Separate from File so
	// the managed branch can carry the MDM plist source without conflating it
	// with the operator-editable "file" layers.
	PlistFile string
}

// mimoCodeManagedPrefsReader, when non-nil (tests only), replaces the production
// macOS Managed Preferences reader so the detect-and-fail-loud managed-shadow
// path can be exercised on a Windows/Linux CI runner without a real /Library
// plist or `plutil`. Mirrors the established func-var test-seam idiom
// (clients.go copyFileTornWindowHook): nil in production, assigned in a test
// with a t.Cleanup restore. Bot PR #420 finding 1.
var mimoCodeManagedPrefsReader func(name string) (mimoCodeShadowSource, error)

// ErrMimoCodeManagedShadowsServer is returned by AddEntry when the macOS Managed
// Preferences (MDM) layer already defines mcp.<Server>. That layer is read-only
// to the hub and MiMoCode merges it ABOVE every user/env layer (config.ts:876-885
// "override everything"), so a write into the global mimocode.json would "succeed"
// yet MiMoCode would keep resolving the MDM entry — a silent false success. The
// hub cannot and must not write a managed layer (it is MDM-deployed), so AddEntry
// fails loud: the operator must remove the entry in their MDM / Managed
// Preferences profile, not in any hub-writable file. Distinct from the file /
// inline shadow errors because the remediation surface is MDM, not a file or env
// var the operator edits directly.
type ErrMimoCodeManagedShadowsServer struct {
	Server      string
	WriteTarget string
	PlistFile   string // the managed plist that defines the server ("" if not captured)
}

func (e *ErrMimoCodeManagedShadowsServer) Error() string {
	where := "the macOS Managed Preferences (MDM) layer"
	if e.PlistFile != "" {
		where = fmt.Sprintf("the macOS Managed Preferences (MDM) layer %q", e.PlistFile)
	}
	return fmt.Sprintf("mimocode: server %q is already defined in %s, which MiMoCode merges on top of the hub's write target %q and wins — the hub entry would have no effect and the hub cannot write a managed (MDM-deployed) layer; remove mcp.%s from the managed configuration profile, then retry",
		e.Server, where, e.WriteTarget, e.Server)
}

// mimoCodeReadManagedPrefs is the production macOS Managed Preferences reader for
// the AddEntry shadow check (bot PR #420 finding 1). It is DETECT-ONLY: it never
// merges the managed layer into a writable path — it only reports whether the MDM
// plist defines mcp.<name> so AddEntry can fail loud.
//
//   - Non-darwin → ("", nil) no-op (managed.ts:47 returns undefined off darwin).
//   - darwin → reads /Library/Managed Preferences/<user>/ai.opencode.managed.plist
//     then /Library/Managed Preferences/ai.opencode.managed.plist (managed.ts:50-53;
//     the plist DOMAIN is "ai.opencode.managed", inherited from the OpenCode
//     upstream, NOT "mimocode"). Each present plist is converted to JSON via
//     `plutil -convert json` (the SAME tool managed.ts:58 uses — present on every
//     macOS host, so no new Go dependency), parsed with the shared JSONC helper,
//     and tested for mcp.<name>. The FIRST plist that defines it wins (per-user
//     plist takes precedence over the system plist, matching managed.ts's path
//     order). A `plutil`/parse failure on a present plist is WARNING-SKIP — it
//     `continue`s to the next plist and is treated as "no managed shadow" (bot PR
//     #420 finding 3, managed.ts:58-62 nothrow+continue). detect-fail-loud is
//     preserved ONLY for the real case: a SUCCESSFULLY-PARSED managed config that
//     actually defines the server. An unparseable plist cannot be PROVEN to define
//     the server, so it must not block every install on the host.
//
// The plist directory honors MIMOCODE_TEST_MANAGED_CONFIG_DIR-style isolation
// only indirectly: the primary test seam is mimoCodeManagedPrefsReader (the
// func-var above), which replaces this whole function on non-darwin CI.
func mimoCodeReadManagedPrefs(name string) (mimoCodeShadowSource, error) {
	if runtime.GOOS != "darwin" {
		return mimoCodeShadowSource{}, nil
	}
	const managedPlistDomain = "ai.opencode.managed"
	// Resolve the username the way managed.ts:49 does (os.userInfo().username);
	// os/user.Current() is the Go equivalent. Fall back to $USER if the lookup
	// fails (cgo-less cross builds), and to no per-user plist if neither resolves.
	username := ""
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	} else if env := os.Getenv("USER"); env != "" {
		username = env
	}
	plists := []string{
		filepath.Join("/Library/Managed Preferences", managedPlistDomain+".plist"),
	}
	if username != "" {
		// Per-user plist takes precedence (managed.ts lists it first).
		plists = append([]string{
			filepath.Join("/Library/Managed Preferences", username, managedPlistDomain+".plist"),
		}, plists...)
	}
	for _, plist := range plists {
		if _, err := os.Stat(plist); err != nil {
			continue // not present — skip (managed.ts:56 existsSync gate)
		}
		// plutil/parse failures are WARNING-SKIP, not fatal (bot PR #420 finding 3,
		// managed.ts:58-62). Upstream runs plutil with nothrow and `continue`s to
		// the next plist on a non-zero exit; it never aborts. A managed shadow is
		// raised ONLY by a SUCCESSFULLY-PARSED managed config that actually defines
		// the server — so an unparseable plist is treated as "no managed shadow"
		// (it cannot be PROVEN to define the server), preserving detect-fail-loud
		// only for the real, proven case. Without this, a host whose MDM plist
		// merely happens to be unconvertible (an unrelated managed profile, a
		// plutil quirk) would have EVERY hub install on that host fail loud with a
		// managed-shadow error for a server the managed layer never defined.
		out, err := exec.Command("plutil", "-convert", "json", "-o", "-", plist).Output()
		if err != nil {
			continue // plutil convert failed — warn-skip, treat as no shadow
		}
		m, err := parseJSONCBytes(out)
		if err != nil {
			continue // unparseable converted JSON — warn-skip, treat as no shadow
		}
		if mimoCodeMapDefines(m, name) {
			return mimoCodeShadowSource{Kind: "managed", Label: "macOS Managed Preferences", PlistFile: plist}, nil
		}
	}
	return mimoCodeShadowSource{}, nil
}

// mimoCodeHigherLayerDefining walks every READ layer ABOVE the hub's global
// write target (mimocode.json) from HIGHEST precedence DOWN and returns the
// FIRST that defines mcp.<name> — i.e. the layer that actually wins the merge,
// the one the operator must edit to un-shadow. Order (highest→lowest):
//
//  1. macOS Managed Preferences (MDM) — read-only, "override everything"
//  2. MIMOCODE_CONFIG_CONTENT inline string
//  3. MIMOCODE_CONFIG_DIR overlay: mimocode.jsonc then mimocode.json
//  4. MIMOCODE_CONFIG file
//  5. global higher layer mimocode.jsonc (sibling of the write target)
//
// config.json and the write-target mimocode.json itself are BELOW the write
// target / are the write target, so they cannot shadow and are not checked. It
// reads ONLY the explicit configFile / overlayDir / inlineContent captured at
// construction plus the write target's own dir (state-safe — never recomputes a
// dir from env/home). A parse error on a present layer propagates (a malformed
// higher layer must not be silently treated as "no shadow" — that would
// re-introduce the silent-false-success this guards).
//
// The ~/.claude.json MCP import is deliberately NOT a shadow source here, even
// though config.ts merges it ABOVE everything (887-890): it is SKIP-IF-NAME-EXISTS
// (694-698), so once AddEntry writes mcp.<name> to the write target the import
// skips that name and the hub entry wins the merge. A claude.json entry therefore
// can never shadow a name the hub writes — see the closing note in the body.
func (o *mimoCodeClient) mimoCodeHigherLayerDefining(name string) (mimoCodeShadowSource, error) {
	// 1. macOS Managed Preferences (MDM) — highest, read-only, detect-and-fail-loud.
	reader := mimoCodeManagedPrefsReader
	if reader == nil {
		reader = mimoCodeReadManagedPrefs
	}
	if src, err := reader(name); err != nil {
		return mimoCodeShadowSource{}, err
	} else if src.Kind != "" {
		return src, nil
	}
	// 2. Inline content.
	if o.inlineContent != "" {
		defined, err := mimoCodeInlineDefines(o.inlineContent, name)
		if err != nil {
			return mimoCodeShadowSource{}, err
		}
		if defined {
			return mimoCodeShadowSource{Kind: "inline"}, nil
		}
	}
	// 3. MIMOCODE_CONFIG_DIR overlay (mimocode.jsonc preferred over mimocode.json).
	if o.overlayDir != "" {
		for i := len(mimoCodeOverlayLayerNames) - 1; i >= 0; i-- {
			f := filepath.Join(o.overlayDir, mimoCodeOverlayLayerNames[i])
			// Skip a file physically identical to the write target (bot PR #420
			// finding 2): if MIMOCODE_CONFIG_DIR resolves to the global config dir
			// (or to it via a symlink / case variant), this loop reads the same
			// mimocode.json o.path writes — editing o.path IS what takes effect, so
			// it is not a higher shadowing layer. Same exemption polarity as the
			// MIMOCODE_CONFIG == o.path case below, upgraded to physical comparison.
			if mimoCodePathsSamePhysical(f, o.path) {
				continue
			}
			defined, err := mimoCodeFileDefines(f, name)
			if err != nil {
				return mimoCodeShadowSource{}, err
			}
			if defined {
				return mimoCodeShadowSource{Kind: "file", Label: "MIMOCODE_CONFIG_DIR overlay", File: f}, nil
			}
		}
	}
	// 4. MIMOCODE_CONFIG file — but NOT when it points at the write target itself
	// (bot PR #420). When MIMOCODE_CONFIG resolves to o.path (e.g. set to the
	// global mimocode.json the hub already writes, possibly via a symlink / case
	// variant), it is the SAME file the write lands in, not a higher layer that
	// would shadow it — editing o.path is exactly what takes effect, so an existing
	// entry must not be refused as a shadow. A genuine MIMOCODE_CONFIG at a
	// DIFFERENT physical path still shadows. Physical comparison (single owner
	// mimoCodePathsSamePhysical) so a symlinked / case-variant / "./x/.."-spelled
	// configFile does not slip past a plain Clean compare.
	if o.configFile != "" && !mimoCodePathsSamePhysical(o.configFile, o.path) {
		defined, err := mimoCodeFileDefines(o.configFile, name)
		if err != nil {
			return mimoCodeShadowSource{}, err
		}
		if defined {
			return mimoCodeShadowSource{Kind: "file", Label: "MIMOCODE_CONFIG file", File: o.configFile}, nil
		}
	}
	// 5. global higher layer mimocode.jsonc (sibling of the write target).
	jsonc := filepath.Join(filepath.Dir(o.path), "mimocode.jsonc")
	if !mimoCodePathsSamePhysical(jsonc, o.path) { // never treat the write target itself as a shadow
		defined, err := mimoCodeFileDefines(jsonc, name)
		if err != nil {
			return mimoCodeShadowSource{}, err
		}
		if defined {
			return mimoCodeShadowSource{Kind: "file", Label: "global higher layer", File: jsonc}, nil
		}
	}
	// NOTE on the ~/.claude.json MCP import — it is NOT a shadow source. Unlike
	// every layer above, the claude import is SKIP-IF-NAME-EXISTS (config.ts:694-698:
	// an entry already defined by a non-"claude" layer is skipped). The hub's
	// AddEntry is ABOUT TO write mcp.<name> to the write target, so AFTER the write
	// the write target defines <name> and the claude import SKIPS it — the hub's
	// entry wins the merge. A ~/.claude.json entry can therefore never shadow a name
	// the hub writes, so it is deliberately absent from this shadow walk (an earlier
	// revision wrongly treated it as a shadow). The import still contributes to the
	// READ merge (readMergedLayers) and to GetEntry membership for names the hub
	// does NOT write.
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

// mimoCodeFileDefinesStdioLSP reports whether the JSONC file at path defines
// mcp.<name> AND that WRITE-TARGET value is ITSELF a stdio mcp-language-server
// entry. The shape test routes through the single canonical classifier
// (matchLanguageServerStdio) after the SAME normalization the merged-view match
// applied (array `command` → string command + args, then disabled-dropped), so
// the write-target shape is judged by the exact same owner that produced the
// match. A missing/empty file → false; a parse error on present bytes propagates.
//
// This is the write-target SHAPE gate for FindStdioLanguageServerEntries (bot PR
// #420 r12 HIGH finding). Name-membership alone (mimoCodeFileDefines) is NOT
// enough: a name can be stdio in a HIGHER layer (so the merged-view match is
// stdio) yet REMOTE in the write target, and RemoveEntry deletes the write
// target's value — reporting on name alone would wrong-delete the write-target
// remote and leave the higher stdio active. Reporting only when the write
// target's OWN value is stdio-LSP keeps the destructive cleanup honest.
func mimoCodeFileDefinesStdioLSP(path, name string) (bool, error) {
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
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false, nil
	}
	// Reuse the merged-view normalization + disabled-drop so the write-target
	// value is classified exactly as the merged match was, then run the single
	// canonical stdio-LSP classifier over the one named entry.
	normalized := mimoCodeNormalizeCommandArrays(mimoCodeDropDisabled(servers))
	entry, ok := normalized[name].(map[string]any)
	if !ok {
		return false, nil
	}
	_, _, isStdioLSP := matchLanguageServerStdio(entry)
	return isStdioLSP, nil
}

// mimoCodeFileEntryValue returns the WRITE-target file's OWN parsed mcp.<name>
// map — the value PHYSICALLY present in `path`, independent of the merged
// multi-layer view. Modeled on mimoCodeFileDefinesStdioLSP's single-file
// read+parse (readRawConfig + parseJSONCBytes) so the parse is a single owner,
// NOT re-implemented here.
//
//   - absent/empty file, or no such name → (nil, false, nil).
//   - present name → (verbatim raw map, true, nil).
//   - parse error on present bytes → (nil, false, err) — propagated so a
//     malformed write target aborts (the r12/r13 data-loss guard), never
//     silently reads as "no own value".
//
// Used by GetEntry (bot PR #420 finding 3) to snapshot the rollback Raw from the
// write target's OWN value instead of the merged synthesis, so a lower-layer
// `command` merged with a write-target enabled-only override is never copied UP
// into mimocode.json on a rollback. NO normalization / disabled-drop is applied:
// the verbatim physical value is exactly what a rollback AddEntry must write back.
func mimoCodeFileEntryValue(path, name string) (map[string]any, bool, error) {
	data, err := readRawConfig(path)
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return nil, false, nil
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return nil, false, nil
	}
	v, ok := servers[name].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	return v, true, nil
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
	// ~/.claude.json MCP import — the TOP layer (config.ts:887-890), SKIP-IF-NAME-
	// EXISTS so it only ADDS mcp.<name> entries not already defined by any layer
	// above (an explicit mimo entry always wins). Read-only: the hub never writes
	// .claude.json. Empty / no-op when MIMOCODE_DISABLE_CLAUDE_CODE_MCP is set, in
	// the state-safe single-file mode (claudeHome ""), or when ~/.claude.json
	// defines no importable mcpServers. A read/parse FAILURE on a present
	// ~/.claude.json is SWALLOWED (best-effort import — bot PR #420 finding 2): it
	// imports nothing and the rest of the merged config still loads. The returned
	// error here is therefore effectively always nil today, but the signature is
	// kept for symmetry with the other merge steps and so a future hard-failure
	// import mode stays a one-line change.
	if err := o.mergeClaudeImport(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// MimoCodeDisableClaudeImportEnv is MiMoCode's env flag that disables the Claude
// Code MCP compatibility import (config.ts:887 `Flag.MIMOCODE_DISABLE_CLAUDE_CODE_MCP`).
// Exported (single owner of the string) so internal/api tests share the same
// suppression flag for state-safe scan isolation without hardcoding the literal.
const MimoCodeDisableClaudeImportEnv = "MIMOCODE_DISABLE_CLAUDE_CODE_MCP"

// mimoCodeDisableClaudeImportEnv is the unexported alias used inside the package.
const mimoCodeDisableClaudeImportEnv = MimoCodeDisableClaudeImportEnv

// claudeJSONPath returns the ~/.claude.json path the import reads, or "" when no
// claudeHome was captured (direct/test construction → state-safe no import).
// MiMoCode anchors this on the OS home (config.ts:888 `Global.Path.home` =
// process.env.HOME || USERPROFILE), NOT MIMOCODE_HOME, so claudeHome holds the
// OS home dir.
func (o *mimoCodeClient) claudeJSONPath() string {
	if o.claudeHome == "" {
		return ""
	}
	return filepath.Join(o.claudeHome, ".claude.json")
}

// mergeClaudeImport applies MiMoCode's ~/.claude.json MCP compatibility import
// (config.ts:887-890, mergeClaudeMcp config.ts:688-716) onto the already-merged
// map, in place. SKIP-IF-NAME-EXISTS: each ~/.claude.json mcpServers entry is
// imported under mcp.<name> ONLY when the merged map does not already define that
// name (an explicit mimo entry wins). Each claude entry is converted to MiMoCode's
// native mcp shape via mimoCodeFromClaude (faithful to ConfigMCP.fromClaude); an
// unconvertible entry (sse / non-string command|url / missing both) is skipped,
// matching upstream's warning-skip.
//
// No-op (returns nil, leaves merged unchanged) when:
//   - MIMOCODE_DISABLE_CLAUDE_CODE_MCP is set (config.ts:887 gate), OR
//   - claudeHome is empty (direct/test construction — state-safe), OR
//   - ~/.claude.json is absent / has no mcpServers object.
//
// A read/parse FAILURE on a PRESENT ~/.claude.json is SWALLOWED by
// claudeImportEntries (best-effort import — bot PR #420 finding 2): a malformed
// claude.json imports nothing and never aborts the merged read. Read-only: never
// writes .claude.json.
func (o *mimoCodeClient) mergeClaudeImport(merged map[string]any) error {
	imported, err := o.claudeImportEntries()
	if err != nil {
		return err
	}
	if len(imported) == 0 {
		return nil
	}
	servers, _ := merged[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		merged[mimoCodeMCPKey] = servers
	}
	for name, entry := range imported {
		if _, exists := servers[name]; exists {
			continue // skip-if-name-exists: explicit mimo entry wins
		}
		servers[name] = entry
	}
	return nil
}

// claudeImportEntries reads ~/.claude.json (claudeJSONPath) and returns the
// converted MiMoCode-shape mcp entries it would import, keyed by server name.
// This is the SINGLE owner of the claude.json read + conversion; both
// readMergedLayers (skip-if-name-exists merge) and mimoCodeHigherLayerDefining /
// mimoCodeDefinedAtOrAboveWriteTarget (shadow membership) consume it.
//
// Returns an empty map (nil) when the import is disabled, claudeHome is empty,
// the file is absent, or it has no convertible mcpServers. Read-only.
//
// BEST-EFFORT (bot PR #420 finding 2). A read or parse FAILURE on a present
// ~/.claude.json is SWALLOWED — it imports NO entries and returns (nil, nil)
// instead of propagating — so a malformed claude.json never aborts the whole
// merged config read; the rest of the MiMoCode config still loads. This mirrors
// upstream readClaudeConfig (config.ts:675-686), which catches the JSON.parse
// failure, log.warn-s, and returns undefined → mergeClaudeMcp (config.ts:689-690)
// `if (!isRecord(data)) return` skips the import. The ~/.claude.json import is a
// pure compatibility convenience the hub never writes, so a defect there must not
// take the operator's real MiMoCode config down with it.
func (o *mimoCodeClient) claudeImportEntries() (map[string]any, error) {
	if o.claudeHome == "" {
		return nil, nil
	}
	if os.Getenv(mimoCodeDisableClaudeImportEnv) != "" {
		return nil, nil
	}
	path := o.claudeJSONPath()
	data, err := readRawConfig(path)
	if err != nil {
		// A present-but-unreadable ~/.claude.json (permission, IO) is swallowed —
		// the import is best-effort and must not abort the merged read.
		return nil, nil
	}
	if len(data) == 0 {
		return nil, nil
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		// Malformed JSON in ~/.claude.json — warning-skip, matching upstream's
		// catch-and-continue. Import NO entries; the rest of the config loads.
		return nil, nil
	}
	claudeServers, _ := m["mcpServers"].(map[string]any)
	if len(claudeServers) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	for name, raw := range claudeServers {
		if converted, ok := mimoCodeFromClaude(raw); ok {
			out[name] = converted
		}
	}
	return out, nil
}

// mimoCodeClaudeLocalTypes / mimoCodeClaudeRemoteTypes mirror mcp.ts:68-69. A
// claude entry with a `command` string and (no type OR a local type) becomes a
// mimo LOCAL entry; with a `url` string and (no type OR a remote type) a mimo
// REMOTE entry.
var (
	mimoCodeClaudeRemoteTypes = map[string]bool{"http": true, "streamable-http": true, "remote": true}
	mimoCodeClaudeLocalTypes  = map[string]bool{"stdio": true, "local": true}
)

// mimoCodeFromClaude converts one ~/.claude.json mcpServers entry into MiMoCode's
// native `mcp` entry shape, faithful to ConfigMCP.fromClaude (mcp.ts:95-158). It
// returns (converted, true) for an importable entry and (nil, false) for one
// MiMoCode skips with a warning (not an object, sse transport, non-string /
// non-array args, command-not-string, url-not-string, unsupported type, or
// neither command nor url).
//
//   - {command:string} (+ no type or a local type) → {type:"local",
//     command:[command, ...args], environment?, enabled, timeout?}. args come
//     from `args` (must be a string array). environment from `environment` or
//     `env`. enabled = !(disabled==true) && !(enabled==false) → defaults true.
//   - {url:string} (+ no type or a remote type) → {type:"remote", url, enabled,
//     headers?, oauth?, timeout?}. oauth is converted via mimoCodeOAuth, faithful
//     to mcp.ts:83-93: oauth:false is PRESERVED+emitted; an oauth object is
//     FILTERED to the upstream-accepted string keys (clientId/clientSecret/scope/
//     redirectUri) and emitted only if at least one is present; any other oauth
//     value (or an object with no accepted key) is OMITTED (bot PR #420 finding 5).
//
// The produced shape is the SAME normalized shape the rest of the merge emits, so
// every downstream consumer (shapeMimoCodeEntry, GetEntry, the LSP classifier)
// reads it without a second special case.
func mimoCodeFromClaude(raw any) (map[string]any, bool) {
	input, ok := raw.(map[string]any)
	if !ok {
		return nil, false // not an object — skip
	}
	typeStr, _ := input["type"].(string)
	if typeStr == "sse" {
		return nil, false // unsupported transport — skip
	}
	// args: must be a []any of strings when present.
	var args []any
	if av, present := input["args"]; present {
		arr, isArr := av.([]any)
		if !isArr {
			return nil, false // args is not an array — skip
		}
		for _, item := range arr {
			if _, isStr := item.(string); !isStr {
				return nil, false // args must contain only strings — skip
			}
		}
		args = arr
	}
	// enabled = disabled===true ? false : enabled===false ? false : true (mcp.ts:111).
	enabled := true
	if d, ok := input["disabled"].(bool); ok && d {
		enabled = false
	} else if e, ok := input["enabled"].(bool); ok && !e {
		enabled = false
	}
	environment := mimoCodeStringRecord(input["environment"])
	if environment == nil {
		environment = mimoCodeStringRecord(input["env"])
	}
	timeout, hasTimeout := input["timeout"].(float64)

	// LOCAL: command string + (no type or local type).
	if cmd, ok := input["command"].(string); ok && (typeStr == "" || mimoCodeClaudeLocalTypes[typeStr]) {
		command := make([]any, 0, len(args)+1)
		command = append(command, cmd)
		command = append(command, args...)
		entry := map[string]any{
			"type":    "local",
			"command": command,
			"enabled": enabled,
		}
		if environment != nil {
			entry["environment"] = environment
		}
		if hasTimeout {
			entry["timeout"] = timeout
		}
		return entry, true
	}
	if _, present := input["command"]; present {
		return nil, false // command present but not a string — skip
	}

	// REMOTE: url string + (no type or remote type).
	if url, ok := input["url"].(string); ok && (typeStr == "" || mimoCodeClaudeRemoteTypes[typeStr]) {
		entry := map[string]any{
			"type":    "remote",
			"url":     url,
			"enabled": enabled,
		}
		if headers := mimoCodeStringRecord(input["headers"]); headers != nil {
			entry["headers"] = headers
		}
		if oauthVal, present := mimoCodeOAuth(input["oauth"]); present {
			entry["oauth"] = oauthVal
		}
		if hasTimeout {
			entry["timeout"] = timeout
		}
		return entry, true
	}
	if _, present := input["url"]; present {
		return nil, false // url present but not a string — skip
	}

	// Unsupported type, or neither command nor url — skip (mcp.ts:151-157).
	return nil, false
}

// mimoCodeOAuth converts a ~/.claude.json mcpServers entry's `oauth` value into
// MiMoCode's accepted oauth shape, faithful to mcp.ts:83-93 `oauth()` (bot PR
// #420 finding 5). It returns (value, true) when the field should be EMITTED and
// (nil, false) when it should be OMITTED:
//
//   - oauth === false → (false, true). The literal `false` is PRESERVED and
//     emitted (a deliberate "disable OAuth" signal). The prior adapter dropped it
//     because it only kept a map value.
//   - oauth is NOT an object (string/number/null/absent) → (nil, false), omitted
//     (upstream `!isRecord(input)` → undefined).
//   - oauth is an object → FILTERED to ONLY the upstream-accepted string fields
//     clientId / clientSecret / scope / redirectUri (each kept only when its value
//     is a string). If at least one matched → (filtered, true); if NONE matched →
//     (nil, false), omitted (upstream `Object.keys(result).length > 0 ? result :
//     undefined`). Arbitrary unknown object keys are DROPPED (the prior adapter
//     copied them verbatim).
func mimoCodeOAuth(v any) (any, bool) {
	if b, isBool := v.(bool); isBool {
		if !b {
			return false, true // oauth:false is preserved + emitted
		}
		return nil, false // oauth:true is not a record → omitted
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false // not an object → omitted
	}
	out := map[string]any{}
	for _, k := range mimoCodeOAuthStringKeys {
		if s, ok := m[k].(string); ok {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil, false // no accepted field present → omitted
	}
	return out, true
}

// mimoCodeOAuthStringKeys is the upstream-accepted oauth object field set
// (mcp.ts:86-91): only these string-valued keys survive the fromClaude oauth
// projection. Any other key in a claude.json oauth object is dropped.
var mimoCodeOAuthStringKeys = []string{"clientId", "clientSecret", "scope", "redirectUri"}

// mimoCodeStringRecord coerces a value into a map[string]any of string→string,
// faithful to mcp.ts:72-78 stringRecord: when the value is NOT a map it returns
// nil (upstream undefined → the optional field is omitted); when it IS a map it
// FILTERS to only the string-valued keys and returns that (possibly empty) map.
// Upstream emits the field whenever stringRecord is non-undefined (an empty
// filtered object is truthy in JS), so a present-but-all-non-string map yields an
// empty map here, not nil — the field is still emitted, matching upstream.
func mimoCodeStringRecord(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

// mimoCodeDeepMerge recursively merges src over dst (src wins per key) and
// returns dst. Nested objects are merged key-by-key; every other value
// (including arrays and scalars) is replaced wholesale by src's. This mostly
// mirrors MiMoCode's `mergeDeep` (remeda) layer semantics — remeda's mergeDeep
// merges objects by key and REPLACES arrays/scalars (the lone config.ts
// exception is the `instructions` array) — so a server defined in a lower layer
// survives when a higher layer carries only unrelated settings.
//
// DELIBERATE DIVERGENCE for the `mcp` key (bot PR #420 finding 2). remeda's
// mergeDeep field-merges same-named NESTED objects, so a server NAME redefined
// across layers — a lower-layer LOCAL entry {type:local, command:[...]} plus a
// higher-layer REMOTE entry {type:remote, url:...} for the SAME name — would
// field-merge into a HYBRID carrying BOTH `command` AND `url`. That hybrid is
// unclassifiable: a hub mcp entry is EITHER stdio OR remote, never both, and the
// consumers split-brain on it (FindStdioLanguageServerEntries reads it as stdio
// while shapeMimoCodeEntry reads it as remote). So for a FULL redefinition this
// adapter REPLACES the `mcp` entry VALUE by name: the entry NAMES still union
// across layers, but each name's VALUE is taken wholesale from the HIGHEST layer
// that defines it (no recursion into the entry object). Every OTHER top-level key
// still deep-merges.
//
// ENABLED-ONLY OVERLAY EXCEPTION (bot PR #420 r12 finding). MiMoCode's schema
// (config.ts:194-201) allows TWO mcp-entry forms in the layer union: the FULL
// ConfigMCP.Info (a Local {type,command,...} or Remote {type,url,...}) AND a
// legacy enabled-ONLY disable form `z.object({enabled: z.boolean()}).strict()`.
// Upstream merges layers with remeda mergeDeep (field-merge), so a higher
// enabled-only override field-merges onto the lower full entry, overlaying ONLY
// `enabled` and PRESERVING the lower command/url/type. A blanket wholesale
// replace-by-name would DROP the lower command/url here, leaving an
// unclassifiable bare {enabled:...} value. So this adapter special-cases an
// enabled-only HIGHER value (a map carrying a bool `enabled` and NONE of
// type/command/url) — it OVERLAYS just the higher entry's keys onto a COPY of the
// lower entry, mirroring upstream's field-merge for this one form, while keeping
// replace-by-name for every full redefinition (so the local+remote hybrid stays
// prevented). The structural "no type/command/url" test (not upstream's strict
// "only key is enabled") is the tolerant-read choice: an extra unknown key fails
// SAFE (overlay, preserve the lower entry) rather than destructive (wholesale
// replace, drop the lower command/url). `enabled` must be a bool — a malformed
// {enabled:"false"} falls through to wholesale replace, matching mimoCodeDropDisabled.
// When the lower entry is ABSENT (enabled-only is the first/only definer), the
// bare {enabled:...} is kept verbatim (an inert disable stub MiMoCode never
// spawns; the hub's classifiers ignore it).
//
// In practice a server is defined in exactly ONE layer, so this changes behavior
// only on the redefine-across-layers edge. Kept LOCAL to the mimocode adapter
// (no shared-type change).
func mimoCodeDeepMerge(dst, src map[string]any) map[string]any {
	for k, sv := range src {
		// `mcp` entry VALUES are replace-by-name, not field-merged (see the
		// divergence note above) — EXCEPT an enabled-only higher override, which
		// overlays just `enabled` onto a copy of the lower entry. Union the entry
		// NAMES across layers, but take each name's VALUE from the later (higher)
		// layer (wholesale for a full redefinition, overlay for enabled-only).
		if k == mimoCodeMCPKey {
			if svMap, ok := sv.(map[string]any); ok {
				if dvMap, ok := dst[k].(map[string]any); ok {
					for name, entry := range svMap {
						dvMap[name] = mimoCodeMergeMCPEntry(dvMap[name], entry)
					}
					continue
				}
			}
			// dst has no `mcp` map yet (or src's `mcp` is not a map): fall through
			// to the wholesale assignment below — the first layer to define `mcp`
			// seeds it; a later non-map src replaces it, mirroring replace-scalar.
		}
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

// mimoCodeMergeMCPEntry computes the merged VALUE of one `mcp.<name>` entry given
// the lower-layer value (lower, may be nil/absent) and the higher-layer value
// (higher, src wins). It implements the enabled-only overlay exception described
// on mimoCodeDeepMerge:
//   - higher is an enabled-only override (a map with a bool `enabled` and no
//     type/command/url) AND lower is a full entry map → OVERLAY: a shallow COPY
//     of lower with higher's keys applied on top, preserving the lower
//     command/url/type. The copy is mandatory — lower may alias a layer's live
//     parsed map (the first layer to define `mcp` is seeded by reference in
//     readMergedLayers), so an in-place overlay would mutate that layer's map.
//   - otherwise → wholesale replace-by-name (higher wins entirely), the
//     finding-2 hybrid-prevention default.
func mimoCodeMergeMCPEntry(lower, higher any) any {
	higherMap, hOK := higher.(map[string]any)
	lowerMap, lOK := lower.(map[string]any)
	if hOK && lOK && mimoCodeIsEnabledOnlyOverride(higherMap) {
		merged := make(map[string]any, len(lowerMap)+len(higherMap))
		for k, v := range lowerMap {
			merged[k] = v
		}
		for k, v := range higherMap {
			merged[k] = v
		}
		return merged
	}
	return higher // full redefinition (or no lower entry): wholesale replace-by-name
}

// mimoCodeIsEnabledOnlyOverride reports whether entry is MiMoCode's legacy
// enabled-ONLY disable form — a higher-layer override that should overlay just
// `enabled` onto a lower full entry rather than replace it. Verified against
// config.ts:194-201: the union's second member is z.object({enabled:
// z.boolean()}).strict(). The structural test here (bool `enabled` present, and
// NONE of the full-entry discriminating keys type/command/url) is the
// tolerant-read form: a stray unknown key fails SAFE to overlay (preserve the
// lower entry) instead of the destructive wholesale replace the strict
// only-key-is-enabled test would trigger. A non-bool `enabled` is malformed and
// is NOT treated as an enabled-only override (it falls through to wholesale
// replace), matching mimoCodeDropDisabled's bool-only `enabled` handling.
func mimoCodeIsEnabledOnlyOverride(entry map[string]any) bool {
	if _, ok := entry["enabled"].(bool); !ok {
		return false
	}
	if _, ok := entry["type"]; ok {
		return false
	}
	if _, ok := entry["command"]; ok {
		return false
	}
	if _, ok := entry["url"]; ok {
		return false
	}
	return true
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

// MimoCodeNonRegularActiveLayer reports whether ANY resolved READ layer that
// EXISTS on disk is a NON-REGULAR entry (directory, FIFO, device, or symlink) —
// a config fault that the merged read (MimoCodeMergedConfig → readRawConfig)
// would surface as a hard read error. Returns the offending layer path and true
// on the first such layer, ("", false) when every existing layer is a plain
// regular file (or absent — an absent layer is skipped in the merge, not a
// fault). Uses the SAME resolved layer set as MimoCodeReadLayerPaths /
// MimoCodeMergedConfig (single owner of the layer resolution), so it stays
// state-safe (a temp/test override path is single-file) and cannot diverge from
// the layers the merge actually reads.
//
// Exported for internal/api/scan.go (bot PR #420 finding 4): when ONE active
// mimo layer is non-regular, scanMimoCode's whole-config merged read returns an
// error and would fail the ENTIRE multi-client scan with `mimocode: ...`. The
// scan side uses this to surface a PER-CLIENT config-error presence (and skip
// scanMimoCode) instead, so a bad mimo layer can never take the whole scan down.
//
// os.Lstat (NOT Stat) so a SYMLINK layer is classified as non-regular here
// rather than followed — consistent with the readRawConfig failure a symlinked
// or non-regular layer would produce. A stat error other than IsNotExist
// (permission, I/O fault) on an existing layer is ALSO a fault (returns that
// path, true): the merged read would fail on it identically.
func MimoCodeNonRegularActiveLayer(path string) (string, bool) {
	for _, f := range MimoCodeReadLayerPaths(path) {
		st, err := os.Lstat(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue // absent layer — skipped in the merge, not a fault
			}
			return f, true // unreadable existing layer — merged read would fail
		}
		if !st.Mode().IsRegular() {
			return f, true // directory / FIFO / device / symlink — merged read would fail
		}
	}
	return "", false
}

// MimoCodeHasInlineContent reports whether the scan-supplied config path resolves
// to a PARSEABLE MIMOCODE_CONFIG_CONTENT inline layer. Exported so
// internal/api/scan.go can promote MiMoCode's config PRESENCE for an INLINE-ONLY
// profile — one whose ONLY mimo config layer is MIMOCODE_CONFIG_CONTENT (no
// stat-able file on disk; bot PR #420 finding 1). MimoCodeReadLayerPaths yields
// only FILE paths, so an inline-only host has no presence file to stat and would
// never promote to "ok" — yet MimoCodeMergedConfig WOULD parse the inline layer
// and surface its servers. This helper closes that gap by reporting inline
// presence directly.
//
// It uses the SAME env-resolution gate as MimoCodeReadLayerPaths /
// MimoCodeMergedConfig: the inline env is honored ONLY when path is a known
// global layer name (a temp/test override path stays state-safe single-file and
// never reads MIMOCODE_CONFIG_CONTENT). "Parseable" is required so a malformed
// inline string does NOT promote presence to "ok" — the malformed case is its
// own config-error state (see MimoCodeInlineContentState), surfaced loud by scan
// rather than left silently "absent" (bot PR #420 finding 4).
//
// Signature retained as bool (a thin wrapper over MimoCodeInlineContentState) so
// existing callers are unaffected; the malformed branch is handled by the new
// tri-state probe.
func MimoCodeHasInlineContent(path string) bool {
	state, _ := MimoCodeInlineContentState(path)
	return state == mimoCodeInlineStateOK
}

// Inline-content tri-state values returned by MimoCodeInlineContentState.
const (
	mimoCodeInlineStateNone  = ""      // no inline content (unset / state-safe override path)
	mimoCodeInlineStateOK    = "ok"    // inline content present AND parseable
	mimoCodeInlineStateError = "error" // inline content present BUT unparseable (malformed profile)
)

// MimoCodeInlineContentState reports the tri-state of the resolved
// MIMOCODE_CONFIG_CONTENT inline layer for a scan-supplied config path (bot PR
// #420 finding 4):
//
//   - "" (none)  — no inline content (unset, or a state-safe override path that
//     does not read the env). err is nil.
//   - "ok"       — inline content present AND parseable. err is nil.
//   - "error"    — inline content present BUT unparseable. err carries the parse
//     error so the caller can surface it.
//
// A malformed inline-only profile (MIMOCODE_CONFIG_CONTENT set + broken, no file
// layers) previously returned not-present here, so scan left the client "missing"
// and never reached the loud merged-read error — an active-but-broken profile
// looked ABSENT. This tri-state lets scan.go promote presence to the config-error
// "error" state instead, rendering the matrix cell as a config fault, not absent.
// Same env-resolution gate as MimoCodeHasInlineContent (inline honored only for a
// known global layer path).
func MimoCodeInlineContentState(path string) (string, error) {
	content := mimoCodeClientForScanPath(path).inlineContent
	if content == "" {
		return mimoCodeInlineStateNone, nil
	}
	if _, err := parseJSONCBytes([]byte(content)); err != nil {
		return mimoCodeInlineStateError, err
	}
	return mimoCodeInlineStateOK, nil
}

// MimoCodeHasClaudeImport reports whether the scan-supplied config path resolves
// to a PARSEABLE ~/.claude.json mcpServers import that yields at least one
// importable MiMoCode entry. Exported so internal/api/scan.go can promote
// MiMoCode's config PRESENCE for a CLAUDE-IMPORT-ONLY profile — one whose ONLY
// active mimo MCP source is the ~/.claude.json import (no mimo config file, no
// inline layer; bot PR #420 finding 1, r15). MimoCodeReadLayerPaths yields only
// FILE layers and MimoCodeInlineContentState only the inline string, so neither
// promotes such a profile — yet MimoCodeMergedConfig WOULD import the claude
// servers and surface them. Without this, scanIfReadable skips scanMimoCode and
// the claude-imported servers vanish from the Servers matrix until the operator
// creates a stub mimocode.json. This helper closes that gap, mirroring the
// existing inline-content (MimoCodeHasInlineContent) promotion.
//
// It uses the SAME env-resolution gate as MimoCodeReadLayerPaths / the merged
// read: the ~/.claude.json import is honored ONLY when path is a known global
// layer name AND the MIMOCODE_DISABLE_CLAUDE_CODE_MCP flag is unset (a temp/test
// override path keeps claudeHome "" → no import; the disable flag short-circuits
// it). So a scan under the standard test barrier (isolateMimoCodeScanEnv sets the
// disable flag) NEVER reads the developer's real ~/.claude.json — exactly the
// same state-safe gate the read path uses. claudeImportEntries is the single
// owner of the read + conversion (and swallows a malformed/unreadable claude.json
// to zero entries — best-effort import), so a broken claude.json never promotes
// to "ok" (it imports nothing) and never aborts the scan.
func MimoCodeHasClaudeImport(path string) bool {
	imported, err := mimoCodeClientForScanPath(path).claudeImportEntries()
	if err != nil {
		// claudeImportEntries is best-effort and never returns a non-nil error
		// today (a malformed/unreadable claude.json is swallowed to nil,nil), but
		// guard defensively: a future hard-failure import mode must not be read as
		// "has import" → no promotion on error.
		return false
	}
	return len(imported) > 0
}

// mimoCodeClientForScanPath builds the read-only client a scan/extract entry
// point uses for a supplied config path, resolving the MIMOCODE_CONFIG file, the
// MIMOCODE_CONFIG_DIR overlay, the MIMOCODE_CONFIG_CONTENT inline content, and the
// ~/.claude.json import home from the live environment — but ONLY when the path is
// a known global layer name (so a temp/test path stays state-safe single-file).
// Single owner of the scan-side path→resolution mapping so the merged-read and the
// layer-path probe never diverge.
//
// claudeHome is the OS home dir (os.UserHomeDir → $HOME / $USERPROFILE), matching
// MiMoCode's Global.Path.home (config.ts:888); it lets the scan side see the same
// ~/.claude.json-imported servers the adapter does (bot PR #420 finding 3). A
// home-resolution failure is non-fatal here (claudeHome stays "" → no import), so
// a scan still works on a host with no resolvable home.
func mimoCodeClientForScanPath(path string) *mimoCodeClient {
	if !mimoCodeIsGlobalLayerName(filepath.Base(path)) {
		// Explicit/temp override — state-safe single-file, no env-resolved layers.
		return &mimoCodeClient{path: path}
	}
	// State-safety: the ~/.claude.json import reads $HOME / $USERPROFILE via
	// os.UserHomeDir(), an ambient input the MIMOCODE_* env isolation does NOT
	// clear. Gate the home resolution on the SAME flag that disables the import in
	// production (MIMOCODE_DISABLE_CLAUDE_CODE_MCP): when it is set, claudeHome
	// stays "" → no import. isolateMimoCodeEnv sets this flag truthy by default, so
	// a scan/merge through a global-layer-named TEMP path under the standard test
	// barrier never reaches the developer's real ~/.claude.json. Production
	// (flag unset) resolves the OS home exactly as before — claudeImportEntries
	// re-checks the same flag, so behavior for real operators is unchanged. A
	// home-resolution failure is non-fatal (claudeHome stays "" → no import).
	claudeHome := ""
	if os.Getenv(mimoCodeDisableClaudeImportEnv) == "" {
		claudeHome, _ = os.UserHomeDir()
	}
	return &mimoCodeClient{
		path:          path,
		configFile:    cwdResolvedEnv("MIMOCODE_CONFIG"),
		overlayDir:    cwdResolvedEnv("MIMOCODE_CONFIG_DIR"),
		inlineContent: os.Getenv("MIMOCODE_CONFIG_CONTENT"),
		claudeHome:    claudeHome,
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
	} else if shadow.Kind == "managed" {
		// macOS Managed Preferences (MDM) — read-only, cannot be written by the
		// hub; fail loud so the operator removes it in their MDM profile (bot PR
		// #420 finding 1).
		return &ErrMimoCodeManagedShadowsServer{
			Server:      entry.Name,
			WriteTarget: o.path,
			PlistFile:   shadow.PlistFile,
		}
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

// mimoCodeDefinedAtOrAboveWriteTarget reports whether mcp.<name> is defined in
// the write target (mimocode.json = o.path) OR any READ layer ABOVE it
// (mimocode.jsonc, the MIMOCODE_CONFIG file, the MIMOCODE_CONFIG_DIR overlay, or
// the MIMOCODE_CONFIG_CONTENT inline string) — i.e. in any layer the hub's own
// write could have CLOBBERED, or that wins the merge over the write target. It
// deliberately EXCLUDES the ONLY layer strictly BELOW the write target,
// config.json in the write-target dir: a name present ONLY there is an operator
// entry the hub never wrote (setMember/deleteMember touch o.path alone — see
// deleteMember's doc). GetEntry uses this verdict to STAMP SourceBelowWriteTarget
// (false here = at/above = copy-up-OK; true = below/import = remove-not-copy-up on
// rollback) rather than to suppress the entry, so read-membership still sees a
// below-only name — see GetEntry's source-layer split (bot PR #420 finding 1).
//
// State-safe: in the explicit/temp single-file mode (basename not a global layer
// name) readLayerFiles returns only [o.path] (the write target), so the loop
// checks exactly that file and the lower-layer exclusion is a no-op — it never
// reaches the real ~/.config/mimocode. A parse error on a present layer
// propagates (a malformed layer must not be silently read as "not defined").
func (o *mimoCodeClient) mimoCodeDefinedAtOrAboveWriteTarget(name string) (bool, error) {
	lowerLayer := filepath.Clean(filepath.Join(filepath.Dir(o.path), "config.json"))
	for _, f := range o.readLayerFiles() {
		if filepath.Clean(f) == lowerLayer {
			continue // the sole layer strictly below the write target — excluded
		}
		defined, err := mimoCodeFileDefines(f, name)
		if err != nil {
			return false, err
		}
		if defined {
			return true, nil
		}
	}
	// Inline MIMOCODE_CONFIG_CONTENT is a TOP (above) layer, not a file path.
	if o.inlineContent != "" {
		defined, err := mimoCodeInlineDefines(o.inlineContent, name)
		if err != nil {
			return false, err
		}
		if defined {
			return true, nil
		}
	}
	// The ~/.claude.json MCP import is deliberately NOT counted here. It is
	// SKIP-IF-NAME-EXISTS (config.ts:694-698) and the hub never writes .claude.json,
	// so a name present ONLY in ~/.claude.json is — exactly like the config.json
	// layer below — an entry the hub never wrote and never clobbered. For the
	// install/register rollback it must behave as "no prior" (nil-prior →
	// RemoveEntry the hub's write-target key, letting the ~/.claude.json entry
	// re-emerge via the merge), NOT as a restore candidate that GetEntry would copy
	// UP into the write target (which would shadow the operator's claude.json entry
	// forever — the same hazard the config.json exclusion above prevents).
	return false, nil
}

// mimoCodeNameReResolvesAfterWriteTargetRemoval reports whether mcp.<name> would
// STILL resolve in the merged read after a hypothetical RemoveEntry(name) from
// the WRITE TARGET (mimocode.json = o.path) — i.e. whether ANY layer OTHER than
// the write target defines the name. Because the merge is REPLACE-by-name (an mcp
// entry never field-merges across layers; see readMergedLayers), the name
// re-resolves iff some other layer still defines it after the write-target key is
// gone.
//
// This is the "sole effective definition" predicate for the DESTRUCTIVE direct-LSP
// cleanup (bot PR #420 r15 finding 2). FindStdioLanguageServerEntries' consumers
// (register.go post-register cleanup, `mcphub language-server cleanup`) RemoveEntry
// each returned name from the write target ONLY. If the SAME name is also defined
// in another layer, the cleanup logs success yet the other layer re-emerges in the
// merged read — the LSP entry stays active. So the cleanup must DECLINE (not
// report) a name that re-resolves after removal: report only when the write target
// is the SOLE definer.
//
// LAYER COVERAGE — the complement of the write target, composed from the existing
// single-owner per-layer membership primitives (NO re-implementation of the merge):
//
//   - HIGHER layers (MDM, inline, MIMOCODE_CONFIG_DIR overlay, MIMOCODE_CONFIG
//     file, global mimocode.jsonc) → mimoCodeHigherLayerDefining(name). It already
//     excludes the write target and the claude import, exactly what is needed here.
//   - The config.json layer strictly BELOW the write target → mimoCodeFileDefines.
//     A name present ONLY there re-emerges after the write-target removal, so it
//     must count.
//   - The ~/.claude.json import → claudeImportEntries(). This is the CRITICAL
//     asymmetry from the AddEntry shadow guard (mimoCodeHigherLayerDefining) and
//     mimoCodeDefinedAtOrAboveWriteTarget, both of which correctly EXCLUDE the
//     import: there the import can never shadow a name the hub is about to write /
//     has written (skip-if-name-exists skips it while the write target defines it).
//     HERE the sequence is reversed — the write target CURRENTLY defines the name
//     (so the import is currently skipped), RemoveEntry deletes the write-target
//     key, and on the next merged read skip-if-name-exists no longer fires → the
//     import RE-EMERGES the entry. So the import IS a re-emergence source and MUST
//     be checked. Any re-emerged shape (stdio-LSP or not) means the destructive
//     RemoveEntry did not clear the name, so name presence in the converted import
//     — not its shape — is the right test.
//
// DISABLED ENTRIES COUNT. The membership primitives test raw name presence and do
// NOT drop enabled:false (unlike the matcher path, which drops disabled before
// shape-matching). That asymmetry is intentional and correct: the question is "is
// mcp.<name> still PRESENT in the merged map after removal", not "is it still an
// active stdio-LSP". A disabled definition in another layer still leaves the key
// present (the deep-merge keeps it), so the destructive removal did not clear the
// name → decline. Declining is the conservative side (the operator resolves the
// ambiguous cross-layer entry manually); reporting-and-deleting on the false
// belief that the name is gone is the bug this guards.
//
// State-safe: in the explicit/temp single-file mode mimoCodeHigherLayerDefining
// reads only the explicit configFile/overlayDir/inline captured at construction
// (empty for a direct/test client), the config.json-below path sits in the temp
// dir, and claudeImportEntries returns nil when claudeHome is "" — so it never
// reaches the real ~/.config/mimocode or ~/.claude.json. A parse error on any
// present layer propagates (a malformed layer must not be silently read as "does
// not define" — that would re-introduce the silent-false-success this guards).
func (o *mimoCodeClient) mimoCodeNameReResolvesAfterWriteTargetRemoval(name string) (bool, error) {
	// HIGHER layers (MDM / inline / overlay / MIMOCODE_CONFIG / global jsonc).
	if shadow, err := o.mimoCodeHigherLayerDefining(name); err != nil {
		return false, err
	} else if shadow.Kind != "" {
		return true, nil
	}
	// config.json strictly BELOW the write target (in the write-target dir).
	belowLayer := filepath.Join(filepath.Dir(o.path), "config.json")
	if defined, err := mimoCodeFileDefines(belowLayer, name); err != nil {
		return false, err
	} else if defined {
		return true, nil
	}
	// ~/.claude.json import — a re-emergence source HERE (skip-if-name-exists no
	// longer skips once the write-target key is removed).
	imported, err := o.claudeImportEntries()
	if err != nil {
		return false, err
	}
	if _, ok := imported[name]; ok {
		return true, nil
	}
	return false, nil
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
// SOURCE-LAYER SPLIT (bot PR #420 finding 1; r14 read-vs-rollback split).
// GetEntry reads the MERGED view, so a same-named server defined ONLY in the
// config.json layer BELOW the write target — or ONLY in the ~/.claude.json
// mcpServers import (also never hub-written) — projects as a non-nil entry. Two
// callers consume that entry through the generic Client interface:
//
//   - READ-MEMBERSHIP (discovery / idempotency / hub-gate / lsp-router /
//     demigrate / serena-reconcile): they need a non-nil entry so the hub SEES
//     the server as present. Returning (nil, nil) for a below/import-only name —
//     as the prior self-review did — BREAKS that membership read.
//   - ROLLBACK RESTORE-UP (install.go / register.go): on a downstream failure it
//     runs AddEntry(*prior) to restore the prior UP into the write target. For a
//     below/import-sourced prior that copy-up is WRONG: it SHADOWS the operator's
//     config.json / ~/.claude.json forever (the write-target copy now wins the
//     merge) and, for an import prior, LEAKS the claude.json credentials into
//     mimocode.json. The hub physically only ever writes the write target
//     (setMember/deleteMember touch o.path alone), so a name present only below
//     or in the import is an operator entry the hub never wrote and never
//     clobbered: the correct rollback is RemoveEntry the hub's write-target key,
//     letting the lower/import layer re-emerge.
//
// To satisfy BOTH, GetEntry RETURNS the projected entry (read-membership) but
// STAMPS SourceBelowWriteTarget = !mimoCodeDefinedAtOrAboveWriteTarget(name):
// true for a below/import-only name, false for a name in the write target or a
// HIGHER layer (a real, hub-clobberable restore candidate — moot at rollback
// since AddEntry shadow-refuses the install before any write). The 2 rollback
// sites route a SourceBelowWriteTarget prior to RemoveEntry instead of AddEntry,
// preserving the no-credential-copy-up security invariant. Every OTHER GetEntry
// caller ignores the field; no other adapter sets it.
//
// A genuinely missing entry returns (nil, nil).
func (o *mimoCodeClient) GetEntry(name string) (*MCPEntry, error) {
	// Source-layer verdict (bot PR #420 finding 1, r14 read-vs-rollback split).
	// atOrAbove=true ⇒ the name lives in the write target or a layer ABOVE it (a
	// real restore candidate the rollback may copy UP). atOrAbove=false ⇒ it lives
	// ONLY in config.json (below) or the ~/.claude.json import — never hub-written;
	// read-membership must still SEE it (return a non-nil entry), but the rollback
	// must REMOVE the hub key, not copy this entry up (no credential copy-up). So
	// instead of early-returning (nil,nil) for a below/import-only name (which broke
	// read membership), GetEntry now projects the merged entry and stamps
	// SourceBelowWriteTarget=true so the install/register rollback takes the
	// remove-not-copy-up branch. The ERROR still aborts (r12/r13 data-loss guard):
	// a malformed lower layer returns (nil, err), never (nil, nil).
	atOrAbove, err := o.mimoCodeDefinedAtOrAboveWriteTarget(name)
	if err != nil {
		return nil, err
	}
	below := !atOrAbove
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
	// WRITE-TARGET-OWN-VALUE snapshot (bot PR #420 finding 3). `raw` above is the
	// MERGED view across layers. The merge is REPLACE-by-name EXCEPT an
	// enabled-only HIGHER override, which FIELD-merges its `enabled` onto the lower
	// entry (mimoCodeDeepMerge). So when the write target carries a bare
	// {enabled:false} stub over a LOWER config.json server, `raw` is the SYNTHESIS
	// {command:...lower..., enabled:false}. A rollback AddEntry(*prior) with that
	// merged Raw would COPY the lower layer's `command` UP into mimocode.json,
	// permanently shadowing the layer the hub never owned. So snapshot the Raw from
	// the write target's OWN physical value (ownRaw) — the bare stub as it exists
	// in mimocode.json — and judge the branch SHAPE against it too.
	//
	// CONDITION — snapshot ownRaw ONLY when the write target is the layer that
	// RESOLVES the merge for `name`, i.e. ownOK AND NO higher layer defines it.
	// When a HIGHER layer (mimocode.jsonc / MIMOCODE_CONFIG / overlay / inline /
	// MDM) ALSO defines `name`, that higher layer WINS the merge (replace-by-name),
	// so `raw` is the higher-layer value and read-membership MUST return it (the
	// merge-precedence tests depend on this) — and the rollback is MOOT there
	// anyway (AddEntry shadow-refuses a higher-layer-defined name before any
	// write). When the write target has NO own value (!ownOK — name lives only in a
	// HIGHER layer, or only below/in the import), effRaw stays the merged raw: a
	// higher-only prior is shadow-refused, and a below/import prior
	// (SourceBelowWriteTarget=true) routes to RemoveEntry, not AddEntry. So no
	// lower/higher-layer command/url is EVER copied up.
	ownRaw, ownOK, err := mimoCodeFileEntryValue(o.path, name)
	if err != nil {
		return nil, err
	}
	higher, err := o.mimoCodeHigherLayerDefining(name)
	if err != nil {
		return nil, err
	}
	effRaw := raw
	if ownOK && higher.Kind == "" {
		effRaw = ownRaw
	}
	// Disabled verdict (bot PR #420 finding 5), computed ONCE against effRaw (the
	// write-target's own value when resident) so it matches the physical entry
	// regardless of which shape branch the projection takes below. Stamped on
	// EVERY return branch so a disabled entry is flagged whether it is a bare
	// {enabled:false} stub (url-less branch), a disabled remote (url branch), or a
	// disabled extra-fields remote. The gating consumer (api.GatedOnClients)
	// excludes a disabled `mcphub-hub` aggregate from gate-ON detection: the client
	// never loads a disabled aggregate, so a `--reset-port` would orphan nothing —
	// blocking the reset on it is a false positive. Read-membership / rollback
	// callers ignore the flag and still see the non-nil entry. Mirrors the scan
	// path (shapeMimoCodeEntry classifies enabled:false as Transport "absent").
	disabled := mimoCodeEntryDisabled(effRaw)
	url, _ := effRaw["url"].(string)
	if url == "" {
		// URL-less local entry (or a url-less remote) — not representable as a
		// URL MCPEntry. Carry the verbatim value in Raw so rollback restores it
		// exactly rather than deleting it or corrupting it to {type:remote,url:""}.
		return &MCPEntry{Name: name, Raw: effRaw, SourceBelowWriteTarget: below, Disabled: disabled}, nil
	}
	// A DISABLED URL entry (enabled:false) is also not faithfully representable
	// as a {URL,Headers} MCPEntry: AddEntry's normal install path hardcodes
	// enabled:true, so a GetEntry→AddEntry(*prior) rollback would silently
	// RE-ENABLE a server the operator had disabled (bot PR #420 finding 5).
	// Carry the verbatim entry in Raw so the rollback writes it back byte-shaped
	// (Raw wins in AddEntry), preserving enabled:false.
	if disabled {
		return &MCPEntry{Name: name, Raw: effRaw, SourceBelowWriteTarget: below, Disabled: true}, nil
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
	if mimoCodeRemoteHasExtraFields(effRaw) {
		return &MCPEntry{Name: name, Raw: effRaw, SourceBelowWriteTarget: below, Disabled: disabled}, nil
	}
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(effRaw, "headers"), SourceBelowWriteTarget: below, Disabled: disabled}, nil
}

// mimoCodeEntryDisabled reports whether a parsed mcp entry map carries an
// explicit `enabled: false`. MiMoCode uses `enabled` (default true) as the
// active flag, so an absent or true `enabled` is ACTIVE and only a literal
// boolean false is disabled — matching mimoCodeDropDisabled's bool-only handling
// and shapeMimoCodeEntry's scan-side classification (bot PR #420 finding 5).
func mimoCodeEntryDisabled(raw map[string]any) bool {
	if enabled, present := raw["enabled"]; present {
		if b, ok := enabled.(bool); ok && !b {
			return true
		}
	}
	return false
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
// `command` as an ARRAY (["npx","-y",...] / ["gopls","mcp"]) rather than a
// string; the shared collectStdioEntries reads `command` as a STRING and would
// miss them. mimoCodeNormalizeCommandArrays rewrites each array `command` into
// the (command-string, prepended-args) shape collectStdioEntries understands —
// the SAME normalization FindStdioLanguageServerEntries applies — so a mimo
// array-command stdio entry IS surfaced (bot PR #420 finding 3). Without it the
// orphan-process kill-pattern derivation (patternsFromClientStdio) and the gopls
// direct-LSP scan never see a mimo local entry.
//
// Normalization is orthogonal to layer scoping: it only fixes the array read
// shape, so the FULL merged view (across every layer) is preserved — the
// non-destructive kill-pattern consumer must still see lower-layer entries, an
// orphan process spawned from a lower-layer entry is a real orphan.
//
// KNOWN LIMITATION (gopls direct-LSP destructive path): the gopls cleanup at
// register.go consumes this merged view then RemoveEntry-s a match, but
// RemoveEntry deletes from the write target ONLY. A gopls direct entry living in
// a NON-write-target layer would log a false success and re-emerge via merge —
// the same class as finding 4 (filed as an adjacent finding) but on the shared
// AllStdioEntries/gopls path, which cannot be write-target-scoped mimo-locally
// without touching a shared surface. The write-target restriction is applied in
// FindStdioLanguageServerEntries (mcp-language-server cleanup) only.
func (o *mimoCodeClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return collectStdioEntries(mimoCodeNormalizeCommandArrays(servers)), nil
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
	matched := findLanguageServerStdioInMap(mimoCodeNormalizeCommandArrays(mimoCodeDropDisabled(servers)))
	// Write-target SHAPE restriction (bot PR #420 finding 4 + r12 HIGH). The ONLY
	// consumers of this method are DESTRUCTIVE — the post-register direct-LSP
	// cleanup (register.go) and `mcphub language-server cleanup`
	// (cli/language_server.go) both call RemoveEntry on each returned entry.
	// RemoveEntry → deleteMember deletes from the write target (o.path =
	// mimocode.json) ONLY. An mcp-language-server entry living in a LOWER layer
	// (config.json) or a HIGHER layer (mimocode.jsonc / MIMOCODE_CONFIG / overlay
	// / inline) would be "removed" with a logged success yet re-emerge via the
	// merge — leaving the LSP entry active.
	//
	// Membership-by-NAME is NOT sufficient (r12 HIGH): the matched shape is from
	// the MERGED view (could be a HIGHER layer's stdio shape), while RemoveEntry
	// deletes the WRITE-TARGET's value (could be a DIFFERENT shape). A name that
	// is stdio in mimocode.jsonc but a hub REMOTE entry in mimocode.json would, on
	// a name-only test, be reported → RemoveEntry would delete the write-target
	// REMOTE, leaving the higher stdio active (WRONG entry deleted). So report
	// only entries whose WRITE-TARGET OWN value is ITSELF the stdio-LSP shape —
	// mimoCodeFileDefinesStdioLSP routes that decision through the same single
	// classifier (matchLanguageServerStdio) that produced the merged match. Under
	// the replace-by-name merge an mcp entry never field-merges across layers, so
	// a write-target stdio entry is self-contained when the write target is the
	// highest definer; when a higher layer shadows it, declining is correct (the
	// write-target value is not what MiMoCode loads).
	//
	// SOLE-EFFECTIVE-DEFINITION (bot PR #420 r15 finding 2). The write-target SHAPE
	// gate above is necessary but NOT sufficient: even when the write target IS the
	// stdio-LSP shape, RemoveEntry deletes mcp.<name> from the write target ONLY, so
	// if the SAME name is ALSO defined in ANOTHER layer (config.json BELOW, a higher
	// file/inline layer, or the ~/.claude.json import) that other layer RE-EMERGES
	// in the merged read after removal — the destructive cleanup logs success yet
	// the LSP entry stays active. So additionally DECLINE any name that would
	// re-resolve after a hypothetical write-target removal: report only when the
	// write target is the SOLE defining layer.
	// mimoCodeNameReResolvesAfterWriteTargetRemoval owns that cross-layer predicate
	// (composing the existing per-layer membership helpers; it does NOT fork the
	// merge), and crucially COUNTS the ~/.claude.json import — opposite the AddEntry
	// shadow guard's exclusion — because skip-if-name-exists stops skipping once the
	// write-target key is gone.
	out := matched[:0]
	for _, e := range matched {
		defined, err := mimoCodeFileDefinesStdioLSP(o.path, e.Name)
		if err != nil {
			return nil, err
		}
		if !defined {
			continue
		}
		reResolves, err := o.mimoCodeNameReResolvesAfterWriteTargetRemoval(e.Name)
		if err != nil {
			return nil, err
		}
		if reResolves {
			// Another layer also defines this name → RemoveEntry from the write
			// target would NOT clear it (the other layer re-emerges). Decline so the
			// destructive cleanup does not falsely report a removable LSP entry the
			// hub cannot fully remove.
			continue
		}
		out = append(out, e)
	}
	return out, nil
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

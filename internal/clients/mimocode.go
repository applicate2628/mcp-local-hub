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
//     writable path and NEVER written. If the managed layer defines the server being
//     installed, AddEntry returns a typed loud error rather than reporting a
//     successful write to mimocode.json (MiMoCode would keep resolving the managed
//     entry — a silent false success). TWO managed layers are detected (bot PR #420
//     r17 finding P1b): macOS Managed Preferences (MDM, via mimoCodeReadManagedPrefs)
//     AND the managed config dir (%ProgramData%\opencode | /Library/Application
//     Support/opencode | /etc/opencode, via mimoCodeManagedConfigDirShadows;
//     basename "opencode", inherited from upstream). The org-account config and the
//     remote well-known config remain documented un-detectable limitations: detecting
//     them would need MiMoCode's private auth state and a per-operation network call
//     the hub deliberately does not make — see mimoCodeHigherLayerDefining's doc.
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
// (it would otherwise win the merge and silently mask the hub entry), and
// RemoveEntry fails loud when a higher layer still retains the server after the
// write-target delete (bot PR #420 r17 finding B4). The HOME ~/.mimocode dir IS
// now detected by the shadow guard (resolvable from home alone — bot PR #420 r17
// finding P1a). The PROJECT-root / project `.mimocode` overlay remains a documented
// LIMITATION: the single-target hub adapter has no project/worktree context and the
// shadowing project depends on the directory the operator later runs MiMoCode from
// — see mimoCodeHigherLayerDefining's doc. This adapter targets the resolved global
// dir plus the MIMOCODE_CONFIG file, the MIMOCODE_CONFIG_DIR overlay,
// MIMOCODE_CONFIG_CONTENT inline, and the home ~/.mimocode dir; the managed config
// dir + MDM are detect-and-fail-loud.
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
	// home anchors the GLOBAL config-dir / write-target resolution. It uses
	// os.UserHomeDir() to match MiMoCode's xdg-basedir ~/.config fallback, which
	// resolves ~ via os.homedir() (NOT $HOME-first). This is unchanged by B6.
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
	// claudeHome anchors the ~/.claude.json import AND the home ~/.mimocode shadow
	// layer. It uses the MiMoCode Global.Path.home resolution (HOME || USERPROFILE
	// || os.homedir() — mimoCodeClaudeImportHome), NOT os.UserHomeDir(), so a
	// Windows host where HOME != USERPROFILE reads the SAME ~/.claude.json MiMoCode
	// imports (bot PR #420 r17 finding B6). A resolution failure here is non-fatal:
	// claudeHome stays "" → no import / no home-.mimocode shadow, never an error
	// that fails the whole client construction.
	claudeHome, _ := mimoCodeClaudeImportHome()
	return newLockingClient(&mimoCodeClient{
		path:          r.writeTarget,
		configFile:    r.configFile,
		overlayDir:    r.overlayDir,
		inlineContent: r.inlineContent,
		claudeHome:    claudeHome,
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

// mimoCodeClaudeImportHome resolves the OS home dir the ~/.claude.json import
// (and the home ~/.mimocode shadow layer) anchors on, FAITHFULLY to MiMoCode's
// Global.Path.home (global.ts:13-15 / 68):
//
//	process.env.HOME || process.env.USERPROFILE || os.homedir()
//
// This is the SINGLE owner of the claude-import home so both the live client
// (NewMimoCode) and the scan side (mimoCodeClientForScanPath) read the SAME
// ~/.claude.json MiMoCode itself would import (bot PR #420 r17 finding B6). It
// MUST NOT use os.UserHomeDir() directly: on Windows os.UserHomeDir() returns
// %USERPROFILE%, but MiMoCode prefers $HOME first. On a Git-Bash / MSYS host
// where HOME != USERPROFILE, os.UserHomeDir() would read %USERPROFILE%\.claude.json
// while MiMoCode reads $HOME\.claude.json → claude-import-only servers vanish from
// the matrix and the re-emergence checks miss them.
//
// On POSIX os.UserHomeDir() already returns $HOME first, so the explicit HOME ->
// USERPROFILE -> os.UserHomeDir() chain is a no-op refinement there and a
// behavior fix only on Windows; keeping it uniform avoids an OS-specific branch.
// Returns ("", err) only when none of the three resolve.
func mimoCodeClaudeImportHome() (string, error) {
	if h := os.Getenv("HOME"); h != "" {
		return h, nil
	}
	if u := os.Getenv("USERPROFILE"); u != "" {
		return u, nil
	}
	return os.UserHomeDir()
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
//     1. config.json + mimocode.json + mimocode.jsonc in o.path's own dir
//     (the global getGlobal() layers, .jsonc winning);
//     2. configFile (MIMOCODE_CONFIG), when set, ABOVE the global layers
//     (bot PR #420 finding 1 — it merges on top, NOT a replacement);
//     3. the HOME ~/.mimocode dir's mimocode.json + mimocode.jsonc, when
//     claudeHome is set, ABOVE the MIMOCODE_CONFIG file (config.ts:773-783,
//     step 3b — same precedence band the shadow walk uses; bot PR #420 r18 P2);
//     4. when overlayDir (MIMOCODE_CONFIG_DIR) is set, its mimocode.json +
//     mimocode.jsonc appended LAST so the overlay wins per key.
//
// It never recomputes a directory from env/home beyond the configFile /
// overlayDir captured at construction and the home ~/.mimocode dir gated on
// claudeHome (the SAME state-safe gate the home-shadow read and the
// ~/.claude.json import use — claudeHome is "" for direct/test construction and
// under the scan test barrier, so a temp/test read never reaches the real home);
// it stays inside dir(o.path) plus those explicit paths.
func (o *mimoCodeClient) readLayerFiles() []string {
	return mimoCodeReadLayerFiles(o.path, o.configFile, o.overlayDir, o.claudeHome)
}

// mimoCodeReadLayerFiles is the pure resolver behind
// (*mimoCodeClient).readLayerFiles. See that method's doc for the rules. The
// basename heuristic is the state-safe single-file collapse for direct
// (scan/extract/test) construction whose path is not a known global layer name.
func mimoCodeReadLayerFiles(path, configFile, overlayDir, claudeHome string) []string {
	base := filepath.Base(path)
	if !mimoCodeIsGlobalLayerName(base) {
		// Explicit override (temp/test path, basename not a global layer name):
		// operate only on the supplied file; do NOT recompute the dir, pull
		// siblings, the MIMOCODE_CONFIG file, the home .mimocode dir, or the overlay.
		return []string{path}
	}
	dir := filepath.Dir(path)
	files := make([]string, 0, len(mimoCodeGlobalLayerNames)+1+2*len(mimoCodeOverlayLayerNames))
	for _, n := range mimoCodeGlobalLayerNames {
		files = append(files, filepath.Join(dir, n))
	}
	// MIMOCODE_CONFIG file: a single FILE merged ABOVE the global layers.
	if configFile != "" {
		files = append(files, configFile)
	}
	// HOME ~/.mimocode dir (config.ts:773-783, step 3b): merged ABOVE the
	// MIMOCODE_CONFIG file and BELOW the MIMOCODE_CONFIG_DIR overlay — the same
	// precedence band the shadow walk gives it (mimoCodeHomeMimocodeDirShadows). Read
	// AND shadow-detected consistently now (bot PR #420 r18 P2): before this, the
	// home layer was shadow-refused on AddEntry yet invisible to readMergedLayers /
	// GetEntry / scan, so a server living ONLY in ~/.mimocode was hidden from the
	// matrix and read-membership while AddEntry refused the same name. Gated on
	// claudeHome (state-safe: "" for direct/test and under the scan barrier).
	if claudeHome != "" {
		homeDir := filepath.Join(claudeHome, ".mimocode")
		for _, n := range mimoCodeOverlayLayerNames {
			files = append(files, filepath.Join(homeDir, n))
		}
	}
	// MIMOCODE_CONFIG_DIR overlay: ABOVE the home .mimocode dir.
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
// macOS Managed Preferences (MDM) layer (PlistFile names the plist);
// "managed-config-dir" for the cross-platform admin/system-deployed managed
// config DIR layer (%ProgramData%\opencode | /Library/Application Support/opencode
// | /etc/opencode) — File names the offending managed file (bot PR #420 r18
// MEDIUM finding). The MDM and managed-config-dir kinds are DISTINCT because their
// remediation surfaces differ: MDM points the operator at a Managed Preferences
// PROFILE (macOS-only, plist), the managed config dir points at an actual editable
// FILE present on every OS — conflating them told a Windows/Linux operator to
// remove a non-existent MDM profile and omitted the real file path.
type mimoCodeShadowSource struct {
	Kind  string // "", "inline", "file", "managed", or "managed-config-dir"
	Label string // human label for the "file" / "managed-config-dir" kind
	File  string // file path for the "file" / "managed-config-dir" kind

	// PlistFile is the plist path for the "managed" (macOS MDM) kind. Separate
	// from File so the managed branch can carry the MDM plist source without
	// conflating it with the operator-editable "file" / "managed-config-dir"
	// layers.
	PlistFile string
}

// ErrMimoCodeHigherLayerRetainsServer is returned by RemoveEntry when, AFTER
// deleting mcp.<Server> from the write target, the name STILL re-resolves from a
// HIGHER layer the hub cannot remove (the MDM/managed layer, the
// MIMOCODE_CONFIG_CONTENT inline string, the MIMOCODE_CONFIG_DIR overlay, the
// MIMOCODE_CONFIG file, or the global mimocode.jsonc) (bot PR #420 r17 finding
// B4). RemoveEntry deletes only the write target (deleteMember touches o.path
// alone); a hub entry whose effective definition lives in a higher layer would
// otherwise have the delete report success while MiMoCode keeps loading the
// higher-layer value — a silent false success that makes a demigrate / uncheck
// look done while the server stays live. The hub must not edit the operator's /
// MDM's higher layer, so RemoveEntry fails loud: the operator removes the entry
// in the shadowing layer named here. NOTE: re-emergence from a layer BELOW the
// write target (config.json) or the ~/.claude.json import is the INTENDED
// rollback behavior (the operator's prior re-emerges) and is NOT an error — only
// a HIGHER (winning) layer triggers this.
type ErrMimoCodeHigherLayerRetainsServer struct {
	Server      string
	WriteTarget string
	Source      mimoCodeShadowSource // the higher layer that still defines the server
	// WriteTargetUnchanged is true when this error came from the MANAGED PRE-CHECK
	// in RemoveEntry — i.e. it was returned BEFORE deleteMember ran, so the write
	// target was NOT mutated (the file is byte-unchanged). It is false on the
	// post-delete B4 path, where the write-target key WAS removed but a higher
	// layer still wins. Error() branches the user-facing wording on this so a
	// pre-check refusal never tells the operator the server was "removed" when the
	// file was left untouched (bot PR #425 r5: "don't report a delete before it happens").
	WriteTargetUnchanged bool
}

func (e *ErrMimoCodeHigherLayerRetainsServer) Error() string {
	where := "a higher MiMoCode config layer"
	switch e.Source.Kind {
	case "managed":
		where = "the macOS Managed Preferences (MDM) layer"
		if e.Source.PlistFile != "" {
			where = fmt.Sprintf("the macOS Managed Preferences (MDM) layer %q", e.Source.PlistFile)
		}
	case "managed-config-dir":
		// The cross-platform managed config DIR layer (bot PR #420 r18 MEDIUM
		// finding): name the actual editable file, NOT an MDM profile (which does
		// not exist on Windows/Linux).
		where = "the managed config dir"
		if e.Source.File != "" {
			where = fmt.Sprintf("the managed config dir (%s)", e.Source.File)
		}
	case "inline":
		where = "the MIMOCODE_CONFIG_CONTENT inline config"
	case "file":
		if e.Source.File != "" {
			where = fmt.Sprintf("%s (%s)", e.Source.Label, e.Source.File)
		} else if e.Source.Label != "" {
			where = e.Source.Label
		}
	}
	if e.WriteTargetUnchanged {
		return fmt.Sprintf("mimocode: refused to remove server %q from the hub write target %q — it is also defined in %s, a read-only layer MiMoCode merges on top and wins, so removing the write-target entry would NOT unregister the server. The write target was left UNCHANGED; remove mcp.%s from that layer to unregister it",
			e.Server, e.WriteTarget, where, e.Server)
	}
	return fmt.Sprintf("mimocode: removed server %q from the hub write target %q, but it is still defined in %s, which MiMoCode merges on top and wins — the entry remains active; remove mcp.%s from that layer to fully unregister it",
		e.Server, e.WriteTarget, where, e.Server)
}

// mimoCodeManagedPrefsReader, when non-nil (tests only), replaces the production
// macOS Managed Preferences reader so the detect-and-fail-loud managed-shadow
// path can be exercised on a Windows/Linux CI runner without a real /Library
// plist or `plutil`. Mirrors the established func-var test-seam idiom
// (clients.go copyFileTornWindowHook): nil in production, assigned in a test
// with a t.Cleanup restore. Bot PR #420 finding 1.
var mimoCodeManagedPrefsReader func(name string) (mimoCodeShadowSource, error)

// mimoCodeManagedPrefsDisableOnlyReader, when non-nil (tests only), replaces the
// production macOS Managed Preferences DISABLE-ONLY classifier of the SELECTED plist
// (mimoCodeReadManagedPlistDisableOnly) so the managed disable-only case can be exercised
// on a Windows/Linux CI runner without a real /Library plist or `plutil`. Same
// nil-in-production func-var idiom as the enable-only-true seam above; keyed on `name`
// because on a non-darwin runner there is no real plist path to thread, so the seam owns
// the verdict directly. It reports whether the MDM plist carries a bare {enabled:false}
// ENABLE-ONLY overlay for mcp.<name> — the polarity mimoCodeShadowIsDisableOnlyOverride
// needs to classify a managed (MDM) shadow as disable-only (retains NO active server → the
// entry is removable; bot PR #425 follow-up FINDING 1 regression fix).
var mimoCodeManagedPrefsDisableOnlyReader func(name string) (bool, error)

// mimoCodeManagedPrefsEnableOnlyTrueReader, when non-nil (tests only), replaces the
// production macOS Managed Preferences ENABLE-ONLY-TRUE detector
// (mimoCodeReadManagedPrefsEnableOnlyTrue) so the FINDING 1 conservative cleanup guard's
// managed MDM-plist case can be exercised on a Windows/Linux CI runner without a real
// /Library plist or `plutil`. Same nil-in-production func-var idiom as the disable-only
// seam above. It reports whether the MDM plist carries a bare {enabled:true} ENABLE-ONLY
// overlay for mcp.<name> — the polarity mimoCodeManagedEnableOnlyTrueOverlay needs to
// CONSERVATIVELY exclude a candidate whose managed enable-only overlay would re-activate a
// lower disabled full entry (bot PR #425 follow-up FINDING 1 cleanup guard).
var mimoCodeManagedPrefsEnableOnlyTrueReader func(name string) (bool, error)

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

// ErrMimoCodeManagedConfigDirShadowsServer is returned by AddEntry when the
// cross-platform MANAGED CONFIG DIR layer (%ProgramData%\opencode |
// /Library/Application Support/opencode | /etc/opencode) already defines
// mcp.<Server>. DISTINCT from the macOS MDM error (bot PR #420 r18 MEDIUM
// finding): the offending source here is an actual editable FILE present on every
// OS — NOT an MDM Managed-Preferences profile (which exists only on macOS). The
// earlier code shared Kind:"managed" with the MDM path, so on Windows/Linux a
// managed-config-dir shadow told the operator to "remove mcp.X from the managed
// configuration profile" (a profile that does not exist there) and never named
// the real file. This error names the actual File the operator must edit. It is
// a read-only admin/system-deployed layer (config.ts:868-874), so the hub cannot
// write it — the operator edits the named file directly.
type ErrMimoCodeManagedConfigDirShadowsServer struct {
	Server      string
	WriteTarget string
	File        string // the managed config-dir file that defines the server
}

func (e *ErrMimoCodeManagedConfigDirShadowsServer) Error() string {
	where := "the managed config dir"
	if e.File != "" {
		where = fmt.Sprintf("the managed config dir file %q", e.File)
	}
	return fmt.Sprintf("mimocode: server %q is already defined in %s, which MiMoCode merges on top of the hub's write target %q and wins — the hub entry would have no effect and the hub cannot write an admin/system-deployed managed layer; remove mcp.%s from that file, then retry",
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
	// Plist path resolution (per-user-first, managed.ts:49 username + path order) is
	// the single owner mimoCodeManagedPlistFiles; off darwin it returns nil so the
	// loop is a no-op (managed.ts:47). The shadow VERDICT (mimoCodeMapShadows) is
	// unchanged — only the path-list construction is shared with the raw-value reader.
	for _, plist := range mimoCodeManagedPlistFiles() {
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
		// SHADOW-AWARE (bot PR #420 r17 finding B5): an enabled-only:true managed
		// override is not a shadow either — only a disabling/full-redefinition value
		// overrides the hub write.
		if mimoCodeMapShadows(m, name) {
			return mimoCodeShadowSource{Kind: "managed", Label: "macOS Managed Preferences", PlistFile: plist}, nil
		}
	}
	return mimoCodeShadowSource{}, nil
}

// mimoCodeManagedPlistFiles resolves the ordered macOS Managed Preferences (MDM)
// plist paths (per-user first, then system), matching managed.ts's path order. The
// SINGLE owner of the plist-path resolution shared by every MDM reader
// (mimoCodeReadManagedPrefs and the raw-value reader)
// so the username/path logic is not re-encoded per reader. Off darwin → nil.
func mimoCodeManagedPlistFiles() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	const managedPlistDomain = "ai.opencode.managed"
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
	return plists
}

// mimoCodeReadManagedPlistDisableOnly reports whether the SPECIFIC macOS Managed
// Preferences (MDM) plist at `plist` carries a bare {enabled:false} ENABLE-ONLY overlay
// for mcp.<name> (bot PR #425 FINDING 2). It classifies the SELECTED plist — the one
// mimoCodeManagedLayerShadows / mimoCodeReadManagedPrefs ALREADY CHOSE as the shadow
// source (pinned by mimoCodeShadowSource.PlistFile) — NOT a fresh top-of-list re-scan.
// This is the FINDING 2 fix: a dual-plist host (per-user {enabled:true} + system
// disable-only for the same name) has the SHADOW-AWARE reader skip the higher
// non-shadowing per-user plist and select the LOWER system disable-only plist; a re-scan
// that stopped at the FIRST plist merely DEFINING the name would classify the per-user
// {enabled:true} plist (verdict false) and wrongly treat the LOWER actual disable-only
// shadow as active retention. Classifying shadow.PlistFile keeps the disable-only verdict
// CONSISTENT with the shadow reader's own selection BY CONSTRUCTION.
//
// It reads the plist's OWN value directly via the SAME plutil read + JSONC parse as the
// shadow reader mimoCodeReadManagedPrefs (no merge with the managed config dir, no
// func-var value chain), then applies the shared shape gate mimoCodeMapDefinesDisableOnly.
// An empty `plist` (no managed shadow source) → (false, nil). Warn-skip-on-unparseable
// matches the shadow reader's posture (an unconvertible/unparseable plist is treated as
// "no disable-only overlay" — it cannot be PROVEN to carry one); the test seam is
// mimoCodeManagedPrefsDisableOnlyReader.
func mimoCodeReadManagedPlistDisableOnly(plist, name string) (bool, error) {
	if plist == "" {
		return false, nil // no selected managed plist source
	}
	if _, err := os.Stat(plist); err != nil {
		return false, nil // not present — cannot be PROVEN disable-only
	}
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", plist).Output()
	if err != nil {
		return false, nil // plutil convert failed — warn-skip
	}
	m, err := parseJSONCBytes(out)
	if err != nil {
		return false, nil // unparseable converted JSON — warn-skip
	}
	// Apply the disable-only shape gate to the SELECTED plist's own value — no merge,
	// no re-scan.
	return mimoCodeMapDefinesDisableOnly(m, name), nil
}

// mimoCodeReadManagedPrefsEnableOnlyTrue is the enable-ONLY-TRUE-polarity sibling of the
// disable-only managed classifier (bot PR #425 follow-up FINDING 1 — conservative
// cleanup guard). It reports whether the macOS Managed Preferences (MDM) plist carries
// a bare {enabled:true} ENABLE-ONLY overlay for mcp.<name>. It reads the MDM layer's OWN
// value directly via the SAME plist-path resolution + plutil read + JSONC parse as the
// shadow reader mimoCodeReadManagedPrefs (single owner mimoCodeManagedPlistFiles, no merge
// with the managed config dir, no func-var value chain), then applies the shared
// enable-only-true shape gate mimoCodeMapDefinesEnableOnlyTrue on the FIRST plist that
// defines mcp.<name>. (Unlike the FINDING-2 disable-only classifier, this enable-only-TRUE
// over-block guard is consumer-side conservatism, not a shadow-selection agreement, so it
// keeps the first-defining scan — there is no selected-shadow plist to thread here.)
//
// Consumed by mimoCodeManagedEnableOnlyTrueOverlay (the managed half of the two simulate
// consumers' conservative guard): a managed enable-only:true overlay re-activates a lower
// disabled full entry, so the consumers EXCLUDE the candidate (never report it removable
// → no false-cleanup). Off darwin → (false, nil) (mimoCodeManagedPlistFiles returns nil).
// Warn-skip-on-unparseable matches the shadow reader's posture (an unparseable plist
// cannot be PROVEN to carry the overlay — treated as "no enable-only overlay", which here
// means "do not over-block on an unprovable managed plist"); the test seam is
// mimoCodeManagedPrefsEnableOnlyTrueReader.
func mimoCodeReadManagedPrefsEnableOnlyTrue(name string) (bool, error) {
	for _, plist := range mimoCodeManagedPlistFiles() {
		if _, err := os.Stat(plist); err != nil {
			continue // not present — skip (existsSync gate)
		}
		out, err := exec.Command("plutil", "-convert", "json", "-o", "-", plist).Output()
		if err != nil {
			continue // plutil convert failed — warn-skip
		}
		m, err := parseJSONCBytes(out)
		if err != nil {
			continue // unparseable converted JSON — warn-skip
		}
		servers, _ := m[mimoCodeMCPKey].(map[string]any)
		if servers == nil {
			continue
		}
		if _, present := servers[name]; !present {
			continue
		}
		// The FIRST plist that defines mcp.<name> wins (same precedence as the shadow
		// reader). Apply the enable-only-true shape gate to ITS own value — no merge.
		return mimoCodeMapDefinesEnableOnlyTrue(m, name), nil
	}
	return false, nil
}

// mimoCodeManagedConfigDir resolves MiMoCode's MANAGED config DIRECTORY faithfully
// to ConfigManaged.managedConfigDir (managed.ts:23-36, bot PR #420 r17 finding
// P1b). It is a read-only system/admin-deployed layer MiMoCode merges ABOVE the
// hub's write target (config.ts:868-874), below only the macOS MDM plist. Order:
//
//   - MIMOCODE_TEST_MANAGED_CONFIG_DIR (test override) wins when set (managed.ts:35).
//   - win32  → %ProgramData%\opencode (default C:\ProgramData\opencode).
//   - darwin → /Library/Application Support/opencode.
//   - other  → /etc/opencode.
//
// NOTE the directory basename is "opencode", NOT "mimocode" — inherited verbatim
// from the OpenCode upstream (same as the MDM plist DOMAIN "ai.opencode.managed"
// already handled above). Do NOT "correct" it to mimocode.
func mimoCodeManagedConfigDir() string {
	if d := os.Getenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR"); d != "" {
		return d
	}
	switch runtime.GOOS {
	case "windows":
		programData := os.Getenv("ProgramData")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "opencode")
	case "darwin":
		return "/Library/Application Support/opencode"
	default:
		return "/etc/opencode"
	}
}

// mimoCodeManagedConfigDirShadows is the DETECT-ONLY reader for the managed config
// dir layer (bot PR #420 r17 finding P1b). It reports whether mimocode.json or
// mimocode.jsonc in mimoCodeManagedConfigDir() defines a mcp.<name> VALUE that
// SHADOWS the write target (mimoCodeMapShadows — shadow-aware per B5). It NEVER
// writes the managed layer; it only detects so AddEntry/RemoveEntry can fail loud.
//
// PARSE-ERROR POSTURE — WARN-SKIP, matching the MDM plist precedent (bot PR #420
// finding 3): an unparseable managed file on a corporate host must NOT make every
// hub install on that host fail loud for a server the managed layer may not even
// define. A managed shadow is raised ONLY by a SUCCESSFULLY-PARSED managed config
// that actually defines the server. A missing dir / missing file is "no shadow".
// (config.ts:869 existsSync gates the dir; mimocode.json then mimocode.jsonc are
// both merged, .jsonc last = higher within the dir, so it is checked first here.)
func mimoCodeManagedConfigDirShadows(name string) mimoCodeShadowSource {
	dir := mimoCodeManagedConfigDir()
	if dir == "" {
		return mimoCodeShadowSource{}
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return mimoCodeShadowSource{} // absent / not a dir — no shadow (existsSync gate)
	}
	// Highest-within-dir first: mimocode.jsonc wins over mimocode.json.
	for i := len(mimoCodeOverlayLayerNames) - 1; i >= 0; i-- {
		f := filepath.Join(dir, mimoCodeOverlayLayerNames[i])
		data, err := readRawConfig(f)
		if err != nil {
			continue // unreadable managed file — warn-skip, treat as no shadow (MDM precedent)
		}
		if len(data) == 0 {
			continue
		}
		m, err := parseJSONCBytes(data)
		if err != nil {
			continue // unparseable managed file — warn-skip, treat as no shadow
		}
		if mimoCodeMapShadows(m, name) {
			// DISTINCT kind from the macOS MDM plist (bot PR #420 r18 MEDIUM
			// finding): this is an editable managed FILE present on every OS, NOT an
			// MDM profile. File carries the actual offending path so AddEntry /
			// RemoveEntry / scan messages name the real file the operator edits.
			return mimoCodeShadowSource{Kind: "managed-config-dir", Label: "managed config dir", File: f}
		}
	}
	return mimoCodeShadowSource{}
}

// mimoCodeHomeMimocodeDirShadows is the DETECT reader for the HOME ~/.mimocode
// directory layer (bot PR #420 r17 finding P1a). MiMoCode's ConfigPaths.directories
// (paths.ts:36-40) unconditionally loads $HOME/.mimocode/{mimocode.json,
// mimocode.jsonc} in the per-directory loop, which runs ABOVE the global write
// target and the MIMOCODE_CONFIG file (config.ts:760, 773-783) — so a same-named
// entry there shadows the hub write. UNLIKE the project .mimocode dirs (which need
// project/worktree context the single-target hub adapter lacks — see the doc on
// mimoCodeHigherLayerDefining), the HOME .mimocode dir is resolvable from home
// alone (Global.Path.home = HOME || USERPROFILE || os.homedir() = claudeHome), so
// it IS detected. Shadow-aware (mimoCodeMapShadows, B5). A parse error on a present
// file propagates (a malformed home overlay must not be silently treated as "no
// shadow"); a missing dir / missing file is "no shadow".
//
// State-safe: o.claudeHome is "" for direct/test construction and under the scan
// test barrier (MIMOCODE_DISABLE_CLAUDE_CODE_MCP set), so this never reads the
// developer's real ~/.mimocode there — the SAME gate the ~/.claude.json import uses.
func (o *mimoCodeClient) mimoCodeHomeMimocodeDirShadows(name string) (mimoCodeShadowSource, error) {
	if o.claudeHome == "" {
		return mimoCodeShadowSource{}, nil
	}
	dir := filepath.Join(o.claudeHome, ".mimocode")
	// Highest-within-dir first: mimocode.jsonc wins over mimocode.json.
	for i := len(mimoCodeOverlayLayerNames) - 1; i >= 0; i-- {
		f := filepath.Join(dir, mimoCodeOverlayLayerNames[i])
		// Skip a file physically identical to the write target (defensive — the home
		// .mimocode dir is normally distinct from the global config dir, but a
		// symlink / case variant could collide); editing o.path is what takes effect.
		if mimoCodePathsSamePhysical(f, o.path) {
			continue
		}
		shadows, err := mimoCodeFileShadows(f, name)
		if err != nil {
			return mimoCodeShadowSource{}, err
		}
		if shadows {
			return mimoCodeShadowSource{Kind: "file", Label: "home .mimocode dir", File: f}, nil
		}
	}
	return mimoCodeShadowSource{}, nil
}

// mimoCodeHigherLayerDefining walks every READ layer ABOVE the hub's global
// write target (mimocode.json) from HIGHEST precedence DOWN and returns the
// FIRST that defines mcp.<name> — i.e. the layer that actually wins the merge,
// the one the operator must edit to un-shadow. Order (highest→lowest), matching
// the verified config.ts merge precedence:
//
//  1. macOS Managed Preferences (MDM) — read-only, "override everything"
//     1b. managed config dir (%ProgramData%\opencode | /Library/Application Support/
//     opencode | /etc/opencode) — read-only admin/system layer (config.ts:868-874)
//  2. MIMOCODE_CONFIG_CONTENT inline string
//  3. MIMOCODE_CONFIG_DIR overlay: mimocode.jsonc then mimocode.json
//     3b. HOME ~/.mimocode dir: mimocode.jsonc then mimocode.json (config.ts:773-783)
//  4. MIMOCODE_CONFIG file
//  5. global higher layer mimocode.jsonc (sibling of the write target)
//
// SHADOW-AWARE (bot PR #420 r17 finding B5): a layer "shadows" only when its
// mcp.<name> VALUE actually overrides the write target — a FULL redefinition or a
// DISABLING (enabled:false) overlay (mimoCodeValueShadowsWriteTarget). An
// enabled-only:TRUE overlay is NOT a shadow (it overlays just the flag; the lower
// write-target content still supplies the url/command, so the hub write IS
// effective).
//
// config.json and the write-target mimocode.json itself are BELOW the write
// target / are the write target, so they cannot shadow and are not checked. It
// reads ONLY the explicit configFile / overlayDir / inlineContent captured at
// construction, the write target's own dir, the home ~/.mimocode dir (via the
// state-safe claudeHome), and the managed config dir (state-safe — never recomputes
// a project dir from env/home). A parse error on a present operator-editable layer
// propagates (a malformed higher layer must not be silently treated as "no shadow"
// — that would re-introduce the silent-false-success this guards); the read-only
// managed layers (MDM plist, managed config dir) warn-skip an unparseable file
// instead (finding 3 precedent — a broken corporate managed file must not fail
// every install on the host).
//
// DOCUMENTED UN-DETECTABLE SHADOW LAYERS (bot PR #420 r17 findings P1a + P1b).
// MiMoCode loads two more layer classes ABOVE the hub write target that the
// single-target hub adapter CANNOT check, so it does NOT claim to and the
// limitation is documented (not silently treated as "no shadow"):
//
//   - PROJECT layers — project-root mimocode.json(c) found walking up from a run
//     directory to its worktree (config.ts:750-754, paths.ts:12-23) and project
//     `.mimocode/` dirs (config.ts:773-783, paths.ts:25-43). The hub adapter has NO
//     project/worktree context (it is a single-target GLOBAL writer invoked from
//     CLI install/register and the GUI migrate/demigrate routes, none of which carry
//     a project directory), and which project shadows depends on the directory the
//     operator later runs MiMoCode from — an unbounded set. So a project-layer shadow
//     is genuinely un-decidable here. (The HOME ~/.mimocode dir, step 3b, IS
//     resolvable from home alone and therefore IS detected — distinct from project
//     dirs.)
//   - ORG-ACCOUNT config — fetched from <account.url>/api/config when MiMoCode has an
//     active account with an organization (config.ts:832-866). Detecting it would
//     require MiMoCode's private auth/account state (no hub access) AND a per-operation
//     authenticated NETWORK call the hub deliberately does not make. It is itself
//     best-effort upstream (catch-and-continue on fetch failure).
//
// If a hub-registered server does not appear in MiMoCode under a given project or an
// org-managed profile, the operator checks that project's mimocode.json(c) / .mimocode
// dir, or the org console config, for a same-named mcp.<server> override. This is a
// documented limitation, NOT a silent success.
//
// The ~/.claude.json MCP import is deliberately NOT a shadow source here, even
// though config.ts merges it ABOVE everything (887-890): it is SKIP-IF-NAME-EXISTS
// (694-698), so once AddEntry writes mcp.<name> to the write target the import
// skips that name and the hub entry wins the merge. A claude.json entry therefore
// can never shadow a name the hub writes — see the closing note in the body.
// mimoCodeManagedLayerShadows reports whether either MANAGED layer ABOVE the hub
// write target — the macOS Managed Preferences (MDM) plist (step 1) or the
// cross-platform managed config dir (step 1b) — carries a mcp.<name> VALUE that
// SHADOWS the write target. These are the ONLY two layers that
// mimoCodeHigherLayerDefining covers but readLayerFiles / readMergedLayersExcluding
// do NOT fold (they are detect-only, outside the merge-input set — a managed MDM
// plist and the managed config dir are read-only admin/system policy surfaces the
// hub never writes and the file merge never reads). It is the SINGLE owner of the
// managed-layer detection: mimoCodeHigherLayerDefining delegates steps 1+1b here,
// and the re-resolve simulate
// (mimoCodeNameReResolvesAfterWriteTargetRemoval) OR-s it in to cover the managed
// layers the fold misses (bot PR #425 follow-up GAP 1).
//
// Order (highest→lowest), matching mimoCodeHigherLayerDefining's verified
// config.ts precedence:
//
//  1. macOS Managed Preferences (MDM) plist — read-only, "override everything".
//     Routed through the mimoCodeManagedPrefsReader func-var test seam (nil in
//     production → mimoCodeReadManagedPrefs; replaced on non-darwin CI). The reader
//     is SHADOW-AWARE (mimoCodeMapShadows in mimoCodeReadManagedPrefs), so an
//     enabled-only:true managed override is NOT returned as a shadow. A read error
//     propagates.
//     1b. managed config dir (%ProgramData%\opencode | /Library/Application Support/
//     opencode | /etc/opencode | MIMOCODE_TEST_MANAGED_CONFIG_DIR) — read-only
//     admin/system layer, also shadow-aware (mimoCodeMapShadows), warn-skip on an
//     unparseable file.
//
// Returns Kind == "" when neither managed layer shadows. The returned shadow's
// shape is already the mimoCodeValueShadowsWriteTarget verdict (full-redefine or
// disabling) — callers that need the active/inert distinction (the simulate)
// further subtract the disable-only case via mimoCodeShadowIsDisableOnlyOverride.
func (o *mimoCodeClient) mimoCodeManagedLayerShadows(name string) (mimoCodeShadowSource, error) {
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
	// 1b. Managed config dir (bot PR #420 r17 finding P1b) — a read-only
	// system/admin-deployed layer MiMoCode merges directly BELOW the MDM plist and
	// ABOVE everything the hub checks (config.ts:868-874). Detect-only; warn-skip on
	// an unparseable managed file (MDM precedent). %ProgramData%\opencode (win),
	// /Library/Application Support/opencode (darwin), /etc/opencode (linux), or
	// MIMOCODE_TEST_MANAGED_CONFIG_DIR.
	if src := mimoCodeManagedConfigDirShadows(name); src.Kind != "" {
		return src, nil
	}
	return mimoCodeShadowSource{}, nil
}

func (o *mimoCodeClient) mimoCodeHigherLayerDefining(name string) (mimoCodeShadowSource, error) {
	// 1 + 1b. MANAGED layers (MDM plist, managed config dir) — extracted to the
	// single owner mimoCodeManagedLayerShadows and delegated here (bot PR #425
	// follow-up GAP 1: the simulate path OR-s in the SAME helper to cover the
	// managed layers the file fold misses). Behavior is byte-identical to the prior
	// inline steps 1+1b.
	if src, err := o.mimoCodeManagedLayerShadows(name); err != nil {
		return mimoCodeShadowSource{}, err
	} else if src.Kind != "" {
		return src, nil
	}
	// 2. Inline content. SHADOW-AWARE (bot PR #420 r17 finding B5): an
	// enabled-only:true overlay is NOT a shadow (it overlays the flag and lets the
	// lower write-target content win), so use mimoCodeInlineShadows, not bare
	// name-presence.
	if o.inlineContent != "" {
		shadows, err := mimoCodeInlineShadows(o.inlineContent, name)
		if err != nil {
			return mimoCodeShadowSource{}, err
		}
		if shadows {
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
			shadows, err := mimoCodeFileShadows(f, name)
			if err != nil {
				return mimoCodeShadowSource{}, err
			}
			if shadows {
				return mimoCodeShadowSource{Kind: "file", Label: "MIMOCODE_CONFIG_DIR overlay", File: f}, nil
			}
		}
	}
	// 3b. HOME ~/.mimocode dir (bot PR #420 r17 finding P1a) — loaded in the same
	// per-directory loop as the MIMOCODE_CONFIG_DIR overlay (config.ts:773-783), so
	// it shares that precedence band (above the MIMOCODE_CONFIG file). Resolvable
	// from home alone (the project .mimocode dirs are NOT — see the doc); shadow-aware.
	if src, err := o.mimoCodeHomeMimocodeDirShadows(name); err != nil {
		return mimoCodeShadowSource{}, err
	} else if src.Kind != "" {
		return src, nil
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
		shadows, err := mimoCodeFileShadows(o.configFile, name)
		if err != nil {
			return mimoCodeShadowSource{}, err
		}
		if shadows {
			return mimoCodeShadowSource{Kind: "file", Label: "MIMOCODE_CONFIG file", File: o.configFile}, nil
		}
	}
	// 5. global higher layer mimocode.jsonc (sibling of the write target).
	jsonc := filepath.Join(filepath.Dir(o.path), "mimocode.jsonc")
	if !mimoCodePathsSamePhysical(jsonc, o.path) { // never treat the write target itself as a shadow
		shadows, err := mimoCodeFileShadows(jsonc, name)
		if err != nil {
			return mimoCodeShadowSource{}, err
		}
		if shadows {
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

// The write-target stdio-LSP SHAPE gate is mimoCodeWriteTargetDefinesStdioLSP (defined
// next to its plain-stdio sibling mimoCodeWriteTargetDefinesStdio). It reads the write
// target's OWN value via mimoCodeFileEntryValue WITHOUT a disabled-drop so an enabled:false
// write-target entry a higher overlay re-enables is still judged OWNED (bot PR #425
// FINDING 2); name-membership alone (mimoCodeFileDefines) is NOT enough — a name can be
// stdio in a HIGHER layer yet REMOTE in the write target, and RemoveEntry deletes the
// write-target value, so reporting on name alone would wrong-delete the write-target remote
// and leave the higher stdio active.

// mimoCodeFileEntryValue returns the WRITE-target file's OWN parsed mcp.<name>
// map — the value PHYSICALLY present in `path`, independent of the merged
// multi-layer view. Uses the same single-file read+parse idiom as
// mimoCodeFileDefines (readRawConfig + parseJSONCBytes) so the parse is a single owner,
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

// mimoCodeValueIsContentBearing reports whether a mcp.<name> VALUE carries the
// server's own loadable IDENTITY — a `type`/`command`/`url` (the inverse of
// mimoCodeIsEnabledOnlyOverride) — rather than being a bare enabled-only overlay
// ({enabled:<bool>} with no content). A bare {enabled:true} or {enabled:false}
// overlay does NOT supply a URL/command of its own: it field-merges its `enabled`
// onto a LOWER full entry, so the URL/command still comes from below (or the
// ~/.claude.json import). A non-map value (a malformed scalar/array/null) is
// treated as content-bearing — like mimoCodeValueShadowsWriteTarget (2058), a
// non-object value is a full (un-mergeable) value present at the layer, so the
// conservative side is to count it as an own value rather than silently treat it
// as "no content".
//
// This is the SHAPE half of the at/above OWNERSHIP question (bot PR #420 r19
// finding 1): "does a layer at/above the write target OWN this server's URL?" is
// NOT bare name-presence — an enabled-only overlay at/above does not own the
// lower/import URL, so a write target carrying ONLY {enabled:true} over an import
// hub URL must NOT count as at/above (else the scan offers a demigrate the hub
// cannot complete: the URL lives in a layer the hub never wrote). The enabled-only
// semantics stay a SINGLE owner — this reuses mimoCodeIsEnabledOnlyOverride rather
// than re-encoding the type/command/url discriminator.
func mimoCodeValueIsContentBearing(value any) bool {
	entry, ok := value.(map[string]any)
	if !ok {
		// Non-object value (scalar / array / null): a full un-mergeable value
		// present → count as content-bearing (conservative, matches the shadow
		// predicate's non-map polarity).
		return true
	}
	// A map value owns the URL iff it is NOT an enabled-only overlay (i.e. it
	// carries type/command/url). Both enabled:true and enabled:false overlays are
	// enabled-only → not content-bearing → do not own.
	return !mimoCodeIsEnabledOnlyOverride(entry)
}

// mimoCodeMapDefinesContentBearing reports whether a parsed config map carries a
// CONTENT-BEARING mcp.<name> value (mimoCodeValueIsContentBearing) — name present
// AND its value owns the server's URL/command. The shape-aware variant of
// mimoCodeMapDefines (bare presence) for the at/above ownership predicate.
func mimoCodeMapDefinesContentBearing(m map[string]any, name string) bool {
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false
	}
	value, present := servers[name]
	if !present {
		return false
	}
	return mimoCodeValueIsContentBearing(value)
}

// mimoCodeFileDefinesContentBearing reports whether the JSONC file at path defines
// a CONTENT-BEARING mcp.<name> (mimoCodeMapDefinesContentBearing). Same read+parse
// as mimoCodeFileDefines, but a bare enabled-only overlay does NOT count. A
// missing/empty file → false; a parse error on present bytes propagates (a
// malformed layer must not be silently read as "not at/above").
func mimoCodeFileDefinesContentBearing(path, name string) (bool, error) {
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
	return mimoCodeMapDefinesContentBearing(m, name), nil
}

// mimoCodeInlineDefinesContentBearing is the inline-string analog of
// mimoCodeFileDefinesContentBearing for the MIMOCODE_CONFIG_CONTENT top layer. A
// parse error propagates.
func mimoCodeInlineDefinesContentBearing(content, name string) (bool, error) {
	m, err := parseJSONCBytes([]byte(content))
	if err != nil {
		return false, fmt.Errorf("parse MIMOCODE_CONFIG_CONTENT: %w", err)
	}
	return mimoCodeMapDefinesContentBearing(m, name), nil
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
//
// This is the un-excluded merge: a thin delegation to readMergedLayersExcluding("")
// so the merge body has exactly ONE owner (bot PR #425 re-resolve redesign).
func (o *mimoCodeClient) readMergedLayers() (map[string]any, error) {
	return o.readMergedLayersExcluding("")
}

// readMergedLayersExcluding is the single merge body behind readMergedLayers. It
// is byte-identical to the historical readMergedLayers EXCEPT one surgical
// difference: when skipName != "" and the layer being folded is the hub's
// WRITE TARGET (identified by physical equality mimoCodePathsSamePhysical(f,
// o.path) — the same single-owner helper the AddEntry shadow walk uses), it
// deletes skipName from a COPY of that layer's parsed `mcp` map BEFORE folding it
// into the deep-merge. This simulates "what does the merged read look like after
// RemoveEntry(skipName) from the write target" without ever mutating the live
// parsed layer maps and without forking the merge.
//
// WHY PRE-FOLD ON A COPY, NOT POST-MERGE SUBTRACTION (bot PR #425 re-resolve
// redesign, architect-owned): removing the write-target key changes the merge
// INPUTS, not just the output. Subtracting skipName from the FULL merge after the
// fact is wrong on two MiMoCode merge mechanics:
//   - skip-if-name-exists: the ~/.claude.json import only ADDS a name not already
//     defined above. With the write-target key present the import skips it; with
//     the key gone the import RE-EMERGES the entry. Post-merge subtraction would
//     never let the import in.
//   - enabled-only overlay: a higher {enabled:true} overlay overlays its flag onto
//     the LOWER write-target entry (mimoCodeMergeMCPEntry). Once the write-target
//     entry is gone the overlay has no lower entry to overlay onto, so it stays a
//     content-less inert stub. Post-merge subtraction would lose that distinction
//     and keep the overlaid (active) entry visible.
//
// The COPY is mandatory: mimoCodeDeepMerge seeds the first `mcp`-defining layer by
// reference and then mutates dst[mcp] in place, so the parsed write-target layer's
// `mcp` map can be aliased into the merged result — an in-place delete would
// corrupt the live layer and leak into a later readMergedLayers() on the same
// process. Only the per-layer `mcp` sub-map is shallow-copied (one extra map per
// the single write-target layer); the entry VALUES are shared, which is safe
// because the simulate only DELETES a key, never edits a value.
//
// skipName == "" reproduces the historical merge exactly (no layer is altered),
// so readMergedLayers and every existing caller are byte-for-byte unchanged.
// Missing file layers are skipped; a parse error on a present layer (file OR
// inline) propagates; the ~/.claude.json import stays best-effort-swallowed.
func (o *mimoCodeClient) readMergedLayersExcluding(skipName string) (map[string]any, error) {
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
		// WRITE-TARGET key exclusion (the only divergence from the historical
		// body): drop skipName from a COPY of this layer's `mcp` map before the
		// fold, so the deep-merge sees the inputs RemoveEntry(skipName) would
		// leave. Only the write target is altered (it is the sole file the hub's
		// RemoveEntry touches); every other layer folds verbatim.
		//
		// KNOWN LIMITATION — hard-link inode identity (bot PR #425 follow-up
		// FINDING 1/3). The write-target layer is matched by INODE identity
		// (mimoCodePathsSamePhysical → os.SameFile), not by path string, so the
		// exclusion fires for ANY layer file that shares the write target's inode. If
		// an operator DELIBERATELY hard-links a LOWER-layer config.json to the
		// write-target mimocode.json (two distinct MiMoCode global layers pointing at
		// one inode — a non-default manual setup), this simulate models BOTH as losing
		// skipName, so it predicts the lower inode's entry also disappears. PRODUCTION
		// diverges: RemoveEntry writes via atomic temp-file + rename
		// (setMember/deleteMember), which BREAKS the hard link and leaves the lower
		// inode's entry LIVE — so the name actually re-emerges from the still-live
		// lower layer. This simulate is DELIBERATELY left as-is (do NOT "fix" by
		// switching to path-string equality: mimoCodePathsSamePhysical's FOUR
		// shadow-walk callers correctly NEED inode identity — they must treat a path
		// that IS the write target by another name as the write target). Instead the
		// false-removable prediction is BLOCKED at the destructive-consumer level by
		// mimoCodeLowerLayerHardLinkedToWriteTargetDefines (the FINDING-1-corrected
		// true-hard-link detector), called by the register-grain candidate methods AND
		// the workspace-free CLI methods (FindStdioLanguageServerEntries +
		// RemovableStdioEntries). Tracked in
		// work-items/bugs/2026-06-24-mimocode-hardlink-simulate-residual.md.
		if skipName != "" && mimoCodePathsSamePhysical(f, o.path) {
			m = mimoCodeLayerWithoutMCPName(m, skipName)
		}
		merged = mimoCodeDeepMerge(merged, m)
	}
	// MIMOCODE_CONFIG_CONTENT inline layer — a JSONC STRING, not a file path, so
	// it merges AFTER (above) every file layer (bot PR #420 finding 4). Empty
	// when unset OR in the state-safe single-file mode (direct/test construction
	// leaves o.inlineContent empty). A parse error on a present inline string
	// propagates — a malformed MIMOCODE_CONFIG_CONTENT must not be silently
	// dropped (that would re-introduce a silent-wrong-merge). The inline layer is
	// NOT the write target (the hub never writes MIMOCODE_CONFIG_CONTENT), so
	// skipName never excludes it — a name the inline layer defines correctly still
	// re-resolves after a write-target removal.
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
	//
	// When skipName excluded the write-target key, this import is what lets the
	// re-resolve simulate observe the ~/.claude.json RE-EMERGENCE: with the
	// write-target name gone, skip-if-name-exists no longer fires for that name and
	// the import adds it back — exactly the merged read RemoveEntry would produce.
	if err := o.mergeClaudeImport(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// mimoCodeLayerWithoutMCPName returns a layer map equivalent to m but with
// mcp.<name> deleted, WITHOUT mutating m or any map it shares with the live
// parsed config. When m has no `mcp` map, or that map does not define name, m is
// returned unchanged (no copy made — nothing to drop). Otherwise a shallow copy
// of the top-level map and a shallow copy of its `mcp` sub-map are produced, the
// name is deleted from the copied sub-map, and the rest of the layer (entry
// VALUES, other top-level keys) is shared by reference. Sharing the values is
// safe: the re-resolve simulate only removes a key, it never edits an entry.
func mimoCodeLayerWithoutMCPName(m map[string]any, name string) map[string]any {
	servers, ok := m[mimoCodeMCPKey].(map[string]any)
	if !ok {
		return m
	}
	if _, present := servers[name]; !present {
		return m
	}
	serversCopy := make(map[string]any, len(servers))
	for k, v := range servers {
		serversCopy[k] = v
	}
	delete(serversCopy, name)
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	out[mimoCodeMCPKey] = serversCopy
	return out
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
// MiMoCode anchors this on Global.Path.home (config.ts:888 = process.env.HOME ||
// USERPROFILE || os.homedir()), NOT MIMOCODE_HOME, so claudeHome holds that home
// dir as resolved by mimoCodeClaudeImportHome (HOME-first, the B6 fix).
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

// mimoCodeValueShadowsWriteTarget reports whether a higher-layer mcp.<name>
// VALUE actually SHADOWS (overrides the resolved server identity of) the hub's
// lower write-target entry — the precise predicate the AddEntry shadow guard
// needs (bot PR #420 r17 finding B5). Bare name-presence (mimoCodeMapDefines) is
// NOT the right test: MiMoCode merges an enabled-ONLY higher override by OVERLAYING
// just `enabled` onto the lower full entry (mimoCodeMergeMCPEntry /
// mimoCodeDeepMerge), so the lower write-target entry STILL supplies the
// type/command/url — the hub's write IS effective. Only two higher-layer shapes
// truly shadow:
//
//   - a DISABLING enabled-only override (enabled:false) — it overlays enabled:false
//     onto the hub's entry, so the server merges to disabled and the hub's write
//     has no effect (the operator must remove/flip the higher override); AND
//   - a FULL redefinition carrying any of type/command/url — it REPLACES the
//     entry wholesale (replace-by-name), so the hub's write is fully overridden.
//
// An enabled-only override with enabled:TRUE (or any enabled-only shape that is
// not the disabling form) does NOT shadow: it just re-affirms/overlays the flag
// while the lower write-target content wins. A non-map value (a malformed
// scalar/array at mcp.<name>) is treated as a FULL redefinition (it replaces the
// entry and is not an enabled-only overlay) → shadows, fail-loud (the operator
// has an un-mergeable higher value the hub cannot reconcile).
func mimoCodeValueShadowsWriteTarget(value any) bool {
	entry, ok := value.(map[string]any)
	if !ok {
		// Non-object higher value (scalar / array / null). It is not an
		// enabled-only overlay, so the merge replaces the write target wholesale →
		// the hub write would have no effect → shadow.
		return true
	}
	if mimoCodeIsEnabledOnlyOverride(entry) {
		// Enabled-only overlay: shadows ONLY when it disables (enabled:false). An
		// enabled:true overlay overlays the flag and lets the lower write-target
		// content win, so the hub write is effective — NOT a shadow.
		enabled, _ := entry["enabled"].(bool) // bool by mimoCodeIsEnabledOnlyOverride's gate
		return !enabled
	}
	// Full redefinition (carries type/command/url, or any non-enabled-only shape):
	// replace-by-name → the hub write is overridden → shadow.
	return true
}

// mimoCodeFileShadows reports whether the JSONC file at path defines a mcp.<name>
// VALUE that SHADOWS the hub write target (mimoCodeValueShadowsWriteTarget). This
// is the shadow-walk variant of mimoCodeFileDefines (which tests bare name
// presence): the same read+parse, but the shape-aware shadow verdict (bot PR #420
// r17 finding B5). A missing/empty file → false; a parse error on present bytes
// propagates.
func mimoCodeFileShadows(path, name string) (bool, error) {
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
	return mimoCodeMapShadows(m, name), nil
}

// mimoCodeInlineShadows is the inline-string analog of mimoCodeFileShadows for
// the MIMOCODE_CONFIG_CONTENT layer. A parse error propagates.
func mimoCodeInlineShadows(content, name string) (bool, error) {
	m, err := parseJSONCBytes([]byte(content))
	if err != nil {
		return false, fmt.Errorf("parse MIMOCODE_CONFIG_CONTENT: %w", err)
	}
	return mimoCodeMapShadows(m, name), nil
}

// mimoCodeMapShadows reports whether a parsed config map carries a mcp.<name>
// value that SHADOWS the hub write target — name present AND its value shadows
// (mimoCodeValueShadowsWriteTarget). An absent name does not shadow.
func mimoCodeMapShadows(m map[string]any, name string) bool {
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false
	}
	value, present := servers[name]
	if !present {
		return false
	}
	return mimoCodeValueShadowsWriteTarget(value)
}

// mimoCodeMapDefinesDisableOnly reports whether a parsed config map carries a
// mcp.<name> value that is a bare ENABLE-ONLY overlay with enabled==false — name
// present, mimoCodeIsEnabledOnlyOverride (the single-owner enabled-only shape gate),
// AND the flag is false. This is the polarity mimoCodeShadowIsDisableOnlyOverride
// needs for the MANAGED (MDM) case (bot PR #425 follow-up FINDING 1): a disable-only
// MDM overlay DISABLES the server, so it retains NO active server and the entry is
// removable. An absent name, a content-bearing value, or an {enabled:true} overlay
// all return false.
func mimoCodeMapDefinesDisableOnly(m map[string]any, name string) bool {
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false
	}
	value, present := servers[name]
	if !present {
		return false
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if !mimoCodeIsEnabledOnlyOverride(entry) {
		return false
	}
	enabled, _ := entry["enabled"].(bool) // bool by mimoCodeIsEnabledOnlyOverride's gate
	return !enabled
}

// mimoCodeMapDefinesEnableOnlyTrue is the enable-TRUE-polarity sibling of
// mimoCodeMapDefinesDisableOnly: it reports whether a parsed config map carries a
// mcp.<name> value that is a bare ENABLE-ONLY overlay with enabled==true — name
// present, mimoCodeIsEnabledOnlyOverride (the single-owner enabled-only shape gate),
// AND the flag is true. It reuses the SAME shape gate, only flipping the boolean
// polarity, so the enable-only classification stays a single owner.
//
// This is the shape the conservative cleanup guard needs (bot PR #425 follow-up
// FINDING 1): a managed {enabled:true} enable-only overlay re-activates a lower
// disabled full entry through MiMoCode's enabled-overlay merge, so once the
// write-target key is deleted the server stays ACTIVE from the lower layer + the
// managed enable. The simulate consumers therefore CONSERVATIVELY exclude a candidate
// carrying such a managed overlay (no false-cleanup). An absent name, a
// content-bearing value, or an {enabled:false} overlay all return false.
func mimoCodeMapDefinesEnableOnlyTrue(m map[string]any, name string) bool {
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	if servers == nil {
		return false
	}
	value, present := servers[name]
	if !present {
		return false
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if !mimoCodeIsEnabledOnlyOverride(entry) {
		return false
	}
	enabled, _ := entry["enabled"].(bool) // bool by mimoCodeIsEnabledOnlyOverride's gate
	return enabled
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
// os.Stat (FOLLOW symlinks — NOT Lstat) so this fault classifier matches the
// actual reader EXACTLY (bot PR #420 r17 finding B3). The merged read goes
// through readRawConfig → os.ReadFile, which FOLLOWS a symlink to its target,
// so a layer that is a symlink RESOLVING TO A REGULAR FILE reads fine and is NOT
// a fault — Lstat wrongly classified it as non-regular, downgraded the client to
// config-error, and made valid servers vanish from the matrix. The hub never
// writes these layers, so a symlinked-to-regular layer is a legitimate operator
// setup. A layer is a fault ONLY when it FAILS to resolve to a regular file:
//   - os.Stat IsNotExist (absent file OR a DANGLING symlink whose target is
//     missing) → skip; readRawConfig returns (nil, nil) there too — not a fault.
//   - os.Stat other error (permission, I/O fault, symlink → directory which
//     surfaces as "is a directory") → fault; readRawConfig would fail identically.
//   - os.Stat success but !IsRegular (a real directory / FIFO / device, or a
//     symlink resolving to one) → fault; readRawConfig would fail reading it.
func MimoCodeNonRegularActiveLayer(path string) (string, bool) {
	for _, f := range MimoCodeReadLayerPaths(path) {
		st, err := os.Stat(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue // absent file or dangling symlink — readRawConfig returns nil,nil; not a fault
			}
			return f, true // unreadable existing layer — merged read would fail
		}
		if !st.Mode().IsRegular() {
			return f, true // directory / FIFO / device (or a symlink to one) — merged read would fail
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

// MimoCodeNameAtOrAboveWriteTarget reports whether mcp.<name> for the
// scan-supplied config path is defined by a CONTENT-BEARING entry in the hub's
// WRITE target (mimocode.json) OR any READ layer ABOVE it — i.e. in a layer the
// hub's own write could have CLOBBERED or that wins the merge AND that OWNS the
// server's URL/command. It is the SCAN-side, single-owner answer to "is this
// server name hub-OWNABLE?" exported so internal/api/scan.go can label an
// import-inherited hub cell WITHOUT re-deriving the layer model.
//
//   - true  → a CONTENT-BEARING (type/command/url-bearing) definition resolves
//     at/above the write target. The hub can own and demigrate it; classify keeps
//     the cell "via-hub".
//   - false → the name's URL/command resolves ONLY from a layer the hub NEVER
//     writes — the config.json layer strictly BELOW the write target, OR the
//     ~/.claude.json mcpServers import (skip-if-name-exists) — OR the only thing
//     at/above is a bare enabled-only overlay ({enabled:<bool>}, no
//     type/command/url) that field-merges its flag onto that lower/import URL
//     without owning it. The hub cannot demigrate it (RemoveEntry on the hub's
//     own key would leave the import/below URL live); for an http hub-loopback
//     cell scan.go flags ClientEntry.Inherited so classify returns
//     "via-hub-inherited" (read-only) instead of offering a demigrate switch that
//     would always fail closed.
//
// It wraps the single-owner OWNERSHIP predicate mimoCodeOwnedAtOrAboveWriteTarget
// (content-bearing, bot PR #420 r19 finding 1 — an enabled-only stub at/above does
// NOT own the lower/import URL), NOT the bare-presence
// mimoCodeDefinedAtOrAboveWriteTarget that GetEntry uses to stamp the rollback
// source. It runs on the SAME client construction MimoCodeMergedConfig /
// MimoCodeHasClaudeImport use (mimoCodeClientForScanPath), so the env-layer
// resolution and state-safety gate (MIMOCODE_DISABLE_CLAUDE_CODE_MCP +
// global-layer-name check) are identical — a temp/test path stays single-file and
// never reads the developer's real config. A parse error on a present layer
// propagates (a malformed layer must not be silently read as "not at/above");
// scan.go fails open to NOT-inherited on that error (the backend demigrate gate
// still fail-closes, so the worst case is the pre-fix cryptic Apply error, never a
// wrong write).
func MimoCodeNameAtOrAboveWriteTarget(path, name string) (bool, error) {
	return mimoCodeClientForScanPath(path).mimoCodeOwnedAtOrAboveWriteTarget(name)
}

// mimoCodeClientForScanPath builds the read-only client a scan/extract entry
// point uses for a supplied config path, resolving the MIMOCODE_CONFIG file, the
// MIMOCODE_CONFIG_DIR overlay, the MIMOCODE_CONFIG_CONTENT inline content, and the
// ~/.claude.json import home from the live environment — but ONLY when the path is
// a known global layer name (so a temp/test path stays state-safe single-file).
// Single owner of the scan-side path→resolution mapping so the merged-read and the
// layer-path probe never diverge.
//
// claudeHome is the home dir resolved by mimoCodeClaudeImportHome (HOME ||
// USERPROFILE || os.homedir()), matching MiMoCode's Global.Path.home
// (config.ts:888); it lets the scan side see the same ~/.claude.json-imported
// servers the adapter does (bot PR #420 finding 3, r17 finding B6 — HOME-first on
// Windows). A home-resolution failure is non-fatal here (claudeHome stays "" → no
// import), so a scan still works on a host with no resolvable home.
func mimoCodeClientForScanPath(path string) *mimoCodeClient {
	if !mimoCodeIsGlobalLayerName(filepath.Base(path)) {
		// Explicit/temp override — state-safe single-file, no env-resolved layers.
		return &mimoCodeClient{path: path}
	}
	// State-safety: the ~/.claude.json import reads $HOME / $USERPROFILE via
	// mimoCodeClaudeImportHome, an ambient input the MIMOCODE_* env isolation does
	// NOT clear. Gate the home resolution on the SAME flag that disables the import
	// in production (MIMOCODE_DISABLE_CLAUDE_CODE_MCP): when it is set, claudeHome
	// stays "" → no import. isolateMimoCodeEnv sets this flag truthy by default, so
	// a scan/merge through a global-layer-named TEMP path under the standard test
	// barrier never reaches the developer's real ~/.claude.json. Production
	// (flag unset) resolves the home as Global.Path.home (HOME || USERPROFILE ||
	// os.homedir()) — the SAME ~/.claude.json MiMoCode imports, so the scan sees
	// the same servers on a Windows HOME != USERPROFILE host (bot PR #420 r17
	// finding B6). claudeImportEntries re-checks the same flag, so behavior for
	// real operators is unchanged. A home-resolution failure is non-fatal
	// (claudeHome stays "" → no import).
	claudeHome := ""
	if os.Getenv(mimoCodeDisableClaudeImportEnv) == "" {
		claudeHome, _ = mimoCodeClaudeImportHome()
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
	return o.setMemberWithWriter(name, value, nil)
}

func (o *mimoCodeClient) setMemberWithWriter(name string, value any, writer WriteConfigFileFunc) error {
	return mutateJSONObjectMemberWithWriter(o.path, mimoCodeMCPKey, name, value, false, writer)
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
	return o.deleteMemberWithWriter(name, nil)
}

func (o *mimoCodeClient) deleteMemberWithWriter(name string, writer WriteConfigFileFunc) error {
	return mutateJSONObjectMemberWithWriter(o.path, mimoCodeMCPKey, name, nil, true, writer)
}

// AddEntry writes either the operator's verbatim prior entry (rollback restore
// path: entry.Raw != nil — Raw WINS, URL/Headers ignored, so a MiMoCode LOCAL
// entry with a `command` ARRAY round-trips byte-identical) or the hub-managed
// remote-HTTP entry (normal install path: entry.Raw == nil). MiMoCode's remote
// entry shape is `{"type":"remote","url":...,"enabled":true}`; an optional
// `headers` object is emitted when MCPEntry.Headers is non-empty.
func (o *mimoCodeClient) AddEntry(entry MCPEntry) error {
	return o.AddEntryWithConfigWriter(entry, nil)
}

func (o *mimoCodeClient) AddEntryWithConfigWriter(entry MCPEntry, writer WriteConfigFileFunc) error {
	// Raw-restore path FIRST: when GetEntry captured a non-representable prior
	// (a local command-array entry, or any url-less entry), the rollback calls
	// AddEntry(*prior). Write that raw value verbatim so the operator's original
	// is restored exactly — never re-projected onto a lossy {url:""} remote. The
	// rollback restore is exempt from the overlay-shadow guard below: it
	// re-asserts a prior state on the write target (o.path), not a new
	// hub-managed install, and must never be blocked.
	if entry.Raw != nil {
		return o.setMemberWithWriter(entry.Name, entry.Raw, writer)
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
	} else if shadow.Kind == "managed-config-dir" {
		// Cross-platform managed config DIR (bot PR #420 r18 MEDIUM finding) — an
		// editable admin/system-deployed FILE, read-only to the hub. DISTINCT from
		// the MDM error so a Windows/Linux operator sees the real file path, not a
		// "remove your MDM profile" message for a profile that does not exist there.
		return &ErrMimoCodeManagedConfigDirShadowsServer{
			Server:      entry.Name,
			WriteTarget: o.path,
			File:        shadow.File,
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
	return o.setMemberWithWriter(entry.Name, serverEntry, writer)
}

func (o *mimoCodeClient) RemoveEntry(name string) error {
	// Snapshot whether the write target itself physically held mcp.<name> BEFORE
	// the delete (bot PR #420 r17 finding B4). The higher-layer-retention guard
	// below must fire ONLY when the hub actually removed a write-target value the
	// caller believed it owned — not when the delete was a no-op against a name the
	// hub never wrote to the write target (e.g. AddEntry was shadow-refused, so the
	// install never wrote it, and a later rollback RemoveEntry must stay a clean
	// no-op rather than fail on the operator's pre-existing higher-layer entry).
	_, hadOwnValue, ownErr := mimoCodeFileEntryValue(o.path, name)
	if ownErr != nil {
		return ownErr
	}
	// MANAGED-LAYER RETENTION PRE-CHECK (bot PR #425 follow-up — write-then-fail
	// data-loss). deleteMember below MUTATES the write target on disk (o.path); the
	// B4 retention guard further down then fires AFTER the mutation, so for a
	// retained MANAGED layer the file was already edited when the typed error
	// returns — the caller sees an error yet the write target lost the entry. The
	// MANAGED layers (MDM plist, managed config dir) are read-only and cannot be
	// removed by the hub, so a managed retention is decidable BEFORE the delete and
	// must short-circuit it: when the write target physically held the name AND an
	// ACTIVE managed layer (a managed shadow — full-redefine or disabling — minus the
	// inert disable-only stub, read by mimoCodeManagedLayerReResolves from the managed
	// layer's OWN value only) still defines it, return
	// ErrMimoCodeHigherLayerRetainsServer WITHOUT touching the file. SCOPED TO MANAGED
	// ONLY: the file/inline/below-layer and the disable-only-overlay outcomes are
	// decided by the B4 post-delete path below, BYTE-IDENTICAL to before (this
	// pre-check never alters them — it only diverts the managed-retain case, which the
	// post-delete path would have reported anyway, to fire before the mutation rather
	// than after it). mimoCodeManagedLayerReResolves composes the SAME two readers the
	// post-delete B4 guard uses (mimoCodeManagedLayerShadows + the disable-only subtract
	// via mimoCodeShadowIsDisableOnlyOverride), so its verdict CANNOT diverge from the
	// post-delete managed verdict BY CONSTRUCTION — the pre-check's whole point (bot PR
	// #425 follow-up managed-OR simplification, architect GATE REVISE → PATH-B).
	if hadOwnValue {
		managedRetains, err := o.mimoCodeManagedLayerReResolves(name)
		if err != nil {
			return err
		}
		if managedRetains {
			// Active managed retention — refuse BEFORE deleteMember so the write target
			// is left byte-unchanged (no write-then-fail). The operator must edit the
			// managed (read-only) layer named here. Re-read the source for the typed
			// error's Source field (the same layer mimoCodeManagedLayerReResolves just
			// classified active); a reader error here propagates fail-closed.
			managed, err := o.mimoCodeManagedLayerShadows(name)
			if err != nil {
				return err
			}
			return &ErrMimoCodeHigherLayerRetainsServer{
				Server:               name,
				WriteTarget:          o.path,
				Source:               managed,
				WriteTargetUnchanged: true, // pre-check refused BEFORE deleteMember — file untouched (bot #425 r5)
			}
		}
	}
	// Comment-preserving delete on the top write layer only; absence is a no-op.
	if err := o.deleteMember(name); err != nil {
		return err
	}
	if !hadOwnValue {
		// No-op delete: the write target never defined the name, so the hub removed
		// nothing of its own. Any higher-layer definition is the operator's, not a
		// false-success of the hub's removal — return cleanly (idempotent).
		return nil
	}
	// Higher-layer-retention guard (bot PR #420 r17 finding B4). deleteMember
	// touched only the write target (o.path). If the server is ALSO defined in a
	// HIGHER layer the hub cannot remove (MDM/managed, inline, overlay,
	// MIMOCODE_CONFIG, or global mimocode.jsonc), MiMoCode merges that layer on top
	// and keeps loading the server — so the delete "succeeded" yet the entry stays
	// live (a demigrate / uncheck-via-hub would falsely report success). Fail loud
	// naming the shadowing layer the operator must edit. A re-emergence from a layer
	// BELOW the write target (config.json) or from the ~/.claude.json import is the
	// INTENDED rollback behavior (the operator's prior re-emerges) and is excluded
	// by mimoCodeHigherLayerDefining, so only a winning higher layer triggers this.
	//
	// Shadow-aware (post-B5): an enabled-only:true overlay does NOT count (it is not
	// a winning server-defining layer once the write target is gone), so an operator
	// with only such an overlay can still cleanly remove.
	shadow, err := o.mimoCodeHigherLayerDefining(name)
	if err != nil {
		return err
	}
	if shadow.Kind == "" {
		return nil
	}
	// DISABLE-ONLY OVERLAY is NOT a retained ACTIVE server (bot PR #420 r18 P3).
	// mimoCodeHigherLayerDefining reuses the AddEntry shadow predicate, which counts
	// a DISABLING ({enabled:false}) enabled-only overlay as a shadow — correct for
	// AddEntry (the overlay would disable the hub's write). But for THIS post-removal
	// retain question it over-fires: once the write target's real entry is deleted, a
	// bare {enabled:false} stub (no type/command/url) has nothing left to merge onto
	// and MiMoCode loads NOTHING, so the active hub entry WAS successfully removed —
	// failing loud here would falsely report a demigrate/unregister failure. So when
	// the shadowing layer's OWN value is a content-less enabled-only override, the
	// removal succeeded; return cleanly. Any ACTIVE definition — a full redefinition
	// carrying type/command/url, INCLUDING a disabled-FULL {type,url,enabled:false}
	// (which still re-emerges a server-shaped key in a HIGHER layer; consistent with
	// the higher-layer-shadow semantic in
	// mimoCodeNameReResolvesAfterWriteTargetRemoval — see "ENABLED LOWER/IMPORT
	// ENTRIES COUNT; DISABLED ONES DO NOT" there, which leaves the higher-layer
	// branch unchanged), or a non-map scalar — still fails loud naming the layer the
	// operator must edit.
	disableOnly, err := o.mimoCodeShadowIsDisableOnlyOverride(shadow, name)
	if err != nil {
		return err
	}
	if disableOnly {
		return nil
	}
	return &ErrMimoCodeHigherLayerRetainsServer{
		Server:      name,
		WriteTarget: o.path,
		Source:      shadow,
	}
}

// mimoCodeShadowIsDisableOnlyOverride reports whether the SHADOWING layer named by
// `shadow` defines mcp.<name> as a content-less enabled-only override (a bare
// {enabled:<bool>} with no type/command/url — mimoCodeIsEnabledOnlyOverride). Used
// by RemoveEntry's B4 retain check to distinguish a disable-only overlay (which
// retains NO active server once the write target is gone → removal succeeded) from
// a full redefinition (which re-emerges a live server → fail loud). It re-reads the
// SAME single layer the shadow source already identified — never a merged read, so
// the below-layer / claude-import re-emergence semantics (intended rollback
// success) are provably untouched. mimoCodeHigherLayerDefining already excluded the
// enabled-only:TRUE overlay upstream (it never returns a shadow for it), so in
// practice this returns true only for the disable-only ({enabled:false}) stub.
//
// The macOS Managed Preferences (MDM) "managed" kind classifies the SELECTED plist's
// OWN value (mimoCodeReadManagedPlistDisableOnly over shadow.PlistFile, bot PR #425
// FINDING 2 — NOT a fresh re-scan): a disable-only MDM overlay ({enabled:false} with no
// type/command/url) DISABLES the server, so once the write-target key is gone it retains
// NO active server and the removal succeeded — it must NOT keep failing loud. The prior
// FINDING 1 code treated every "managed" kind as NOT disable-only (making RemoveEntry
// refuse to delete a write-target entry a disable-only MDM overlay was actually
// disabling); the FINDING 2 follow-up classifies the SAME plist mimoCodeManagedLayerShadows
// selected (a re-scan that stopped at the first NAME-DEFINING plist could classify a
// HIGHER non-shadowing {enabled:true} plist on a dual-plist host and wrongly treat the
// LOWER disable-only shadow as active retention). mimoCodeManagedLayerReResolves (the RemoveEntry managed
// pre-check) reuses THIS SAME classifier to subtract the disable-only case from a managed
// shadow, so the pre-check and this B4 post-delete guard stay mutually consistent BY
// CONSTRUCTION (one disable-only owner, no divergent merge). A reader error propagates
// fail-closed.
func (o *mimoCodeClient) mimoCodeShadowIsDisableOnlyOverride(shadow mimoCodeShadowSource, name string) (bool, error) {
	switch shadow.Kind {
	case "file", "managed-config-dir":
		// The shadow's File is the offending layer file; read ITS own value only.
		v, ok, err := mimoCodeFileEntryValue(shadow.File, name)
		if err != nil || !ok {
			return false, err
		}
		return mimoCodeIsEnabledOnlyOverride(v), nil
	case "inline":
		// The MIMOCODE_CONFIG_CONTENT inline string is the shadowing layer.
		m, err := parseJSONCBytes([]byte(o.inlineContent))
		if err != nil {
			return false, fmt.Errorf("parse MIMOCODE_CONFIG_CONTENT: %w", err)
		}
		servers, _ := m[mimoCodeMCPKey].(map[string]any)
		v, ok := servers[name].(map[string]any)
		if !ok {
			return false, nil
		}
		return mimoCodeIsEnabledOnlyOverride(v), nil
	case "managed":
		// macOS Managed Preferences (MDM) plist — classify the SELECTED plist's own value
		// (bot PR #425 FINDING 2). The shadow source `shadow` is the plist
		// mimoCodeManagedLayerShadows already CHOSE (the SHADOW-AWARE reader skips a
		// higher non-shadowing {enabled:true} plist and selects the LOWER plist that
		// actually shadows), and shadow.PlistFile pins it. The disable-only classifier
		// MUST agree with THAT selection. A test seam (mimoCodeManagedPrefsDisableOnlyReader),
		// when set, owns the verdict directly (it stands in for the whole MDM read on a
		// non-darwin runner, so there is no real plist to thread). In production the seam
		// is nil and we classify shadow.PlistFile's OWN value directly — NOT a fresh
		// top-of-list re-scan. The prior FINDING-1 code re-scanned from the top and stopped
		// at the FIRST plist that merely DEFINED the name, which on a dual-plist host
		// (per-user {enabled:true} + system disable-only) is the HIGHER non-shadowing
		// per-user plist → it returned false and the LOWER actual disable-only shadow was
		// wrongly treated as active retention (the FINDING 2 regression). Classifying the
		// SELECTED plist makes the disable-only verdict consistent with the shadow reader's
		// own selection BY CONSTRUCTION.
		if reader := mimoCodeManagedPrefsDisableOnlyReader; reader != nil {
			return reader(name)
		}
		return mimoCodeReadManagedPlistDisableOnly(shadow.PlistFile, name)
	default:
		// Any unexpected kind: not classified as disable-only — keep the conservative
		// fail-loud.
		return false, nil
	}
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

// mimoCodeDefinedAtOrAboveWriteTarget reports whether mcp.<name> is defined (bare
// name presence) in the write target (mimocode.json = o.path) OR any READ layer
// ABOVE it (mimocode.jsonc, the MIMOCODE_CONFIG file, the MIMOCODE_CONFIG_DIR
// overlay, or the MIMOCODE_CONFIG_CONTENT inline string) — i.e. in any layer the
// hub's own write could have CLOBBERED, or that wins the merge over the write
// target. It deliberately EXCLUDES the ONLY layer strictly BELOW the write target,
// config.json in the write-target dir: a name present ONLY there is an operator
// entry the hub never wrote (setMember/deleteMember touch o.path alone — see
// deleteMember's doc). GetEntry uses this verdict to STAMP SourceBelowWriteTarget
// (false here = at/above = copy-up-OK; true = below/import = remove-not-copy-up on
// rollback) rather than to suppress the entry, so read-membership still sees a
// below-only name — see GetEntry's source-layer split (bot PR #420 finding 1).
//
// PRESENCE, not ownership. This is the ROLLBACK-SOURCE predicate: it answers "is
// the name PHYSICALLY in the write target or a layer above it" so GetEntry can
// decide copy-up-vs-remove. A bare {enabled:false}/{enabled:true} overlay
// PHYSICALLY in the write target IS at/above here (SourceBelowWriteTarget=false) —
// correct, because GetEntry then snapshots the write target's OWN stub value
// (mimoCodeFileEntryValue / effRaw) for the rollback and never copies a lower
// layer's command/url up; the rollback is moot for a shadowed name anyway (AddEntry
// shadow-refuses before any write). The SCAN-side "is the URL hub-OWNABLE?"
// question is DIFFERENT (an enabled-only stub does not own the lower/import URL) and
// uses the separate content-bearing predicate mimoCodeOwnedAtOrAboveWriteTarget
// (bot PR #420 r19 finding 1) — do NOT conflate the two.
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

// mimoCodeReResolveConsumer selects which destructive consumer's ACTIVE-set
// definition the re-resolve predicate applies after the write-target key is
// simulated away. The register-grain consumers compute their candidate set over the
// SAME merged + disabled-dropped + command-array-normalized view; they differ ONLY
// in the final survivor filter (which entries count as "active membership"):
//   - reResolveConsumerStdio: collectStdioEntries (any non-empty-command stdio
//     entry) — the RemovableStdioEntries set.
//   - reResolveConsumerLSP: findLanguageServerStdioInMap (the stdio entries that
//     ALSO match the mcp-language-server --lsp invocation) — the
//     FindStdioLanguageServerEntries set.
//
// There is no broad "any re-emerges" consumer: the RemoveEntry managed pre-check now
// reads ONLY the managed layer's own value (mimoCodeManagedLayerReResolves, no merge,
// consumer-agnostic), so the coarse content-bearing test is no longer routed through
// this enum (bot PR #425 follow-up managed-OR simplification, architect GATE REVISE →
// PATH-B).
type mimoCodeReResolveConsumer int

const (
	reResolveConsumerStdio mimoCodeReResolveConsumer = iota
	reResolveConsumerLSP
)

// mimoCodeNameInActiveSet reports whether mcp.<name> is present in the consumer's
// ACTIVE membership set computed over the given merged config. It applies the EXACT
// same active-filter pipeline the consumer itself uses — mimoCodeDropDisabled (drop
// enabled:false), then mimoCodeNormalizeCommandArrays (MiMoCode array `command` →
// string command + args), then the consumer's survivor filter (collectStdioEntries for
// the stdio set, findLanguageServerStdioInMap for the LSP set). Routing through the SAME
// owners (no re-derivation) is the whole point of the redesign: the re-resolve decision
// uses the identical computation that decided the name was an active candidate in the
// first place.
func mimoCodeNameInActiveSet(merged map[string]any, name string, consumer mimoCodeReResolveConsumer) bool {
	servers, _ := merged[mimoCodeMCPKey].(map[string]any)
	active := mimoCodeNormalizeCommandArrays(mimoCodeDropDisabled(servers))
	switch consumer {
	case reResolveConsumerLSP:
		for _, e := range findLanguageServerStdioInMap(active) {
			if e.Name == name {
				return true
			}
		}
	default: // reResolveConsumerStdio
		for _, e := range collectStdioEntries(active) {
			if e.Name == name {
				return true
			}
		}
	}
	return false
}

// mimoCodeOwnedAtOrAboveWriteTarget reports whether mcp.<name> is OWNED at/above the
// write target — i.e. whether a CONTENT-BEARING (type/command/url-bearing)
// definition resolves in the write target (mimocode.json = o.path) OR any READ
// layer ABOVE it. It is the SCAN-side "is this server's URL hub-OWNABLE?" predicate
// (bot PR #420 r19 finding 1), DISTINCT from mimoCodeDefinedAtOrAboveWriteTarget
// (bare presence, used by GetEntry to stamp the rollback SOURCE).
//
// OWNERSHIP ≠ PRESENCE. MiMoCode merges a bare enabled-only overlay
// ({enabled:<bool>} with no type/command/url) by FIELD-MERGING just `enabled` onto
// a LOWER full entry, so the URL/command still comes from below (the config.json
// layer) or the ~/.claude.json import. A write target carrying ONLY {enabled:true}
// over an import/below hub URL therefore does NOT own that URL. Counting it as
// at/above (as bare presence does) makes the scan stamp Inherited=false and offer a
// demigrate the hub CANNOT complete — RemoveEntry would clear only the enabled-only
// key, leaving the import/below URL live. So each layer is judged by
// mimoCodeFileDefinesContentBearing / mimoCodeInlineDefinesContentBearing (the
// enabled-only-aware shape gate, single owner mimoCodeIsEnabledOnlyOverride),
// applied to the write target's OWN value too (the write-target {enabled:true} stub
// is the actual bug locus). An enabled-only overlay of EITHER polarity (true or
// false) is not content-bearing → NOT owned (Inherited=true → "via-hub-inherited",
// read-only); a full entry carrying type/command/url IS content-bearing → owned
// (stays "via-hub", demigratable). A non-map value (malformed scalar/array) is
// treated as content-bearing (owned) — the conservative side keeps it demigratable
// (the backend demigrate gate fails closed if wrong) rather than silently flipping
// to read-only.
//
// Layer set, config.json-below exclusion, claude-import exclusion, and parse-error
// propagation are IDENTICAL to mimoCodeDefinedAtOrAboveWriteTarget — only the
// per-layer shape gate (content-bearing vs. bare presence) differs. State-safe by
// the same readLayerFiles single-file mode in the explicit/temp override path.
func (o *mimoCodeClient) mimoCodeOwnedAtOrAboveWriteTarget(name string) (bool, error) {
	lowerLayer := filepath.Clean(filepath.Join(filepath.Dir(o.path), "config.json"))
	for _, f := range o.readLayerFiles() {
		if filepath.Clean(f) == lowerLayer {
			continue // the sole layer strictly below the write target — excluded
		}
		owned, err := mimoCodeFileDefinesContentBearing(f, name)
		if err != nil {
			return false, err
		}
		if owned {
			return true, nil
		}
	}
	// Inline MIMOCODE_CONFIG_CONTENT is a TOP (above) layer, not a file path.
	if o.inlineContent != "" {
		owned, err := mimoCodeInlineDefinesContentBearing(o.inlineContent, name)
		if err != nil {
			return false, err
		}
		if owned {
			return true, nil
		}
	}
	// The ~/.claude.json MCP import is EXCLUDED for the same reason as in
	// mimoCodeDefinedAtOrAboveWriteTarget: it is SKIP-IF-NAME-EXISTS and the hub
	// never writes it, so an import-sourced URL is NOT hub-ownable — a name owned
	// ONLY by the import must report false → "via-hub-inherited" (read-only).
	return false, nil
}

// mimoCodeNameReResolvesAfterWriteTargetRemoval reports whether mcp.<name> would
// STILL be an ACTIVE entry in the merged read after a hypothetical
// RemoveEntry(name) from the WRITE TARGET (mimocode.json = o.path) — i.e. whether
// the name re-emerges as a server MiMoCode actually loads once the hub's
// write-target key is gone.
//
// REDESIGN (bot PR #425 re-resolve, architect-owned). Earlier revisions
// re-derived this BY HAND per layer (mimoCodeHigherLayerDefining +
// mimoCodeFileDefinesEnabled + the claude import), and the codex bot flagged
// three rounds of layer-merge edge cases that piecemeal walk got wrong. The
// asymmetry was the bug factory: the PRE-removal "is it active?" side already used
// the REAL merge (readJSON → readMergedLayers), while only the POST-removal
// predicate re-derived membership by hand. This now re-runs the REAL merge with
// the write target's own mcp.<name> key excluded (readMergedLayersExcluding) and
// tests post-removal ACTIVE membership with the SAME computation that decided the
// name was an active candidate — eliminating the hand-walk entirely.
//
// WHY THIS IS CORRECT ACROSS THE MERGE MECHANICS:
//   - skip-if-name-exists (~/.claude.json import): excluding the write-target key
//     from the merge INPUTS lets the import re-emerge exactly as RemoveEntry would
//     — the import only skipped the name because the write target defined it.
//   - enabled-only overlay: a higher {enabled:true} overlay that re-enabled an
//     enabled:false write-target entry has no lower entry to overlay onto once the
//     key is excluded, so it stays a content-less inert stub — collectStdioEntries
//     (which requires a non-empty command, clients.go) correctly excludes it →
//     does NOT re-resolve → the active direct entry IS removable (the bug this
//     redesign's parent PR #425 closes).
//   - disabled lower / disabled import: dropped by mimoCodeDropDisabled in the
//     active set → does NOT re-resolve → does NOT block removal.
//   - enabled lower / enabled import / full higher redefine: survive the merge as
//     an active entry → re-resolves → blocks removal (correct decline).
//
// PARSE-ERROR PROPAGATION (architect claim 2): readMergedLayersExcluding
// propagates a parse error on any present non-import layer (a malformed
// config.json / mimocode.jsonc must abort, never silently read as "does not
// define"); the ~/.claude.json import keeps its best-effort swallow. On err this
// returns (false, err) so the destructive consumer aborts and deletes nothing.
//
// BRANCH (a) is SEPARATE and UNTOUCHED. This predicate is branch (b) only — the
// cross-layer re-resolve test. The write-target-OWNERSHIP gate
// (mimoCodeWriteTargetDefinesStdio for RemovableStdioEntries,
// mimoCodeWriteTargetDefinesStdioLSP for FindStdioLanguageServerEntries) stays a distinct
// caller-side check that runs BEFORE this. Folding ownership into the simulate
// would break the r12 higher-stdio-over-write-target-remote case (where the
// write-target value is REMOTE but a higher layer is stdio): branch (a) declines
// it on shape so RemoveEntry never wrong-deletes the operator-visible remote.
//
// State-safe: in the explicit/temp single-file mode readLayerFiles collapses to
// the single supplied file, o.inlineContent is empty, and claudeImportEntries
// returns nil when claudeHome is "" — so the simulate never reaches the real
// ~/.config/mimocode or ~/.claude.json.
//
// MANAGED-LAYER OR (bot PR #425 follow-up GAP 1). readMergedLayersExcluding folds
// ONLY the read-layer files (readLayerFiles) + inline content + the ~/.claude.json
// import. The TWO MANAGED layers — the macOS MDM plist and the cross-platform
// managed config dir — are DETECT-ONLY and live OUTSIDE that fold (they are
// read-only admin/system policy surfaces the file merge never reads). The OLD
// hand-walked re-resolve covered them via mimoCodeHigherLayerDefining; the
// merge-based redesign would silently miss a managed-layer re-emergence, leaving a
// managed-shadowed gopls / LSP entry wrongly reported removable. So OR-in the
// SHARED managed detector mimoCodeManagedLayerShadows here, with the SAME
// active/inert semantic the folded layers use:
//
//   - mimoCodeManagedLayerShadows already returns a shadow ONLY for a full-redefine
//     or a DISABLING value (mimoCodeValueShadowsWriteTarget), never for an
//     enabled-only:TRUE overlay — so a managed {enabled:true}-only overlay (inert
//     once the write-target key is gone, the lower content removed) is correctly
//     NOT a re-emergent server and leaves the entry REMOVABLE.
//   - then SUBTRACT the disable-only managed case via
//     mimoCodeShadowIsDisableOnlyOverride: a managed {enabled:false}-only stub has
//     nothing to merge onto once the write-target entry is deleted, so it retains
//     NO active server. This mirrors RemoveEntry's B4 retention semantic exactly
//     (same two owners) — the managed OR means "an ACTIVE server re-emerges".
//
// No double-counting: readLayerFiles' paths (dir(o.path) globals, MIMOCODE_CONFIG,
// the home .mimocode dir, the MIMOCODE_CONFIG_DIR overlay) are DISJOINT from the
// managed paths (the MDM plist dir and mimoCodeManagedConfigDir()), so a name is
// never both folded and managed-OR-ed.
//
// A managed-reader error (MDM plist read) propagates → the destructive consumer
// aborts and deletes nothing, same fail-closed posture as the merge parse error.
func (o *mimoCodeClient) mimoCodeNameReResolvesAfterWriteTargetRemoval(name string, consumer mimoCodeReResolveConsumer) (bool, error) {
	mergedAfter, err := o.readMergedLayersExcluding(name)
	if err != nil {
		return false, err
	}
	if mimoCodeNameInActiveSet(mergedAfter, name, consumer) {
		return true, nil
	}
	// MANAGED-LAYER OR — the two detect-only layers the fold above misses, delegated
	// to the single owner so the managed step has exactly one definition shared by
	// this combined predicate, RemovableStdioEntries, FindStdioLanguageServerEntries,
	// and the RemoveEntry pre-check. The managed verdict is CONSUMER-AGNOSTIC (a managed
	// full-redefine shadows every consumer shape; an enable-only overlay retains
	// nothing on its own regardless of consumer), so it takes no consumer argument —
	// the consumer-shape narrowing lives ENTIRELY on the folded file-survivor half above
	// (bot PR #425 follow-up — managed-OR simplification, architect GATE REVISE → PATH-B).
	return o.mimoCodeManagedLayerReResolves(name)
}

// mimoCodeManagedLayerReResolves reports whether a MANAGED layer (the macOS MDM plist
// or the cross-platform managed config dir) RETAINS an active server for mcp.<name>
// ON ITS OWN after a hypothetical write-target removal — the managed half of the
// post-removal re-resolve question, isolated so it can be applied WITHOUT the
// file-merge survivor. It is the SINGLE owner of the managed-active verdict, consumed
// by:
//   - mimoCodeNameReResolvesAfterWriteTargetRemoval (OR-ed after the folded survivor);
//   - RemovableStdioCandidatesWriteTargetOwned / FindStdioLanguageServerCandidatesWriteTargetOwned
//     — the ONLY managed guard once their stdio/LSP-WIDE file survivor moved CALLER-SIDE
//     to a workspace-scoped recheck (register.go); and
//   - RemoveEntry's managed-only write-then-fail pre-check.
//
// MANAGED-OWN-VALUE-ONLY PREDICATE (bot PR #425 follow-up — architect GATE REVISE →
// PATH-B). The prior revision computed an EFFECTIVE-managed-merge (the managed value
// MERGED over the post-removal lower file merge, then a consumer-shape active test).
// The architect ruled that a WRONG ABSTRACTION: it made the managed verdict depend on
// the below-layer/file merge, creating a TWO-OWNER invariant (this pre-check vs the
// post-delete B4 guard) impossible to hold by hand, and it gave the F3 enable-over-lower
// feature an irreducible conflict with B4's intended below-layer rollback. This
// predicate is therefore deliberately NARROWED to read ONLY the managed layer's OWN
// value — it NEVER reads readMergedLayersExcluding — and is CONSUMER-AGNOSTIC (a managed
// full-redefine shadows every consumer shape; an enable-only overlay retains nothing on
// its own). It composes the SAME two readers the B4 post-delete guard uses, so the
// pre-check and B4 CANNOT diverge BY CONSTRUCTION:
//
//   - mimoCodeManagedLayerShadows(name) — the shadow-shape reader (B4's own owner). It
//     returns Kind != "" ONLY for a managed value that SHADOWS the write target (a full
//     redefine or a DISABLING {enabled:false} overlay); it returns Kind == "" for an
//     enable-only:true overlay (correctly not a shadow).
//   - When a managed shadow exists, subtract the disable-only case via
//     mimoCodeShadowIsDisableOnlyOverride: a disable-only ({enabled:false}) managed
//     overlay DISABLES the server, so once the write-target key is gone it retains NO
//     active server (removable). A full-redefine or disabling-full managed shadow
//     retains an active server (retained). This is F1.
//   - No managed shadow (Kind == ""), including an enable-only:true overlay → the
//     managed layer retains NOTHING on its own (false). If such an overlay re-activates
//     a LOWER survivor, that re-emergence is the FILE-survivor's job — the register.go
//     workspace-scoped recheck for the register grain, and B4's intended-rollback ALLOW
//     for the below-layer config.json case. (Residual F3-over-below-layer +
//     F4 managed-chain: work-items/bugs/2026-06-24-mimocode-managed-enable-over-lower-residual.md.)
//
// The managed layers are DETECT-ONLY (outside readMergedLayersExcluding's fold),
// read-only to the hub, and carry NO workspace — so the managed verdict is
// workspace-blind by construction, which is exactly why it stays in the adapter while
// the workspace-aware file survivor moves to the caller.
//
// Fail-closed: a managed-reader error (the MDM plist read or the disable-only
// classifier) propagates → the destructive consumer aborts and deletes nothing.
func (o *mimoCodeClient) mimoCodeManagedLayerReResolves(name string) (bool, error) {
	shadow, err := o.mimoCodeManagedLayerShadows(name)
	if err != nil {
		return false, err
	}
	if shadow.Kind == "" {
		// No managed shadow (including an enable-only:true overlay, which the
		// shadow-aware reader correctly returns Kind=="" for): the managed layer
		// retains NOTHING on its own. Any re-activation of a lower survivor is the
		// file-survivor's / B4-rollback's job, not the managed verdict's.
		//
		// KNOWN RESIDUAL — a managed {enabled:true}-bare overlay over a content-bearing
		// below-target config.json survivor reports REMOVABLE here, and the server
		// re-emerges from config.json after the delete. That is the INTENDED B4 rollback
		// (no data loss), but an operator might expect the managed enable to PIN it.
		// Architect-ruled ACCEPTABLE RESIDUAL (PATH-B). Tracked in
		// work-items/bugs/2026-06-24-mimocode-managed-enable-over-lower-residual.md.
		return false, nil
	}
	// A managed shadow exists (full-redefine or disabling). Subtract the disable-only
	// case via the B4 guard's OWN classifier so the pre-check and the post-delete B4
	// guard share the identical disable-only verdict: a disable-only managed overlay
	// retains no active server (removable); a full-redefine / disabling-full one retains
	// it (retained).
	disableOnly, err := o.mimoCodeShadowIsDisableOnlyOverride(shadow, name)
	if err != nil {
		return false, err
	}
	return !disableOnly, nil
}

// mimoCodeManagedConfigDirEnableOnlyTrue reports whether the cross-platform MANAGED
// CONFIG DIR layer (%ProgramData%\opencode | /Library/Application Support/opencode |
// /etc/opencode | MIMOCODE_TEST_MANAGED_CONFIG_DIR) carries a bare {enabled:true}
// ENABLE-ONLY overlay for mcp.<name>. It mirrors mimoCodeManagedConfigDirShadows'
// existsSync-gated, highest-within-dir-first read (mimocode.jsonc over mimocode.json)
// and SAME warn-skip-on-unparseable posture, but applies the enable-only-true shape gate
// mimoCodeMapDefinesEnableOnlyTrue to the FIRST managed file that defines mcp.<name>
// instead of the shadow verdict. The FIRST defining managed file wins (its own value, no
// merge). A missing dir / missing file / unparseable file → false.
//
// This is the managed-config-dir half of the conservative cleanup guard's managed
// detector (bot PR #425 follow-up FINDING 1): the simulate consumers exclude a candidate
// when a managed layer's enable-only:true overlay could re-activate a lower disabled full
// entry the write-target delete cannot clear.
func mimoCodeManagedConfigDirEnableOnlyTrue(name string) bool {
	dir := mimoCodeManagedConfigDir()
	if dir == "" {
		return false
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return false // absent / not a dir — no overlay (existsSync gate)
	}
	// Highest-within-dir first: mimocode.jsonc wins over mimocode.json.
	for i := len(mimoCodeOverlayLayerNames) - 1; i >= 0; i-- {
		f := filepath.Join(dir, mimoCodeOverlayLayerNames[i])
		data, err := readRawConfig(f)
		if err != nil {
			continue // unreadable managed file — warn-skip
		}
		if len(data) == 0 {
			continue
		}
		m, err := parseJSONCBytes(data)
		if err != nil {
			continue // unparseable managed file — warn-skip
		}
		servers, _ := m[mimoCodeMCPKey].(map[string]any)
		if servers == nil {
			continue
		}
		if _, present := servers[name]; !present {
			continue
		}
		// The FIRST managed file that defines mcp.<name> wins — apply the enable-only-true
		// shape gate to ITS own value (no merge).
		return mimoCodeMapDefinesEnableOnlyTrue(m, name)
	}
	return false
}

// mimoCodeManagedEnableOnlyTrueOverlay reports whether a MANAGED layer — the macOS
// Managed Preferences (MDM) plist OR the cross-platform managed config dir — carries a
// bare {enabled:true} ENABLE-ONLY overlay for mcp.<name>. It is the FINDING-1 conservative
// cleanup guard's managed detector (bot PR #425 follow-up): the two simulate consumers
// EXCLUDE a candidate when this returns true, because a managed enable-only:true overlay
// re-activates a lower disabled full config.json entry through MiMoCode's enabled-overlay
// merge — so after RemoveEntry deletes the write-target key the server stays ACTIVE from
// the lower layer + the managed enable, but the cleanup would have falsely reported it
// cleared. Excluding it conservatively avoids the false-cleanup; the operator removes the
// redundant entry manually.
//
// DELIBERATELY a single-managed-layer-OWN-value detector — NO value-chain / effective
// merge / readMergedLayersExcluding. It is INDEPENDENT of mimoCodeManagedLayerReResolves
// (the PATH-B managed re-resolve owner shared with the RemoveEntry pre-check + B4 guard),
// which is left untouched: this guard lives ONLY in the two simulate consumers, so the
// pre-check / B4 path keeps its byte-identical PATH-B own-value-only verdict.
//
// OVER-BLOCK is ACCEPTABLE (architect-ruled, bot PR #425 follow-up FINDING 1): a managed
// {enabled:true} overlay with NO lower entry still excludes the candidate even though
// there is nothing to re-activate. That is safe conservatism (no false-cleanup); the
// operator can clean the redundant managed entry. Reuses the same MDM-plist test seam
// idiom (mimoCodeManagedPrefsEnableOnlyTrueReader) as the other managed readers; a reader
// error propagates fail-closed (the destructive consumer aborts and deletes nothing).
func (o *mimoCodeClient) mimoCodeManagedEnableOnlyTrueOverlay(name string) (bool, error) {
	// 1. macOS Managed Preferences (MDM) plist — highest managed layer, read-only.
	reader := mimoCodeManagedPrefsEnableOnlyTrueReader
	if reader == nil {
		reader = mimoCodeReadManagedPrefsEnableOnlyTrue
	}
	mdm, err := reader(name)
	if err != nil {
		return false, err
	}
	if mdm {
		return true, nil
	}
	// 1b. Managed config dir — cross-platform read-only admin/system layer.
	return mimoCodeManagedConfigDirEnableOnlyTrue(name), nil
}

// mimoCodeManagedEnableOnlyReactivatesLowerSurvivor reports whether a managed
// {enabled:true} enable-only overlay would RE-ACTIVATE a lower-layer content-bearing
// entry that SURVIVES a hypothetical write-target removal — the precise false-cleanup
// condition for the workspace-FREE CLI consumers (bot PR #425 FOLLOW-UP-2 FINDING 1).
// It is true iff BOTH:
//
//   - a managed enable-only:true overlay is present (mimoCodeManagedEnableOnlyTrueOverlay),
//     AND
//   - after the write-target key is removed AND the name entry is re-enabled (modeling the
//     managed enable flipping it ON), the FILE merge still defines a survivor OF THE
//     CLEANUP'S OWN CONSUMER SHAPE for mcp.<name> (mimoCodeNameInActiveSet over
//     readMergedLayersExcluding for the given consumer — collectStdioEntries for stdio,
//     findLanguageServerStdioInMap for LSP) — i.e. a lower layer supplies a stdio/LSP command
//     of the cleanup's shape that the managed enable flips back ON.
//
// This is DELIBERATELY NARROWER than the register-grain candidate methods'
// over-blocking mimoCodeManagedEnableOnlyTrueOverlay-only guard. The register grain
// ACCEPTS the over-block (a managed {enabled:true} with NO surviving lower content still
// excludes — architect-ruled acceptable, since its branch (b) is managed-only). The CLI
// consumers use the FULL file survivor (branch (b)), so an unconditional over-block would
// WRONGLY drop a genuinely-removable entry whose content lives ONLY in the write target
// (no lower survivor → nothing for the managed enable to re-activate). That no-lower case
// is PINNED removable by TestMimoCode_Followup_ManagedEnabledOnlyTrueOverlay_StillRemovable
// and TestMimoCode_Followup_ManagedEnableTrue_NoLowerContent_StaysRemovable, so this guard
// fires ONLY when a re-activatable lower survivor actually exists.
//
// CONSUMER SHAPE (bot PR #425 FINDING 3). The lower survivor must match THE CLEANUP'S OWN
// CONSUMER SHAPE, not any content-bearing value. A plain mimoCodeMapDefinesContentBearing
// over the post-removal merge matches a lower REMOTE entry or a DIFFERENT stdio command —
// so for the stdio cleanup over a write-target gopls it would wrongly fire on a disabled
// lower REMOTE survivor (which the managed enable would re-activate as a REMOTE server, NOT
// a stdio one the stdio cleanup is removing), and for the LSP cleanup it would fire on a
// non-mcp-language-server stdio survivor. Deleting the write-target key then leaves NO
// matching direct-stdio / direct-LSP survivor of the cleanup's shape, so the entry WAS
// removable — over-blocking it is a false negative. So the guard threads the `consumer` and
// applies the SAME survivor filter the cleanup uses (collectStdioEntries for stdio via
// reResolveConsumerStdio, findLanguageServerStdioInMap for LSP via reResolveConsumerLSP),
// through the single owner mimoCodeNameInActiveSet — the SAME computation that decided the
// name was a candidate. The guard fires ONLY when the managed enable re-activates a survivor
// OF THE CLEANUP'S OWN SHAPE.
//
// Why the survivor must be re-enabled before the shape test: readMergedLayersExcluding does
// NOT fold the managed layer (managed is detect-only), so it sees the lower entry as
// {command, enabled:false}. The MANAGED {enabled:true} overlay (already confirmed present
// above) is exactly what flips it ON in PRODUCTION, so to model the post-delete reality the
// guard re-enables the name entry (strips its `enabled` flag on a COPY) before applying the
// active-set survivor filter — otherwise mimoCodeDropDisabled / the consumer filters would
// drop the disabled lower entry and the guard could never fire. The re-enable is scoped to
// the SINGLE name on a COPY (mimoCodeReEnableNameOnCopy); the live merged maps are never
// mutated.
//
// Why branch (b) misses this without the guard: branch (b)'s survivor
// (mimoCodeNameInActiveSet over readMergedLayersExcluding) drops the disabled lower entry,
// and the managed OR (mimoCodeManagedLayerReResolves) returns FALSE for an enable-only
// overlay (Kind=="", PATH-B). So branch (b) reports removable while PRODUCTION leaves the
// server active (lower command + managed enable). A readJSON/managed-reader/parse error
// propagates fail-closed (the destructive consumer aborts and deletes nothing).
func (o *mimoCodeClient) mimoCodeManagedEnableOnlyReactivatesLowerSurvivor(name string, consumer mimoCodeReResolveConsumer) (bool, error) {
	overlay, err := o.mimoCodeManagedEnableOnlyTrueOverlay(name)
	if err != nil {
		return false, err
	}
	if !overlay {
		return false, nil
	}
	mergedAfter, err := o.readMergedLayersExcluding(name)
	if err != nil {
		return false, err
	}
	// Re-enable the name entry on a COPY (the managed {enabled:true} overlay confirmed
	// above flips it ON in production) so a disabled lower survivor is not dropped before
	// the consumer-shape filter, then test the cleanup's OWN consumer shape.
	reEnabled := mimoCodeReEnableNameOnCopy(mergedAfter, name)
	return mimoCodeNameInActiveSet(reEnabled, name, consumer), nil
}

// mimoCodeReEnableNameOnCopy returns a view of `merged` in which the mcp.<name> entry has
// its `enabled` key REMOVED (so mimoCodeDropDisabled / the consumer survivor filters treat
// it as enabled — MiMoCode's default) — modeling a managed {enabled:true} overlay flipping a
// lower {enabled:false} entry back ON. It is a SHALLOW, NAME-SCOPED copy: only the top-level
// map, the mcp servers map, and the single `name` entry are copied; every OTHER entry and
// nested value is shared by reference, and the LIVE merged maps are NEVER mutated (the
// no-live-mutation invariant the re-resolve redesign pins). If the name is absent or its
// value is not a map (a malformed scalar/array), `merged` is returned unchanged — the
// downstream consumer-shape filter decides those by its own shape gate.
func mimoCodeReEnableNameOnCopy(merged map[string]any, name string) map[string]any {
	servers, _ := merged[mimoCodeMCPKey].(map[string]any)
	entry, ok := servers[name].(map[string]any)
	if !ok {
		return merged // absent or non-map — nothing to re-enable; let the shape filter decide
	}
	if _, hasEnabled := entry["enabled"]; !hasEnabled {
		return merged // already enabled-by-default — no copy needed
	}
	entryCopy := make(map[string]any, len(entry))
	for k, v := range entry {
		if k == "enabled" {
			continue // drop the flag → enabled by MiMoCode default
		}
		entryCopy[k] = v
	}
	serversCopy := make(map[string]any, len(servers))
	for k, v := range servers {
		serversCopy[k] = v
	}
	serversCopy[name] = entryCopy
	out := make(map[string]any, len(merged))
	for k, v := range merged {
		out[k] = v
	}
	out[mimoCodeMCPKey] = serversCopy
	return out
}

// mimoCodeLowerLayerHardLinkedToWriteTargetDefines reports whether ANY read-layer file
// DISTINCT from the write target by clean-path is a TRUE HARD LINK to the write target
// (a distinct directory entry, a regular file with NO symlink in either chain, sharing
// one inode) AND defines mcp.<name>. It is the FINDING-2 conservative cleanup guard's
// detector (bot PR #425 follow-up): when an operator hard-links a lower-layer config.json
// to the write-target mimocode.json (two distinct global layers over one inode — a
// deliberate non-default setup), the merge-based simulate (readMergedLayersExcluding)
// models BOTH layer copies as losing the name, so it predicts the candidate removable.
// PRODUCTION diverges — deleteMember's atomic temp-file + rename BREAKS the hard link and
// leaves the lower layer's entry LIVE, so the server re-emerges after RemoveEntry yet the
// cleanup falsely reported it cleared. Excluding such a candidate conservatively avoids
// the false-cleanup.
//
// TRUE-HARD-LINK gate via FULL-PATH SYMLINK RESOLUTION (bot PR #425 FINDING 1 — COMPLETE
// version). A naive os.Stat + os.SameFile is WRONG here: os.Stat FOLLOWS symlinks, so a
// SYMLINK (or, on a case-insensitive volume, a case-only alias) pointing AT the write target
// reports the SAME inode and would be mis-classified as a hard link. The earlier correction
// used os.Lstat on BOTH ends, but os.Lstat only inspects the FINAL path component for a
// symlink — it still FOLLOWS symlinks in the PARENT directory components. So a layer that
// reaches the write target THROUGH A SYMLINKED PARENT DIR (e.g.
// MIMOCODE_CONFIG=/dotfiles/mimocode/mimocode.json where /dotfiles/mimocode is a symlink to
// ~/.config/mimocode) presents a REGULAR final file to os.Lstat, and os.SameFile then reports
// the SAME inode — mis-classifying the symlinked-parent ALIAS as a TRUE hard link and wrongly
// BLOCKING an otherwise-removable candidate. Like any symlink alias, such a path FOLLOWS
// deleteMember's temp+rename (after the rename it resolves to the NEW file), so NO old entry
// re-emerges and it must NOT block.
//
// So compare RESOLVED paths: filepath.EvalSymlinks(o.path) and filepath.EvalSymlinks(f)
// resolve EVERY symlink in BOTH chains (parents AND the final component) plus the OS's own
// case folding on a case-insensitive volume.
//
//   - Resolved paths EQUAL (filepath.Clean compare) → they name the SAME directory entry via
//     a symlink ANYWHERE in the path (or a case-fold alias) → an ALIAS, NOT a hard link → skip
//     (the candidate stays removable). This subsumes the earlier Lstat-final + the
//     case-fold-probe checks into one correct full-path-resolution check.
//   - Resolved paths DISTINCT → they are two genuinely-different directory entries; confirm a
//     TRUE hard link via os.SameFile over the RESOLVED regular files (same inode, both
//     regular). Two distinct directory entries over one inode are precisely a hard link, and
//     production's temp+rename breaks it and leaves THIS file live → exclude conservatively.
//   - EvalSymlinks ERROR on either path (non-existent / unreadable) → fail toward
//     NOT-a-hard-link: do NOT block on a resolution error (the candidate stays removable). A
//     genuinely-missing candidate cannot be a live lower layer that re-emerges. As a narrow
//     fallback for the (rare) case where ONLY the candidate fails to resolve yet both raw
//     paths exist as distinct regular entries over one inode, an unresolved os.SameFile over
//     os.Lstat'd FileInfo still catches a real hard link — but the RESOLVED comparison above
//     is the authoritative path.
//
// os.SameFile here is raw inode identity over the RESOLVED files — NOT the protected
// mimoCodePathsSamePhysical, which is needed verbatim by the four shadow-walk callers. A
// parse error while checking name-definition propagates fail-closed (the destructive caller
// aborts).
//
// The clean-path inequality gate excludes the write target's OWN entry in readLayerFiles
// (same path, trivially same inode) so only a DISTINCT entry reaches the resolution compare.
//
// State-safe: in the explicit/temp single-file mode readLayerFiles returns only [o.path]
// (the write target), which the clean-path gate skips, so the loop is a no-op and the
// real ~/.config/mimocode is never reached.
func (o *mimoCodeClient) mimoCodeLowerLayerHardLinkedToWriteTargetDefines(name string) (bool, error) {
	// Lstat the write target (NO symlink follow): a true hard link is between two
	// REGULAR directory entries. If the write target is itself a symlink (or absent),
	// no lower file can be a true hard link to its regular-file inode.
	targetLstat, err := os.Lstat(o.path)
	if err != nil {
		// The write target itself is absent/unreadable — no inode to share, so no
		// hard-linked lower layer can match. (A genuinely missing write target means the
		// candidate could not have come from it anyway.)
		return false, nil
	}
	if !targetLstat.Mode().IsRegular() {
		// The write target is a symlink / non-regular — it is not the hard-link end of a
		// "two regular entries, one inode" pair, so nothing to guard here.
		return false, nil
	}
	cleanTarget := filepath.Clean(o.path)
	// Resolve the write target's FULL path once (every symlink in its chain, parents
	// included, plus OS case folding). An error leaves resolvedTarget == "" so the
	// resolved-equality branch never spuriously matches a candidate (it falls through to
	// the unresolved SameFile fallback instead).
	resolvedTarget := ""
	if r, err := filepath.EvalSymlinks(o.path); err == nil {
		resolvedTarget = filepath.Clean(r)
	}
	for _, f := range o.readLayerFiles() {
		cleanF := filepath.Clean(f)
		if cleanF == cleanTarget {
			continue // the write target's own entry — same path, not a DISTINCT hard link
		}
		// FULL-PATH SYMLINK RESOLUTION (bot PR #425 FINDING 1 — complete version). Resolve
		// the candidate's ENTIRE chain (parents AND final component) and compare to the
		// resolved write target. If they resolve EQUAL the candidate is an ALIAS of the
		// write target (a symlink ANYWHERE in the path — including a symlinked PARENT dir —
		// or a case-fold alias on a case-insensitive volume), which FOLLOWS the temp+rename
		// and leaves NO old entry → NOT a hard link → skip. This single check subsumes the
		// former Lstat-final-component check and the case-fold probe.
		if resolvedF, err := filepath.EvalSymlinks(f); err == nil {
			if resolvedTarget != "" && filepath.Clean(resolvedF) == resolvedTarget {
				continue // resolves to the SAME entry via a symlink/case-fold alias — not a hard link
			}
			// DISTINCT resolved paths: confirm a TRUE hard link by raw inode identity over
			// the RESOLVED regular files. os.Stat follows no further symlinks (the path is
			// already fully resolved); require BOTH to be regular.
			fResolvedStat, ferr := os.Stat(resolvedF)
			tResolvedStat, terr := os.Stat(resolvedTarget)
			if ferr == nil && terr == nil &&
				fResolvedStat.Mode().IsRegular() && tResolvedStat.Mode().IsRegular() &&
				os.SameFile(tResolvedStat, fResolvedStat) {
				defines, err := mimoCodeFileDefines(f, name)
				if err != nil {
					return false, err
				}
				if defines {
					return true, nil
				}
			}
			continue
		}
		// EvalSymlinks failed for the candidate (absent / unreadable / a broken symlink) —
		// fail toward NOT-a-hard-link. As a narrow fallback, an unresolved os.Lstat +
		// os.SameFile still catches a real hard link between two existing distinct regular
		// directory entries (no symlink in either chain, so EvalSymlinks would have
		// succeeded — meaning this fallback only runs when the candidate genuinely could
		// not be resolved, e.g. a transient read error). A missing candidate Lstat-errs and
		// is skipped.
		fLstat, err := os.Lstat(f)
		if err != nil {
			continue // absent / unreadable lower layer — cannot re-emerge live
		}
		if !fLstat.Mode().IsRegular() {
			continue // a symlink / non-regular — follows the rename, not a hard link
		}
		if !os.SameFile(targetLstat, fLstat) {
			continue // distinct inode — not hard-linked to the write target
		}
		defines, err := mimoCodeFileDefines(f, name)
		if err != nil {
			return false, err
		}
		if defines {
			return true, nil
		}
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
// since AddEntry shadow-refuses the install before any write). The 3 rollback
// sites (install.go, register.go, register_supervisor.go) route a
// SourceBelowWriteTarget prior to RemoveEntry instead of AddEntry, preserving the
// no-credential-copy-up security invariant. Every OTHER GetEntry caller ignores
// the field; no other adapter sets it.
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
	// Disabled verdict (bot PR #420 finding 5; r18 P2). Computed from the MERGED
	// view (`raw`), NOT effRaw — the two answer DIFFERENT questions. effRaw is the
	// write-target's OWN physical value, snapshotted ONLY to keep the rollback Raw
	// from copying a lower/higher layer's command/url up (the no-copy-up invariant).
	// Disabled, by contrast, feeds api.GatedOnClients, which must reflect whether
	// MiMoCode actually LOADS the server — i.e. the merged-EFFECTIVE `enabled`. The
	// merge overlays an enabled-only:TRUE higher override onto the write target
	// (mimoCodeMergeMCPEntry), so a write target carrying {enabled:false} under a
	// {"enabled":true} overlay merges to enabled:true and IS loaded — yet effRaw
	// (=ownRaw, since B5 returns higher.Kind=="" for an enabled-only:true overlay,
	// so the effRaw=ownRaw branch above fires) still reads enabled:false. Computing
	// Disabled from effRaw there would stamp Disabled:true on a LIVE `mcphub-hub`
	// aggregate, making GatedOnClients skip it so `mcphub gui --reset-port` could
	// orphan its URL — the exact footgun the Disabled flag exists to prevent. So
	// take Disabled from `raw` (merged-effective). The Raw/url SHAPE branches below
	// still use effRaw (no-copy-up unchanged); only this scalar reflects the merge.
	// A disabling higher overlay (enabled:false) over an enabled write target merges
	// `raw` to enabled:false → Disabled:true, also correct. Mirrors the scan path
	// (shapeMimoCodeEntry classifies enabled:false as Transport "absent").
	// Projection is single-owned in mimoCodeProjectEntry (shared with the CAS
	// write-target compare, casWriteTargetEntry). Disabled comes from the MERGED
	// `raw` (per the comment above); the url/Raw/Headers SHAPE comes from `effRaw`
	// (no-copy-up). Behaviour-identical to the prior inline tail.
	return mimoCodeProjectEntry(name, raw, effRaw, below), nil
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

// mimoCodeProjectEntry is the SINGLE owner of the mimocode value-map → *MCPEntry
// projection GetEntry returns. mergedRaw supplies the merged-effective Disabled
// scalar (feeds api.GatedOnClients); effRaw supplies the url/Raw/Headers SHAPE
// decision (the rollback-Raw no-copy-up value). Factored out of GetEntry so the
// CAS write-target compare (casWriteTargetEntry) builds a byte-identically-shaped
// entry — the injected recognizer must run against an entry constructed the SAME
// way GetEntry constructs one (fable-5 Phase-3 P2). Behaviour-identical to the
// prior inline GetEntry tail.
func mimoCodeProjectEntry(name string, mergedRaw, effRaw map[string]any, below bool) *MCPEntry {
	disabled := mimoCodeEntryDisabled(mergedRaw)
	url, _ := effRaw["url"].(string)
	if url == "" {
		// URL-less local entry (or a url-less remote) — not representable as a
		// URL MCPEntry. Carry the verbatim value in Raw so rollback restores it
		// exactly rather than deleting it or corrupting it to {type:remote,url:""}.
		return &MCPEntry{Name: name, Raw: effRaw, SourceBelowWriteTarget: below, Disabled: disabled}
	}
	// A DISABLED URL entry (enabled:false) is also not faithfully representable
	// as a {URL,Headers} MCPEntry: AddEntry's normal install path hardcodes
	// enabled:true, so a GetEntry→AddEntry(*prior) rollback would silently
	// RE-ENABLE a server the operator had disabled (bot PR #420 finding 5).
	// Carry the verbatim entry in Raw so the rollback writes it back byte-shaped
	// (Raw wins in AddEntry), preserving enabled:false.
	if disabled {
		return &MCPEntry{Name: name, Raw: effRaw, SourceBelowWriteTarget: below, Disabled: true}
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
		return &MCPEntry{Name: name, Raw: effRaw, SourceBelowWriteTarget: below, Disabled: disabled}
	}
	return &MCPEntry{Name: name, URL: url, Headers: extractHeaders(effRaw, "headers"), SourceBelowWriteTarget: below, Disabled: disabled}
}

func (o *mimoCodeClient) LatestBackupPath() (string, bool, error) {
	return latestBackup(o.path, o.Name())
}

func (o *mimoCodeClient) RestoreEntryFromBackup(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface doc
// on Client.RestoreEntryFromBackupForRollback). Install rollback and Serena
// migrate rollback use it when the timestamped backup is the source of truth.
func (o *mimoCodeClient) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return o.restoreEntryFromBackup(backupPath, name, true)
}

func (o *mimoCodeClient) RestoreEntryFromBackupForRollbackWithConfigWriter(backupPath, name string, writer WriteConfigFileFunc) error {
	return o.restoreEntryFromBackupWithWriter(backupPath, name, true, writer)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a backup entry already in hub-HTTP shape (a hub
// loopback URL under `url` with no `command`) with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes the
// backup bytes verbatim regardless of shape. The restore/delete both touch the
// top write layer only (setMember/deleteMember on o.path), so a lower-layer
// operator original is preserved and re-emerges via the merge.
func (o *mimoCodeClient) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	return o.restoreEntryFromBackupWithWriter(backupPath, name, allowHubEntry, nil)
}

func (o *mimoCodeClient) restoreEntryFromBackupWithWriter(backupPath, name string, allowHubEntry bool, writer WriteConfigFileFunc) error {
	// os.ReadFile (NOT readRawConfig): a named backup that is missing is a
	// genuine read error the demigrate caller must see, not a silent
	// treat-as-empty. Empty / comment-only / malformed bytes are then
	// classified by parseJSONCBytes (empty map vs parse error).
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	// Re-attach the backup file path to any error the path-free core returns, so
	// the file-backed demigrate/rollback caller keeps a "which backup failed"
	// diagnostic (the core stays path-free for Phase 3's in-memory snapshot
	// bytes). %w preserves ErrBackupEntryAlreadyMigrated for errors.Is callers.
	if err := o.restoreEntryFromBytes(backupData, name, allowHubEntry, writer); err != nil {
		return fmt.Errorf("restore backup %s: %w", backupPath, err)
	}
	return nil
}

// restoreEntryFromBytes is the post-ReadFile restore core: given the
// already-read backup bytes it parses them, reads the live config, and
// surgically restores (or strips) the named entry. The restore/delete both
// touch the top write layer only (setMember/deleteMember on o.path), so a
// lower-layer operator original is preserved and re-emerges via the merge.
// restoreEntryFromBackupWithWriter is the thin file-reading wrapper over this
// core. The parse error omits the source path because this core also serves
// callers that pass in-memory bytes with no backing file.
func (o *mimoCodeClient) restoreEntryFromBytes(backupData []byte, name string, allowHubEntry bool, writer WriteConfigFileFunc) error {
	backupMap, err := parseJSONCBytes(backupData)
	if err != nil {
		return fmt.Errorf("parse backup: %w", err)
	}
	backupServers, _ := backupMap[mimoCodeMCPKey].(map[string]any)
	// Rollback-only atomic entry-scoped skip-if-unchanged (design round-4): compare
	// against the TOP WRITE-TARGET layer (o.path) — NOT the merged readJSON() view,
	// which folds lower operator layers the hub never writes and would misjudge
	// presence/value. readRawConfig returns empty on ENOENT so a removed top file
	// reads livePresent=false. A read/parse error FALLS THROUGH to the existing
	// restore (no new failure mode). Gated on allowHubEntry so demigrate keeps its
	// ErrBackupEntryAlreadyMigrated guard. The read + the setMember/deleteMember
	// write below run under the SAME withConfigLock hold ⇒ no TOCTOU.
	if allowHubEntry {
		// Whole-file-gone recovery FIRST (design round-5): SecureWrite path #1 may have
		// REMOVED the TOP write-target file o.path (target entry + siblings). Recover the
		// whole backup (a snapshot of o.path — see BackupKeep) to o.path before the
		// entry-scoped skip, which would else false-skip the both-absent case or
		// surgically recreate only the target entry (losing siblings).
		if handled, werr := wholeFileRestoreIfWriteTargetGone(o.path, backupData); handled {
			return werr
		}
		if rawLive, rerr := readRawConfig(o.path); rerr == nil {
			if liveMap, perr := parseJSONCBytes(rawLive); perr == nil {
				liveServers, _ := liveMap[mimoCodeMCPKey].(map[string]any)
				le, lp := liveServers[name]
				be, bp := backupServers[name]
				if entryRestoreIsNoop(le, lp, be, bp) {
					return nil
				}
			}
		}
	}
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
			return o.setMemberWithWriter(name, backupEntry, writer)
		}
	}
	return o.deleteMemberWithWriter(name, writer)
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
// enabled:false entries are dropped FIRST (bot PR #420 r18 P3): MiMoCode uses
// `enabled` (default true) as the active flag and never spawns a disabled entry,
// so neither consumer of this method should treat a disabled entry as live. The
// post-register gopls-mcp cleanup (register.go) backs up + RemoveEntry-s every
// matching stdio entry — deleting a user-authored DISABLED `gopls mcp` entry the
// operator deliberately turned off (and MiMoCode would not load) is wrong; and
// the orphan-process kill-pattern derivation (cleanup.go) need not derive a
// pattern for a server MiMoCode never spawns. This mirrors
// FindStdioLanguageServerEntries' and the scan path's disabled handling
// (shapeMimoCodeEntry classifies enabled:false as absent), keeping the cleanup
// view consistent with what MiMoCode actually loads. Disabled-drop is orthogonal
// to layer scoping (it only removes inert entries): the full merged view across
// layers is otherwise preserved, so a real lower-layer orphan source is still
// surfaced.
func (o *mimoCodeClient) AllStdioEntries() ([]StdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	return collectStdioEntries(mimoCodeNormalizeCommandArrays(mimoCodeDropDisabled(servers))), nil
}

// RemovableStdioEntries returns the subset of MiMoCode stdio entries that a
// single RemoveEntry call can actually remove from the EFFECTIVE configuration.
// Unlike AllStdioEntries (which intentionally returns the full merged view for
// non-destructive orphan-process pattern discovery), this method is the
// destructive-safe view for WORKSPACE-FREE consumers that call RemoveEntry per
// entry and report success (the WORKSPACE-AWARE register cleanup uses the narrower
// RemovableStdioCandidatesWriteTargetOwned instead — see below).
//
// MiMoCode RemoveEntry deletes only the hub's write target (o.path,
// mimocode.json). Entries defined in config.json, mimocode.jsonc,
// MIMOCODE_CONFIG, MIMOCODE_CONFIG_DIR, inline content, home .mimocode, or the
// Claude import can remain effective after the write-target key is deleted. So
// the cleanup view must be constrained to entries whose OWN write-target value is
// stdio AND whose name would not re-emerge as ACTIVE from a layer the hub cannot
// remove. This mirrors FindStdioLanguageServerEntries' destructive-cleanup
// scoping, but keeps the broader AllStdioEntries contract unchanged for
// non-destructive callers.
//
// WORKSPACE-FREE CONSERVATIVE SURVIVOR (architect REVISE → Option C). This method
// is the candidate source for WORKSPACE-FREE destructive consumers — it keeps the
// FULL cross-layer survivor (branch (b) = mimoCodeNameReResolvesAfterWriteTargetRemoval,
// which under CHANGE 1 OR-s in the managed-layer coverage). Any same-name ACTIVE
// re-emergence from a FILE / inline / import / managed layer DECLINES the entry, so
// a workspace-blind caller (e.g. `mcphub language-server cleanup` via
// FindStdioLanguageServerEntries' sibling) never wrong-deletes an entry that
// re-emerges. The WORKSPACE-SCOPED destructive gopls/LSP register cleanup does NOT
// consume this method — it consumes the narrower
// RemovableStdioCandidatesWriteTargetOwned (branch (a) + managed-only) and applies
// its own workspace-scoped survivor caller-side (register.go), so a same-name
// DIFFERENT-workspace re-emergence does not block removal of the real entry.
//
// EFFECTIVE-enabled, not write-target-enabled (bot PR #425 finding 2). The
// candidate set is the MERGED stdio view (collectStdioEntries over the
// disabled-dropped merged servers — the exact AllStdioEntries computation), NOT
// the disabled-dropped WRITE-TARGET servers. The write-target's own `enabled`
// flag is the wrong gate: MiMoCode's merge lets a HIGHER enabled-only:true
// overlay RE-ENABLE a write-target entry whose own value is enabled:false. In
// that config the effective server is an ACTIVE stdio gopls (the write target
// supplies the command, the higher overlay flips it on), and deleting the
// write-target key DOES clear it (the higher enabled-only stub goes inert with no
// command left to supply) — so it IS removable. Building candidates from the
// disabled-dropped write target (the prior code) dropped that entry BEFORE the
// re-resolve check, leaving the active direct gopls behind register cleanup →
// duplicate LSP processes. Using the merged effective view captures it, and
// mimoCodeWriteTargetDefinesStdio below re-imposes the write-target ownership
// constraint so a higher-stdio-over-write-target-remote name is still excluded.
func (o *mimoCodeClient) RemovableStdioEntries() ([]StdioEntry, error) {
	merged, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := merged[mimoCodeMCPKey].(map[string]any)
	// EFFECTIVE stdio view (same computation as AllStdioEntries): the disabled
	// drop here is over the MERGED value, so an entry the higher overlay
	// re-enabled survives, and one disabled with no re-enabling overlay does not.
	entries := collectStdioEntries(mimoCodeNormalizeCommandArrays(mimoCodeDropDisabled(servers)))
	out := entries[:0]
	for _, e := range entries {
		// (a) The write target's OWN value must itself be stdio shape, so
		// RemoveEntry (which deletes only the write-target key) can actually clear
		// it. Excludes lower-layer-only entries and the higher-stdio-over-
		// write-target-remote case (RemoveEntry would delete the write-target
		// remote and leave the higher stdio active — the r12 HIGH scenario
		// FindStdioLanguageServerEntries guards, applied here for plain stdio).
		ownsStdio, err := o.mimoCodeWriteTargetDefinesStdio(e.Name)
		if err != nil {
			return nil, err
		}
		if !ownsStdio {
			continue
		}
		// (b) The name must not re-resolve as an ACTIVE entry from another layer
		// after the write-target key is removed — the FULL cross-layer survivor
		// (file / inline / import / managed via the CHANGE-1 managed OR). An
		// enabled-only:true overlay re-enabling an enabled:false write-target entry
		// goes inert once the key is gone (no command left), so it does NOT
		// re-resolve and the entry is reported removable (bot PR #425 finding 2).
		reResolves, err := o.mimoCodeNameReResolvesAfterWriteTargetRemoval(e.Name, reResolveConsumerStdio)
		if err != nil {
			return nil, err
		}
		if reResolves {
			continue
		}
		// (c) CONSERVATIVE cleanup guards (bot PR #425 follow-up FINDINGS 1+2). The
		// workspace-FREE CLI consumer rests on branch (b)'s merge-based survivor
		// (mimoCodeNameReResolvesAfterWriteTargetRemoval), which the simulate consumers'
		// two SIMULATE-ONLY guards also backstop — kept identical here so the CLI path
		// (`mcphub language-server cleanup`) is SYMMETRIC with the register-grain candidate
		// methods. They are NOT in the RemoveEntry pre-check / B4 path (which keep their
		// byte-identical PATH-B verdict). Each EXCLUDES a candidate branch (b) would
		// otherwise falsely report removable:
		//
		//   FINDING 1 — a managed {enabled:true} enable-only overlay re-activates a lower
		//   disabled full config.json entry through MiMoCode's enabled-overlay merge, so
		//   after the write-target delete the server stays ACTIVE. Branch (b)'s survivor
		//   (mimoCodeNameInActiveSet over readMergedLayersExcluding) drops the disabled
		//   lower entry, and the managed OR's mimoCodeManagedLayerReResolves returns FALSE
		//   for an enable-only overlay (Kind=="" — PATH-B), so branch (b) reports FALSE and
		//   the entry would be wrongly reported removable. The CONDITIONED detector excludes
		//   it ONLY when a re-activatable lower content-bearing survivor actually exists —
		//   NARROWER than the register grain's over-blocking guard, because the CLI's FULL
		//   file survivor (branch (b)) means an unconditional over-block would wrongly drop a
		//   genuinely-removable entry whose content lives ONLY in the write target (the
		//   no-lower case, pinned removable by the CLAIM-2 followup tests). The guard applies
		//   THIS consumer's stdio survivor shape (bot PR #425 FINDING 3) so a disabled lower
		//   REMOTE / non-stdio survivor does not falsely exclude a removable stdio entry.
		managedEnableReactivates, err := o.mimoCodeManagedEnableOnlyReactivatesLowerSurvivor(e.Name, reResolveConsumerStdio)
		if err != nil {
			return nil, err
		}
		if managedEnableReactivates {
			continue
		}
		//   FINDING 2 — a lower-layer file HARD-LINKED to the write target re-emerges live
		//   after RemoveEntry (temp+rename breaks the link), but the merge-based simulate
		//   (readMergedLayersExcluding) matches the write target by INODE so it modeled both
		//   copies as losing the name → reResolves reads FALSE and the entry looks
		//   removable. The SAME FINDING-1-corrected detector excludes it (a SYMLINK /
		//   case-alias follows the rename and is NOT blocked).
		hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines(e.Name)
		if err != nil {
			return nil, err
		}
		if hardLinked {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// RemovableStdioCandidatesWriteTargetOwned is the WORKSPACE-AWARE register-grain
// candidate source for the destructive direct-gopls cleanup (architect REVISE →
// Option C, bot PR #425 follow-up GAP 2). It is RemovableStdioEntries with branch
// (b) reduced to the MANAGED-only re-resolve: it keeps branch (a)
// (mimoCodeWriteTargetDefinesStdio, the write-target ownership gate) and the
// MANAGED-layer guard (mimoCodeManagedLayerReResolves — the workspace-blind,
// read-only MDM plist + managed config dir that readMergedLayersExcluding never
// folds), but OMITS the FILE/inline/import-WIDE survivor.
//
// That file survivor is too coarse for the gopls grain: a same-name lower-layer
// DIFFERENT stdio (npx / a different LSP language / gopls for a DIFFERENT
// workspace) would wrongly block removal of the real gopls. So it MOVES CALLER-SIDE
// to a WORKSPACE-SCOPED recheck (register.go matchingDirectGoplsMCPEntries via
// ActiveStdioEntriesExcludingWriteTarget), where the workspace identity lives. A
// managed-active re-emergence still excludes the candidate HERE (workspace-blind,
// read-only — the hub cannot clear it regardless of workspace).
//
// This method is consumed ONLY by the workspace-aware register grain. The
// workspace-FREE consumers keep the conservative full-survivor RemovableStdioEntries
// above, so they never wrong-delete a file/import re-emergence.
func (o *mimoCodeClient) RemovableStdioCandidatesWriteTargetOwned() ([]StdioEntry, error) {
	merged, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := merged[mimoCodeMCPKey].(map[string]any)
	entries := collectStdioEntries(mimoCodeNormalizeCommandArrays(mimoCodeDropDisabled(servers)))
	out := entries[:0]
	for _, e := range entries {
		// (a) write-target ownership gate — unchanged from RemovableStdioEntries.
		ownsStdio, err := o.mimoCodeWriteTargetDefinesStdio(e.Name)
		if err != nil {
			return nil, err
		}
		if !ownsStdio {
			continue
		}
		// (b) MANAGED-only guard — the file/inline/import survivor is the caller's
		// workspace-scoped job (register.go), so only the workspace-blind managed
		// retention (a managed full-redefine / disabling shadow, minus the disable-only
		// case) excludes a candidate here. A managed enable-only overlay retains nothing
		// on its own (Kind==""), so it never excludes the candidate; any re-activation of
		// a lower survivor is the caller's workspace-scoped recheck's job.
		managedRetains, err := o.mimoCodeManagedLayerReResolves(e.Name)
		if err != nil {
			return nil, err
		}
		if managedRetains {
			continue
		}
		// (c) CONSERVATIVE cleanup guards (bot PR #425 follow-up FINDINGS 1+2). These are
		// SIMULATE-CONSUMER-ONLY blocks (NOT in the RemoveEntry pre-check / B4 path, which
		// keep their byte-identical PATH-B verdict). They EXCLUDE a candidate the simulate
		// would otherwise falsely report removable:
		//
		//   FINDING 1 — a managed {enabled:true} enable-only overlay re-activates a lower
		//   disabled full config.json entry through MiMoCode's enabled-overlay merge, so
		//   after the write-target delete the server stays ACTIVE. mimoCodeManagedLayer
		//   ReResolves returns FALSE for an enable-only overlay (Kind=="" — PATH-B), so it
		//   does NOT cover this; the dedicated own-value-only detector does. Over-blocking
		//   a managed {enabled:true} with no lower entry is ACCEPTABLE conservatism.
		managedEnableOnly, err := o.mimoCodeManagedEnableOnlyTrueOverlay(e.Name)
		if err != nil {
			return nil, err
		}
		if managedEnableOnly {
			continue
		}
		//   FINDING 2 — a lower-layer file HARD-LINKED to the write target re-emerges live
		//   after RemoveEntry (temp+rename breaks the link), but the merge-based simulate
		//   modeled both copies as losing the name. Exclude when a distinct hard-linked
		//   lower layer defines the candidate.
		hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines(e.Name)
		if err != nil {
			return nil, err
		}
		if hardLinked {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// mimoCodeWriteTargetEnableOnlyActivatesEffectiveStdio reports whether the write
// target's OWN value for mcp.<name> is a bare {enabled:true} ENABLE-ONLY overlay that
// EFFECTIVELY ACTIVATES a lower-layer stdio command of the consumer's shape (bot PR #425
// FOLLOW-UP-2 FINDING 3). It is the effective-ownership sibling of the own-value SHAPE
// gates: the write target carries no command of its OWN, but MiMoCode's enabled-overlay
// merge (mimoCodeMergeMCPEntry) overlays the write-target {enabled:true} onto a lower full
// entry (config.json {command, enabled:false}), so the MERGED effective entry is an ACTIVE
// stdio/LSP server — and deleting the write-target key re-DISABLES it (the lower
// enabled:false takes over). The write-target enable is therefore what ACTIVATES the
// server, so the hub OWNS the activation and RemoveEntry can clean it.
//
// CRITICAL distinction from the FINDING 1 MANAGED enable-only guard
// (mimoCodeManagedEnableOnlyTrueOverlay, which EXCLUDES a candidate): there the
// {enabled:true} lives in a MANAGED layer the hub CANNOT write/delete, so the activation
// survives RemoveEntry and the candidate must be conservatively blocked. HERE the
// {enabled:true} is the WRITE TARGET's OWN value, which RemoveEntry deletes — so it is
// REMOVABLE. Both verdicts are consistent: write-target-enable = owned/removable;
// managed-enable = excluded. (The re-resolve survivor downstream still excludes the
// candidate if a NON-managed lower/higher layer keeps it active after removal — that is
// branch (b)'s job, not this own-value gate's.)
//
// Gated on the write-target value actually being an enable-only override
// (mimoCodeIsEnabledOnlyOverride — the SINGLE owner of the enabled-only shape) so a
// write-target value carrying its own command never routes here (the direct-command branch
// in the callers already owns that). The merged effective shape is decided by the SAME
// single owner the candidate loop used (mimoCodeNameInActiveSet over o.readJSON), so this
// gate cannot drift from the active-set definition that produced the candidate. A
// non-enable-only own value → false (no effective-ownership claim). A readJSON parse error
// propagates fail-closed.
func (o *mimoCodeClient) mimoCodeWriteTargetEnableOnlyActivatesEffectiveStdio(name string, ownValue map[string]any, consumer mimoCodeReResolveConsumer) (bool, error) {
	if !mimoCodeIsEnabledOnlyOverride(ownValue) {
		return false, nil
	}
	merged, err := o.readJSON()
	if err != nil {
		return false, err
	}
	return mimoCodeNameInActiveSet(merged, name, consumer), nil
}

// mimoCodeWriteTargetDefinesStdio reports whether the WRITE TARGET's (o.path) OWN
// value for mcp.<name> is itself a stdio entry — it physically carries a
// `command` (string or MiMoCode array form). It reads the write target's verbatim
// own value via mimoCodeFileEntryValue (single owner of the write-target raw
// read+parse, independent of the merged multi-layer view) and routes the shape
// decision through the SAME normalization the merged-view stdio match applied
// (mimoCodeNormalizeCommandArrays → non-empty string `command`), so the write
// target is judged by the exact owner that produced the candidate. NO disabled
// drop: an enabled:false write-target value re-enabled by a higher overlay still
// OWNS the stdio shape RemoveEntry will delete, which is exactly what this gate
// must confirm.
//
// EFFECTIVE-ENABLED OWNERSHIP (bot PR #425 FOLLOW-UP-2 FINDING 3). When the write
// target's own value carries NO command of its own but IS a bare {enabled:true}
// enable-only overlay that ACTIVATES a lower-layer stdio command (the merge overlays the
// flag onto config.json's {command, enabled:false}), the hub OWNS the activation —
// deleting the write-target key re-disables it. So this gate ALSO returns true via
// mimoCodeWriteTargetEnableOnlyActivatesEffectiveStdio. This is DISTINCT from the FINDING 1
// MANAGED enable-only guard (which EXCLUDES — the hub cannot clean a managed layer).
//
// A missing write target / absent name / non-stdio value → false; a parse error on present
// bytes propagates.
func (o *mimoCodeClient) mimoCodeWriteTargetDefinesStdio(name string) (bool, error) {
	value, ok, err := mimoCodeFileEntryValue(o.path, name)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	normalized := mimoCodeNormalizeCommandArrays(map[string]any{name: value})
	entry, ok := normalized[name].(map[string]any)
	if !ok {
		return false, nil
	}
	if cmd, _ := entry["command"].(string); cmd != "" {
		return true, nil // the write target's own value physically carries the command
	}
	// No own command — but a write-target {enabled:true} enable-only overlay that
	// activates a lower-layer stdio command OWNS the activation (FINDING 3).
	return o.mimoCodeWriteTargetEnableOnlyActivatesEffectiveStdio(name, value, reResolveConsumerStdio)
}

// mimoCodeWriteTargetDefinesStdioLSP reports whether the WRITE TARGET's (o.path) OWN
// value for mcp.<name> is ITSELF a stdio mcp-language-server entry. It is the LSP-shape
// sibling of mimoCodeWriteTargetDefinesStdio: it reads the write target's verbatim own
// value via mimoCodeFileEntryValue (single owner of the write-target raw read+parse,
// independent of the merged multi-layer view), applies the SAME normalization the
// merged-view LSP match used (mimoCodeNormalizeCommandArrays: array `command` → string
// command + prepended args), then runs the single canonical classifier
// matchLanguageServerStdio over the one named entry.
//
// NO mimoCodeDropDisabled (bot PR #425 FINDING 2). A gate that drops an enabled:false
// write-target value BEFORE classifying would judge a write-target mcp-language-server
// entry written {enabled:false} that a HIGHER overlay re-enables (so the merged match IS
// the active LSP) as "not the LSP shape", leaving the active direct LSP behind after
// register cleanup — the same effective-enabled defect mimoCodeWriteTargetDefinesStdio
// already avoids for plain gopls by judging the write-target shape disabled-or-not.
// An enabled:false write-target value re-enabled by a higher overlay still OWNS the
// stdio-LSP shape RemoveEntry will delete (RemoveEntry deletes the whole write-target key,
// disabled flag and all), which is exactly what this ownership gate must confirm. The
// cross-layer survivor (whether the merged name STAYS active after the write-target delete)
// is a SEPARATE concern owned by mimoCodeNameReResolvesAfterWriteTargetRemoval / the
// managed guards downstream, not by this own-value SHAPE gate.
//
// EFFECTIVE-ENABLED OWNERSHIP (bot PR #425 FOLLOW-UP-2 FINDING 3) — the LSP sibling of the
// same branch in mimoCodeWriteTargetDefinesStdio. A write-target bare {enabled:true}
// enable-only overlay that ACTIVATES a lower-layer mcp-language-server command (the merge
// overlays the flag onto config.json's {command:["mcp-language-server",...], enabled:false})
// makes the merged effective entry an ACTIVE direct LSP; deleting the write-target key
// re-disables it, so the hub OWNS the activation. Routed through the SAME
// mimoCodeWriteTargetEnableOnlyActivatesEffectiveStdio owner with the LSP consumer.
// DISTINCT from the MANAGED enable-only guard (which EXCLUDES).
//
// A missing write target / absent name / non-LSP value → false; a parse error on present
// bytes propagates fail-closed (the destructive caller aborts).
func (o *mimoCodeClient) mimoCodeWriteTargetDefinesStdioLSP(name string) (bool, error) {
	value, ok, err := mimoCodeFileEntryValue(o.path, name)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	normalized := mimoCodeNormalizeCommandArrays(map[string]any{name: value})
	entry, ok := normalized[name].(map[string]any)
	if !ok {
		return false, nil
	}
	if _, _, isStdioLSP := matchLanguageServerStdio(entry); isStdioLSP {
		return true, nil // the write target's own value physically carries the LSP command
	}
	// No own LSP command — but a write-target {enabled:true} enable-only overlay that
	// activates a lower-layer mcp-language-server command OWNS the activation (FINDING 3).
	return o.mimoCodeWriteTargetEnableOnlyActivatesEffectiveStdio(name, value, reResolveConsumerLSP)
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
	// mimoCodeWriteTargetDefinesStdioLSP routes that decision through the same single
	// classifier (matchLanguageServerStdio) that produced the merged match, judging the
	// write target's OWN value WITHOUT a disabled-drop (bot PR #425 FINDING 2) so an
	// enabled:false write-target entry a higher overlay re-enables is still OWNED. Under
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
	// (the FULL file/inline/import survivor, plus the CHANGE-1 managed OR), and
	// crucially COUNTS the ~/.claude.json import — opposite the AddEntry shadow
	// guard's exclusion — because skip-if-name-exists stops skipping once the
	// write-target key is gone.
	//
	// WORKSPACE-FREE CONSERVATIVE SURVIVOR (architect REVISE → Option C). This method
	// is consumed by the WORKSPACE-FREE `mcphub language-server cleanup`
	// (cli/language_server.go), so it KEEPS the full cross-layer survivor: any
	// same-name ACTIVE re-emergence declines, preventing a false-success delete.
	// The WORKSPACE-AWARE register cleanup does NOT consume this method — it consumes
	// FindStdioLanguageServerCandidatesWriteTargetOwned (branch (a) + managed-only)
	// and applies its own workspace-scoped survivor caller-side.
	out := matched[:0]
	for _, e := range matched {
		defined, err := o.mimoCodeWriteTargetDefinesStdioLSP(e.Name)
		if err != nil {
			return nil, err
		}
		if !defined {
			continue
		}
		reResolves, err := o.mimoCodeNameReResolvesAfterWriteTargetRemoval(e.Name, reResolveConsumerLSP)
		if err != nil {
			return nil, err
		}
		if reResolves {
			// Another layer re-resolves this name as an ACTIVE stdio mcp-language-
			// server entry after the write-target key is removed → RemoveEntry would
			// NOT clear it (the other layer re-emerges). Decline so the destructive
			// cleanup does not falsely report a removable LSP entry the hub cannot
			// fully remove. The LSP active-set survivor filter
			// (findLanguageServerStdioInMap) is applied inside the predicate, so a
			// non-LSP same-named re-emergence in another layer does not block the LSP
			// cleanup.
			continue
		}
		// MANAGED-ENABLE-ONLY GUARD (bot PR #425 follow-up FINDING 1). A managed
		// {enabled:true} enable-only overlay re-activates a lower DISABLED full
		// mcp-language-server entry through MiMoCode's enabled-overlay merge, so after the
		// write-target delete the LSP stays ACTIVE. The branch-(b) survivor above
		// (mimoCodeNameReResolvesAfterWriteTargetRemoval) does NOT catch it: its merge-based
		// half drops the disabled lower entry, and its managed OR
		// (mimoCodeManagedLayerReResolves) returns FALSE for an enable-only overlay (Kind=="",
		// PATH-B), so it reads removable. The CONDITIONED detector excludes it ONLY when a
		// re-activatable lower content-bearing survivor actually exists — NARROWER than the
		// register grain's over-blocking guard, because the CLI's FULL file survivor means an
		// unconditional over-block would wrongly drop a genuinely-removable entry whose
		// content lives only in the write target (the no-lower case). The guard applies THIS
		// consumer's LSP survivor shape (bot PR #425 FINDING 3) so a disabled lower REMOTE /
		// non-mcp-language-server stdio survivor does not falsely exclude a removable LSP entry.
		managedEnableReactivates, err := o.mimoCodeManagedEnableOnlyReactivatesLowerSurvivor(e.Name, reResolveConsumerLSP)
		if err != nil {
			return nil, err
		}
		if managedEnableReactivates {
			continue
		}
		// HARD-LINK GUARD (bot PR #425 FINDING 3). mimoCodeNameReResolvesAfterWriteTarget
		// Removal's simulate (readMergedLayersExcluding) matches the write target by INODE
		// (mimoCodePathsSamePhysical), so a lower-layer config.json HARD-LINKED to the write
		// target also has the name dropped in the simulate → reResolves reads FALSE and the
		// entry looks removable. PRODUCTION diverges: RemoveEntry's temp+rename breaks the
		// link and leaves the lower-layer entry LIVE, so the LSP re-emerges yet the CLI
		// reported it removable (false cleanup). The SAME FINDING-1-corrected detector the
		// register-grain candidate methods use excludes such a candidate here. (A SYMLINK /
		// case-alias is NOT a true hard link — it follows the rename — so the detector does
		// NOT block it.)
		hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines(e.Name)
		if err != nil {
			return nil, err
		}
		if hardLinked {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// FindStdioLanguageServerCandidatesWriteTargetOwned is the WORKSPACE-AWARE
// register-grain candidate source for the destructive direct-LSP cleanup (architect
// REVISE → Option C, bot PR #425 follow-up GAP 2). It is FindStdioLanguageServerEntries
// with branch (b) reduced to the MANAGED-only re-resolve: it keeps branch (a)
// (mimoCodeWriteTargetDefinesStdioLSP, the write-target stdio-LSP shape gate — no
// disabled-drop, bot PR #425 FINDING 2) and the MANAGED-layer guard
// (mimoCodeManagedLayerReResolves), but OMITS the FILE/inline/import-WIDE LSP survivor.
//
// That survivor is too coarse for the LSP grain: a same-name lower-layer
// mcp-language-server for a DIFFERENT workspace re-emerges as an active LSP entry
// and would wrongly block removal of the real workspace-A entry. So it MOVES
// CALLER-SIDE to a WORKSPACE-SCOPED recheck (register.go
// matchingDirectLanguageServerEntries via ActiveLanguageServerEntriesExcludingWriteTarget).
// A managed-active re-emergence (workspace-blind, read-only) still excludes the
// candidate HERE.
//
// Consumed ONLY by the workspace-aware register grain. The workspace-FREE
// `mcphub language-server cleanup` keeps the conservative full-survivor
// FindStdioLanguageServerEntries above.
func (o *mimoCodeClient) FindStdioLanguageServerCandidatesWriteTargetOwned() ([]LanguageServerStdioEntry, error) {
	m, err := o.readJSON()
	if err != nil {
		return nil, err
	}
	servers, _ := m[mimoCodeMCPKey].(map[string]any)
	matched := findLanguageServerStdioInMap(mimoCodeNormalizeCommandArrays(mimoCodeDropDisabled(servers)))
	out := matched[:0]
	for _, e := range matched {
		// (a) write-target stdio-LSP shape gate — same as FindStdioLanguageServerEntries
		// (mimoCodeWriteTargetDefinesStdioLSP, no disabled-drop, bot PR #425 FINDING 2).
		defined, err := o.mimoCodeWriteTargetDefinesStdioLSP(e.Name)
		if err != nil {
			return nil, err
		}
		if !defined {
			continue
		}
		// (b) MANAGED-only guard — the file/inline/import survivor is the caller's
		// workspace-scoped job (register.go). Only the workspace-blind managed retention
		// (a managed full-redefine / disabling shadow, minus the disable-only case)
		// excludes the candidate here; a managed enable-only overlay retains nothing on
		// its own and any lower-survivor re-activation is the caller's recheck's job.
		managedRetains, err := o.mimoCodeManagedLayerReResolves(e.Name)
		if err != nil {
			return nil, err
		}
		if managedRetains {
			continue
		}
		// (c) CONSERVATIVE cleanup guards (bot PR #425 follow-up FINDINGS 1+2) — same
		// SIMULATE-CONSUMER-ONLY blocks as RemovableStdioCandidatesWriteTargetOwned, kept
		// out of the RemoveEntry pre-check / B4 path. FINDING 1: a managed {enabled:true}
		// enable-only overlay re-activating a lower disabled full LSP entry (PATH-B managed
		// re-resolve returns false for it) → exclude. FINDING 2: a lower-layer file
		// hard-linked to the write target that defines the candidate re-emerges live after
		// the temp+rename delete → exclude. Both avoid a false-cleanup.
		managedEnableOnly, err := o.mimoCodeManagedEnableOnlyTrueOverlay(e.Name)
		if err != nil {
			return nil, err
		}
		if managedEnableOnly {
			continue
		}
		hardLinked, err := o.mimoCodeLowerLayerHardLinkedToWriteTargetDefines(e.Name)
		if err != nil {
			return nil, err
		}
		if hardLinked {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// ActiveStdioEntriesExcludingWriteTarget returns the ACTIVE stdio entries that
// re-emerge in the merged read AFTER a hypothetical RemoveEntry(name) from the
// WRITE TARGET — i.e. collectStdioEntries over readMergedLayersExcluding(name),
// the SAME active-stdio computation RemovableStdioEntries / AllStdioEntries use.
// It is the workspace-free, name-scoped post-removal reader the caller-side
// WORKSPACE-SCOPED gopls survivor recheck consumes (register.go
// matchingDirectGoplsMCPEntries, bot PR #425 follow-up GAP 2, architect GATE PASS).
//
// It deliberately carries NO managed layer (managed retention is the in-adapter
// mimoCodeManagedLayerReResolves guard) and NO workspace (the workspace identity
// lives only in register.go; the caller applies directEntryWorkspaceMatches over
// each returned entry's Args). The returned StdioEntry set is the full re-emergent
// active stdio view minus the write target's own mcp.<name>; the caller scans it
// for a same-name gopls-for-THIS-workspace survivor.
//
// State-safe and fail-closed: readMergedLayersExcluding collapses to the single
// supplied file in explicit/temp mode and propagates a parse error on any present
// non-import layer (so the destructive caller aborts and deletes nothing).
func (o *mimoCodeClient) ActiveStdioEntriesExcludingWriteTarget(name string) ([]StdioEntry, error) {
	mergedAfter, err := o.readMergedLayersExcluding(name)
	if err != nil {
		return nil, err
	}
	servers, _ := mergedAfter[mimoCodeMCPKey].(map[string]any)
	return collectStdioEntries(mimoCodeNormalizeCommandArrays(mimoCodeDropDisabled(servers))), nil
}

// ActiveLanguageServerEntriesExcludingWriteTarget is the LSP-shaped sibling of
// ActiveStdioEntriesExcludingWriteTarget: the ACTIVE mcp-language-server entries
// that re-emerge after a hypothetical RemoveEntry(name) from the write target,
// i.e. findLanguageServerStdioInMap over readMergedLayersExcluding(name). It
// returns LanguageServerStdioEntry so the caller can scope on `Language` (extracted
// by the single canonical classifier matchLanguageServerStdio inside
// findLanguageServerStdioInMap — never re-derived caller-side) plus
// directEntryWorkspaceMatches over Args. Consumed by the workspace-scoped LSP
// survivor recheck in register.go matchingDirectLanguageServerEntries (bot PR #425
// follow-up GAP 2). Workspace-free, managed-free, fail-closed — same contract as
// the stdio reader.
func (o *mimoCodeClient) ActiveLanguageServerEntriesExcludingWriteTarget(name string) ([]LanguageServerStdioEntry, error) {
	mergedAfter, err := o.readMergedLayersExcluding(name)
	if err != nil {
		return nil, err
	}
	servers, _ := mergedAfter[mimoCodeMCPKey].(map[string]any)
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

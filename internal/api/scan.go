package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// ScanOpts provides per-client config paths so tests can point at temp dirs.
// Production callers pass "" for each to use the OS default discovery.
//
// §9.2 FAMILY-B drift-prevention (2026-06-17): the canonical per-client
// path set is now the registry-derived `ConfigPaths` map (client-name →
// path override). Production callers populate it from
// clients.ConfigPathForName over clients.SupportedClientNames(), so a new
// registry client is automatically scanned/probed with ZERO scan.go edits
// — closing the drift class where a registry client with no named
// ScanOpts field was invisible to the scan surface (the bug ab80309 fixed
// once already for the original seven, then recurred for wave-2 and the
// agent-skills vendor set).
//
// The per-client named fields below are RETAINED as a thin back-compat
// layer: ~20 test sites construct `ScanOpts{ClaudeConfigPath: x, ...}` and
// keep working. effectiveConfigPaths() folds the named fields into the
// same map keyed by client id, with ConfigPaths taking precedence when a
// client is set in both. Empty paths (named field "" and absent map key)
// are skipped by ScanFrom/probeClientConfigPresence exactly as before.
type ScanOpts struct {
	// ConfigPaths is the registry-keyed config-path set (client id →
	// absolute path). The canonical input for production scan/probe.
	// nil/absent key for a client means "use the named-field fallback,
	// else skip". A "" value is treated the same as absent (skipped).
	ConfigPaths map[string]string

	// --- Back-compat named fields (folded into ConfigPaths by
	// effectiveConfigPaths; ConfigPaths wins on conflict). New code should
	// prefer ConfigPaths. ---
	ClaudeConfigPath      string
	CodexConfigPath       string
	CursorConfigPath      string
	VSCodeConfigPath      string
	GeminiConfigPath      string
	QwenConfigPath        string
	AntigravityConfigPath string
	// Wave-2 opt-in clients (PR #306 added the adapters; a follow-up PR
	// wired them into the scan/GUI surface).
	ZedConfigPath      string
	KiroConfigPath     string
	WindsurfConfigPath string
	ClineConfigPath    string
	KiloCodeConfigPath string
	OpenCodeConfigPath string
	MimoCodeConfigPath string
	HermesConfigPath   string
	OpenClawConfigPath string

	ManifestDir      string
	WithProcessCount bool // populate ScanEntry.ProcessCount via wmic
	GUIPort          int  // live GUI/hub listener port; zero means unknown/CLI
}

// legacyNamedConfigPathSet maps the back-compat named ScanOpts fields to
// their client id. Only clients that ever had a named field appear here;
// registry clients added after the §9.2 refactor (copilot-cli, amazon-q,
// openhands, aider, and future ones) have NO named field and are reached
// exclusively through ConfigPaths — which is the whole point of the
// refactor. This map exists only so effectiveConfigPaths can fold the
// legacy fields in; it is NOT the canonical client set (that is
// clients.SupportedClientNames()).
func (o ScanOpts) legacyNamedConfigPathSet() map[string]string {
	return map[string]string{
		"claude-code": o.ClaudeConfigPath,
		"codex-cli":   o.CodexConfigPath,
		"cursor":      o.CursorConfigPath,
		"vscode":      o.VSCodeConfigPath,
		"gemini-cli":  o.GeminiConfigPath,
		"qwen-cli":    o.QwenConfigPath,
		"antigravity": o.AntigravityConfigPath,
		"zed":         o.ZedConfigPath,
		"kiro":        o.KiroConfigPath,
		"windsurf":    o.WindsurfConfigPath,
		"cline":       o.ClineConfigPath,
		"kilocode":    o.KiloCodeConfigPath,
		"opencode":    o.OpenCodeConfigPath,
		"mimocode":    o.MimoCodeConfigPath,
		"hermes":      o.HermesConfigPath,
		"openclaw":    o.OpenClawConfigPath,
	}
}

// effectiveConfigPaths is the SINGLE derivation point that turns ScanOpts
// into the client-id → path set the scan/probe loops iterate. It merges,
// per client:
//   - ConfigPaths[name]      (registry-derived input; wins)
//   - the back-compat named field for that client (fallback)
//
// Empty values are dropped so the returned map only carries clients with a
// real path — preserving the long-standing "absent from the probe map when
// the path is empty" contract that
// TestProbeClientConfigPresence_Wave2Clients pins.
//
// Iteration is driven by the union of (a) every registry client that has a
// ConfigPaths entry and (b) every legacy named field — so a registry
// client with only a ConfigPaths value is included even though it has no
// named field, and a legacy field still works even if the caller never
// touched ConfigPaths.
func (o ScanOpts) effectiveConfigPaths() map[string]string {
	out := map[string]string{}
	// 1. Legacy named fields (fallback layer).
	for name, p := range o.legacyNamedConfigPathSet() {
		if p != "" {
			out[name] = p
		}
	}
	// 2. ConfigPaths overrides (registry-derived; wins on conflict).
	for name, p := range o.ConfigPaths {
		if p != "" {
			out[name] = p
		} else {
			// An explicit "" in ConfigPaths blanks the entry so a caller
			// can suppress a client even if a legacy field is also set.
			delete(out, name)
		}
	}
	return out
}

// probeClientConfigPresence reports whether each known MCP client's
// config file exists on disk and is stat-able. Used by the Servers
// matrix UI to distinguish "client installed but has no entries for
// this server yet" (operator can migrate to it) from "client not
// installed on this host" (cell disabled).
//
// Bug-bash A2 (#13) closure: pre-fix, the UI inferred client presence
// from per-entry `client_presence` keys, which collapsed when a
// wholesale demigrate emptied `mcpServers`. After fix, the UI reads
// this top-level map and renders an "available" cell whenever the
// client is "ok" even with no server entries.
//
// v0.4.5 init-button: distinguish "config file absent but the client's
// root directory exists" (the user has the client installed, just
// never created an MCP config — `missing-init-possible`, GUI shows
// an Initialize affordance) from "neither the file nor its parent
// directory exists" (`missing`, GUI keeps the column disabled because
// creating the parent tree would be a surprising side effect of
// a refresh / one-click action). The distinction is parent-dir
// granular: the immediate dirname must Lstat successfully as a
// directory.
//
// v0.4.5 PR #208 codex r1 F2: a third "missing-init-blocked-symlink"
// state covers parents that are symlinks (e.g. dotfile-management
// setups). The hardened init pipeline refuses to follow parent
// symlinks (O_NOFOLLOW on POSIX, FILE_FLAG_OPEN_REPARSE_POINT on
// Windows), so the GUI suppresses the Initialize button for such
// rows rather than offering a click that would deterministically
// fail with INIT_FAILED.
//
// Only paths the caller actually passed via ScanOpts are probed —
// keeps tempdir-based tests deterministic.
func probeClientConfigPresence(opts ScanOpts) map[string]string {
	out := map[string]string{}
	// §9.2 drift-prevention: iterate the registry-derived effective path
	// set (ConfigPaths + back-compat named fields) instead of a hardcoded
	// per-client list. ANY client a caller supplies a path for — including
	// registry clients with no named ScanOpts field — is probed here with
	// ZERO scan.go edits. The loop body below is client-agnostic.
	// effectiveConfigPaths already drops empty paths, so a client absent
	// from the map (no path supplied) never appears in `out` — preserving
	// the "absent when path empty" contract.
	for name, path := range opts.effectiveConfigPaths() {
		p := struct {
			name string
			path string
		}{name, path}
		// PR #208 deep-sec Lane B rounds 4-6 P2 closure: Lstat-first
		// probe + symlink resolution that matches the write pipeline's
		// default-relax contract.
		//
		// Wrong-shape entries to refuse:
		//   - directory / named pipe / device / junction at config path
		//     → "error" (round 5; matrix shows config-error diagnostic
		//     instead of an "ok" cell that migrate/backup will choke on
		//     when readJSON sees a non-regular target)
		//   - dangling symlink → "error-symlink" (round 4; init button +
		//     secure-create refusal lose otherwise)
		//   - symlink-to-regular-file in DEFAULT or STRICT mode →
		//     "error-symlink". Post-PR #209 the secure-write pipeline
		//     refuses pre-existing symlinks by default
		//     (`resolveSymlinkForSecureWrite` was removed from
		//     `secureWriteWithOperatorOpt`), so reporting "ok" while a
		//     write would fail with a symlink-refuse error is the UX trap
		//     bot codex-r7 flagged: green column, click Apply, write
		//     fails. NOT unconditional, though — the
		//     OperatorAllowsClientConfigSymlink() branch BELOW reports
		//     "ok" for a symlink-to-regular-file when the operator has
		//     opted in via MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK on a
		//     non-strict host (under that env, secureWriteWithOperatorOpt
		//     resolves the symlink before writing, so "ok = write will
		//     succeed" still holds). Strict mode
		//     (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) overrides the opt-in and
		//     keeps the refusal — corp-managed hosts stay hardened.
		//
		// The "error-symlink" status is therefore the DEFAULT/STRICT-mode
		// classification, returned DISTINCT from the generic
		// stat/wrong-shape failure ("error") so the Servers matrix can
		// drive an opt-in-aware tooltip (set
		// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK on a single-user host, or
		// replace the symlink / edit the target) instead of the
		// misleading "stat error — check permissions and disk health"
		// diagnostic — NOT a claim of unconditional refusal
		// (work-items/bugs/2026-05-19-codex-config-symlink-blocked-by-pr209.md).
		// Only the reported category is split here; the write contract
		// (including the opt-in) is unchanged.
		if lst, lerr := os.Lstat(p.path); lerr == nil {
			isSymlink := lst.Mode()&os.ModeSymlink != 0
			if !lst.Mode().IsRegular() && !isSymlink {
				out[p.name] = "error"
				continue
			}
			if isSymlink {
				// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in (post-PR
				// #209 reintroduction under explicit operator
				// consent): when set, treat a symlink whose target
				// is a regular file as "ok" so the matrix renders
				// the column enabled. Solo-developer dotfile setups
				// (chezmoi / yadm / GNU stow / plain ln -s from
				// ~/.codex/config.toml → /e/env/Agents/...) work
				// again. Strict mode short-circuits to refusal
				// inside OperatorAllowsClientConfigSymlink so
				// corp-managed hosts keep unconditional refusal.
				//
				// Uses os.Stat (kernel-level symlink follow) rather
				// than filepath.EvalSymlinks. EvalSymlinks is Go's
				// string-based resolver and chokes on the
				// POSIX-style target paths Git Bash's `ln -s` can
				// store inside Windows reparse points — the
				// resolver tries to walk the literal path string
				// and fails with ERROR_PATH_NOT_FOUND on segments
				// the kernel would have happily resolved through
				// drive-letter or junction layers. os.Stat goes
				// through the kernel which honors reparse-point
				// semantics regardless of how the stored target
				// string is formatted. A non-regular result
				// (dangling target, points to a directory or
				// special file) still classifies as "error".
				if OperatorAllowsClientConfigSymlink() {
					if rst, rstErr := os.Stat(p.path); rstErr == nil && rst.Mode().IsRegular() {
						out[p.name] = "ok"
						continue
					}
				}
				// Distinct from the generic "error" so the matrix renders
				// a symlink-specific diagnostic (see header comment).
				out[p.name] = "error-symlink"
				continue
			}
		}
		if _, err := os.Stat(p.path); err == nil {
			out[p.name] = "ok"
		} else if os.IsNotExist(err) {
			out[p.name] = classifyMissingClientConfig(p.path)
		} else {
			out[p.name] = "error"
		}
	}
	return out
}

// classifyMissingClientConfig returns one of:
//
//   - "missing-init-possible"        : the config file's immediate
//     parent directory exists and is a regular directory (operator can
//     click Initialize; the hardened init pipeline will accept it).
//   - "missing-init-creatable"       : the config file's immediate
//     parent directory does NOT exist, but the path is under the user
//     home and the longest-existing prefix of the parent chain is a
//     real (non-symlink) directory chain. The hardened init pipeline
//     can securely create the missing parent components and then seed
//     the stub. The GUI renders Initialize with a "will create <dir>"
//     tooltip. G17 (vendor-init-uninstalled-clients).
//   - "missing-init-blocked-symlink" : the parent path resolves through
//     a symlink (Init pipeline opens the parent with O_NOFOLLOW on POSIX
//     / FILE_FLAG_OPEN_REPARSE_POINT on Windows, both of which refuse
//     to follow the link — so an Initialize click would deterministically
//     fail with INIT_FAILED, broken UX). Common with dotfile-management
//     setups where the operator symlinks ~/.config/Claude/ to a real
//     dotfile repo location. The GUI suppresses the Initialize button
//     for this state; operators who want the stub seeded should either
//     remove the symlink (and let mcphub create the config in the real
//     parent), or seed the file manually inside the symlinked target.
//     This state ALSO covers a missing parent whose longest-existing
//     prefix passes through a symlink — the secure parent-create refuses
//     to descend through it, so the affordance stays suppressed.
//   - "missing"                       : neither file nor parent dir
//     exists AND the path is not under the user home (or the existing
//     prefix contains a non-directory), so secure creation is refused.
//
// Split out for testability and to keep probeClientConfigPresence flat.
//
// v0.4.5 PR #208 codex r1 F2 closure: prior version used os.Stat which
// follows symlinks; a symlinked parent passed the IsDir check, scan
// classified as missing-init-possible, GUI showed Initialize button,
// click failed with INIT_FAILED. The Lstat-first probe now distinguishes
// the symlinked-parent case so the UI matches the actual write contract.
//
// G17 (2026-06-18): the absent-parent case now distinguishes
// "missing-init-creatable" (parent under home, creatable via the secure
// parent-create) from "missing" (not under home / non-dir prefix). This
// lets the operator pre-configure a not-yet-installed client whose config
// dir does not exist yet — the secure parent-create makes the missing
// directory component-by-component, refusing symlinks/reparse-points and
// any path outside the user home (see SecureCreateClientConfigParentDir).
func classifyMissingClientConfig(path string) string {
	parent := filepath.Dir(path)
	if parent == "" || parent == "." {
		return "missing"
	}
	lst, lerr := os.Lstat(parent)
	if lerr != nil {
		// Parent path itself is absent OR stat failed. If the absence
		// is a clean "not found", probe whether the secure parent-create
		// could make it (under home, no symlink/non-dir in the existing
		// prefix). Any other stat error → "missing" (not initializable).
		if os.IsNotExist(lerr) {
			return classifyAbsentParentCreatable(parent)
		}
		return "missing"
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		// Symlinked parent. The hardened init pipeline refuses to open
		// the parent through a symlink (POSIX O_NOFOLLOW, Windows
		// FILE_FLAG_OPEN_REPARSE_POINT — the latter opens the reparse
		// point itself without auto-resolving for relative child ops,
		// which similarly breaks the init create flow). Classify so
		// the GUI suppresses the Initialize affordance.
		return "missing-init-blocked-symlink"
	}
	if !lst.IsDir() {
		// Parent path exists but is not a directory (regular file,
		// device, pipe). Not initializable.
		return "missing"
	}
	return "missing-init-possible"
}

// isPromotableAbsentPresenceState reports whether a probeClientConfigPresence
// verdict is an ABSENT (config-file-not-yet-present) state whose write target is
// TRULY WRITABLE, so a lower/overlay/inline read layer may promote it to "ok".
// The full set classifyMissingClientConfig can return is "missing",
// "missing-init-possible", "missing-init-creatable", and
// "missing-init-blocked-symlink"; the NON-missing verdicts are "ok" (the file is
// a regular file) and the config-FAULT states "error" (non-regular target:
// directory / FIFO / device) and "error-symlink" (refused / dangling symlink).
//
// Used only by the MiMoCode lower-layer presence promotion (ScanFrom). The
// promotion makes the cell render a normal enabled state and lets Apply/backup
// proceed against the write target, so it must fire ONLY when that write target
// could actually be created and written:
//
//   - It NEVER upgrades a config-FAULT verdict ("error" / "error-symlink") — that
//     would mask a bad write target behind a green cell Apply/backup cannot write
//     to (bot PR #420 finding 2 refinement).
//   - It also EXCLUDES "missing-init-blocked-symlink": the write/init pipeline
//     refuses to create the missing write target through a parent symlink
//     (POSIX O_NOFOLLOW / Windows FILE_FLAG_OPEN_REPARSE_POINT), so even though a
//     lower layer (config.json) exists, an Apply that needs to create the write
//     target would deterministically fail. Promoting it to "ok" would show a
//     normal enabled cell whose later Apply fails — exactly the broken UX the
//     config-FAULT exclusion already prevents. So only the genuinely-creatable
//     absent states (missing / missing-init-possible / missing-init-creatable)
//     are promotable.
func isPromotableAbsentPresenceState(state string) bool {
	switch state {
	case "missing", "missing-init-possible", "missing-init-creatable":
		return true
	default:
		return false
	}
}

// classifyAbsentParentCreatable classifies a config-file parent dir that
// is absent (clean os.IsNotExist). It mirrors the safety contract of the
// secure parent-create (SecureCreateClientConfigParentDir) WITHOUT
// mutating the filesystem, so the GUI affordance matches what an
// Initialize click would actually do:
//
//   - "missing-init-creatable"       : parent is under the user home,
//     and the longest-existing prefix of the chain is a real
//     (non-symlink) directory chain. The remaining components can be
//     securely created.
//   - "missing-init-blocked-symlink" : the longest-existing prefix
//     passes through a symlink — the secure create refuses to descend,
//     so the affordance is suppressed (same UX as a directly-symlinked
//     parent).
//   - "missing"                       : parent is NOT under the user
//     home, OR the existing prefix contains a non-directory entry, OR
//     the user home cannot be resolved. Not securely creatable.
//
// The "under the user home" constraint is the blast-radius bound: the
// secure create never makes directories outside HOME, so neither does
// this classifier offer the affordance for such paths.
func classifyAbsentParentCreatable(parent string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "missing"
	}
	rel, under := pathUnderHome(home, parent)
	if !under {
		return "missing"
	}
	// rel is the set of components from home down to parent. Walk the
	// existing prefix: each existing component must be a real directory
	// (no symlink, no non-dir). The first absent component (and beyond)
	// is what the secure create would make.
	cur := filepath.Clean(home)
	for _, comp := range rel {
		cur = filepath.Join(cur, comp)
		lst, lerr := os.Lstat(cur)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				// Reached the first absent component. Everything from
				// here down is creatable; nothing more to inspect.
				return "missing-init-creatable"
			}
			// Anomalous stat on an existing-prefix component — not
			// safely creatable.
			return "missing"
		}
		if lst.Mode()&os.ModeSymlink != 0 {
			// Existing prefix passes through a symlink — the secure
			// create refuses to descend through it. Suppress.
			return "missing-init-blocked-symlink"
		}
		if !lst.IsDir() {
			// Existing prefix component is a non-directory (regular
			// file, device, pipe) — cannot create a child under it.
			return "missing"
		}
	}
	// Whole chain already exists as real dirs — caller's earlier Lstat
	// said the parent was absent, so this is a benign race (the parent
	// appeared between the two stats). Treat as initializable.
	return "missing-init-possible"
}

// pathUnderHome reports whether `target` (an absolute path) lies at or
// below the user `home` directory, and if so returns the slice of path
// components from home down to target (empty slice when target == home).
//
// Both inputs are filepath.Clean-ed and compared with the OS-appropriate
// path-equality semantics (case-insensitive on Windows). Returns
// (nil, false) when target is not under home, or when filepath.Rel
// produces a parent-escaping (`..`) or absolute result. This is the
// classifier-side mirror of the secure parent-create's home-containment
// gate; both must agree so the GUI affordance matches the write contract.
func pathUnderHome(home, target string) ([]string, bool) {
	home = filepath.Clean(home)
	target = filepath.Clean(target)
	rel, err := filepath.Rel(home, target)
	if err != nil {
		return nil, false
	}
	if rel == "." {
		return []string{}, true
	}
	// Reject any path that escapes home (`..`) or is absolute (Rel can
	// return an absolute path when the volumes differ on Windows).
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, false
	}
	return strings.Split(rel, string(filepath.Separator)), true
}

// clientScanFunc reads a single client's config at `path` and merges any
// discovered MCP server entries into `entries`. Every per-client scanner
// (scanClaude, scanCodex, …) has this signature.
type clientScanFunc func(entries map[string]*ScanEntry, path string) error

// clientScanners is the §9.2 drift-prevention registry mapping a client id
// to (its scan function, its error-wrap prefix). ScanFrom dispatches
// through this map driven by clients.SupportedClientNames(), so adding a
// registry client + its scanner here is the ONLY edit needed to give that
// client scan coverage — the per-client `if opts.XConfigPath != "" {...}`
// ladder it replaced forced an edit to ScanFrom for every new client and
// silently dropped any registry client that lacked a named ScanOpts field.
//
// A registry client absent from this map (copilot-cli, amazon-q, openhands,
// aider today — no shape parser written yet) is still presence-probed by
// probeClientConfigPresence; it is only skipped for entry discovery. When a
// shape parser is added, register it here and it is auto-dispatched.
//
// The wrap prefix is carried explicitly to keep error messages byte-for-byte
// identical to the pre-refactor ladder (which used short names like
// "claude"/"codex"/"gemini"/"qwen", not the client ids
// "claude-code"/"codex-cli"/…).
func clientScanners() map[string]struct {
	scan   clientScanFunc
	prefix string
} {
	return map[string]struct {
		scan   clientScanFunc
		prefix string
	}{
		"claude-code": {scanClaude, "claude"},
		"codex-cli":   {scanCodex, "codex"},
		"cursor":      {scanCursor, "cursor"},
		"vscode":      {scanVSCode, "vscode"},
		"gemini-cli":  {scanGemini, "gemini"},
		"qwen-cli":    {scanQwen, "qwen"},
		"antigravity": {scanAntigravity, "antigravity"},
		// Wave-2 opt-in clients. Each scanner mirrors the shape its adapter
		// writes (see internal/clients/<client>.go). The HTTP-direct clients
		// reuse the generic per-client shaper; zed uses the relay-shape
		// detector because, like antigravity, its hub entry is a `mcphub
		// relay` stdio invocation rather than a loopback url.
		"zed":      {scanZed, "zed"},
		"kiro":     {scanKiro, "kiro"},
		"windsurf": {scanWindsurf, "windsurf"},
		"cline":    {scanCline, "cline"},
		"kilocode": {scanKiloCode, "kilocode"},
		"opencode": {scanOpenCode, "opencode"},
		// mimocode is an OpenCode fork sharing the top-level `mcp` config
		// shape — scanMimoCode mirrors scanOpenCode under the mimocode id.
		"mimocode": {scanMimoCode, "mimocode"},
		"hermes":   {scanHermes, "hermes"},
		"openclaw": {scanOpenClaw, "openclaw"},
		// TIER-1 skills-CLI clients. These all use the canonical
		// top-level mcpServers JSON shape; Pi is relay-stdio, while the
		// others are HTTP-direct and expose the endpoint under url.
		"bob":           {scanBob, "bob"},
		"codebuddy":     {scanCodeBuddy, "codebuddy"},
		"command-code":  {scanCommandCode, "command-code"},
		"cortex":        {scanCortex, "cortex"},
		"deepagents":    {scanDeepAgents, "deepagents"},
		"devin":         {scanDevin, "devin"},
		"droid":         {scanDroid, "droid"},
		"firebender":    {scanFirebender, "firebender"},
		"iflow-cli":     {scanIFlowCLI, "iflow-cli"},
		"junie":         {scanJunie, "junie"},
		"kimi-code-cli": {scanKimiCodeCLI, "kimi-code-cli"},
		"kode":          {scanKode, "kode"},
		"ona":           {scanOna, "ona"},
		"pi":            {scanPi, "pi"},
		"qoder":         {scanQoder, "qoder"},
		"qoder-cn":      {scanQoderCN, "qoder-cn"},
		"roo":           {scanRoo, "roo"},
		"rovodev":       {scanRovoDev, "rovodev"},
		"tabnine-cli":   {scanTabnineCLI, "tabnine-cli"},
	}
}

// perSessionServers are MCP servers whose sessions must remain isolated
// per local client/process. Even when an upstream tool supports a session_id
// parameter, we conservatively keep them per-session unless the hub
// enforces caller authentication and session ownership.
//
// gdb was previously listed here; PR #24 restored servers/gdb/manifest.yaml
// as a hub-managed daemon because GDB-MCP has built-in session management
// (modules/{gdb,lldb}/sessionManager.py) where each client call carries a
// session_id, so one daemon serves N concurrent debug sessions safely.
// Keeping gdb in this map after restoring the manifest would force
// CanMigrate=false in scan results and contradict the manifest contract.
// Codex bot review on PR #24.
var perSessionServers = map[string]bool{
	"playwright": true,
}

// ScanFrom builds a unified cross-client view. Exposed (rather than Scan) so
// tests can pass arbitrary paths.
//
// PR #208 deep-sec Lane B round 6 P2 closure: presence is computed
// FIRST so adapter reads can be skipped for clients whose config
// path is not a readable regular file (directory, FIFO, dangling
// symlink, etc.). Previously the per-adapter `scan*` calls hit
// `os.ReadFile` before presence was known, and any non-IsNotExist
// read error propagated as a whole-response 500 SCAN_FAILED —
// hiding the per-client diagnostic the frontend needs to render the
// `config-error` cell. With the reorder, a wrong-shape entry at one
// client's config path leaves the rest of the scan intact and the
// per-client diagnostic surfaces in `client_config_presence`.
func (a *API) ScanFrom(opts ScanOpts) (*ScanResult, error) {
	entries := map[string]*ScanEntry{}
	presence := probeClientConfigPresence(opts)
	paths := opts.effectiveConfigPaths()
	// MiMoCode multi-layer presence (bot PR #420 finding 2): the generic probe
	// above stats ONLY the registered scan path (mimocode's WRITE target —
	// mimocode.json). MiMoCode deep-merges lower layers (config.json), the
	// MIMOCODE_CONFIG file, and the MIMOCODE_CONFIG_DIR overlay, so a profile
	// whose servers live ONLY in a lower/overlay layer (write target absent)
	// would probe a MISSING state → the scanIfReadable gate would SKIP
	// scanMimoCode → the operator's real entries vanish from the matrix. Promote
	// presence to "ok" when ANY resolved read layer exists as a regular file (so
	// the row is both scanned and shown).
	//
	// SCOPE OF THE PROMOTION (bot PR #420 finding 2 refinement): only a MISSING
	// (absent) write-target verdict may be promoted. If the generic probe already
	// classified the write target itself as a config ERROR — "error" (the path is
	// a directory / FIFO / device) or "error-symlink" (a refused / dangling
	// symlink) — that is a real write-target fault Apply/backup must not proceed
	// against, and the GUI must keep rendering the config-error cell. Promoting
	// THAT to "ok" because a SEPARATE lower layer happens to exist would mask the
	// fault: the row would render green, the operator clicks Apply, and the
	// hardened write fails against the bad write target. So the promotion is
	// gated on isPromotableAbsentPresenceState — it upgrades only the WRITABLE
	// absent states (missing / missing-init-possible / missing-init-creatable),
	// never an error/error-symlink/ok verdict, and never
	// missing-init-blocked-symlink (the write/init pipeline refuses to create the
	// write target through a parent symlink, so a promoted cell's later Apply
	// would deterministically fail — same broken-UX hazard as the config-FAULT
	// states).
	//
	// INLINE-ONLY PROFILES (bot PR #420 finding 1): MimoCodeReadLayerPaths yields
	// only FILE paths, so a profile whose ONLY mimo config layer is
	// MIMOCODE_CONFIG_CONTENT (inline, no file on disk) has nothing to stat and
	// would never promote — yet MimoCodeMergedConfig parses that inline layer and
	// surfaces its servers. So promote on a parseable inline layer too, not just a
	// stat-able file. Either signal upgrades the absent state to "ok".
	//
	// MALFORMED INLINE-ONLY (bot PR #420 finding 4): a present-but-UNPARSEABLE
	// MIMOCODE_CONFIG_CONTENT (no file layers) is an active-but-broken profile.
	// Left as the default "missing"/absent verdict it would render the cell as
	// not-configured and the scanner would never run the merged read that surfaces
	// the parse error — the broken profile looks ABSENT. So promote it to the
	// existing config-FAULT "error" state (the same state a non-regular write
	// target / wrong-shape file produces), making the matrix render a loud
	// config-error cell instead of silently dropping the profile. Only a TRULY
	// absent verdict is promoted (isPromotableAbsentPresenceState already excludes
	// "ok"/"error"/"error-symlink"), so this never downgrades an "ok" or masks an
	// existing fault.
	if mp := paths["mimocode"]; mp != "" && isPromotableAbsentPresenceState(presence["mimocode"]) {
		for _, lf := range clients.MimoCodeReadLayerPaths(mp) {
			if st, err := os.Stat(lf); err == nil && st.Mode().IsRegular() {
				presence["mimocode"] = "ok"
				break
			}
		}
		if presence["mimocode"] != "ok" {
			switch state, _ := clients.MimoCodeInlineContentState(mp); state {
			case "ok":
				presence["mimocode"] = "ok"
			case "error":
				// Malformed inline-only profile → loud config-error cell, not absent.
				presence["mimocode"] = "error"
			}
		}
	}
	scanIfReadable := func(name string) bool {
		// "ok" is the only state for which an adapter read is
		// guaranteed to find a regular file. "missing" /
		// "missing-init-possible" / "error" / "error-symlink" all
		// imply that the adapter's `os.ReadFile` would either return
		// IsNotExist (which adapters already absorb to "no entries")
		// or hit the wrong-shape / symlink-refusal failure we want
		// to avoid.
		return presence[name] == "ok"
	}

	// §9.2 drift-prevention: dispatch per-client scanning through the
	// clientScanners registry keyed by client id, driven by the
	// registry-derived effective path set. A registry client gains scan
	// coverage the moment it has BOTH a config path (via ConfigPaths /
	// SupportedClientNames in production) AND a clientScanners entry — with
	// ZERO edits to this loop. Clients in the registry but without a parser
	// (e.g. copilot-cli/amazon-q/openhands/aider until a shape parser is
	// written) are still PROBED for presence above; they are simply skipped
	// here, surfacing in the matrix as an available/installed column with
	// no discovered entries rather than silently vanishing.
	//
	// Iteration order follows clients.SupportedClientNames() (stable
	// registry order) so multi-client scan results are deterministic — the
	// original code's fixed claude→...→openclaw order is preserved because
	// that is exactly the registry order.
	scanners := clientScanners()
	for _, name := range clients.SupportedClientNames() {
		sc, ok := scanners[name]
		if !ok || sc.scan == nil {
			continue // registry client with no shape parser yet
		}
		path := paths[name]
		if path == "" || !scanIfReadable(name) {
			continue
		}
		if err := sc.scan(entries, path); err != nil {
			return nil, fmt.Errorf("%s: %w", sc.prefix, err)
		}
	}

	manifestNames, err := readManifestNames(opts.ManifestDir)
	if err != nil {
		return nil, fmt.Errorf("manifests: %w", err)
	}
	for name, e := range entries {
		e.Name = name
		e.ManifestExists = manifestNames[name]
		e.CanMigrate = e.ManifestExists && !perSessionServers[name]
		// Port-aware via-hub: a loopback-http entry is only "via-hub" when
		// its URL port matches one of THIS server's manifest daemon ports.
		// Load the set once per row and expose it on the entry so the
		// frontend matrix can mirror the same port-match rule (a stale-port
		// loopback entry that backend classifies "external" must not render
		// as a green via-hub cell).
		e.DaemonPorts = manifestDaemonPorts(opts.ManifestDir, name)
		e.Status = classify(e, name, manifestNames, e.DaemonPorts, opts.GUIPort)
		// Managed is the explicit hub-routed flag — set true iff the
		// classifier landed on "via-hub". Keeping it derived from Status (one
		// owner) avoids a second hub-detection path drifting out of sync with
		// classify().
		e.Managed = e.Status == "via-hub"
	}
	// Pass over manifest names to ensure servers with no client
	// presence still appear in the matrix. Without this, a wholesale
	// demigrate (uncheck-and-Apply on every client column for a
	// server) would make the row vanish entirely — the operator
	// could not re-enable the server from the matrix because the
	// row to click was gone. Empty ClientPresence means every
	// non-disabled column renders as "available" — operator checks
	// the desired cells + Apply to re-install.
	for name := range manifestNames {
		if _, exists := entries[name]; exists {
			continue
		}
		e := &ScanEntry{
			Name:           name,
			ManifestExists: true,
			CanMigrate:     !perSessionServers[name],
			ClientPresence: map[string]ClientEntry{},
		}
		e.DaemonPorts = manifestDaemonPorts(opts.ManifestDir, name)
		e.Status = classify(e, name, manifestNames, e.DaemonPorts, opts.GUIPort)
		e.Managed = e.Status == "via-hub" // always false here (empty presence), kept for symmetry with the main loop.
		entries[name] = e
	}

	// Task 3.3: LSP-bridge recognition. Load the workspace registry as a
	// soft ownership hint (best-effort; nil registry still yields
	// language-aware labels). Cross-references hub HTTP rows with the
	// optional direct-stdio coexistence rows so the matrix shows the
	// anomaly under a single (server, client) cell instead of two
	// disjoint rows the operator has to mentally pair.
	var reg *Registry
	if regPath, perr := DefaultRegistryPath(); perr == nil {
		r := NewRegistry(regPath)
		if lerr := r.Load(); lerr == nil {
			reg = r
		}
	}
	classifyLSPEntries(entries, reg)

	out := &ScanResult{
		At:                   time.Now(),
		ClientConfigPresence: presence,
		GUIPort:              opts.GUIPort,
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, *e)
	}
	if opts.WithProcessCount {
		// One process-snapshot shared across every entry. Previously
		// CountProcesses launched wmic per entry — for ~20 scan rows
		// that's ~13 s wall time. Single snapshot + in-memory count
		// drops the scan to ~1 s.
		snap := takeProcessSnapshot()
		for i := range out.Entries {
			patterns := patternsForServer(out.Entries[i].Name, opts.ManifestDir)
			if len(patterns) == 0 {
				continue
			}
			out.Entries[i].ProcessCount = a.CountProcessesFromSnapshot(snap, patterns)
		}
	}
	return out, nil
}

// isOurRelayBinary returns true when the given path points at our CLI
// binary — either the current name (mcphub.exe) or the legacy name
// (mcp.exe) that early installations may still have in client configs.
// Delegates to clients.IsMcphubBinary so the binary-name allowlist lives in
// exactly one place; this wrapper is kept only because the classifier
// below reads better when paired with a local name. If a future client
// ever persists a different binary name, update only IsMcphubBinary.
func isOurRelayBinary(cmd string) bool {
	return clients.IsMcphubBinary(cmd)
}

// genericInterpreters are command names that match far too many unrelated
// processes (every npx-invoked tool, every python script, etc.) to be useful
// as identifying patterns. We skip them when building patterns and rely on
// server-specific tokens (package names, script paths) for identification.
var genericInterpreters = map[string]bool{
	"npx": true, "npx.cmd": true,
	"node": true, "node.exe": true,
	"python": true, "python.exe": true, "python3": true,
	"uv": true, "uvx": true, "uv.exe": true, "uvx.exe": true,
	"cmd": true, "cmd.exe": true,
	"sh": true, "bash": true,
}

// patternsForServer returns the substring patterns used to identify running
// processes of this server. Generic interpreters (npx, node, python, uvx)
// are skipped because they match thousands of unrelated processes; only
// server-specific tokens (package names, script paths) reliably identify
// a server's own processes. For non-manifested (unknown/per-session) servers,
// falls back to the server name — callers treat counts for unknown servers
// as an upper bound.
func patternsForServer(serverName, manifestDir string) []string {
	var (
		data []byte
		err  error
	)
	if manifestDir == "" {
		// Production path: embed first, disk fallback.
		data, err = loadManifestYAMLEmbedFirst(serverName)
	} else {
		// Test-pathway: explicit dir only.
		data, err = os.ReadFile(filepath.Join(manifestDir, serverName, "manifest.yaml"))
	}
	if err != nil {
		return []string{serverName}
	}
	m, err := parseManifestForName(serverName, data)
	if err != nil {
		return []string{serverName}
	}
	var out []string
	if m.Command != "" && !genericInterpreters[m.Command] {
		out = append(out, m.Command)
	}
	for _, arg := range m.BaseArgs {
		if len(arg) <= 3 || strings.HasPrefix(arg, "-") {
			continue
		}
		// Apply the same generic-interpreter filter to BaseArgs so manifests
		// that embed an interpreter in their args (e.g. gdb's "python",
		// "server.py") don't contribute substrings that match unrelated
		// processes system-wide. Dropbox ships a bundled Python, VS Code
		// ships node, MSYS2 fills every shell with "python"/"node" tokens —
		// matching any of those as an orphan pattern is a false-positive
		// bomb. Keep only paths / package-name tokens that are unique to
		// the server.
		base := strings.ToLower(filepath.Base(arg))
		if genericInterpreters[base] {
			continue
		}
		out = append(out, arg)
	}
	if len(out) == 0 {
		out = append(out, serverName)
	}
	return out
}

func scanClaude(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["claude-code"] = shapeClaudeEntry(raw)
	}
	return nil
}

func shapeClaudeEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanCodex(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var root map[string]any
	if err := toml.Unmarshal(data, &root); err != nil {
		return err
	}
	srv, _ := root["mcp_servers"].(map[string]any)
	for name, raw := range srv {
		m, _ := raw.(map[string]any)
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["codex-cli"] = shapeCodexEntry(m)
	}
	return nil
}

func shapeCodexEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanCursor(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["cursor"] = shapeCursorEntry(raw)
	}
	return nil
}

func shapeCursorEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanVSCode(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	// VS Code's settings.json is JSONC (comments + trailing commas), same as
	// Zed — tolerate it (strict JSON parses byte-identically through the
	// preprocessor, so this is behavior-preserving for a comment-free file).
	if err := json.Unmarshal(stripJSONCommentsAndTrailingCommas(data), &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.Servers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["vscode"] = shapeVSCodeEntry(raw)
	}
	return nil
}

func shapeVSCodeEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanGemini(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["gemini-cli"] = shapeGeminiEntry(raw)
	}
	return nil
}

func shapeGeminiEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	if url, ok := raw["httpUrl"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanQwen(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["qwen-cli"] = shapeQwenEntry(raw)
	}
	return nil
}

func shapeQwenEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["httpUrl"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

func scanAntigravity(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["antigravity"] = shapeAntigravityEntry(raw)
	}
	return nil
}

func shapeAntigravityEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["serverUrl"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	// Detect our own relay shape: command is mcphub.exe (or legacy mcp.exe)
	// with args[0]=="relay". Accepting both names because early installs
	// used "mcp.exe" before the rename.
	if cmd, ok := raw["command"].(string); ok {
		if args, ok := raw["args"].([]any); ok && len(args) > 0 {
			if first, _ := args[0].(string); first == "relay" && isOurRelayBinary(cmd) {
				return ClientEntry{Transport: "relay", Endpoint: cmd, RelayURL: relayURLFromArgs(args), Raw: raw}
			}
		}
		return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
	}
	return ClientEntry{Transport: "absent", Raw: raw}
}

func relayURLFromArgs(args []any) string {
	for i := 0; i+1 < len(args); i++ {
		flag, _ := args[i].(string)
		if flag != "--url" {
			continue
		}
		url, _ := args[i+1].(string)
		return url
	}
	return ""
}

// ---------------------------------------------------------------------------
// Wave-2 opt-in client scanners (PR #306 adapters → scan/GUI surface).
//
// Each scanner reads the exact config shape its adapter writes
// (internal/clients/<client>.go) and records a per-(server, client)
// ClientEntry under the canonical client id. The shaper for each client
// recognises that client's url key so classify() can tag a loopback hub
// entry as "via-hub". zed is relay-stdio (like antigravity), so it reuses
// the relay-shape detector.
// ---------------------------------------------------------------------------

// scanZed reads Zed's user settings.json. Unlike the JSON family, Zed nests
// its MCP map under the top-level `context_servers` key and writes a
// relay-stdio hub entry (`command`=mcphub binary, args[0]=="relay") — the
// same shape antigravity uses. shapeAntigravityEntry already classifies the
// relay shape, so it is reused verbatim here.
func scanZed(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		ContextServers map[string]map[string]any `json:"context_servers"`
	}
	// Zed's settings.json is JSONC (allows // and /* */ comments + trailing
	// commas), which strict encoding/json rejects with
	// "invalid character '/' looking for beginning of value" — the failure
	// that turned the whole Servers scan into a 500. Reuse the existing
	// JSONC preprocessor (the VS Code import path's
	// stripJSONCommentsAndTrailingCommas) so a real editor config parses.
	if err := json.Unmarshal(stripJSONCommentsAndTrailingCommas(data), &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.ContextServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["zed"] = shapeAntigravityEntry(raw)
	}
	return nil
}

func scanKiro(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "kiro", shapeURLOrCommandEntry)
}

func scanWindsurf(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "windsurf", shapeWindsurfEntry)
}

func scanCline(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "cline", shapeURLOrCommandEntry)
}

func scanKiloCode(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "kilocode", shapeURLOrCommandEntry)
}

func scanBob(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "bob", shapeURLOrCommandEntry)
}

func scanCodeBuddy(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSONC(entries, path, "codebuddy", shapeURLOrCommandEntry)
}

func scanCommandCode(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "command-code", shapeURLOrCommandEntry)
}

func scanCortex(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "cortex", shapeURLOrCommandEntry)
}

func scanDeepAgents(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "deepagents", shapeURLOrCommandEntry)
}

func scanDevin(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "devin", shapeURLOrCommandEntry)
}

func scanDroid(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "droid", shapeURLOrCommandEntry)
}

func scanFirebender(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "firebender", shapeURLOrCommandEntry)
}

func scanIFlowCLI(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "iflow-cli", shapeURLOrCommandEntry)
}

func scanJunie(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "junie", shapeURLOrCommandEntry)
}

func scanKimiCodeCLI(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "kimi-code-cli", shapeURLOrCommandEntry)
}

func scanKode(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "kode", shapeURLOrCommandEntry)
}

func scanOna(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "ona", shapeURLOrCommandEntry)
}

func scanPi(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "pi", shapeAntigravityEntry)
}

func scanQoder(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "qoder", shapeURLOrCommandEntry)
}

func scanQoderCN(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "qoder-cn", shapeURLOrCommandEntry)
}

func scanRoo(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "roo", shapeURLOrCommandEntry)
}

func scanRovoDev(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "rovodev", shapeURLOrCommandEntry)
}

func scanTabnineCLI(entries map[string]*ScanEntry, path string) error {
	return scanMCPServersJSON(entries, path, "tabnine-cli", shapeURLOrCommandEntry)
}

// scanMCPServersJSON is the shared body for the JSON-family clients whose
// MCP map lives under the top-level `mcpServers` key (kiro, windsurf, cline,
// kilocode). The per-client url-shape difference is captured by the shaper
// callback, keeping the read/parse logic in one owner.
func scanMCPServersJSON(entries map[string]*ScanEntry, path, client string, shaper func(map[string]any) ClientEntry) error {
	return scanMCPServersJSONWithDecoder(entries, path, client, shaper, func(data []byte) []byte { return data })
}

func scanMCPServersJSONC(entries map[string]*ScanEntry, path, client string, shaper func(map[string]any) ClientEntry) error {
	return scanMCPServersJSONWithDecoder(entries, path, client, shaper, stripJSONCommentsAndTrailingCommas)
}

func scanMCPServersJSONWithDecoder(entries map[string]*ScanEntry, path, client string, shaper func(map[string]any) ClientEntry, prepare func([]byte) []byte) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(prepare(data), &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence[client] = shaper(raw)
	}
	return nil
}

// scanOpenCode reads OpenCode's config. Its MCP map lives under the top-level
// `mcp` key (not `mcpServers`) and a remote hub entry is
// `{"type":"remote","url":...}`. The url key is the standard `url`, so the
// generic url/command shaper recognises the loopback hub entry.
func scanOpenCode(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCP map[string]map[string]any `json:"mcp"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCP {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["opencode"] = shapeURLOrCommandEntry(raw)
	}
	return nil
}

// scanMimoCode reads MiMoCode's config FAITHFULLY (MiMoCode is an OpenCode
// fork using the top-level `mcp` map, but its config has three behaviors the
// generic OpenCode scan path does not handle):
//
//   - JSONC: the resolved config can be a commented/trailing-comma
//     `mimocode.jsonc` (the path resolver explicitly PREFERS it), which raw
//     encoding/json rejects with a scan-failing 500. Decode via the adapter's
//     JSONC-tolerant merged read (clients.MimoCodeMergedConfig).
//   - In-dir layer merge: mimocode.json + mimocode.jsonc in the resolved dir
//     are deep-merged (.jsonc wins) so a server defined in either layer is
//     visible. clients.MimoCodeMergedConfig is the single owner of that merge
//     (also used by the adapter's read path), and it honors explicit override
//     paths (a temp/test path is read verbatim, never recomputing the dir).
//   - Local command arrays + `enabled` flag: handled by shapeMimoCodeEntry.
//
// The merged map is decoded into the `mcp` section; each entry is shaped by
// shapeMimoCodeEntry (NOT the generic shapeURLOrCommandEntry, which only reads
// a string `command` and ignores `enabled:false`).
func scanMimoCode(entries map[string]*ScanEntry, path string) error {
	merged, err := clients.MimoCodeMergedConfig(path)
	if err != nil {
		return err
	}
	servers, _ := merged["mcp"].(map[string]any)
	for name, rawAny := range servers {
		raw, ok := rawAny.(map[string]any)
		if !ok {
			continue
		}
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["mimocode"] = shapeMimoCodeEntry(raw)
	}
	return nil
}

// shapeMimoCodeEntry classifies a MiMoCode `mcp` entry FAITHFULLY:
//
//   - enabled:false → an ABSENT presence (Transport "absent"). MiMoCode uses
//     `enabled` (default true) as the active flag; a disabled entry is one
//     MiMoCode will NOT load. Recording it as active http/stdio would let the
//     matrix show a disabled hub entry as connected `via-hub`, or a disabled
//     local entry as a migrate candidate, and let Apply re-enable/clobber a
//     server the operator intentionally disabled. The frontend's routing
//     treats an "absent" transport as not-installed/disabled and never
//     promotes it to "available" off config_presence:"ok" (routing.ts Finding
//     4 clobber-protection). A missing/true `enabled` is active.
//   - remote (`url`) → http (classify() then tags a loopback url as via-hub).
//   - local (`command` ARRAY ["npx","-y",...]) → stdio with the real command
//     in Endpoint (first array element) and the full argv preserved in Raw, so
//     Discovery shows the executable instead of an empty "Unknown stdio" and
//     extract can recover command+args. A string `command` is also accepted
//     (defensive). The shaper never delegates the array case to the shared
//     string-only shapeURLOrCommandEntry.
//
// For the command-ARRAY case the stored Raw is NORMALIZED (bot PR #420 finding
// 6): MiMoCode keeps LSP tokens (`--lsp go`) in the `command` ARRAY, but the
// downstream LSP classifier (classifyLSPEntries / extractLSPLanguageFromArgs)
// reads them from `Raw["args"]`. Without normalization a non-canonical mimo
// mcp-language-server entry (whose name does not follow the canonical shape)
// could not be identified as a direct LSP legacy/conflict. The normalized Raw
// carries `args` = command-array tail ++ entry's own `args` (mirroring the
// adapter's mimoCodeNormalizeCommandArrays) so the args-reverse-lookup finds
// `--lsp <bin>` (and the gopls `mcp` arg). extract does NOT consume this Raw (it
// re-reads MimoCodeMergedConfig directly), so normalizing here is safe.
func shapeMimoCodeEntry(raw map[string]any) ClientEntry {
	if enabled, present := raw["enabled"]; present {
		if b, ok := enabled.(bool); ok && !b {
			return ClientEntry{Transport: "absent", Raw: raw}
		}
	}
	if url, ok := raw["url"].(string); ok && url != "" {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	// Local stdio: MiMoCode stores `command` as an ARRAY. Surface the executable
	// (first element) as the endpoint; the normalized argv goes in Raw so the LSP
	// args-reverse-lookup sees the `--lsp`/`mcp` tokens the command array carries.
	if cmd, tail := mimoCodeCommandArray(raw); cmd != "" {
		return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: mimoCodeRawWithNormalizedArgs(raw, tail)}
	}
	// Defensive: a string command (non-canonical but harmless) still classifies
	// as stdio with that command as the endpoint.
	if cmd, ok := raw["command"].(string); ok && cmd != "" {
		return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
	}
	return ClientEntry{Transport: "absent", Raw: raw}
}

// mimoCodeRawWithNormalizedArgs returns a shallow copy of a MiMoCode local
// entry's raw map with `args` rewritten to (command-array tail ++ entry's own
// `args`) so the LSP classifier's args-reverse-lookup (which reads Raw["args"])
// finds the `--lsp <bin>` / `mcp` tokens MiMoCode stores in the `command` ARRAY.
// The original map is not mutated; `command` is left as the original array (no
// downstream consumer of this scan-side Raw reads it — extract re-reads the
// adapter merge directly). Mirrors the adapter's mimoCodeNormalizeCommandArrays
// (tail PREPENDED to existing args).
func mimoCodeRawWithNormalizedArgs(raw map[string]any, cmdTail []string) map[string]any {
	out := make(map[string]any, len(raw)+1)
	for k, v := range raw {
		out[k] = v
	}
	merged := make([]any, 0, len(cmdTail))
	for _, a := range cmdTail {
		merged = append(merged, a)
	}
	if existing, ok := raw["args"].([]any); ok {
		merged = append(merged, existing...)
	}
	out["args"] = merged
	return out
}

// mimoCodeCommandArray reads a MiMoCode local entry's `command` ARRAY
// (["npx","-y","pkg"]) and splits it into (executable, args). Returns ("", nil)
// when `command` is absent or not an array (a string command is handled by the
// caller). Non-string array elements are skipped.
func mimoCodeCommandArray(raw map[string]any) (string, []string) {
	arr, ok := raw["command"].([]any)
	if !ok || len(arr) == 0 {
		return "", nil
	}
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

// scanHermes reads Hermes' ~/.hermes/config.yaml. Its MCP map lives under the
// top-level YAML `mcp_servers` key and a hub entry carries a `url`. Hermes
// reads many unrelated settings from the same file, so only the mcp_servers
// section is consulted.
func scanHermes(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCPServers map[string]map[string]any `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCPServers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["hermes"] = shapeURLOrCommandEntry(raw)
	}
	return nil
}

// scanOpenClaw reads OpenClaw's config. Its MCP map is NESTED two levels
// under `mcp.servers` (not a single top-level key) and a hub entry carries a
// `url`. The nested object is read defensively so a config missing the `mcp`
// object or its `servers` child simply yields no entries.
func scanOpenClaw(entries map[string]*ScanEntry, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		MCP struct {
			Servers map[string]map[string]any `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	for name, raw := range cfg.MCP.Servers {
		e := entries[name]
		if e == nil {
			e = &ScanEntry{ClientPresence: map[string]ClientEntry{}}
			entries[name] = e
		}
		e.ClientPresence["openclaw"] = shapeURLOrCommandEntry(raw)
	}
	return nil
}

// shapeURLOrCommandEntry classifies a JSON-family entry whose remote endpoint
// is under the standard `url` key (kiro, cline, kilocode, opencode, hermes,
// openclaw). Mirrors shapeCursorEntry/shapeClaudeEntry: a `url` value →
// http transport; otherwise a `command` value → stdio. classify() then
// recognises a loopback `url` as the hub binding (IsHubHTTPURL).
func shapeURLOrCommandEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

// shapeWindsurfEntry classifies a Windsurf entry. Windsurf names the
// remote-HTTP endpoint field `serverUrl` (its adapter writes that key); it
// also accepts the standard `url`. Either loopback value is recognised by
// classify() as the hub binding.
func shapeWindsurfEntry(raw map[string]any) ClientEntry {
	if url, ok := raw["serverUrl"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	if url, ok := raw["url"].(string); ok {
		return ClientEntry{Transport: "http", Endpoint: url, Raw: raw}
	}
	cmd, _ := raw["command"].(string)
	return ClientEntry{Transport: "stdio", Endpoint: cmd, Raw: raw}
}

// readManifestNames returns the set of available server names.
// Empty dir selects the production path (embedded manifests union
// on-disk defaultManifestDir). A non-empty dir restricts to that
// directory only — used by tests to inject a hermetic manifest set.
func readManifestNames(dir string) (map[string]bool, error) {
	names := map[string]bool{}
	// add registers a manifest name UNLESS it is a kind=companion manifest. A
	// companion is a hub-managed NON-MCP process (e.g. the excalidraw canvas) and
	// must never appear as an MCP server in classify / the Servers matrix /
	// via-hub detection. This is the single SOURCE-FILTER the companion design
	// relies on (routing/install/migrate sink-guards are defense-in-depth) —
	// excluding it here removes it from every scan-derived MCP surface at once. A
	// manifest that fails to load is kept (existing behavior — a malformed
	// manifest still surfaces as a name); only a successfully-parsed companion is
	// filtered out.
	add := func(n string) {
		if m, err := loadManifestForServer(dir, n); err == nil && m != nil && m.Kind == config.KindCompanion {
			return
		}
		names[n] = true
	}
	if dir == "" {
		list, err := listManifestNamesEmbedFirst()
		if err != nil {
			return nil, err
		}
		for _, n := range list {
			add(n)
		}
		return names, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return names, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.yaml")); err == nil {
			add(e.Name())
		}
	}
	return names, nil
}

// manifestDaemonPorts returns the set of daemon ports declared by the
// server's manifest. Empty dir → embed-first production path; a non-empty
// dir reads only that directory (test fixtures). Returns nil on any
// load/parse error (missing manifest, unknown server) so callers treat
// "no known ports" as "no loopback entry can match" → such an entry
// classifies external, not via-hub. The returned set drives the
// port-aware via-hub gate in classify() and is surfaced on
// ScanEntry.DaemonPorts so the frontend matrix mirrors the same rule.
func manifestDaemonPorts(dir, name string) []int {
	m, err := loadManifestForServer(dir, name)
	if err != nil || m == nil {
		return nil
	}
	var ports []int
	seen := map[int]bool{}
	for _, d := range m.Daemons {
		if d.Port > 0 && !seen[d.Port] {
			seen[d.Port] = true
			ports = append(ports, d.Port)
		}
	}
	return ports
}

// loopbackEntryPort parses the TCP port out of a hub-shaped loopback URL
// (`http://localhost:<port>/...`, `http://127.0.0.1:<port>/...`,
// `http://[::1]:<port>/...`). Returns (port, true) only when the endpoint
// IsHubHTTPURL AND a numeric port is present. A loopback URL with no
// explicit port (would default to 80) yields (0, false) — that cannot be
// a hub daemon binding, so it is treated as a non-matching loopback (→
// external), never via-hub. Callers MUST gate on IsHubHTTPURL before
// trusting a true result for hub classification.
func loopbackEntryPort(endpoint string) (int, bool) {
	if !clients.IsHubHTTPURL(endpoint) {
		return 0, false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return 0, false
	}
	portStr := u.Port()
	if portStr == "" {
		return 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}

// loopbackPortMatchesDaemon reports whether a hub-shaped loopback endpoint
// targets one of THIS server's manifest daemon ports. This is the
// load-bearing port-aware gate: a loopback-http entry is "via-hub" only
// when its URL port matches a declared daemon port. A loopback entry at a
// non-matching port (stale migration pointing at another server's daemon,
// or no manifest at all) is NOT hub-managed for this server — it is an
// external/unmanaged remote.
func loopbackPortMatchesDaemon(endpoint string, daemonPorts []int) bool {
	port, ok := loopbackEntryPort(endpoint)
	if !ok {
		return false
	}
	for _, p := range daemonPorts {
		if p == port {
			return true
		}
	}
	return false
}

func classify(e *ScanEntry, name string, manifestNames map[string]bool, daemonPorts []int, guiPort int) string {
	if perSessionServers[name] {
		return "per-session"
	}
	hasHub := false
	hasStdio := false
	// hasRemoteExternal: at least one client routes this server through a
	// NON-hub remote HTTP endpoint (a real external remote MCP — e.g.
	// context7 -> mcp.context7.com, qt-docs -> qt.io). These are http
	// transport whose URL is NOT a hub loopback (IsHubHTTPURL is false).
	// Pre-fix such an entry, having neither hub nor stdio presence, hit the
	// final "not-installed" branch and the Discovery (ex-Migration) screen
	// DROPPED it (migration-grouping.ts default case), so the operator's
	// real external remotes vanished from "see ALL MCP servers". We now
	// classify them as "external" so they surface under the Unmanaged /
	// External group while staying read-only (not hub-managed, not a
	// migrate candidate).
	//
	// Port-aware via-hub (security review): a loopback-http entry counts as
	// hub-managed ONLY when its URL port matches one of THIS server's
	// manifest daemon ports. IsHubHTTPURL is a pure loopback-SHAPE test
	// (no port check) — so a stale-port entry like fetch at
	// http://localhost:9121/mcp (when fetch's daemon is 9133 and 9121 is
	// serena's port) was wrongly shown as a clean green "via-hub" cell that
	// the matrix would offer to overwrite/remove. A loopback entry whose
	// port does NOT match any of the server's daemon ports is treated
	// exactly like a non-loopback remote https entry → "external"
	// (unmanaged remote), so it surfaces read-only in the Unmanaged /
	// External group instead of masquerading as a correct migration.
	hasRemoteExternal := false
	for _, c := range e.ClientPresence {
		if c.Transport == "http" && clients.IsHubHTTPURL(c.Endpoint) {
			switch {
			case IsSerenaServer(name) && IsLiveSerenaRouterURL(c.Endpoint, guiPort):
				// serena's canonical client URL is the /serena/mcp router on the
				// LIVE GUI port — recognize it as via-hub regardless of the
				// manifest's legacy per-daemon port (9121). Without this, a
				// correctly-routed serena entry parses its port (the GUI port) as
				// not-a-daemon-port and is misclassified as external, so the matrix
				// shows a connected serena as not-connected (serena-client-revert-
				// on-manifest-sync read-side).
				hasHub = true
			case IsSerenaServer(name) && IsSerenaRouterURL(c.Endpoint):
				// serena router SHAPE but NOT on the live GUI port → a STALE router
				// URL (e.g. the GUI previously ran on 9121 and later moved). Classify
				// it external HERE, BEFORE the daemon-port fallback below — otherwise
				// loopbackPortMatchesDaemon would match serena's legacy 9121 manifest
				// daemon and re-flag the dead router URL as via-hub, hiding it instead
				// of letting Apply rewrite it to the live port (#379 r5).
				hasRemoteExternal = true
			case loopbackPortMatchesDaemon(c.Endpoint, daemonPorts):
				hasHub = true
			default:
				// Loopback shape but wrong/absent port for this server's
				// daemons (stale migration, or operator's own local server).
				// Not hub-managed — classify as an external remote so it
				// surfaces read-only rather than as a deceptive green cell.
				hasRemoteExternal = true
			}
		}
		if c.Transport == "http" && !clients.IsHubHTTPURL(c.Endpoint) {
			// Non-hub remote HTTP endpoint = a genuine external remote MCP.
			hasRemoteExternal = true
		}
		if c.Transport == "relay" {
			if IsSerenaServer(name) {
				// Serena relay entries must target the LIVE /serena/mcp router.
				// A stale or absent relay --url should remain re-migratable rather
				// than looking hub-managed while the client dials a dead GUI port.
				if IsLiveSerenaRouterURL(c.RelayURL, guiPort) {
					hasHub = true
				} else {
					hasRemoteExternal = true
				}
			} else {
				// Antigravity's hub-routed shape: the hub rewrites Antigravity
				// bindings into a relay command (mcphub binary + args[0]=="relay").
				// scan.go:310 flags this as Transport: "relay". Without this branch
				// hub-routed Antigravity servers fall to "not-installed" and the
				// Migration screen drops them, hiding a real demigrate candidate.
				// (PR #4 Codex R1.)
				hasHub = true
			}
		}
		if c.Transport == "stdio" {
			hasStdio = true
		}
	}
	if hasHub && !hasStdio {
		return "via-hub"
	}
	if hasStdio && manifestNames[name] {
		return "can-migrate"
	}
	if hasStdio {
		return "unknown"
	}
	// A client-present non-hub remote with no stdio presence is a real
	// external MCP server — surface it (not "not-installed"). Ordered AFTER
	// the hub/stdio branches so a hub-routed or migratable entry keeps its
	// richer status; only an entry whose ONLY presence is a non-hub remote
	// reaches here.
	if hasRemoteExternal {
		return "external"
	}
	// "not-installed" is now reserved for rows with ZERO actionable client
	// presence — chiefly the manifest-only pass in ScanFrom (a server known
	// via a manifest but referenced by no client config), so the matrix row
	// does not vanish. Such rows have an empty ClientPresence and thus set
	// none of the flags above.
	return "not-installed"
}

// Scan is the production entry point: it resolves client config paths from
// OS defaults and calls ScanFrom.
//
// §9.2 drift-prevention: the per-client config-path set is DERIVED from the
// canonical registry (clients.SupportedClientNames() +
// clients.ConfigPathForName) via DefaultScanConfigPaths, NOT a hand-listed
// set of named ScanOpts fields. A new registry client is therefore scanned
// + presence-probed automatically with ZERO edits here — closing the drift
// class where a registry client without a named ScanOpts field was invisible
// to scan.
func (a *API) Scan() (*ScanResult, error) {
	// Preserve the legacy contract: a home-dir resolution failure fails the
	// whole Scan rather than silently yielding an empty result. (Each
	// ConfigPathForName call below also resolves home, but failing fast here
	// keeps the error surface identical to the pre-refactor Scan.)
	if _, err := os.UserHomeDir(); err != nil {
		return nil, err
	}
	return a.ScanFrom(ScanOpts{
		ConfigPaths: DefaultScanConfigPaths(),
		// Empty ManifestDir → ScanFrom uses the embed-first resolution
		// path. The on-disk defaultManifestDir stays available as a
		// secondary source for dev-checkout scenarios where a freshly-
		// added manifest hasn't been compiled into the binary yet.
		ManifestDir: "",
	})
}

// DefaultScanConfigPaths returns the canonical client-id → OS-default
// config-path map for every client in clients.SupportedClientNames(),
// resolved through clients.ConfigPathForName — the SAME resolver the
// CLI/install/GUI write surface uses, so the scan surface and the write
// surface always agree on the on-disk location. A client whose resolver
// errors (e.g. unresolvable home dir) is OMITTED from the map; ScanFrom
// then skips it, identical to the legacy mustClientConfigPath("") behavior.
//
// This is the single point through which a freshly-registered client gains
// scan/probe coverage: it lands in SupportedClientNames(), gets a path
// here, and (if it also has a clientScanners entry) is scanned — all
// without touching ScanOpts or ScanFrom.
//
// Exported so the CLI scan/migrate callers derive the identical set rather
// than maintaining their own parallel hand-listed copy (the original §9.2
// drift had THREE such copies: api.Scan, cli scan, cli migrate).
func DefaultScanConfigPaths() map[string]string {
	out := map[string]string{}
	for _, name := range clients.SupportedClientNames() {
		if path, err := clients.ConfigPathForName(name); err == nil && path != "" {
			out[name] = path
		}
	}
	return out
}

// defaultManifestDir returns the path to `servers/` resolved against the
// running binary's location. Two layouts supported:
//   - binary and servers/ in the same directory (legacy / standalone install)
//   - binary in bin/ and servers/ in bin/../ (standard Go project layout)
//
// If neither exists, returns the sibling path (exeDir/servers) without
// consulting the current working directory. This avoids untrusted CWD-based
// manifest resolution while preserving a deterministic on-disk location.
func defaultManifestDir() string {
	exe, err := os.Executable()
	if err != nil {
		// Fail closed to a deterministic, clearly invalid absolute-ish path
		// rather than a CWD-relative location.
		return filepath.Join(string(os.PathSeparator), "nonexistent", "mcphub", "servers")
	}
	exeDir := filepath.Dir(exe)
	// Legacy: exe and servers/ at same level.
	sibling := filepath.Join(exeDir, "servers")
	if st, err := os.Stat(sibling); err == nil && st.IsDir() {
		return sibling
	}
	// Standard: exe in bin/, servers/ one level up.
	parent := filepath.Join(exeDir, "..", "servers")
	if st, err := os.Stat(parent); err == nil && st.IsDir() {
		return parent
	}
	// Last resort: deterministic path near the executable (not CWD).
	return sibling
}

// ExtractManifestFromClient reads a stdio entry from the specified client
// config and renders a draft manifest.yaml suitable for the GUI "Create
// manifest" flow. The draft includes bindings for every supported managed
// client; users edit as desired before saving.
func (a *API) ExtractManifestFromClient(client, serverName string, opts ScanOpts) (string, error) {
	var raw map[string]any

	switch client {
	case "claude-code":
		if opts.ClaudeConfigPath == "" {
			return "", fmt.Errorf("ClaudeConfigPath empty")
		}
		data, err := os.ReadFile(opts.ClaudeConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]

	case "codex-cli":
		if opts.CodexConfigPath == "" {
			return "", fmt.Errorf("CodexConfigPath empty")
		}
		data, err := os.ReadFile(opts.CodexConfigPath)
		if err != nil {
			return "", err
		}
		var root map[string]any
		if err := toml.Unmarshal(data, &root); err != nil {
			return "", err
		}
		servers, _ := root["mcp_servers"].(map[string]any)
		if servers != nil {
			raw, _ = servers[serverName].(map[string]any)
		}

	case "cursor":
		if opts.CursorConfigPath == "" {
			return "", fmt.Errorf("CursorConfigPath empty")
		}
		data, err := os.ReadFile(opts.CursorConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]

	case "vscode":
		if opts.VSCodeConfigPath == "" {
			return "", fmt.Errorf("VSCodeConfigPath empty")
		}
		data, err := os.ReadFile(opts.VSCodeConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			Servers map[string]map[string]any `json:"servers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.Servers[serverName]

	case "gemini-cli":
		if opts.GeminiConfigPath == "" {
			return "", fmt.Errorf("GeminiConfigPath empty")
		}
		data, err := os.ReadFile(opts.GeminiConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]

	case "qwen-cli":
		if opts.QwenConfigPath == "" {
			return "", fmt.Errorf("QwenConfigPath empty")
		}
		data, err := os.ReadFile(opts.QwenConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]

	case "antigravity":
		if opts.AntigravityConfigPath == "" {
			return "", fmt.Errorf("AntigravityConfigPath empty")
		}
		data, err := os.ReadFile(opts.AntigravityConfigPath)
		if err != nil {
			return "", err
		}
		var cfg struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", err
		}
		raw = cfg.MCPServers[serverName]
		// Antigravity entries written by mcphub migrate use command=mcphub,
		// args[0]="relay". Extracting a manifest from THAT would loop:
		// manifest → install → relay entry → manifest → ... Reject narrowly:
		// command must be the mcphub binary AND args[0] must equal "relay".
		// A user's genuine stdio server whose first arg happens to be "relay"
		// but whose command is not mcphub passes through unchanged. Uses the
		// shared clients.IsMcphubBinary helper (also used by RestoreEntryFromBackup
		// for hub-relay detection in adapter defensive checks).
		if raw != nil {
			cmd, _ := raw["command"].(string)
			if clients.IsMcphubBinary(cmd) {
				if args, ok := raw["args"].([]any); ok && len(args) > 0 {
					if first, ok := args[0].(string); ok && first == "relay" {
						return "", fmt.Errorf("entry %q is a mcphub-managed relay stdio (command is mcphub binary + args[0]==\"relay\") — not user-configured stdio, cannot extract a manifest from it", serverName)
					}
				}
			}
		}

	case "mimocode":
		if opts.MimoCodeConfigPath == "" {
			return "", fmt.Errorf("MimoCodeConfigPath empty")
		}
		// MiMoCode's config is JSONC and split across mimocode.json +
		// mimocode.jsonc layers; reuse the adapter's merged read so a commented
		// .jsonc and a split-layer local entry are both visible. The explicit
		// path is honored verbatim (never recomputes the real ~/.config/mimocode
		// dir) by the same layer resolver the scan/adapter use.
		merged, err := clients.MimoCodeMergedConfig(opts.MimoCodeConfigPath)
		if err != nil {
			return "", err
		}
		servers, _ := merged["mcp"].(map[string]any)
		if servers != nil {
			raw, _ = servers[serverName].(map[string]any)
		}
		if raw == nil {
			return "", fmt.Errorf("server %q not found in client %q config", serverName, client)
		}
		// MiMoCode local entries use a `command` ARRAY (["npx","-y",...]) and
		// store env under `environment` (NOT `env`). Translate both here and
		// render directly — the generic string-`command`/`env` tail below does
		// NOT understand either, so a fall-through would drop the env vars and
		// emit an empty command. A remote/HTTP entry (no command array) is
		// rejected with the same demigrate guidance as the shared tail.
		cmd, args := mimoCodeCommandArray(raw)
		if cmd == "" {
			// No usable local command array — either a remote/hub entry or a
			// (non-canonical) string command. Fall through to the shared tail,
			// which classifies a string `command` and rejects HTTP-only entries
			// with actionable demigrate guidance.
			break
		}
		// A MiMoCode entry may carry a SEPARATE `args` array alongside the
		// `command` array (e.g. command:["mcp-language-server"], args:["--lsp",
		// "go"]). The command-array tail above drops it, so append it after the
		// tail — matching the adapter's mimoCodeNormalizeCommandArrays
		// (command-array tail PREPENDED to the entry's own args), so the
		// generated manifest starts the full command line (bot PR #420 finding 4).
		if extraArgs, ok := raw["args"].([]any); ok {
			for _, v := range extraArgs {
				if s, ok := v.(string); ok {
					args = append(args, s)
				}
			}
		}
		envMap := map[string]string{}
		if envAny, ok := raw["environment"].(map[string]any); ok {
			for k, v := range envAny {
				if s, ok := v.(string); ok {
					envMap[k] = s
				}
			}
		}
		port, err := pickNextFreePort(opts.ManifestDir)
		if err != nil {
			return "", err
		}
		return renderDraftManifestYAML(serverName, cmd, args, envMap, port), nil

	default:
		return "", fmt.Errorf("extract not yet supported for client %q (extend here when needed)", client)
	}
	if raw == nil {
		return "", fmt.Errorf("server %q not found in client %q config", serverName, client)
	}

	cmd, _ := raw["command"].(string)
	// Reject HTTP-only / hub-managed entries early. Extract is for stdio
	// servers; an entry that has no `command` cannot produce a valid
	// manifest (renderDraftManifestYAML would emit an empty `command:`
	// line and ServerManifest.Validate would then fail with a less
	// actionable error). The most common case is a user trying to
	// extract from a server they already migrated — the entry is now
	// hub-HTTP (HTTP-native clients) or hub-relay with empty-command
	// downgrades — so we guide them toward demigrate instead.
	if cmd == "" {
		return "", fmt.Errorf("server %q in client %q has no `command` field — it is an HTTP-only or hub-managed entry, not user-configured stdio (run `mcphub demigrate %s` to restore the pre-migrate shape first if this server was migrated)", serverName, client, serverName)
	}
	var args []string
	if arr, ok := raw["args"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				args = append(args, s)
			}
		}
	}
	envMap := map[string]string{}
	if envAny, ok := raw["env"].(map[string]any); ok {
		for k, v := range envAny {
			if s, ok := v.(string); ok {
				envMap[k] = s
			}
		}
	}

	// Pick next free port in 9121-9139 range not already used by other manifests.
	port, err := pickNextFreePort(opts.ManifestDir)
	if err != nil {
		return "", err
	}

	return renderDraftManifestYAML(serverName, cmd, args, envMap, port), nil
}

func pickNextFreePort(manifestDir string) (int, error) {
	used := map[int]bool{}
	entries, _ := os.ReadDir(manifestDir)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(manifestDir, e.Name(), "manifest.yaml"))
		if err != nil {
			continue
		}
		// Minimal YAML scrape — we do not want to pull go-yaml just for this.
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			const p = "port:"
			if strings.HasPrefix(line, p) {
				var n int
				fmt.Sscanf(line, "port: %d", &n)
				if n > 0 {
					used[n] = true
				}
			}
		}
	}
	for p := 9121; p <= 9139; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in 9121-9139 range")
}

func renderDraftManifestYAML(name, cmd string, args []string, env map[string]string, port int) string {
	doc := struct {
		Name           string            `yaml:"name"`
		Kind           string            `yaml:"kind"`
		Transport      string            `yaml:"transport"`
		Command        string            `yaml:"command"`
		BaseArgs       []string          `yaml:"base_args,omitempty"`
		Env            map[string]string `yaml:"env,omitempty"`
		Daemons        []map[string]any  `yaml:"daemons"`
		ClientBindings []map[string]any  `yaml:"client_bindings"`
		WeeklyRefresh  bool              `yaml:"weekly_refresh"`
	}{
		Name:      name,
		Kind:      "global",
		Transport: "stdio-bridge",
		Command:   cmd,
		BaseArgs:  args,
		Env:       env,
		Daemons: []map[string]any{
			{"name": "default", "port": port},
		},
		// Derive the draft client_bindings from the canonical client registry
		// (clients.SupportedClientNames) so the preview never drifts from the
		// adapters this build actually supports — the original seven plus the
		// eight opt-in wave-2 clients. Previously these were hardcoded to the
		// seven core ids, so a GUI-extracted draft silently omitted bindings
		// for zed/kiro/windsurf/cline/kilocode/opencode/hermes/openclaw.
		ClientBindings: draftClientBindings(),
		WeeklyRefresh:  false,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		// yaml.Marshal on this static structure should not fail; fallback keeps behavior.
		return ""
	}
	return string(out)
}

// draftClientBindings returns one client_binding row per supported client,
// derived from the canonical clients.SupportedClientNames() registry so the
// extract-draft preview stays in lockstep with the adapters this build wires
// up (core + opt-in wave-2). Each row targets the single "default" daemon at
// url_path "/mcp" — the same shape the GUI binding editor emits. Deriving from
// the registry (instead of a hardcoded list) means a future adapter addition
// automatically appears in the draft with no second edit site to forget.
func draftClientBindings() []map[string]any {
	names := clients.SupportedClientNames()
	bindings := make([]map[string]any, 0, len(names))
	for _, name := range names {
		bindings = append(bindings, map[string]any{
			"client":   name,
			"daemon":   "default",
			"url_path": "/mcp",
		})
	}
	return bindings
}

// lspLanguages enumerates the canonical language tokens recognised by
// classifyLSPEntries when reverse-parsing the `mcp-language-server-<lang>`
// entry-name shape produced by ResolveEntryName. Kept as a package-level var
// so future plan items (Task 3.x) can extend it without touching ScanFrom.
// Order does not matter — ParseEntryName uses longest-prefix matching.
var lspLanguages = []string{
	"clangd",
	"fortran",
	"go",
	"javascript",
	"python",
	"rust",
	"typescript",
	"vscode-css",
	"vscode-html",
}

// lspCommandToLanguage reverses the manifest mapping (LanguageSpec.lsp_command
// → LanguageSpec.name) for stdio entries whose entry-name does not itself
// match `mcp-language-server-<lang>`. Required because direct-stdio rows
// often carry operator-chosen names (e.g. "rust-langserver-direct") that lack
// the canonical prefix; the only reliable language signal in that case is the
// `--lsp <binary>` arg + the command basename. typescript-language-server is
// intentionally absent — both javascript and typescript map to it
// (servers/mcp-language-server/manifest.yaml:42,59) so reverse-lookup is
// ambiguous; the algorithm falls back to entry-name parsing for that case
// per the spec's "language is determined by entry NAME" rule.
var lspCommandToLanguage = map[string]string{
	"clangd":                      "clangd",
	"fortls":                      "fortran",
	"gopls":                       "go",
	"pyright-langserver":          "python",
	"rust-analyzer":               "rust",
	"vscode-css-language-server":  "vscode-css",
	"vscode-html-language-server": "vscode-html",
}

// extractLSPLanguageFromArgs inspects a direct-stdio ClientEntry whose
// command basename is mcp-language-server and returns the recognised
// language (or "" when not deducible). The args slice carries the lsp
// binary as `--lsp <bin>`; the bin reverse-maps via lspCommandToLanguage.
// Returns ("") for unambiguous binaries (typescript-language-server)
// since the spec requires those to be resolved from entry-name only.
func extractLSPLanguageFromArgs(raw map[string]any) string {
	args, ok := raw["args"].([]any)
	if !ok {
		return ""
	}
	for i := 0; i < len(args); i++ {
		s, _ := args[i].(string)
		if s != "--lsp" {
			continue
		}
		if i+1 >= len(args) {
			return ""
		}
		bin, _ := args[i+1].(string)
		bin = filepath.Base(strings.TrimSuffix(bin, ".exe"))
		if lang, ok := lspCommandToLanguage[bin]; ok {
			return lang
		}
		return ""
	}
	return ""
}

// classifyLSPEntries is Task 3.3's three-rule recognition pass. For each
// entry produced by the per-client scanners, walk its ClientPresence map
// and tag rows whose name follows the canonical `mcp-language-server-<lang>`
// shape — OR whose stdio invocation reveals the language via `--lsp <bin>`
// reverse-lookup. Three classification rules — all additive on Status;
// coexistence also collapses dangling direct-stdio rows into LegacyConflict
// so the matrix shows the anomaly under a single (server, client) cell
// rather than two visually-disjoint rows.
//
// Rule 1 — hub-managed HTTP entry: transport=http, name matches
// `mcp-language-server-<lang>`. Mark Status = "via-hub".
//
// Rule 2 — direct-stdio mcp-language-server: transport=stdio, command
// basename (suffix-trimmed) equals "mcp-language-server". Recognise as
// legacy. Language comes from entry-name when the name follows the
// canonical shape; otherwise from `--lsp <bin>` args reverse-lookup.
//
// Rule 3 — gopls direct: transport=stdio, command basename "gopls",
// raw args contain "mcp". Recognise as Go legacy.
//
// Coexistence collapse: when a hub row AND a separate legacy stdio
// row both exist for the same language and the same client name,
// MOVE the stdio entry from its own row's ClientPresence to the hub
// row's LegacyConflict[clientName]. If the donor row's ClientPresence
// is empty after the move, prune the row entirely so the matrix
// renders one cell instead of two.
//
// Ownership disambiguation: when the workspace registry is available,
// each row's expected client→entry-name mapping is used as a soft
// sanity gate; the actual recognition labels still come from
// ParseEntryName / args-reverse-lookup so a missing or out-of-date
// registry degrades to language-labelling only.
func classifyLSPEntries(entries map[string]*ScanEntry, reg *Registry) {
	if len(entries) == 0 {
		return
	}

	// Pass 1: classify every row + collect the legacy-stdio donor index.
	// donors maps (clientName, language) -> []donor; we keep slices because
	// multiple stdio rows can legitimately exist (e.g. one canonical
	// mcp-language-server-<lang> stdio row plus an unrelated user row that
	// also happens to call mcp-language-server). Coexistence collapse only
	// fires when the receiving HUB row covers the same (clientName, lang)
	// pair; everything else stays where it is.
	type donor struct {
		entryName  string
		clientName string
		entry      ClientEntry
	}
	// hubsByPair stores a SLICE of matching hub rows because one client
	// can legitimately host multiple hub rows for the same language —
	// e.g. when two workspaces register clangd in codex-cli, each gets
	// its own `mcp-language-server-clangd-<wsKey4hex>` entry. Pre-fix
	// (bot review PR #222 P2 scan.go:1218) the index was a single
	// *ScanEntry, so later iterations overwrote earlier ones and a
	// stdio donor was attached to whichever hub row survived map
	// iteration — nondeterministic. We now attach the donor to EVERY
	// matching hub row so the matrix surfaces the coexistence anomaly
	// on every plausible parent, deterministically.
	hubsByPair := map[string][]*ScanEntry{} // key: clientName + "\x00" + lang
	donorsByPair := map[string][]donor{}    // same key
	legacyEntryNames := map[string]bool{}   // set of entries flagged as legacy donors

	for name, e := range entries {
		nameLang, _ := ParseEntryName(name, lspLanguages)
		for clientName, ce := range e.ClientPresence {
			// Determine the language for this row+client pair. Prefer
			// entry-name parsing (canonical, unambiguous when present);
			// fall back to args reverse-lookup for direct-stdio rows
			// whose names don't follow the canonical shape.
			lang := nameLang
			if lang == "" && ce.Transport == "stdio" {
				base := filepath.Base(strings.TrimSuffix(ce.Endpoint, ".exe"))
				if base == "mcp-language-server" {
					lang = extractLSPLanguageFromArgs(ce.Raw)
				} else if base == "gopls" {
					if args, ok := ce.Raw["args"].([]any); ok {
						for _, a := range args {
							if s, _ := a.(string); s == "mcp" {
								lang = "go"
								break
							}
						}
					}
				}
			}
			if lang == "" {
				continue
			}
			pairKey := clientName + "\x00" + lang
			switch {
			case ce.Transport == "http":
				// Rule 1: hub-managed.
				e.Status = "via-hub"
				hubsByPair[pairKey] = append(hubsByPair[pairKey], e)
			case ce.Transport == "stdio":
				base := filepath.Base(strings.TrimSuffix(ce.Endpoint, ".exe"))
				// Rule 2: direct-stdio mcp-language-server.
				if base == "mcp-language-server" {
					donorsByPair[pairKey] = append(donorsByPair[pairKey], donor{
						entryName: name, clientName: clientName, entry: ce,
					})
					legacyEntryNames[name] = true
					continue
				}
				// Rule 3: gopls stdio with "mcp" arg.
				if lang == "go" && base == "gopls" {
					if args, ok := ce.Raw["args"].([]any); ok {
						for _, a := range args {
							if s, _ := a.(string); s == "mcp" {
								donorsByPair[pairKey] = append(donorsByPair[pairKey], donor{
									entryName: name, clientName: clientName, entry: ce,
								})
								legacyEntryNames[name] = true
								break
							}
						}
					}
				}
			}
		}
	}

	// Registry soft-sanity gate: for ownership disambiguation, walk the
	// workspaces and confirm at least one workspace's ClientEntries maps
	// the receiving hub row's entry name back to the same client. The
	// gate is informational only — production scans without a workspace
	// registry must still produce the recognition labels, so a failed
	// lookup never blocks the coexistence collapse below.
	_ = reg // reserved for future hard-gate evolutions; see plan §"Matrix LSP recognition"

	// Pass 2: coexistence collapse. For each donor matching ANY hub row's
	// (clientName, lang) pair, attach the stdio entry to EVERY matching
	// hub row's LegacyConflict[clientName] and remove the donor entry
	// from its origin row's ClientPresence. Attaching to every matching
	// hub is the deterministic fix for the bot-review PR #222 P2
	// nondeterminism: the donor has no workspace context (its entry
	// name is the bare `mcp-language-server-<lang>` form), so any of
	// the multiple workspace-suffixed hub rows in the same (client, lang)
	// pair could be its parent — surfacing the conflict on all of them
	// keeps the matrix honest about the ambiguity rather than silently
	// picking one based on map iteration order.
	//
	// Hubs within a pair are sorted by entry name so the LegacyConflict
	// assignment order is stable across runs (the value is the same
	// either way; sort only matters if a future refactor depends on
	// iteration order).
	for pairKey, ds := range donorsByPair {
		hubs, ok := hubsByPair[pairKey]
		if !ok || len(hubs) == 0 {
			continue
		}
		sort.Slice(hubs, func(i, j int) bool { return hubs[i].Name < hubs[j].Name })
		// Sort donors deterministically (closes bot PR#222 P2-6: when
		// multiple donors share the same clientName but different
		// entries, the previous code overwrote hub.LegacyConflict[d.clientName]
		// in donorsByPair-map iteration order, hiding all but the last
		// non-deterministically. Sorting by (clientName, entryName)
		// makes the surviving conflict entry reproducible — the
		// alphabetically-last entry for that client always wins.
		//
		// LIMITATION: this preserves ONE conflict entry per client, not
		// ALL of them. Preserving ALL would require changing
		// LegacyConflict to `map[string][]ClientEntry` which is a
		// public-API type change (types.go:103, JSON wire format,
		// downstream consumers). Tracked as a follow-up.
		sort.Slice(ds, func(i, j int) bool {
			if ds[i].clientName != ds[j].clientName {
				return ds[i].clientName < ds[j].clientName
			}
			return ds[i].entryName < ds[j].entryName
		})
		for _, d := range ds {
			for _, hub := range hubs {
				if hub.LegacyConflict == nil {
					hub.LegacyConflict = map[string]ClientEntry{}
				}
				hub.LegacyConflict[d.clientName] = d.entry
			}
			if donor, exists := entries[d.entryName]; exists {
				delete(donor.ClientPresence, d.clientName)
			}
		}
	}

	// Pass 3: prune any donor row whose ClientPresence was emptied by the
	// collapse. We keep rows that retain at least one client (the row may
	// also be present in another client's config), and we never prune a
	// row whose manifest exists on disk — those are matrix-visible by the
	// manifest-only pass in ScanFrom.
	for name := range legacyEntryNames {
		e, ok := entries[name]
		if !ok {
			continue
		}
		if len(e.ClientPresence) > 0 {
			continue
		}
		if e.ManifestExists {
			continue
		}
		delete(entries, name)
	}

	// Re-sync Managed against the FINAL Status. Pass 1 above can promote an
	// LSP-bridge row to Status == "via-hub" (line ~1592) AFTER ScanFrom's
	// main loop already set Managed from the pre-LSP classify() result, so
	// without this pass an LSP hub row would carry Status "via-hub" but
	// Managed false. Keeping Managed strictly derived from the final Status
	// (one owner) is the invariant the Discovery view's "Managed by hub"
	// badge relies on.
	for _, e := range entries {
		e.Managed = e.Status == "via-hub"
	}
}

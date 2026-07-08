package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/process"
)

// errOrphanOptsServerScanClientsConflict is returned by CleanupOrphans
// when both opts.Server and opts.ScanClientConfigs are set. Codex bot
// r3 P1 on PR #190: the two modes have no overlap — client stdio
// entries carry no manifest-server key — so mixing them would expand
// the kill-pattern set with cmdlines unrelated to the requested
// server. Operator must pick one mode.
var errOrphanOptsServerScanClientsConflict = errors.New(
	"cleanup: --scan-clients is incompatible with --server (client stdio entries carry no server-name key; pick one mode)",
)

// isOurOwnProcess returns true when the cmdline's executable token is one
// of our own binaries: mcphub.exe (running as daemon or any subcommand),
// the standalone godbolt / lldb-bridge / perftools exes, or the legacy
// mcp.exe name from early installations. Basename match, case-insensitive.
// Used by the orphan detector to skip processes that are running by
// design — their parent is typically the Task Scheduler service, so the
// parent-is-our-daemon heuristic alone cannot protect them.
//
// Codex deep-sec PR #143 round 4 finding A1: this function used to ship
// its own first-token parser (split at first space, fall through to
// filepath.Base). That was inconsistent with redactCmdlineForDisplay's
// shape-aware parser and would mis-classify a cmdline like
// `C:\Program Files\mcphub\mcphub.exe daemon --server gdb` — the naive
// split returned `Program` instead of `mcphub.exe`, so isOurOwnProcess
// returned false and parseOrphans would have flagged a real hub daemon
// as orphan and killed it. The shared helper below now performs the
// extension-lookup walk for both functions.
func isOurOwnProcess(cmdline string) bool {
	base := strings.ToLower(firstTokenBasename(cmdline))
	switch base {
	case "mcphub.exe", "mcphub",
		"mcp.exe", "mcp",
		"godbolt.exe", "godbolt",
		"lldb-bridge.exe", "lldb-bridge",
		"perftools.exe", "perftools":
		return true
	}
	return false
}

// OrphanProcess describes one orphan MCP subprocess discovered by CleanupOrphans.
// KillErr carries the per-candidate outcome text. On a kill attempt (DryRun=false,
// ReapVerdict=reap-eligible) it distinguishes an identity mismatch (PID recycled or
// process replaced since the census scan — the friendly-fire guard refused the kill),
// an access-denied refusal, an already-exited process, and a missing identity proof;
// it is empty on a clean kill. It is ALSO populated (on BOTH dry-run and apply) with a
// human "skipped: <reason>" note for a candidate the A2 PR5 reap-eligibility gate SPARED
// (config-referenced / config-scan-degraded / snapshot-degraded / below-kill-age-floor) —
// so a dry-run preview shows WHY a candidate would not be reaped. The structured reason
// is ReapVerdict; downstream consumers branch on KillErr != "" (an outcome exists), never
// on its message text.
//
// Wire-format note (Cleanup-6 security fix): the raw Cmdline is kept on
// the struct for server-side use (manifest-pattern match in CleanupOrphans,
// CLI display via `mcphub cleanup`, NeverKill enforcement) but is hidden
// from JSON output (`json:"-"`) because full command lines often carry
// workspace paths, username segments, file paths, and process arguments
// that may include API keys or tokens. The GUI/HTTP wire instead exposes
// `cmdline_display` populated by redactCmdlineForDisplay — basename of
// the executable only — which is enough for an operator to recognize
// the process while keeping sensitive context off-wire and out of
// browser dev-tools / screenshots.
type OrphanProcess struct {
	PID            int    `json:"pid"`
	ParentID       int    `json:"parent_pid"`
	Server         string `json:"server"` // inferred from matching manifest
	RAMBytes       uint64 `json:"ram_bytes"`
	Cmdline        string `json:"-"`               // server-side use only; redacted from wire
	CmdlineDisplay string `json:"cmdline_display"` // basename of executable for GUI display
	AgeSec         int64  `json:"age_sec"`
	KillErr        string `json:"kill_err,omitempty"`

	// ReapVerdict is the kill-eligibility outcome stamped by the H5-revised gate,
	// populated on BOTH dry-run and apply so a preview shows WHY a candidate would or
	// would not be reaped. It is a fixed enum label (never a path/cmdline), wire-safe.
	// bug 2026-07-04 npx-orphan-accumulation / A2 PR5.
	ReapVerdict string `json:"reap_verdict,omitempty"`

	// ExecutablePath is the process image path captured at census, and
	// StartedAt is the RFC3339Nano process start time (from the snapshot
	// CreationDate). Together with PID they form the PIDIdentityProof that
	// process.TerminatePIDWithIdentity re-verifies on a held handle at kill
	// time (see reapOrphans), so a PID recycled between the census scan and
	// the kill fails the proof and is left untouched — the PID-recycle
	// friendly-fire guard. Both are server-side only (json:"-"): the image
	// path can embed a username / workspace segment, same rationale as
	// Cmdline. Empty when the snapshot could not report a path → the proof
	// fails closed and the process is NOT killed.
	ExecutablePath string `json:"-"`
	StartedAt      string `json:"-"`

	// MatchSource explains why an AGGRESSIVE candidate was included:
	// the ancestor basename that anchored the scope (e.g. "codex") for
	// a --client run, or "root-pid <pid>" for a --root-pid run. Empty
	// for the default safe sweep. It is a redacted basename / fixed
	// label only — never a full cmdline — so it is wire-safe.
	MatchSource string `json:"match_source,omitempty"`
}

// Reap-eligibility verdicts stamped by CleanupOrphans's H5-revised gate (A2 PR5).
// A candidate is killed ONLY on ReapVerdictReapEligible; every other verdict SPARES it
// (fail-safe direction). The verdict is surfaced in the dry-run preview + the audit log.
const (
	ReapVerdictReapEligible             = "reap-eligible"
	ReapVerdictSparedConfigReferenced   = "spared-config-referenced"    // signature still in an installed client config
	ReapVerdictSparedConfigScanDegraded = "spared-config-scan-degraded" // an existing client config was unparseable → whole run fails closed
	ReapVerdictSparedSnapshotDegraded   = "spared-snapshot-degraded"    // the process snapshot was truncated → whole run fails closed
	ReapVerdictSparedBelowKillAgeFloor  = "spared-below-kill-age-floor" // younger than the kill-eligibility age floor
)

// cleanupKillMinAgeSec is the kill-eligibility age floor for the auto-reaper
// (design.md P2 "age>~10min"): a candidate is LISTED at the 60s default but not
// KILLED until it is this old, so a just-orphaned-mid-restart child gets several
// ticker re-evaluations on fresh snapshots before an auto-kill (A2 PR5).
const cleanupKillMinAgeSec = 600

// firstTokenBasename returns the basename (no directory prefix) of the
// FIRST token of a cmdline string — the executable path component before
// any arguments. Returns "" for empty/whitespace-only input.
//
// Codex deep-sec PR #143 round 4 finding A1: this helper exists so the
// orphan detector and the redaction pipeline share ONE shape-aware parser.
// Two divergent first-token parsers used to ship — `isOurOwnProcess` had
// a naive first-space split (so `C:\Program Files\mcphub\mcphub.exe ...`
// returned `Program` and could be classified as an orphan and killed),
// while `redactCmdlineForDisplay` already knew the WMIC-stripped Windows
// path shape. Unified here to eliminate the safety bug.
//
// Parsing rules:
//   - Empty / whitespace-only input: returns "".
//   - Opening double-quote: take everything between the matching quotes
//     (handles `"C:\Program Files\foo.exe" arg1`).
//   - Unterminated opening quote: take the rest of the string as the
//     path token (best-effort, no panic on malformed wmic output).
//   - First token contains a path separator AND a recognized Windows
//     executable extension (.exe/.com/.cmd/.bat/.ps1) precedes the
//     earliest whitespace: anchor on `<ext><whitespace>` so a path
//     with an embedded space (the WMIC-stripped `Program Files` case)
//     stays intact.
//   - Otherwise: first-whitespace split — handles bare-basename launchers
//     (`uvx mcp-server-time --cache C:\tmp\helper.exe` correctly returns
//     `uvx`, NOT `helper.exe` from a later argument).
//
// Codex deep-sec finding A3 / Q1: the whitespace separator set is
// ` \t\n\r` (NOT just space + tab). A cmdline like
// `node.exe\n--api-key=sk-secret` previously merged the entire string
// into the "first token" because `\n` was not a separator, and the
// returned basename would have leaked the API key into cmdline_display.
//
// Codex deep-sec finding S2: the result is capped at 256 bytes to keep
// pathological inputs (long unicode-decoded basename, etc.) from bloating
// JSON output and the orphan-table UI. Real exe basenames are <100 chars;
// the cap is forgiving but bounded.
func firstTokenBasename(cmdline string) string {
	c := strings.TrimSpace(cmdline)
	if c == "" {
		return ""
	}
	first := c
	if c[0] == '"' {
		if end := strings.IndexByte(c[1:], '"'); end > 0 {
			first = c[1 : 1+end]
		} else {
			// Unterminated quote → treat the rest as the path token.
			first = c[1:]
		}
	} else {
		// Whitespace separators include `\n` and `\r` so a cmdline like
		// `node.exe\n--api-key=sk-secret` cannot smuggle the API-key
		// suffix into the "first token" and through filepath.Base into
		// cmdline_display (codex deep-sec finding A3 / Q1).
		sp := strings.IndexAny(c, " \t\n\r")
		switch {
		case sp < 0:
			// No whitespace anywhere — single-token cmdline, the whole
			// thing is the executable path.
			first = c
		case strings.ContainsAny(c[:sp], `\/`):
			// First token has a path separator — could be a partial
			// Windows path with embedded spaces. The character right
			// after the first whitespace decides:
			//   - Flag-like (`-` or `/-` for old-style switches) → the
			//     path is COMPLETE before the whitespace; the rest is
			//     CLI arguments. Use first-whitespace as the boundary.
			//     Defends against extensionless-executable + later
			//     `.exe`-bearing argument case (codex bot PR #143
			//     round 5 P2: `C:\tools\python -m server --cache
			//     C:\tmp\helper.exe` used to return `helper.exe`
			//     because the extension scan ran over the entire
			//     cmdline; now flag detection terminates the path
			//     at `python`).
			//   - Anything else (path continuation like `Files\...`
			//     or non-flag arg) → the path may have embedded
			//     spaces (WMIC-stripped quotes case); try extension
			//     lookup; fall back to first-whitespace if no
			//     extension is found.
			if firstWhitespaceTerminatesPath(c, sp) {
				first = c[:sp]
			} else if extEnd := findWindowsExeExtensionEnd(c); extEnd > 0 {
				first = c[:extEnd]
			} else {
				first = c[:sp]
			}
		default:
			// First token is a bare basename (e.g. `uvx`, `python3`,
			// `node.exe`) — first-whitespace IS the boundary. Skip the
			// extension lookup so a later argument's `.exe` substring
			// cannot win.
			first = c[:sp]
		}
	}
	first = strings.TrimSpace(first)
	if first == "" {
		return ""
	}
	// filepath.Base handles both backslash and forward-slash separators on
	// Windows; on POSIX it splits on forward-slash only. The orphan
	// detector consumes Windows-shaped CommandLine values from CIM/wmic,
	// so we walk both separators ourselves to stay platform-independent.
	if i := strings.LastIndexAny(first, `\/`); i >= 0 {
		first = first[i+1:]
	}
	if first == "" {
		return ""
	}
	// Codex deep-sec finding S2: cap pathologically long basenames so a
	// rogue input cannot bloat the JSON wire / UI table. 256 bytes is
	// more than 2.5× the longest real exe basename ever observed.
	const cmdlineDisplayCap = 256
	if len(first) > cmdlineDisplayCap {
		// Reserve 3 bytes for the truncation marker so the UI shows the
		// "this was clipped" affordance.
		first = first[:cmdlineDisplayCap-3] + "..."
	}
	return first
}

// redactCmdlineForDisplay returns the executable's basename only, with no
// path and no arguments. Cleanup-6 security fix: full command lines often
// embed workspace paths (D:\dev\client-confidential-project\...), username
// segments (C:\Users\<name>\...), filesystem paths to private files, and
// process arguments that may contain API keys or tokens (--api-key=sk-...).
// The orphan-table UI in the GUI and any operator screenshots / browser
// dev-tools that observe the wire MUST see only the basename — the PID,
// inferred Server, RAM, and Age columns carry the operational info needed
// to confirm a kill.
//
// Behavior is delegated to firstTokenBasename; this thin wrapper just maps
// the empty-input sentinel to the UI-visible `<unknown>` string. Both
// `isOurOwnProcess` and `redactCmdlineForDisplay` consume the same parser
// to keep the orphan-allowlist gate consistent with the displayed name
// (codex deep-sec PR #143 round 4 finding A1).
func redactCmdlineForDisplay(cmdline string) string {
	first := firstTokenBasename(cmdline)
	if first == "" {
		return "<unknown>"
	}
	return first
}

// windowsExeExtensions enumerates the recognized Windows executable
// suffixes. Listed shortest-first so the loop terminates on the first
// match without overrunning into a longer suffix that happens to share
// a prefix. Case-insensitive comparison is performed by the caller.
var windowsExeExtensions = []string{".exe", ".com", ".cmd", ".bat", ".ps1"}

// findWindowsExeExtensionEnd returns the byte index immediately AFTER the
// first occurrence of a known Windows executable extension that is followed
// by either whitespace or end-of-string, or -1 if no such boundary exists.
// The lookup is case-insensitive: `.exe`, `.EXE`, `.Exe` all match.
//
// The function exists to handle WMIC-stripped quoted Windows paths where
// the quotes vanish but the path still contains an embedded space (the
// most common case is `C:\Program Files\...`). Splitting at the first
// space would put the boundary inside the path; splitting at the first
// `.exe<space>` keeps the executable intact.
//
// Codex bot PR #143 round 1 P2: prior implementation truncated
// `C:\Program Files\nodejs\node.exe -y server-memory` to `C:\Program`
// because the first space won. This helper finds `.exe ` and returns
// the index after the extension instead.
func findWindowsExeExtensionEnd(s string) int {
	lower := strings.ToLower(s)
	bestIdx := -1
	for _, ext := range windowsExeExtensions {
		// Walk every occurrence; pick the first one that is followed by
		// whitespace or end-of-string. Short-circuit at the earliest hit
		// so an extension inside a later argument cannot overshadow the
		// path's actual executable.
		searchFrom := 0
		for searchFrom < len(lower) {
			idx := strings.Index(lower[searchFrom:], ext)
			if idx < 0 {
				break
			}
			abs := searchFrom + idx
			end := abs + len(ext)
			// Boundary check: end-of-string OR next char is whitespace.
			// Newline/CR included alongside space/tab so a cmdline that
			// uses `\n` or `\r\n` between exe and args still anchors here
			// (codex deep-sec PR #143 round 4 finding A3 / Q1).
			if end == len(s) || s[end] == ' ' || s[end] == '\t' || s[end] == '\n' || s[end] == '\r' {
				if bestIdx < 0 || end < bestIdx {
					bestIdx = end
				}
				break
			}
			searchFrom = abs + 1
		}
	}
	return bestIdx
}

// firstWhitespaceTerminatesPath returns true when the character
// immediately after the first whitespace at index `sp` is a flag-like
// marker (`-`), meaning the executable path is complete BEFORE that
// whitespace and the rest is CLI arguments. Used by firstTokenBasename
// to decide whether to bother running the Windows extension lookup.
//
// Codex bot PR #143 round 5 P2: prior implementation always ran the
// full-cmdline extension scan whenever the first token contained a
// path separator. For an extensionless executable plus a later
// `.exe`-bearing argument (e.g. `C:\tools\python -m server --cache
// C:\tmp\helper.exe`), the scan anchored on the argument's `helper.exe`
// instead of the actual executable `python`. This helper short-circuits
// the scan when the first space is followed by a flag character, so
// the executable token stays at `c[:sp]` (`C:\tools\python`).
//
// We deliberately treat ONLY `-` as the flag marker; `/` is reserved
// for POSIX paths and old-style Windows switches (`/c`), but a `/` at
// position sp+1 is overwhelmingly more likely to be a POSIX path
// continuation (e.g. `node /usr/local/share/foo`) than a switch.
// Erring on the side of running the extension lookup in the `/` case
// preserves WMIC-stripped quoted Windows paths while accepting the
// (vanishingly rare) miss on `cmd.exe /c something`-style invocations
// where the `/` would terminate the path token cleanly anyway.
func firstWhitespaceTerminatesPath(c string, sp int) bool {
	if sp+1 >= len(c) {
		return true // nothing after the whitespace; path is complete.
	}
	next := c[sp+1]
	return next == '-'
}

// CleanupOpts controls CleanupOrphans.
type CleanupOpts struct {
	ManifestDir string
	MinAgeSec   int64  // don't kill processes younger than this (default 60)
	DryRun      bool   // if true, just report
	Server      string // empty = all servers; otherwise only that one

	// ScanClientConfigs (A6): when true, CleanupOrphans extracts
	// additional cmdline patterns from every installed MCP client's
	// stdio entries (codex / claude / cursor / vscode / gemini / qwen /
	// antigravity) via Client.AllStdioEntries. Used to detect orphan
	// stdio MCP servers whose spawning client has died (typical after
	// `mcphub language-server cleanup` or `mcphub migrate` followed by
	// a client crash) — the child node.exe / python.exe / etc. lives
	// on, re-parented to explorer.exe or svchost.
	//
	// Pair this with the known-client-launcher allowlist (built-in)
	// so LIVE stdio children of currently-running clients are NOT
	// killed by mistake.
	ScanClientConfigs bool

	// Aggressive (Phase H.1) opts INTO killing live-rooted MCP-stdio
	// processes that the default safe sweep CORRECTLY refuses — the
	// per-subagent stdio fan-out where a single live client (e.g.
	// codex) spawns N internal subagents that each spawn their own
	// stdio MCP children which never get reaped on subagent finish.
	// The default sweep walks the ancestor chain and excludes anything
	// rooted under a live client launcher (see parseOrphans); aggressive
	// mode INVERTS that for ONE explicitly-named scope so the operator
	// can reclaim the accumulation. It NEVER bypasses the mcphub.exe
	// daemon ancestor guard (hub-managed processes are always spared)
	// and it applies the dangerous-class deny-list below.
	//
	// REQUIRES exactly one scope: Client (a known launcher basename)
	// OR RootPID. AggressiveCleanup rejects an aggressive run with
	// neither (or both) set.
	Aggressive bool

	// Client narrows the aggressive sweep to processes whose ancestor
	// chain contains this client-launcher basename (case-insensitive,
	// .exe/.cmd/.bat/.ps1 stripped). Must be a recognized launcher in
	// knownClientLauncherBasenames. Mutually exclusive with RootPID.
	Client string

	// RootPID narrows the aggressive sweep to descendants of this PID.
	// Mutually exclusive with Client.
	RootPID int

	// ExpectPIDs, when non-nil, BINDS an aggressive KILL to a previously
	// resolved + confirmed candidate set: only candidates whose PID is in
	// this allowlist are killed, so a process that spawned AFTER the set was
	// validated is excluded — never killed unacknowledged (bot #373 R5; the
	// GUI apply path passes the token-validated PIDs). nil → no binding (the
	// CLI and every dry-run recompute the full current set). An empty (but
	// non-nil) slice kills nothing, which is the correct safe outcome for a
	// validated-empty set.
	ExpectPIDs []int

	// IncludeClasses lists dangerous process classes the operator has
	// explicitly opted to include in an aggressive kill (e.g. "chrome"
	// for a Playwright cleanup). Each entry overrides one deny-list
	// class; basenames are matched case-insensitively with the exe
	// suffix stripped. Empty (the default) keeps every dangerous class
	// excluded.
	IncludeClasses []string
}

// aggressiveDenyClasses are process basenames excluded from an
// aggressive kill BY DEFAULT even when they are descendants of the
// scoped client / root PID (spec H.1): operator terminals
// (cmd/conhost/pwsh/powershell) and Playwright browser sessions
// (chrome) the operator may still be using. Override one class at a
// time via CleanupOpts.IncludeClasses (CLI --include-class), which
// emits a stderr warning. Stored as bare lowercase basenames (exe
// suffix stripped) to pair with stripExtension + firstTokenBasename.
var aggressiveDenyClasses = []string{
	"cmd",
	"conhost",
	"pwsh",
	"powershell",
	"chrome",
}

// AggressiveDenyClasses returns a copy of the default dangerous-class
// deny-list for the aggressive sweep. Exported so the CLI audit-event
// builder can report which classes stayed excluded without duplicating
// the list (single owner).
func AggressiveDenyClasses() []string {
	return slices.Clone(aggressiveDenyClasses)
}

// AggressiveConfirmToken derives a deterministic confirmation token bound
// to the candidate snapshot. The token is the first 16 hex chars of
// SHA-256 over the SORTED (PID, exe-basename, match-source) tuples. Two
// runs over the same candidate set produce the same token; any
// add/remove/identity-change produces a different token, so a stale token
// is rejected by recompute-and-compare in the kill path.
//
// This is the SINGLE OWNER of the preview-token contract. Both the CLI
// (`mcphub cleanup aggressive` --confirm-aggressive-token) and the GUI
// (POST /api/cleanup/aggressive token field) compute and compare against
// it, so the token semantics cannot drift between the two surfaces.
func AggressiveConfirmToken(candidates []OrphanProcess) string {
	lines := make([]string, 0, len(candidates))
	for _, o := range candidates {
		lines = append(lines, fmt.Sprintf("%d|%s|%s", o.PID, o.CmdlineDisplay, o.MatchSource))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// errAggressiveScopeRequired is returned by AggressiveCleanup when
// --aggressive is set without exactly one of --client / --root-pid.
// Spec H.1: "no implicit all-live-rooted mode".
var errAggressiveScopeRequired = errors.New(
	"cleanup --aggressive requires exactly one scope: --client <name> OR --root-pid <pid>",
)

// errAggressiveUnknownClient is returned when --client names a
// launcher that is not in the recognized allowlist. An unrecognized
// client would never match any ancestor and silently sweep nothing
// (or worse, be a typo for a real one), so reject loudly.
var errAggressiveUnknownClient = errors.New(
	"cleanup --aggressive --client: unknown client launcher (recognized: claude, codex, gemini, qwen, cursor, code, cascade, antigravity)",
)

// ErrAggressiveScopeRequired / ErrAggressiveUnknownClient are exported
// aliases of the two malformed-scope sentinels above so callers OUTSIDE
// the api package (the GUI /api/cleanup/aggressive handler) can classify
// an AggressiveCleanup error with errors.Is and map it to HTTP 400
// instead of a generic 500. The CLI keeps using the unexported names;
// these are additive aliases, not a rename, so the existing in-package
// reject tests stay byte-identical.
var (
	ErrAggressiveScopeRequired = errAggressiveScopeRequired
	ErrAggressiveUnknownClient = errAggressiveUnknownClient
)

// knownClientLauncherBasenames is the allowlist of "this process
// looks like an MCP client we recognize" exe basenames. A running
// stdio MCP child whose ancestor chain contains ANY of these is
// considered LIVE-managed (the spawning client is still running)
// and is excluded from cleanup. Cross-platform basename match
// (forward + back slashes), case-insensitive, ".exe" stripped.
//
// Entries are deliberately exe basenames (not paths) so portable
// launchers (Squirrel.Update-style relocations, user-renamed
// installs to non-default dirs, code-name vs. ship-name binaries)
// still match. Each entry is documented with the user-facing
// product it represents.
var knownClientLauncherBasenames = []string{
	// Anthropic Claude — Claude Code CLI and Claude desktop app.
	"claude",
	// OpenAI / Codex CLI.
	"codex",
	// Google Gemini CLI.
	"gemini",
	// Alibaba Qwen CLI.
	"qwen",
	// Cursor IDE.
	"cursor",
	// VS Code (Code.exe is the shipped binary name) and forks.
	"code",
	// Google Antigravity (Gemini-CLI fork inside the Cascade IDE).
	"cascade",
	"antigravity",
}

// isKnownClientLauncher reports whether cmdline's first-token
// basename matches one of the recognized MCP-client launcher exes.
// Used by parseOrphans to skip processes whose ancestor chain
// contains a live client process — those are LIVE-managed stdio
// children, NOT orphans, and killing them would disrupt the user's
// current agent session.
//
// Returns false for empty / unrecognized cmdlines. Case-insensitive
// basename match across both Windows and POSIX separators, ".exe"
// stripped. Shares firstTokenBasename with isOurOwnProcess so a
// path with embedded spaces like
// `C:\Program Files\Cursor\Cursor.exe` correctly resolves to
// "cursor".
func isKnownClientLauncher(cmdline string) bool {
	// Strip ALL recognized launcher suffixes (.exe, .cmd, .bat,
	// .ps1) — not just .exe — so Windows wrapper-based installs
	// (claude.cmd / codex.cmd / gemini.bat shims around the real
	// binary) still match the allowlist. Codex bot r1 P1.2 on
	// PR #190: an `.exe`-only normalization let `cleanup
	// --scan-clients --confirm` kill stdio children of a live
	// claude.cmd-launched session because the .cmd ancestor was
	// classified as unknown.
	base := stripExtension(strings.ToLower(firstTokenBasename(cmdline)))
	return slices.Contains(knownClientLauncherBasenames, base)
}

// patternsFromClientStdio extracts cmdline patterns from every
// installed MCP client's live stdio entries. Returned patterns
// feed CleanupOrphans's reverse-lookup mode when
// CleanupOpts.ScanClientConfigs is true.
//
// Per stdio entry, the helper emits up to two patterns:
//
//  1. command basename (case-insensitive, .exe stripped, both
//     separators normalized) — IF that basename is not a broad
//     launcher token (node, python, npx, ...). The basename is the
//     most discriminating signal because most MCP server packages
//     ship a unique binary like `mcp-server-time` /
//     `mcp-language-server`.
//
//  2. each positional arg of length ≥ 4 that does NOT start with
//     `-`. The length floor drops short flag aliases like `-v`,
//     `-p`; the leading-dash skip drops long flags like `--lsp`
//     (the FLAG itself is shared across many entries; only the
//     VALUE following it is discriminating, but we emit values as
//     separate args in their own right when they meet the length
//     floor).
//
// Patterns are deduplicated so a multi-language setup with N
// `mcp-language-server --lsp <X>` entries contributes one
// `mcp-language-server` pattern, not N.
//
// Adapter read failures are best-effort: one broken client config
// does not block cleanup. The adapter pipeline already returns
// (nil, nil) when the config file does not exist (host without
// that client installed), so no per-client gating is required
// here.
func patternsFromClientStdio() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, c := range clients.AllClients() {
		if !c.Exists() {
			continue
		}
		entries, err := c.AllStdioEntries()
		if err != nil {
			// Nomination is best-effort: a parse error just means fewer
			// candidate patterns (missing a candidate is the SAFE direction
			// for NOMINATION). The kill-eligibility side uses the fail-closed
			// scanClientConfigsFailClosed instead.
			continue
		}
		appendPatternsFromEntries(entries, add, false) // strict: nomination can KILL
	}
	return out
}

// appendPatternsFromEntries emits the cleanup-match patterns for a set of client stdio
// entries via `add`. Shared by two callers whose arg-inclusion posture DIFFERS:
//
//   - NOMINATION (patternsFromClientStdio, inclusive=false): a pattern here can KILL, so
//     args pass the strict argIsDiscriminatingPattern filter (≥8 chars, has a path/pkg
//     separator) — a short token must not sweep unrelated processes into a kill.
//   - fail-closed KILL-ELIGIBILITY / config REFERENCE (scanClientConfigsFailClosed,
//     inclusive=true): a pattern here can only ever SPARE (candidateConfigReferenced widens
//     the spare set), so it uses the relaxed argIsReferenceCandidate filter and keeps a
//     short-but-real package token like "serena" — otherwise an adopted/manifest pattern
//     could nominate a still-configured `uvx serena` after its parent chain breaks and the
//     reference scan would have no pattern to spare it (bot PR #520 P2).
//
// The command-basename branch and patternIsTooBroad gate are identical for both. Skips an
// mcphub-relay-form entry whole (see the Antigravity note below).
func appendPatternsFromEntries(entries []clients.StdioEntry, add func(string), inclusive bool) {
	for _, e := range entries {
		base := stripExtension(basenameAcrossSeparators(e.Command))
		// Codex security finding e8745334 (Antigravity relay args → kill patterns):
		// an mcphub-relay-form entry (command=mcphub.exe args=[relay,--server,X,
		// --daemon,Y]) carries CONTROL-PLANE tokens (server/daemon names), not
		// external-process signatures. Emitting them as kill-match patterns would
		// substring-match unrelated processes (e.g. a `time-tracker.exe` user app vs.
		// the manifest's "time" server). Skip the ENTIRE entry when command is mcphub.
		if isMcphubBinaryBasename(strings.ToLower(base)) {
			continue
		}
		if !patternIsTooBroad(base) {
			add(base)
		}
		for _, arg := range e.Args {
			if inclusive {
				if !argIsReferenceCandidate(arg) {
					continue
				}
			} else if !argIsDiscriminatingPattern(arg) {
				continue
			}
			if patternIsTooBroad(arg) {
				continue
			}
			add(arg)
		}
	}
}

// argIsReferenceCandidate is the RELAXED arg filter used ONLY by the fail-closed config
// REFERENCE side (scanClientConfigsFailClosed, inclusive=true). Over-inclusion here can only
// SPARE a candidate — never kill — so it drops the nomination side's 8-char / discriminating
// -separator requirement and keeps a short but real package token like "serena". It still
// excludes flags (leading `-`), all-digit tokens (ports/PIDs/timeouts), and <3-char noise
// that would substring-match almost anything.
func argIsReferenceCandidate(arg string) bool {
	if len(arg) < 3 {
		return false
	}
	if strings.HasPrefix(arg, "-") {
		return false
	}
	if isAllDigits(arg) {
		return false
	}
	return true
}

// scanClientConfigsFailClosed collects the discriminating stdio patterns from every
// INSTALLED, PARSEABLE client config, and separately reports the Name() of every config
// that EXISTS but could NOT be parsed. This is the KILL-ELIGIBILITY substrate for the
// reaper's config-absence gate (H5 step 4): a config that exists but is unreadable /
// unparseable means the reaper CANNOT prove a candidate's signature is absent from it,
// so the caller must FAIL CLOSED (spare every otherwise-eligible candidate for the run).
// A not-installed client (Exists()==false) is NOT degraded — it contributes no patterns.
// Partial degradation still collects patterns from the OTHER (parseable) clients so one
// broken config does not blind the whole scan.
//
// Injectable for tests via scanClientConfigsFailClosedFn.
var scanClientConfigsFailClosedFn = scanClientConfigsFailClosedImpl

func scanClientConfigsFailClosed() (patterns []string, degradedClients []string) {
	return scanClientConfigsFailClosedFn()
}

func scanClientConfigsFailClosedImpl() (patterns []string, degradedClients []string) {
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		patterns = append(patterns, p)
	}
	clientsMap, factoryFailed := clients.AllClientsWithErrors()
	// A client whose factory could not even be constructed (UserHomeDir /
	// config-path resolution failure) is DEGRADED, not absent: the scan cannot
	// determine where its config lives, so it cannot prove a candidate's signature
	// is absent from it. Fail closed (bot PR #520 P2). clients.AllClients() drops
	// these silently, which would let the gate fail open.
	degradedClients = append(degradedClients, factoryFailed...)
	for _, c := range clientsMap {
		if !c.Exists() {
			// Exists()==false conflates "genuinely absent" with "stat errored":
			// adapters implement Exists as os.Stat(path)==nil, so an unreadable
			// installed config (ACCESS_DENIED on a sandbox-broadened %LOCALAPPDATA%,
			// see the CLAUDE.md secret-DACL runbook) reads as not-installed. Re-stat
			// ConfigPath to CLASSIFY: a non-IsNotExist error means we CANNOT prove the
			// config is absent, so it must count as DEGRADED (fail closed) — a candidate
			// referenced only by that unreadable config would otherwise pass the absence
			// check and be killed (bot PR #520 + $security-reviewer P2).
			if p := c.ConfigPath(); p != "" {
				if _, err := os.Stat(p); err != nil && !os.IsNotExist(err) {
					degradedClients = append(degradedClients, c.Name())
				}
			}
			continue
		}
		entries, err := c.AllStdioEntries()
		if err != nil {
			degradedClients = append(degradedClients, c.Name())
			continue
		}
		appendPatternsFromEntries(entries, add, true) // inclusive: reference side only SPARES
	}
	return patterns, degradedClients
}

// candidateConfigReferenced reports whether the orphan candidate's cmdline still matches
// a discriminating pattern derived from an installed client config — i.e. some client
// STILL WANTS this signature, so the candidate must be SPARED (a live client may hold
// its stdio pipes through a broken process chain, indistinguishable from a true orphan
// without deeper inspection). Reuses isBroadLauncherToken so the exclusion side shares
// the inclusion side's broad-token safety net (a bare `node` never counts as a reference).
func candidateConfigReferenced(o OrphanProcess, configPatterns []string) bool {
	for _, p := range configPatterns {
		if isBroadLauncherToken(p) {
			continue
		}
		if strings.Contains(o.Cmdline, p) {
			return true
		}
	}
	return false
}

// patternIsTooBroad reports whether tok would substring-match too
// many unrelated processes if it ended up in CleanupOrphans's
// pattern set. The check folds four prior bot findings on PR #190
// into ONE consistent gate applied to both the command-basename
// and args branches:
//
//   - empty string (nothing to match)
//   - broad interpreters (node / python / npx / uv / uvx / py /
//     python3) — isBroadLauncherToken
//   - known MCP-client launcher basenames (claude / codex / gemini
//     / qwen / cursor / code / cascade / antigravity) — r2 P1
//   - mcphub's own binary basenames (mcphub / mcp / mcphub.exe /
//     mcp.exe) — r5 P1: Antigravity stdio entries are written as
//     `command = "...\mcphub.exe"` so the command-basename branch
//     would emit "mcphub" without this guard, and any unrelated
//     shell whose cmdline mentions `mcphub install` (operator
//     command-line history, scripts, status displays) would be
//     classified as an orphan in `--scan-clients --confirm`.
//
// stripExtension is applied so .exe/.cmd/.bat/.ps1 wrappers normalize
// to the bare basename before comparison.
func patternIsTooBroad(tok string) bool {
	if tok == "" {
		return true
	}
	if isBroadLauncherToken(tok) {
		return true
	}
	bare := strings.ToLower(stripExtension(tok))
	if slices.Contains(knownClientLauncherBasenames, bare) {
		return true
	}
	if isMcphubBinaryBasename(bare) {
		return true
	}
	return false
}

// isMcphubBinaryBasename reports whether bare (the lowercase
// extension-stripped token) is one of mcphub's own CLI binary
// names. Mirrors clients.IsMcphubBinary's allowlist but works on
// pre-normalized basenames so it pairs cleanly with the rest of
// patternIsTooBroad.
func isMcphubBinaryBasename(bare string) bool {
	switch bare {
	case "mcphub", "mcp":
		return true
	}
	return false
}

// argIsDiscriminatingPattern reports whether an arg token is
// specific enough to be safely emitted as a kill-match substring
// pattern by CleanupOrphans (--scan-clients mode).
//
// Manual smoke on PR #190 surfaced that lax filtering let
// generic-word args poison the pattern set with substrings that
// false-matched unrelated workstation processes:
//
//   - "localhost" (9 chars, no separator) — would match any
//     loopback URL in any process's cmdline
//   - "false" (5 chars) — matches anything containing the literal
//     word "false" (Dropbox crashpad-handler cmdlines have it)
//   - "3.13" (4 chars) — matches Python 3.13 install paths
//
// Rules (must satisfy ALL):
//
//   - length ≥ 8 chars — drops short common words
//   - does NOT start with `-` — flags are shared across many entries
//   - is NOT purely numeric — port numbers, PIDs, timeouts are
//     non-discriminating
//   - contains at least one of `-`, `@`, `/`, `\`, `.` — npm scoped
//     packages, dashed binary names, paths, and Python-module dotted
//     names all match; bare English words don't.
//
// Trade-off: some legitimate short discriminators (e.g. "serena",
// "memory" as a server-name arg) are no longer emitted. Operators
// who need cleanup for those orphans must use the manifest-pattern
// path (without --scan-clients) or kill manually. Safer to miss
// than to false-positive Dropbox.
func argIsDiscriminatingPattern(arg string) bool {
	if len(arg) < 8 {
		return false
	}
	if strings.HasPrefix(arg, "-") {
		return false
	}
	if isAllDigits(arg) {
		return false
	}
	if !strings.ContainsAny(arg, `-@/\.`) {
		return false
	}
	return true
}

// isAllDigits reports whether s is non-empty and contains only
// ASCII digits 0-9. Used to drop numeric-only args (ports, PIDs,
// timeouts) from the pattern set — they pass the length floor but
// would substring-match unrelated processes that mention the same
// number.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// basenameAcrossSeparators returns the trailing path component of p
// folding both `\` and `/` separators. Mirrors the helper in
// internal/clients/clients.go; duplicated here to keep
// internal/api/cleanup.go from importing internal/clients just for
// one trivial string helper (and to keep the dependency cycle
// shallow: clients does not import api).
func basenameAcrossSeparators(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// stripExtension drops a recognized Windows exe extension from the
// tail of name, case-insensitively. Returns name unchanged when no
// match.
func stripExtension(name string) string {
	low := strings.ToLower(name)
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		if rest, ok := strings.CutSuffix(low, ext); ok {
			return name[:len(rest)]
		}
	}
	return name
}

// CleanupOrphans finds MCP server processes that match a manifest's command
// pattern but whose parent is NOT our `mcp.exe daemon` wrapper. Reports them
// (dry-run) or kills them (non-dry-run).
func (a *API) CleanupOrphans(opts CleanupOpts) ([]OrphanProcess, error) {
	// Codex bot r3 P1 / r4 P2 on PR #190: --scan-clients and
	// --server are incompatible. Client stdio entries are identified
	// by entry name + (command, args); they carry NO manifest-server
	// key, so no useful narrowing exists. Mixing the two would expand
	// allPatterns with cmdlines unrelated to the requested server,
	// and a process matching one of those client-derived patterns
	// would be killed in --confirm mode despite being out of scope.
	//
	// This check runs BEFORE the runtime.GOOS Windows-only short-
	// circuit so the flag-semantics contract holds on every platform
	// (r4 P2: validating cross-platform keeps Linux/macOS CI tests
	// honest and prevents a silent acceptance on POSIX hosts that
	// would diverge from the CLI help text).
	if opts.ScanClientConfigs && opts.Server != "" {
		return nil, errOrphanOptsServerScanClientsConflict
	}
	if runtime.GOOS != "windows" {
		// Process introspection below uses Windows-specific tooling.
		// Return an empty result on other platforms so the CLI stays usable
		// (`mcp cleanup` just prints "No orphan processes found.").
		return nil, nil
	}
	if opts.MinAgeSec == 0 {
		opts.MinAgeSec = 60
	}
	// Collect patterns per manifest.
	patterns := map[string][]string{}
	if opts.Server != "" {
		patterns[opts.Server] = patternsForServerNominatable(opts.Server, opts.ManifestDir)
	} else {
		names, err := readManifestNames(opts.ManifestDir)
		if err != nil {
			return nil, err
		}
		for name := range names {
			patterns[name] = patternsForServerNominatable(name, opts.ManifestDir)
		}
	}

	// Snapshot processes. wmic was the historical tool but Windows 11 24H2+
	// ships without it; PowerShell's Get-CimInstance works on every modern
	// Windows and produces equivalent data.
	out, err := runProcessSnapshot()
	if err != nil {
		return nil, err
	}

	// Flat list of patterns — any match counts this PID as a candidate orphan.
	// Drops broad launcher tokens AND bare-name-fallback patterns (T3 demotion).
	allPatterns := manifestNominationPatterns(patterns)
	// A6: when --scan-clients is set, fold in patterns derived from
	// every installed client's live stdio entries. Reverse-lookup
	// catches MCP-server children whose spawning client has died and
	// whose process tree has been re-parented to explorer.exe /
	// svchost.exe — the manifest-pattern path alone would miss those
	// because user-added stdio entries are not represented in the
	// shipped manifests.
	//
	// Codex bot r3 P1 on PR #190: --scan-clients is INCOMPATIBLE with
	// --server. Client-config stdio entries have no server-name key —
	// they're identified by entry name + (command, args). Mixing the
	// two would expand allPatterns with cmdlines unrelated to the
	// requested server, so a process matching one of those would be
	// killed in --confirm mode despite being out of scope. Skip the
	// client-derived patterns when --server narrows the run.
	if opts.ScanClientConfigs && opts.Server == "" {
		allPatterns = append(allPatterns, patternsFromClientStdio()...)
	}
	orphans, snapErr := parseOrphans(strings.NewReader(string(out)), allPatterns)

	// Age filter + assign server. Same broad-token filter applies here:
	// matching o.Cmdline against `node` would label any unrelated node
	// process with the manifest's server name.
	filtered := orphans[:0]
	for _, o := range orphans {
		if o.AgeSec < opts.MinAgeSec {
			continue
		}
		for name, ps := range patterns {
			for _, p := range ps {
				if isBroadLauncherToken(p) {
					continue
				}
				if strings.Contains(o.Cmdline, p) {
					o.Server = name
					break
				}
			}
			if o.Server != "" {
				break
			}
		}
		filtered = append(filtered, o)
	}

	applyReapEligibilityGate(filtered, opts.DryRun, snapErr != nil)

	return filtered, nil
}

// applyReapEligibilityGate stamps the H5-revised config-absence reap-eligibility verdict
// (A2 PR5) on every candidate and kills ONLY the reap-eligible ones (when !dryRun).
//
// A candidate whose signature is still referenced by an installed, PARSEABLE client config
// is SPARED — a live client may hold its stdio pipes through a broken process chain,
// indistinguishable from a true orphan without deeper inspection (H5 §Decision.3 step 4).
// If ANY existing client config could not be parsed this run, the reaper cannot prove
// absence, so every otherwise-eligible candidate is spared (fail closed). Only a
// reap-eligible candidate is killed, via the unchanged identity-re-verifying reapOneOrphan
// (process.TerminatePIDWithIdentity) — a PID recycled between census and kill fails the
// {executable, basename, start-time} re-verify on a held handle and is left untouched.
//
// Extracted so it is unit-testable via the scanClientConfigsFailClosedFn + orphanTerminateFn
// seams without a live process snapshot. AggressiveCleanup deliberately does NOT call this:
// it reaps live-rooted processes under an operator-confirmed scope and must not inherit the
// config-absence gate (which would make it inert — a live client's config is still present).
func applyReapEligibilityGate(filtered []OrphanProcess, dryRun bool, snapshotDegraded bool) {
	cfgPatterns, degradedClients := scanClientConfigsFailClosed()
	for i := range filtered {
		verdict, skipReason := classifyReapVerdict(filtered[i], cfgPatterns, degradedClients, snapshotDegraded)
		filtered[i].ReapVerdict = verdict
		// KillErr carries the per-row OUTCOME, which exists only on the APPLY path. A
		// dry-run preview must NOT stamp a spare-reason into KillErr: the GUI renders
		// `kill_err || "killed"` and treats a listed row with an empty kill_err as killed,
		// so a preview that stamped spare-reasons would (a) surface a result column master
		// never shows in preview and (b) render a reap-ELIGIBLE row (empty KillErr) as a
		// false "killed" before the operator ever pressed Clean — consent-surface
		// misinformation on a destructive action (workflow + bot PR #520 P1). The
		// structured ReapVerdict above is the dry-run-safe audit field the GUI can render.
		if !dryRun && verdict != ReapVerdictReapEligible {
			filtered[i].KillErr = skipReason
		}
	}
	if dryRun {
		return
	}
	for i := range filtered {
		if filtered[i].ReapVerdict != ReapVerdictReapEligible {
			continue // spared by the config-absence / snapshot-degraded gate
		}
		filtered[i].KillErr = reapOneOrphan(filtered[i])
	}
}

// classifyReapVerdict computes the H5-revised reap-eligibility verdict for one candidate
// plus the human, path-free skip-reason the APPLY path records in KillErr. Pure: no kill,
// no dry-run branch, no OS access — the caller decides whether to record the reason (apply
// only) and whether to kill. The precedence is deliberate: snapshot-degraded (a dropped
// live-ancestor row can mis-classify a live child as an orphan) and config-referenced both
// spare BEFORE the age floor, and the 600s kill-age floor (design.md P2 "age>~10min") is
// the last defense-in-depth check so a just-orphaned-mid-restart child gets several ticker
// re-evaluations on fresh snapshots before any auto-kill.
func classifyReapVerdict(o OrphanProcess, cfgPatterns, degradedClients []string, snapshotDegraded bool) (verdict, skipReason string) {
	switch {
	case snapshotDegraded:
		return ReapVerdictSparedSnapshotDegraded,
			"skipped: process snapshot was truncated this run; kills disabled (fail closed)"
	case candidateConfigReferenced(o, cfgPatterns):
		return ReapVerdictSparedConfigReferenced,
			"skipped: signature still referenced by an installed client config"
	case len(degradedClients) > 0:
		return ReapVerdictSparedConfigScanDegraded,
			"skipped: a client config was unreadable this run (" + strings.Join(degradedClients, ",") + "); reaper fails closed"
	case o.AgeSec < cleanupKillMinAgeSec:
		return ReapVerdictSparedBelowKillAgeFloor,
			"skipped: below the kill-age floor (needs a longer orphan age before an auto-kill)"
	default:
		return ReapVerdictReapEligible, ""
	}
}

// ReapVerdictLabel maps a ReapVerdict* value to a short, human, path-free PREVIEW
// label for CLI / GUI display, so a dry-run surfaces per-row whether a candidate
// will be cleaned or is already spared and WHY — instead of the operator learning
// it only after Apply (bot PR #520 P2). An empty or unknown verdict (an older wire,
// or an AggressiveCleanup row that never ran the config-absence gate) returns "" so
// the caller keeps its default rendering. ReapVerdictIsEligible is the matching
// predicate for "will this row actually be cleaned" preview counts.
func ReapVerdictLabel(v string) string {
	switch v {
	case ReapVerdictReapEligible:
		return "will clean"
	case ReapVerdictSparedConfigReferenced:
		return "skip: still referenced by a client config"
	case ReapVerdictSparedConfigScanDegraded:
		return "skip: a client config was unreadable this run"
	case ReapVerdictSparedSnapshotDegraded:
		return "skip: process snapshot was truncated this run"
	case ReapVerdictSparedBelowKillAgeFloor:
		return "skip: below the kill-age floor"
	default:
		return ""
	}
}

// ReapVerdictIsEligible reports whether a row will actually be cleaned on Apply.
// An empty verdict (older wire, or an AggressiveCleanup row that never ran the
// config-absence gate) counts as eligible — those paths kill every listed row —
// so a preview "N will clean" count matches what Apply does.
func ReapVerdictIsEligible(v string) bool {
	return v == "" || v == ReapVerdictReapEligible
}

// manifestNominationPatterns flattens the per-server manifest patterns into the flat
// kill-nomination pattern set, dropping broad launcher tokens (node/npx/python/...) that
// would match unrelated dev-tooling.
//
// The synthesized BARE-NAME FALLBACK is dropped upstream at the source now, not here: the
// caller builds this map with patternsForServerNominatable, which returns nil for a corrupt
// or no-discriminating-pattern manifest so a bare generic server name ("time"/"memory"/
// "fetch") is never nominated for the unattended apply:true auto-ticker (T3 demotion, A2 PR5
// $security-reviewer). Doing the drop at the source (rather than a len(ps)==1 && ps[0]==name
// check here) preserves a REAL self-named manifest whose command basename legitimately equals
// the server name — e.g. the shipped `mcp-language-server` — which a name-equality check here
// would have wrongly dropped, silently emptying `cleanup --server mcp-language-server` (bot
// PR #520 P2).
func manifestNominationPatterns(patterns map[string][]string) []string {
	var out []string
	for _, ps := range patterns {
		for _, p := range ps {
			if isBroadLauncherToken(p) {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// isBroadLauncherToken returns true if the manifest pattern is a bare
// generic interpreter / launcher name (node, npx, python, ...). Such
// tokens are too noisy as orphan-match patterns: nearly every dev box
// has unrelated node/python/npx processes running, and a `Contains`
// match against `cmdline` would sweep them into our cleanup.
//
// The check normalizes the pattern before comparing:
//
//   - trim whitespace and surrounding single/double quotes
//   - lowercase
//   - split on BOTH `\\` and `/` separators so `C:\Users\...\python.exe`
//     and `/usr/bin/python3` both reduce to a bare interpreter name —
//     filepath.Base would NOT strip backslashes when running under
//     Linux/macOS CI, so we walk both separators ourselves.
//   - strip Windows executable suffixes (.exe, .cmd, .bat, .ps1) so
//     `node.exe`, `npx.cmd`, `uvx.exe` all reduce to the bare token
//
// Codex finding fix: PR #121 only matched bare tokens (e.g. `"node"`)
// and missed the .exe-suffixed and absolute-path forms that real wmic
// output produces. Cross-platform fix: use platform-independent base()
// since this code path runs in the orphan-cleanup logic that consumes
// wmic CSV output (Windows-style paths) on any platform's CI.
func isBroadLauncherToken(pattern string) bool {
	p := strings.TrimSpace(pattern)
	p = strings.Trim(p, `"'`)
	if p == "" {
		return true
	}
	p = strings.ToLower(p)
	// Platform-independent basename: take everything after the last
	// '/' or '\\', whichever comes later. Don't use filepath.Base on
	// Unix runners — it would treat `C:\Users\...\node.exe` as a
	// single token because '\\' is not a separator there.
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		p = p[i+1:]
	}
	for _, suffix := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		if strings.HasSuffix(p, suffix) {
			p = strings.TrimSuffix(p, suffix)
			break
		}
	}
	switch p {
	case "node", "npx", "uv", "uvx", "python", "python3", "py":
		return true
	}
	return false
}

// parseProcessRows reads the wmic/CIM CSV process snapshot
// (`CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize`)
// and returns the parsed rows plus an index by PID for ancestor walks.
// It delegates row parsing to the shared snapshot parser in processes.go so
// the default orphan sweep, aggressive sweep, process counting, and log-watcher
// cleanup all consume the same comma-safe row shape.
func parseProcessRows(r io.Reader) ([]procRow, map[int]procRow, error) {
	rows, err := parseProcessSnapshotRows(r)
	// A read error (e.g. bufio.ErrTooLong on a pathologically long CommandLine
	// row) ends the loop early and can truncate the process snapshot, dropping
	// rows the orphan-detector's parent-chain walk relies on — a fail-OPEN input
	// (a dropped live-ancestor row makes its protected descendant look orphaned).
	// Surface it: the rows parsed so far are returned best-effort for the dry-run
	// listing, but the error is propagated so the caller can DISABLE kills for the
	// run (snapshot-degraded fail-closed — A2 PR5). $security-reviewer P1.
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcphub: warning: process snapshot scan ended early: %v\n", err)
	}

	// Index by PID so callers can inspect a parent's cmdline.
	byPID := map[int]procRow{}
	for _, r := range rows {
		byPID[r.pid] = r
	}
	return rows, byPID, err
}

// parseOrphans reads `wmic process get CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize`
// CSV output and returns processes whose CommandLine matches any of the given
// patterns BUT whose parent is NOT an `mcp.exe daemon` process.
//
// Visible for unit tests so fixture CSVs can drive the logic without wmic.
func parseOrphans(r io.Reader, patterns []string) ([]OrphanProcess, error) {
	rows, byPID, snapErr := parseProcessRows(r)

	var out []OrphanProcess
	for _, r := range rows {
		// Never flag our own binaries — mcphub.exe (running as daemon or
		// as any subcommand), the standalone godbolt/lldb-bridge/perftools
		// exes, or the legacy mcp.exe name. These are running by design;
		// their parent is the Task Scheduler service, so the parent-is-our-
		// daemon check below won't save them. Without this explicit
		// allowlist the cleanup would flag e.g. `mcphub.exe daemon --server
		// gdb` as an orphan gdb-server just because "gdb" appears in its
		// cmdline.
		if isOurOwnProcess(r.cmdline) {
			continue
		}
		// Codex bot r2 P1 on PR #190 (defense in depth): also
		// skip rows whose own cmdline IS a known client launcher.
		// The ancestor-walk below only inspects parents from
		// r.ppid upward, not the row itself, so a candidate that
		// is e.g. claude.exe whose cmdline happens to contain a
		// pattern from allPatterns (such as `--daemon claude` —
		// see Antigravity adapter) would fall through to the
		// orphan path and be killed in confirm mode.
		// patternsFromClientStdio now filters launcher names out
		// of the arg branch, but this row-level guard remains as
		// belt-and-suspenders against any future pattern source
		// that could introduce a launcher basename.
		if isKnownClientLauncher(r.cmdline) {
			continue
		}
		matched := false
		for _, p := range patterns {
			if strings.Contains(r.cmdline, p) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// Is any ANCESTOR one of our own daemons OR a known MCP client
		// launcher? Walk the parent chain up to a bounded depth (16
		// levels is well beyond anything real). Single-level check
		// misses uvx/npx/node sub-processes that wrap the actual
		// server — e.g. mcphub.exe daemon → uv → python → server.py
		// forms a 4-deep chain where python's direct parent is uv,
		// not our daemon. Walking the chain means every descendant of
		// either a live `mcphub.exe daemon` OR a live client process
		// (claude / codex / gemini / cursor / vscode / antigravity /
		// qwen) is correctly excluded from cleanup.
		//
		// Why the client-launcher check: --scan-clients mode (A6)
		// expands the pattern set with cmdline shapes derived from
		// every installed client's stdio entries. Without the
		// client-launcher allowlist, a node.exe spawned by a
		// currently-running claude.exe (with a still-active stdio
		// config entry) would be matched by a pattern and killed,
		// breaking the user's live agent session. The walk treats
		// any ancestor at a recognized client basename as proof the
		// stdio child is LIVE-managed, not an orphan.
		ourDescendant := false
		for cur, depth := r.ppid, 0; depth < 16; depth++ {
			parent, ok := byPID[cur]
			if !ok {
				break
			}
			pcmd := parent.cmdline
			if strings.Contains(pcmd, "daemon") &&
				(strings.Contains(pcmd, "mcphub.exe") || strings.Contains(pcmd, "mcp.exe")) {
				ourDescendant = true
				break
			}
			if isKnownClientLauncher(pcmd) {
				ourDescendant = true
				break
			}
			if parent.ppid == 0 || parent.ppid == cur {
				break // reached the root or a self-loop
			}
			cur = parent.ppid
		}
		if ourDescendant {
			continue // NOT orphan — descendant of our daemon or a live client
		}
		age := int64(0)
		if !r.created.IsZero() {
			age = int64(time.Since(r.created).Seconds())
		}
		out = append(out, OrphanProcess{
			PID:            r.pid,
			ParentID:       r.ppid,
			RAMBytes:       r.ram,
			Cmdline:        r.cmdline,
			CmdlineDisplay: redactCmdlineForDisplay(r.cmdline),
			AgeSec:         age,
			ExecutablePath: r.exePath,
			StartedAt:      orphanStartedAt(r.created),
		})
	}
	return out, snapErr
}

// isAggressiveDenyClass reports whether cmdline's executable basename is
// one of the dangerous classes excluded from an aggressive kill by
// default, UNLESS that class appears in includeClasses (operator
// override). cmdline is the candidate's OWN command line; classification
// uses the same firstTokenBasename + stripExtension normalization as the
// rest of the orphan detector so a quoted Windows path like
// `"C:\...\chrome.exe" --type=renderer` resolves to "chrome".
func isAggressiveDenyClass(cmdline string, includeClasses []string) bool {
	base := strings.ToLower(stripExtension(firstTokenBasename(cmdline)))
	if !slices.Contains(aggressiveDenyClasses, base) {
		return false
	}
	for _, inc := range includeClasses {
		if strings.ToLower(stripExtension(strings.TrimSpace(inc))) == base {
			return false // operator opted this class back in
		}
	}
	return true
}

// parseAggressiveCandidates is the inverse-of-safe sweep for Phase H.1.
// It returns processes that ARE live-rooted under the requested scope
// (clientBasename OR rootPID) and therefore CORRECTLY refused by the
// default parseOrphans sweep — the per-subagent stdio fan-out the
// operator explicitly wants to reclaim.
//
// Invariants it still upholds (no aggressive bypass):
//   - mcphub.exe daemon descendants are ALWAYS spared (hub-managed).
//   - our own binaries (isOurOwnProcess) are never candidates.
//   - a process whose OWN cmdline is the scoping client launcher is not
//     a candidate (we kill its leaked children, not the live client).
//   - dangerous classes (cmd/conhost/pwsh/powershell/chrome) are
//     excluded unless opted in via includeClasses.
//
// Each candidate carries MatchSource: the ancestor basename that
// anchored the scope (--client) or "root-pid <pid>" (--root-pid).
//
// Exactly one of clientBasename / rootPID must be set; the caller
// (AggressiveCleanup) enforces that before calling. clientBasename is
// the already-normalized lowercase launcher basename.
func parseAggressiveCandidates(r io.Reader, clientBasename string, rootPID int, includeClasses []string) ([]OrphanProcess, error) {
	// snapErr surfaces a truncated census: a dropped live `mcphub.exe daemon` ancestor
	// row would let the ancestor walk mis-scope a hub-managed child as an aggressive
	// candidate, violating the "hub-managed processes are ALWAYS spared" invariant. The
	// caller (AggressiveCleanup) fails closed on it before any APPLY kill (all-return-paths
	// of the parseProcessRows snapshot-error contract; codex PR #520).
	rows, byPID, snapErr := parseProcessRows(r)

	var out []OrphanProcess
	for _, r := range rows {
		// Our own binaries are never aggressive targets.
		if isOurOwnProcess(r.cmdline) {
			continue
		}
		// The live client launcher itself is not a target — we reap
		// its leaked stdio children, never the live session root.
		if isKnownClientLauncher(r.cmdline) {
			continue
		}
		// Dangerous-class deny-list (operator-overridable).
		if isAggressiveDenyClass(r.cmdline, includeClasses) {
			continue
		}

		// Walk the ancestor chain. Record the first recognized scope
		// anchor, but keep walking so the mcphub-daemon guard can still
		// take priority even when it appears above the requested scope
		// root. A child that is BOTH under the scoped client/root and
		// under a hub daemon is always spared.
		matchSource := ""
		spared := false
		for cur, depth := r.ppid, 0; depth < 16; depth++ {
			parent, ok := byPID[cur]
			if !ok {
				break
			}
			pcmd := parent.cmdline
			// Hub-managed processes are always spared — aggressive
			// mode never bypasses this guard.
			if strings.Contains(pcmd, "daemon") &&
				(strings.Contains(pcmd, "mcphub.exe") || strings.Contains(pcmd, "mcp.exe")) {
				spared = true
				break
			}
			if rootPID != 0 {
				if cur == rootPID && matchSource == "" {
					matchSource = "root-pid " + strconv.Itoa(rootPID)
				}
			} else if clientBasename != "" {
				if matchSource == "" && stripExtension(strings.ToLower(firstTokenBasename(pcmd))) == clientBasename {
					matchSource = clientBasename
				}
			}
			if parent.ppid == 0 || parent.ppid == cur {
				break // reached the root or a self-loop
			}
			cur = parent.ppid
		}
		if spared || matchSource == "" {
			continue // hub-managed, or not under the requested scope
		}

		age := int64(0)
		if !r.created.IsZero() {
			age = int64(time.Since(r.created).Seconds())
		}
		out = append(out, OrphanProcess{
			PID:            r.pid,
			ParentID:       r.ppid,
			RAMBytes:       r.ram,
			Cmdline:        r.cmdline,
			CmdlineDisplay: redactCmdlineForDisplay(r.cmdline),
			AgeSec:         age,
			ExecutablePath: r.exePath,
			StartedAt:      orphanStartedAt(r.created),
			MatchSource:    matchSource,
		})
	}
	return out, snapErr
}

// AggressiveCleanup (Phase H.1) reports — and, when DryRun=false, kills
// — the live-rooted MCP-stdio processes under the scope named by
// opts.Client OR opts.RootPID. It is the operator-confirmed override for
// the per-subagent stdio fan-out class that the default safe sweep
// (CleanupOrphans) correctly refuses to touch.
//
// The caller is responsible for the dry-run/confirmation-token protocol
// (the CLI layer binds a token to the previewed candidate snapshot);
// this method is the pure scope-resolve + (optional) kill primitive.
// Returns errAggressiveScopeRequired / errAggressiveUnknownClient on a
// malformed scope so the CLI can surface a precise message.
func (a *API) AggressiveCleanup(opts CleanupOpts) ([]OrphanProcess, error) {
	// Exactly one scope. No implicit "all live-rooted" mode (spec H.1).
	hasClient := strings.TrimSpace(opts.Client) != ""
	hasRoot := opts.RootPID != 0
	if hasClient == hasRoot {
		return nil, errAggressiveScopeRequired
	}
	clientBasename := ""
	if hasClient {
		clientBasename = stripExtension(strings.ToLower(strings.TrimSpace(opts.Client)))
		if !slices.Contains(knownClientLauncherBasenames, clientBasename) {
			return nil, errAggressiveUnknownClient
		}
	}
	if runtime.GOOS != "windows" {
		// Process introspection below is Windows-specific. Return an
		// empty result on other platforms so the flag contract stays
		// usable (CLI prints "No aggressive candidates found.").
		return nil, nil
	}
	if opts.MinAgeSec == 0 {
		opts.MinAgeSec = 60
	}

	out, err := runProcessSnapshot()
	if err != nil {
		return nil, err
	}
	candidates, snapErr := parseAggressiveCandidates(
		strings.NewReader(string(out)), clientBasename, opts.RootPID, opts.IncludeClasses)

	// Age filter — same floor as the default sweep so a just-spawned
	// in-flight child is not reaped mid-handshake.
	filtered := candidates[:0]
	for _, c := range candidates {
		if c.AgeSec < opts.MinAgeSec {
			continue
		}
		filtered = append(filtered, c)
	}

	// Validated-set binding (bot #373 R5): when ExpectPIDs is non-nil the
	// caller has already token-validated a specific candidate set, so the
	// kill must touch ONLY those PIDs. A process that spawned between the
	// validation snapshot and this one is excluded; a validated PID that has
	// since died simply drops out. nil → no binding (CLI / dry-run preview).
	if opts.ExpectPIDs != nil {
		filtered = filterToExpectedPIDs(filtered, opts.ExpectPIDs)
	}

	// Fail closed on a truncated census before any APPLY kill: a dropped live
	// `mcphub.exe daemon` ancestor row can mis-scope a hub-managed child as an
	// aggressive candidate, so an unreliable snapshot must never drive a kill
	// (all-return-paths of the parseProcessRows snapshot-error contract; codex
	// PR #520). The dry-run preview stays best-effort — it lists the candidates
	// but touches nothing — so the operator can see and retry.
	if !opts.DryRun && snapErr != nil {
		return filtered, fmt.Errorf("aggressive cleanup: process snapshot was truncated this run; kills disabled (fail closed): %w", snapErr)
	}

	// Same identity-re-verifying reap as the default sweep — never a raw
	// taskkill against a census-captured PID, so a PID recycled between the
	// census scan and the kill can never friendly-fire an unrelated process
	// (PID-recycle guard, shared with CleanupOrphans via reapOrphans).
	reapOrphans(filtered, opts.DryRun)
	return filtered, nil
}

// filterToExpectedPIDs binds a freshly-snapshotted candidate set to a
// previously token-validated PID allowlist: it returns only the candidates
// whose PID is in expectPIDs, in their original order. A process that spawned
// after the validation snapshot (PID not in the allowlist) is therefore never
// killed, and a validated PID that has since exited simply drops out (no
// longer a candidate). This is the api-level half of the GUI apply path's
// "bind the kill to the token-validated set" contract (bot #373 R5).
func filterToExpectedPIDs(candidates []OrphanProcess, expectPIDs []int) []OrphanProcess {
	allow := make(map[int]bool, len(expectPIDs))
	for _, p := range expectPIDs {
		allow[p] = true
	}
	out := candidates[:0]
	for _, c := range candidates {
		if allow[c.PID] {
			out = append(out, c)
		}
	}
	return out
}

// orphanTerminateFn is the identity-re-verifying kill primitive the cleanup
// reapers use. It re-opens a handle for the PID and re-verifies
// {ExecutablePath, basename, kernel start-time} BEFORE terminating, failing
// closed on ACCESS_DENIED / identity mismatch / a PID recycled between the
// census scan and the kill. It is the SAME primitive the supervisor's
// port-squatter reap kills through (internal/cli/supervise_squatter.go).
// Injectable so tests exercise the recycle / mismatch / already-gone paths
// without killing a real process. process.TerminatePIDWithIdentity is defined
// on every platform (Windows handle re-verify, Linux pidfd, other →
// unsupported), so this compiles cross-platform even though CleanupOrphans and
// AggressiveCleanup only reach the kill path on Windows.
var orphanTerminateFn = process.TerminatePIDWithIdentity

const cleanupIdentityStartTolerance = 500 * time.Millisecond

var (
	orphanIdentityLookupTimeout = 5 * time.Second
	errOrphanIdentityUnverified = errors.New("cleanup: process identity unverified")
)

// orphanStartedAt renders a census-captured process creation time as the
// RFC3339Nano proof timestamp consumed by process.TerminatePIDWithIdentity's
// start-time re-verify. A zero time (missing / unparseable CreationDate) yields
// "" so the proof fails closed on a missing started_at rather than matching a
// bogus year-1 epoch. Fractional seconds are preserved when the snapshot
// provides them so cleanup can use a tight PID-reuse tolerance.
func orphanStartedAt(created time.Time) string {
	if created.IsZero() {
		return ""
	}
	return created.UTC().Format(time.RFC3339Nano)
}

// reapOrphans kills each already-filtered orphan through the identity-gated kill
// primitive (reapOneOrphan → orphanTerminateFn), recording the per-orphan outcome on
// KillErr. It is now AggressiveCleanup's kill loop ONLY (operator-confirmed live-rooted
// sweep); the default safe sweep (CleanupOrphans) kills through applyReapEligibilityGate
// instead, which applies the A2 PR5 config-absence / snapshot-degraded / age-floor
// reap-eligibility gate BEFORE reapOneOrphan — reapOneOrphan is the single kill primitive
// both paths share, so neither can regress to a raw taskkill. AggressiveCleanup
// deliberately does NOT go through the reap-eligibility gate (a live client's config is
// definitionally present, which would make the aggressive sweep inert). No-op on dryRun.
// Mutates filtered in place.
func reapOrphans(filtered []OrphanProcess, dryRun bool) {
	if dryRun {
		return
	}
	for i := range filtered {
		filtered[i].KillErr = reapOneOrphan(filtered[i])
	}
}

// reapOneOrphan performs the identity-gated kill of a single orphan and returns
// the KillErr string ("" on a clean kill). The census-captured ExecutablePath +
// StartedAt (plus the PID) form the proof re-verified on a held handle inside
// process.TerminatePIDWithIdentity, so a PID recycled onto an unrelated process
// between the census scan and this kill fails the proof and is NOT killed. The
// returned strings distinguish the four non-clean outcomes so the CLI/GUI report
// stays informative (all of them count as "skipped" — KillErr != "").
func reapOneOrphan(o OrphanProcess) string {
	if o.ExecutablePath == "" || o.StartedAt == "" || o.Cmdline == "" {
		// No identity captured at census (e.g. a process whose image path the
		// snapshot could not read) → cannot build a proof → do NOT kill.
		return "skipped: process identity unavailable at census (no executable path, start time, or command line); not killed"
	}
	if err := revalidateOrphanCommandLine(o); err != nil {
		return classifyReapError(err)
	}
	err := orphanTerminateFn(process.PIDIdentityProof{
		PID:            o.PID,
		ExecutablePath: o.ExecutablePath,
		StartedAt:      o.StartedAt,
		StartTolerance: cleanupIdentityStartTolerance,
	})
	return classifyReapError(err)
}

func revalidateOrphanCommandLine(o OrphanProcess) error {
	ctx, cancel := context.WithTimeout(context.Background(), orphanIdentityLookupTimeout)
	defer cancel()
	live, err := orphanLookupIdentityFn(ctx, o.PID)
	if err != nil {
		return fmt.Errorf("%w: %w: PID %d command-line proof unavailable: %v", process.ErrProcessIdentityMismatch, errOrphanIdentityUnverified, o.PID, err)
	}
	liveCmdline := normalizeOrphanCommandLineForIdentityCompare(live.CommandLine)
	censusCmdline := normalizeOrphanCommandLineForIdentityCompare(o.Cmdline)
	if liveCmdline == "" || censusCmdline == "" || liveCmdline != censusCmdline {
		return fmt.Errorf("%w: PID %d command-line mismatch", process.ErrProcessIdentityMismatch, o.PID)
	}
	return nil
}

func normalizeOrphanCommandLineForIdentityCompare(s string) string {
	// WMIC CSV drops quote delimiters that PowerShell preserves. Keep this
	// deliberately narrow: remove only double-quote bytes and collapse
	// whitespace. Do not case-fold or shell-parse arguments here.
	return strings.Join(strings.Fields(strings.ReplaceAll(s, `"`, "")), " ")
}

func classifyReapError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, process.ErrProcessAlreadyExited):
		return "already exited before kill (no action needed)"
	case errors.Is(err, errOrphanIdentityUnverified):
		return "skipped: identity unverified - command-line proof unavailable; not killed"
	case errors.Is(err, process.ErrProcessIdentityMismatch):
		return "skipped: identity mismatch — PID recycled or process replaced since census; not killed"
	default:
		// ACCESS_DENIED, unsupported platform, or any other terminate failure.
		return err.Error()
	}
}

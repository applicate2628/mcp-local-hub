package api

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"slices"
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
// KillErr is populated only when DryRun=false and taskkill failed for this PID
// (access denied, process already gone, etc.); empty on success or dry-run.
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
	PID             int    `json:"pid"`
	ParentID        int    `json:"parent_pid"`
	Server          string `json:"server"` // inferred from matching manifest
	RAMBytes        uint64 `json:"ram_bytes"`
	Cmdline         string `json:"-"`               // server-side use only; redacted from wire
	CmdlineDisplay  string `json:"cmdline_display"` // basename of executable for GUI display
	AgeSec          int64  `json:"age_sec"`
	KillErr         string `json:"kill_err,omitempty"`
}

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
}

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
		if p == "" {
			return
		}
		if seen[p] {
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
			continue
		}
		for _, e := range entries {
			base := stripExtension(basenameAcrossSeparators(e.Command))
			// Codex security finding e8745334 (Antigravity relay
			// args → kill patterns): when an adapter writes
			// mcphub-relay-form entries (command=mcphub.exe
			// args=[relay,--server,X,--daemon,Y]) — Antigravity
			// is the only such adapter today — the args are
			// CONTROL-PLANE tokens (server name, daemon name),
			// not external-process signatures. Emitting them as
			// kill-match patterns causes strings.Contains to
			// match unrelated processes whose cmdline mentions
			// the same generic word (e.g. a `time-tracker.exe
			// --relay-mode` user app vs. the manifest's "time"
			// server). Skip the ENTIRE entry when command is
			// mcphub — no basename pattern, no arg patterns,
			// nothing. patternIsTooBroad already rejects
			// "mcphub" as the basename pattern, but the
			// security finding pointed out that args still
			// leaked through. This per-entry skip closes the
			// hole at the source.
			if isMcphubBinaryBasename(strings.ToLower(base)) {
				continue
			}
			if !patternIsTooBroad(base) {
				add(base)
			}
			for _, arg := range e.Args {
				if !argIsDiscriminatingPattern(arg) {
					continue
				}
				if patternIsTooBroad(arg) {
					continue
				}
				add(arg)
			}
		}
	}
	return out
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
		patterns[opts.Server] = patternsForServer(opts.Server, opts.ManifestDir)
	} else {
		names, err := readManifestNames(opts.ManifestDir)
		if err != nil {
			return nil, err
		}
		for name := range names {
			patterns[name] = patternsForServer(name, opts.ManifestDir)
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
	// Drop broad launcher tokens (node, npx, uv, uvx, python, ...) so we
	// don't sweep the operator's unrelated dev-tooling processes that
	// happen to share an interpreter with one of our manifest commands.
	var allPatterns []string
	for _, ps := range patterns {
		for _, p := range ps {
			if isBroadLauncherToken(p) {
				continue
			}
			allPatterns = append(allPatterns, p)
		}
	}
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
	orphans := parseOrphans(strings.NewReader(string(out)), allPatterns)

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

	// Kill if not dry-run. Preserve taskkill's stderr on each failure so the
	// caller can distinguish "access denied" from "PID already gone" in the
	// per-orphan report instead of silently swallowing the error.
	if !opts.DryRun {
		for i := range filtered {
			cmd := exec.Command("taskkill", "/PID", strconv.Itoa(filtered[i].PID), "/F")
			process.NoConsole(cmd)
			out, err := cmd.CombinedOutput()
			if err != nil {
				msg := strings.TrimSpace(string(out))
				if msg == "" {
					msg = err.Error()
				}
				filtered[i].KillErr = msg
			}
		}
	}

	return filtered, nil
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

// parseOrphans reads `wmic process get CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize`
// CSV output and returns processes whose CommandLine matches any of the given
// patterns BUT whose parent is NOT an `mcp.exe daemon` process.
//
// Visible for unit tests so fixture CSVs can drive the logic without wmic.
//
// Known limitation (codex deep-sec PR #143 round 4 finding Q2): the
// bufio.Scanner below splits strictly on newline, so a CommandLine field
// that contains an embedded `\n` (rare; quotes around such fields would
// normally protect them) would prematurely end the row. Real WMIC /
// CIM output never produces this shape in practice; rewriting the
// parser to a state-machine CSV reader is deferred until a real-world
// case appears.
func parseOrphans(r io.Reader, patterns []string) []OrphanProcess {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	type row struct {
		pid, ppid int
		created   time.Time
		cmdline   string
		ram       uint64
	}
	var rows []row
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "Node,") || strings.TrimSpace(line) == "" {
			continue
		}
		fields := splitCSVLine(line)
		if len(fields) < 6 {
			continue
		}
		// WMIC's `/format:csv` output does NOT quote the cmdline
		// field. When cmdline contains commas (very common:
		// `--setting-sources=user,project,local`,
		// `--allow-list=a,b,c`, anything with CSV-shaped values),
		// splitCSVLine produces MORE than 6 fields and naive
		// fields[3]/fields[4] indexing returns pieces of the
		// cmdline instead of ppid/pid. Garbage PIDs then poison
		// byPID, and the ancestor-walk in the orphan detector
		// can't find live client launchers — silently flagging
		// LIVE stdio MCP children for killing on
		// `cleanup --scan-clients --confirm`.
		//
		// Manual smoke on PR #190 caught this with claude.exe
		// whose cmdline included
		// `--setting-sources=user,project,local`: PID 173992 was
		// stored under a garbage integer parsed from `local
		// --permission-mode bypassPermissions ...` and the gopls
		// child of that claude session was flagged as orphan.
		//
		// Fix: anchor from the RIGHT. The trailing 4 fields
		// (created date, ppid, pid, ram) are well-shaped
		// integers/dates with no embedded commas, so they
		// align reliably regardless of how many commas the
		// cmdline contributed. The cmdline is reassembled by
		// rejoining the middle slice with commas.
		n := len(fields)
		cmdline := strings.Join(fields[1:n-4], ",")
		created := parseWmicDate(strings.TrimSpace(fields[n-4]))
		ppid, err1 := strconv.Atoi(strings.TrimSpace(fields[n-3]))
		pid, err2 := strconv.Atoi(strings.TrimSpace(fields[n-2]))
		ram, err3 := strconv.ParseUint(strings.TrimSpace(fields[n-1]), 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || pid == 0 {
			// Malformed row — skip rather than poison byPID with
			// garbage that would corrupt downstream parent-chain
			// lookups for other rows.
			continue
		}
		rows = append(rows, row{pid: pid, ppid: ppid, created: created, cmdline: cmdline, ram: ram})
	}

	// Index by PID so we can inspect parent's cmdline.
	byPID := map[int]row{}
	for _, r := range rows {
		byPID[r.pid] = r
	}

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
		})
	}
	return out
}

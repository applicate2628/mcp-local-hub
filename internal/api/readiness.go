package api

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/lldb"
	"mcp-local-hub/internal/secrets"
	"mcp-local-hub/internal/toolchain"
)

// ReadinessRequirement is one checked prerequisite for a server to run. It is
// the unit the GUI renders and the install flow summarizes: a human Name,
// whether it is OK, and — when not — a Reason plus an actionable Fix (the
// exact command or next step). This is the DETECT half of the install
// "detect + guided prompt" UX
// (work-items/epics/2026-06-19-install-and-it-works-ux.md): a missing
// dependency or unset secret yields a guided fix here instead of a bare
// "command not found" or a downstream cryptic HTTP-502 at the client.
type ReadinessRequirement struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Optional marks an ADVISORY requirement that does NOT block readiness:
	// an unmet optional requirement (a not-yet-set `secret:` ref) still lets
	// the server install + spawn, so it does NOT flip ReadinessReport.Ready
	// to false. The GUI renders these as "set to enable" prompt fields at
	// install rather than blockers (install-and-it-works: secrets optional).
	Optional bool   `json:"optional,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

// ReadinessReport aggregates every prerequisite check for one server. Ready is
// true iff every NON-optional Requirement is OK (an unmet Optional requirement
// — e.g. an unset secret — is advisory and does not block Ready). Unlike
// Preflight (which fails fast at the first BLOCKING issue because it gates a
// mutating install), CheckServerReadiness runs every check so the operator
// sees the FULL list — blockers and optional prompts — at once.
type ReadinessReport struct {
	Server       string                 `json:"server"`
	Ready        bool                   `json:"ready"`
	Requirements []ReadinessRequirement `json:"requirements"`
}

// LauncherGuidance maps a manifest launcher command to its (human display
// name, actionable install guidance). It is the SINGLE OWNER of "how do I get
// this dependency" knowledge so the install preflight, the GUI readiness
// panel, and any future onboarding flow all surface the SAME guided fix
// rather than each re-deriving (or omitting) it. Unknown launchers get a
// generic but still actionable fallback. Per-OS hints are inline because a
// single mcphub binary serves Windows GA / Linux beta / macOS preview and the
// operator should see the line for their platform without a lookup.
func LauncherGuidance(command string) (display, fix string) {
	switch command {
	case "uvx", "uv":
		return "uv (Python tool runner)",
			"Install uv — Windows: `winget install astral-sh.uv`; macOS/Linux: `curl -LsSf https://astral.sh/uv/install.sh | sh`; or `pip install uv`. Docs: https://docs.astral.sh/uv/"
	case "npx", "npm", "node":
		return "Node.js (npx/npm/node)",
			"Install Node.js (ships npx/npm) — Windows: `winget install OpenJS.NodeJS.LTS`; macOS: `brew install node`; all: https://nodejs.org/"
	case "python", "python3":
		return "Python 3",
			"Install Python 3 — Windows: `winget install Python.Python.3.12`; macOS: `brew install python`; all: https://www.python.org/downloads/"
	case "go":
		return "Go toolchain",
			"Install Go — Windows: `winget install GoLang.Go`; macOS: `brew install go`; all: https://go.dev/dl/"
	case "mcp-language-server":
		return "mcp-language-server",
			"Install — `go install github.com/isaacphi/mcp-language-server@latest` (needs Go on PATH), or rely on the bundled mcphub LSP router."
	case "mcphub":
		return "mcphub (self)",
			"Run `mcphub setup` to install the canonical mcphub binary on PATH."
	case "gdb":
		return "gdb (GNU debugger)",
			"Install gdb — Windows (MSYS2 ucrt64): `pacman -S mingw-w64-ucrt-x86_64-gdb`; Linux: `apt install gdb`; macOS: `brew install gdb`."
	case "lldb":
		return "lldb (LLVM debugger)",
			"Install lldb — Windows (MSYS2 clang64): `pacman -S mingw-w64-clang-x86_64-lldb`; Linux: `apt install lldb`; macOS: ships with Xcode CLT (`xcode-select --install`)."
	case "clang", "clang++", "clang-cl":
		return "clang (LLVM)",
			"Install clang/LLVM — Windows: `winget install LLVM.LLVM`; Linux: `apt install clang`; macOS: `xcode-select --install`."
	case "git":
		return "git",
			"Install git — Windows: `winget install Git.Git`; macOS: `brew install git` (or Xcode CLT); Linux: `apt install git`."
	default:
		// Basename only: an unknown command may be an absolute host path from a
		// custom manifest, and LauncherGuidance's return feeds the GUI-rendered
		// display Name AND the Fix string (not only Reason), so strip the
		// directory at the single owner of the fallback display/fix — otherwise
		// /api/server/readiness echoes a username-bearing host path via Name/Fix
		// (Codex #377 r12). No-op for bare names.
		base := filepath.Base(command)
		return base,
			fmt.Sprintf("Install %q and ensure it is on PATH, then re-run install.", base)
	}
}

// normalizeLauncher reduces a manifest command to its bare launcher name for
// matching: it strips any directory (an absolute `C:\tools\uvx.exe` still IS
// uvx) and a Windows executable/shim suffix (`.exe`/`.cmd`/`.bat`/`.ps1`), so
// `uvx`, `uvx.exe`, and `/opt/bin/uvx` all compare equal. Single owner used by
// runtimeBehindLauncher, manifestNeedsGit, and the entry-script gate so none of
// them silently skip a dependency on an absolute / suffixed launcher (Codex
// #377 r14).
func normalizeLauncher(command string) string {
	// Split on BOTH separators, not filepath.Base: a Windows-style path read on
	// POSIX (where filepath.Base only splits "/") would otherwise keep its
	// backslashes, so `C:\nodejs\npx.exe` would not match `npx` on Linux/macOS
	// and the dependency gates would silently skip the deeper checks (Codex #377
	// r16).
	base := command
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		if len(base) > len(ext) && strings.EqualFold(base[len(base)-len(ext):], ext) {
			return base[:len(base)-len(ext)]
		}
	}
	return base
}

// runtimeBehindLauncher returns the deeper runtime command a launcher needs to
// actually fetch + run its package, when that differs from the launcher
// itself. LookPath(launcher) succeeding does NOT prove the runtime can fetch +
// run the target package; the npm/npx shims are themselves `#!/usr/bin/env
// node` scripts, so both delegate to node. Empty string means the launcher IS
// self-contained (uvx bootstraps its own Python; go, node, mcphub are
// themselves the runtime), so no deeper check is added. The command is
// normalized first so `npm.cmd` / an absolute npx path still match (Codex #377
// r14).
func runtimeBehindLauncher(command string) string {
	switch normalizeLauncher(command) {
	case "npx", "npm":
		return "node"
	default:
		return ""
	}
}

// bridgeListenerUp reports whether something is already accepting TCP on addr
// (host:port), mirroring the lldb-bridge's own pre-spawn dial. A live listener
// means the bridge will reuse it and needs no local debugger binary, so
// readiness is satisfied without one (Codex #377 r12). Short timeout: this is a
// readiness probe, not a connection.
func bridgeListenerUp(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// binaryAvailable reports whether an external binary a manifest depends on is
// runnable. It honors the native-debugger discovery for gdb/lldb (the bridges
// resolve them via DebuggerDirs, so a debugger present in a toolchain dir but
// not on the process PATH still runs — a plain LookPath would falsely report it
// missing), and falls back to LookPath for everything else. SINGLE OWNER shared
// by CheckServerReadiness and Preflight so the two install gates cannot diverge
// on what counts as a present dependency (Codex #377 r13).
func binaryAvailable(bin string) bool {
	// resolve checks one concrete token for RUNNABILITY via exec.LookPath, which
	// verifies executability — not just existence. DefaultGdbPath/DefaultLldbPath
	// return an absolute path after only an os.Stat, so a present-but-NON-
	// executable gdb/lldb file in a debugger dir would otherwise be reported
	// available and the daemon would then fail at exec with permission denied
	// (Codex #377 r17). exec.LookPath tries an absolute/slash-bearing path
	// directly (checking the execute bit) and resolves a bare name against PATH —
	// without re-entering the switch, so the bare gdb/lldb default path does not
	// recurse.
	resolve := func(p string) bool {
		_, err := exec.LookPath(p)
		return err == nil
	}
	switch bin {
	case "gdb":
		return resolve(toolchain.DefaultGdbPath())
	case "lldb":
		return resolve(toolchain.DefaultLldbPath())
	default:
		return resolve(bin)
	}
}

// manifestNeedsGit reports whether a uvx/uv manifest fetches a `git+` source
// (e.g. serena's `--from git+https://…@<sha>`), which shells out to the git
// binary at launch — uv does NOT vendor git, so uvx-on-PATH does not prove git
// is. SINGLE OWNER shared by readiness and Preflight (Codex #377 r13).
func manifestNeedsGit(m *config.ServerManifest) bool {
	switch normalizeLauncher(m.Command) {
	case "uvx", "uv":
	default:
		return false
	}
	args := append(append([]string{}, m.BaseArgs...), m.BaseArgsTemplate...)
	// The git+ source can sit anywhere that lands in the child argv: a dynamic-
	// pool manifest's daemon_template.extra_args_template (Codex #377 r16) OR a
	// static daemon's per-daemon extra_args (Codex #377 r18). Scan all of them or
	// readiness/Preflight would miss the git dependency.
	if m.DaemonTemplate != nil {
		args = append(args, m.DaemonTemplate.ExtraArgsTemplate...)
	}
	for _, d := range m.Daemons {
		args = append(args, d.ExtraArgs...)
	}
	for _, a := range args {
		if strings.Contains(a, "git+") {
			return true
		}
	}
	return false
}

// entryScriptStatus stats one node/python entry-script target the way the
// launcher needs it: the path must EXIST and be a regular file. A directory
// (e.g. base_args[0] pointing at `build/` instead of `build/index.js`) cannot be
// run as an entry script, so it is rejected like a missing file (Codex #377
// r16). SINGLE OWNER shared by CheckServerReadiness and Preflight. Returns (ok,
// reason-when-not-ok).
func entryScriptStatus(path string) (bool, string) {
	info, err := os.Stat(path)
	if err != nil {
		return false, "does not exist"
	}
	if info.IsDir() {
		return false, "is a directory, not a runnable entry script"
	}
	return true, ""
}

// globProbeMatches is the SINGLE OWNER of the D-3 install_probe file_globs[]
// check — the OPT-IN glob-pattern path. It COMPOSES the existing regular-file
// owner entryScriptStatus over each filepath.Glob match — it adds NO new
// detection (entryScriptStatus stays the literal-path stat owner; this helper
// only widens the input from one exact path to a glob's match set). It returns
// (true, "") if ANY glob match is a runnable regular file, else (false, reason).
// The shape lets a SHARED cross-host catalog declare a version-agnostic probe
// (e.g. "…\\Live *\\…\\Ableton Live *.exe" matching Live 11/12) without baking an
// exact host path.
//
// GLOB-ONLY by design (codex catalog finding — explicit glob intent): this helper
// is reached ONLY for file_globs[] entries, where the operator/catalog DECLARED a
// pattern. It does NOT stat the verbatim pattern first — a literal path lives in
// files[] (entryScriptStatus owner) and is never routed here. That split removes
// the glob-vs-literal ambiguity of a single files[] field: a files[] literal with
// a glob metacharacter ("/opt/Foo*/marker", "Foo [Beta]") stats literally and a
// sibling can never satisfy it; only a file_globs[] entry globs.
//
// Polarity stays fail-closed, matching the prior owner:
//   - filepath.Glob errors (ErrBadPattern, a malformed pattern) → fail inert with
//     a named reason rather than silently passing.
//   - glob returns (nil, nil) on NO match → (false, "does not exist") —
//     byte-identical to entryScriptStatus on a missing literal path, so the
//     probe-fails-inert verdict is preserved (no match = the host app absent).
//   - a match that is a directory is rejected exactly as a literal directory is.
func globProbeMatches(pattern string) (bool, string) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		// filepath.Glob's documented error is ErrBadPattern only. A malformed
		// pattern can never enable the row, so fail inert with a reason instead of
		// letting it slip through.
		return false, "is not a valid glob pattern"
	}
	if len(matches) == 0 {
		// No match — the host app/marker is absent. Reuse entryScriptStatus's
		// missing-path reason so the diagnostic shape matches a literal miss.
		return false, "does not exist"
	}
	// ANY match that is a runnable regular file satisfies the probe. Compose the
	// existing regular-file owner over each glob match (a directory match is
	// rejected exactly as a literal directory is).
	var lastReason string
	for _, m := range matches {
		ok, reason := entryScriptStatus(m)
		if ok {
			return true, ""
		}
		lastReason = reason
	}
	// Every match existed but none was a runnable regular file (e.g. the glob hit
	// only directories). Surface the regular-file owner's own reason.
	return false, lastReason
}

// availabilityInert is the SINGLE OWNER of the D-3 watch/disabled predicate,
// consumed by AdmissionCheck (the gate) and any GUI signal so the meaning of
// "inert" is defined exactly once (architecture law: one owner per cross-cutting
// invariant). A ready/empty availability is NOT inert and behaves exactly as
// every existing manifest does today.
func availabilityInert(m *config.ServerManifest) bool {
	return m.Availability == config.AvailabilityWatch || m.Availability == config.AvailabilityDisabledUntilProbe
}

// availabilityProbePasses reports whether the D-3 install-probe is satisfied. It
// is a pure DRY-RUN over the EXISTING readiness primitives — it neither spawns
// the daemon nor writes any config, and adds ZERO new detection: it composes the
// SINGLE OWNERS binaryAvailable (PATH/toolchain runnability), entryScriptStatus
// (LITERAL regular-file existence), and globProbeMatches (a glob over
// entryScriptStatus), the exact functions Preflight and CheckServerReadiness
// already use, so there is no second detection path that can drift from the
// install gate. AND semantics across ALL THREE fields: every declared binary must
// be runnable AND every declared files[] LITERAL must exist AND every declared
// file_globs[] pattern must have at least one matching regular file. A nil probe
// never passes (Gate A rule A6 already forbids declaring an inert row without
// one). Returns (ok, reason-when-not-ok).
//
// files[] is EXACT-STAT ONLY (codex catalog finding — make glob expansion opt-in):
// a files[] entry is a literal path stat'd verbatim via entryScriptStatus and is
// NEVER globbed, so a literal install path that happens to contain a glob
// metacharacter ("/opt/Foo*/marker", "Foo [Beta]") resolves to ITSELF and an
// absent literal can never silently fall to a sibling match. file_globs[] is the
// OPT-IN pattern path (globProbeMatches). Splitting the two fields removes the
// ambiguity of inferring intent from one value.
func availabilityProbePasses(p *config.AvailabilityProbe) (bool, string) {
	if p == nil {
		return false, "no install probe declared"
	}
	for _, bin := range p.Binaries {
		if !binaryAvailable(bin) {
			// DISPLAY ONLY: basenameAcrossSeparators (not filepath.Base) so a Windows
			// absolute probe path (now accepted by the cross-platform validator) does
			// not echo the WHOLE `C:\...\tool` back through the 412/readiness error on
			// a non-Windows build, where filepath.Base treats '\' as a normal char and
			// returns the full path (codex r6 finding 3). The probe target passed to
			// binaryAvailable above is the verbatim value — only the surfaced string is
			// shortened.
			return false, fmt.Sprintf("%q not found on PATH", basenameAcrossSeparators(bin))
		}
	}
	for _, f := range p.Files {
		// files[] is the LITERAL-path owner: stat the verbatim path via
		// entryScriptStatus (exact, NEVER globbed). A path containing a glob
		// metacharacter is treated literally — the file at that exact path must exist
		// as a runnable regular file. AND-semantics across entries: a single missing
		// literal fails the whole probe.
		if ok, reason := entryScriptStatus(f); !ok {
			// DISPLAY ONLY: dual-separator basename for the same reason as the binary
			// branch above — entryScriptStatus stats the verbatim path; only the
			// user-visible marker name is stripped (codex r6 finding 3).
			return false, fmt.Sprintf("%q %s", basenameAcrossSeparators(f), reason)
		}
	}
	for _, g := range p.FileGlobs {
		// file_globs[] is the OPT-IN PATTERN owner: globProbeMatches filepath.Glob-
		// expands g then composes entryScriptStatus over each match. A version-agnostic
		// pattern (e.g. "…\\Live *\\…") passes if ANY match is a runnable regular file.
		// AND-semantics across entries are preserved: a single non-matching pattern
		// fails the whole probe.
		if ok, reason := globProbeMatches(g); !ok {
			// DISPLAY ONLY: dual-separator basename for the same reason as the branches
			// above — globProbeMatches globs the verbatim pattern; only the user-visible
			// marker name is stripped (codex r6 finding 3).
			return false, fmt.Sprintf("%q %s", basenameAcrossSeparators(g), reason)
		}
	}
	return true, ""
}

// fixedPortStatus checks a FIXED daemon port for the readiness report the way
// the daemon will actually bind it at launch: a valid 1..65535 range AND
// bindable via the same probe the pool allocator uses (portAvailable),
// tolerating our OWN already-running daemon for an idempotent reinstall. A
// dial-only probe would miss an out-of-range port (a failed dial reads as
// "free") and a port bound-but-not-listening (Codex #377 r15/r16). Preflight
// keeps its own dial-based, supervisor-intent-aware collision check (richer than
// this simple probe) and only shares the range guard. Returns (ok, reason).
func fixedPortStatus(port int, server, daemon string, internal bool) (bool, string) {
	if port < 1 || port > 65535 {
		kind := "port"
		if internal {
			kind = "native-http internal upstream port"
		}
		return false, fmt.Sprintf("%s %d is outside the valid range 1..65535", kind, port)
	}
	if !portAvailable(port) && !portHeldByOurDaemonForPortArm(port, server, daemon, internal) {
		return false, fmt.Sprintf("port %d is already in use by another process", port)
	}
	return true, ""
}

// entryScript is one entry-script target the node/python gate stats. daemon is
// the owning daemon name for a per-daemon (relative-cwd) target, or "" for a
// cwd-independent absolute target that applies to every daemon — callers that
// install a single daemon filter on it (Codex #377 r15). resolvable is false
// when a RELATIVE base_args[0] meets a non-absolute daemon cwd: the launch
// directory is unknowable from this process, so the target is surfaced as a
// known-tolerated advisory rather than checked against the wrong cwd (Codex
// #377 r18).
type entryScript struct {
	label, path, daemon string
	resolvable          bool
}

// entryScriptCheckTargets returns the entry scripts to stat for a node/python
// manifest. base_args[0] is the local script the launcher runs; the launcher
// being on PATH does NOT prove the script exists (e.g. wolfram's build/index.js
// inside an uncloned repo). An ABSOLUTE arg is one cwd-independent target. A
// RELATIVE arg resolves against EACH daemon's cwd (a multi-daemon manifest can
// differ per daemon); an EMPTY daemon cwd means the daemon inherits mcphub's
// launch cwd, approximated by the process cwd (os.Getwd) — the best proxy
// available, and far better than silently skipping the check (Codex #377
// r10/r14). SINGLE OWNER shared by CheckServerReadiness and Preflight so the
// readiness report and the install gate cannot diverge.
func entryScriptCheckTargets(m *config.ServerManifest) []entryScript {
	switch normalizeLauncher(m.Command) {
	case "node", "python", "python3":
	default:
		return nil
	}
	if len(m.BaseArgs) == 0 || strings.HasPrefix(m.BaseArgs[0], "-") {
		return nil
	}
	arg := m.BaseArgs[0]
	if filepath.IsAbs(arg) {
		// daemon "" → cwd-independent, applies to every daemon (never filtered).
		return []entryScript{{label: filepath.Base(arg), path: arg, resolvable: true}}
	}
	daemons := m.Daemons
	if len(daemons) == 0 {
		daemons = []config.DaemonSpec{{Name: "default"}}
	}
	var out []entryScript
	for _, d := range daemons {
		name := d.Name
		if name == "" {
			name = "default"
		}
		if filepath.IsAbs(d.Cwd) {
			out = append(out, entryScript{label: filepath.Base(arg) + " (" + name + ")", path: filepath.Join(d.Cwd, arg), daemon: name, resolvable: true})
			continue
		}
		// Empty/relative daemon cwd: the daemon inherits mcphub's launch cwd,
		// which is unpredictable across the supervisor / scheduler / GUI surfaces,
		// so a RELATIVE script cannot be verified from THIS process. Resolving
		// against os.Getwd() would judge the readiness/install process's directory,
		// not the daemon's — a verdict for the wrong directory (Codex #377 r18,
		// architect Q3). Mark unresolvable: readiness surfaces a known-tolerated
		// advisory, Preflight does not block.
		out = append(out, entryScript{label: filepath.Base(arg) + " (" + name + ")", daemon: name, resolvable: false})
	}
	return out
}

// IN-SCOPE CRITERION (the boundary that keeps this gate from chasing an
// unbounded edge surface). A prerequisite belongs in readiness/Preflight if and
// ONLY if it is all three of:
//
//	(a) PRE-SPAWN     — evaluable WITHOUT running the daemon (a stat, a LookPath,
//	                    a port probe, or a pure validator like lldb.ParseHostPort
//	                    / BuildPlan run as a dry-run);
//	(b) STATE-COMMITTING — failing it means mcphub would commit client-config +
//	                    supervisor-intent state for a daemon that is then
//	                    guaranteed dead (a FALSE INSTALL), not a transient runtime
//	                    hiccup;
//	(c) MCPHUB-FIXABLE — there is an actionable Fix mcphub can name.
//
// Anything downstream of cmd.Start (a git+ remote being unreachable, an npm
// registry outage, a wheel that fails to build, the server's own auth/runtime
// behavior) fails (a) and/or (c) and is OUT OF SCOPE BY DESIGN — the daemon
// reports its own error. `$VAR`/`file:` env-ref handling and the optional
// `secret:` advisory already draw this line.
//
// KNOWN-TOLERATED non-convergences (correct to keep as advisory, not bugs): a
// RELATIVE base_args[0] under a daemon with no absolute cwd cannot be verified
// from this process (the daemon inherits an unpredictable launch cwd) — surfaced
// Optional, never resolved against os.Getwd() (Codex #377 r18). The fixed-port
// detail rows still use the readiness bind probe (portAvailable, the allocator's
// own truth) while AdmissionCheck preserves Preflight's dial probe plus
// supervisor-intent awareness (portHeldByOurDaemonForPortArm); Ready follows the
// AdmissionCheck owner, and the bind-probe rows remain richer GUI diagnostics.
//
// CheckServerReadiness runs every prerequisite check for a server WITHOUT
// failing fast and returns a structured, GUI-renderable report. AdmissionCheck
// seeds the scope-independent admission result; later requirement rows can still
// flip Ready=false when they add a non-optional blocker, including the
// effective-scope install-plan dry-run. The requirement rows below are the rich
// rendering layer on top: launcher/runtime guidance, per-key secret prompts,
// port detail rows, and install-plan explanations for the GUI.
func CheckServerReadiness(m *config.ServerManifest) *ReadinessReport {
	return CheckServerReadinessWithScope(m, AdmissionScope{})
}

// CheckServerReadinessWithScope runs readiness checks for the install scope.
// A daemon-filtered install must not be blocked by sibling daemons that the
// actual install path will skip during Preflight and planning.
func CheckServerReadinessWithScope(m *config.ServerManifest, scope AdmissionScope) *ReadinessReport {
	// Run the scope-independent admission gate ONCE and reuse it both to seed
	// Ready and to surface its advisory (Optional) findings as visible rows
	// below — re-calling AdmissionCheck would repeat the port/binary probes.
	admission := AdmissionCheck(m, scope)
	rep := &ReadinessReport{Server: m.Name, Ready: !containsNonOptional(admission)}
	add := func(r ReadinessRequirement) {
		if !r.OK && !r.Optional {
			rep.Ready = false
		}
		rep.Requirements = append(rep.Requirements, r)
	}

	// D-3 inert short-circuit (Tier-0). When the manifest is inert
	// (watch / disabled-until-probe) and its install-probe has NOT passed,
	// AdmissionCheck returns ONLY the availability-probe blocking finding and
	// short-circuits every downstream port/binary/script check — the row is not
	// going to install. Mirror that here: surface the availability-probe finding
	// as a VISIBLE requirement row (so Ready=false has a rendered explanation
	// instead of a bare false with no reason) and return. Without this the GUI
	// panel would show Ready=false alongside launcher/script rows that do not
	// reflect why the row is blocked. We REUSE the finding from the SINGLE
	// AdmissionCheck call above (findingByID) rather than re-calling
	// availabilityProbeFinding — re-running it would evaluate the file-based probe
	// a SECOND time within one readiness request, so a probe that changes mid-
	// request (a binary/file appearing or disappearing between the two calls)
	// could yield a Ready seed and an advisory row that disagree (intra-request
	// TOCTOU, bot catalog-r3 P3). One call, one verdict. ADDITIVE: a ready/empty
	// manifest (or an inert row whose probe passed) carries no such finding, so
	// the requirement list is byte-identical to before.
	if f, inertBlock := findingByID(admission, "availability-probe"); inertBlock {
		add(ReadinessRequirement{
			Name:   f.Name,
			OK:     false,
			Reason: f.Reason,
			Fix:    f.Fix,
		})
		return rep
	}

	// D-2 vendored-license advisory (Tier-0). AdmissionCheck emits an OPTIONAL
	// "vendored-license-unvetted" finding when a vendored/community-fork server's
	// license_status is empty / pending / unknown (not confirmed). Preflight
	// ignores optional findings (they do not block the mutating install — the
	// operator may knowingly install a pending-license fork on their own host),
	// and Ready was seeded from containsNonOptional, so the advisory neither
	// blocks install nor flips Ready. But without surfacing it here the operator
	// would never SEE the unvetted-license advisory in the GUI readiness panel.
	// Mirror how the inert-probe finding is surfaced above: render the SAME
	// AdmissionCheck finding (reusing its owner — no re-derivation of the predicate
	// or text) as an Optional (advisory, NON-blocking) row. ADDITIVE: a manifest
	// with no vendored_source (every existing manifest) produces no such finding,
	// so the requirement list is byte-identical to before.
	if f, ok := optionalFindingByID(admission, "vendored-license-unvetted"); ok {
		add(ReadinessRequirement{
			Name:     f.Name,
			OK:       false,
			Optional: true,
			Reason:   f.Reason,
			Fix:      f.Fix,
		})
	}

	// Launcher on PATH (with guided fix), then the runtime behind it. For a
	// workspace-scoped (dynamic-pool LSP) manifest the launcher is ADVISORY:
	// the actual backend is selected per workspace/language and some paths do
	// NOT use m.Command at all — a Go workspace runs the gopls-mcp backend
	// (launches gopls directly), so a missing mcp-language-server must not
	// mark a gopls-ready Go workspace not-ready (Codex #377). The per-language
	// required_binaries (already advisory) cover the real per-language tools.
	// Advisory ONLY for the per-LANGUAGE LSP shape (m.Languages, no
	// daemon_template), where the backend is selected per language and a Go
	// workspace runs gopls-mcp without m.Command. A daemon_template
	// workspace-scoped manifest (e.g. serena) DOES use m.Command as the child
	// launcher and Preflight rejects it missing, so keep that blocking
	// (Codex #377 r5).
	launcherOptional := m.Kind == config.KindWorkspaceScoped && m.DaemonTemplate == nil
	if m.Command != "" {
		disp, fix := LauncherGuidance(m.Command)
		if _, err := exec.LookPath(m.Command); err != nil {
			add(ReadinessRequirement{
				Name:     "launcher: " + disp,
				OK:       false,
				Optional: launcherOptional,
				// Basename only: m.Command is a free-form manifest field that a
				// custom manifest could set to an absolute host path; the GUI
				// renders Reason verbatim, so strip the directory (Codex #377
				// merge-gate P3). No-op for the bare names (uvx/npx/...) every
				// embedded manifest uses.
				Reason: fmt.Sprintf("%q not found on PATH", filepath.Base(m.Command)),
				Fix:    fix,
			})
		} else {
			add(ReadinessRequirement{Name: "launcher: " + disp, OK: true})
			if rt := runtimeBehindLauncher(m.Command); rt != "" {
				rdisp, rfix := LauncherGuidance(rt)
				if _, err := exec.LookPath(rt); err != nil {
					add(ReadinessRequirement{
						Name:     "runtime: " + rdisp,
						OK:       false,
						Optional: launcherOptional,
						Reason:   fmt.Sprintf("%q (needed by %s) not found on PATH", rt, filepath.Base(m.Command)),
						Fix:      rfix,
					})
				} else {
					add(ReadinessRequirement{Name: "runtime: " + rdisp, OK: true})
				}
			}
		}
	}

	// node / python-script launchers run a LOCAL script as base_args[0]; the
	// launcher existing does NOT prove the script does (e.g. wolfram's
	// build/index.js inside an uncloned wolframalpha-llm-mcp). Check it so such
	// a manifest is not reported ready while its entry script is missing
	// (Codex #377 r8). base_args[0] is ALREADY env-expanded at parse — do NOT
	// re-expand (os.ExpandEnv would mangle a literal `$` in the path; Codex
	// pre-catch r9). A RELATIVE base_args[0] resolves against the daemon's
	// per-daemon cwd (m.Daemons[i].Cwd) at spawn, so resolve it against that
	// (when known + absolute) rather than skipping or statting it against this
	// process's cwd (Codex #377 r9). Skip flag-shaped args (python -m, --flag).
	for _, c := range entryScriptCheckTargets(m) {
		// A daemon-scoped install (scope.DaemonFilter set) skips sibling
		// daemons — Preflight / BuildPlanWithOpts(DaemonFilter) never stat a
		// sibling's entry script, so readiness must not block on one either
		// (bot review readiness.go:366). c.daemon=="" is the cwd-independent
		// ABSOLUTE target that applies to EVERY daemon (entryScriptCheckTargets
		// leaves it unfiltered, see the daemon "" comment) — never skip it.
		if scope.DaemonFilter != "" && c.daemon != "" && c.daemon != scope.DaemonFilter {
			continue
		}
		if !c.resolvable {
			// Relative entry script + non-absolute daemon cwd: known-tolerated
			// non-convergence — the launch cwd is unknowable here. Surface as an
			// ADVISORY (Optional) so it neither blocks Ready nor reports a verdict
			// for the wrong directory (Codex #377 r18; see the package-doc
			// in-scope criterion).
			add(ReadinessRequirement{
				Name:     "script: " + c.label,
				OK:       false,
				Optional: true,
				Reason:   "relative entry script with no absolute daemon cwd — the daemon inherits an unpredictable working directory, so the script cannot be verified here",
				Fix:      "Make base_args[0] absolute, or set an absolute daemon cwd, so readiness can verify the entry script exists.",
			})
			continue
		}
		if ok, reason := entryScriptStatus(c.path); !ok {
			add(ReadinessRequirement{
				Name:     "script: " + c.label,
				OK:       false,
				Optional: launcherOptional,
				// Reason names only the script BASENAME + the normalized launcher
				// (never the absolute host path), which the GUI renders (Codex
				// pre-catch r9 / r14).
				Reason: fmt.Sprintf("the %s entry script %q %s", normalizeLauncher(m.Command), filepath.Base(c.path), reason),
				Fix:    "Install/clone the server so the manifest's base_args[0] script path exists and points at a file (not a directory), then re-run install.",
			})
		} else {
			add(ReadinessRequirement{Name: "script: " + c.label, OK: true})
		}
	}

	// uvx/uv with a `git+` source (e.g. serena's `--from git+https://…@<sha>`)
	// shells out to the `git` binary to clone the pinned commit; uv does NOT
	// vendor git, so uvx-on-PATH does not prove git is. Surface it so a host
	// with uv but no git is not reported ready (Codex pre-catch r9).
	if manifestNeedsGit(m) {
		gdisp, gfix := LauncherGuidance("git")
		if !binaryAvailable("git") {
			add(ReadinessRequirement{Name: "binary: " + gdisp, OK: false, Optional: launcherOptional,
				Reason: "git is required to fetch the uvx git+ source but is not on PATH", Fix: gfix})
		} else {
			add(ReadinessRequirement{Name: "binary: " + gdisp, OK: true})
		}
	}

	// Canonical mcphub binary — Preflight ALWAYS requires it (even on the
	// remote-http branch), so a fresh / dev-run host without `mcphub setup`
	// passes the launcher checks yet fails the install immediately. Mirror it
	// (Codex #377). In the live GUI mcphub is the running binary, so this is
	// only red on a not-yet-set-up host.
	if _, err := ensureCanonicalMcphubPresent(); err != nil {
		_, mfix := LauncherGuidance("mcphub")
		add(ReadinessRequirement{Name: "mcphub binary", OK: false, Reason: "canonical mcphub binary not installed", Fix: mfix})
	} else {
		add(ReadinessRequirement{Name: "mcphub binary", OK: true})
	}

	// Declared required external binaries (e.g. gdb-mcp → required_binaries:
	// [gdb]). These run THROUGH mcphub, so the launcher check passes even when
	// the actual backend tool is absent; surface them so the server is not
	// reported ready while its tool is missing (Codex #377).
	//
	// Server-level RequiredBinaries are BLOCKING (the server cannot function
	// without them). Per-LANGUAGE RequiredBinaries (workspace-scoped LSP
	// manifests) are ADVISORY/Optional: a manifest's `languages` are
	// ALTERNATIVES selected per workspace, so a Go-only workspace must not be
	// marked not-ready because rust-analyzer / fortls / tsserver are absent
	// (Codex #377). They are surfaced (with a Fix) but do not block Ready.
	// The package-level binaryAvailable is the SINGLE OWNER (shared with
	// Preflight) of "is this external binary runnable" — it honors the native-
	// debugger discovery for gdb/lldb (DebuggerDirs: MCPHUB_DEBUGGER_TOOLCHAIN_DIR
	// + probed MSYS2 bins, so a gdb present in a debugger dir but NOT on the GUI
	// process PATH still resolves) and checks executability via exec.LookPath
	// (Codex #377 r8/r13/r17). Called directly below — no local alias.
	seenBin := map[string]bool{}
	addBin := func(bin string, optional bool) {
		if bin == "" || seenBin[bin] {
			return
		}
		seenBin[bin] = true
		bdisp, bfix := LauncherGuidance(bin)
		if !binaryAvailable(bin) {
			add(ReadinessRequirement{Name: "binary: " + bdisp, OK: false, Optional: optional,
				// Basename only: required_binaries entries are free-form and may
				// be absolute; the GUI renders Reason verbatim (Codex #377
				// merge-gate P3).
				Reason: fmt.Sprintf("%q not found on PATH", filepath.Base(bin)), Fix: bfix})
		} else {
			add(ReadinessRequirement{Name: "binary: " + bdisp, OK: true})
		}
	}
	for _, bin := range m.RequiredBinaries {
		addBin(bin, false) // server-level: blocking
	}
	for _, lang := range m.Languages {
		for _, bin := range lang.RequiredBinaries {
			addBin(bin, true) // per-language: advisory (alternatives per workspace)
		}
	}

	// Conditional native-debugger bridge readiness. A stdio-bridge whose
	// base_args are ["lldb-bridge", "<addr>"] DIALS <addr> first and only
	// spawns + needs a local lldb when nothing is already listening there
	// (internal/lldb/bridge.go). So readiness is CONDITIONAL: OK when a listener
	// is already up on <addr> OR an lldb binary is available; NOT-ok only when
	// BOTH are absent. An unconditional required_binaries:[lldb] over-blocks the
	// listener case (Codex #377 r9); declaring NO check over-reports ready while
	// the daemon then fails on a bare host (Codex #377 r12). gdb-bridge has no
	// <addr> arg — it always spawns gdb directly — so it keeps an unconditional
	// required_binaries:[gdb] and does NOT match this branch.
	if m.Transport == config.TransportStdioBridge && len(m.BaseArgs) >= 2 && m.BaseArgs[0] == "lldb-bridge" {
		addr := m.BaseArgs[1]
		disp, dfix := LauncherGuidance("lldb")
		_, _, addrErr := lldb.ParseHostPort(addr)
		switch {
		case addrErr != nil:
			// The lldb-bridge subcommand rejects a malformed host:port BEFORE
			// spawning (lldb.ParseHostPort is its own validator), so a daemon with
			// e.g. a bare host or a port > 65535 exits immediately — never mark it
			// ready on the listener/binary branch (Codex #377 r15). addr is the
			// operator's own manifest value echoed back so they can fix it.
			add(ReadinessRequirement{
				Name:   "debugger: " + disp,
				OK:     false,
				Reason: fmt.Sprintf("the lldb-bridge address %q is not a valid host:port — the bridge rejects it before spawning", addr),
				Fix:    "Set base_args[1] to a valid host:port (e.g. localhost:47000) in the manifest.",
			})
		case bridgeListenerUp(addr):
			add(ReadinessRequirement{Name: "debugger: " + disp + " (listener up)", OK: true})
		case binaryAvailable("lldb"):
			add(ReadinessRequirement{Name: "debugger: " + disp, OK: true})
		default:
			add(ReadinessRequirement{
				Name: "debugger: " + disp,
				OK:   false,
				// addr is a manifest-declared loopback address (e.g.
				// localhost:47000), not a host path — safe to render.
				Reason: fmt.Sprintf("no MCP listener on %s and no lldb binary found — the lldb-bridge needs one or the other", addr),
				Fix:    dfix + " — OR start an lldb MCP listener on " + addr + " first, then re-run install.",
			})
		}
	}

	// Daemon ports free — mirror Preflight's FOREIGN-collision check (a port
	// held by OUR own daemon is fine: idempotent reinstall). A green report
	// followed by an install that fails on a held port is exactly the lie this
	// surface must not tell (Codex #377).
	// A FIXED daemon port must be valid + bindable; pool daemons never reach
	// here (m.Daemons is empty for dynamic-pool manifests). fixedPortStatus is
	// the single owner shared with Preflight — it range-checks (rejecting 0 /
	// > 65535, which a failed TCP dial would read as "free") AND bind-probes via
	// the pool allocator's portAvailable, so a bound-but-not-listening port is
	// not reported free (Codex #377 r15/r16).
	for _, d := range m.Daemons {
		if scope.DaemonFilter != "" && d.Name != scope.DaemonFilter {
			continue
		}
		// A kind=companion daemon binds NO mcphub MCP port (Port==0 is valid — it
		// listens on its own port directly). Skip the fixed-port requirement so the
		// companion is not reported Ready=false for an out-of-range port that
		// Preflight now admits — keeping readiness in sync with install (Codex #381).
		if m.Kind == config.KindCompanion {
			continue
		}
		portName := fmt.Sprintf("port %d (%s)", d.Port, d.Name)
		if ok, reason := fixedPortStatus(d.Port, m.Name, d.Name, false); !ok {
			add(ReadinessRequirement{Name: portName, OK: false, Reason: reason,
				Fix: "Set a valid free fixed port (1..65535) for this daemon in the manifest."})
		} else {
			add(ReadinessRequirement{Name: portName, OK: true})
		}
		if m.Transport == config.TransportNativeHTTP {
			internal := d.Port + config.NativeHTTPInternalPortOffset
			iname := fmt.Sprintf("internal port %d (%s)", internal, d.Name)
			if ok, reason := fixedPortStatus(internal, m.Name, d.Name, true); !ok {
				add(ReadinessRequirement{Name: iname, OK: false, Reason: reason,
					Fix: "Free the port or change the daemon port (internal = external + offset, both must be 1..65535)."})
			} else {
				// Emit the green row too, so a healthy internal port is visible
				// and the report's requirement set is symmetric (Codex pre-catch r9).
				add(ReadinessRequirement{Name: iname, OK: true})
			}
		}
	}

	// Workspace-scoped (dynamic-pool) registration allocates from a PortPool.
	// Admission/readiness keep only scope-independent blocking checks here:
	// native-http offset overflow and registry read/resolve failure. AllocatePort
	// remains the registration-time owner for OS-bound/foreign-bound/free-port
	// decisions.
	var allocatedPorts map[int]bool
	var registryLoadErr error
	// register resolves the path (DefaultRegistryPath) AND loads it before
	// allocating; BOTH can fail (no resolvable home/state dir in a headless
	// session → path error; corrupt file → load error). Either leaves register
	// unable to allocate, so capture it and let checkPool surface it as
	// blocking rather than silently reporting the pool ready while register
	// fails (Codex #377 r5/r7).
	allocatedPorts, registryLoadErr = loadRegistryAllocatedPorts()
	checkPool := func(pp manifestPortPool, nativeHTTP bool) {
		p := pp.pool
		if p == nil || p.End < p.Start {
			return
		}
		overflow := nativeHTTPPoolOverflows(p, nativeHTTP)
		if overflow {
			add(ReadinessRequirement{
				Name:   portPoolName(p),
				OK:     false,
				Reason: nativeHTTPPoolOverflowReason(p),
				Fix:    nativeHTTPPoolOverflowFix(),
			})
		}
		if registryLoadErr != nil {
			add(ReadinessRequirement{
				Name: portPoolName(p),
				OK:   false,
				// Reason is GUI-rendered — do not echo registryLoadErr, which
				// wraps the absolute workspaces.yaml path (Codex pre-catch r9).
				Reason: "the workspace registry could not be read or resolved (register reads it before allocating a pool port)",
				Fix:    "Fix or remove the corrupt workspaces.yaml registry (register reads it before allocating a pool port).",
			})
			return
		}
		if overflow {
			return
		}
		name := portPoolName(p)
		if portPoolFullyAllocatedByRegistry(p, allocatedPorts) {
			add(ReadinessRequirement{Name: name, OK: false, Optional: true,
				Reason: "no port in the workspace pool is free for a NEW workspace (registry-allocated by existing workspaces); existing workspaces and reinstall are unaffected",
				Fix:    "Free a pool port or widen the pool in the manifest before registering a new workspace."})
		}
	}
	// Serena's dynamic-pool manifest is transport: native-http, so its
	// materialized proxies bind external+offset upstream — mNative drives the
	// offset check for both the server pool and the daemon-template pool.
	mNative := m.Transport == config.TransportNativeHTTP
	for _, pp := range manifestPortPools(m) {
		checkPool(pp, mNative)
	}

	// A vault that EXISTS but is unreadable/undecryptable is BLOCKING for a
	// manifest that uses secret refs — the daemon (OpenVaultOptional +
	// HasSecretRef) fails the spawn, so readiness must NOT report it ready as a
	// merely-optional unset key. A truly-absent vault is fine (secrets optional)
	// (Codex #377 r5).
	if secrets.HasSecretRef(m.Env) {
		if _, verr := secrets.OpenVaultOptional(secrets.DefaultKeyPath(), secrets.DefaultVaultPath()); verr != nil {
			add(ReadinessRequirement{
				Name: "secrets vault",
				OK:   false,
				// Redacted: verr wraps the absolute vault/key file path (Codex
				// pre-catch r9).
				Reason: "the secrets vault exists but could not be read or decrypted",
				Fix:    "Fix or remove the corrupt vault — a secret-using server fails to start when it cannot be read.",
			})
		}
	}

	// Declared secrets — reported PER KEY so the GUI can offer each as an
	// inline "fill this field at install" prompt (the operator's request:
	// "секреты нужно явно предлагать в конкретные поля при установке").
	// Secrets are OPTIONAL BY DEFAULT: an unset key is advisory (Optional=true),
	// NOT a blocker — the server still installs + spawns (the env var is omitted)
	// and reports its own missing-key if it actually needs it. The EXCEPTION is a
	// key listed in m.RequiredSecrets (the opt-in install gate): that one is
	// BLOCKING (Optional=false, RED), so an unset required key flips Ready=false —
	// in PARITY with the AdmissionCheck "required-secret" blocking finding. The
	// classification derives from the SAME requiredSecretSet owner the admission
	// finding consults, so readiness and admission can never disagree on which
	// secrets block.
	reqSecretSet := requiredSecretSet(m)
	for k, v := range m.Env {
		if !strings.HasPrefix(v, "secret:") {
			continue
		}
		key := strings.TrimPrefix(v, "secret:")
		required := reqSecretSet[key]
		req := ReadinessRequirement{Name: "secret: " + key, Optional: !required}
		if err := checkSecretRefs(map[string]string{k: v}); err != nil {
			req.OK = false
			if required {
				// Required (opt-in install gate): the server hard-exits on startup
				// without it, so this is a BLOCKER, not an advisory.
				req.Reason = "is REQUIRED but could not be resolved from the vault — the server exits on startup when it is unset"
				req.Fix = fmt.Sprintf("Set %s on the Secrets screen or `mcphub secrets set %s` before installing.", key, key)
			} else {
				// Neutral wording: checkSecretRefs errors for BOTH "key not set" and
				// "vault unreadable", so do not assert "not set" — the dedicated
				// "secrets vault" requirement above covers the unreadable case
				// (Codex pre-catch r9).
				req.Reason = "could not be resolved from the vault (optional — set it, or fix the vault if it exists but is unreadable; the server otherwise runs without it)"
				req.Fix = fmt.Sprintf("Enter %s at install, or set it later via the Secrets screen / `mcphub secrets set %s`.", key, key)
			}
		} else {
			req.OK = true
		}
		add(req)
	}
	// `file:` env refs are ALWAYS fatal at spawn: the production resolver is
	// built with a nil local-config map (daemon.go / daemon_serena.go pass
	// NewResolver(vault, nil)), so a `file:` ref can never resolve and the
	// daemon fails to start — surface it as BLOCKING. `$VAR` refs are NOT
	// checked here: the daemon resolves them in its OWN spawn environment
	// (os.Environ + the daemon env overlay), which differs from this
	// GUI/readiness process, so a $VAR unset HERE but set THERE would be a
	// false-negative — env-dependent refs are left to the spawn gate (Codex
	// #377 r9). secret: refs are surfaced per-key above.
	for k, v := range m.Env {
		if !strings.HasPrefix(v, "file:") {
			continue
		}
		add(ReadinessRequirement{
			Name:   "env: " + k,
			OK:     false,
			Reason: fmt.Sprintf("the file: env ref %q cannot be resolved (mcphub has no local config map), so the daemon fails to start", k),
			Fix:    "Replace the file: env ref with a secret: ref (vault) or a literal value in the manifest.",
		})
	}

	// Remote-http manifests carry REQUIRED vault values as ${secret:KEY}
	// placeholders in url + headers (NOT m.Env). buildRemoteHTTPPlan expands
	// them and FAILS the install on a missing one, so — unlike the optional
	// stdio env secrets above — these are BLOCKING (Codex #377).
	remoteSecretKeys := map[string]struct{}{}
	scan := func(s string) {
		for _, mt := range SecretPlaceholderRE.FindAllStringSubmatch(s, -1) {
			remoteSecretKeys[mt[1]] = struct{}{}
		}
	}
	scan(m.URL)
	for _, hv := range m.Headers {
		scan(hv)
	}
	for key := range remoteSecretKeys {
		req := ReadinessRequirement{Name: "secret (remote): " + key}
		if err := checkSecretRefs(map[string]string{"_": "secret:" + key}); err != nil {
			req.OK = false
			req.Reason = "required for the remote endpoint URL/headers but not set in the vault"
			req.Fix = fmt.Sprintf("Enter %s at install, or `mcphub secrets set %s` — the install fails without it.", key, key)
		} else {
			req.OK = true
		}
		add(req)
	}
	// Install-plan dry-run — the SINGLE-OWNER authoritative check. BuildPlan
	// is pure (validates + returns before any side effect) and runs the exact
	// binding / url_path / remote-matrix validation the real install performs:
	// non-remote client_bindings (unknown daemon, unsupported client, invalid
	// url_path), remote-http client_bindings against the adapter matrix, and
	// ExpandSecrets over url+headers (malformed `${secret:}`, CR/LF, missing
	// remote secret). Calling it here instead of re-deriving each check keeps
	// readiness from ever green-lighting an install the planner rejects, and
	// avoids duplicating gate logic that drifts (Codex #377 r2/r3/r4). The
	// per-key secret + dependency requirements above stay for the structured,
	// user-actionable inline prompts; this is the one catch-all blocker.
	// Validate EXACTLY the operator's effective default-install client set —
	// the same scope a normal `mcphub install` uses. NOT the bare
	// BuildPlan(m,"") compile-time trio (which would MISS a bad binding on a
	// client the operator persisted into their default set), and NOT
	// IncludeAllClients (which would validate OPT-IN bindings a default install
	// never touches → a false Ready=false on a bad opt-in binding, Codex #377
	// r7). DefaultInstallClientNamesEffectiveIn reads gui-preferences.yaml (it
	// derefs no *API state, so the zero value calls it); fall back to the
	// compile-time default on a read error.
	clientScope := clients.DefaultInstallClientNames()
	if eff, cerr := (&API{}).DefaultInstallClientNamesEffectiveIn(SettingsPath()); cerr == nil && len(eff) > 0 {
		clientScope = eff
	}
	// Scope the dry-run to the SAME daemon the real install targets. A
	// daemon-filtered install (scope.DaemonFilter set) only validates the
	// chosen daemon's bindings; a sibling's invalid url_path / unknown-daemon
	// binding must not block readiness when BuildPlanWithOpts(DaemonFilter)
	// would skip it (bot review readiness.go:366). Empty filter = full install
	// = validate every daemon, unchanged from the global path.
	if _, err := BuildPlanWithOpts(m, BuildPlanOpts{DefaultClientsOverride: clientScope, DaemonFilter: scope.DaemonFilter}); err != nil {
		add(ReadinessRequirement{
			Name: "install plan",
			OK:   false,
			// redactErrorDetail strips absolute paths (e.g. the canonical
			// ~/.local/bin/mcphub path ensureCanonicalMcphubPresent surfaces)
			// + token-like runs from the GUI-rendered Reason (Codex #377 r9).
			Reason: "the install planner rejects this manifest: " + redactErrorDetail(err.Error()),
			Fix:    "Fix the manifest client_bindings / url / headers per the error above (the install gate runs the same validation).",
		})
	}

	// daemon_template (dynamic-pool) manifests: BuildPlan does NOT exercise the
	// InstallParsedManifest admission gates (native-http transport, non-empty
	// daemon_template.context, no duplicate --context). Run the SAME validator
	// (single owner) so readiness mirrors that install path too (Codex #377 r6).
	if err := validateDynamicPoolManifest(m); err != nil {
		add(ReadinessRequirement{
			Name:   "dynamic-pool",
			OK:     false,
			Reason: err.Error(),
			Fix:    "Fix the daemon_template manifest: native-http transport, a non-empty daemon_template.context, and no --context token in base_args/extra_args_template.",
		})
	}

	return rep
}

// CheckServerReadinessByName resolves a server's manifest embed-first (the same
// source the installer uses) and runs CheckServerReadiness. The GUI
// /api/server/readiness endpoint calls this so the operator sees per-server
// readiness — with guided fixes — BEFORE installing, instead of discovering a
// missing dependency as a downstream cryptic failure. Returns an error only
// when the manifest cannot be resolved or parsed (an unknown server name); a
// resolvable manifest always yields a report (possibly Ready=false).
func CheckServerReadinessByName(name string) (*ReadinessReport, error) {
	return CheckServerReadinessByNameWithScope(name, AdmissionScope{})
}

// CheckServerReadinessByNameWithScope resolves a server manifest and runs a
// scope-aware readiness report for partial installs.
func CheckServerReadinessByNameWithScope(name string, scope AdmissionScope) (*ReadinessReport, error) {
	data, err := loadManifestYAMLEmbedFirst(name)
	if err != nil {
		return nil, fmt.Errorf("resolve manifest %q: %w", name, err)
	}
	m, err := parseManifestForName(name, data)
	if err != nil {
		return nil, fmt.Errorf("parse manifest %q: %w", name, err)
	}
	return CheckServerReadinessWithScope(m, scope), nil
}

// AllServerReadiness resolves every embedded/installed server manifest and
// returns a readiness report per server, so the GUI can show a fleet-wide
// "what needs fixing before this works" view. A manifest that fails to
// resolve/parse is SURFACED as a not-ready report with a "manifest parse"
// requirement rather than dropped: listManifestNamesEmbedFirst unions embedded
// AND disk manifests, so a parse failure can be an operator-facing broken
// CUSTOM server, and silently omitting it would make the fleet view look
// complete while hiding the server that needs attention (Codex #377).
func AllServerReadiness() []*ReadinessReport {
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return nil
	}
	out := make([]*ReadinessReport, 0, len(names))
	for _, name := range names {
		rep, err := CheckServerReadinessByName(name)
		if err != nil {
			// err wraps the manifest's absolute disk path (os.PathError); this
			// report is GUI-rendered, so do NOT echo it — name only the server
			// (Codex pre-catch r9). The full error is the caller's to log.
			out = append(out, &ReadinessReport{
				Server: name,
				Ready:  false,
				Requirements: []ReadinessRequirement{{
					Name:   "manifest: " + name,
					OK:     false,
					Reason: "the manifest could not be loaded or parsed",
					Fix:    "Fix the manifest YAML (a custom server under the manifest dir), or remove it.",
				}},
			})
			continue
		}
		out = append(out, rep)
	}
	return out
}

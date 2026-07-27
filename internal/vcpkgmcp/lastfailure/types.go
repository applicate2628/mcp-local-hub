// Package lastfailure implements vcpkg_last_failure, the flagship tool from
// work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md.
//
// # Layering (architectural correction, applied 2026-07-25)
//
// build_failed.log is a CUSTOM layer one operator's own wrapper script
// writes on top of vcpkg — stock vcpkg does not produce it. Building this
// tool around requiring that file would make it work only for that one
// wrapper and break the moment the wrapper's shape changes, or on any plain
// vcpkg install. So:
//
//   - PRIMARY source, always available: the buildtrees layout itself,
//     `<buildtrees-root>/<port>/<phase>[-<triplet>][-<config>][-<artifact>]-{out,err}.log`,
//     written by vcpkg itself (see buildtrees.go doc comment for the exact
//     naming pattern, derived from a real observed tree, not invented).
//   - OPTIONAL enrichment: a build_failed.log-shaped wrapper file, accepted
//     via an explicit build_failed_log parameter (never auto-discovered —
//     the wrapper may write it anywhere, there is no vcpkg convention to
//     search for). When present it gives the invocation context cheaply
//     (overlay chain, --x-buildtrees-root, --triplet, --x-install-root,
//     exit_code, failed_ports) and is the HIGHEST-fidelity source for that
//     context because it is the actual invocation, not a re-derivation.
//     When absent, unreadable, or malformed, the tool degrades to the
//     native buildtrees path and says so via Notes — it never fails or
//     fabricates an answer because a non-standard file is missing.
//
// context_source is always a LIST (never a single value) naming every
// source actually consulted, so the answer's evidentiary basis is visible.
package lastfailure

import (
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Reason is populated only when Status == evidence.StatusUnknown. Closed enum.
//
// # Naming rule (2026-07-26 vocabulary audit)
//
// Every value here names WHAT THE TOOL OBSERVED, never what it inferred about
// the world. Three observation kinds are allowed, and they must stay
// distinguishable in the name itself:
//
//   - a VERIFIED FACT — the tool looked and saw the thing it names
//     (build_interrupted: an interrupt marker was actually read);
//   - an OBSERVATION ABOUT ITS OWN INPUT — something was not supplied
//     (port_not_specified);
//   - an OBSERVATION ABOUT ITS OWN SEARCH — something was not found where it
//     looked (port_dir_not_found, no_diagnostic_found).
//
// The last two may never be phrased as a fact about the build. A value that
// converts "I was not given X" or "I did not find X" into "X is not so" is
// the defect this rule exists to prevent; it hands the operator a confident
// denial in place of an honest "I do not know", which is strictly worse than
// saying nothing because it stops them looking.
type Reason string

const (
	// ReasonVcpkgRootNotResolved: no vcpkg root (and hence no buildtrees
	// location) could be RESOLVED — either nothing was supplied and discovery
	// found no unambiguous candidate, or an explicit root was supplied and
	// discovery refused it (no vcpkg binary under it, or the probe failed).
	//
	// Named for the tool's own search, not for its input: the predecessor
	// value `root_not_specified` also fired for an explicitly SUPPLIED root
	// that discovery rejected, telling the caller they had specified nothing
	// when they had. Which discovery rule refused it is available in full
	// from vcpkg_discover_root, which owns that vocabulary.
	ReasonVcpkgRootNotResolved Reason = "vcpkg_root_not_resolved"
	// ReasonPortNotSpecified: port omitted, and no wrapper file (or a
	// wrapper with != 1 failed port entries) was available to infer it.
	ReasonPortNotSpecified Reason = "port_not_specified"
	// ReasonMultipleFailedPortsAmbiguous: port omitted, wrapper file parsed
	// and named MORE than one failed port. The tool never silently picks.
	ReasonMultipleFailedPortsAmbiguous Reason = "multiple_failed_ports_ambiguous"
	// ReasonBuildtreesRootAbsent: the buildtrees root does not exist on disk
	// at all (a VERIFIED absence — evidence.PresenceAbsent, never a failed
	// probe, which is ReasonBuildtreesRootUnreadable).
	//
	// The most common CAUSE is --clean-buildtrees-after-build removing it
	// after the run; verified real case: a wrapper invocation naming that
	// flag whose --x-buildtrees-root no longer existed post-run. But the
	// probe establishes the absence, NOT its cause — a wrong --x-buildtrees-
	// root, a wrong triplet, or a tree on an unmounted volume produce the
	// identical observation. The predecessor value `buildtrees_cleaned`
	// asserted the cause, which sent an operator whose real mistake was a
	// mistyped root hunting for a cleanup flag instead. Callers wanting the
	// remedy for the common case get it from the tool description.
	ReasonBuildtreesRootAbsent Reason = "buildtrees_root_absent"
	// ReasonPortDirNotFound: the buildtrees root DOES exist, but this
	// specific port never has a subdirectory under it — a different cause
	// than cleanup (wrong port name, wrong triplet, wrong root, or the
	// port genuinely never started building here).
	ReasonPortDirNotFound Reason = "port_dir_not_found"
	// ReasonNoPhaseLogsFound: the port directory exists but contains none
	// of the recognized phase log files (extract/config/install *-out/err
	// logs) — only ancillary files like vcpkg_abi_info.txt. Verified real
	// case: a port in a --clean-buildtrees-after-build run whose build
	// never reached the extract phase (or whose logs were also cleaned).
	ReasonNoPhaseLogsFound Reason = "no_phase_logs_found"
	// ReasonNoDiagnosticFound: phase logs were read, but no line matched a
	// recognized diagnostic shape (MSVC or GCC/Clang) in any of them —
	// e.g. libtool erased the exit code and the real error is unstructured
	// prose. log_paths are still returned so a caller can read further.
	ReasonNoDiagnosticFound Reason = "no_diagnostic_found"
	// ReasonTripletAmbiguous: triplet omitted, and the port directory's
	// unambiguous marker files (stdout-<triplet>.log, <triplet>.vcpkg_abi_info.txt)
	// named MORE than one distinct triplet — never silently picked.
	ReasonTripletAmbiguous Reason = "triplet_ambiguous"
	// ReasonBuildInterrupted: a phase log carries an operator/process
	// interrupt marker as a WHOLE LINE ("User interrupt", "ninja: build
	// stopped: interrupted by user.") rather than a genuine build defect. The
	// whole-line requirement is load-bearing, not stylistic: this reason
	// OUTRANKS every other verdict, so a marker matched mid-line — inside a
	// path, an echoed `ninja -v` command line, or a source line quoted back by
	// a compiler diagnostic — would suppress a real failure. See
	// interruptMarkers in diagnostics.go for the producer evidence. Verified
	// real case (scout pass, boost-thread\config-wingpl-out.log): a
	// "FAILED: [code=1]" line immediately followed by "User interrupt" —
	// reporting this as a build failure would misdirect the operator
	// toward fixing a bug that does not exist; the build was stopped, not
	// broken. Deliberately distinct from ReasonNoDiagnosticFound.
	ReasonBuildInterrupted Reason = "build_interrupted"

	// --- Evidence-integrity reasons -------------------------------------
	// The five reasons below all say "the tool declined to conclude because
	// the evidence does not settle the question". They are kept as SEPARATE
	// values rather than one blanket `insufficient_evidence` because each
	// names a different operator remedy: pass an absolute path, fix an ACL,
	// supply a complete wrapper, read the primary logs, distrust a probe.
	// A single collapsed reason would be auditable but useless.

	// ReasonRelativeRoot: a root-like parameter (root / buildtrees_root, or
	// a --x-buildtrees-root recovered from a wrapper command line) is not
	// absolute. A relative root silently binds to the HUB DAEMON's working
	// directory — not the caller's, and not the one the recorded vcpkg
	// invocation used — so every path derived from it points at an
	// unrelated tree while the answer still looks confident.
	ReasonRelativeRoot Reason = "relative_root"
	// ReasonInvalidPortName: port is not a single legal vcpkg port-name
	// segment (see portNameRE), or its joined path would escape the
	// buildtrees root.
	ReasonInvalidPortName Reason = "invalid_port_name"
	// ReasonBuildtreesRootUnreadable: the buildtrees root could not be
	// probed at all (permission denied, I/O error). Deliberately NOT
	// ReasonBuildtreesCleaned — "cleaned" is a verified-absence claim and
	// must never be manufactured from a failure to look.
	ReasonBuildtreesRootUnreadable Reason = "buildtrees_root_unreadable"
	// ReasonPortDirUnreadable: the port directory could not be probed at
	// all. Sibling of ReasonBuildtreesRootUnreadable, same rationale
	// relative to ReasonPortDirNotFound.
	ReasonPortDirUnreadable Reason = "port_dir_unreadable"
	// ReasonPhaseLogUnreadable: at least one phase log that is relevant to
	// the verdict exists but could not be read. Any such log can change the
	// answer — it may hold a later phase's error (changing which phase is
	// reported), the only error in the port (changing unknown to failed),
	// or an interrupt marker (changing a failure into a stopped build) —
	// so a confident verdict cannot be produced while one is unread.
	ReasonPhaseLogUnreadable Reason = "phase_log_unreadable"
	// ReasonPhaseLogSizeLimitExceeded: a relevant log is larger than this
	// package will materialize (maxLogBytes), or its content defeated the
	// line scanner, so part of it was never examined. Sibling of
	// ReasonPhaseLogUnreadable and fail-closed for the same reason: the
	// unread tail can hold a later error, the only error, or an interrupt
	// marker. Kept SEPARATE because the operator remedy differs — this one
	// says "read the log yourself, it is too big for me", not "fix an ACL".
	// Named for the observation (a limit of ours was exceeded), not for a
	// judgement that the file is "too large".
	ReasonPhaseLogSizeLimitExceeded Reason = "phase_log_size_limit_exceeded"
	// ReasonNoFailureDiagnostic: recognized diagnostics WERE found, but not
	// one of them has error severity. Warnings and notes are evidence, not
	// failures. Deliberately distinct from ReasonNoDiagnosticFound (nothing
	// matched at all): here the caller gets the warnings back and can judge
	// them, which is a materially different situation.
	ReasonNoFailureDiagnostic Reason = "no_failure_diagnostic"
	// ReasonCapabilityProbeOnly: the only error-severity diagnostic came
	// from a CMakeConfigureLog.yaml.log try_compile dump, and no primary
	// phase log independently identified a failure. A failing try_compile is
	// the NORMAL mechanism of CMake feature detection (FindThreads.cmake
	// probing for pthread.h fails on every MSVC build ever made), so it is
	// evidence of a capability probe, not of a broken port.
	ReasonCapabilityProbeOnly Reason = "capability_probe_only"
)

// Note is a small closed vocabulary of non-authoritative observations
// attached to a result regardless of its Status — e.g. "the wrapper file
// was present but malformed, so it was ignored" even though buildtrees
// alone still produced an ok answer. Kept closed (not free text) so it
// stays auditable.
//
// The Reason naming rule above binds these identically, and notes are where
// it was most often broken: a note is easy to write as a conclusion because
// it does not carry the verdict. It still reaches the operator.
type Note string

const (
	// NoteWrapperNotSupplied: the caller passed no build_failed_log. The
	// tool NEVER auto-discovers one (there is no vcpkg convention naming
	// where a wrapper would put it), so it has not looked and cannot know
	// whether such a file exists. The predecessor value `wrapper_absent`
	// asserted the file's absence from the parameter's absence.
	NoteWrapperNotSupplied Note = "wrapper_not_supplied"
	// NoteWrapperNotFound: a build_failed_log path WAS supplied and does not
	// exist on disk. A verified absence of that specific path, distinct from
	// NoteWrapperNotSupplied above (nothing was looked for) and from
	// NoteWrapperUnreadable below (looked, blocked).
	NoteWrapperNotFound Note = "wrapper_not_found"
	// NoteWrapperUnreadable: a supplied build_failed_log exists but could not
	// be read (permission denied, sharing violation, I/O error). NOT
	// malformed — the tool never saw its content, so it can say nothing about
	// its shape; folding this into NoteWrapperMalformed sent the operator to
	// fix a wrapper script when the actual remedy is an ACL.
	NoteWrapperUnreadable Note = "wrapper_unreadable"
	// NoteWrapperMalformed: the wrapper file WAS read end to end and nothing
	// recognizable was recovered from it. A verified fact about content.
	NoteWrapperMalformed            Note = "wrapper_malformed_ignored"
	NoteWrapperUsedForContext       Note = "wrapper_used_for_invocation_context"
	NotePortAutoSelectedFromWrapper Note = "port_auto_selected_from_wrapper_single_failure"
	NoteTripletAutoSelectedFromDir  Note = "triplet_auto_selected_from_buildtrees_dir"
	NoteOverlayChainFromWrapper     Note = "overlay_chain_from_wrapper_invocation"
	NoteOverlayChainFromEnv         Note = "overlay_chain_from_env"
	// NoteOverlayChainFromParam: the chain came from the caller's own
	// overlays parameter.
	NoteOverlayChainFromParam Note = "overlay_chain_from_explicit_param"
	// NoteOverlayChainFromVcpkgConfiguration: the chain came from an
	// overlay-ports array in a vcpkg-configuration.json under the resolved
	// vcpkg root. Previously this ALSO reported
	// overlay_chain_from_explicit_param, naming a parameter the caller never
	// passed — a provenance claim the caller cannot verify by checking the
	// source it names.
	NoteOverlayChainFromVcpkgConfiguration Note = "overlay_chain_from_vcpkg_configuration"
	// NoteOverlayChainNotSupplied: no overlay chain was supplied by the
	// caller, recovered from a wrapper invocation, or found in any source
	// this call consulted.
	//
	// It says NOTHING about whether the BUILD used overlays, because this
	// tool cannot know that: the buildtrees layout does not record the
	// overlay chain, so absent a wrapper file there is no artifact to read it
	// from. The predecessor value `overlay_chain_none_builtin_ports_only`
	// asserted the build used builtin ports only. Verified field failure
	// (2026-07-26): it was emitted for a build that actually used FOUR
	// overlay directories, purely because the caller had not passed the
	// overlays parameter.
	NoteOverlayChainNotSupplied Note = "overlay_chain_not_supplied"
	// NoteOverlayChainConfigNotConsulted: the vcpkg-configuration.json
	// fallback could not be consulted at all because no vcpkg root is known
	// (buildtrees was located directly, from buildtrees_root or a wrapper's
	// --x-buildtrees-root, so root discovery never ran). Emitted ALONGSIDE
	// NoteOverlayChainNotSupplied so the two "no chain" outcomes stay
	// distinguishable: one consulted every documented source, the other could
	// not reach one of them.
	NoteOverlayChainConfigNotConsulted Note = "overlay_chain_vcpkg_configuration_not_consulted_no_root"
	// NoteExactCommandNotRecovered: no reproducible top-level vcpkg
	// invocation could be recovered. vcpkg does not record its own argv
	// anywhere in buildtrees, so without a wrapper file there is nothing to
	// read it from. Stated explicitly rather than left as a bare empty field,
	// so "the tool could not recover it" is not confused with "the tool
	// forgot" — and so the remedy (pass build_failed_log) is visible.
	NoteExactCommandNotRecovered Note = "exact_command_not_recovered"
	// NoteWrapperConfirmsNoFailure: the wrapper's failed_ports list is
	// present and does NOT name this port — real evidence the port did
	// not fail in that run, not a guess.
	NoteWrapperConfirmsNoFailure Note = "wrapper_confirms_port_not_failed"
	// NoteDiagnosticFromCapabilityProbeLog: the reported diagnostic was
	// recovered only from a CMakeConfigureLog.yaml.log artifact (a
	// last-resort source, scanned only when the primary phase logs show
	// nothing). Real observed nesting (scout pass,
	// boost-atomic\config-wingpl-rel-CMakeConfigureLog.yaml.log): a
	// try_compile CAPABILITY PROBE (e.g. FindThreads.cmake checking for
	// pthread.h) commonly fails as a NORMAL part of feature detection —
	// a diagnostic from this source may describe a probe, not the port's
	// actual build failure. Surfaced so a caller does not over-trust it.
	NoteDiagnosticFromCapabilityProbeLog Note = "diagnostic_from_capability_probe_log"
	// NoteWrapperFailedPortsCompletenessUnproven: the wrapper's failed_ports
	// list could not be PROVEN exhaustive (scan error, missing
	// build_failed_count, or a count that disagrees with the number of
	// entries), so its silence about the queried port was NOT accepted as
	// proof the port did not fail. The tool fell through to buildtree
	// evidence instead.
	//
	// Named for the proof, not the list: the predecessor value
	// `wrapper_failed_ports_list_incomplete` asserted the list IS incomplete,
	// which the tool cannot know. A wrapper that simply never writes
	// build_failed_count emits a perfectly complete list that this guard
	// still (correctly) declines to rely on.
	NoteWrapperFailedPortsCompletenessUnproven Note = "wrapper_failed_ports_list_completeness_unproven"
)

// ContextSource names one input the answer actually rests on. Closed enum,
// always returned as a list (evidentiary basis, not a single-value pick).
type ContextSource string

const (
	SourceBuildtrees     ContextSource = "buildtrees"
	SourceWrapperSummary ContextSource = "wrapper_summary"
)

// Phase is the build phase a diagnostic (or the failure overall) belongs
// to. Values extract/patch/config/install mirror the exact file-phase
// tokens observed in a real buildtrees directory (scout pass over 618 real
// log files confirmed exactly these four phases, plus the wrapper-produced
// "stdout" narration stream which is not a vcpkg phase); build is inferred
// (not a separate log file) when a diagnostic inside an install-phase log
// has compiler/linker diagnostic shape rather than a CMake install-step
// message — vcpkg's own ninja invocation compiles AND installs in one
// "install" step, so a compile error physically lands in the install log.
type Phase string

const (
	PhaseExtract Phase = "extract"
	// PhasePatch: patch-<triplet>-<N>-{out,err}.log, N = 0-based patch
	// ordinal. Applying a port's patches sequentially, between extract and
	// configure.
	PhasePatch   Phase = "patch"
	PhaseConfig  Phase = "config"
	PhaseBuild   Phase = "build"
	PhaseInstall Phase = "install"
)

// DiagnosticTier says whether a recognized diagnostic NAMES A CAUSE or only
// SUMMARISES causes reported elsewhere in the same failure. Closed enum, part
// of the wire contract, and the secondary ranking key after severity.
//
// # Why severity alone is not enough (2026-07-27 field refinement)
//
// Severity-then-first-occurrence is right as far as it goes, but among
// error-severity lines some are pure consequences. Verified field failure: on
// a real clang-cl/lld-link link failure the headline was
//
//	clang-cl: error: linker command failed with exit code 1120
//
// while the actual cause sat third in the list:
//
//	lld-link: error: undefined symbol: __declspec(dllimport) gzopen_w
//
// The driver line reports only the exit status of a sub-tool it launched; it
// cannot tell the operator anything they can act on. Ranking specific ahead of
// aggregate puts the actionable line first without dropping either.
//
// # The sweep (every recognized shape, tier stated)
//
//	gccClangDiagRE  file:line:col: sev: msg          -> ALWAYS specific (names a source position)
//	msvcCompileDiagRE  file(line[,col]): sev [C]: msg -> ALWAYS specific (names a source position)
//	msvcLinkDiagRE  file : sev LNKnnnn: msg          -> aggregate iff LNKnnnn is in
//	                                                    aggregateLinkCodeRE, else specific
//	toolDiagRE      <driver>: sev: msg               -> aggregate iff msg matches
//	                                                    aggregateDriverMsgRE, else specific
//	ninjaFailedRE   FAILED: [code=N] <target>        -> ALWAYS aggregate (names the failed
//	                                                    target and an exit code, never a cause;
//	                                                    ninja prints it BEFORE the compiler
//	                                                    output it summarises, so in file order
//	                                                    it would otherwise always win)
//
// # Two tiers, not one, and a stricter third class that is not a tier
//
// An aggregate is a LEGITIMATE fallback headline: when it is the only error
// present it is strictly better than nothing, and the earlier round's
// `fparser_parse-opt.exe : fatal error LNK1120: 4 unresolved externals` case
// depends on exactly that. Causeless build-wrapper noise (NMAKE's U-series) is
// a STRICTER class and is deliberately NOT modelled as a third tier value: it
// carries no information at all, can never be a headline under any
// circumstance, and is therefore excluded one layer earlier by isWrapperNoise
// so it never becomes a Diagnostic. A tier value for it would be unreachable
// on the wire and would blur the two rules together.
type DiagnosticTier string

const (
	// TierSpecific: the line names a concrete cause an operator can act on —
	// a source position, an undefined symbol, an unopenable file.
	TierSpecific DiagnosticTier = "specific"
	// TierAggregate: the line only summarises other diagnostics, carrying a
	// COUNT or a sub-tool EXIT CODE instead of a cause. Real information, so
	// it is returned and may be the headline when nothing more specific
	// exists — but it loses to any specific line in the same failure.
	TierAggregate DiagnosticTier = "aggregate"
)

// Diagnostic is one extracted compiler/linker diagnostic line.
type Diagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity"`
	// Tier is always populated on a returned diagnostic (never omitempty), so
	// a consumer can rely on reading it rather than re-deriving the
	// distinction from Text with its own substring guesses.
	Tier DiagnosticTier `json:"tier"`
	Text string         `json:"text"`
}

// Args is the vcpkg_last_failure tool's input contract.
type Args struct {
	// Port is optional: when omitted, the tool infers it from a wrapper
	// file's failed_ports list IF exactly one is named there; otherwise it
	// asks (never guesses).
	Port    string `json:"port,omitempty"`
	Triplet string `json:"triplet,omitempty"`
	// Root is the vcpkg root (explicit param, highest discovery precedence).
	Root string `json:"root,omitempty"`
	// BuildtreesRoot overrides the default <root>/buildtrees location.
	// Highest precedence for locating buildtrees (an explicit param).
	BuildtreesRoot string `json:"buildtrees_root,omitempty"`
	// BuildFailedLog is the OPTIONAL wrapper-file path. Never
	// auto-discovered — accepted only when the caller supplies it.
	BuildFailedLog string `json:"build_failed_log,omitempty"`
	// Overlays is an explicit, ordered overlay-ports chain (order IS
	// precedence). Echoed back, never used to resolve which port wins
	// (that is vcpkg_port_resolution, out of scope this increment).
	Overlays []string `json:"overlays,omitempty"`
}

// Result is the vcpkg_last_failure tool's output contract.
type Result struct {
	Status Status `json:"status"`
	Reason Reason `json:"reason,omitempty"`

	Phase        Phase  `json:"phase,omitempty"`
	FailedTarget string `json:"failed_target,omitempty"`
	// ExactCommand is the reproducible TOP-LEVEL vcpkg invocation — the line
	// an operator can paste into a shell to re-run the failing install.
	//
	// It is recovered ONLY from an authoritative record of that invocation
	// (currently: a wrapper file's `command:` line). It is NEVER lifted out
	// of a phase log, because a phase log holds a NESTED build tool's output,
	// not vcpkg's own command line. Verified field failure (2026-07-26): the
	// predecessor returned the first non-empty line of the chosen phase log,
	// which for a make-driven port is make's trace —
	// `Makefile:36039: update target 'all-recursive' due to: target is
	// .PHONY`. A wrong command an operator pastes into a shell is worse than
	// no command, so when nothing authoritative is available this field is
	// omitted and NoteExactCommandNotRecovered says so.
	ExactCommand string `json:"exact_command,omitempty"`
	// BuildCommand is the build-layer sub-invocation recorded by CMake
	// ("Run Build Command(s): ..."), when the log that produced the reported
	// diagnostics carries one. Deliberately a SEPARATE field: it is a real,
	// useful command, but it belongs to the build step rather than to vcpkg,
	// and reporting it as exact_command would misstate its layer.
	//
	// Its provenance is stateable: it is read from the same (phase,
	// configuration) build step named by DiagnosticLog.
	BuildCommand string `json:"build_command,omitempty"`
	// DiagnosticLog is the log file the HEADLINE diagnostic (FirstError, or
	// the top-ranked diagnostic when none is an error) was read from. It makes
	// the association between the returned diagnostics and the returned
	// BuildCommand explicit rather than incidental — the predecessor took the
	// command from whichever log the phase loop happened to touch last, which
	// need not be the one the reported diagnostic came from.
	DiagnosticLog string `json:"diagnostic_log,omitempty"`
	// FirstError is the HEADLINE error, or omitted when the set holds none.
	// Additive convenience over Diagnostics: the single actionable line,
	// reachable without scanning or filtering the array at all. It is always
	// the same diagnostic as Diagnostics[0] whenever any error is present.
	//
	// "First" means first under the tool's documented ranking — the first
	// error that NAMES A CAUSE (Tier specific), falling back to the first
	// aggregate when every error is one. It is not raw file order: a driver's
	// `linker command failed with exit code 1120` physically precedes the
	// `lld-link: error: undefined symbol` that caused it, and reporting the
	// consequence as the headline is the defect this ordering exists to
	// prevent. See DiagnosticTier.
	FirstError *Diagnostic `json:"first_error,omitempty"`
	// Diagnostics is ORDERED: by severity (error, then warning, then note),
	// then by tier within a severity (specific before aggregate), then by
	// first-occurrence. See rankDiagnostics and DiagnosticTier.
	//
	// The ordering is part of the wire contract. Warnings are never dropped —
	// a -Werror build's warning is genuinely interesting, and filtering is
	// the caller's choice — they simply sort after the errors. Aggregates are
	// never dropped either: an aggregate is real information (a count, an exit
	// code) and is the honest headline when nothing more specific was found.
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	// ExitCode is a pointer so "0" and "not known" are distinguishable in
	// JSON (omitted entirely when unknown).
	ExitCode *int `json:"exit_code,omitempty"`
	// LogPaths is ALWAYS populated whenever any log was found, regardless
	// of Status — "always return log_paths so an agent can read more
	// itself" (design doc invariant).
	LogPaths []string `json:"log_paths,omitempty"`
	// OverlayChain is echoed back in precedence order (highest first) —
	// never resolved to a winner, just reported so the caller can verify
	// what the answer rests on.
	OverlayChain []string `json:"overlay_chain,omitempty"`
	// ContextSource lists EVERY source actually consulted, in the order
	// consulted. A list, never a single value (2026-07-25 correction).
	ContextSource []ContextSource `json:"context_source"`
	Notes         []Note          `json:"notes,omitempty"`

	Evidence evidence.Evidence `json:"evidence"`
}

// Status aliases evidence.Status so callers of this package do not need a
// second import just to read Result.Status.
type Status = evidence.Status

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
	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
)

// Reason is populated only when Status == evidence.StatusUnknown. Closed enum.
type Reason string

const (
	// ReasonRootNotSpecified: neither root, buildtrees_root, nor a usable
	// wrapper file was supplied, and vcpkg-root discovery did not resolve
	// one either — nothing to locate buildtrees from.
	ReasonRootNotSpecified Reason = "root_not_specified"
	// ReasonPortNotSpecified: port omitted, and no wrapper file (or a
	// wrapper with != 1 failed port entries) was available to infer it.
	ReasonPortNotSpecified Reason = "port_not_specified"
	// ReasonMultipleFailedPortsAmbiguous: port omitted, wrapper file parsed
	// and named MORE than one failed port. The tool never silently picks.
	ReasonMultipleFailedPortsAmbiguous Reason = "multiple_failed_ports_ambiguous"
	// ReasonBuildtreesCleaned: the buildtrees root (or the specific port's
	// subdirectory) does not exist on disk at all — almost always because
	// --clean-buildtrees-after-build removed it after a successful build,
	// or the whole triplet's tree was cleaned. Verified real case: a
	// wrapper invocation naming --clean-buildtrees-after-build whose
	// --x-buildtrees-root no longer exists post-run.
	ReasonBuildtreesCleaned Reason = "buildtrees_cleaned"
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
)

// Note is a small closed vocabulary of non-authoritative observations
// attached to a result regardless of its Status — e.g. "the wrapper file
// was present but malformed, so it was ignored" even though buildtrees
// alone still produced an ok answer. Kept closed (not free text) so it
// stays auditable.
type Note string

const (
	NoteWrapperAbsent              Note = "wrapper_absent"
	NoteWrapperMalformed           Note = "wrapper_malformed_ignored"
	NoteWrapperUsedForContext      Note = "wrapper_used_for_invocation_context"
	NotePortAutoSelectedFromWrapper Note = "port_auto_selected_from_wrapper_single_failure"
	NoteTripletAutoSelectedFromDir Note = "triplet_auto_selected_from_buildtrees_dir"
	NoteOverlayChainFromWrapper    Note = "overlay_chain_from_wrapper_invocation"
	NoteOverlayChainFromEnv        Note = "overlay_chain_from_env"
	NoteOverlayChainFromParam      Note = "overlay_chain_from_explicit_param"
	NoteOverlayChainNone           Note = "overlay_chain_none_builtin_ports_only"
	// NoteWrapperConfirmsNoFailure: the wrapper's failed_ports list is
	// present and does NOT name this port — real evidence the port did
	// not fail in that run, not a guess.
	NoteWrapperConfirmsNoFailure Note = "wrapper_confirms_port_not_failed"
)

// ContextSource names one input the answer actually rests on. Closed enum,
// always returned as a list (evidentiary basis, not a single-value pick).
type ContextSource string

const (
	SourceBuildtrees     ContextSource = "buildtrees"
	SourceWrapperSummary ContextSource = "wrapper_summary"
)

// Phase is the build phase a diagnostic (or the failure overall) belongs
// to. Values extract/config/install mirror the exact file-phase tokens
// observed in a real buildtrees directory; build is inferred (not a
// separate log file) when a diagnostic inside an install-phase log has
// compiler/linker diagnostic shape rather than a CMake install-step
// message — vcpkg's own ninja invocation compiles AND installs in one
// "install" step, so a compile error physically lands in the install log.
type Phase string

const (
	PhaseExtract Phase = "extract"
	PhaseConfig  Phase = "config"
	PhaseBuild   Phase = "build"
	PhaseInstall Phase = "install"
)

// Diagnostic is one extracted compiler/linker diagnostic line.
type Diagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity"`
	Text     string `json:"text"`
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

	Phase        Phase        `json:"phase,omitempty"`
	FailedTarget string       `json:"failed_target,omitempty"`
	ExactCommand string       `json:"exact_command,omitempty"`
	Diagnostics  []Diagnostic `json:"diagnostics,omitempty"`
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

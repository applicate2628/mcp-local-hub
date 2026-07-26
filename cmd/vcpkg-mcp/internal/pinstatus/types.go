package pinstatus

import (
	"time"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
)

// Status aliases evidence.Status so callers of this package do not need a
// second import just to read PortResult.Status.
type Status = evidence.Status

// RemoteKind identifies which vcpkg source-acquisition function supplied
// the remote (or the absence of one). Closed enum.
type RemoteKind string

const (
	RemoteGitHub   RemoteKind = "github"
	RemoteGit      RemoteKind = "git"
	RemoteGitLab   RemoteKind = "gitlab"
	RemoteDistfile RemoteKind = "distfile"
	// RemoteNone: no recognized vcpkg_from_*/vcpkg_download_distfile call
	// was found at all — a provider/metapackage port that fetches nothing.
	// This is a legitimate, valid portfile shape, never a parse failure.
	RemoteNone RemoteKind = "none"
)

// RefShape classifies the pinned REF token's literal syntactic shape AS
// WRITTEN in the portfile (before any variable resolution) — never an
// interpretation of what the remote currently holds. Closed enum.
//
// Per the measured-ground-truth hard limit (see package doc), there is no
// "behind" value anywhere in this package's vocabulary, including here.
type RefShape string

const (
	// RefShapeCommit40Hex: REF is a literal 40-hex-character commit SHA.
	// The only shape this package can compare against a remote tip with
	// full confidence (equal -> current, unequal -> pin_not_at_tip,
	// carrying both SHAs, never a direction).
	RefShapeCommit40Hex RefShape = "commit_40hex"
	// RefShapeTag: REF is a literal, non-hex token that looks like a
	// version tag (heuristic classification only — see looksLikeTag).
	RefShapeTag RefShape = "tag"
	// RefShapeBranch: REF is a literal, non-hex token that does not look
	// like a tag — treated as a branch name.
	RefShapeBranch RefShape = "branch"
	// RefShapeVariableResolved: REF as written is a ${VARIABLE} reference.
	// ResolvedRef carries the literal value recovered from a local set()
	// in the same portfile (empty when resolution failed).
	RefShapeVariableResolved RefShape = "variable_resolved"
	// RefShapeNone: no REF field was found at all (malformed/absent).
	RefShapeNone RefShape = "none"
)

// RefValueSource records where a ${VARIABLE} REF was resolved. It is empty
// for literal refs and unresolved variables, so consumers never mistake a
// manifest-derived version for a portfile-local value.
type RefValueSource string

const (
	RefValueSourceLocalSet RefValueSource = "local_set"
	RefValueSourceManifest RefValueSource = "manifest"
)

// Reason is populated only when Status == evidence.StatusUnknown. Closed
// enum. The task's minimum set (not_git_comparable, pin_not_at_tip,
// ref_unresolvable, remote_query_failed, network_disabled,
// portfile_unparsable) is extended with one more precise value,
// ref_not_found_on_remote, so a genuinely-missing tag/branch is not
// conflated with a local ${VARIABLE} resolution failure — both are real,
// distinct causes and the enum stays closed either way.
type Reason string

const (
	// ReasonNotGitComparable: the port fetches via vcpkg_download_distfile,
	// or via no recognized fetch call at all (provider/metapackage) — there
	// is no git remote to compare against, ever.
	ReasonNotGitComparable Reason = "not_git_comparable"
	// ReasonPinNotAtTip: the pinned commit does NOT equal the remote's
	// current tracked-ref tip. Both PinnedSHA and TipSHA are populated,
	// plus CompareURL on a known forge — never a direction (see package
	// doc "hard limit").
	ReasonPinNotAtTip Reason = "pin_not_at_tip"
	// ReasonRefUnresolvable: REF was a ${VARIABLE} reference and no
	// matching set(VARIABLE ...) was found in the same portfile to resolve
	// it to a literal.
	ReasonRefUnresolvable Reason = "ref_unresolvable"
	// ReasonRefNotFoundOnRemote: REF (or its resolved literal) is a
	// non-commit token (tag/branch shape) and the live remote no longer has
	// a ref by that name. Distinct from ReasonRefUnresolvable, which is a
	// purely local parsing problem — this one required a live query to
	// discover.
	ReasonRefNotFoundOnRemote Reason = "ref_not_found_on_remote"
	// ReasonRemoteQueryFailed: the injected remoteRefsFn returned an error,
	// OR it succeeded but returned nothing usable to compare a commit-shaped
	// pin against (no HEAD entry, no matching HEAD_REF branch). Either way
	// the query did not produce an answer — never coerced to "current".
	ReasonRemoteQueryFailed Reason = "remote_query_failed"
	// ReasonNetworkDisabled: Args.DisableNetwork was set. Checked first and
	// unconditionally — every port returns this, regardless of what its
	// portfile would otherwise resolve to.
	ReasonNetworkDisabled Reason = "network_disabled"
	// ReasonPortfileUnparsable: portfile.cmake could not be read at all, OR
	// this package's text-only parser could not extract a usable fetch call
	// (unbalanced parens, or a mandatory REPO/URL/GITLAB_URL/REF field
	// absent or empty after resolution attempts). This package never
	// executes CMake, so it can only ever report what its own textual scan
	// found.
	ReasonPortfileUnparsable Reason = "portfile_unparsable"
	// ReasonGuardUnresolvable means source selection depends on a CMake guard
	// this textual parser cannot decide for the default configuration.
	ReasonGuardUnresolvable Reason = "guard_unresolvable"
	// ReasonMultipleFetchCalls means several viable source-acquisition calls
	// exist and none is explicitly bound to SOURCE_PATH.
	ReasonMultipleFetchCalls Reason = "multiple_fetch_calls"
)

// Remote is the parsed remote source this port fetches from.
type Remote struct {
	Kind RemoteKind `json:"kind"`
	// URL is the resolved git-fetchable remote: https://github.com/<repo>.git
	// for github, the vcpkg_from_git URL literal (after any ${VARIABLE}
	// resolution) for git, or <gitlab-base>/<repo>.git for gitlab. Empty
	// when Kind is distfile or none, or when a mandatory field could not be
	// resolved.
	URL string `json:"url,omitempty"`
	// Repo is the raw REPO/path field (owner/name for github, project path
	// for gitlab) after variable resolution, before being turned into URL.
	// Empty for git/distfile/none.
	Repo string `json:"repo,omitempty"`
}

// Pin is the pinned ref exactly as this package understands it.
type Pin struct {
	// Ref is the raw REF token as written in the portfile — may itself be a
	// ${VARIABLE} placeholder (see Shape/ResolvedRef).
	Ref   string   `json:"ref"`
	Shape RefShape `json:"shape"`
	// ResolvedRef is the literal value recovered from a local set() when
	// Shape == RefShapeVariableResolved. Empty when resolution failed (the
	// caller sees ReasonRefUnresolvable in that case) or when Shape is not
	// RefShapeVariableResolved.
	ResolvedRef string `json:"resolved_ref,omitempty"`
	// ResolvedFrom distinguishes a same-portfile set() from a sibling
	// vcpkg.json version field when ResolvedRef is populated.
	ResolvedFrom RefValueSource `json:"resolved_from,omitempty"`
}

// FetchCandidate records one recognized source-acquisition call. Candidates
// include calls whose default guard is false so an operator can audit the
// entire portfile; only ActiveForDefault candidates participate in source
// selection and ambiguity decisions.
type FetchCandidate struct {
	Remote           Remote `json:"remote"`
	Pin              Pin    `json:"pin"`
	HeadRef          string `json:"head_ref,omitempty"`
	Guard            string `json:"guard,omitempty"`
	GuardVariable    string `json:"guard_variable,omitempty"`
	ActiveForDefault bool   `json:"active_for_default"`
	BindsSourcePath  bool   `json:"binds_source_path,omitempty"`
}

// PortResult is the vcpkg_pin_status answer for one port directory.
type PortResult struct {
	PortDir string `json:"port_dir"`
	Status  Status `json:"status"`
	Reason  Reason `json:"reason,omitempty"`

	Remote Remote `json:"remote"`
	Pin    Pin    `json:"pin"`
	// Candidates records every recognized fetch call and its guard. A false
	// default guard is retained for auditability but cannot be selected.
	Candidates []FetchCandidate `json:"candidates,omitempty"`
	// UnresolvedGuardVariable names the CMake variable that prevented source
	// selection when Reason is ReasonGuardUnresolvable.
	UnresolvedGuardVariable string `json:"unresolved_guard_variable,omitempty"`

	// PinnedSHA / TipSHA are populated only for the commit-shaped comparison
	// path (Status==ok, or Status==unknown(pin_not_at_tip)) — both SHAs are
	// always handed back together, NEVER a direction (see package doc "hard
	// limit": ls-remote cannot establish ancestry).
	PinnedSHA string `json:"pinned_sha,omitempty"`
	TipSHA    string `json:"tip_sha,omitempty"`
	// TrackedRef names which remote ref TipSHA came from: "HEAD" (the
	// remote's default branch) or vcpkg_from_git/vcpkg_from_github's own
	// HEAD_REF override when the portfile supplied one.
	TrackedRef string `json:"tracked_ref,omitempty"`
	// CompareURL is a browsable diff link, populated only when Remote.Kind
	// is a recognized forge (github/gitlab) and both SHAs are known.
	CompareURL string `json:"compare_url,omitempty"`

	// ObservedAt is when THIS call queried (or, for network-disabled/parse
	// failures, would have queried) the remote. This package does not
	// itself cache results, but every result carries this timestamp so a
	// caller that does cache can never present a stale verdict as live
	// without saying so.
	ObservedAt time.Time         `json:"observed_at"`
	Evidence   evidence.Evidence `json:"evidence"`
}

// Args is vcpkg_pin_status's input contract.
type Args struct {
	// PortDirs is a list of absolute port directory paths, each expected to
	// contain a portfile.cmake.
	PortDirs []string `json:"port_dirs"`
	// DisableNetwork defaults to false (network allowed) — the safe,
	// zero-value default matches the tool's documented default behavior.
	// When true, EVERY port in PortDirs returns unknown(network_disabled)
	// without ever invoking RemoteRefs, regardless of source-acquisition
	// shape.
	DisableNetwork bool `json:"disable_network,omitempty"`
}

// Result is vcpkg_pin_status's output contract: one PortResult per entry in
// Args.PortDirs, in input order.
type Result struct {
	Ports []PortResult `json:"ports"`
}

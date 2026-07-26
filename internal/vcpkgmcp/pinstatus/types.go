package pinstatus

import (
	"time"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
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
	// RefShapeVariableResolved: REF as written CONTAINS one or more ${VARIABLE}
	// references — either the whole token ("${VTK_GIT_REF}") or an embedded
	// one ("v${VERSION}", "util-macros-${VERSION}", "${PORT}-${VERSION}").
	// ResolvedRef carries the fully-substituted literal, and is empty when
	// ANY variable could not be resolved (see Pin.UnresolvedVariable) — a
	// partially-expanded ref is never produced, because comparing one against
	// a remote is how a false "ref does not exist" verdict gets manufactured.
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
	// RefValueSourcePortName: ${PORT}, resolved from the port directory's own
	// name — one of the two ubiquitous vcpkg ref idioms alongside ${VERSION}.
	RefValueSourcePortName RefValueSource = "port_name"
	// RefValueSourceMixed: the token contained SEVERAL variables that
	// resolved from different sources (e.g. "${PORT}-${VERSION}"). Reported
	// explicitly rather than left empty, because an empty ResolvedFrom means
	// "not resolved at all".
	RefValueSourceMixed RefValueSource = "mixed"
)

// Reason is populated only when Status == evidence.StatusUnknown. Closed
// enum. The task's minimum set (not_git_comparable, pin_not_at_tip,
// ref_unresolvable, remote_query_failed, network_disabled,
// portfile_unparsable) is extended with precise values for a missing remote
// tag/branch, an existing but non-comparable named ref, and an unresolved
// HEAD_REF variable. These are distinct facts and the enum stays closed.
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
	// ReasonNamedRefNotComparable: REF (or its resolved literal) is a named
	// tag or branch that the remote still advertises. Its current target can
	// be observed, but that does not prove the port's pin is current.
	ReasonNamedRefNotComparable Reason = "named_ref_not_comparable"
	// ReasonHeadRefUnresolvable: HEAD_REF was supplied as a ${VARIABLE}, but
	// its local value could not be resolved. This is distinct from omitting
	// HEAD_REF, which intentionally uses the remote's default HEAD.
	ReasonHeadRefUnresolvable Reason = "head_ref_unresolvable"
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
	// ReasonRemoteQueryTimeout: the remote query exceeded its deadline —
	// either the caller's, or this package's own RemoteQueryTimeout backstop.
	// Distinct from ReasonRemoteQueryFailed because it is a live-remote
	// LATENCY fact (retry later, or raise the budget), not a broken remote.
	ReasonRemoteQueryTimeout Reason = "remote_query_timeout"
	// ReasonRemoteQueryCanceled: the caller canceled before this port's query
	// finished (or before it started). Says nothing at all about the port —
	// it is the honest "we stopped looking" verdict, never coerced to ok.
	ReasonRemoteQueryCanceled Reason = "remote_query_canceled"
	// ReasonRemoteRefLimit: the remote's ref advertisement exceeded a
	// configured bound (MaxRemoteRefs, MaxRemoteRefLineBytes, or
	// MaxRemoteOutputBytes). Reported rather than answered from a truncated
	// ref set, because a partial set can silently turn "this tag exists" into
	// "this tag is gone".
	ReasonRemoteRefLimit Reason = "remote_ref_limit"
	// ReasonRemoteURLCredentialBearing: the portfile's remote URL embeds a
	// credential (userinfo, or a secret-shaped query parameter). Querying it
	// would place that secret in the child process's command line, which is
	// world-readable on both Windows and Linux for the life of the process,
	// so the query is refused. Every URL this package EMITS is redacted
	// regardless (see redact.go); this reason covers the separate argv
	// channel that redaction cannot reach.
	ReasonRemoteURLCredentialBearing Reason = "remote_url_credential_bearing"
)

// BatchReason is the CLOSED enum for the WHOLE CALL's outcome — why the tool
// invocation itself could not produce per-port answers at all. It is
// deliberately a separate type from Reason (which is always per-port), so a
// caller switching exhaustively over per-port reasons is never handed a value
// that can only occur at the batch level. Same split as
// cmakewrap.Reason vs cmakegraph.Reason.
type BatchReason string

const (
	// BatchReasonNoPortDirs: port_dirs was omitted or empty. An empty batch
	// is rejected rather than answered with an empty list, which would be
	// indistinguishable from "checked everything, all fine".
	BatchReasonNoPortDirs BatchReason = "no_port_dirs"
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
	// vcpkg.json version field, the port directory name, or a mix, when
	// ResolvedRef is populated.
	ResolvedFrom RefValueSource `json:"resolved_from,omitempty"`
	// UnresolvedVariable names the FIRST ${NAME} this package could not
	// resolve, when Shape is RefShapeVariableResolved and ResolvedRef is
	// empty. It is the diagnostic that tells an operator which variable to
	// supply, instead of leaving them with a bare "unresolvable".
	UnresolvedVariable string `json:"unresolved_variable,omitempty"`
	// Literal is true when REF came from a CMake BRACKET argument
	// ([[...]] / [=[...]=]), whose contents CMake does not interpret. A "${"
	// inside such a ref is part of the ref NAME, not a variable reference, so
	// it is compared verbatim on purpose. This bit is what lets the
	// unexpanded-ref guard distinguish "CMake would have expanded this and we
	// could not" from "this genuinely is the ref's name".
	Literal bool `json:"literal,omitempty"`
}

// FetchCandidate records one recognized source-acquisition call. Candidates
// include calls whose default guard is false so an operator can audit the
// entire portfile; only ActiveForDefault candidates participate in source
// selection and ambiguity decisions.
type FetchCandidate struct {
	Remote                    Remote `json:"remote"`
	Pin                       Pin    `json:"pin"`
	HeadRef                   string `json:"head_ref,omitempty"`
	UnresolvedHeadRefVariable string `json:"unresolved_head_ref_variable,omitempty"`
	Guard                     string `json:"guard,omitempty"`
	GuardVariable             string `json:"guard_variable,omitempty"`
	ActiveForDefault          bool   `json:"active_for_default"`
	BindsSourcePath           bool   `json:"binds_source_path,omitempty"`
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
	// UnresolvedHeadRefVariable names a supplied HEAD_REF variable that could
	// not be resolved. It is deliberately distinct from an omitted HEAD_REF.
	UnresolvedHeadRefVariable string `json:"unresolved_head_ref_variable,omitempty"`

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

	// NamedRef and NamedRefSHA record an existing tag or branch that cannot
	// be compared for currency without fetching source history. They are set
	// only with ReasonNamedRefNotComparable.
	NamedRef    string `json:"named_ref,omitempty"`
	NamedRefSHA string `json:"named_ref_sha,omitempty"`

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
	// Status/Reason describe whether THIS TOOL CALL produced per-port answers
	// at all — never a per-port verdict (see PortResult.Status/Reason for
	// those). ok means the batch ran; individual ports may still be unknown.
	// unknown(no_port_dirs) means there was nothing to run.
	//
	// Without this, an omitted port_dirs returned {"ports":[]} — byte-identical
	// to a successful zero-work call — which violated the every-tool
	// ok|failed|unknown(reason) contract this binary is built on.
	Status Status      `json:"status"`
	Reason BatchReason `json:"reason,omitempty"`

	Ports []PortResult `json:"ports"`
}

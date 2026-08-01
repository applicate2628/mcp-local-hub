// Package pinstatus implements vcpkg_pin_status: "is this port's pinned
// source still current upstream?"
//
// # The hard limit (measured, not assumed)
//
// A live 61-remote probe (2026-07-26) proved `git ls-remote` can NEVER
// answer "behind": it proves only what a named ref points at NOW. When the
// pin equals that tip, "current" is sound. When it does not, ls-remote
// cannot establish that the pinned commit still exists, that it is an
// ancestor of the tip (behind) rather than diverged or rebased away, or how
// far behind it is. Querying refs/tags/<40-hex-commit> is a tag-NAME lookup
// whose empty result proves nothing about ancestry either.
//
// For valid absolute inputs, the verdict vocabulary this package produces is
// strictly current | unknown(reason) — there is NO "behind" value anywhere in
// this package (see TestNoCodePathProducesBehind). Invalid relative input is
// failed(relative_port_dir) before I/O. Every non-current, non-error
// commit-shaped result returns BOTH the pinned SHA and the tip SHA (never a
// direction), plus a browsable compare URL on a known forge so a human can
// look and decide for themselves.
//
// # Source-acquisition shapes covered
//
// Measured across 59 real ports (2026-07-26 scout pass): vcpkg_from_github
// (39 ports), vcpkg_download_distfile (13, never git-comparable),
// vcpkg_from_git (5, some with FETCH_REF/HEAD_REF, some with a
// ${VARIABLE} REF resolved by a local set()), vcpkg_from_gitlab (3, any of
// GITLAB_URL/REPO/REF may themselves be ${VARIABLE}s), and 2
// provider/metapackage ports that fetch nothing. 13 of the 59 ports have no
// git remote at all. This package parses portfile.cmake TEXTUALLY (see
// portfile.go) — it never invokes cmake, per the design doc's ban on
// executing untrusted build scripts to answer a read-only question.
// Measured coverage on that tree: 40 current, 19 unknown; 61 round-trips /
// 60.8s total, median ~0.7s per remote, worst 11.7s — interactive for one
// port, not for a whole-tree scan.
//
// # Caching
//
// This package does not itself persist a cache — that is a caller/
// orchestration concern — but every PortResult carries ObservedAt so a
// caller that DOES cache results can never present a cached verdict as
// live without saying so.
package pinstatus

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	hubprocess "mcp-local-hub/internal/process"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Deps bounds every ambient input PinStatus reads (filesystem, the network
// query, wall-clock time), mirroring the discovery/lastfailure packages'
// determinism seam: nothing here reads a real file or shells out to git
// directly, so tests fully control every input.
type Deps struct {
	FS FS
	// RemoteRefs queries a git remote's advertised refs. Defaults to a real
	// `git ls-remote` invocation; every test in this package substitutes a
	// fake so tests never touch the network.
	RemoteRefs remoteRefsFn
	// Now mirrors time.Now, injected so ObservedAt is deterministic in
	// tests.
	Now func() time.Time
}

// DefaultDeps wires Deps to the real OS/network. Production callers use
// this; tests build their own Deps with a fake FS/RemoteRefs/Now.
func DefaultDeps() Deps {
	return Deps{
		FS:         DefaultFS(),
		RemoteRefs: defaultRemoteRefs,
		Now:        time.Now,
	}
}

// PinStatus answers vcpkg_pin_status for every directory in args.PortDirs,
// in input order. One malformed or offline port never aborts the batch —
// each PortDir gets its own independent PortResult.
//
// The batch itself carries a tri-state Status/Reason, distinct from the
// per-port ones: an empty PortDirs is unknown(no_port_dirs), NOT an ok result
// with an empty list, because "you asked about nothing" and "I checked
// everything you asked about and it was fine" are different answers and a
// caller must not have to guess which it got.
//
// ctx is honoured: cancellation or a deadline stops the batch, the remaining
// ports report unknown(remote_query_canceled), and the child git processes
// already started are terminated and reaped (see defaultRemoteRefs).
func PinStatus(ctx context.Context, args Args, deps Deps) Result {
	nowFn := deps.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	fsys := deps.FS
	if fsys == nil {
		fsys = DefaultFS()
	}
	remoteRefs := deps.RemoteRefs
	if remoteRefs == nil {
		remoteRefs = defaultRemoteRefs
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if len(args.PortDirs) == 0 {
		return Result{
			Status: evidence.StatusUnknown,
			Reason: BatchReasonNoPortDirs,
			Ports:  []PortResult{},
		}
	}

	out := Result{Status: evidence.StatusOK, Ports: make([]PortResult, 0, len(args.PortDirs))}
	for _, dir := range args.PortDirs {
		out.Ports = append(out.Ports, pinStatusOne(ctx, dir, args.DisableNetwork, fsys, remoteRefs, nowFn))
	}
	return out
}

// pinStatusOne answers vcpkg_pin_status for a single port directory. See
// package doc for the overall contract; each early return below names the
// exact Reason it produces and why.
func pinStatusOne(ctx context.Context, portDir string, disableNetwork bool, fsys FS, remoteRefs remoteRefsFn, nowFn func() time.Time) PortResult {
	res := PortResult{PortDir: portDir, ObservedAt: nowFn()}

	if err := ctx.Err(); err != nil {
		res.Status = evidence.StatusUnknown
		res.Reason = cancellationReason(err)
		return res
	}

	// Reject relative caller input before it can bind to the hub daemon's
	// working directory. Never call filepath.Abs here: that would perform the
	// ambiguous binding this gate exists to prevent.
	if !filepath.IsAbs(portDir) {
		res.Status = evidence.StatusFailed
		res.Reason = ReasonRelativePortDir
		return res
	}

	// DisableNetwork is checked unconditionally for every valid port path —
	// per the input contract, every such port returns
	// unknown(network_disabled) when set, regardless of source-acquisition
	// shape (even a distfile-only port that would never need the network
	// anyway).
	if disableNetwork {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonNetworkDisabled
		return res
	}

	portfilePath := filepath.Join(portDir, "portfile.cmake")
	data, err := fsys.ReadFile(portfilePath)
	res.Evidence.AddPath(portfilePath)
	if err != nil {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPortfileUnparsable
		return res
	}

	// VERSION is commonly supplied by the sibling vcpkg.json rather than a
	// set() in the portfile. A missing or malformed manifest is not itself a
	// parse failure: it only leaves ${VERSION} unresolved.
	manifestPath := filepath.Join(portDir, "vcpkg.json")
	manifest, manifestErr := fsys.ReadFile(manifestPath)
	if manifestErr == nil {
		res.Evidence.AddPath(manifestPath)
	}
	parsed, ok := parsePortfileWithManifest(string(data), manifest, filepath.Base(portDir))
	if !ok {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPortfileUnparsable
		return res
	}
	// THE credential boundary. `parsed` may carry a userinfo-bearing URL on
	// the selected remote AND on any audited candidate; from here on nothing
	// but queryURL (a local, never-emitted variable) holds the raw spelling.
	// Every field assigned below is already redacted, so a later addition
	// cannot leak by forgetting to call the redactor — see redact.go.
	queryURL := parsed.Remote.URL
	res.Remote = redactRemote(parsed.Remote)
	res.Pin = parsed.Pin
	res.Candidates = redactCandidates(parsed.Candidates)
	res.UnresolvedGuardVariable = parsed.UnresolvedGuardVariable
	res.UnresolvedHeadRefVariable = parsed.UnresolvedHeadRefVariable

	if parsed.UnresolvedGuardVariable != "" {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonGuardUnresolvable
		return res
	}
	if parsed.MultipleFetchCalls {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonMultipleFetchCalls
		return res
	}

	if parsed.Remote.Kind == RemoteDistfile || parsed.Remote.Kind == RemoteNone {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonNotGitComparable
		return res
	}
	if parsed.UnresolvedHeadRefVariable != "" {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonHeadRefUnresolvable
		return res
	}

	if queryURL == "" {
		// A github/git/gitlab call whose mandatory REPO/URL/GITLAB_URL
		// field is absent, or a ${VARIABLE} that failed to resolve — this
		// package cannot identify a remote to query at all. A
		// parse-completeness problem, not a live-remote problem.
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPortfileUnparsable
		return res
	}

	// Convert the raw spelling into execution authority exactly once. Public
	// redaction above remains independent of this admission decision.
	approvedRemote, approvalReason := approveRemoteURL(queryURL)
	if approvalReason != "" {
		res.Status = evidence.StatusUnknown
		res.Reason = approvalReason
		return res
	}

	effectiveRef := parsed.Pin.Ref
	if parsed.Pin.Shape == RefShapeVariableResolved {
		if parsed.Pin.ResolvedRef == "" {
			res.Status = evidence.StatusUnknown
			res.Reason = ReasonRefUnresolvable
			return res
		}
		effectiveRef = parsed.Pin.ResolvedRef
	}
	if effectiveRef == "" {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPortfileUnparsable
		return res
	}

	// THE FLOOR (field-reported P1). Independently of whether the expander
	// above understood this portfile, a ref that STILL carries a ${...}
	// reference must never reach the remote comparison: comparing the literal
	// string "v${VERSION}" against a remote's ref names yields
	// ref_not_found_on_remote — a confident, wrong NEGATIVE that sends a
	// maintainer to "fix" a pin which is already correct. Unresolvable is the
	// honest verdict; this guard makes that structural rather than dependent
	// on every future parser change getting it right.
	//
	// Pin.Literal is the one exception and is a real one, not a loophole: a
	// bracket argument's contents are uninterpreted by CMake, so "${X}" there
	// IS the ref's name and comparing it verbatim is correct.
	if !parsed.Pin.Literal && strings.Contains(effectiveRef, "${") {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonRefUnresolvable
		return res
	}

	// Evidence is an EMITTED field like any other: it takes the redacted
	// spelling, never queryURL.
	res.Evidence.AddCommand("git ls-remote " + res.Remote.URL)
	refs, err := remoteRefs(ctx, approvedRemote)
	if err != nil {
		res.Status = evidence.StatusUnknown
		res.Reason = remoteQueryReason(err)
		res.Failure = projectRemoteFailure(res.Reason, err)
		return res
	}

	if isCommitHex(effectiveRef) {
		tip, tracked, found := resolveTrackedTip(refs, parsed.HeadRef)
		if !found {
			// ls-remote succeeded but gave us nothing to compare against
			// (no HEAD entry, no matching HEAD_REF branch) — the query
			// itself did not produce a usable answer, never coerced to ok.
			res.Status = evidence.StatusUnknown
			res.Reason = ReasonRemoteQueryFailed
			res.Failure = projectRemoteFailure(res.Reason, nil)
			return res
		}
		res.PinnedSHA = effectiveRef
		res.TipSHA = tip
		res.TrackedRef = tracked
		// res.Remote, not parsed.Remote: the gitlab compare link is built by
		// string-editing the remote URL, so feeding it the raw spelling
		// would reconstruct the credential in a THIRD emitted field.
		res.CompareURL = buildCompareURL(res.Remote, effectiveRef, tip)
		if strings.EqualFold(effectiveRef, tip) {
			res.Status = evidence.StatusOK
			return res
		}
		// Pin != tip. This is NEVER reported as "behind" — ls-remote alone
		// cannot establish ancestry (see package doc "hard limit"). Both
		// SHAs plus a compare URL are handed back so a human can look.
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPinNotAtTip
		return res
	}

	// Non-commit literal (tag or branch name): existence-only check. This
	// NEVER attempts an ordering/currency claim beyond "does the remote
	// still have a ref by this name" — a tag/branch pin has no fixed
	// baseline commit to compare against without downloading the source,
	// which this package never does.
	if sha, found := refFound(refs, effectiveRef); found {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonNamedRefNotComparable
		res.NamedRef = effectiveRef
		res.NamedRefSHA = sha
		return res
	}
	// No ref by this literal name. Before concluding "the remote no longer has
	// it", ask whether it was ever a NAME: an abbreviated commit SHA is a
	// commit, and ls-remote advertises only full SHAs, so its absence from the
	// ref list is expected and says nothing about the pin. Reporting
	// ref_not_found_on_remote here asserted a deletion that never happened.
	//
	// The shape test runs AFTER the name lookup on purpose: a branch or tag
	// that happens to be pure hex is classified by the evidence that the remote
	// advertises it, never by its spelling.
	if isAbbreviatedCommitHex(effectiveRef) {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonCommitPinAbbreviated
		res.PinnedSHA = effectiveRef
		return res
	}
	res.Status = evidence.StatusUnknown
	res.Reason = ReasonRefNotFoundOnRemote
	return res
}

// remoteQueryReason maps a remoteRefsFn error onto this package's CLOSED
// Reason enum. Single owner, and deliberately identity-based (errors.Is) —
// never a string match on an error message, which would silently reclassify
// the day a dependency reworded one.
func remoteQueryReason(err error) Reason {
	switch {
	case errors.Is(err, ErrRemoteRefLimit):
		return ReasonRemoteRefLimit
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonRemoteQueryTimeout
	case errors.Is(err, context.Canceled):
		return ReasonRemoteQueryCanceled
	default:
		return ReasonRemoteQueryFailed
	}
}

// projectRemoteFailure is total over the stable tri-state reason plus the
// typed internal process lifecycle. It never projects raw causes.
func projectRemoteFailure(reason Reason, err error) *PublicFailure {
	failure := &PublicFailure{}
	switch reason {
	case ReasonRemoteRefLimit:
		failure.ID = FailureRemoteParseLimit
	case ReasonRemoteQueryCanceled:
		failure.ID = FailureRemoteCanceled
	case ReasonRemoteQueryTimeout:
		failure.ID = FailureRemoteTimeout
	default:
		var lifecycle *hubprocess.ContainedRunError
		if errors.As(err, &lifecycle) {
			switch lifecycle.Stage {
			case hubprocess.ContainedStageContainment:
				failure.ID = FailureProcessContainmentUnavailable
			case hubprocess.ContainedStageStart:
				failure.ID = FailureRemoteStartFailed
			case hubprocess.ContainedStageExit:
				failure.ID = FailureGitExitNonzero
				if lifecycle.ExitCode != nil {
					code := *lifecycle.ExitCode
					failure.ExitCode = &code
				}
			case hubprocess.ContainedStageCleanup:
				if errors.Is(lifecycle.Cause, hubprocess.ErrCleanupTimeout) {
					failure.ID = FailureProcessCleanupTimeout
				}
			}
		}
		if failure.ID == "" {
			failure.ID = FailureRemoteQueryFailed
		}
	}

	var lifecycle *hubprocess.ContainedRunError
	if errors.As(err, &lifecycle) &&
		lifecycle.CleanupCause != nil &&
		errors.Is(lifecycle.CleanupCause, hubprocess.ErrCleanupTimeout) &&
		failure.ID != FailureProcessCleanupTimeout {
		failure.CauseIDs = append(failure.CauseIDs, FailureProcessCleanupTimeout)
	}

	switch failure.ID {
	case FailureRemoteParseLimit:
		failure.Detail = "remote reference limit reached"
	case FailureRemoteCanceled:
		failure.Detail = "remote query canceled"
	case FailureRemoteTimeout:
		failure.Detail = "remote query timed out"
	case FailureProcessContainmentUnavailable:
		failure.Detail = "process containment unavailable"
	case FailureRemoteStartFailed:
		failure.Detail = "remote query start failed"
	case FailureProcessCleanupTimeout:
		failure.Detail = "process cleanup timed out"
	case FailureGitExitNonzero:
		failure.Detail = "git exited nonzero"
	default:
		failure.Detail = "remote query failed"
	}
	return failure
}

// cancellationReason distinguishes the caller giving up from a deadline
// expiring. Both stop the batch, but only one of them is our fault.
func cancellationReason(err error) Reason {
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonRemoteQueryTimeout
	}
	return ReasonRemoteQueryCanceled
}

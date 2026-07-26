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
// So the verdict vocabulary this package produces is strictly
// current | unknown(reason) — there is NO "behind" value anywhere in this
// package (see TestNoCodePathProducesBehind), and every non-current,
// non-error commit-shaped result returns BOTH the pinned SHA and the tip
// SHA (never a direction), plus a browsable compare URL on a known forge so
// a human can look and decide for themselves.
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
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
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
func PinStatus(args Args, deps Deps) Result {
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

	out := Result{Ports: make([]PortResult, 0, len(args.PortDirs))}
	for _, dir := range args.PortDirs {
		out.Ports = append(out.Ports, pinStatusOne(dir, args.DisableNetwork, fsys, remoteRefs, nowFn))
	}
	return out
}

// pinStatusOne answers vcpkg_pin_status for a single port directory. See
// package doc for the overall contract; each early return below names the
// exact Reason it produces and why.
func pinStatusOne(portDir string, disableNetwork bool, fsys FS, remoteRefs remoteRefsFn, nowFn func() time.Time) PortResult {
	res := PortResult{PortDir: portDir, ObservedAt: nowFn()}

	// DisableNetwork is checked first and unconditionally — per the input
	// contract, EVERY port returns unknown(network_disabled) when set,
	// regardless of source-acquisition shape (even a distfile-only port
	// that would never need the network anyway).
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
	parsed, ok := parsePortfileWithManifest(string(data), manifest)
	if !ok {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPortfileUnparsable
		return res
	}
	res.Remote = parsed.Remote
	res.Pin = parsed.Pin
	res.Candidates = parsed.Candidates
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

	if parsed.Remote.URL == "" {
		// A github/git/gitlab call whose mandatory REPO/URL/GITLAB_URL
		// field is absent, or a ${VARIABLE} that failed to resolve — this
		// package cannot identify a remote to query at all. A
		// parse-completeness problem, not a live-remote problem.
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonPortfileUnparsable
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

	res.Evidence.AddCommand("git ls-remote " + parsed.Remote.URL)
	refs, err := remoteRefs(context.Background(), parsed.Remote.URL)
	if err != nil {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonRemoteQueryFailed
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
			return res
		}
		res.PinnedSHA = effectiveRef
		res.TipSHA = tip
		res.TrackedRef = tracked
		res.CompareURL = buildCompareURL(parsed.Remote, effectiveRef, tip)
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
	res.Status = evidence.StatusUnknown
	res.Reason = ReasonRefNotFoundOnRemote
	return res
}

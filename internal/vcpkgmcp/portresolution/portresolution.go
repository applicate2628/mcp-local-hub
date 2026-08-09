// Package portresolution resolves a vcpkg port name against a list of
// overlay directories and a builtin ports directory, per the contract in
// work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md
// ("Port resolution — overlays in order, first match wins").
//
// For port X, the tool answers: WHICH definition actually wins, and why?
// vcpkg resolves a port name against overlay directories in the order they
// were passed via repeated --overlay-ports flags, falling back to the builtin
// <vcpkg-root>/ports/<name>. First match wins. The result carries the winning
// definition's absolute directory, which source it came from (overlay + index,
// or builtin), the FULL ordered list of every candidate location that was
// CHECKED, and whether the winner shadows any lower-precedence definitions.
package portresolution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/boundedio"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/portname"
)

// Args is the input contract for ResolvePort.
type Args struct {
	// Port is the port name to resolve (required). Empty port is a failed
	// input error.
	Port string `json:"port"`
	// VcpkgRoot is the vcpkg root directory. If omitted, builtin resolution
	// is skipped but overlay resolution still works. Must be an absolute path
	// if supplied.
	VcpkgRoot string `json:"vcpkg_root,omitempty"`
	// OverlayPorts is the list of overlay directories in precedence order
	// (first match wins). Each must be an absolute path if supplied.
	OverlayPorts []string `json:"overlay_ports,omitempty"`
}

// Reason is populated only when Result.Status == evidence.StatusUnknown.
// Closed enum.
type Reason string

const (
	// MaxOverlayRoots bounds request batch admission before normalization or
	// filesystem work. It is also exported to the MCP schema owner.
	MaxOverlayRoots                = 64
	maxOverlayMetadataBytes  int64 = 64 << 10
	overlayMetadataPageBytes int64 = 4 << 10
	// ReasonPortNotFound: port was not found in any overlay or builtin.
	ReasonPortNotFound Reason = "port_not_found"
	// ReasonNoRootsSupplied: neither vcpkg_root nor overlay_ports were given,
	// so no candidates could be checked.
	ReasonNoRootsSupplied Reason = "no_roots_supplied"
	// ReasonRootUnreadable: a supplied root (vcpkg_root or overlay) could not
	// be read (permission denied, doesn't exist, not a directory).
	ReasonRootUnreadable Reason = "root_unreadable"
	// ReasonEmptyPort: the port name is empty or whitespace-only.
	ReasonEmptyPort Reason = "empty_port"
	// ReasonInvalidPortName: port is not ONE legal vcpkg port-name segment, or
	// the path it joins to would leave the root it was joined under.
	//
	// Only an EMPTY port used to be rejected, so a traversal name like
	// "../../outside" was normalised BY the join and the resulting path — a
	// sibling of the overlay root, entirely outside it — was then stat'ed,
	// listed and reported as a resolved candidate location. That turns a port
	// LOOKUP into an arbitrary-directory probe against whatever the daemon can
	// read. Same gate, and the same reason spelling, as lastfailure's
	// invalid_port_name for its buildtrees root.
	ReasonInvalidPortName Reason = "invalid_port_name"
	// ReasonRelativeRoot: a supplied root was relative, which would make
	// resolution depend on the process working directory.
	ReasonRelativeRoot Reason = "relative_root"
	// ReasonHigherPrecedenceOverlayUnreadable: an overlay before the reported
	// winner could not be examined, so the true winner is unknown.
	ReasonHigherPrecedenceOverlayUnreadable Reason = "higher_precedence_overlay_unreadable"
	// ReasonTooManyOverlayRoots reports a caller batch above MaxOverlayRoots.
	ReasonTooManyOverlayRoots Reason = "too_many_overlay_roots"
)

// CandidateState describes whether a candidate was found, absent, unreadable,
// or deliberately not checked.
type CandidateState string

const (
	CandidateStateFound      CandidateState = "found"
	CandidateStateAbsent     CandidateState = "absent"
	CandidateStateUnreadable CandidateState = "unreadable"
	CandidateStateNotChecked CandidateState = "not_checked"
)

// CandidateLocation is one directory that was checked during resolution.
type CandidateLocation struct {
	// Directory is the absolute path to the candidate directory. It is empty
	// when State is not_checked because no path could be formed.
	Directory string `json:"directory"`
	// Source describes which overlay (or "builtin") this path came from.
	Source string `json:"source"`
	// PortDirFound reports whether Directory/port contains portfile.cmake
	// or vcpkg.json. Directories that exist but hold neither manifest file
	// are recorded as candidates with PortDirFound=false and a reason.
	PortDirFound bool `json:"port_dir_found"`
	// State is the machine-readable inspection result. PortDirFound is retained
	// for compatibility; State distinguishes absence from unreadability.
	State CandidateState `json:"state"`
	// Reason is populated when PortDirFound=false, explaining why this
	// candidate was rejected (e.g. "directory does not exist", "no
	// portfile.cmake or vcpkg.json found").
	Reason string `json:"reason,omitempty"`
}

// Winner describes the winning port definition.
type Winner struct {
	// Directory is the absolute path to the winning port directory.
	Directory string `json:"directory"`
	// Source is a label describing which overlay or "builtin" this came from.
	// For overlays it is "overlay-%02d: <path>", where %02d is the original
	// supplied overlay index (minimum width two) and <path> is trimmed. For
	// builtin it is "builtin <vcpkg-root>/ports".
	Source string `json:"source"`
}

// Shadow describes a lower-precedence definition that is shadowed by the winner.
type Shadow struct {
	// Directory is the absolute path to the shadowed port directory.
	Directory string `json:"directory"`
	// Source is a label describing which overlay or "builtin" held this
	// shadowed definition.
	Source string `json:"source"`
}

// Result is the outcome of one ResolvePort call.
type Result struct {
	Status Status `json:"status"`
	// Reason is set only when Status == unknown.
	Reason Reason `json:"reason,omitempty"`
	// Winner describes the winning port definition when Status == ok.
	Winner *Winner `json:"winner,omitempty"`
	// AllCandidates lists every candidate location in precedence order
	// (overlays first, then builtin), including a typed not_checked builtin
	// entry when vcpkg_root was omitted.
	AllCandidates []CandidateLocation `json:"all_candidates,omitempty"`
	// BlockingCandidate identifies the unreadable higher-precedence candidate
	// that prevents a definitive winner.
	BlockingCandidate *CandidateLocation `json:"blocking_candidate,omitempty"`
	// InvalidRoot identifies the supplied relative root rejected as invalid
	// input, so callers do not need to infer it from ambient working state.
	InvalidRoot string `json:"invalid_root,omitempty"`
	// InvalidPort echoes the rejected port name when Reason is
	// ReasonInvalidPortName, so the caller sees exactly what was refused.
	InvalidPort string `json:"invalid_port,omitempty"`
	// Shadows lists every lower-precedence definition of this port that was
	// shadowed by the winner. Populated only when Status == ok and at least
	// one shadowed definition exists.
	Shadows []Shadow `json:"shadows,omitempty"`
	// OverlayToOverlayShadowingOccurred reports whether any overlay-to-overlay
	// shadowing was detected (one port name in two overlay dirs). In practice
	// this may be rare if overlays are curated, but the precedence order
	// must still be respected as contract.
	OverlayToOverlayShadowingOccurred bool              `json:"overlay_to_overlay_shadowing_occurred,omitempty"`
	Evidence                          evidence.Evidence `json:"evidence"`
}

// Status is a local alias so callers do not need to import evidence package.
type Status = evidence.Status

// Deps abstracts filesystem access behind injectable functions for
// testability. Never read os.Stat or filepath.Abs directly; use Deps instead.
type Deps struct {
	// Stat reports whether path exists (any file type).
	Stat func(path string) (os.FileInfo, error)
	// Open supplies bounded file admission.
	Open func(path string) (io.ReadCloser, error)
	// OpenDir supplies bounded directory admission.
	OpenDir func(path string) (boundedio.DirReader, error)
	// Abs converts a path to an absolute path.
	Abs func(path string) (string, error)
}

// DefaultDeps wires Deps to the real OS. Production callers use this;
// tests build their own Deps with fake functions.
func DefaultDeps() Deps {
	return Deps{
		Stat:    os.Stat,
		Open:    func(path string) (io.ReadCloser, error) { return boundedio.OpenRegular(path) },
		OpenDir: func(path string) (boundedio.DirReader, error) { return os.Open(path) },
		Abs:     filepath.Abs,
	}
}

type depsFS struct{ deps Deps }

func (f depsFS) Open(path string) (io.ReadCloser, error) { return f.deps.Open(path) }
func (f depsFS) OpenRegular(path string) (boundedio.RegularFile, error) {
	reader, err := f.deps.Open(path)
	if err != nil {
		return nil, err
	}
	file, ok := reader.(boundedio.RegularFile)
	if !ok {
		_ = reader.Close()
		return nil, os.ErrInvalid
	}
	return file, nil
}
func (f depsFS) Stat(path string) (os.FileInfo, error)            { return f.deps.Stat(path) }
func (f depsFS) OpenDir(path string) (boundedio.DirReader, error) { return f.deps.OpenDir(path) }

type probeKind uint8

const (
	probeFound probeKind = iota
	probeAbsent
	probeUnreadable
	probeCanceled
)

type probeOutcome struct {
	kind   probeKind
	reason string
}

func canceledProbe(ctx context.Context) (probeOutcome, bool) {
	if err := ctx.Err(); err != nil {
		return probeOutcome{kind: probeCanceled, reason: err.Error()}, true
	}
	return probeOutcome{}, false
}

func probeStat(ctx context.Context, deps Deps, path string) (os.FileInfo, error, probeOutcome, bool) {
	if outcome, canceled := canceledProbe(ctx); canceled {
		return nil, nil, outcome, true
	}
	fi, err := deps.Stat(path)
	if outcome, canceled := canceledProbe(ctx); canceled {
		return nil, nil, outcome, true
	}
	return fi, err, probeOutcome{}, false
}

// inspectRoot establishes whether a supplied root is readable. A missing root
// is unreadable rather than absent: its unexamined contents could contain the
// port and therefore affect precedence.
func inspectRoot(ctx context.Context, deps Deps, dir string) probeOutcome {
	fi, err, outcome, canceled := probeStat(ctx, deps, dir)
	if canceled {
		return outcome
	}
	if err != nil {
		if os.IsNotExist(err) {
			return probeOutcome{kind: probeUnreadable, reason: "root directory does not exist"}
		}
		return probeOutcome{kind: probeUnreadable, reason: err.Error()}
	}
	if !fi.IsDir() {
		return probeOutcome{kind: probeUnreadable, reason: "root path is not a directory"}
	}
	_, err = boundedio.ReadDirComplete(ctx, depsFS{deps}, dir, 0, 1)
	if outcome, canceled := canceledProbe(ctx); canceled {
		return outcome
	}
	if err != nil {
		return probeOutcome{kind: probeUnreadable, reason: err.Error()}
	}
	return probeOutcome{kind: probeFound}
}

// inspectPortCandidate reports whether dir contains portfile.cmake or
// vcpkg.json. It distinguishes an absent definition from an unreadable one.
func inspectPortCandidate(ctx context.Context, deps Deps, dir string) probeOutcome {
	if dir == "" {
		return probeOutcome{kind: probeUnreadable, reason: "empty directory path"}
	}

	// Check existence of the directory itself.
	fi, err, outcome, canceled := probeStat(ctx, deps, dir)
	if canceled {
		return outcome
	}
	if err != nil {
		if os.IsNotExist(err) {
			return probeOutcome{kind: probeAbsent, reason: "directory does not exist"}
		}
		return probeOutcome{kind: probeUnreadable, reason: err.Error()}
	}
	if !fi.IsDir() {
		return probeOutcome{kind: probeAbsent, reason: "path is not a directory"}
	}

	var unreadable string
	for _, name := range []string{"portfile.cmake", "vcpkg.json"} {
		manifestPath := filepath.Join(dir, name)
		manifest, manifestErr, outcome, canceled := probeStat(ctx, deps, manifestPath)
		if canceled {
			return outcome
		}
		if manifestErr != nil {
			if os.IsNotExist(manifestErr) {
				continue
			}
			unreadable = manifestErr.Error()
			continue
		}
		if !manifest.Mode().IsRegular() {
			continue
		}
		_, readErr := boundedio.ReadFile(ctx, depsFS{deps}, manifestPath, 0, 1)
		if outcome, canceled := canceledProbe(ctx); canceled {
			return outcome
		}
		if readErr != nil {
			unreadable = readErr.Error()
			continue
		}
		return probeOutcome{kind: probeFound}
	}
	if unreadable != "" {
		return probeOutcome{kind: probeUnreadable, reason: unreadable}
	}
	return probeOutcome{kind: probeAbsent, reason: "no readable regular portfile.cmake or vcpkg.json found"}
}

// inspectIndividualOverlay implements vcpkg's two overlay forms. A supplied
// root that contains portfile.cmake plus vcpkg.json/CONTROL is one port whose
// name comes from that metadata; only when that shape is absent may resolution
// probe <root>/<requested-port> as a collection overlay.
func inspectIndividualOverlay(ctx context.Context, deps Deps, dir, requested string) (bool, probeOutcome) {
	portfile := filepath.Join(dir, "portfile.cmake")
	portInfo, portErr, outcome, canceled := probeStat(ctx, deps, portfile)
	if canceled {
		return true, outcome
	}
	if portErr != nil {
		if os.IsNotExist(portErr) {
			return false, probeOutcome{}
		}
		return true, probeOutcome{kind: probeUnreadable, reason: portErr.Error()}
	}
	if !portInfo.Mode().IsRegular() {
		return true, probeOutcome{kind: probeUnreadable, reason: "individual overlay portfile.cmake is not a regular file"}
	}
	if _, err := boundedio.ReadFile(ctx, depsFS{deps}, portfile, 1, 1); err != nil {
		return true, probeOutcome{kind: probeUnreadable, reason: err.Error()}
	}

	for _, metadata := range []string{"vcpkg.json", "CONTROL"} {
		path := filepath.Join(dir, metadata)
		info, statErr, statOutcome, statCanceled := probeStat(ctx, deps, path)
		if statCanceled {
			return true, statOutcome
		}
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return true, probeOutcome{kind: probeUnreadable, reason: statErr.Error()}
		}
		if !info.Mode().IsRegular() {
			return true, probeOutcome{kind: probeUnreadable, reason: metadata + " is not a regular file"}
		}
		admitted, readErr := boundedio.ReadFile(ctx, depsFS{deps}, path, maxOverlayMetadataBytes, overlayMetadataPageBytes)
		if readErr != nil {
			return true, probeOutcome{kind: probeUnreadable, reason: readErr.Error()}
		}
		if admitted.Limited {
			return true, probeOutcome{kind: probeUnreadable, reason: metadata + " exceeds the metadata byte limit"}
		}

		declared := ""
		if metadata == "vcpkg.json" {
			var manifest struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(admitted.Data, &manifest); err != nil {
				return true, probeOutcome{kind: probeUnreadable, reason: "invalid vcpkg.json: " + err.Error()}
			}
			declared = strings.TrimSpace(manifest.Name)
		} else {
			for _, line := range strings.Split(string(admitted.Data), "\n") {
				key, value, found := strings.Cut(line, ":")
				if found && strings.EqualFold(strings.TrimSpace(key), "Source") {
					declared = strings.TrimSpace(value)
					break
				}
			}
		}
		if declared == "" {
			return true, probeOutcome{kind: probeUnreadable, reason: metadata + " does not declare a port name"}
		}
		if declared != requested {
			return true, probeOutcome{kind: probeAbsent, reason: fmt.Sprintf("individual overlay declares port %q, not %q", declared, requested)}
		}
		return true, probeOutcome{kind: probeFound}
	}
	return false, probeOutcome{}
}

func candidateState(outcome probeOutcome) CandidateState {
	switch outcome.kind {
	case probeFound:
		return CandidateStateFound
	case probeAbsent:
		return CandidateStateAbsent
	case probeUnreadable, probeCanceled:
		return CandidateStateUnreadable
	default:
		panic("portresolution: invalid probe outcome")
	}
}

// overlayRoot is one non-blank overlay together with its position in the
// caller-supplied precedence list. Source labels use the original position;
// resolution uses the filtered slice order.
type overlayRoot struct {
	path        string
	sourceIndex int
}

type resolutionState struct {
	result                    Result
	winner                    *Winner
	winnerPrecedence          int
	blockingOverlay           *CandidateLocation
	blockingOverlayPrecedence int
	builtinUnreadable         bool
	canceled                  bool
	overlayToOverlayHit       bool
}

func newResolutionState() *resolutionState {
	return &resolutionState{
		result: Result{
			Shadows:       make([]Shadow, 0),
			AllCandidates: make([]CandidateLocation, 0),
		},
		winnerPrecedence:          -1,
		blockingOverlayPrecedence: -1,
	}
}

// transition is the single consumer of completed probe outcomes. The return
// value is true only when a readable root permits its candidate probe to begin.
func (state *resolutionState) transition(ctx context.Context, outcome probeOutcome, candidate CandidateLocation, precedence int, overlay, recordComplete bool, unreadablePrefix string) bool {
	if ctx.Err() != nil {
		outcome = probeOutcome{kind: probeCanceled, reason: ctx.Err().Error()}
	}
	if outcome.kind == probeFound && !recordComplete {
		return true
	}

	candidate.State = candidateState(outcome)
	candidate.PortDirFound = outcome.kind == probeFound
	candidate.Reason = outcome.reason
	if candidate.State == CandidateStateUnreadable && unreadablePrefix != "" {
		candidate.Reason = unreadablePrefix + candidate.Reason
	}
	state.result.AllCandidates = append(state.result.AllCandidates, candidate)

	switch outcome.kind {
	case probeCanceled:
		state.canceled = true
	case probeUnreadable:
		if overlay && state.blockingOverlay == nil {
			blocking := candidate
			state.blockingOverlay = &blocking
			state.blockingOverlayPrecedence = precedence
		}
		if !overlay {
			state.builtinUnreadable = true
		}
	}
	return false
}

// finalize is the sole post-validation owner of the public status, reason,
// winner and blocking-candidate precedence decision.
func (state *resolutionState) finalize(ctx context.Context) Result {
	if ctx.Err() != nil || state.canceled {
		state.result.Status = evidence.StatusUnknown
		state.result.Reason = ReasonRootUnreadable
		state.result.Winner = nil
		state.result.BlockingCandidate = nil
		return state.result
	}
	if state.blockingOverlay != nil && (state.winner == nil || state.blockingOverlayPrecedence < state.winnerPrecedence) {
		state.result.Status = evidence.StatusUnknown
		state.result.Reason = ReasonHigherPrecedenceOverlayUnreadable
		state.result.Winner = nil
		state.result.BlockingCandidate = state.blockingOverlay
		return state.result
	}
	if state.builtinUnreadable && state.winner == nil {
		state.result.Status = evidence.StatusUnknown
		state.result.Reason = ReasonRootUnreadable
		state.result.Winner = nil
		return state.result
	}
	if state.winner != nil {
		state.result.Status = evidence.StatusOK
		state.result.Reason = ""
		state.result.Winner = state.winner
		state.result.OverlayToOverlayShadowingOccurred = state.overlayToOverlayHit
		return state.result
	}
	state.result.Status = evidence.StatusUnknown
	state.result.Reason = ReasonPortNotFound
	return state.result
}

// normalizedOverlayRoots is the sole owner of overlay blank filtering and
// whitespace normalization for ResolvePort.
func normalizedOverlayRoots(paths []string) []overlayRoot {
	var roots []overlayRoot
	for sourceIndex, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		roots = append(roots, overlayRoot{path: path, sourceIndex: sourceIndex})
	}
	return roots
}

// ResolvePort resolves a port name against overlay directories and the
// builtin ports directory. Returns a tri-state result (ok/failed/unknown)
// with detailed evidence.
func ResolvePort(args Args, deps Deps) Result {
	return ResolvePortContext(context.Background(), args, deps)
}

// ResolvePortContext is ResolvePort with caller cancellation propagated to
// bounded filesystem probes. ResolvePort remains the compatibility wrapper.
func ResolvePortContext(ctx context.Context, args Args, deps Deps) Result {
	if ctx == nil {
		ctx = context.Background()
	}
	var res Result
	res.Shadows = make([]Shadow, 0)
	res.AllCandidates = make([]CandidateLocation, 0)
	if len(args.OverlayPorts) > MaxOverlayRoots {
		res.Status = evidence.StatusFailed
		res.Reason = ReasonTooManyOverlayRoots
		return res
	}

	// Gate 1: Empty port is a failed input error.
	if strings.TrimSpace(args.Port) == "" {
		res.Status = evidence.StatusFailed
		res.Reason = ReasonEmptyPort
		return res
	}

	port := strings.TrimSpace(args.Port)

	// Gate 1b: parse through the shared leaf BEFORE any filesystem operation.
	// The leaf owns both the legal-name grammar and per-root containment proof.
	name, nameErr := portname.Parse(port)
	if nameErr != nil {
		res.Status = evidence.StatusFailed
		res.Reason = ReasonInvalidPortName
		res.InvalidPort = port
		return res
	}

	overlayRoots := normalizedOverlayRoots(args.OverlayPorts)

	// Roots are an explicit absolute-path contract. Never call Abs here: doing
	// so would silently bind relative inputs to the process working directory.
	for _, overlay := range overlayRoots {
		if !filepath.IsAbs(overlay.path) {
			res.Status = evidence.StatusFailed
			res.Reason = ReasonRelativeRoot
			res.InvalidRoot = overlay.path
			return res
		}
	}
	if vcpkgRoot := strings.TrimSpace(args.VcpkgRoot); vcpkgRoot != "" && !filepath.IsAbs(vcpkgRoot) {
		res.Status = evidence.StatusFailed
		res.Reason = ReasonRelativeRoot
		res.InvalidRoot = vcpkgRoot
		return res
	}

	// Gate 2: At least one root must be supplied (overlay or vcpkg).
	hasOverlays := len(overlayRoots) > 0
	hasVcpkg := strings.TrimSpace(args.VcpkgRoot) != ""
	if !hasOverlays && !hasVcpkg {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonNoRootsSupplied
		return res
	}

	type resolvedOverlay struct {
		overlayRoot
		portPath string
	}
	resolvedOverlays := make([]resolvedOverlay, 0, len(overlayRoots))
	for _, overlay := range overlayRoots {
		portPath, portErr := portname.Join(overlay.path, name)
		if portErr != nil {
			res.Status = evidence.StatusFailed
			res.Reason = ReasonInvalidPortName
			res.InvalidPort = port
			return res
		}
		resolvedOverlays = append(resolvedOverlays, resolvedOverlay{overlayRoot: overlay, portPath: portPath})
	}
	var builtinPortPath string
	if hasVcpkg {
		var portErr error
		builtinPortPath, portErr = portname.Join(filepath.Join(strings.TrimSpace(args.VcpkgRoot), "ports"), name)
		if portErr != nil {
			res.Status = evidence.StatusFailed
			res.Reason = ReasonInvalidPortName
			res.InvalidPort = port
			return res
		}
	}

	state := newResolutionState()

	// Check overlays in precedence order (first match wins).
	for overlayPrecedence, overlay := range resolvedOverlays {
		if state.canceled {
			break
		}
		overlayPath := overlay.path
		sourceIndex := overlay.sourceIndex
		source := formatOverlaySource(overlayPath, sourceIndex)
		rootCandidate := CandidateLocation{Directory: overlayPath, Source: source}
		if !state.transition(ctx, inspectRoot(ctx, deps, overlayPath), rootCandidate, overlayPrecedence, true, false, "overlay root unreadable: ") {
			continue
		}
		if individual, outcome := inspectIndividualOverlay(ctx, deps, overlayPath, name.String()); individual {
			state.result.Evidence.AddPath(overlayPath)
			candidate := CandidateLocation{Directory: overlayPath, Source: source}
			state.transition(ctx, outcome, candidate, overlayPrecedence, true, true, "")
			if outcome.kind == probeFound && !state.canceled {
				if state.winner == nil {
					state.winner = &Winner{Directory: overlayPath, Source: source}
					state.winnerPrecedence = overlayPrecedence
				} else {
					state.overlayToOverlayHit = true
					state.result.Shadows = append(state.result.Shadows, Shadow{Directory: overlayPath, Source: source})
				}
			}
			continue
		}

		portPath := overlay.portPath
		state.result.Evidence.AddPath(portPath)
		candidate := CandidateLocation{Directory: portPath, Source: source}

		outcome := inspectPortCandidate(ctx, deps, portPath)
		state.transition(ctx, outcome, candidate, overlayPrecedence, true, true, "")
		if outcome.kind == probeFound && !state.canceled {
			if state.winner == nil {
				// First match wins.
				state.winner = &Winner{
					Directory: portPath,
					Source:    source,
				}
				state.winnerPrecedence = overlayPrecedence
			} else {
				// Overlay-to-overlay shadowing detected.
				state.overlayToOverlayHit = true
				state.result.Shadows = append(state.result.Shadows, Shadow{
					Directory: portPath,
					Source:    source,
				})
			}
		}
	}

	// Check builtin (only if vcpkg_root is supplied).
	if hasVcpkg && !state.canceled {
		vcpkgRoot := strings.TrimSpace(args.VcpkgRoot)
		state.result.Evidence.AddPath(builtinPortPath)
		rootCandidate := CandidateLocation{Directory: builtinPortPath, Source: "builtin (vcpkg_root)"}
		if state.transition(ctx, inspectRoot(ctx, deps, vcpkgRoot), rootCandidate, len(overlayRoots), false, false, "vcpkg_root unreadable: ") {
			builtinSource := "builtin " + filepath.Join(vcpkgRoot, "ports")
			candidate := CandidateLocation{Directory: builtinPortPath, Source: builtinSource}
			outcome := inspectPortCandidate(ctx, deps, builtinPortPath)
			state.transition(ctx, outcome, candidate, len(overlayRoots), false, true, "")

			if outcome.kind == probeFound && !state.canceled {
				if state.winner == nil {
					// Builtin wins if no overlay matched.
					state.winner = &Winner{Directory: builtinPortPath, Source: builtinSource}
					state.winnerPrecedence = len(overlayRoots)
				} else {
					// Overlay already won, builtin is shadowed.
					state.result.Shadows = append(state.result.Shadows, Shadow{Directory: builtinPortPath, Source: builtinSource})
				}
			}
		}
	} else if !hasVcpkg {
		state.result.AllCandidates = append(state.result.AllCandidates, CandidateLocation{
			Source: "builtin",
			State:  CandidateStateNotChecked,
			Reason: "vcpkg_root was not supplied",
		})
	}

	return state.finalize(ctx)
}

// formatOverlaySource is the sole formatter for overlay source identity.
// Width two is a minimum, so supplied index 103 remains "103".
func formatOverlaySource(overlayPath string, idx int) string {
	return fmt.Sprintf("overlay-%02d: %s", idx, strings.TrimSpace(overlayPath))
}

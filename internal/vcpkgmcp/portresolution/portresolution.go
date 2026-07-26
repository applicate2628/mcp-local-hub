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
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
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
	// ReasonRelativeRoot: a supplied root was relative, which would make
	// resolution depend on the process working directory.
	ReasonRelativeRoot Reason = "relative_root"
	// ReasonHigherPrecedenceOverlayUnreadable: an overlay before the reported
	// winner could not be examined, so the true winner is unknown.
	ReasonHigherPrecedenceOverlayUnreadable Reason = "higher_precedence_overlay_unreadable"
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
	// For overlays: "<overlay-path>" or "overlay-<index>" (overlay index in
	// the precedence list). For builtin: "builtin <vcpkg-root>/ports".
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
	// ReadDir lists entries in a directory.
	ReadDir func(path string) ([]os.DirEntry, error)
	// Abs converts a path to an absolute path.
	Abs func(path string) (string, error)
}

// DefaultDeps wires Deps to the real OS. Production callers use this;
// tests build their own Deps with fake functions.
func DefaultDeps() Deps {
	return Deps{
		Stat:    os.Stat,
		ReadDir: os.ReadDir,
		Abs:     filepath.Abs,
	}
}

// inspectRoot establishes whether a supplied root is readable. A missing root
// is unreadable rather than absent: its unexamined contents could contain the
// port and therefore affect precedence.
func inspectRoot(deps Deps, dir string) (CandidateState, string) {
	fi, err := deps.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return CandidateStateUnreadable, "root directory does not exist"
		}
		return CandidateStateUnreadable, err.Error()
	}
	if !fi.IsDir() {
		return CandidateStateUnreadable, "root path is not a directory"
	}
	if _, err := deps.ReadDir(dir); err != nil {
		return CandidateStateUnreadable, err.Error()
	}
	return CandidateStateFound, ""
}

// inspectPortCandidate reports whether dir contains portfile.cmake or
// vcpkg.json. It distinguishes an absent definition from an unreadable one.
func inspectPortCandidate(deps Deps, dir string) (CandidateState, string) {
	if dir == "" {
		return CandidateStateUnreadable, "empty directory path"
	}

	// Check existence of the directory itself.
	fi, err := deps.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return CandidateStateAbsent, "directory does not exist"
		}
		return CandidateStateUnreadable, err.Error()
	}
	if !fi.IsDir() {
		return CandidateStateAbsent, "path is not a directory"
	}

	// Read the directory to check for manifest files.
	entries, err := deps.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return CandidateStateAbsent, "directory does not exist"
		}
		return CandidateStateUnreadable, err.Error()
	}

	for _, entry := range entries {
		if !entry.IsDir() && (entry.Name() == "portfile.cmake" || entry.Name() == "vcpkg.json") {
			return CandidateStateFound, ""
		}
	}

	return CandidateStateAbsent, "no portfile.cmake or vcpkg.json found"
}

// ResolvePort resolves a port name against overlay directories and the
// builtin ports directory. Returns a tri-state result (ok/failed/unknown)
// with detailed evidence.
func ResolvePort(args Args, deps Deps) Result {
	var res Result
	res.Shadows = make([]Shadow, 0)
	res.AllCandidates = make([]CandidateLocation, 0)

	// Gate 1: Empty port is a failed input error.
	if strings.TrimSpace(args.Port) == "" {
		res.Status = evidence.StatusFailed
		res.Reason = ReasonEmptyPort
		return res
	}

	port := strings.TrimSpace(args.Port)

	// Roots are an explicit absolute-path contract. Never call Abs here: doing
	// so would silently bind relative inputs to the process working directory.
	for _, overlayPath := range args.OverlayPorts {
		overlayPath = strings.TrimSpace(overlayPath)
		if overlayPath != "" && !filepath.IsAbs(overlayPath) {
			res.Status = evidence.StatusFailed
			res.Reason = ReasonRelativeRoot
			res.InvalidRoot = overlayPath
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
	hasOverlays := len(args.OverlayPorts) > 0
	hasVcpkg := strings.TrimSpace(args.VcpkgRoot) != ""
	if !hasOverlays && !hasVcpkg {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonNoRootsSupplied
		return res
	}

	var winner *Winner
	winnerPrecedence := -1
	var overlayToOverlayHit bool
	var blockingOverlay *CandidateLocation
	blockingOverlayPrecedence := -1
	builtinUnreadable := false

	// Check overlays in precedence order (first match wins).
	for overlayIdx, overlayPath := range args.OverlayPorts {
		overlayPath = strings.TrimSpace(overlayPath)
		if overlayPath == "" {
			continue
		}

		portPath := filepath.Join(overlayPath, port)
		res.Evidence.AddPath(portPath)

		if state, reason := inspectRoot(deps, overlayPath); state == CandidateStateUnreadable {
			candidate := CandidateLocation{
				Directory: portPath,
				Source:    formatOverlaySource(overlayPath, overlayIdx),
				State:     CandidateStateUnreadable,
				Reason:    "overlay root unreadable: " + reason,
			}
			res.AllCandidates = append(res.AllCandidates, candidate)
			if blockingOverlay == nil {
				blocking := candidate
				blockingOverlay = &blocking
				blockingOverlayPrecedence = overlayIdx
			}
			continue
		}

		state, reason := inspectPortCandidate(deps, portPath)
		found := state == CandidateStateFound
		res.AllCandidates = append(res.AllCandidates, CandidateLocation{
			Directory:    portPath,
			Source:       formatOverlaySource(overlayPath, overlayIdx),
			PortDirFound: found,
			State:        state,
			Reason:       reason,
		})
		if state == CandidateStateUnreadable && blockingOverlay == nil {
			blocking := res.AllCandidates[len(res.AllCandidates)-1]
			blockingOverlay = &blocking
			blockingOverlayPrecedence = overlayIdx
		}

		if found {
			if winner == nil {
				// First match wins.
				winner = &Winner{
					Directory: portPath,
					Source:    formatOverlaySource(overlayPath, overlayIdx),
				}
				winnerPrecedence = overlayIdx
			} else {
				// Overlay-to-overlay shadowing detected.
				overlayToOverlayHit = true
				res.Shadows = append(res.Shadows, Shadow{
					Directory: portPath,
					Source:    formatOverlaySource(overlayPath, overlayIdx),
				})
			}
		}
	}

	// Check builtin (only if vcpkg_root is supplied).
	if hasVcpkg {
		vcpkgRoot := strings.TrimSpace(args.VcpkgRoot)
		builtinPortPath := filepath.Join(vcpkgRoot, "ports", port)
		res.Evidence.AddPath(builtinPortPath)
		if state, reason := inspectRoot(deps, vcpkgRoot); state == CandidateStateUnreadable {
			builtinUnreadable = true
			// If we already have a winner from an overlay, don't fail.
			// But record the builtin as unreadable.
			res.AllCandidates = append(res.AllCandidates, CandidateLocation{
				Directory:    builtinPortPath,
				Source:       "builtin (vcpkg_root)",
				PortDirFound: false,
				State:        CandidateStateUnreadable,
				Reason:       "vcpkg_root unreadable: " + reason,
			})
		} else {
			state, reason := inspectPortCandidate(deps, builtinPortPath)
			found := state == CandidateStateFound
			builtinSource := "builtin " + filepath.Join(vcpkgRoot, "ports")
			res.AllCandidates = append(res.AllCandidates, CandidateLocation{
				Directory:    builtinPortPath,
				Source:       builtinSource,
				PortDirFound: found,
				State:        state,
				Reason:       reason,
			})

			if found {
				if winner == nil {
					// Builtin wins if no overlay matched.
					winner = &Winner{
						Directory: builtinPortPath,
						Source:    builtinSource,
					}
					winnerPrecedence = len(args.OverlayPorts)
				} else {
					// Overlay already won, builtin is shadowed.
					res.Shadows = append(res.Shadows, Shadow{
						Directory: builtinPortPath,
						Source:    builtinSource,
					})
				}
			}
		}
	} else {
		res.AllCandidates = append(res.AllCandidates, CandidateLocation{
			Source: "builtin",
			State:  CandidateStateNotChecked,
			Reason: "vcpkg_root was not supplied",
		})
	}

	// Return final result.
	if blockingOverlay != nil && (winner == nil || blockingOverlayPrecedence < winnerPrecedence) {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonHigherPrecedenceOverlayUnreadable
		res.BlockingCandidate = blockingOverlay
		return res
	}
	if winner != nil {
		res.Status = evidence.StatusOK
		res.Winner = winner
		res.OverlayToOverlayShadowingOccurred = overlayToOverlayHit
		return res
	}
	if builtinUnreadable {
		res.Status = evidence.StatusUnknown
		res.Reason = ReasonRootUnreadable
		return res
	}

	// No winner found anywhere.
	res.Status = evidence.StatusUnknown
	res.Reason = ReasonPortNotFound
	return res
}

// formatOverlayIndex returns a zero-padded index for display in source labels.
func formatOverlayIndex(idx int) string {
	if idx < 10 {
		return "0" + string(rune('0'+idx))
	}
	return string(rune('0'+(idx/10))) + string(rune('0'+(idx%10)))
}

// formatOverlaySource returns a human-readable source label for an overlay.
func formatOverlaySource(overlayPath string, idx int) string {
	// Return the overlay path itself as the source label for clarity.
	return overlayPath
}

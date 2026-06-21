//go:build !windows

// client_write_resolve_posix.go — POSIX leg of the symlink resolver
// used by the secure-write relax lane. filepath.EvalSymlinks works
// uniformly on POSIX (no subst/junction abstraction to traverse).

package api

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func resolveSymlinkFinalPath(path string) (string, error) {
	// Absolutize the INPUT before EvalSymlinks. A relative client-config
	// symlink (link.json -> real.json in the same dir) resolves to a RELATIVE
	// target when the input is relative, and even an absolute input that points
	// at a relative-target symlink yields an absolute result only because the
	// link's own directory is absolute. Anchoring on an absolute input
	// guarantees an ABSOLUTE resolved path so (a) the volume-root component
	// descent (openResolvedParentByComponentDescentPosix) can anchor at "/" and
	// (b) the PR-A full-target pin — derived and re-verified through this SAME
	// owner (resolveSymlinkForSecureWrite -> resolveSymlinkFinalPath at both
	// confirm time and write time) — stays byte-consistent (no
	// relative-vs-absolute mismatch can break the pin or the same-parent-repoint
	// guard). The Windows leg already returns an absolute path
	// (GetFinalPathNameByHandle), so this only changes the POSIX relative case.
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve symlink: absolutize %q: %w", path, err)
	}
	return filepath.EvalSymlinks(abs)
}

// secureWriteThroughResolvedParentHandle is the POSIX handle-pinned
// write-through for the symlink-resolve relax lane (env-var F2 and scoped
// consent). It is the AF-1 fix: instead of returning a resolved path STRING
// for SecureWriteClientConfig to re-walk (filepath.Split + a second
// path-based openat of the parent), it descends to the resolved target's
// parent COMPONENT-BY-COMPONENT, O_NOFOLLOW at every step, anchored at the
// VOLUME ROOT of the resolved target (POSIX = "/"), and runs the shared
// hardened owner (secureWriteClientConfigToResolvedParent) against the final
// held parent fd.
//
// AF-1 F1 — why component-by-component (not one path-based open of the whole
// parent string): O_NOFOLLOW on a single open of the full parent path
// protects ONLY the final component; the kernel re-walks every INTERMEDIATE
// component at open time, so an intermediate dir swapped to a symlink between
// resolve and this open would redirect the fd. Opening one real component at
// a time relative to the previously-held fd, O_NOFOLLOW each step, never
// follows a swapped intermediate.
//
// Trust anchor = the volume root of the RESOLVED target, NOT $HOME. The
// resolved target is frequently OUTSIDE home by design (the motivating case
// ~/.codex/config.toml → E:\env\Agents\.codex\config.toml). There is NO
// path-containment refusal here: the operator already consented to THIS
// resolved target (env-var / scoped consent), and the chain already exists
// (nothing is created). The ONLY property delivered is "no intermediate
// component is followed through a swap" — this is NOT G17's pathUnderHome
// containment or its mkdir-if-missing.
//
// resolvedTarget MUST already be the fully-resolved final disk path (from
// resolveSymlinkFinalPath / EvalSymlinks). It returns the resolved-parent
// directory path (filepath.Dir of resolvedTarget, cleaned) so the caller can
// pin-match it against a scoped consent's PinnedResolvedPath (SEAM-B
// swap-between-confirm-and-write guard).
func secureWriteThroughResolvedParentHandle(resolvedTarget string, contents []byte, skipParentGate bool) (string, error) {
	parentDir, base := filepath.Split(resolvedTarget)
	if base == "" {
		return "", fmt.Errorf("secure write: empty base name in resolved target %q", resolvedTarget)
	}
	if parentDir == "" {
		parentDir = "."
	}
	parentDir = filepath.Clean(parentDir)

	dirFd, err := openResolvedParentByComponentDescentPosix(resolvedTarget)
	if err != nil {
		return parentDir, err
	}
	defer unix.Close(dirFd)

	if err := secureWriteClientConfigToResolvedParent(dirFd, parentDir, base, contents, skipParentGate); err != nil {
		return parentDir, err
	}
	return parentDir, nil
}

// openResolvedParentByComponentDescentPosix opens the PARENT directory of
// `resolvedTarget` by descending from the volume root ("/" on POSIX)
// component-by-component, O_NOFOLLOW at every step. The returned fd is the
// caller's pinned parent; the caller owns its close.
//
// Per-component open posture (Finding 2 + Finding 3 — read-gating ancestors):
//   - The ROOT ANCHOR ("/") and INTERMEDIATE ancestors are opened SEARCH-ONLY
//     (searchOnlyDirOpenFlags / openSearchOnlyDirAt: O_PATH on Linux, O_RDONLY
//     preview-fallback on darwin) so an execute-only ancestor (0711/0111) — or
//     an execute-only "/" (Finding 3) — is traversable without directory READ
//     permission; ordinary path traversal and the old single parent open only
//     ever needed SEARCH/EXECUTE. O_NOFOLLOW|O_DIRECTORY still refuse a
//     swapped-symlink or non-dir intermediate, so the F1 TOCTOU closure is
//     unchanged. (When "/" is itself the final parent — the pathological "file
//     directly under /" case — it falls back to the normal read fd; see below.)
//   - The FINAL parent (the directory that receives the temp-create + rename)
//     is opened with the NORMAL read fd (openExistingRealDirAt), exactly as
//     before, because the shared write owner runs the parent-dir mode/uid gate
//     (when not skipped) and the *at write ops against this fd. The final
//     parent is the operator's own config directory and is read-traversable;
//     only the ANCESTORS above it ever needed the search-only relaxation.
//
// This deliberately does NOT touch openExistingRealDirAt or G17's
// mkdirOrOpenRealDirAt — G17's read-fd DACL/mode-verify path is unchanged.
//
// resolvedTarget must be absolute (resolveSymlinkFinalPath now absolutizes the
// input before EvalSymlinks, so a relative-target symlink still yields an
// absolute resolved path). The base name is dropped — only the directory
// components are descended; the final filename is published by the shared
// owner's handle-relative atomic rename, not opened here.
func openResolvedParentByComponentDescentPosix(resolvedTarget string) (int, error) {
	cleaned := filepath.Clean(resolvedTarget)
	if !filepath.IsAbs(cleaned) {
		return -1, fmt.Errorf("secure write: resolved target %q is not absolute; cannot descend from volume root", resolvedTarget)
	}

	// Drop the leading "/" and split into components. The LAST component is
	// the base (the file to be written) and is NOT descended — only its
	// ancestor directories are opened. An empty/"." component is skipped
	// (defensive; filepath.Clean already collapses these).
	rel := strings.TrimPrefix(cleaned, "/")
	parts := strings.Split(rel, "/")
	// parts[len-1] is the base name; descend parts[:len-1].
	dirComponents := parts[:len(parts)-1]

	// Identify the LAST non-empty directory component — the final parent —
	// so it (and only it) is opened with the normal read fd. Computing the
	// index over the cleaned components (rather than assuming dirComponents
	// has no trailing empties) keeps the "final parent gets the read fd"
	// rule correct even on a defensive empty/"." entry.
	finalParentIdx := -1
	for i, comp := range dirComponents {
		if comp == "" || comp == "." {
			continue
		}
		finalParentIdx = i
	}

	// Volume root anchor. On POSIX the volume name is empty and the root is
	// "/". O_NOFOLLOW|O_DIRECTORY keep the descent uniform (the root is never
	// a symlink). Open posture (Finding 3): when at least one intermediate
	// component follows, "/" is TRAVERSED THROUGH to reach a deeper final
	// parent, so it is opened SEARCH-ONLY (searchOnlyDirOpenFlags / O_PATH on
	// Linux) — ordinary absolute traversal + the old single parent open only
	// needed SEARCH on "/", so an execute-only root no longer reintroduces a
	// READ requirement before the O_PATH intermediate walk. When there is NO
	// intermediate component (the pathological "file directly under /" case),
	// "/" IS the final parent and must carry the normal read fd the shared
	// write owner's gate + *at write ops require, so it is opened O_RDONLY
	// exactly as the pre-Finding-3 code did — no regression for that case.
	var anchorFd int
	var err error
	if finalParentIdx < 0 {
		// No intermediate: "/" is itself the final parent (read fd).
		anchorFd, err = unix.Open("/", unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	} else {
		// "/" is an intermediate ancestor: search-only.
		anchorFd, err = unix.Open("/", searchOnlyDirOpenFlags(), 0)
	}
	if err != nil {
		return -1, fmt.Errorf("secure write: open volume-root anchor /: %w", err)
	}

	curFd := anchorFd
	for i, comp := range dirComponents {
		if comp == "" || comp == "." {
			continue
		}
		var nextFd int
		var openErr error
		if i == finalParentIdx {
			// Final parent: normal read fd (gate verify + *at write ops).
			nextFd, openErr = openExistingRealDirAt(curFd, comp)
		} else {
			// Intermediate ancestor: search-only (traverse an execute-only
			// dir without READ); O_NOFOLLOW still refuses a swapped symlink.
			nextFd, openErr = openSearchOnlyDirAt(curFd, comp)
		}
		_ = unix.Close(curFd)
		if openErr != nil {
			return -1, fmt.Errorf(
				"secure write: refuse to descend through non-directory or symlink at component %q of resolved target %s: %w",
				comp, resolvedTarget, openErr,
			)
		}
		curFd = nextFd
		// Test-only injection seam: fires AFTER opening this component and
		// BEFORE opening the next, so a test can swap a not-yet-opened
		// intermediate into a symlink and prove the next O_NOFOLLOW open
		// refuses it (ELOOP). nil in production.
		if resolvedParentDescendStepHook != nil {
			resolvedParentDescendStepHook(comp)
		}
	}
	return curFd, nil
}

// openSearchOnlyDirAt opens an EXISTING real directory `comp` relative to the
// already-held `dirFd` SEARCH-ONLY (no directory READ requirement) for the
// resolved-symlink intermediate-ancestor descent. On Linux this uses O_PATH
// (the fd is valid as the dirfd for the next openat/renameat); on darwin it
// falls back to the read-fd flags (preview-tier read-gate). O_NOFOLLOW refuses
// a symlink at the slot (ELOOP) and O_DIRECTORY refuses a non-dir (ENOTDIR),
// so the F1 intermediate-swap TOCTOU closure is identical to the read-fd step.
//
// It is SEPARATE from openExistingRealDirAt by design: G17's
// mkdirOrOpenRealDirAt needs the normal read fd for its DACL/mode verify, so
// that shared step is left UNCHANGED; only this resolved-symlink walk drops
// the READ requirement on ancestors.
func openSearchOnlyDirAt(dirFd int, comp string) (int, error) {
	return unix.Openat(dirFd, comp, searchOnlyDirOpenFlags(), 0)
}

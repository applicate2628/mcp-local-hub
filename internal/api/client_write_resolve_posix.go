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
	return filepath.EvalSymlinks(path)
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
// component-by-component, O_NOFOLLOW at every step via openExistingRealDirAt
// (the step shared with G17's mkdirOrOpenRealDirAt). The returned fd is the
// caller's pinned parent; the caller owns its close.
//
// resolvedTarget must be absolute (EvalSymlinks returns an absolute path).
// The base name is dropped — only the intermediate directory components are
// descended; the final component is published by the shared owner's
// handle-relative atomic rename, not opened here.
func openResolvedParentByComponentDescentPosix(resolvedTarget string) (int, error) {
	cleaned := filepath.Clean(resolvedTarget)
	if !filepath.IsAbs(cleaned) {
		return -1, fmt.Errorf("secure write: resolved target %q is not absolute; cannot descend from volume root", resolvedTarget)
	}

	// Volume root anchor. On POSIX the volume name is empty and the root is
	// "/". Open it O_NOFOLLOW|O_DIRECTORY — the root is never a symlink, but
	// the flags keep the descent uniform.
	anchorFd, err := unix.Open("/", unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("secure write: open volume-root anchor /: %w", err)
	}

	// Drop the leading "/" and split into components. The LAST component is
	// the base (the file to be written) and is NOT descended — only its
	// ancestor directories are opened. An empty/"." component is skipped
	// (defensive; filepath.Clean already collapses these).
	rel := strings.TrimPrefix(cleaned, "/")
	parts := strings.Split(rel, "/")
	// parts[len-1] is the base name; descend parts[:len-1].
	dirComponents := parts[:len(parts)-1]

	curFd := anchorFd
	for _, comp := range dirComponents {
		if comp == "" || comp == "." {
			continue
		}
		nextFd, openErr := openExistingRealDirAt(curFd, comp)
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

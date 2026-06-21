//go:build !windows

// client_write_resolve_posix.go — POSIX leg of the symlink resolver
// used by the secure-write relax lane. filepath.EvalSymlinks works
// uniformly on POSIX (no subst/junction abstraction to traverse).

package api

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func resolveSymlinkFinalPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

// secureWriteThroughResolvedParentHandle is the POSIX handle-pinned
// write-through for the symlink-resolve relax lane (env-var F2 and scoped
// consent). It is the AF-1 fix: instead of returning a resolved path STRING
// for SecureWriteClientConfig to re-walk (filepath.Split + a second
// path-based openat of the parent), it OPENS the resolved target's parent
// directory ONCE (O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC — the final component is
// frozen at open and a symlinked parent is refused) and runs the shared
// hardened owner (secureWriteClientConfigToResolvedParent) against that held
// fd. A co-resident who swaps the symlink, or any intermediate component,
// BETWEEN resolve and this open cannot redirect the privileged write — the
// parent fd is pinned and there is no second path-based open to race.
//
// resolvedTarget MUST already be the fully-resolved final disk path (from
// resolveSymlinkFinalPath / EvalSymlinks). It returns the resolved-parent
// directory path that was actually opened (filepath.Dir of resolvedTarget,
// cleaned) so the caller can pin-match it against a scoped consent's
// PinnedResolvedPath (SEAM-B swap-between-confirm-and-write guard).
func secureWriteThroughResolvedParentHandle(resolvedTarget string, contents []byte, skipParentGate bool) (string, error) {
	parentDir, base := filepath.Split(resolvedTarget)
	if parentDir == "" {
		parentDir = "."
	}
	if base == "" {
		return "", fmt.Errorf("secure write: empty base name in resolved target %q", resolvedTarget)
	}
	parentDir = filepath.Clean(parentDir)

	dirFd, err := unix.Open(parentDir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return parentDir, fmt.Errorf("secure write: open resolved parent %s: %w", parentDir, err)
	}
	defer unix.Close(dirFd)

	if err := secureWriteClientConfigToResolvedParent(dirFd, parentDir, base, contents, skipParentGate); err != nil {
		return parentDir, err
	}
	return parentDir, nil
}

//go:build !windows

// internal/api/secure_create_client_config_parent_posix.go
//
// POSIX leg of SecureCreateClientConfigParentDir (G17). Creates the
// missing parent-directory chain of a client config COMPONENT-BY-
// COMPONENT, anchored fd-relative to the user home, refusing to follow
// any symlink and refusing any path outside the home. See the package
// doc in secure_create_client_config_parent.go for the full contract.
//
// On POSIX the load-bearing security boundary is the inode mode bits:
// mkdirat with mode 0700 (subject to umask, then re-asserted via fchmod)
// makes each created directory owner-only regardless of how loose the
// home is. O_NOFOLLOW on every open refuses a symlinked component
// atomically at the kernel, and mkdirat fails with EEXIST if the name
// already exists (so a race-planted entry cannot be silently adopted —
// the re-stat then classifies it).

package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func secureCreateClientConfigParentDirImpl(configPath string, skipParentGate bool) error {
	parent := filepath.Dir(configPath)
	if parent == "" || parent == "." {
		return fmt.Errorf("secure mkparent: empty parent for path %q", configPath)
	}
	parent = filepath.Clean(parent)

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("secure mkparent: resolve home: %w", err)
	}
	rel, under := pathUnderHome(home, parent)
	if !under {
		return fmt.Errorf(
			"secure mkparent: refuse to create %q outside the user home %q (init only creates client config dirs under your home)",
			parent, home,
		)
	}

	// Open the home anchor with O_NOFOLLOW|O_DIRECTORY so a symlinked
	// home component is refused and a non-dir home fails fast.
	anchorFd, err := unix.Open(home, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("secure mkparent: open home anchor %s: %w", home, err)
	}
	defer unix.Close(anchorFd)

	// Strict gate: when enforced, the home anchor must be owner-only.
	// Bypassed on the relax lane (skipParentGate=true); created dirs
	// stay 0700 regardless.
	if !skipParentGate {
		if err := verifyPosixParentDirFromFd(anchorFd); err != nil {
			return fmt.Errorf("%w (path %s): %v", ErrSecureWriteParentInsecure, home, err)
		}
	}

	// Descend the chain fd-relative, creating each missing component.
	// curFd is always an open fd on the current real directory; each
	// step either creates `comp` fresh or re-opens an existing real dir.
	curFd := anchorFd
	closeCur := func() {
		if curFd != anchorFd {
			_ = unix.Close(curFd)
		}
	}
	curPath := home
	for _, comp := range rel {
		nextPath := filepath.Join(curPath, comp)
		nextFd, err := mkdirOrOpenRealDirAt(curFd, comp, nextPath)
		closeCur()
		if err != nil {
			return err
		}
		curFd = nextFd
		curPath = nextPath
	}
	// curFd is the (now-existing) target parent. Close it (unless it is
	// the anchor — i.e. parent == home, which means parent already
	// existed and there was nothing to create).
	closeCur()
	return nil
}

// mkdirOrOpenRealDirAt ensures `comp` exists as a real directory under
// `dirFd` and returns an open O_NOFOLLOW|O_DIRECTORY fd on it. Either:
//
//   - mkdirat creates it fresh (mode 0700, fchmod-reasserted), or
//   - mkdirat returns EEXIST and the existing entry is a REAL directory
//     (re-opened with O_NOFOLLOW so a symlink is refused).
//
// A pre-existing symlink / reparse-point / non-directory at `comp` is
// REFUSED — the open with O_NOFOLLOW fails (ELOOP/ENOTDIR) and the error
// is surfaced; the walk never follows it.
//
// `fullPath` is only used for diagnostics.
func mkdirOrOpenRealDirAt(dirFd int, comp, fullPath string) (int, error) {
	mkErr := unix.Mkdirat(dirFd, comp, 0o700)
	if mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
		return -1, fmt.Errorf("secure mkparent: mkdirat %s: %w", fullPath, mkErr)
	}
	// Open the component fd-relative with O_NOFOLLOW so a symlink at
	// this slot (whether pre-existing or race-planted between mkdirat
	// EEXIST and here) is refused, and O_DIRECTORY so a non-dir fails.
	fd, openErr := unix.Openat(dirFd, comp, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if openErr != nil {
		if mkErr == nil {
			// We created it but cannot re-open as a real dir — anomalous
			// (a hostile swap in the microsecond window). Surface loud.
			return -1, fmt.Errorf(
				"secure mkparent: created %s but re-open as real dir failed (possible concurrent swap): %w",
				fullPath, openErr,
			)
		}
		// EEXIST path: the existing entry is not a followable real
		// directory (symlink → ELOOP, non-dir → ENOTDIR). Refuse.
		return -1, fmt.Errorf(
			"secure mkparent: refuse to descend through non-directory or symlink at %s: %w",
			fullPath, openErr,
		)
	}
	if mkErr == nil {
		// Freshly created — re-assert 0700 against umask drift.
		if err := unix.Fchmod(fd, 0o700); err != nil {
			_ = unix.Close(fd)
			return -1, fmt.Errorf("secure mkparent: fchmod %s: %w", fullPath, err)
		}
	}
	return fd, nil
}

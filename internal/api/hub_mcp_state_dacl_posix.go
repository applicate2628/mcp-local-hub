//go:build !windows

// hub_mcp_state_dacl_posix.go — POSIX leg of VerifyHubMcpStateDACL.
//
// Sequence:
//
//  1. open(path, O_RDONLY|O_NOFOLLOW|O_CLOEXEC). O_NOFOLLOW returns
//     ELOOP when path is a symlink; we map that to ErrIrregularFile.
//  2. fstat(fd). Reject:
//     - irregular file types (symlink survives only if ELOOP didn't
//       fire; defense in depth via os.FileMode.Type() bits)
//     - non-owner uid (ErrWrongOwner)
//     - mode bits with group / other access (ErrTooLoose)
//
// On POSIX the per-user state-dir is 0700, so ancestor-chain safety
// is covered by the trust boundary. This file only verifies the leaf
// file's metadata.

package api

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func verifyHubMcpStateDACLImpl(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return ErrIrregularFile
		}
		return err
	}
	defer unix.Close(fd)

	// Stat from the open fd so a swap between stat-and-read is
	// impossible. Use unix.Fstat directly (NOT os.NewFile + f.Stat) —
	// os.File installs a finalizer that may close fd after the
	// surrounding unix.Close has already run, causing nondeterministic
	// EBADF or unrelated-fd close in long-running processes (codex bot
	// r1 P2 closure).
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("hub-mcp state verify: fstat: %w", err)
	}
	// Map mode to os.FileMode for symlink/irregular check.
	mode := os.FileMode(st.Mode & 0o777)
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFLNK:
		return ErrIrregularFile
	case syscall.S_IFREG:
		// regular file — proceed
	default:
		// FIFO, socket, block/char device, etc.
		return ErrIrregularFile
	}
	// Wrap sentinels with operator-actionable context (path + uid/mode
	// bits). Phase 2's hub-mcp loader surfaces these strings directly
	// when refusing to start the hub; bare sentinels make debugging
	// painful. Wrapped form remains errors.Is-compatible — callers
	// that branch on `errors.Is(err, ErrWrongOwner)` continue to work.
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: path=%s uid=%d (need current uid %d)", ErrWrongOwner, path, st.Uid, os.Getuid())
	}
	if mode&0o077 != 0 {
		return fmt.Errorf("%w: path=%s mode=%04o", ErrTooLoose, path, mode)
	}
	return nil
}

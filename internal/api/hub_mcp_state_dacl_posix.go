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
	// impossible.
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return ErrIrregularFile
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("hub-mcp state verify: stat sys() type unexpected")
	}
	// Wrap sentinels with operator-actionable context (path + uid/mode
	// bits). Phase 2's hub-mcp loader surfaces these strings directly
	// when refusing to start the hub; bare sentinels make debugging
	// painful. Wrapped form remains errors.Is-compatible — callers
	// that branch on `errors.Is(err, ErrWrongOwner)` continue to work.
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: path=%s uid=%d (need current uid %d)", ErrWrongOwner, path, st.Uid, os.Getuid())
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: path=%s mode=%04o", ErrTooLoose, path, info.Mode().Perm())
	}
	return nil
}

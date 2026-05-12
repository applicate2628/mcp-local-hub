//go:build !windows

// hub_mcp_state_dacl_posix.go — POSIX leg of VerifyHubMcpStateDACL.
//
// Sequence (codex bot r8 P1 closure — handle-bound parent verify):
//
//  1. Open parent dir via path → pfd
//  2. Verify parent uid + mode bits (pfd-bound fstat)
//  3. Openat(pfd, basename) → fd. Uses pfd as the dir-handle anchor so
//     the file open resolves RELATIVE to the parent we just verified.
//     Defeats "swap parent between file-open and parent-open" races.
//  4. Verify file uid + mode bits (fd-bound fstat)
//  5. Reject any irregular file type
//
// Order matters: parent-first guarantees the parent we admit is the
// same one the file is reached through.

package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func verifyHubMcpStateDACLImpl(path string) error {
	parentPath := filepath.Dir(path)
	basename := filepath.Base(path)

	// Step 1: Open parent dir FIRST.
	pfd, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open parent %s: %w", parentPath, err)
	}
	defer unix.Close(pfd)

	// Step 2: Verify parent uid + mode via pfd-bound fstat.
	var pst unix.Stat_t
	if err := unix.Fstat(pfd, &pst); err != nil {
		return fmt.Errorf("fstat parent %s: %w", parentPath, err)
	}
	pmode := uint32(pst.Mode)
	if int(pst.Uid) != os.Getuid() {
		return fmt.Errorf("%w: parent=%s uid=%d (need current uid %d)", ErrWrongOwner, parentPath, pst.Uid, os.Getuid())
	}
	if pmode&0o077 != 0 {
		return fmt.Errorf("%w: parent=%s mode=%04o exposes bits to group/world", ErrTooLoose, parentPath, pmode&0o777)
	}

	// Step 3: Openat the file via pfd (TOCTOU-safe: parent we
	// just verified IS the directory the file is reached through).
	fd, err := unix.Openat(pfd, basename, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return ErrIrregularFile
		}
		return err
	}
	defer unix.Close(fd)

	// Step 4: Stat from the open fd. Use unix.Fstat directly (NOT
	// os.NewFile + f.Stat) — os.File installs a finalizer that may
	// close fd after the surrounding unix.Close has already run,
	// causing nondeterministic EBADF or unrelated-fd close in
	// long-running processes (codex bot r1 P2 closure).
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("hub-mcp state verify: fstat: %w", err)
	}
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
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: path=%s uid=%d (need current uid %d)", ErrWrongOwner, path, st.Uid, os.Getuid())
	}
	if mode&0o077 != 0 {
		return fmt.Errorf("%w: path=%s mode=%04o", ErrTooLoose, path, mode)
	}
	return nil
}

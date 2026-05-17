//go:build !windows

// binary_rename_aside_posix.go — POSIX rename-aside symmetry with the
// Windows leg.
//
// On POSIX, atomic rename(2) over a running executable is safe because
// unlink(2) decrements the link count while the kernel keeps the inode
// alive until all opens close (the executing image retains its mapping
// through its open file descriptor on the original inode). Linux,
// macOS, and FreeBSD all behave this way; ETXTBSY is not raised on
// rename, only on opening the still-running binary for write.
//
// We still create the `.old-<ts>` aside file (instead of a direct
// rename-over) so SweepOldBinaries has consistent state to clean up
// regardless of platform. This keeps operator forensics uniform —
// "the prior installed binary is at <install-dir>/mcphub.old-<ts>"
// holds on all OSes until the 7-day sweep removes it.

package api

import (
	"fmt"
	"os"
	"time"
)

// renameAsideTimestampLayout matches the Windows leg (filename-safe,
// sortable). POSIX accepts colons in filenames but we use the same
// no-colon layout for cross-platform sweep uniformity and so backup
// tools that round-trip names through Windows shares do not mangle
// the suffix.
const renameAsideTimestampLayout = "20060102T150405Z"

// RenameAsideReplace performs the POSIX two-step binary replacement:
// rename target → target+".old-<ts>", then rename newSrc → target.
//
// On step-2 failure the function attempts a best-effort rollback so
// the prior binary path is restored.
func RenameAsideReplace(target, newSrc string) error {
	ts := time.Now().UTC().Format(renameAsideTimestampLayout)
	oldPath := target + ".old-" + ts

	if err := os.Rename(target, oldPath); err != nil {
		return fmt.Errorf("rename target→old (%s → %s): %w", target, oldPath, err)
	}
	if err := os.Rename(newSrc, target); err != nil {
		// Rollback best-effort.
		_ = os.Rename(oldPath, target)
		return fmt.Errorf("rename newSrc→target (%s → %s): %w", newSrc, target, err)
	}
	return nil
}

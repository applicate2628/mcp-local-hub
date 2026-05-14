//go:build !windows

package api

import (
	"fmt"
	"os"
)

// hardenTempFileForUnhardenedFallback sets owner-only permissions on
// the relax-lane temp file before contents land. POSIX uses Chmod
// 0o600 — the kernel-side mode bits ARE the security boundary
// (the file becomes inaccessible to group/world regardless of
// umask, parent dir mode, or pre-existing ACEs).
//
// Bot r1 P1 closure on PR #185 distinguishes this from the Windows
// case where Chmod is purely a FILE_ATTRIBUTE_READONLY toggle and
// does not affect the DACL — see client_write_fallback_windows.go
// for the equivalent ACL hardening on that platform.
func hardenTempFileForUnhardenedFallback(f *os.File) error {
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod to 0600: %w", err)
	}
	return nil
}

//go:build windows

package api

import (
	"fmt"
	"os"
)

// hardenTempFileForUnhardenedFallback sets owner-only DACL on the
// relax-lane temp file before contents land. Bot r1 P1 closure on
// PR #185: Go's os.Chmod on Windows only toggles FILE_ATTRIBUTE_
// READONLY and does NOT alter the DACL — so a Chmod-only relax
// would leave the new file inheriting the parent dir's permissive
// ACEs (CodexSandboxUsers, AppContainer SIDs, orphan AD SIDs —
// exactly the principals the parent-dir gate was trying to keep
// out). Token content would become readable by those principals
// even though the gate had "fired" and the operator never opted
// into widening the trust boundary.
//
// Fix: drive the file's DACL explicitly via setRestrictiveDACL
// (reused from the hardened-write pipeline in secure_write_
// windows.go). This grants GENERIC_ALL to {current-user-SID,
// LocalSystem, BuiltinAdministrators} only, with PROTECTED_DACL_
// SECURITY_INFORMATION to block inheritance from the parent. The
// new file's security boundary is now identical to what the
// hardened pipeline would have produced — the parent-dir gate
// just got bypassed because the parent itself isn't single-user-
// safe, not because the new file shouldn't be.
//
// The relax lane's residual gap vs the hardened pipeline narrows
// to: a small TOCTOU window between os.Lstat (in
// fallbackWriteRefusingSymlink) and the os.Rename below. Handle-
// relative ops would close it, but the relax lane documents the
// trade-off as the operator's choice to trust the host for
// symlink-swap races.
func hardenTempFileForUnhardenedFallback(f *os.File) error {
	// Use path-based SetNamedSecurityInfo (via setRestrictiveDACLByPath)
	// rather than handle-based setRestrictiveDACL: os.CreateTemp opens
	// the temp file without WRITE_DAC on the handle, so a handle-based
	// SetSecurityInfo call returns ERROR_ACCESS_DENIED. The path-based
	// variant re-opens internally with the right access. Same DACL
	// content (owner-only GENERIC_ALL for {current-user, LocalSystem,
	// BuiltinAdmins}; PROTECTED to block parent inheritance) — only
	// the access path differs.
	if err := setRestrictiveDACLByPath(f.Name()); err != nil {
		return fmt.Errorf("setRestrictiveDACLByPath on relax-lane temp: %w", err)
	}
	return nil
}

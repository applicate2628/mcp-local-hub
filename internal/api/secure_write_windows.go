//go:build windows

// secure_write_windows.go — Windows leg of SecureWriteClientConfig.
//
// Task 1.3 (this file's first revision) contains only a stub that
// returns ErrSecureWriteWindowsNotImplemented. Task 1.4 replaces the
// stub with the full NT-syscall sequence:
//
//   - openDirHandleNoReparse(parent)
//   - DACL verify on parent handle
//   - NtCreateFile(temp, FILE_CREATE) relative to parent handle
//   - SetSecurityInfo with restrictive DACL on file handle
//   - WriteFile + FlushFileBuffers
//   - NtSetInformationFile(FileRenameInformationEx) relative to parent
//   - re-open via parent handle + DACL re-verify
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"SecureWriteClientConfig sequence" (Windows leg).

package api

import "errors"

// errSecureWriteWindowsNotImplemented is returned by the Windows
// secure-write stub installed in Task 1.3. Task 1.4 replaces this
// stub with the full handle-relative implementation. Callers that
// reach this error during Phase 1 development indicate the Windows
// leg hasn't landed yet.
var errSecureWriteWindowsNotImplemented = errors.New("SecureWriteClientConfig: Windows handle-relative writer not yet implemented (Task 1.4)")

func secureWriteClientConfigImpl(_ string, _ []byte) error {
	return errSecureWriteWindowsNotImplemented
}

// Package api — G4 SecureWriteClientConfig (Phase 1, Task 1.3 / 1.4).
//
// SecureWriteClientConfig is the handle-relative, TOCTOU-safe writer
// used for token-bearing client config files (Phase 5 install
// reconciler) and hub state files (Phase 2 endpoint/token loader). It
// defeats every classic path-based race window:
//
//   - The parent dir is held open with an O_DIRECTORY / FILE_LIST_DIRECTORY
//     handle for the lifetime of the write. The temp file name and the
//     final rename target are resolved RELATIVE TO THAT HANDLE rather
//     than via a fresh walk from root.
//   - The temp name is unpredictable (crypto/rand 8 bytes hex) so a
//     same-uid attacker cannot pre-create the slot.
//   - On Windows the DACL is set on the file HANDLE before any bytes
//     hit disk; on POSIX 0600 is enforced by O_CREAT mode + a defensive
//     fchmod after open.
//   - The temp file is opened with O_NOFOLLOW (POSIX) /
//     FILE_OPEN_REPARSE_POINT-fail (Windows) so a pre-existing symlink
//     in the slot is refused outright.
//   - After the atomic rename, the destination is re-opened via the
//     SAME parent-dir handle and its DACL / mode is re-verified — this
//     catches policy ACLs that may auto-apply on some Windows paths
//     between rename and close.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"SecureWriteClientConfig sequence" + §"Windows DACL verification".
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 1.3
// (POSIX leg) and Task 1.4 (Windows leg).
//
// Caller contract: any error from SecureWriteClientConfig is a HARD
// FAIL — the caller MUST refuse to install the token-bearing config
// and fall back to per-daemon URLs. Do NOT downgrade to os.WriteFile
// on error; that would defeat the entire guarantee.
package api

// SecureWriteClientConfig writes contents to path atomically via a
// handle-relative pipeline. See the package doc for the sequence and
// the spec / plan references. Returns the first error from any step.
// On any error, no partial file is left at path; the temp file (if
// created) is unlinked.
//
// On POSIX: openat(parentDir) + Openat(O_CREAT|O_EXCL|O_NOFOLLOW|0600)
// + Fchmod(0600) + Write + Fsync + Renameat + post-rename re-Openat +
// mode-bit re-verify.
//
// On Windows: NtCreateFile relative to dirHandle (FILE_CREATE) +
// SetSecurityInfo with restrictive DACL on the handle + WriteFile +
// FlushFileBuffers + NtSetInformationFile(FileRenameInformationEx,
// RootDirectory=dirHandle) + post-rename Openat + DACL re-verify.
//
// Spec: §"SecureWriteClientConfig sequence". Plan: Task 1.3 + 1.4.
func SecureWriteClientConfig(path string, contents []byte) error {
	return secureWriteClientConfigImpl(path, contents)
}

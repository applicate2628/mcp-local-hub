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

import (
	"errors"
	"time"
)

// ErrSecureWriteParentInsecure is the typed error returned by
// SecureWriteClientConfig when the destination's IMMEDIATE parent
// directory cannot pass the single-user hardening gate (Windows:
// non-allowlist DACL ACEs; POSIX: any group/world permission bits
// or non-owner uid).
//
// Cross-package callers (the clients.WriteConfigFile hook in
// client_write_init.go) match on this sentinel via errors.Is to
// decide whether to:
//   - surface the operator-opt-in hint, or
//   - fall back to a plain os.WriteFile when the operator has
//     explicitly enabled the unhardened path via the
//     MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE env var.
//
// Issue #161 P1 closure: the global SecureWriteClientConfig wiring
// in client_write_init.go could break ordinary install/migrate on
// corp-policy Windows machines (Domain Users inheriting read access
// on %USERPROFILE%) and on POSIX users whose $HOME has any group/
// world bits. The hardening is the spec'd outcome; the softening
// is operator-explicit, observable, and always logged.
var ErrSecureWriteParentInsecure = errors.New("secure write: parent directory not single-user safe")

const (
	postRenameOpenMaxAttempts = 3
	postRenameOpenRetryDelay  = 10 * time.Millisecond
)

// SecureWriteClientConfig writes contents to path atomically via a
// handle-relative pipeline. See the package doc for the sequence and
// the spec / plan references. Returns the first error from any step.
// On errors before the atomic rename, no partial file is left at path
// and the temp file (if created) is unlinked. After the atomic rename,
// a definitive owner/mode/DACL verification failure removes the
// just-published file. A transient post-rename re-open failure is returned
// without erasing the complete published file, because the writer has not
// proved that file is unsafe.
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
	return secureWriteClientConfigImpl(path, contents, false)
}

// secureWriteClientConfigSkipParentGate is the relax lane — runs the
// SAME hardened pipeline as SecureWriteClientConfig but with the
// parent-dir DACL/mode gate BYPASSED. Used by the
// secureWriteWithOperatorOpt fallback when the operator (implicitly
// or explicitly) opted into the unhardened-parent path. The per-file
// DACL/mode hardening still applies at temp-create time (no race
// window), so the new file is owner-only regardless of what
// principals are on the parent's ACL.
//
// PR #185 r3: replaces the previous fallbackWriteRefusingSymlink +
// os.CreateTemp + path-based SetNamedSecurityInfo path, which left a
// pre-hardening window during which co-resident principals could
// race-open the temp file. The handle-relative pipeline closes that
// window by installing the restrictive DACL on the file HANDLE
// before any bytes hit disk (Windows) or by creating with mode 0600
// via O_CREAT (POSIX).
func secureWriteClientConfigSkipParentGate(path string, contents []byte) error {
	return secureWriteClientConfigImpl(path, contents, true)
}

// postRenameVerifyFailHook is a TEST-ONLY seam consulted by both the
// POSIX and Windows secureWriteClientConfigImpl legs immediately after
// the post-rename re-open, BEFORE the real owner/mode/DACL verify. When
// nil (the production default) it is a no-op and the real verify runs.
// When a test sets it to a function returning a non-nil error, the impl
// treats that as a post-rename verify failure and runs the
// "no file on error" cleanup (handle/dirfd-relative delete of the
// just-published file). It is the only platform-neutral way to exercise
// the post-rename cleanup contract without synthesizing a real
// mode/DACL mismatch on the persisted inode. Never set in production.
var postRenameVerifyFailHook func() error

// postRenameOpenFailHook is a TEST-ONLY seam consulted by both platform legs
// immediately before the post-rename re-open. When nil, production opens the
// renamed destination normally. Tests set it to synthesize transient re-open
// failures that are otherwise timing-dependent and difficult to force
// deterministically across platforms. Never set in production.
var postRenameOpenFailHook func() error

// The AF-1 secure-write observation counters
// (secureWritePathBasedStringEntryCount / secureWriteResolvedParentEntryCount)
// and their observeStringEntry() / observeResolvedParentEntry() recorders live
// behind the af1_counters build tag — secure_write_counters_aftag.go (the real
// counters) and secure_write_counters_prod.go (empty no-op recorders compiled
// into shipped binaries). The production secure-write lanes call the observe*
// recorders, never an inline increment, so a shipped binary mutates NO shared
// counter state on a write (the F3 data-race fix) and contains no counter
// symbol (compile-out proof). See those two files for the full rationale.

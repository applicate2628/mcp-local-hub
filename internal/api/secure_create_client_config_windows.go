//go:build windows

// internal/api/secure_create_client_config_windows.go
//
// Windows leg of SecureCreateClientConfigIfMissing (PR #208 deep-sec
// Lane C #1 closure). Structurally identical to the Windows leg of
// SecureWriteClientConfig in secure_write_windows.go with three
// differences:
//
//   - Pre-flight: if a regular file already exists at the destination,
//     return (false, nil) — idempotent success. Symlinks and other
//     non-regular entries return a refusal error (the existing
//     `refusePreexistingReparsePoint` covers the symlink case; the
//     extra Lstat refusal catches Windows directories, junctions to
//     directories, and named pipes that aren't reparse points).
//   - Rename: uses `ntRenameRelativeNoReplace` instead of
//     `ntRenameRelative`. The Flags field omits
//     `FILE_RENAME_REPLACE_IF_EXISTS`, so NtSetInformationFile returns
//     STATUS_OBJECT_NAME_COLLISION if the destination exists. On
//     collision, the caller re-classifies the winner and returns
//     idempotent success if it's a regular file.
//   - Return shape: `(created bool, err error)` instead of `error`.
//     The `created` flag is true only when this call's rename
//     succeeded (a regular file landed by another writer in the race
//     window flips it to false without error).
//
// Reuses every other helper from secure_write_windows.go:
// openDirHandleNoReparse, verifyWindowsDACLFromHandle,
// refusePreexistingReparsePoint, buildRestrictiveSecurityDescriptor,
// ntCreateRelative, setRestrictiveDACL, windowsWriteAll,
// ntOpenRelative.

package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// secureCreateClientConfigIfMissingImpl is the Windows entry point
// wired to SecureCreateClientConfigIfMissing. Walks the same
// hardened pipeline as secureWriteClientConfigImpl but with
// no-replace rename + idempotent regular-file early-out.
func secureCreateClientConfigIfMissingImpl(path string, contents []byte, skipParentGate bool) (created bool, err error) {
	parentDir, base := filepath.Split(path)
	if parentDir == "" {
		parentDir = "."
	}
	if base == "" {
		return false, fmt.Errorf("secure create: empty base name in path %q", path)
	}
	parentDir = filepath.Clean(parentDir)

	dirHandle, err := openDirHandleNoReparse(parentDir)
	if err != nil {
		return false, fmt.Errorf("secure create: open parent %s: %w", parentDir, err)
	}
	defer windows.CloseHandle(dirHandle)

	// skipParentGate=true bypasses ONLY the parent-dir DACL gate.
	// Per-file allowlist DACL at create time (step 5 below via
	// ntCreateRelative + OBJECT_ATTRIBUTES.SecurityDescriptor) still
	// makes the new stub owner-only regardless of parent broadening.
	// Mirror of secure_write_windows.go's skipParentGate semantics.
	if !skipParentGate {
		if err := verifyWindowsDACLFromHandle(dirHandle); err != nil {
			return false, fmt.Errorf("%w (path %s): %v", ErrSecureWriteParentInsecure, parentDir, err)
		}
	}

	// Pre-flight: idempotent fast path. If a regular file is already
	// present at `base`, return (false, nil). This handles the
	// "second-click" case where another tab or another mcphub
	// invocation already wrote the file between the GUI's scan
	// refresh and the Initialize POST. Reparse-point refusal is
	// delegated to `refusePreexistingReparsePoint` below; the Lstat
	// here also catches Windows directories, junctions, and named
	// pipes that are not reparse points.
	if existed, refusal := lstatRefusingForCreate(path); refusal != nil {
		return false, refusal
	} else if existed {
		return false, nil
	}

	if err := refusePreexistingReparsePoint(dirHandle, base); err != nil {
		return false, fmt.Errorf("secure create: target %s: %w", path, err)
	}

	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		return false, fmt.Errorf("secure create: crypto/rand: %w", err)
	}
	tempName := fmt.Sprintf(".%s.init.tmp.%d.%s", base, os.Getpid(), hex.EncodeToString(randBytes))

	sd, err := buildRestrictiveSecurityDescriptor()
	if err != nil {
		return false, fmt.Errorf("secure create: build SD: %w", err)
	}

	fileHandle, err := ntCreateRelative(
		dirHandle,
		tempName,
		windows.DELETE|windows.GENERIC_WRITE|windows.SYNCHRONIZE|windows.WRITE_DAC,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		sd,
	)
	if err != nil {
		return false, fmt.Errorf("secure create: ntcreate temp %s: %w", tempName, err)
	}
	closed := false
	defer func() {
		if !closed {
			windows.CloseHandle(fileHandle)
		}
	}()

	cleanup := func() {
		_ = setFileDeleteOnClose(fileHandle)
	}

	if err := setRestrictiveDACL(fileHandle); err != nil {
		cleanup()
		return false, fmt.Errorf("secure create: set DACL: %w", err)
	}

	if err := windowsWriteAll(fileHandle, contents); err != nil {
		cleanup()
		return false, fmt.Errorf("secure create: write temp: %w", err)
	}
	if err := windows.FlushFileBuffers(fileHandle); err != nil {
		cleanup()
		return false, fmt.Errorf("secure create: flush temp: %w", err)
	}

	// No-replace rename. STATUS_OBJECT_NAME_COLLISION (kernel) maps
	// to ERROR_ALREADY_EXISTS (Win32) via the NtSetInformationFile
	// shim — handled by re-classifying the winner.
	if renameErr := ntRenameRelativeNoReplace(fileHandle, dirHandle, base); renameErr != nil {
		cleanup()
		// Distinguish "destination exists" from any other rename
		// failure. STATUS_OBJECT_NAME_COLLISION surfaces as
		// ERROR_OBJECT_NAME_EXISTS or its Win32 equivalent.
		if isAlreadyExistsErr(renameErr) {
			// Re-classify what's at `base` now. If regular file (the
			// race-winner is a legitimate concurrent writer or a
			// previously-present regular file we missed in pre-flight),
			// return idempotent success. If symlink/non-regular,
			// refuse honestly so the operator sees the attacker-
			// planted-in-window case.
			if existed, refusal := lstatRefusingForCreate(path); refusal != nil {
				return false, refusal
			} else if existed {
				return false, nil
			}
			return false, fmt.Errorf(
				"secure create: rename collision but destination is now absent: %w",
				renameErr,
			)
		}
		return false, fmt.Errorf("secure create: ntrename %s -> %s: %w", tempName, base, renameErr)
	}

	closeErr := windows.CloseHandle(fileHandle)
	closed = true
	if closeErr != nil {
		return false, fmt.Errorf("secure create: close temp: %w", closeErr)
	}

	// Post-rename DACL re-verify. Symmetric with secure_write_windows.go
	// step 9.
	verifyHandle, err := ntOpenRelative(
		dirHandle,
		base,
		windows.GENERIC_READ|windows.READ_CONTROL,
	)
	if err != nil {
		return false, fmt.Errorf("secure create: re-open %s: %w", base, err)
	}
	defer windows.CloseHandle(verifyHandle)
	if err := verifyWindowsDACLFromHandle(verifyHandle); err != nil {
		return false, fmt.Errorf("secure create: post-rename DACL verify %s: %w", base, err)
	}
	return true, nil
}

// lstatRefusingForCreate inspects what's at `path` without following
// symlinks. Returns:
//
//   - existed=true,  err=nil: regular file (idempotent success).
//   - existed=false, err=nil: nothing there.
//   - existed=false, err!=nil: refusal (symlink, junction, dir, or
//     any non-regular entry) — surface so the operator can clean up
//     the slot before retrying.
//
// Mirror of clients.classifyExistingForInit; duplicated here so the
// hardened pipeline stays self-contained (no cross-package call from
// the security-critical create path back into clients).
func lstatRefusingForCreate(path string) (existed bool, err error) {
	st, statErr := os.Lstat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, statErr
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf(
			"secure create: refuse to initialize through symlink at %s "+
				"(remove the symlink and retry)",
			path,
		)
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf(
			"secure create: refuse to initialize over non-regular entry at %s (mode %s)",
			path, st.Mode(),
		)
	}
	return true, nil
}

// ntRenameRelativeNoReplace is the no-replace sibling of
// ntRenameRelative. Identical sequence except the Flags field omits
// FILE_RENAME_REPLACE_IF_EXISTS — NtSetInformationFile fails with
// STATUS_OBJECT_NAME_COLLISION if `newBase` exists, instead of
// silently replacing.
func ntRenameRelativeNoReplace(
	fileHandle windows.Handle,
	dirHandle windows.Handle,
	newBase string,
) error {
	newBaseUTF16, err := windows.UTF16FromString(newBase)
	if err != nil {
		return fmt.Errorf("utf16 %q: %w", newBase, err)
	}
	nameLenChars := len(newBaseUTF16) - 1
	if nameLenChars < 0 {
		nameLenChars = 0
	}
	nameLenBytes := nameLenChars * 2

	headerSize := int(unsafe.Offsetof(fileRenameInformationEx{}.FileName))
	bufferSize := headerSize + nameLenBytes
	if bufferSize < int(unsafe.Sizeof(fileRenameInformationEx{})) {
		bufferSize = int(unsafe.Sizeof(fileRenameInformationEx{}))
	}
	buffer := make([]byte, bufferSize)
	typedBuf := (*fileRenameInformationEx)(unsafe.Pointer(&buffer[0]))
	// POSIX semantics without REPLACE: rename fails on collision.
	typedBuf.Flags = windows.FILE_RENAME_POSIX_SEMANTICS
	typedBuf.RootDirectory = dirHandle
	typedBuf.FileNameLength = uint32(nameLenBytes)
	if nameLenChars > 0 {
		dst := unsafe.Slice((*uint16)(unsafe.Pointer(&typedBuf.FileName[0])), nameLenChars)
		copy(dst, newBaseUTF16[:nameLenChars])
	}

	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		fileHandle,
		&iosb,
		&buffer[0],
		uint32(bufferSize),
		fileInformationClassRenameEx,
	)
}

// isAlreadyExistsErr matches Windows error codes that indicate
// "destination already exists" from NtSetInformationFile's rename
// path. NtSetInformationFile's golang.org/x/sys/windows wrapper
// returns the NTSTATUS DIRECTLY (typed as `windows.NTStatus`,
// implements error) — NOT the Win32 errno. So the loser of a
// publish race surfaces as `STATUS_OBJECT_NAME_COLLISION`, not
// `ERROR_ALREADY_EXISTS`. The helper must therefore inspect both
// the NTStatus and Errno representations along the error chain.
//
// PR #208 deep-sec Lane A round 4 P2 closure: prior version only
// checked `windows.Errno` + `os.IsExist`. NTStatus did not match
// either path, so the loser branch fell through to 500 INIT_FAILED
// when the correct response is `(false, nil)` idempotent success.
func isAlreadyExistsErr(err error) bool {
	if err == nil {
		return false
	}
	if ntStatusIs(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		return true
	}
	if ntStatusIs(err, windows.STATUS_OBJECT_NAME_EXISTS) {
		return true
	}
	if errnoIs(err, windows.ERROR_ALREADY_EXISTS) {
		return true
	}
	if errnoIs(err, windows.ERROR_FILE_EXISTS) {
		return true
	}
	return os.IsExist(err)
}

// errnoIs unwraps to windows.Errno and compares against `want`.
func errnoIs(err error, want windows.Errno) bool {
	for cur := err; cur != nil; {
		if e, ok := cur.(windows.Errno); ok {
			return e == want
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := cur.(unwrapper); ok {
			cur = u.Unwrap()
			continue
		}
		return false
	}
	return false
}

// ntStatusIs unwraps to windows.NTStatus and compares against
// `want`. Sibling of errnoIs for the NTSTATUS error family
// returned by Nt-prefixed syscalls.
func ntStatusIs(err error, want windows.NTStatus) bool {
	for cur := err; cur != nil; {
		if s, ok := cur.(windows.NTStatus); ok {
			return s == want
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := cur.(unwrapper); ok {
			cur = u.Unwrap()
			continue
		}
		return false
	}
	return false
}

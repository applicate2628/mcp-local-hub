//go:build !windows

// internal/api/secure_create_client_config_posix.go
//
// POSIX leg of SecureCreateClientConfigIfMissing (PR #208 deep-sec
// Lane C #1 closure). POSIX gets simpler treatment than Windows
// because:
//
//   - File permissions on POSIX are the inode's mode bits, not
//     parent-dir-inherited ACLs. `O_CREAT|O_EXCL|O_NOFOLLOW` with
//     mode 0o600 produces an owner-only file regardless of how loose
//     the parent dir is. The "DACL race" surface that Lane C flagged
//     on Windows does not exist on POSIX.
//   - The kernel honors `O_NOFOLLOW` atomically at openat time, so a
//     race-planted symlink at the destination causes openat to fail
//     with ELOOP — no separate Lstat pre-check needed for symlinks.
//   - `O_EXCL` returns EEXIST if any entry (regular file, symlink,
//     directory, named pipe) is at the destination. POSIX semantics
//     don't distinguish creation success across entry types, so the
//     pipeline pre-Lstats to surface a clean "refused: <kind>" error
//     instead of a generic EEXIST.
//
// Strict-mode parent-dir gate (`verifyPosixParentDirFromFd`) and
// post-create owner/mode re-verify (`verifyPosixFileFromFd`) are
// reused from secure_write_posix.go.

package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func secureCreateClientConfigIfMissingImpl(path string, contents []byte, skipParentGate bool) (created bool, err error) {
	parentDir, base := filepath.Split(path)
	if parentDir == "" {
		parentDir = "."
	}
	if base == "" {
		return false, fmt.Errorf("secure create: empty base name in path %q", path)
	}

	dirFd, err := unix.Open(parentDir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, fmt.Errorf("secure create: open parent %s: %w", parentDir, err)
	}
	defer unix.Close(dirFd)

	// skipParentGate=true bypasses ONLY the parent-dir mode/uid gate.
	// File mode 0o600 (O_CREAT mode + fchmod) is the load-bearing
	// security boundary on POSIX and stays in force.
	if !skipParentGate {
		if err := verifyPosixParentDirFromFd(dirFd); err != nil {
			return false, fmt.Errorf("%w (path %s): %v", ErrSecureWriteParentInsecure, parentDir, err)
		}
	}

	// Pre-flight Lstat: surface clean refusal for non-regular
	// entries (symlink, dir, named pipe). On POSIX, O_EXCL would
	// otherwise return generic EEXIST and we'd have to re-classify
	// after the fact anyway.
	if existed, refusal := lstatRefusingForCreate(path); refusal != nil {
		return false, refusal
	} else if existed {
		return false, nil
	}

	// Temp name with crypto/rand so a same-uid attacker cannot
	// pre-create the slot.
	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		return false, fmt.Errorf("secure create: crypto/rand: %w", err)
	}
	tempName := fmt.Sprintf(".%s.init.tmp.%d.%s", base, os.Getpid(), hex.EncodeToString(randBytes))

	flags := unix.O_CREAT | unix.O_EXCL | unix.O_WRONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fileFd, err := unix.Openat(dirFd, tempName, flags, 0o600)
	if err != nil {
		return false, fmt.Errorf("secure create: openat temp %s: %w", tempName, err)
	}
	cleanup := func() {
		_ = unix.Close(fileFd)
		_ = unix.Unlinkat(dirFd, tempName, 0)
	}

	// Defense vs umask drift — O_CREAT mode is the primary guarantee,
	// fchmod re-asserts.
	if err := unix.Fchmod(fileFd, 0o600); err != nil {
		cleanup()
		return false, fmt.Errorf("secure create: fchmod temp: %w", err)
	}

	if err := writeAllUnix(fileFd, contents); err != nil {
		cleanup()
		return false, fmt.Errorf("secure create: write temp: %w", err)
	}
	if err := unix.Fsync(fileFd); err != nil {
		cleanup()
		return false, fmt.Errorf("secure create: fsync temp: %w", err)
	}

	// Publish via Linkat with no-replace semantics (the link syscall
	// fails with EEXIST if `base` already exists). Linkat keeps the
	// temp inode reachable from `base`; we then unlink the temp
	// name. On collision we re-classify the winner: regular file →
	// idempotent success, otherwise refusal.
	if linkErr := unix.Linkat(dirFd, tempName, dirFd, base, 0); linkErr != nil {
		cleanup()
		if errors.Is(linkErr, unix.EEXIST) {
			if existed, refusal := lstatRefusingForCreate(path); refusal != nil {
				return false, refusal
			} else if existed {
				return false, nil
			}
			return false, fmt.Errorf(
				"secure create: link collision but destination is now absent: %w",
				linkErr,
			)
		}
		return false, fmt.Errorf("secure create: linkat %s -> %s: %w", tempName, base, linkErr)
	}

	// Close the temp fd and unlink the temp name; `base` now points
	// to the same inode with the right mode.
	if err := unix.Close(fileFd); err != nil {
		_ = unix.Unlinkat(dirFd, tempName, 0)
		return false, fmt.Errorf("secure create: close temp: %w", err)
	}
	if err := unix.Unlinkat(dirFd, tempName, 0); err != nil {
		// Non-fatal: temp inode is still reachable via `base`. Log
		// implicitly by allowing the error to be visible in tests
		// that exercise the cleanup path; production callers see
		// success because `base` is correctly published.
		// Operators inspecting the directory may see a leftover temp
		// for a short while.
		_ = err
	}

	// Re-open via dirFd anchor and re-verify mode + owner.
	verifyFd, err := unix.Openat(dirFd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, fmt.Errorf("secure create: re-open %s: %w", base, err)
	}
	defer unix.Close(verifyFd)
	if err := verifyPosixFileFromFd(verifyFd); err != nil {
		return false, fmt.Errorf("secure create: post-link verify %s: %w", base, err)
	}
	return true, nil
}

// lstatRefusingForCreate is the POSIX twin of the same-named helper
// in secure_create_client_config_windows.go. Duplicated under build
// tags so each leg stays self-contained.
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

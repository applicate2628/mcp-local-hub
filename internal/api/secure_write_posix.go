//go:build !windows

// secure_write_posix.go — POSIX leg of SecureWriteClientConfig (Task 1.3).
//
// Sequence:
//
//  1. open(parentDir, O_DIRECTORY|O_RDONLY|O_CLOEXEC) — dirFd
//  2. fstat(dirFd): refuse if non-owner or group/world-writable
//  3. crypto/rand 8 bytes -> hex; tempName = ".<base>.tmp.<pid>.<hex>"
//  4. openat(dirFd, tempName, O_CREAT|O_EXCL|O_WRONLY|O_NOFOLLOW|O_CLOEXEC, 0600)
//  5. fchmod(tempFd, 0600) — defense vs umask drift
//  6. write(tempFd, contents)
//  7. fsync(tempFd)
//  8. renameat(dirFd, tempName, dirFd, base) — atomic + dirfd-relative
//  9. close(tempFd)
// 10. openat(dirFd, base, O_RDONLY|O_NOFOLLOW|O_CLOEXEC) — verifyFd
// 11. fstat(verifyFd): re-verify owner + mode
// 12. close(verifyFd); close(dirFd)
//
// Any error in steps 4-8 unlinks the temp file before returning. The
// state-dir trust boundary (per-user 0700) is the ancestor-chain
// guarantee; the dirHandle freezes the FINAL parent component for the
// rest of the sequence.

package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// secureWriteClientConfigImpl is the POSIX dirfd-relative writer.
// Symmetric with the Windows impl in secure_write_windows.go.
//
// When skipParentGate=true the parent-dir mode/uid verify (step 2)
// is SKIPPED — relax lane for hosts whose $HOME has group/world
// permission bits the operator does not want to tighten. The per-
// file mode 0600 applied via O_CREAT + Fchmod at steps 4-5 is still
// the load-bearing security boundary on POSIX (mode bits on the
// inode are the boundary, not the parent dir bits); only operator-
// uid alignment of the parent is relaxed. The dirfd-relative create
// + atomic renameat + post-rename verify (steps 4-11) all still
// apply, so the dirfd-anchored TOCTOU guarantees are preserved.
func secureWriteClientConfigImpl(path string, contents []byte, skipParentGate bool) error {
	parentDir, base := filepath.Split(path)
	if parentDir == "" {
		parentDir = "."
	}
	if base == "" {
		return fmt.Errorf("secure write: empty base name in path %q", path)
	}

	// 1. Open parent dir with O_DIRECTORY so a non-dir path is rejected
	// fast, and O_NOFOLLOW so a symlinked final component is refused.
	dirFd, err := unix.Open(parentDir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("secure write: open parent %s: %w", parentDir, err)
	}
	defer unix.Close(dirFd)

	// 2. Parent DACL verify reduces to owner-uid + non-loose mode
	// on POSIX. The per-user trust boundary covers ancestor chain.
	// Wrap with ErrSecureWriteParentInsecure so the cross-package
	// wrapper in client_write_init.go can match via errors.Is and
	// surface the operator opt-in hint (issue #161 P1).
	//
	// skipParentGate=true bypasses ONLY this step. The per-file mode
	// 0600 (O_CREAT mode + Fchmod) still gives owner-only access to
	// the new file regardless of how loose the parent dir mode is.
	if !skipParentGate {
		if err := verifyPosixParentDirFromFd(dirFd); err != nil {
			return fmt.Errorf("%w (path %s): %v", ErrSecureWriteParentInsecure, parentDir, err)
		}
	}

	// 2a. Refuse to overwrite a pre-existing symlink/junction at `base`.
	// Renameat with POSIX semantics would silently replace the symlink
	// with a regular file — caller might expect the symlink target to
	// receive the write. Refuse outright so the caller is forced to
	// clean up the symlink first.
	if err := refusePreexistingSymlink(dirFd, base); err != nil {
		return fmt.Errorf("secure write: target %s: %w", path, err)
	}

	// 3. Unpredictable temp name to defeat slot-squat races.
	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		return fmt.Errorf("secure write: crypto/rand: %w", err)
	}
	tempName := fmt.Sprintf(".%s.tmp.%d.%s", base, os.Getpid(), hex.EncodeToString(randBytes))

	// 4. Create the temp file relative to dirFd with O_EXCL|O_NOFOLLOW.
	flags := unix.O_CREAT | unix.O_EXCL | unix.O_WRONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fileFd, err := unix.Openat(dirFd, tempName, flags, 0o600)
	if err != nil {
		return fmt.Errorf("secure write: openat temp %s: %w", tempName, err)
	}

	// Helper to unlink+close on any post-create failure. Errors from
	// cleanup are intentionally ignored — the caller already has a
	// failure to surface; leaving a temp file is preferable to masking
	// the original error.
	cleanup := func() {
		_ = unix.Close(fileFd)
		_ = unix.Unlinkat(dirFd, tempName, 0)
	}

	// 5. fchmod defense vs umask drift. O_CREAT mode 0600 is the
	// primary guarantee; this catches a hostile umask that widened it.
	if err := unix.Fchmod(fileFd, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("secure write: fchmod temp: %w", err)
	}

	// 6. Write contents. unix.Write may return a short write; loop
	// until everything is committed.
	if err := writeAllUnix(fileFd, contents); err != nil {
		cleanup()
		return fmt.Errorf("secure write: write temp: %w", err)
	}

	// 7. fsync the file before rename so the inode bytes are durable.
	if err := unix.Fsync(fileFd); err != nil {
		cleanup()
		return fmt.Errorf("secure write: fsync temp: %w", err)
	}

	// 8. Atomic rename relative to dirFd. POSIX renameat replaces an
	// existing destination atomically; the final-component path
	// resolution is anchored to dirFd (not a re-walk).
	if err := unix.Renameat(dirFd, tempName, dirFd, base); err != nil {
		cleanup()
		return fmt.Errorf("secure write: renameat %s -> %s: %w", tempName, base, err)
	}

	// 9. Now safe to close the file handle — rename completed.
	if err := unix.Close(fileFd); err != nil {
		return fmt.Errorf("secure write: close temp: %w", err)
	}

	// 10. Re-open destination via SAME dirFd to re-verify. Path-based
	// re-open here would be TOCTOU; the dirFd anchor closes the window.
	//
	// After step 8's atomic renameat a COMPLETE owner-only file is published
	// at `base`. Re-open can fail transiently (signal, ETXTBSY, temporary
	// access hiccup), so do not erase the published file unless we actually
	// re-open it and prove the owner/mode is wrong. A definitive verify
	// failure below still uses the dirFd-relative cleanup path.
	verifyFd, err := openPostRenameVerifyFdPosix(dirFd, base)
	if err != nil {
		return fmt.Errorf("secure write: re-open %s: %w", base, err)
	}
	defer unix.Close(verifyFd)

	// Test-only seam (secure_write_client_config.go): force a post-rename
	// verify failure so the "no file on error" cleanup path is
	// exercisable without engineering a real mode/owner mismatch on the
	// persisted inode. nil in production → the real verify below runs.
	if postRenameVerifyFailHook != nil {
		if herr := postRenameVerifyFailHook(); herr != nil {
			return postRenameFailurePosix(dirFd, base,
				fmt.Errorf("secure write: post-rename verify %s: %w", base, herr))
		}
	}

	// 11. Re-verify mode + ownership on the persisted file.
	if err := verifyPosixFileFromFd(verifyFd); err != nil {
		return postRenameFailurePosix(dirFd, base,
			fmt.Errorf("secure write: post-rename verify %s: %w", base, err))
	}
	return nil
}

func openPostRenameVerifyFdPosix(dirFd int, base string) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= postRenameOpenMaxAttempts; attempt++ {
		if postRenameOpenFailHook != nil {
			if herr := postRenameOpenFailHook(); herr != nil {
				lastErr = herr
				if !isRetryablePostRenameOpenErrPosix(herr) || attempt == postRenameOpenMaxAttempts {
					return -1, postRenameOpenErrAfterAttempts(attempt, herr)
				}
				time.Sleep(postRenameOpenRetryDelay)
				continue
			}
		}
		fd, err := unix.Openat(dirFd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == nil {
			return fd, nil
		}
		lastErr = err
		if !isRetryablePostRenameOpenErrPosix(err) || attempt == postRenameOpenMaxAttempts {
			return -1, postRenameOpenErrAfterAttempts(attempt, err)
		}
		time.Sleep(postRenameOpenRetryDelay)
	}
	return -1, lastErr
}

func postRenameOpenErrAfterAttempts(attempts int, err error) error {
	if attempts <= 1 {
		return err
	}
	return fmt.Errorf("still failing after %d attempts: %w", attempts, err)
}

func isRetryablePostRenameOpenErrPosix(err error) bool {
	return errors.Is(err, unix.EINTR) ||
		errors.Is(err, unix.ETXTBSY) ||
		errors.Is(err, unix.EBUSY) ||
		errors.Is(err, unix.EAGAIN) ||
		errors.Is(err, unix.EACCES)
}

// postRenameFailurePosix is the "no file on error" cleanup for a
// definitive post-rename verify failure (step 11). It best-effort UNLINKs
// the just-published `base` via a dirFd-relative unlinkat (anchored to the
// same dirFd the rename used — not a path re-walk that could race a swapped
// symlink). ENOENT (already gone) is success. If the unlink fails the
// published file genuinely REMAINS, so that fact is folded into the returned
// error rather than hidden. origErr is always preserved as the primary cause.
func postRenameFailurePosix(dirFd int, base string, origErr error) error {
	if derr := unix.Unlinkat(dirFd, base, 0); derr != nil && !errors.Is(derr, unix.ENOENT) {
		return fmt.Errorf("%w; AND the published file could not be removed (it may remain at the destination): %v", origErr, derr)
	}
	return origErr
}

// writeAllUnix retries unix.Write until every byte in p is committed
// or an error occurs. unix.Write may return a short count without an
// error on some kernels (large buffers / signal interruption).
func writeAllUnix(fd int, p []byte) error {
	for len(p) > 0 {
		n, err := unix.Write(fd, p)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("write returned 0 bytes")
		}
		p = p[n:]
	}
	return nil
}

// refusePreexistingSymlink rejects if `name` already exists under
// dirFd as a symlink. If `name` doesn't exist at all, returns nil
// (the write will atomically create it). If `name` exists as a
// regular file the write proceeds (atomic replace).
//
// Renameat is atomic — without this check a pre-existing symlink at
// `name` would be silently replaced by a regular file, surprising
// callers who assumed they were updating the symlink target.
func refusePreexistingSymlink(dirFd int, name string) error {
	var st unix.Stat_t
	err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		// ENOENT: name doesn't exist, atomic create+rename proceeds.
		// ANY OTHER error (EACCES, EIO, EBUSY, etc.) means we could not
		// verify the slot is safe — fail closed (codex bot r2 P2 closure
		// — earlier branch returned nil for every error, including
		// permission/I-O anomalies, which would let the write proceed
		// over an unverified slot).
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("symlink probe failed: %w", err)
	}
	// S_IFLNK (symlink) test against the file-type bits.
	if (st.Mode & unix.S_IFMT) == unix.S_IFLNK {
		return fmt.Errorf("pre-existing symlink refused")
	}
	return nil
}

// verifyPosixParentDirFromFd stats the parent dir via the open fd and
// rejects ANY group/other permission bits + non-owner uid. The
// state-dir per-user trust boundary covers the ancestor chain; this
// check only needs to confirm the immediate parent matches.
//
// Codex bot r4 P2 closure: earlier wording rejected only 0o022 (group/
// world-WRITABLE) but accepted 0o755 / 0o750 (group/world-readable +
// listable). Token-bearing state dirs MUST NOT be listable by other
// local users — even directory enumeration leaks file presence and
// timing. Windows leg already rejects any non-allowlist read access;
// POSIX is now stricter to match.
func verifyPosixParentDirFromFd(fd int) error {
	return verifyPosixOwnerAndModeFromFd(
		fd,
		func(uid, want int) error {
			return fmt.Errorf("parent owned by uid %d, want %d", uid, want)
		},
		func(mode uint32) error {
			// 0o077 = ANY group/world bit (read OR write OR execute). A
			// parent must be 0700 or 0500 (read-only owner) — nothing
			// weaker.
			return fmt.Errorf("parent mode %#o exposes bits to group/world (require 0700-equivalent)", mode)
		},
	)
}

// verifyPosixFileFromFd stats the persisted file via the verify fd and
// rejects any group/other-readable bits or non-owner uid.
func verifyPosixFileFromFd(fd int) error {
	return verifyPosixOwnerAndModeFromFd(
		fd,
		func(uid, want int) error {
			return fmt.Errorf("file owned by uid %d, want %d", uid, want)
		},
		func(mode uint32) error {
			return fmt.Errorf("file mode %#o is group- or other-accessible", mode)
		},
	)
}

// verifyPosixOwnerAndModeFromFd is the shared owner-uid + mode-bit gate
// behind verifyPosixParentDirFromFd and verifyPosixFileFromFd. Both
// the parent-dir check (step 2) and the post-rename file re-verify
// (step 11) enforce the identical rule — owner is the current uid AND
// no group/other permission bit (0o077) is set — and only diverged in
// their diagnostic wording. Unifying them keeps the single 0o077 gate
// in one place so a future tightening (e.g. a new exempt bit) cannot
// drift between the two call sites.
//
// ownerErr is invoked with (st.Uid, os.Getuid()) and modeErr with
// (mode & 0o777); each caller supplies a constant-format closure so the
// operator-facing wording is byte-identical to the pre-dedup form and
// `go vet`'s printf analyzer still sees a constant format string.
//
// unix.Stat_t.Mode is uint32 on Linux but uint16 on Darwin; widen to
// uint32 once and operate on the widened form so the mask expression
// typechecks under both build targets.
func verifyPosixOwnerAndModeFromFd(fd int, ownerErr func(uid, want int) error, modeErr func(mode uint32) error) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if int(st.Uid) != os.Getuid() {
		return ownerErr(int(st.Uid), os.Getuid())
	}
	mode := uint32(st.Mode)
	if mode&0o077 != 0 {
		return modeErr(mode & 0o777)
	}
	return nil
}

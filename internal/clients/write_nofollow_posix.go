//go:build !windows

package clients

import "syscall"

// createNoFollowFlag returns the POSIX `O_NOFOLLOW` open flag so
// EnsureClientConfigStub's `O_CREAT|O_EXCL` open atomically refuses
// to follow symlinks at the kernel level (PR #208 deep-sec Lane C
// defense). When the destination is a symlink, the open syscall
// returns `ELOOP`; the caller's `os.IsExist` branch catches the
// `EEXIST` from `O_EXCL` and treats it as idempotent success — the
// `ELOOP` from `O_NOFOLLOW` is propagated upward so the operator
// sees an explicit failure rather than silently writing through.
func createNoFollowFlag() int {
	return syscall.O_NOFOLLOW
}

//go:build windows

package clients

// createNoFollowFlag returns 0 on Windows because the Go runtime's
// syscall package does not expose a portable equivalent of POSIX
// `O_NOFOLLOW` for `os.OpenFile`. The kernel-level symlink-refusal
// guarantee on Windows requires going through `NtCreateFile` with
// `FILE_FLAG_OPEN_REPARSE_POINT`, which is tracked for v0.4.6+ as a
// dedicated `SecureCreateClientConfigIfMissing` helper.
//
// For v0.4.5, EnsureClientConfigStub uses the Lstat pre-check (in
// write.go) to refuse pre-existing symlinks; the residual window
// between Lstat and CreateFileW is microseconds wide and bounded in
// impact (an attacker who wins the race causes the stub bytes to be
// written through their planted symlink — the same blast radius as
// any user-space file write the attacker could already perform with
// the parent-dir write permissions they had to have to plant the
// symlink in the first place).
func createNoFollowFlag() int {
	return 0
}

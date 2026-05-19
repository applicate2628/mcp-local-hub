// read_hardening.go — TEMPORARY STUB for Task 2.2. Task 2.4 of the
// v0.5.x Servers matrix revamp replaces this file with the
// platform-specific hardened-open implementation (Windows:
// FILE_FLAG_OPEN_REPARSE_POINT refusal; POSIX: O_NOFOLLOW + regular-
// file pre-check). The signature stays the same so callers in
// overlay.go do not change between Task 2.2 and Task 2.4.

package daemon_env_overlay

import "os"

func hardenedOpen(path string) (*os.File, error) {
	return os.Open(path)
}

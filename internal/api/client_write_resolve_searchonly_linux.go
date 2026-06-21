//go:build linux

// client_write_resolve_searchonly_linux.go — Linux search-only open flags
// for the resolved-symlink intermediate-component descent.
//
// Finding 2 (read-gating ancestors): the F1 component descent opened every
// intermediate with O_RDONLY (openExistingRealDirAt), which requires
// directory READ permission. Ordinary path traversal — and the old single
// parent open — only needed SEARCH/EXECUTE. So a resolved target below an
// execute-only ancestor (0711/0111 dir, writable final parent) is reachable
// by path but failed EACCES BEFORE the parent opened, breaking legitimate
// opted-in symlink configs.
//
// On Linux O_PATH opens a directory for *at()-relative use only — it needs
// SEARCH/EXECUTE on the directory, NOT READ. An O_PATH fd works as the dirfd
// for openat/renameat (kernel >= 3.6), so it is a valid anchor for the next
// descent step. We use it ONLY for the symlink-resolve intermediate ancestors;
// G17's mkdirOrOpenRealDirAt keeps its normal read fd (its DACL/mode verify
// needs it) and the final parent is still opened with the normal read fd.
//
// O_NOFOLLOW + O_DIRECTORY are still applied so a swapped intermediate symlink
// is refused (ELOOP) and a non-dir fails (ENOTDIR) — the F1 TOCTOU closure is
// unchanged; only the READ requirement on ancestors is dropped.

package api

import "golang.org/x/sys/unix"

// searchOnlyDirOpenFlags returns the open() flags for an intermediate
// ancestor directory in the resolved-symlink descent: search-only
// (O_PATH) so an execute-only ancestor is traversable without READ, while
// O_NOFOLLOW|O_DIRECTORY keep the symlink-swap refusal.
func searchOnlyDirOpenFlags() int {
	return unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
}

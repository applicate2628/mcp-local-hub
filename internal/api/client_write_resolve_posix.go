//go:build !windows

// client_write_resolve_posix.go — POSIX leg of the symlink resolver
// used by the secure-write relax lane. filepath.EvalSymlinks works
// uniformly on POSIX (no subst/junction abstraction to traverse).

package api

import "path/filepath"

func resolveSymlinkFinalPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

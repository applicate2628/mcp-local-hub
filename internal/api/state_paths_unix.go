//go:build !windows

// state_paths_unix.go — POSIX glue for the state path resolver.
//
// On Linux/macOS the Windows KnownFolder API does not exist, so we
// install a stub resolver that always returns
// errKnownFolderUnavailable. daemonStateDir on POSIX never reaches
// the resolver (the GOOS branch picks posixStateDir instead), so the
// stub exists purely as a safety net for cross-platform-misuse.
//
// statUID extracts the numeric UID from syscall.Stat_t (the type Go's
// fs.FileInfo.Sys() returns on POSIX). The helper returns (uid, true)
// so posixDirSanityCheck can compare against os.Getuid().

package api

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func init() {
	knownFolderResolverFn = stubKnownFolderUnsupported
}

// stubKnownFolderUnsupported is the POSIX resolver-stub. Production
// callers never invoke it (daemonStateDir routes to posixStateDir on
// POSIX) but defensive code paths can detect a mis-built binary
// trying to call into the Windows path.
func stubKnownFolderUnsupported() (string, error) {
	return "", fmt.Errorf("KnownFolder API is Windows-only: %w", errKnownFolderUnavailable)
}

// statUID extracts the POSIX UID from a stat result. Returns
// (0, false) when the underlying Sys() value is not a *syscall.Stat_t
// — keeps the helper portable across exotic POSIX layers (FUSE, Plan9
// emulation) where the Sys() shape is empty.
func statUID(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// ensureStateRoot creates `root` (and intermediate parents) at 0700
// and runs an explicit Chmod (defense vs umask drift) before
// returning. POSIX-only — Windows uses the simpler MkdirAll path in
// state_paths_windows.go.
func ensureStateRoot(root string) (string, error) {
	if root == "" {
		return "", errKnownFolderUnavailable
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("mkdir state root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("chmod state root: %w", err)
	}
	return root, nil
}

// joinStateRoot composes the per-user state directory path. On POSIX
// daemonStateDir never invokes this helper (posixStateDir owns the
// composition); the stub exists so state_paths_prod.go and
// state_paths_envfallback.go compile on every GOOS without
// per-platform branches in their bodies.
//
//nolint:unused // referenced by daemonStateDir on Windows builds; POSIX call sites bypass it.
func joinStateRoot(parent string) string {
	return parent + string(os.PathSeparator) + stateDirName
}

// joinKnownFolderErr is a no-op wrapper on POSIX (the Windows path is
// the only caller that ever wraps a resolver error). Defined here so
// state_paths_prod.go does not need a build-tagged helper.
//
//nolint:unused // referenced from state_paths_prod.go on Windows builds; POSIX code paths bypass it.
func joinKnownFolderErr(cause error) error {
	if cause == nil {
		return errKnownFolderUnavailable
	}
	return fmt.Errorf("%w: %v", errKnownFolderUnavailable, cause)
}

// Compile-time sanity: errors.Is must continue to match the wrapped
// sentinel. This guard fires the same way as the Windows variant.
var _ = errors.Is(joinKnownFolderErr(errors.New("x")), errKnownFolderUnavailable)

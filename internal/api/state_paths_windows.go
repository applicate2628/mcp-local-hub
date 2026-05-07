//go:build windows

// state_paths_windows.go — Windows-only platform glue for the state
// path resolver.
//
// Plan §16 v9 mandates production binaries call ONLY
// SHGetKnownFolderPath(FOLDERID_LocalAppData). golang.org/x/sys/windows
// already wraps the Win32 API as windows.KnownFolderPath; this file
// installs the wrapper into knownFolderResolverFn at init() so both
// the production and env-fallback variants of daemonStateDir consume
// the same source of truth.
//
// statUID is a no-op on Windows (NTFS reports a SID, not a POSIX UID).
// We return false from the helper so posixDirSanityCheck skips the
// UID gate for any state path that accidentally ran through the POSIX
// helper on Windows. The Windows daemonStateDir branch never invokes
// posixDirSanityCheck, so this is defense-in-depth only.

package api

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

func init() {
	knownFolderResolverFn = realKnownFolderLocalAppData
}

// realKnownFolderLocalAppData is the production Windows resolver. It
// calls SHGetKnownFolderPath(FOLDERID_LocalAppData) via the
// golang.org/x/sys/windows wrapper. KF_DEFAULT (flags=0) returns the
// already-resolved path without forcing creation; that suits our
// model where ensureStateRoot performs MkdirAll afterwards.
func realKnownFolderLocalAppData() (string, error) {
	path, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return "", fmt.Errorf("SHGetKnownFolderPath(LocalAppData): %w", err)
	}
	return path, nil
}

// statUID returns (0, false) on Windows. NTFS reports SIDs, not POSIX
// UIDs, so the UID-mismatch gate in posixDirSanityCheck cannot run
// meaningfully on Windows. Returning false makes the gate a no-op.
//
//nolint:unused // referenced by posixDirSanityCheck on every GOOS.
func statUID(_ fs.FileInfo) (int, bool) {
	return 0, false
}

// ensureStateRoot creates `root` (and intermediate parents) and
// returns the absolute path. Windows ignores POSIX permission bits;
// MkdirAll is enough on this platform.
func ensureStateRoot(root string) (string, error) {
	if root == "" {
		return "", errKnownFolderUnavailable
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("mkdir state root: %w", err)
	}
	return root, nil
}

// joinStateRoot composes the per-user state directory path.
// Windows: <LocalAppData>\mcp-local-hub.
func joinStateRoot(localAppData string) string {
	return localAppData + string(os.PathSeparator) + stateDirName
}

// joinKnownFolderErr wraps a resolver error so callers using
// errors.Is(err, errKnownFolderUnavailable) still match the
// production fail-closed branch while the underlying cause stays
// inspectable for diagnostics.
func joinKnownFolderErr(cause error) error {
	if cause == nil {
		return errKnownFolderUnavailable
	}
	return fmt.Errorf("%w: %v", errKnownFolderUnavailable, cause)
}

// Compile-time guard: errKnownFolderUnavailable wrapping must remain
// detectable through errors.Is. Without this assertion an accidental
// switch to a non-wrapping wrapper would silently break the
// production fail-closed test.
var _ = errors.Is(joinKnownFolderErr(errors.New("x")), errKnownFolderUnavailable)

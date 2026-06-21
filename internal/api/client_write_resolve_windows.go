//go:build windows

// client_write_resolve_windows.go — Windows-specific symlink target
// resolver used by the secure-write relax lane to follow dotfile
// symlinks (e.g. ~/.codex/config.toml ->
// E:\env\Agents\.codex\config.toml).
//
// filepath.EvalSymlinks on Go 1.x fails when the symlink target sits
// behind a substed drive letter (subst.exe / DefineDosDevice) — a
// common pattern when users mount OneDrive-synced dirs as E:\ or
// similar. GetFinalPathNameByHandle walks through subst + junction +
// nested symlinks reliably and returns the canonical disk path.
//
// The function strips the `\\?\` long-path prefix that
// GetFinalPathNameByHandle prepends so the returned path slots back
// into the rest of the secure-write pipeline (which operates on
// standard drive-letter paths).

package api

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// secureWriteThroughResolvedParentHandle is the Windows handle-pinned
// write-through for the symlink-resolve relax lane (env-var F2 and scoped
// consent). It is the AF-1 fix: instead of returning a resolved path STRING
// for SecureWriteClientConfig to re-walk (filepath.Split + re-open the
// parent BY PATH), it OPENS the resolved target's parent directory ONCE
// (openDirHandleNoReparse — frozen at open) and runs the shared hardened
// owner (secureWriteClientConfigToResolvedParent) against that held handle.
// A co-resident who swaps the symlink, or any intermediate component,
// BETWEEN resolve and this open cannot redirect the privileged write
// because the parent handle is pinned — there is no second path-based open
// for the swap to race.
//
// resolvedTarget MUST already be the fully-resolved final disk path (from
// resolveSymlinkFinalPath). It returns the resolved-parent directory path
// that was actually opened (filepath.Dir of resolvedTarget, cleaned) so the
// caller can pin-match it against a scoped consent's PinnedResolvedPath
// (SEAM-B swap-between-confirm-and-write guard).
//
// skipParentGate threads through to the shared owner exactly as the
// path-based lane (the symlink relax lane is itself a relax-on-broadened
// posture; the per-file restrictive DACL is the load-bearing boundary).
func secureWriteThroughResolvedParentHandle(resolvedTarget string, contents []byte, skipParentGate bool) (string, error) {
	parentDir, base := filepath.Split(resolvedTarget)
	if parentDir == "" {
		parentDir = "."
	}
	if base == "" {
		return "", fmt.Errorf("secure write: empty base name in resolved target %q", resolvedTarget)
	}
	parentDir = filepath.Clean(parentDir)

	dirHandle, err := openDirHandleNoReparse(parentDir)
	if err != nil {
		return parentDir, fmt.Errorf("secure write: open resolved parent %s: %w", parentDir, err)
	}
	defer windows.CloseHandle(dirHandle)

	if err := secureWriteClientConfigToResolvedParent(dirHandle, parentDir, base, contents, skipParentGate); err != nil {
		return parentDir, err
	}
	return parentDir, nil
}

// resolveSymlinkFinalPath opens path through any reparse points
// (CreateFile WITHOUT FILE_FLAG_OPEN_REPARSE_POINT) and returns the
// fully resolved final disk path via GetFinalPathNameByHandle. The
// long-path prefix `\\?\` is stripped so the result is a normal
// drive-letter path.
func resolveSymlinkFinalPath(path string) (string, error) {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", fmt.Errorf("utf16 path: %w", err)
	}
	h, err := windows.CreateFile(
		pathW,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		// NO FILE_FLAG_OPEN_REPARSE_POINT — follow the symlink.
		// FILE_FLAG_BACKUP_SEMANTICS lets the open succeed on both
		// regular files and dirs (forward-compat; we expect a
		// file here).
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("open follow %s: %w", path, err)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", fmt.Errorf("final path %s: %w", path, err)
	}
	if int(n) > len(buf) {
		// Buffer too small — grow and retry once.
		buf = make([]uint16, n+1)
		n, err = windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", fmt.Errorf("final path retry %s: %w", path, err)
		}
	}
	resolved := windows.UTF16ToString(buf[:n])
	// GetFinalPathNameByHandle returns paths prefixed with the
	// `\\?\` long-path namespace. Two shapes need separate
	// handling (codex bot r1 P2 on PR #192):
	//
	//   1. Drive-letter form: `\\?\C:\Users\u\foo.toml`
	//      → strip `\\?\` to get `C:\Users\u\foo.toml`
	//   2. UNC form (network share): `\\?\UNC\server\share\path`
	//      → strip `\\?\UNC\` AND re-add `\\` to get the
	//      canonical UNC form `\\server\share\path`. Without
	//      this branch the path becomes the relative-looking
	//      `UNC\server\share\path` and the downstream
	//      CreateFile would interpret it as a current-dir
	//      relative path — breaking symlinked configs stored
	//      on network shares.
	if rest, ok := strings.CutPrefix(resolved, `\\?\UNC\`); ok {
		return `\\` + rest, nil
	}
	resolved = strings.TrimPrefix(resolved, `\\?\`)
	return resolved, nil
}

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
	"strings"

	"golang.org/x/sys/windows"
)

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
	// `\\?\` long-path namespace. The rest of the secure-write
	// pipeline (CreateFile with regular drive letters, atomic
	// rename, etc.) handles those fine on modern Windows but
	// stripping the prefix keeps audit-log paths and error
	// messages readable.
	resolved = strings.TrimPrefix(resolved, `\\?\`)
	return resolved, nil
}

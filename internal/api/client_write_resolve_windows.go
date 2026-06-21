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
// parent BY PATH), it descends to the resolved target's parent
// COMPONENT-BY-COMPONENT, refusing reparse-follow at every step, anchored at
// the VOLUME ROOT of the resolved target, and runs the shared hardened owner
// (secureWriteClientConfigToResolvedParent) against the final held handle.
//
// AF-1 F1 — why component-by-component (not one path-based open of the whole
// parent string): FILE_FLAG_OPEN_REPARSE_POINT on a single open of the full
// parent path protects ONLY the final component; the object manager re-walks
// every INTERMEDIATE component at open time, so an intermediate dir swapped
// to a junction/symlink between resolve and this open would redirect the
// handle. Opening one real component at a time relative to the previously-
// held handle, refusing reparse-follow each step, never follows a swapped
// intermediate.
//
// Trust anchor = the volume root of the RESOLVED target, NOT %USERPROFILE%.
// The resolved target is frequently OUTSIDE home by design (the motivating
// case ~/.codex/config.toml → E:\env\Agents\.codex\config.toml). The anchor
// is the drive root (filepath.VolumeName -> "C:" -> "C:\") or, for a UNC
// path, the share root (\\server\share). There is NO path-containment
// refusal: the operator already consented to THIS resolved target, the chain
// already exists (nothing is created), and the ONLY property delivered is
// "no intermediate component is followed through a swap."
//
// resolvedTarget MUST already be the fully-resolved final disk path (from
// resolveSymlinkFinalPath). It returns the resolved-parent directory path
// (filepath.Dir of resolvedTarget, cleaned) so the caller can pin-match it
// against a scoped consent's PinnedResolvedPath (SEAM-B
// swap-between-confirm-and-write guard).
//
// skipParentGate threads through to the shared owner exactly as the
// path-based lane (the symlink relax lane is itself a relax-on-broadened
// posture; the per-file restrictive DACL is the load-bearing boundary).
func secureWriteThroughResolvedParentHandle(resolvedTarget string, contents []byte, skipParentGate bool) (string, error) {
	parentDir, base := filepath.Split(resolvedTarget)
	if base == "" {
		return "", fmt.Errorf("secure write: empty base name in resolved target %q", resolvedTarget)
	}
	if parentDir == "" {
		parentDir = "."
	}
	parentDir = filepath.Clean(parentDir)

	dirHandle, err := openResolvedParentByComponentDescentWindows(resolvedTarget)
	if err != nil {
		return parentDir, err
	}
	defer windows.CloseHandle(dirHandle)

	if err := secureWriteClientConfigToResolvedParent(dirHandle, parentDir, base, contents, skipParentGate); err != nil {
		return parentDir, err
	}
	return parentDir, nil
}

// openResolvedParentByComponentDescentWindows opens the PARENT directory of
// `resolvedTarget` by descending from the resolved target's volume root
// (drive root "C:\" or UNC share root \\server\share) component-by-component,
// refusing reparse-follow at every step via openExistingRealDirAt (the step
// shared with G17's mkdirOrVerifyRealDirWindows). The returned handle is the
// caller's pinned parent; the caller owns its close.
//
// The base name is dropped — only the intermediate directory components are
// descended; the final component is published by the shared owner's
// handle-relative atomic rename, not opened here.
func openResolvedParentByComponentDescentWindows(resolvedTarget string) (windows.Handle, error) {
	cleaned := filepath.Clean(resolvedTarget)

	vol, dirComponents, err := decomposeResolvedParentWindows(cleaned)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("secure write: decompose resolved target %q: %w", resolvedTarget, err)
	}

	// Volume-root anchor: the drive root "C:\" or the UNC share root
	// "\\server\share". openDirHandleNoReparse opens with
	// FILE_FLAG_OPEN_REPARSE_POINT (so a reparse-point root would be opened
	// as the link itself rather than followed); a drive/share root is never
	// itself a reparse point.
	anchorPath := vol + `\`
	anchorHandle, err := openDirHandleNoReparse(anchorPath)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("secure write: open volume-root anchor %s: %w", anchorPath, err)
	}

	curHandle := anchorHandle
	for _, comp := range dirComponents {
		nextHandle, openErr := openExistingRealDirAt(curHandle, comp)
		_ = windows.CloseHandle(curHandle)
		if openErr != nil {
			return windows.InvalidHandle, fmt.Errorf(
				"secure write: refuse to descend through reparse point / non-directory at component %q of resolved target %s: %w",
				comp, resolvedTarget, openErr,
			)
		}
		curHandle = nextHandle
		// Test-only injection seam: fires AFTER opening this component and
		// BEFORE opening the next. nil in production. (Windows symlink
		// creation needs elevation, so the swap test runs on POSIX; this
		// seam is here for symmetry and for any future elevated test.)
		if resolvedParentDescendStepHook != nil {
			resolvedParentDescendStepHook(comp)
		}
	}
	return curHandle, nil
}

// decomposeResolvedParentWindows splits a cleaned absolute Windows path into
// its volume name (drive "C:" or UNC share "\\server\share") and the list of
// INTERMEDIATE directory components between the volume root and the final
// base name (the base is dropped). Used by the AF-1 F1 component-by-component
// descent. Unit-tested directly (no symlink elevation needed for the
// decomposition logic).
func decomposeResolvedParentWindows(cleaned string) (vol string, dirComponents []string, err error) {
	vol = filepath.VolumeName(cleaned)
	if vol == "" {
		return "", nil, fmt.Errorf("path %q has no volume name (not an absolute drive/UNC path)", cleaned)
	}
	// Everything after the volume name; strip a single leading separator so
	// the split yields clean component names.
	rest := strings.TrimPrefix(cleaned, vol)
	rest = strings.TrimPrefix(rest, `\`)
	rest = strings.TrimPrefix(rest, `/`)
	if rest == "" {
		// The resolved target was the volume root itself with no base —
		// pathological for a file write; the caller's empty-base check
		// already rejects it, but guard here too.
		return vol, nil, nil
	}
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' })
	if len(parts) == 0 {
		return vol, nil, nil
	}
	// Drop the final component (the base name); descend only its ancestors.
	dirComponents = parts[:len(parts)-1]
	return vol, dirComponents, nil
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

//go:build windows

// binary_rename_aside_windows.go — Windows-specific rename-aside binary
// replacement per spec §"Windows binary replacement (rename-aside)".
//
// Background: Windows refuses `MoveFileExW(newSrc, target,
// REPLACE_EXISTING)` over a running executable image with
// ERROR_ACCESS_DENIED — the image loader holds the file with
// FILE_SHARE_DELETE but not FILE_SHARE_WRITE, so REPLACE_EXISTING
// (which conceptually opens the target for delete-and-replace) fails.
//
// The rename-aside pattern dodges this: rename the running binary
// `target → target+".old-<ts>"` first (Windows allows rename of a
// running image because FILE_SHARE_DELETE is granted), THEN rename
// the staged `newSrc → target`. Step 1 is REPLACE_EXISTING because a
// stale `.old-<ts>` from a previous failed upgrade could collide
// (different ts though — practically impossible); we pass the flag
// for symmetry / defensive correctness, not because we expect it
// to fire. Step 2 uses flags=0 because the target was just moved
// aside; if it still existed for any reason, we want a hard error.
//
// The aside `.old-<ts>` file stays on disk until the running process
// exits (Windows can't delete a still-mapped image). SweepOldBinaries
// handles cleanup on the next supervisor startup / upgrade.

package api

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// renameAsideTimestampLayout is a Windows-filename-safe timestamp
// suffix layout (no colons, no fractional seconds): "20060102T150405Z".
// Sortable lexicographically; sufficient resolution for sweep selection
// (the encoded suffix timestamp is the sweep age authority).
const renameAsideTimestampLayout = "20060102T150405Z"

// RenameAsideReplace performs the Windows rename-aside two-step binary
// replacement:
//
//  1. MoveFileExW(target → target+".old-<ts>", REPLACE_EXISTING).
//  2. MoveFileExW(newSrc → target, 0).
//
// On step-2 failure the function attempts a best-effort rollback of
// step 1 so the prior running binary path is restored.
//
// Caller responsibilities:
//   - newSrc must already exist (caller stages it via the secure-write
//     pipeline at e.g. `<install-dir>/mcphub.exe.new`).
//   - target may be the running executable; Windows allows the rename
//     because the image was opened with FILE_SHARE_DELETE.
func RenameAsideReplace(target, newSrc string) error {
	ts := time.Now().UTC().Format(renameAsideTimestampLayout)
	oldPath := target + ".old-" + ts

	targetW, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("utf16 target: %w", err)
	}
	oldW, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return fmt.Errorf("utf16 old: %w", err)
	}
	newSrcW, err := windows.UTF16PtrFromString(newSrc)
	if err != nil {
		return fmt.Errorf("utf16 newSrc: %w", err)
	}

	// Step 1: move running binary out of the way.
	if err := windows.MoveFileEx(targetW, oldW, windows.MOVEFILE_REPLACE_EXISTING); err != nil {
		return fmt.Errorf("MoveFileEx target→old (%s → %s): %w", target, oldPath, err)
	}
	// Step 2: move new binary into place.
	if err := windows.MoveFileEx(newSrcW, targetW, 0); err != nil {
		// Best-effort rollback: restore the old binary to the target slot
		// so the caller is not left with no binary on disk.
		_ = windows.MoveFileEx(oldW, targetW, windows.MOVEFILE_REPLACE_EXISTING)
		return fmt.Errorf("MoveFileEx newSrc→target (%s → %s): %w", newSrc, target, err)
	}
	return nil
}

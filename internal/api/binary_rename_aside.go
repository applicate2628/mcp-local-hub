// binary_rename_aside.go — cross-platform `.old-<ts>` sweep used by
// the rename-aside upgrade pipeline.
//
// On Windows the prior running binary can't be deleted while still
// mapped; it lingers until the running supervisor exits. On POSIX
// the `.old-<ts>` aside is created for symmetry (see
// binary_rename_aside_posix.go). Either way, the sweep removes
// aged-out files on every `mcphub install --upgrade` and every
// `mcphub supervise` startup so accumulation stays bounded.
//
// Spec §"Cold-restart upgrade flow (detail)" / §"Windows binary
// replacement (rename-aside)" §step 4:
//   "glob <install-dir>/mcphub.exe.old-* and os.Remove each whose
//    name parses as .old-<RFC3339> form AND whose mtime is older
//    than 7 days. os.Remove failures (file still mapped, AV scan,
//    ACL flip) logged warn + retried on next pass."

package api

import (
	"os"
	"path/filepath"
	"time"
)

// renameAsideRetention is the maximum age of `.old-<ts>` aside files;
// older entries are eligible for removal by SweepOldBinaries.
const renameAsideRetention = 7 * 24 * time.Hour

// SweepOldBinaries removes `<dir>/mcphub*.old-*` files whose mtime is
// older than 7 days. Both Windows and POSIX patterns are checked on
// every host so a tree that was cross-installed (rare; defensive) is
// still pruned.
//
// Per-file Remove failures are non-fatal — a still-mapped image, an
// in-progress AV scan, or an ACL flip can transiently block the
// delete. The caller (supervisor startup, install --upgrade) is
// expected to log warn and retry on the next pass.
//
// Returns a non-nil error only if filepath.Glob itself fails (a
// programming error in the pattern, not a runtime condition).
func SweepOldBinaries(dir string) error {
	patterns := []string{
		filepath.Join(dir, "mcphub.exe.old-*"),
		filepath.Join(dir, "mcphub.old-*"),
	}
	cutoff := time.Now().Add(-renameAsideRetention)
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil {
				// Race: file vanished between Glob and Stat, or perms flip.
				// Best-effort: skip; next pass will pick it up if still
				// present.
				continue
			}
			if info.Mode().IsDir() {
				// Defensive: a directory named like the glob pattern
				// should not exist, but never recursively delete one if
				// it does. Skip.
				continue
			}
			if info.ModTime().Before(cutoff) {
				_ = os.Remove(m) // best-effort per spec
			}
		}
	}
	return nil
}

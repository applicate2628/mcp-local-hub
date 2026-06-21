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
//    name parses as .old-<RFC3339> form AND whose encoded timestamp
//    is older than 7 days. os.Remove failures (file still mapped, AV scan,
//    ACL flip) logged warn + retried on next pass."

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// renameAsideRetention is the maximum age of `.old-<ts>` aside files;
// older entries are eligible for removal by SweepOldBinaries.
const renameAsideRetention = 7 * 24 * time.Hour

// renameAsideMaxKeep bounds the COUNT of `.old-<ts>` aside files retained
// regardless of age. The 7-day age rule alone never prunes a burst of
// same-day upgrades — every aside is younger than the cutoff — so iterative
// deploys accumulate unbounded (observed: 49 asides / ~1 GB after a single day
// of dev redeploys). The newest renameAsideMaxKeep asides are kept as a
// rollback chain; any beyond that are removed even when younger than the age
// cutoff. A lone aged-out aside is still removed by the age rule (matching the
// prior semantics — no unconditional keep-newest), so this only ADDS pruning.
const renameAsideMaxKeep = 5

// SweepOldBinaries removes `<dir>/mcphub*.old-*` aside files that are EITHER
// older than renameAsideRetention (7 days) OR beyond the newest
// renameAsideMaxKeep (the rollback chain). Both Windows and POSIX patterns are
// checked on every host so a tree that was cross-installed (rare; defensive) is
// still pruned.
//
// Per-file Remove failures are non-fatal — a still-mapped image, an
// in-progress AV scan, or an ACL flip can transiently block the
// delete. The caller (supervisor startup, install --upgrade) is
// expected to log warn and retry on the next pass.
//
// Returns a non-nil error only if filepath.Glob itself fails (a
// programming error in the pattern, not a runtime condition). Optional warn
// callbacks receive per-file remove failures; those failures remain non-fatal.
func SweepOldBinaries(dir string, warn ...func(string, error)) error {
	patterns := []string{
		filepath.Join(dir, "mcphub.exe.old-*"),
		filepath.Join(dir, "mcphub.old-*"),
	}
	// Collect every aside (across both patterns) with its encoded timestamp so the count
	// cap can rank them newest-first independent of which pattern matched.
	type aside struct {
		path      string
		createdAt time.Time
	}
	var asides []aside
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		for _, m := range matches {
			createdAt, ok := generatedRenameAsideTime(m)
			if !ok {
				continue
			}
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
			asides = append(asides, aside{path: m, createdAt: createdAt})
		}
	}
	// Newest first, so indices >= renameAsideMaxKeep are the surplus to trim.
	sort.Slice(asides, func(i, j int) bool {
		return asides[i].createdAt.After(asides[j].createdAt)
	})
	cutoff := time.Now().Add(-renameAsideRetention)
	for i, a := range asides {
		if a.createdAt.Before(cutoff) || i >= renameAsideMaxKeep {
			if err := os.Remove(a.path); err != nil {
				for _, fn := range warn {
					if fn != nil {
						fn(a.path, err)
					}
				}
			}
		}
	}
	return nil
}

// RecoverMissingBinary restores target from the newest valid
// `<target>.old-<timestamp>` aside when a crash/power loss happened after the
// rename-aside step moved target out of the way but before `<target>.new` was
// promoted. Existing targets are never overwritten, and staged `.new` files are
// intentionally ignored because they have not completed the upgrade pipeline.
func RecoverMissingBinary(target string) error {
	if target == "" {
		return fmt.Errorf("recover missing binary: empty target")
	}
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat target %q: %w", target, err)
	}

	dir := filepath.Dir(target)
	basePrefix := filepath.Base(target) + ".old-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("scan rename-aside directory %q: %w", dir, err)
	}
	type candidate struct {
		path       string
		mtime      time.Time
		suffixTime time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, basePrefix) {
			continue
		}
		suffixTime, ok := parseRenameAsideTimestamp(strings.TrimPrefix(name, basePrefix))
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.IsDir() {
			continue
		}
		candidates = append(candidates, candidate{
			path:       filepath.Join(dir, name),
			mtime:      info.ModTime(),
			suffixTime: suffixTime,
		})
	}
	if len(candidates) == 0 {
		return fmt.Errorf("recover missing binary %q: target is missing and no valid %s<timestamp> aside exists", target, basePrefix)
	}
	// Rank by the encoded suffix timestamp FIRST (mtime is only the tiebreaker):
	// the suffix encodes the rename-aside MOMENT (monotonic across upgrades), so
	// the newest suffix is the binary that was live just before the interrupted
	// swap — exactly what crash-recovery must restore. mtime is the binary's
	// build/install time, which can be non-monotonic vs the aside moment (e.g.
	// rollback-then-reupgrade). This also aligns the recovery-selection authority
	// with SweepOldBinaries, which ages purely by the suffix timestamp.
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].suffixTime.Equal(candidates[j].suffixTime) {
			return candidates[i].suffixTime.After(candidates[j].suffixTime)
		}
		if !candidates[i].mtime.Equal(candidates[j].mtime) {
			return candidates[i].mtime.After(candidates[j].mtime)
		}
		return candidates[i].path > candidates[j].path
	})
	if err := restoreMissingBinaryAside(candidates[0].path, target); err != nil {
		return fmt.Errorf("restore missing binary from aside (%s → %s): %w", candidates[0].path, target, err)
	}
	return nil
}

// CanonicalTargetFromAside returns the canonical install target for a generated
// `<target>.old-<timestamp>` rename-aside path.
func CanonicalTargetFromAside(path string) (string, bool) {
	if _, ok := generatedRenameAsideTime(path); !ok {
		return "", false
	}
	base := filepath.Base(path)
	idx := strings.LastIndex(base, ".old-")
	if idx <= 0 {
		return "", false
	}
	return filepath.Join(filepath.Dir(path), base[:idx]), true
}

func generatedRenameAsideTime(path string) (time.Time, bool) {
	base := filepath.Base(path)
	for _, prefix := range []string{"mcphub.exe.old-", "mcphub.old-"} {
		if !strings.HasPrefix(base, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(base, prefix)
		return parseRenameAsideTimestamp(suffix)
	}
	return time.Time{}, false
}

func parseRenameAsideTimestamp(suffix string) (time.Time, bool) {
	for _, layout := range []string{renameAsideTimestampLayout, time.RFC3339Nano, time.RFC3339} {
		ts, err := time.Parse(layout, suffix)
		if err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

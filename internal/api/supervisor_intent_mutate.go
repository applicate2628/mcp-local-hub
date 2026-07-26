package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
)

// MutateSupervisorIntentIfChanged serializes a supervisor-intent.json
// read-modify-write under the same path+".lock" flock used by
// WriteSupervisorIntent's final write. The mutate callback runs while the
// flock is held and must only edit the in-memory file.
//
// This is a thin, backward-compatible wrapper over
// MutateSupervisorIntentIfChangedReturning for the two existing callers
// (internal/cli/strict_mode.go, internal/api/reallocate_dynamic_pool.go)
// that only need the error, not the observed generation — it discards the
// returned snapshot rather than duplicating the read-modify-write body.
func MutateSupervisorIntentIfChanged(path string, mutate func(*SupervisorIntentFile) (bool, error)) error {
	_, err := MutateSupervisorIntentIfChangedReturning(path, mutate)
	return err
}

// MutateSupervisorIntentResult is the observed outcome of a locked
// supervisor-intent read-modify-write.
type MutateSupervisorIntentResult struct {
	// Intent is the EXACT generation this call observed under the flock: a
	// fresh disk read (never the caller's own pre-lock snapshot) with the
	// mutate callback's edits applied. Populated whether or not Changed is
	// true, because the fresh read itself can already differ from
	// whatever the caller loaded before acquiring the lock (a concurrent
	// writer may have committed something else in between) — a caller that
	// must converge with disk (see MutateSupervisorIntentIfChangedReturning's
	// doc comment) needs this snapshot even on a no-op mutation.
	Intent *SupervisorIntentFile
	// Changed reports whether the mutate callback reported a change (and
	// therefore whether a write actually happened).
	Changed bool
}

// MutateSupervisorIntentIfChangedReturning is
// MutateSupervisorIntentIfChanged, additionally returning the exact
// generation observed/committed under the flock.
//
// Why this exists (architecture-adversarial-reverify.md finding A3,
// work-items/active/2026-07-25-mcp-front-daemon/): a caller that reapplies
// its own mutate callback to its OWN pre-lock in-memory copy after this
// function reports success — instead of adopting the snapshot this function
// actually observed under the lock — can silently diverge from disk. If a
// second writer committed a change between the caller's initial load and
// this function's lock acquisition, that concurrent change lands on disk
// (this function's own read is fresh, under the lock) but is invisible to
// the caller's stale copy. QA proved this deterministically for
// internal/cli's supervisor-intent startup seeder: the disk generation
// carried an extra daemon, an extra stop, and StrictMode=true, while the
// seeder's reapply-to-stale-copy return value carried none of them.
//
// The fix: a caller with this requirement must call THIS entry point and
// replace its own local pointer with the returned Intent wholesale, rather
// than reapplying its callback to its own copy. MutateSupervisorIntentIfChanged
// stays the right choice for a caller that only needs the write to happen and
// does not hold, or does not care about, its own outdated snapshot afterward.
func MutateSupervisorIntentIfChangedReturning(path string, mutate func(*SupervisorIntentFile) (bool, error)) (MutateSupervisorIntentResult, error) {
	if strings.TrimSpace(path) == "" {
		return MutateSupervisorIntentResult{}, fmt.Errorf("empty supervisor intent path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return MutateSupervisorIntentResult{}, fmt.Errorf("mkdir supervisor intent dir: %w", err)
	}

	lockPath := path + supervisorIntentLockSuffix
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return MutateSupervisorIntentResult{}, fmt.Errorf("supervisor-intent flock %s: %w", lockPath, err)
	}
	defer func() { _ = lock.Unlock() }()

	file, _, err := readSupervisorIntentForMerge(path)
	if err != nil {
		return MutateSupervisorIntentResult{}, fmt.Errorf("read existing supervisor intent: %w", err)
	}
	file = cloneSupervisorIntentFile(file)
	changed := true
	if mutate != nil {
		changed, err = mutate(file)
		if err != nil {
			return MutateSupervisorIntentResult{}, err
		}
	}
	if !changed {
		return MutateSupervisorIntentResult{Intent: file, Changed: false}, nil
	}
	if file.Version == 0 {
		file.Version = 1
	}
	if err := writeSupervisorIntentLockHeld(path, file); err != nil {
		return MutateSupervisorIntentResult{}, err
	}
	return MutateSupervisorIntentResult{Intent: file, Changed: true}, nil
}

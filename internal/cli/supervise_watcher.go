// Package cli — Task 7.2 parent-dir intent watcher with 60s poll fallback.
//
// Spec §"Reconcile loop" + plan Task 7.2.
//
// The supervisor needs to notice when the two intent files
// (supervisor-intent.json, daemon-intent.json) under <state-dir> change
// so the reconcile loop can pick up new desired-state without operator
// intervention. Sources of change:
//
//   - `mcphub install` / manifest edits — rewrite supervisor-intent.json
//     via atomic-rename through the canonical writer.
//   - `mcphub stop` / `mcphub stop --force` — rewrite daemon-intent.json
//     via the flock+atomic-rename helper.
//   - GUI Servers matrix Apply / per-client demigrate — same canonical
//     writers as above.
//
// Per spec, fsnotify is NOT required for v0.5.0 GA: the IPC `reload`
// command (Task 6.3) is the authoritative signal — clients call it
// immediately after rewriting either intent file, so the supervisor
// reconciles within milliseconds of the write. The 60s parent-dir
// poll fallback exists only to cover edge cases where the IPC reload
// was skipped (out-of-band edit, crashed client, manual file copy).
//
// Pure polling is simpler than fsnotify on Windows (where fsnotify
// requires CGO-free `ReadDirectoryChangesW` wrapping) and on macOS
// (where kqueue file watches are notoriously chatty under high I/O).
// The 60s cadence is the spec-mandated upper bound on watch-miss
// latency; tests inject a shorter pollInterval to exercise the
// fire path in <1s wall-clock.
//
// The watcher tracks mtime changes on the two intent filenames only.
// Other files under <state-dir> (supervisor.lock, supervisor-events.log,
// supervisor-state.json) are owned by their own lifecycle paths and
// must NOT trigger a reconcile — touching them would mean a noisy
// audit-log append fires a full intent re-read every time.
//
// File-absent ↔ file-present transitions count as changes: a daemon
// added to a freshly-created supervisor-intent.json must trigger
// reconcile, and a wiped daemon-intent.json (operator manually
// removed the file to clear all stops) must also propagate. The
// detection uses a separate "tracked map" instead of just comparing
// time.Time zero values so the absent ↔ absent steady state stays
// quiet.
package cli

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// watchedIntentFiles is the canonical list of filenames the watcher
// tracks under <state-dir>. Kept package-private so the reconcile
// loop can't accidentally widen the surface — adding a new tracked
// file is a deliberate code change in this file.
//
// Phase 4-E2: daemon-intent.json is DROPPED from this list. After E2 the
// supervisor-intent.json `stops` sub-block is the SOLE stop source (the
// boot-merge deletes daemon-intent.json + its writers are gone), so a write
// that changes a stop now bumps supervisor-intent.json's mtime — already
// tracked. A perpetually-absent daemon-intent.json need not be polled.
var watchedIntentFiles = []string{
	"supervisor-intent.json",
}

// IntentWatcher polls the two intent files under <state-dir> for
// mtime changes and fires an onChange callback when either file's
// mtime (or presence) differs from the last observed snapshot.
//
// The watcher is intentionally minimal: no fsnotify, no event
// queueing, no de-duplication. The reconcile loop owns
// coalescing — if onChange is invoked twice in quick succession
// (one fs event followed by a poll tick that observed the same
// change), the reconcile pass simply runs twice with the same
// input and converges to the same final state.
//
// Construction is via NewIntentWatcher; the zero value is not
// usable because a nil onChange would silently swallow every
// detected change.
type IntentWatcher struct {
	stateDir     string
	pollInterval time.Duration
	onChange     func()

	// lastMtimes records the last observed mtime per tracked file.
	// Keys are filenames (NOT full paths); a missing key means the
	// file was absent at the last snapshot. Mutated only by Run's
	// own goroutine, so no locking is needed.
	lastMtimes map[string]time.Time

	// lastPresent records whether each tracked file was present at
	// the last snapshot. Separating presence from mtime lets the
	// detection distinguish "file deleted (was present, now absent)"
	// from "file still absent (no-op)" without conflating the two
	// against time.Time{} sentinel comparisons.
	lastPresent map[string]bool

	// lastSizes records the last observed file size (bytes) per tracked file.
	// Paired with lastMtimes so a replace-in-place write that lands within the
	// filesystem mtime-granularity window (same mtime, different content) is
	// still detected when the byte length changed — the common case for a
	// JSON intent edit (deep-review P4). Mutated only by Run's goroutine.
	lastSizes map[string]int64
}

// NewIntentWatcher returns a watcher that polls supervisor-intent.json
// and daemon-intent.json under stateDir every pollInterval. If
// pollInterval is 0 or negative, defaults to 60 seconds per spec
// §"Reconcile loop" — clamping avoids accidental zero-interval
// busy-loops if a future caller forgets to set the field.
//
// The onChange callback MUST be non-nil; nil is allowed at the type
// level only so tests can exercise the zero-value defensively, but
// Run will skip invocation when onChange is nil (the watcher then
// behaves as a pure-observation no-op, which is occasionally useful
// in integration tests that only want to assert the watcher does
// NOT panic on a missing state dir).
func NewIntentWatcher(stateDir string, pollInterval time.Duration, onChange func()) *IntentWatcher {
	if pollInterval <= 0 {
		pollInterval = 60 * time.Second
	}
	return &IntentWatcher{
		stateDir:     stateDir,
		pollInterval: pollInterval,
		onChange:     onChange,
		lastMtimes:   map[string]time.Time{},
		lastPresent:  map[string]bool{},
		lastSizes:    map[string]int64{},
	}
}

// Run blocks until ctx is canceled. On every poll tick it checks
// each tracked intent file's mtime + presence against the last
// snapshot; on any difference it invokes onChange and re-baselines.
//
// The initial baseline is taken BEFORE the first tick so the watcher
// does NOT fire on startup just because lastMtimes was zero-valued.
// This matches operator expectation: the supervisor's startup-path
// reconcile (Task 6.2 stub + Task 7.1) already loads intent once;
// the watcher exists only for *subsequent* changes.
//
// ctx cancellation is honored within one pollInterval of receipt —
// the ticker fires independently of ctx so a long-running onChange
// callback won't starve the cancel signal. On exit, the function
// returns cleanly without firing onChange; pending changes are
// dropped (the next supervisor startup will pick them up via the
// startup-path reconcile).
func (w *IntentWatcher) Run(ctx context.Context) {
	// Initial baseline: record current mtimes WITHOUT firing onChange.
	// Without this step, the first tick would see lastMtimes empty and
	// fire onChange against any pre-existing intent files, causing a
	// spurious reconcile pass that duplicates the startup-path read.
	w.snapshotMtimes()

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.detectChange() {
				if w.onChange != nil {
					w.onChange()
				}
				// Re-baseline AFTER firing so the next tick measures
				// against the new state. If onChange races with a
				// concurrent in-flight write, the next tick will see
				// a fresh mtime and fire again — that's the intended
				// at-least-once semantics; reconcile is idempotent.
				w.snapshotMtimes()
			}
		}
	}
}

// snapshotMtimes records the current mtime + presence for every
// tracked file. Errors other than os.ErrNotExist are treated as
// "absent" — a transient permission denial would otherwise be
// stored as a zero-valued time.Time and the next successful stat
// would unconditionally trigger onChange.
func (w *IntentWatcher) snapshotMtimes() {
	for _, name := range watchedIntentFiles {
		path := filepath.Join(w.stateDir, name)
		info, err := os.Stat(path)
		if err == nil {
			w.lastMtimes[name] = info.ModTime()
			w.lastPresent[name] = true
			w.lastSizes[name] = info.Size()
		} else {
			delete(w.lastMtimes, name)
			w.lastPresent[name] = false
			delete(w.lastSizes, name)
		}
	}
}

// detectChange returns true if any tracked file's mtime or presence
// differs from the last snapshot. The check is intentionally cheap
// (one os.Stat per file, no read or hash) so a 60s tick budget is
// well under any practical state-dir contention threshold.
//
// A file that toggles absent → present (or vice versa) ALWAYS counts
// as a change, even if the mtime fields would otherwise compare
// equal (both effectively zero on the absent side).
func (w *IntentWatcher) detectChange() bool {
	for _, name := range watchedIntentFiles {
		path := filepath.Join(w.stateDir, name)
		info, err := os.Stat(path)
		currentPresent := err == nil
		var currentMtime time.Time
		if currentPresent {
			currentMtime = info.ModTime()
		}

		// Presence transition is always a change.
		if currentPresent != w.lastPresent[name] {
			return true
		}
		// Both present: mtime OR size difference is a change. Use !Equal
		// not != because time.Time carries a monotonic-clock reading on
		// some platforms and direct equality compares the monotonic
		// portion, which is not what file mtime semantics want. The size
		// check catches a replace-in-place write that lands within the
		// filesystem mtime-granularity window (same mtime, different
		// content) — the common case for a byte-length-changing JSON
		// intent edit (deep-review P4).
		if currentPresent {
			if !currentMtime.Equal(w.lastMtimes[name]) || info.Size() != w.lastSizes[name] {
				return true
			}
		}
	}
	return false
}

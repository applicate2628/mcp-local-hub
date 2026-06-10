// Package api — v0.6 Phase 4-E1 dual-intent collapse (ADDITIVE step).
//
// intent_collapse.go owns the one-time, in-place merge that folds the
// legacy daemon-intent.json stop overrides into the supervisor-intent.json
// `stops` sub-block (supervisor_intent.go). It is the SINGLE named merge
// owner the spec §15 P1-c mandates: it holds the daemon-intent.json flock
// across the ENTIRE read → merge → backup → write critical section (NOT a
// read-only lock) and re-reads under the held lock immediately before the
// write so a concurrent old-binary `mcphub stop` that lands after the first
// read but before the write is still captured (never silently lost).
//
// E1 is the ADDITIVE first step of the two-step collapse:
//   - daemon-intent.json STAYS on disk (NEVER deleted here) and STAYS
//     written by install_intent.go's existing writers — a free recovery
//     point and the live cross-process stop channel during the redeploy
//     window. E2 (a later PR) deletes the file + the writers.
//   - the merge writes the merged active-stop set into the unified file's
//     `stops` sub-block so it is the recovery baseline + the new canonical
//     home the repointed IsActiveStop readers learn (via UnifiedStopsFile).
//
// Safety nets (spec §15 P1-c + §12 Phase 4):
//   - a PURE --check / dry-run mode (DaemonIntentCollapseOpts.DryRun) that
//     computes + returns the merge result WITHOUT touching disk, so an
//     operator can preview the merge against the LIVE state-dir BEFORE
//     deploying E1;
//   - a code-baked pre-merge backup to <state-dir>/pre-collapse-backup-<ts>/
//     taken by the merge path itself (not a manual operator step) before
//     any write.
package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gofrs/flock"
)

// preCollapseBackupPrefix is the leaf prefix for the code-baked pre-merge
// backup directory: <state-dir>/pre-collapse-backup-<RFC3339-nanos>/. The
// timestamp suffix uses the colon-free, lexicographically-sortable layout
// (quarantineSuffixLayout, daemon_intent.go:73) so Windows accepts the path
// and operators can sort backups by age.
const preCollapseBackupPrefix = "pre-collapse-backup-"

// preCollapseBackupRetention is the number of newest pre-collapse-backup-<ts>/
// directories writePreCollapseBackup keeps after each successful backup. It
// mirrors the migration-journal "keep the 5 newest" retention (CLAUDE.md §
// "Migration journal layout + retention") so a long-lived host whose stops keep
// changing — each delta pass copies the ~5 MB daemon-intent.json — does not
// accumulate unbounded backup directories under <state-dir> across restarts
// (adversarial review P3-2).
const preCollapseBackupRetention = 5

// collapseAfterFirstSupervisorReadHook is a test-only seam that fires once,
// AFTER the top-of-pass supervisor-intent.json read but BEFORE the write path
// acquires the supervisor-intent flock — exactly the window a concurrent
// supervisor-intent.json writer can land in. Tests install it to simulate that
// concurrent writer and assert the P2-A fresh-re-read preserves the concurrent
// non-Stops edit. nil in production (zero overhead, no behavior).
var collapseAfterFirstSupervisorReadHook func()

// MergeStopsAction labels what the merge did to one task's stop record, for
// the --check / audit surface. Pure-data; no behavior hangs off it.
type MergeStopsAction string

const (
	// MergeStopAdded — daemon-intent.json carries an active stop for a task
	// the unified stops sub-block did not have; the merge adds it.
	MergeStopAdded MergeStopsAction = "add"
	// MergeStopUpdated — both sources carry the task but the DaemonIntent
	// record differs (Desired/Reason/UpdatedAt); the merge overwrites the
	// sub-block with the daemon-intent.json value (the live writers' file
	// is the authority for an active stop).
	MergeStopUpdated MergeStopsAction = "update"
	// MergeStopDroppedExpired — daemon-intent.json has a stopped entry that
	// is NO LONGER an active stop at `now` (TTL expired, stale-bound passed,
	// or Desired=running). The merge does NOT carry it (spec §5.1-E
	// "re-evaluate IsActiveStop(now) so expired/stale stops are dropped").
	MergeStopDroppedExpired MergeStopsAction = "drop-expired"
)

// MergeStopsEntry is one per-task merge decision for the --check report.
type MergeStopsEntry struct {
	TaskName string           `json:"task_name"`
	Action   MergeStopsAction `json:"action"`
	Reason   string           `json:"reason,omitempty"`
}

// DaemonIntentCollapseOpts controls one merge invocation.
type DaemonIntentCollapseOpts struct {
	// DryRun is the --check mode: compute the merge result and return it
	// WITHOUT taking the backup or writing supervisor-intent.json. A pure
	// preview the operator runs on the live state-dir before deploy.
	DryRun bool
	// Now is the reference clock threaded into IsActiveStop for TTL +
	// clock-skew evaluation. Zero value → time.Now().UTC() (so production
	// callers can leave it unset; tests inject a fixed clock).
	Now time.Time
}

// DaemonIntentCollapseResult is the outcome of a merge (or --check) pass.
type DaemonIntentCollapseResult struct {
	// Entries lists every per-task merge decision (add/update/drop-expired),
	// sorted by task name for deterministic output.
	Entries []MergeStopsEntry
	// MergedStops is the resulting unified stops map (the active-stop set
	// the merge would persist into supervisor-intent.json's `stops`
	// sub-block). Always non-nil.
	MergedStops map[string]DaemonIntent
	// Changed reports whether the merge altered the supervisor-intent stops
	// sub-block versus its prior on-disk content. False → the write is a
	// no-op (idempotent re-run, spec §5.1-E).
	Changed bool
	// Wrote reports whether this pass actually wrote supervisor-intent.json.
	// Always false in DryRun mode; true only when !DryRun AND Changed.
	Wrote bool
	// BackupDir is the absolute path of the pre-merge backup directory taken
	// before the write. Empty in DryRun mode or when nothing was written.
	BackupDir string
}

// mergeDaemonIntentStops is the PURE core: given the supervisor intent and
// the legacy daemon-intent file, it computes the merged active-stop set +
// the per-task decisions. No I/O, no clock reads beyond `now`. Shared by the
// dry-run (--check) path and the real write path so the preview is byte-for-
// byte what the write persists.
//
// Rules (spec §5.1-E):
//   - start from the supervisor intent's existing `stops` sub-block (so a
//     prior merge's baseline is preserved across re-runs);
//   - for each daemon-intent.json task, re-evaluate IsActiveStop(now):
//       * active   → carry the FULL DaemonIntent record (Desired, Reason,
//                    UpdatedAt) into the merged map, preserving TTL /
//                    clock-skew / reason semantics verbatim;
//       * inactive → drop it (expired/stale/running), recorded as
//                    drop-expired ONLY when the prior baseline had it (a
//                    stop that just expired) — a never-active task produces
//                    no entry and no noise.
//   - the legacy file is authoritative for the tasks it names: an entry that
//     went inactive REMOVES the corresponding baseline stop so a re-enabled
//     daemon is not left suppressed by a stale baseline.
func mergeDaemonIntentStops(
	supervisorIntent *SupervisorIntentFile,
	daemonIntentFile *DaemonIntentFile,
	now time.Time,
) DaemonIntentCollapseResult {
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Seed the merged set from the existing baseline (copy — never mutate
	// the caller's map).
	merged := map[string]DaemonIntent{}
	if supervisorIntent != nil && supervisorIntent.Stops != nil {
		for k, v := range supervisorIntent.Stops {
			merged[k] = v
		}
	}

	var entries []MergeStopsEntry
	if daemonIntentFile != nil {
		for taskName, di := range daemonIntentFile.Tasks {
			// Normalize to the canonical leading-backslash key so the
			// unified sub-block keys match Daemons[].TaskName + the legacy
			// writers' canonical form (daemon_intent.go canonicalIntentTaskKey).
			key := canonicalIntentTaskKey(taskName)
			active, _ := di.IsActiveStop(now)
			prior, hadPrior := merged[key]
			if active {
				switch {
				case !hadPrior:
					merged[key] = di
					entries = append(entries, MergeStopsEntry{
						TaskName: key, Action: MergeStopAdded, Reason: di.Reason,
					})
				case prior != di:
					merged[key] = di
					entries = append(entries, MergeStopsEntry{
						TaskName: key, Action: MergeStopUpdated, Reason: di.Reason,
					})
					// equal record → no-op, no entry (idempotent re-run)
				}
				continue
			}
			// Inactive in daemon-intent.json: the legacy file is authoritative,
			// so an expired/stale/running task must NOT remain suppressed by a
			// stale baseline. Drop it from the merged set (recorded only when
			// the baseline actually had it).
			if hadPrior {
				delete(merged, key)
				entries = append(entries, MergeStopsEntry{
					TaskName: key, Action: MergeStopDroppedExpired, Reason: di.Reason,
				})
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].TaskName < entries[j].TaskName })

	changed := !stopsMapsEqual(priorStops(supervisorIntent), merged)

	return DaemonIntentCollapseResult{
		Entries:     entries,
		MergedStops: merged,
		Changed:     changed,
	}
}

// priorStops returns the existing stops sub-block (or an empty map) so the
// change-detection comparison never nil-derefs.
func priorStops(f *SupervisorIntentFile) map[string]DaemonIntent {
	if f == nil || f.Stops == nil {
		return map[string]DaemonIntent{}
	}
	return f.Stops
}

// stopsMapsEqual reports whether two stops maps carry the same tasks with the
// same DaemonIntent records. DaemonIntent is comparable (string/string/Time),
// but Time equality must use Equal (monotonic-clock + location safety), so we
// compare field-by-field.
func stopsMapsEqual(a, b map[string]DaemonIntent) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if av.Desired != bv.Desired || av.Reason != bv.Reason || !av.UpdatedAt.Equal(bv.UpdatedAt) {
			return false
		}
	}
	return true
}

// CheckDaemonIntentCollapse is the PURE --check / dry-run entry point. It
// reads BOTH intent files from the state dir (under the daemon-intent flock
// for a consistent snapshot) and returns the merge result WITHOUT writing.
// Safe to run on the LIVE state-dir before deploying E1 (spec §15 P1-c (i)).
//
// stateDir is the resolved per-user state directory (callers pass the same
// value the supervisor resolved; tests pass a t.TempDir via the
// SetDaemonStateRootForTest seam). now=zero → time.Now().UTC().
func CheckDaemonIntentCollapse(stateDir string, now time.Time) (DaemonIntentCollapseResult, error) {
	return runDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{DryRun: true, Now: now})
}

// RunDaemonIntentCollapse is the SINGLE named merge owner. It performs the
// one-time in-place merge: holds the daemon-intent.json flock across the
// ENTIRE read → merge → backup → re-read-under-lock → write critical section,
// takes a code-baked pre-merge backup, and writes the merged stops into
// supervisor-intent.json's `stops` sub-block. It NEVER deletes
// daemon-intent.json (that is E2).
//
// Idempotent: a second invocation whose merge produces no delta writes
// nothing (Result.Changed=false, Wrote=false).
//
// Concurrency (spec §15 P1-c): WriteDaemonIntent is the daemon-intent writer
// that matters in production — it acquires the SAME daemon-intent flock, so
// holding it across this whole pass blocks a concurrent old-binary `mcphub
// stop` until the merge releases — no stop is lost. (ClearDaemonIntent shares
// the flock and the canonical-key normalization but has ZERO production
// callers: the re-enable path writes a Desired=running tombstone via
// WriteDaemonIntent that the merge loop drops, rather than clearing the entry,
// so the baseline-drop relies on that Desired=running tombstone path, not on
// ClearDaemonIntent.) The defensive re-read under the held lock immediately
// before the write re-merges any delta as belt-and-suspenders even against a
// hypothetical future writer that bypassed the flock.
//
// The WRITE TARGET — supervisor-intent.json — has its OWN set of concurrent
// writers (InstallParsedManifest, serena_intent_repair, register_supervisor,
// the autostart shim) that go through the SEPARATE supervisor-intent flock,
// NOT the daemon-intent flock held here. To avoid a lost update on those
// non-Stops fields, the write path below acquires the supervisor-intent flock,
// re-reads the file fresh under it, re-merges, and applies ONLY the Stops
// sub-block (adversarial review P2-A).
//
// Universal lock order (CLAUDE.md): when a caller already holds migration.lock
// it is acquired BEFORE entering here; the supervisor-intent flock is then
// acquired AFTER the daemon-intent flock — the merge owner acquires the
// lower-precedence daemon-intent flock first and the supervisor-intent flock
// second (the same nesting order WriteStateFileAtomic uses internally), so it
// does not deadlock against migration.lock→supervisor.lock holders.
func RunDaemonIntentCollapse(stateDir string, opts DaemonIntentCollapseOpts) (DaemonIntentCollapseResult, error) {
	return runDaemonIntentCollapse(stateDir, opts)
}

func runDaemonIntentCollapse(stateDir string, opts DaemonIntentCollapseOpts) (DaemonIntentCollapseResult, error) {
	if stateDir == "" {
		return DaemonIntentCollapseResult{}, errors.New("intent-collapse: state dir is empty")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	supervisorIntentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	daemonIntentPath := filepath.Join(stateDir, intentFileLeaf)
	daemonLockPath := filepath.Join(stateDir, intentLockLeaf)

	// Acquire the daemon-intent flock for the WHOLE critical section. This is
	// the load-bearing concurrency guarantee: it serializes against the legacy
	// WriteDaemonIntent writer for the entire merge (ClearDaemonIntent shares
	// the flock but is currently unused in production — see the function
	// docstring; the baseline-drop relies on WriteDaemonIntent's Desired=running
	// tombstone path instead).
	lock := flock.New(daemonLockPath)
	if err := lock.Lock(); err != nil {
		return DaemonIntentCollapseResult{}, fmt.Errorf("intent-collapse: flock daemon-intent: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	// Read the supervisor intent (the merge target). Missing file → empty
	// intent (a first-boot host with no descriptors but possibly a legacy
	// daemon-intent.json still merges its stops into a freshly-minted file).
	// Reuses the install-side reader (install_parsed_manifest.go:818) so the
	// missing-file semantics stay owned in one place.
	supervisorIntent, _, err := readSupervisorIntentForMerge(supervisorIntentPath)
	if err != nil {
		return DaemonIntentCollapseResult{}, fmt.Errorf("intent-collapse: read supervisor-intent.json: %w", err)
	}

	// Test-only seam: simulate a concurrent supervisor-intent.json writer that
	// lands in the window between this first read and the write-path's fresh
	// re-read under the supervisor-intent flock (P2-A regression coverage).
	if collapseAfterFirstSupervisorReadHook != nil {
		collapseAfterFirstSupervisorReadHook()
	}

	// First read of daemon-intent.json under the held lock.
	daemonIntent, err := readDaemonIntentForMerge(daemonIntentPath)
	if err != nil {
		return DaemonIntentCollapseResult{}, err
	}

	result := mergeDaemonIntentStops(supervisorIntent, daemonIntent, now)

	if opts.DryRun {
		// --check: no backup, no write. Pure preview.
		return result, nil
	}

	if !result.Changed {
		// Idempotent no-op: the stops sub-block already reflects the active
		// stops. Skip both the backup and the write.
		return result, nil
	}

	// Defensive re-read under the SAME held lock immediately before the write
	// (spec §15 P1-c). Since the flock is held, a flock-honoring writer cannot
	// have landed; this re-merge captures any delta from a hypothetical
	// flock-bypassing writer and keeps the design robust regardless.
	daemonIntentReread, err := readDaemonIntentForMerge(daemonIntentPath)
	if err != nil {
		return DaemonIntentCollapseResult{}, err
	}
	result = mergeDaemonIntentStops(supervisorIntent, daemonIntentReread, now)
	if !result.Changed {
		return result, nil
	}

	// Lost-update guard on the WRITE TARGET (adversarial review P2-A). The
	// daemon-intent flock held above serializes only against daemon-intent.json
	// writers — it does NOT serialize against supervisor-intent.json writers
	// (InstallParsedManifest, serena_intent_repair, register_supervisor, the
	// autostart shim). Those go through WriteStateFileAtomic, which takes the
	// SEPARATE `supervisor-intent.json.lock` flock. The supervisorIntent read at
	// the top of this pass (readSupervisorIntentForMerge) was taken WITHOUT that
	// lock, so a concurrent supervisor-intent writer that lands between that read
	// and our write would have its Daemons / StrictMode / MaintenanceTimers /
	// runtime_spec edits silently reverted by writing back our stale whole-struct
	// snapshot.
	//
	// Fix: acquire the supervisor-intent flock, then re-read the file FRESH under
	// it and re-merge against that fresh baseline, so any concurrent non-Stops
	// edit survives and the Stops sub-block is recomputed against the fresh
	// existing stops. We then apply ONLY the recomputed Stops sub-block onto the
	// freshly-read struct (mirroring the daemon-intent re-read above) and commit
	// via the LOCK-FREE secure-write body — re-entering WriteSupervisorIntent
	// (which re-acquires the same flock) would deadlock, exactly the
	// readIntentLocked/writeIntentLocked split daemon_intent.go uses.
	supLock := flock.New(supervisorIntentPath + supervisorIntentLockSuffix)
	if err := supLock.Lock(); err != nil {
		return DaemonIntentCollapseResult{}, fmt.Errorf("intent-collapse: flock supervisor-intent: %w", err)
	}
	defer func() { _ = supLock.Unlock() }()

	freshSupervisorIntent, _, err := readSupervisorIntentForMerge(supervisorIntentPath)
	if err != nil {
		return DaemonIntentCollapseResult{}, fmt.Errorf("intent-collapse: re-read supervisor-intent.json: %w", err)
	}
	// Recompute the merge against the FRESH baseline so the Stops decision uses
	// the freshly-read existing stops (a concurrent writer may have edited Stops
	// too). The daemon-intent re-read above is authoritative for the active-stop
	// set; the fresh supervisor-intent contributes the up-to-date baseline + all
	// the non-Stops fields we must preserve.
	result = mergeDaemonIntentStops(freshSupervisorIntent, daemonIntentReread, now)
	if !result.Changed {
		return result, nil
	}

	// Code-baked pre-merge backup BEFORE any write (spec §15 P1-c (ii)). Taken
	// under the held supervisor-intent flock so it snapshots exactly the bytes
	// we are about to overwrite.
	backupDir, err := writePreCollapseBackup(stateDir, supervisorIntentPath, daemonIntentPath, now)
	if err != nil {
		return DaemonIntentCollapseResult{}, fmt.Errorf("intent-collapse: pre-merge backup: %w", err)
	}
	result.BackupDir = backupDir

	// Persist the merged stops into the unified file. Apply ONLY the Stops
	// sub-block onto the FRESHLY-read struct so every other field of the
	// supervisor intent (Daemons, MaintenanceTimers, StrictMode, runtime_spec
	// rows) — including any concurrent edit that landed since the top-of-pass
	// read — survives the merge.
	freshSupervisorIntent.Stops = result.MergedStops
	if err := writeSupervisorIntentLockHeld(supervisorIntentPath, freshSupervisorIntent); err != nil {
		return DaemonIntentCollapseResult{}, fmt.Errorf("intent-collapse: write supervisor-intent.json: %w", err)
	}
	result.Wrote = true

	// E1 INVARIANT: daemon-intent.json is NOT deleted here. It stays on disk
	// + stays written by its existing writers as a recovery point. E2 removes
	// it. (Documented so a future edit does not "tidy up" the delete in.)
	return result, nil
}

// readDaemonIntentForMerge parses daemon-intent.json from raw bytes under the
// caller's already-held flock (no second lock). Missing → nil (no overrides).
// Corrupt → fail-closed error: a corrupt stop file must NOT silently merge to
// "no stops" and un-suppress a stopped daemon. The caller holds the
// daemon-intent flock, so this performs a lock-free read.
func readDaemonIntentForMerge(path string) (*DaemonIntentFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("intent-collapse: read daemon-intent.json: %w", err)
	}
	parsed, parseErr := ParseDaemonIntentFile(raw)
	if parseErr != nil {
		return nil, fmt.Errorf("intent-collapse: parse daemon-intent.json (fail-closed; refusing to merge to no-stops): %w", parseErr)
	}
	return &parsed, nil
}

// writePreCollapseBackup snapshots BOTH intent files into a fresh
// <state-dir>/pre-collapse-backup-<ts>/ directory before the merge write.
// Code-baked (spec §15 P1-c (ii)) — not a manual operator step. A missing
// source file is skipped (nothing to back up), not an error. Returns the
// absolute backup dir path.
func writePreCollapseBackup(stateDir, supervisorIntentPath, daemonIntentPath string, now time.Time) (string, error) {
	ts := now.UTC().Format(quarantineSuffixLayout)
	backupDir := filepath.Join(stateDir, preCollapseBackupPrefix+ts)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir backup dir: %w", err)
	}
	for _, src := range []string{supervisorIntentPath, daemonIntentPath} {
		if err := copyFileForBackup(src, filepath.Join(backupDir, filepath.Base(src))); err != nil {
			return "", err
		}
	}
	// Bounded retention (adversarial review P3-2): after a successful backup,
	// prune all but the newest preCollapseBackupRetention backup dirs. Per-dir
	// delete failures are non-fatal — pruning is best-effort housekeeping, not a
	// merge precondition, so a locked/AV-held directory must NOT fail the merge
	// that just wrote a valid recovery point.
	pruneOldPreCollapseBackups(stateDir, preCollapseBackupRetention)
	return backupDir, nil
}

// pruneOldPreCollapseBackups keeps the newest `keep` pre-collapse-backup-<ts>/
// directories under stateDir and os.RemoveAll's the rest. The <ts> suffix uses
// quarantineSuffixLayout (colon-free, fixed-width, lexicographically sortable),
// so a descending string sort of the directory basenames is a newest-first age
// sort. Mirrors the migration-journal "5 newest" retention. Best-effort: any
// glob / remove failure is logged-and-skipped (here: silently skipped — the
// caller already holds a valid backup), never returned, so pruning can never
// fail the merge.
func pruneOldPreCollapseBackups(stateDir string, keep int) {
	if keep < 0 {
		keep = 0
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, preCollapseBackupPrefix+"*"))
	if err != nil {
		return
	}
	// Retain only actual directories named with the prefix (a stray file that
	// happens to share the prefix is left untouched).
	var dirs []string
	for _, m := range matches {
		info, statErr := os.Stat(m)
		if statErr != nil || !info.IsDir() {
			continue
		}
		dirs = append(dirs, m)
	}
	if len(dirs) <= keep {
		return
	}
	// Newest first: descending basename sort (the timestamp suffix sorts
	// chronologically because quarantineSuffixLayout is fixed-width + colon-free).
	sort.Slice(dirs, func(i, j int) bool {
		return filepath.Base(dirs[i]) > filepath.Base(dirs[j])
	})
	for _, stale := range dirs[keep:] {
		// Per-dir delete failure is non-fatal (logged-skip; no error path here):
		// a held/locked backup dir must not break housekeeping or the merge.
		_ = os.RemoveAll(stale)
	}
}

// TryReadUnifiedStops is the Phase 4-E1 tray/GUI-side stop reader. It is the
// unified-source replacement for TryReadDaemonIntent on the tray hot path:
// it reads the live daemon-intent.json (bounded by `timeout`, same flock
// budget as TryReadDaemonIntent) AND the supervisor-intent.json stops
// sub-block, then resolves the single stop source via UnifiedStopsFile —
// live daemon-intent.json wins when present (so E1 introduces NO tray-side
// behavior change while the legacy writers still maintain that file), else
// the merged supervisor-intent.json stops sub-block (recovery baseline).
//
// It returns an IntentReadResult whose File carries the unified stops, with
// the SAME State / Err degradation contract TryReadDaemonIntent has, so the
// aggregator's in-process intent cache + Bug #3 user-stop suppression keep
// working unchanged:
//   - daemon-intent.json valid → State=valid, File=live tasks.
//   - daemon-intent.json missing (Err==nil) → File falls back to the
//     supervisor stops sub-block. State is reported "valid" when that
//     fallback yielded any stops (it IS authoritative data, not a degraded
//     read), else "missing" with nil Err (genuinely no stops anywhere).
//   - daemon-intent.json degraded (lock timeout / corrupt, Err!=nil) →
//     State/Err propagated verbatim so the aggregator keeps its cached
//     snapshot rather than flashing a red icon (the existing degrade path).
func (a *API) TryReadUnifiedStops(timeout time.Duration) IntentReadResult {
	res := a.TryReadDaemonIntent(timeout)

	// Degraded read (lock timeout, corrupt+quarantined): keep the existing
	// contract verbatim. The aggregator's cache covers the gap; overlaying a
	// possibly-stale supervisor stops sub-block here could mask the degrade.
	if res.Err != nil {
		return res
	}

	supervisorIntent := readSupervisorStopsForTray()
	var live *DaemonIntentFile
	if res.State == IntentStateValid {
		f := res.File
		live = &f
	}
	unified := UnifiedStopsFile(supervisorIntent, live)
	if unified.Tasks == nil {
		unified.Tasks = map[string]DaemonIntent{}
	}

	out := IntentReadResult{File: *unified}
	if len(unified.Tasks) > 0 {
		// Authoritative stop data present (live or merged baseline).
		out.State = IntentStateValid
	} else {
		// No stops anywhere — genuinely "no preference" (nil Err so the
		// aggregator overwrites stale stops, per its eviction policy).
		out.State = IntentStateMissing
	}
	return out
}

// readSupervisorStopsForTray loads the supervisor-intent stops sub-block for
// the tray-side unified read. Best-effort: any read/parse failure (missing
// file, parent-dir gate, decode error) degrades to an empty stops sub-block
// so the tray reader falls back to live daemon-intent.json alone — never a
// hard failure on the tray hot path.
func readSupervisorStopsForTray() *SupervisorIntentFile {
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		return &SupervisorIntentFile{}
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		return &SupervisorIntentFile{}
	}
	return got
}

// copyFileForBackup copies src → dst at 0600. A missing src is a no-op (the
// host may have only one of the two intent files). All other errors fail the
// backup so the merge does NOT proceed without a recovery point.
func copyFileForBackup(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

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
//     computes + returns the merge result WITHOUT touching disk or lock files,
//     so an operator can preview the merge against the LIVE state-dir BEFORE
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
	// DryRun is the --check mode: compute the merge result with plain unlocked
	// reads and return it WITHOUT taking locks, backups, or writing
	// supervisor-intent.json. It is advisory: a concurrent writer can race the
	// check, while the real merge path below still takes the write locks.
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
	// DeletedLegacyFile reports whether this pass deleted the legacy
	// daemon-intent.json (Phase 4-E2 destructive step). True only when
	// !DryRun AND the file existed AND its active stops were confirmed in the
	// supervisor-intent.json stops sub-block BEFORE the delete. A second
	// (idempotent) E2 boot finds the file already gone and reports false.
	DeletedLegacyFile bool
}

// mergeDaemonIntentStops is the PURE core: given the supervisor intent and
// the legacy daemon-intent file, it computes the merged active-stop set +
// the per-task decisions. No I/O, no clock reads beyond `now`. Shared by the
// dry-run (--check) path and the real write path so the preview is byte-for-
// byte what the write persists.
//
// Rules (spec §5.1-E): start from the supervisor intent's existing `stops`
// sub-block, then evaluate each legacy daemon-intent.json task at `now`.
//
// Per-task decision table:
//   - legacy ACTIVE + absent in sub-block → ADD the full legacy record.
//   - legacy ACTIVE + sub-block record differs + legacy.UpdatedAt NEWER →
//     UPDATE the sub-block record; the mixed-version legacy writer is the
//     newer authority for this stop.
//   - legacy ACTIVE + sub-block record NEWER-or-equal → KEEP the sub-block
//     record; an old/different legacy file must not downgrade the sole source.
//   - legacy INACTIVE/running → NO-OP; never remove or downgrade an existing
//     sub-block stop from a stale legacy tombstone.
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
				if !hadPrior {
					merged[key] = di
					entries = append(entries, MergeStopsEntry{
						TaskName: key, Action: MergeStopAdded, Reason: di.Reason,
					})
				} else if !daemonIntentRecordsEqual(prior, di) && di.UpdatedAt.After(prior.UpdatedAt) {
					merged[key] = di
					entries = append(entries, MergeStopsEntry{
						TaskName: key, Action: MergeStopUpdated, Reason: di.Reason,
					})
				}
				continue
			}
			// Inactive legacy entries are stale tombstones after E2. The
			// sub-block is authoritative for existing tasks, so an old
			// Desired=running / expired daemon-intent entry is a no-op for
			// the merged stops map, but the preview still reports the drop so
			// operators can audit the legacy file entries E2 will discard.
			entries = append(entries, MergeStopsEntry{
				TaskName: key, Action: MergeStopDroppedExpired, Reason: di.Reason,
			})
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
		if !daemonIntentRecordsEqual(av, bv) {
			return false
		}
	}
	return true
}

func daemonIntentRecordsEqual(a, b DaemonIntent) bool {
	return a.Desired == b.Desired && a.Reason == b.Reason && a.UpdatedAt.Equal(b.UpdatedAt)
}

// CheckDaemonIntentCollapse is the PURE --check / dry-run entry point. It
// reads BOTH intent files from the state dir with plain read-only I/O and
// returns the merge result WITHOUT writing, taking a flock, or creating a lock
// file. Safe to run on the LIVE state-dir before deploying E1 (spec §15 P1-c
// (i)), but advisory: a concurrent writer can race the unlocked read, while the
// real merge path below remains the serialized authority.
//
// stateDir is the resolved per-user state directory (callers pass the same
// value the supervisor resolved; tests pass a t.TempDir via the
// SetDaemonStateRootForTest seam). now=zero → time.Now().UTC().
func CheckDaemonIntentCollapse(stateDir string, now time.Time) (DaemonIntentCollapseResult, error) {
	return runDaemonIntentCollapse(stateDir, DaemonIntentCollapseOpts{DryRun: true, Now: now})
}

// RunDaemonIntentCollapse is the SINGLE named merge owner. It performs the
// one-time in-place merge: holds the daemon-intent.json flock across the
// ENTIRE read → merge → backup → re-read-under-lock → write → DELETE critical
// section, takes a code-baked pre-merge backup, writes the merged stops into
// supervisor-intent.json's `stops` sub-block, and — Phase 4-E2 — DELETES the
// legacy daemon-intent.json (+ its .lock) ONCE the sub-block is confirmed to
// carry the file's active stops.
//
// E2 deletion ordering (spec §5.1-E — "delete daemon-intent.json LAST"):
// the file is deleted ONLY after either (a) the merge wrote the active stops
// into the sub-block, or (b) the no-delta path confirmed the sub-block ALREADY
// reflects them. Immediately before the delete, daemon-intent.json is re-read
// under the held flock and every active stop it names is re-checked present in
// the sub-block (deleteLegacyDaemonIntentIfMerged); the delete is REFUSED if
// any active stop is missing, so a stop can never be lost to the delete. A
// corrupt daemon-intent.json fails the merge CLOSED upstream, so the delete is
// never reached on a corrupt file (forensic state preserved).
//
// Idempotent: a second invocation finds daemon-intent.json already gone →
// readDaemonIntentForMerge returns nil → the merge is a no-op (Changed=false)
// → the delete is a no-op (DeletedLegacyFile=false). A crash AFTER the
// sub-block write but BEFORE the delete leaves daemon-intent.json on disk;
// the next boot re-runs, finds the sub-block already carries the stops
// (Changed=false), re-confirms, and deletes — so the destructive step is
// crash-safe + retried.
//
// Concurrency (spec §15 P1-c): after E2 the live stop writers no longer touch
// daemon-intent.json (they write the sub-block via WriteStopIntent). The only
// remaining daemon-intent.json writer is an OLD binary still running its
// pre-E2 WriteDaemonIntent path during the redeploy window — it acquires the
// SAME daemon-intent flock, so holding the flock across this whole pass blocks
// such a concurrent old-binary `mcphub stop` until the merge releases — no
// stop is lost. The defensive re-read under the held lock immediately before
// the write re-merges any delta as belt-and-suspenders even against a
// hypothetical future writer that bypassed the flock. The delete (the E2 tail)
// runs under the same held flock, so no old-binary write can re-create the
// file between the verify and the delete.
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

	if opts.DryRun {
		return checkDaemonIntentCollapseUnlocked(supervisorIntentPath, daemonIntentPath, now)
	}

	// Acquire the daemon-intent flock for the WHOLE critical section (read →
	// merge → write → DELETE). This is the load-bearing concurrency guarantee:
	// it serializes against an OLD binary's legacy WriteDaemonIntent writer for
	// the entire merge AND the delete, so no concurrent old-binary `mcphub stop`
	// can land a stop after the merge reads but before the delete, and none can
	// re-create the file between the delete-verify and the delete itself.
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

	if !result.Changed {
		// Idempotent no-op on the SUB-BLOCK: the stops sub-block already
		// reflects the active stops (a prior boot merged them, or there were
		// none). Skip the backup + write, but STILL attempt the E2 ordered
		// deletion — the file may be a leftover from E1 / a crash between the
		// E1 write and an E2 delete. deleteLegacyDaemonIntentIfMerged re-reads
		// daemon-intent.json under the held flock and only deletes when every
		// active stop it names is present in the on-disk sub-block.
		deleted, delErr := deleteLegacyDaemonIntentIfMerged(stateDir, supervisorIntentPath, daemonIntentPath, daemonIntent, now)
		if delErr != nil {
			return result, delErr
		}
		result.DeletedLegacyFile = deleted
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
		// As above: no sub-block delta after the re-read, but the file may
		// still need the E2 ordered deletion.
		deleted, delErr := deleteLegacyDaemonIntentIfMerged(stateDir, supervisorIntentPath, daemonIntentPath, daemonIntentReread, now)
		if delErr != nil {
			return result, delErr
		}
		result.DeletedLegacyFile = deleted
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
		deleted, delErr := deleteLegacyDaemonIntentIfMerged(stateDir, supervisorIntentPath, daemonIntentPath, daemonIntentReread, now)
		if delErr != nil {
			return result, delErr
		}
		result.DeletedLegacyFile = deleted
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
	if freshSupervisorIntent == nil {
		freshSupervisorIntent = &SupervisorIntentFile{Version: 1}
	}
	freshSupervisorIntent.Stops = result.MergedStops
	if err := writeSupervisorIntentLockHeld(supervisorIntentPath, freshSupervisorIntent); err != nil {
		return DaemonIntentCollapseResult{}, fmt.Errorf("intent-collapse: write supervisor-intent.json: %w", err)
	}
	result.Wrote = true

	// E2 ORDERED DELETION (spec §5.1-E): the sub-block now carries the active
	// stops (just written above, under both the daemon-intent flock AND the
	// supervisor-intent flock still held by the deferred supLock.Unlock). Delete
	// daemon-intent.json LAST, after re-confirming under the held daemon-intent
	// flock that every active stop it names is present in the sub-block we just
	// wrote. If the delete is refused or fails, the merge still SUCCEEDED (the
	// stops are safely in the sub-block) — the file just lingers for the next
	// boot to retry, never a lost stop.
	deleted, delErr := deleteLegacyDaemonIntentIfMerged(stateDir, supervisorIntentPath, daemonIntentPath, daemonIntentReread, now)
	if delErr != nil {
		return result, delErr
	}
	result.DeletedLegacyFile = deleted
	return result, nil
}

func checkDaemonIntentCollapseUnlocked(supervisorIntentPath, daemonIntentPath string, now time.Time) (DaemonIntentCollapseResult, error) {
	supervisorIntent, _, err := readSupervisorIntentForMerge(supervisorIntentPath)
	if err != nil {
		return DaemonIntentCollapseResult{}, fmt.Errorf("intent-collapse: read supervisor-intent.json: %w", err)
	}
	daemonIntent, err := readDaemonIntentForMerge(daemonIntentPath)
	if err != nil {
		return DaemonIntentCollapseResult{}, err
	}
	return mergeDaemonIntentStops(supervisorIntent, daemonIntent, now), nil
}

// deleteLegacyDaemonIntentIfMerged performs the Phase 4-E2 destructive step:
// it deletes the legacy daemon-intent.json (+ its .lock) ONLY after verifying
// the supervisor-intent.json `stops` sub-block carries every ACTIVE stop the
// file holds. The caller MUST already hold the daemon-intent flock for the
// whole merge pass, so no concurrent old-binary writer can re-populate
// daemon-intent.json between the verify and the delete.
//
// Safety order (NEVER delete before the stops persist):
//  1. If daemon-intent.json is already gone → idempotent no-op (false, nil).
//  2. Re-read the CURRENT supervisor-intent.json sub-block.
//  3. Re-evaluate every daemon-intent.json task's IsActiveStop(now); for each
//     ACTIVE stop, REFUSE the delete unless the sub-block carries the SAME
//     record (Desired/Reason/UpdatedAt). A missing or divergent active stop
//     means the merge has not (yet) durably captured it → keep the file.
//  4. Only on full confirmation: os.Remove(daemon-intent.json), then
//     best-effort os.Remove the sibling .lock.
//
// daemonIntent is the already-read DaemonIntentFile (may be nil when the file
// was absent at read time); passing it avoids a redundant re-read, but step 1
// re-stats the path so a file that vanished mid-pass is handled. now is the
// merge clock.
//
// Returns (true, nil) when the file was deleted this call; (false, nil) when
// there was nothing to delete or the delete was safely refused (stops not yet
// confirmed — non-fatal, retried next boot); (false, err) only on an
// unexpected I/O failure reading the sub-block for confirmation.
func deleteLegacyDaemonIntentIfMerged(
	stateDir, supervisorIntentPath, daemonIntentPath string,
	daemonIntent *DaemonIntentFile,
	now time.Time,
) (bool, error) {
	// Step 1: nothing to delete if the file is already gone (idempotent).
	if _, statErr := os.Stat(daemonIntentPath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("intent-collapse: stat daemon-intent.json before delete: %w", statErr)
	}

	// Step 2: re-read the CURRENT sub-block (post-write or already-merged).
	current, _, err := readSupervisorIntentForMerge(supervisorIntentPath)
	if err != nil {
		return false, fmt.Errorf("intent-collapse: re-read supervisor-intent.json before delete: %w", err)
	}
	subBlock := map[string]DaemonIntent{}
	if current != nil && current.Stops != nil {
		subBlock = current.Stops
	}

	// Step 3: REFUSE the delete unless every ACTIVE stop in daemon-intent.json
	// is present in the sub-block as either the identical record or a strictly
	// newer record. The newer case means the merge already kept the sub-block
	// authority instead of downgrading it to stale legacy data. An inactive
	// (expired/running) task is intentionally NOT required (the merge drops
	// those), so it is not a blocker. This is the "never lose a stop" gate.
	if daemonIntent != nil {
		for taskName, di := range daemonIntent.Tasks {
			active, _ := di.IsActiveStop(now)
			if !active {
				continue
			}
			key := canonicalIntentTaskKey(taskName)
			got, ok := subBlock[key]
			if !ok || !daemonIntentRecordMergedOrSuperseded(got, di) {
				// An active stop is NOT durably in the sub-block yet — keep the
				// file so the next boot re-merges it. Never delete here.
				return false, nil
			}
		}
	}

	// Step 4: confirmed — delete the file, then best-effort the sibling lock.
	if rmErr := os.Remove(daemonIntentPath); rmErr != nil {
		if errors.Is(rmErr, os.ErrNotExist) {
			// Raced to gone between stat + remove — treat as already deleted.
			return false, nil
		}
		return false, fmt.Errorf("intent-collapse: delete daemon-intent.json: %w", rmErr)
	}
	// Best-effort .lock removal: a held flock on Windows cannot be unlinked,
	// and a stale lock file is harmless (the next flock just re-creates/opens
	// it), so a failure here is non-fatal — the destructive step (deleting the
	// stop data) already committed.
	daemonLockPath := filepath.Join(stateDir, intentLockLeaf)
	_ = os.Remove(daemonLockPath)
	return true, nil
}

func daemonIntentRecordMergedOrSuperseded(subBlock, legacy DaemonIntent) bool {
	return daemonIntentRecordsEqual(subBlock, legacy) || subBlock.UpdatedAt.After(legacy.UpdatedAt)
}

// readDaemonIntentForMerge parses daemon-intent.json from raw bytes without
// taking a lock. The write path calls it under the already-held daemon-intent
// flock; the dry-run path calls it intentionally unlocked so --check stays
// read-only. Missing → nil (no overrides). Corrupt → fail-closed error: a
// corrupt stop file must NOT silently merge to "no stops" and un-suppress a
// stopped daemon.
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

// TryReadUnifiedStops is the Phase 4-E2 tray/GUI-side stop reader. After E2 the
// supervisor-intent.json `stops` sub-block is the SOLE stop source
// (daemon-intent.json is deleted), so this reads the sub-block directly and
// no longer consults daemon-intent.json at all.
//
// It returns an IntentReadResult whose File carries the sub-block stops, with
// the SAME State / Err degradation contract TryReadDaemonIntent had, so the
// aggregator's in-process intent cache + Bug #3 user-stop suppression keep
// working unchanged:
//   - sub-block read clean, ≥1 stop → State=valid, File=stops.
//   - sub-block read clean, no stops (or supervisor-intent.json missing) →
//     State=missing, nil Err (genuinely "no preference"; the aggregator may
//     overwrite stale stops per its eviction policy).
//   - sub-block read DEGRADED (corrupt supervisor-intent.json, parent-dir
//     gate failure) → State=corrupt + Err set, so the aggregator KEEPS its
//     cached snapshot rather than fail-OPEN un-suppressing a stopped daemon
//     (the fail-loud contract, extended to the sole stop source — the E1
//     readSupervisorStopsForTray fail-OPEN swallow is replaced).
//
// The `timeout` parameter is retained for signature stability but is now
// advisory: the supervisor-intent.json read is a small lock-free os.ReadFile
// (no ~5 MB daemon-intent.json flock to bound), so there is no lock-acquisition
// budget to honor.
func (a *API) TryReadUnifiedStops(_ time.Duration) IntentReadResult {
	supervisorIntent, readErr := readSupervisorStopsForTray()
	if readErr != nil {
		// Degraded sub-block read: surface it (State=corrupt, Err set) so the
		// tray aggregator preserves its prior cached stops instead of
		// flashing "no stops" and reviving a deliberately-stopped daemon.
		// errors.Is(os.ErrNotExist) is NOT a degrade — handled below as the
		// genuine "no stops" path.
		if errors.Is(readErr, os.ErrNotExist) {
			return IntentReadResult{
				State: IntentStateMissing,
				File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			}
		}
		return IntentReadResult{
			State: IntentStateCorrupt,
			File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			Err:   readErr,
		}
	}

	unified := supervisorIntent.StopsAsDaemonIntentFile()
	if unified.Tasks == nil {
		unified.Tasks = map[string]DaemonIntent{}
	}

	out := IntentReadResult{File: *unified}
	if len(unified.Tasks) > 0 {
		out.State = IntentStateValid
	} else {
		// No stops in the sub-block — genuinely "no preference" (nil Err so
		// the aggregator overwrites stale stops, per its eviction policy).
		out.State = IntentStateMissing
	}
	return out
}

// readSupervisorStopsForTray loads the supervisor-intent.json stops sub-block
// for the tray-side unified read (the sole stop source after E2). It returns
// the parsed intent and the read error so the caller can apply the fail-loud
// degrade contract (a corrupt read must NOT silently un-suppress). A missing
// supervisor-intent.json returns (empty file, os.ErrNotExist) so the caller
// distinguishes "no stops yet" from "read degraded".
func readSupervisorStopsForTray() (*SupervisorIntentFile, error) {
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		return &SupervisorIntentFile{}, err
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		return &SupervisorIntentFile{}, err
	}
	return got, nil
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

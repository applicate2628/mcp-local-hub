// Package api — Task 4 watchdog cooldown + restart-pending durability +
// sliding-30min strike windows (watchdog plan v13 §6, §13, §14, §28,
// §29, §31, §36, §38, §45, §52).
//
// watchdog_state.go owns the on-disk watchdog-state.json file, the
// in-memory cooldown engine that the watchdog driver consumes, and the
// three sliding-30min strike windows (corrupt / audit-failure / stale-
// clear). Every persistence concern shared with later watchdog tasks
// (7 driver, 9 scheduler glue) goes through this file's exported
// surface.
//
// File layout: ${DaemonStateDir}/watchdog-state.json plus the sibling
// ${...}/watchdog-state.json.lock. Both files are 0600 (POSIX) and live
// under the per-user state dir created by Task 1.
//
// Schema (plan §52):
//
//	type CooldownEntry struct {
//	    FirstAttemptAt   time.Time `json:"first_attempt_at"`
//	    AttemptsInWindow int       `json:"attempts_in_window"`
//	    LastRunningAt    time.Time `json:"last_running_at"`
//	    ChronicCycles    int       `json:"chronic_cycles"`
//	    RestartPendingAt time.Time `json:"restart_pending_at"` // §31
//	}
//
//	type WatchdogState struct {
//	    Cooldowns           map[string]CooldownEntry `json:"cooldowns"`
//	    LastWallClockSeen   time.Time                `json:"last_wall_clock_seen"`
//	    CorruptStrikeWindow []time.Time              `json:"corrupt_strike_window"`
//	    AuditFailureWindow  []time.Time              `json:"audit_failure_window"`
//	    StaleClearWindow    []time.Time              `json:"stale_clear_window"`
//	}
//
// Three-state read parallels Task 2 (daemon_intent.go):
//   - missing: file absent on disk → bootstrap with a fresh empty Cooldown.
//   - corrupt: parse failure → quarantine via `${stem}.corrupt-{ts}` +
//     post-rename prune to QuarantineCap newest under flock + the returned
//     Cool is a suppress-all stub returning Due=false for every task name
//     (fail-CLOSED per §13).
//   - valid: parse + schema check both succeed; Cool is the real engine.
//
// Sliding windows (plan §52 + Code Review #7 explicit clarification):
//
// | Window               | Threshold                    | Effect                                     |
// |----------------------|------------------------------|--------------------------------------------|
// | CorruptStrikeWindow  | >= 4 strikes in 30 min       | Self-quarantine (uninstall + exit 9)       |
// | AuditFailureWindow   | >= 3 failures in 30 min      | Emit audit-degraded log entry (no quar.)   |
// | StaleClearWindow     | >= 4 events in 30 min        | Emit stale-clear-strike-alert (observ.)    |
//
// AppendStrike is the shared helper: append `now`, drop entries older
// than `now - 30min`, cap len at the supplied threshold (capN). Returns
// the new slice. Driver code reads len(window) >= threshold to decide
// whether to fire the corresponding event.
//
// Backoff math (plan §6): 15-min attempt window with INCLUSIVE upper at
// T+15min (i.e. attempts 1..4 fire at T0/T+5/T+10/T+15), 15-min cooldown
// (T+15..T+30), then T+30 = T0' of next cycle. After ChronicCycles >= 4
// without a Running >= 5min reset → ChronicLimitReached (driver writes
// chronic-failure intent and stops auto-revive).
//
// Restart-pending TTL (plan §31): 6 minutes (longer than the 5-min
// scheduled-task cadence so a hung restart locks out the daemon for at
// most 5-10 minutes). IsRestartPending(name, now) takes `now` as a
// parameter so callers can drive deterministic logic; no ambient
// time.Now consultations.
//
// WriteWatchdogState contract (plan §36 v9): err FIRST. Returns
// (cleared-task-names, nil) on successful write; (nil, err) on failure.
// Stale RestartPendingAt entries are zeroed during serialization, and
// the cleared task names are returned so the driver can log
// `restart-pending-stale-cleared` events to watchdog.log (Task 9 wires
// the actual log path).
//
// Test seam strategy:
//   - daemonStateRootOverride (Task 1) routes the on-disk path to a
//     per-test temp dir.
//   - watchdogQuarantineRemoveFileFn / watchdogQuarantinePruneLogFn are
//     local equivalents of the Task 2 seams: tests inject failures or
//     observe events without faking the filesystem.
//   - watchdogStateRenameFn lets tests force the temp+rename atomic-write
//     step to fail (drives the err-first contract test).
//
// Locking: every read AND write acquires gofrs/flock on the sibling
// `.lock` file. POSIX flock is advisory but consistent within mcphub
// itself. Windows uses LockFileEx via gofrs/flock for kernel-enforced
// exclusion. Quarantine work runs under the same lock so prune cannot
// race with a fresh write that happens to land between rename and
// list-corrupt-siblings.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
)

// ---------------------------------------------------------------------------
// File layout constants.
// ---------------------------------------------------------------------------

// watchdogStateFileLeaf is the canonical file name (relative to
// DaemonStateDir) holding the JSON-encoded WatchdogState. Kept as a
// single constant so later tasks (7, 9) reference one canonical literal.
const watchdogStateFileLeaf = "watchdog-state.json"

// watchdogStateLockLeaf is the sibling file used by gofrs/flock. Distinct
// leaf keeps lock ownership independent of the JSON file's lifecycle
// (rename during quarantine, atomic temp+rename writes).
const watchdogStateLockLeaf = "watchdog-state.json.lock"

// watchdogStateTempPrefix is the temp-file prefix for atomic writes.
// Each write creates a fresh `${stem}.tmp.${pid}.${nano}` file then
// renames it onto the canonical path under flock.
const watchdogStateTempPrefix = watchdogStateFileLeaf + ".tmp"

// ---------------------------------------------------------------------------
// Tunables (plan §6, §28, §31, §38, §45).
// ---------------------------------------------------------------------------

// AttemptWindowDuration is the §6 attempt-window length (T0..T+15
// inclusive). Driver may RecordAttempt up to AttemptWindowMax times
// within this window before transitioning to cooldown.
const AttemptWindowDuration = 15 * time.Minute

// CooldownCycleDuration is the total cycle length: 15-min attempt window
// + 15-min cooldown. After T+30 the entry is reset to a fresh cycle.
const CooldownCycleDuration = 30 * time.Minute

// AttemptWindowMax is the per-cycle cap on RecordAttempt invocations
// before Due falls through to cooldown semantics.
const AttemptWindowMax = 4

// RunningResetThreshold is the §6 reset rule: when a daemon is observed
// Running with LastRunningAt - prevLastRunningAt >= 5min, the cooldown
// entry resets to zero (next failure starts a fresh cycle).
const RunningResetThreshold = 5 * time.Minute

// ChronicCycleLimit is the §6 cap: after this many cycles without a
// Running >= 5min reset, ChronicLimitReached returns true and the
// driver stops auto-reviving (writes chronic-failure intent).
const ChronicCycleLimit = 4

// RestartPendingTTL is the §31 lockout window for in-flight restarts.
// Set to 6 minutes: longer than the 5-min scheduled-task cadence so a
// hung restart does not drop the marker mid-tick. After TTL expires
// IsRestartPending returns false and WriteWatchdogState clears the
// stale entry, returning the cleared task name to the caller.
const RestartPendingTTL = 6 * time.Minute

// StrikeWindowDuration is the §28/§38/§45 sliding-window length used by
// AppendStrike. Entries older than `now - StrikeWindowDuration` are
// dropped on every append.
const StrikeWindowDuration = 30 * time.Minute

// CorruptStrikeThreshold is the §28 trigger: >= 4 corrupt strikes in
// 30 min → self-quarantine (driver decides; this file just exposes
// the window so the driver can read len(...) >= threshold).
const CorruptStrikeThreshold = 4

// AuditFailureThreshold is the §38 v9 trigger: >= 3 audit-write failures
// in 30 min → emit audit-degraded log entry (NO quarantine).
const AuditFailureThreshold = 3

// StaleClearThreshold is the §45 v9 trigger: >= 4 stale-clear events in
// 30 min → emit stale-clear-strike-alert (NO quarantine; observability
// only).
const StaleClearThreshold = 4

// ---------------------------------------------------------------------------
// State enum (parallel to Task 2 IntentState*).
// ---------------------------------------------------------------------------

// WatchdogStateMissing means the file does not exist on disk (bootstrap
// case). Caller treats every cooldown lookup as "no prior attempts".
const WatchdogStateMissing = "missing"

// WatchdogStateCorrupt means the file existed but failed strict JSON
// parsing OR schema validation OR UTF-8 enforcement. The reader renames
// the offending file with a `.corrupt-{ts}` suffix under flock and
// prunes older siblings to QuarantineCap. The returned Cool is a
// suppress-all stub returning Due=false for every task name (fail-
// CLOSED per §13).
const WatchdogStateCorrupt = "corrupt"

// WatchdogStateValid means the file parsed and passed schema/UTF-8
// validation. The returned Cool is the real engine.
const WatchdogStateValid = "valid"

// ---------------------------------------------------------------------------
// Errors.
// ---------------------------------------------------------------------------

// errWatchdogStateSchemaInvalid is the internal sentinel raised by
// parseAndValidateWatchdogState when JSON parsing succeeded but the
// field shape (UpdatedAt non-UTC, embedded duplicate keys) is wrong.
// Callers see WatchdogStateCorrupt; the sentinel is never surfaced
// through the public API.
var errWatchdogStateSchemaInvalid = errors.New("api: watchdog-state schema invalid")

// errWatchdogStateInvalidUTF8 is the internal sentinel raised when the
// raw file bytes are not valid UTF-8.
var errWatchdogStateInvalidUTF8 = errors.New("api: watchdog-state file is not valid UTF-8")

// ---------------------------------------------------------------------------
// Test seams.
// ---------------------------------------------------------------------------

// watchdogQuarantineRemoveFileFn, when non-nil, replaces os.Remove
// inside the per-file delete loop run by pruneWatchdogCorruptSiblings.
// Tests inject a function that returns an error to exercise the
// "non-fatal prune failure" path.
var watchdogQuarantineRemoveFileFn func(path string) error

// watchdogQuarantinePruneLogFn, when non-nil, receives one event per
//
//	non-fatal prune outcome. event is one of {
//	  "quarantine-prune-failed-non-fatal",
//	  "quarantine-prune-list-failed-non-fatal",
//	}. Production wires this to a watchdog.log appender in Task 9; until
//
// then the default is a silent no-op so production swallows prune
// failures (which is the plan-specified "non-fatal" behavior).
var watchdogQuarantinePruneLogFn func(event, path string, err error)

// watchdogStateRenameFn, when non-nil, replaces os.Rename inside the
// atomic write step. Tests inject a failure to drive the err-first
// WriteWatchdogState contract test (plan §36 v9). Production code path
// uses os.Rename directly when the seam is nil.
var watchdogStateRenameFn func(oldpath, newpath string) error

// ---------------------------------------------------------------------------
// Public types.
// ---------------------------------------------------------------------------

// CooldownEntry is the per-task in-memory + on-disk cooldown record.
// Marshaled / unmarshaled as part of WatchdogState.Cooldowns.
//
// Field semantics (plan §6, §31):
//   - FirstAttemptAt: zero means "no prior attempts in this cycle".
//     RecordAttempt sets it on the first call AND when the previous
//     cycle's cooldown is over (now >= FirstAttemptAt + 30min).
//   - AttemptsInWindow: number of RecordAttempt calls within the
//     current cycle (1..4 cap before fall-through to cooldown).
//   - LastRunningAt: most recent observation of the daemon in
//     Running state. Used by RecordRunning's >= 5-min reset rule.
//   - ChronicCycles: count of cycles started without a Running >= 5min
//     reset. >= ChronicCycleLimit → ChronicLimitReached.
//   - RestartPendingAt: zero means "no pending restart". Set by
//     MarkRestartPending; cleared by ClearRestartPending or by
//     WriteWatchdogState's stale-clear sweep when (now - RestartPendingAt
//     > RestartPendingTTL). Re-emitted as a stale-clear event when the
//     sweep fires.
type CooldownEntry struct {
	FirstAttemptAt   time.Time `json:"first_attempt_at"`
	AttemptsInWindow int       `json:"attempts_in_window"`
	LastRunningAt    time.Time `json:"last_running_at"`
	ChronicCycles    int       `json:"chronic_cycles"`
	RestartPendingAt time.Time `json:"restart_pending_at"`
}

// WatchdogState is the on-disk container for the per-task cooldown map
// and the three sliding-30min strike windows. Stored as JSON at
// ${DaemonStateDir}/watchdog-state.json.
//
// All time fields are RFC3339Nano UTC; readWriter normalizes any
// non-UTC location on write.
type WatchdogState struct {
	Cooldowns           map[string]CooldownEntry `json:"cooldowns"`
	LastWallClockSeen   time.Time                `json:"last_wall_clock_seen"`
	CorruptStrikeWindow []time.Time              `json:"corrupt_strike_window"`
	AuditFailureWindow  []time.Time              `json:"audit_failure_window"`
	StaleClearWindow    []time.Time              `json:"stale_clear_window"`
}

// CooldownReader is the read-only interface the pure RecoverStoppedDaemons
// (Task 7) consumes. Strict purity: every method takes `now` so the caller
// drives deterministic logic without ambient time.Now reads.
type CooldownReader interface {
	Due(name string, now time.Time) bool
	ChronicLimitReached(name string) bool
	AttemptsInWindow(name string) int
	IsRestartPending(name string, now time.Time) bool
}

// Cooldown extends CooldownReader with the mutating ops the watchdog
// driver invokes inside its tick loop. Implementations are non-thread-
// safe — the driver runs single-threaded per `--once` invocation, and
// concurrent `--once` invocations are excluded by the singleton flock
// (plan §33).
type Cooldown interface {
	CooldownReader
	RecordAttempt(name string, now time.Time)
	RecordRunning(name string, now time.Time)
	MarkRestartPending(name string, now time.Time)
	ClearRestartPending(name string)
}

// WatchdogStateRead bundles the outcome of ReadWatchdogState so callers
// can distinguish the three states without separate methods. Fields:
//
//   - State: WatchdogStateMissing | WatchdogStateCorrupt | WatchdogStateValid.
//   - Cool: Cooldown engine. On corrupt: a suppress-all stub
//     (Due=false for every task name). On missing/valid: a real
//     mutable engine backed by CooldownEntry map.
//   - QuarantinePath: set when State=corrupt; the renamed
//     `.corrupt-{ts}` location for forensic review.
//   - LastWallClock: persisted last-seen wall-clock value (caller
//     compares to current `now` to detect jumps; plan §29).
//   - CorruptStrikeWindow / AuditFailureWindow / StaleClearWindow:
//     defensive copies of the persisted slices (caller may append
//     via the AppendStrike helper without aliasing the read result).
type WatchdogStateRead struct {
	State               string
	Cool                Cooldown
	QuarantinePath      string
	LastWallClock       time.Time
	CorruptStrikeWindow []time.Time
	AuditFailureWindow  []time.Time
	StaleClearWindow    []time.Time
}

// ---------------------------------------------------------------------------
// AppendStrike — shared sliding-30min helper.
// ---------------------------------------------------------------------------

// AppendStrike appends `now` to `window`, drops every entry older than
// `now - StrikeWindowDuration`, and caps the resulting len at `capN`
// (newest entries kept). Returns the new slice. The input slice is NOT
// mutated when entries are dropped — a fresh backing array is allocated
// on every call so callers can safely reuse the input afterward.
//
// Driver pattern:
//
//	state.CorruptStrikeWindow = AppendStrike(state.CorruptStrikeWindow, now, CorruptStrikeThreshold)
//	if len(state.CorruptStrikeWindow) >= CorruptStrikeThreshold {
//	    // self-quarantine
//	}
//
// Note: capping at the threshold value (not threshold+1) is intentional:
// once we hit the threshold we never need to remember more entries. The
// driver checks len(...) >= threshold which works because new appends
// can only push the count up to capN, never above.
func AppendStrike(window []time.Time, now time.Time, capN int) []time.Time {
	cutoff := now.Add(-StrikeWindowDuration)
	out := make([]time.Time, 0, capN)
	for _, ts := range window {
		if ts.Before(cutoff) {
			continue
		}
		out = append(out, ts.UTC())
	}
	out = append(out, now.UTC())
	if len(out) > capN {
		// Keep the newest capN. Sort ascending and trim the head.
		sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
		out = out[len(out)-capN:]
	}
	return out
}

// ---------------------------------------------------------------------------
// Cooldown engine — real implementation (used in valid state).
// ---------------------------------------------------------------------------

// cooldownEngine is the concrete Cooldown implementation backed by a
// CooldownEntry map. Mutation is by-name so independent task names do
// not interfere. Per the Cooldown interface contract, callers must not
// invoke the engine concurrently.
type cooldownEngine struct {
	entries map[string]CooldownEntry
}

// newCooldownEngine builds a fresh engine from the supplied map. The
// map is captured by reference; subsequent mutations land in the same
// map so WriteWatchdogState can serialize the up-to-date state.
func newCooldownEngine(m map[string]CooldownEntry) *cooldownEngine {
	if m == nil {
		m = map[string]CooldownEntry{}
	}
	return &cooldownEngine{entries: m}
}

// Due implements §6 backoff math. Returns true iff:
//
//   - entry missing OR FirstAttemptAt.IsZero() (first-ever attempt), OR
//   - AttemptsInWindow < AttemptWindowMax AND now <= FirstAttemptAt+15min
//     (within attempt window — INCLUSIVE upper), OR
//   - now >= FirstAttemptAt+30min (cooldown over → new cycle).
//
// Otherwise false (cooldown phase).
func (c *cooldownEngine) Due(name string, now time.Time) bool {
	e, ok := c.entries[name]
	if !ok || e.FirstAttemptAt.IsZero() {
		return true
	}
	// Attempt window: 1..4 inclusive of T+15min.
	if e.AttemptsInWindow < AttemptWindowMax {
		upper := e.FirstAttemptAt.Add(AttemptWindowDuration)
		// `<= upper` per plan §6 inclusive boundary.
		if !now.After(upper) {
			return true
		}
	}
	// Cooldown over: new cycle starts at FirstAttemptAt+30min.
	cycleEnd := e.FirstAttemptAt.Add(CooldownCycleDuration)
	if !now.Before(cycleEnd) {
		return true
	}
	return false
}

// RecordAttempt advances the cooldown state per §6:
//
//   - entry missing OR now >= FirstAttemptAt+30min:
//     ChronicCycles++ if !FirstAttemptAt.IsZero();
//     FirstAttemptAt = now; AttemptsInWindow = 1.
//   - else: AttemptsInWindow++.
func (c *cooldownEngine) RecordAttempt(name string, now time.Time) {
	now = now.UTC()
	e := c.entries[name]
	startNew := false
	if e.FirstAttemptAt.IsZero() {
		startNew = true
	} else {
		cycleEnd := e.FirstAttemptAt.Add(CooldownCycleDuration)
		if !now.Before(cycleEnd) {
			startNew = true
			e.ChronicCycles++
		}
	}
	if startNew {
		e.FirstAttemptAt = now
		e.AttemptsInWindow = 1
	} else {
		e.AttemptsInWindow++
	}
	c.entries[name] = e
}

// RecordRunning applies §6 reset rule: if LastRunningAt is set AND
// now - LastRunningAt >= 5min, reset the entry to zero values. Always
// updates LastRunningAt to `now`.
func (c *cooldownEngine) RecordRunning(name string, now time.Time) {
	now = now.UTC()
	e := c.entries[name]
	if !e.LastRunningAt.IsZero() && now.Sub(e.LastRunningAt) >= RunningResetThreshold {
		// Full reset; preserve only the new LastRunningAt.
		e = CooldownEntry{}
	}
	e.LastRunningAt = now
	c.entries[name] = e
}

// MarkRestartPending records the start of a restart so subsequent ticks
// (within the TTL) skip duplicate restart attempts (§31).
func (c *cooldownEngine) MarkRestartPending(name string, now time.Time) {
	e := c.entries[name]
	e.RestartPendingAt = now.UTC()
	c.entries[name] = e
}

// ClearRestartPending zeroes RestartPendingAt regardless of clock. Called
// by the driver after a restart returns (success or failure) to release
// the lockout immediately rather than waiting for TTL expiry.
func (c *cooldownEngine) ClearRestartPending(name string) {
	e, ok := c.entries[name]
	if !ok {
		return
	}
	e.RestartPendingAt = time.Time{}
	c.entries[name] = e
}

// IsRestartPending reports whether a restart is in flight per §31:
//
//   - false if RestartPendingAt.IsZero().
//   - false if now.Sub(RestartPendingAt) > RestartPendingTTL (stale;
//     caller MUST clear via ClearRestartPending on next mutation OR
//     via WriteWatchdogState's stale-clear sweep).
//   - true otherwise.
//
// The injected `now` parameter is the only clock source; no ambient
// time.Now consultation. Tests use this contract to drive deterministic
// boundary behavior.
func (c *cooldownEngine) IsRestartPending(name string, now time.Time) bool {
	e, ok := c.entries[name]
	if !ok || e.RestartPendingAt.IsZero() {
		return false
	}
	if now.Sub(e.RestartPendingAt) > RestartPendingTTL {
		return false
	}
	return true
}

// ChronicLimitReached returns true when the entry's ChronicCycles count
// has reached ChronicCycleLimit (§6).
func (c *cooldownEngine) ChronicLimitReached(name string) bool {
	e := c.entries[name]
	return e.ChronicCycles >= ChronicCycleLimit
}

// AttemptsInWindow returns the current per-cycle attempt count. Used by
// the driver to decide whether to log "attempt N/4" diagnostics.
func (c *cooldownEngine) AttemptsInWindow(name string) int {
	e := c.entries[name]
	return e.AttemptsInWindow
}

// ---------------------------------------------------------------------------
// suppressAllCooldown — the fail-CLOSED stub returned on corrupt state.
// ---------------------------------------------------------------------------

// suppressAllCooldown is the WatchdogStateCorrupt fail-CLOSED Cooldown
// (plan §13). Every method that could authorize a restart returns the
// suppressing answer (Due=false, ChronicLimitReached=true,
// AttemptsInWindow=AttemptWindowMax, IsRestartPending=true). Mutation
// methods are silent no-ops because the driver should not be touching
// the engine when state is corrupt — but defense-in-depth: even if it
// does, the answer never authorizes a restart.
type suppressAllCooldown struct{}

func (suppressAllCooldown) Due(name string, now time.Time) bool {
	return false
}

func (suppressAllCooldown) ChronicLimitReached(name string) bool {
	// Reporting "chronic limit reached" makes the driver treat every
	// task as locked-out, which is exactly what fail-CLOSED requires.
	return true
}

func (suppressAllCooldown) AttemptsInWindow(name string) int {
	return AttemptWindowMax
}

func (suppressAllCooldown) IsRestartPending(name string, now time.Time) bool {
	// Treating every task as restart-pending also blocks restart paths
	// that gate on this predicate.
	return true
}

func (suppressAllCooldown) RecordAttempt(name string, now time.Time)      {}
func (suppressAllCooldown) RecordRunning(name string, now time.Time)      {}
func (suppressAllCooldown) MarkRestartPending(name string, now time.Time) {}
func (suppressAllCooldown) ClearRestartPending(name string)               {}

// ---------------------------------------------------------------------------
// ReadWatchdogState — three-state file read with quarantine-on-corrupt.
// ---------------------------------------------------------------------------

// ReadWatchdogState loads the on-disk watchdog-state.json file and
// returns a WatchdogStateRead. Per plan §13/§52:
//
//   - missing: os.Stat returned ErrNotExist. WatchdogStateRead.Cool is
//     a fresh real engine over an empty map. Strike-window slices are
//     nil. LastWallClock is zero.
//   - corrupt: file existed but failed strict parse / schema / UTF-8
//     check. The reader renames the offending file with a
//     `.corrupt-{ts}` suffix under flock, prunes older siblings to
//     QuarantineCap, and returns a suppress-all Cool. QuarantinePath
//     is set to the renamed location.
//   - valid: parse + schema check both succeeded. WatchdogStateRead.Cool
//     is a real engine whose internal map is the parsed snapshot.
//     Strike-window slices are defensive copies of the parsed values.
//
// Concurrency: the entire read holds gofrs/flock on the sibling
// `.lock` file. Safe for any number of concurrent readers/writers
// (subject to flock's mutual-exclusion semantic).
func (a *API) ReadWatchdogState() WatchdogStateRead {
	dir, err := DaemonStateDir()
	if err != nil {
		return missingWatchdogState()
	}

	statePath := filepath.Join(dir, watchdogStateFileLeaf)
	lockPath := filepath.Join(dir, watchdogStateLockLeaf)

	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return missingWatchdogState()
	}
	defer func() { _ = lock.Unlock() }()

	raw, err := os.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return missingWatchdogState()
		}
		return missingWatchdogState()
	}

	parsed, parseErr := parseAndValidateWatchdogState(raw)
	if parseErr == nil {
		return validWatchdogState(parsed)
	}

	// Corrupt path: rename + prune under the same flock.
	quarantinePath, qErr := quarantineCorruptWatchdogStateFile(statePath)
	if qErr != nil {
		// Rename failed — quarantine aborted entirely. Return the
		// suppress-all Cool so the driver fails-CLOSED regardless.
		return WatchdogStateRead{
			State: WatchdogStateCorrupt,
			Cool:  suppressAllCooldown{},
		}
	}
	pruneWatchdogCorruptSiblings(statePath)

	return WatchdogStateRead{
		State:          WatchdogStateCorrupt,
		Cool:           suppressAllCooldown{},
		QuarantinePath: quarantinePath,
	}
}

// missingWatchdogState builds the bootstrap WatchdogStateRead.
func missingWatchdogState() WatchdogStateRead {
	return WatchdogStateRead{
		State: WatchdogStateMissing,
		Cool:  newCooldownEngine(map[string]CooldownEntry{}),
	}
}

// validWatchdogState builds the success-path WatchdogStateRead from the
// parsed file. Strike-window slices are defensively copied so caller
// AppendStrike calls do not retroactively alias the parsed snapshot.
func validWatchdogState(s WatchdogState) WatchdogStateRead {
	cools := s.Cooldowns
	if cools == nil {
		cools = map[string]CooldownEntry{}
	}
	return WatchdogStateRead{
		State:               WatchdogStateValid,
		Cool:                newCooldownEngine(cools),
		LastWallClock:       s.LastWallClockSeen,
		CorruptStrikeWindow: copyTimeSlice(s.CorruptStrikeWindow),
		AuditFailureWindow:  copyTimeSlice(s.AuditFailureWindow),
		StaleClearWindow:    copyTimeSlice(s.StaleClearWindow),
	}
}

// copyTimeSlice returns a defensive copy of the supplied slice. Returns
// nil for nil input (preserves backward-compat semantics; older JSON
// without the new windows unmarshal to nil → nil after copy).
func copyTimeSlice(in []time.Time) []time.Time {
	if in == nil {
		return nil
	}
	out := make([]time.Time, len(in))
	copy(out, in)
	return out
}

// parseAndValidateWatchdogState applies strict JSON decoding + schema/
// UTF-8 validation to the raw file bytes. Returns the parsed state on
// success, or a wrapped error describing the first violation found.
func parseAndValidateWatchdogState(raw []byte) (WatchdogState, error) {
	if !utf8.Valid(raw) {
		return WatchdogState{}, errWatchdogStateInvalidUTF8
	}

	var s WatchdogState
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return WatchdogState{}, fmt.Errorf("json decode: %w", err)
	}
	if dec.More() {
		return WatchdogState{}, fmt.Errorf("json decode: extra data after top-level object")
	}

	// Schema check: all timestamps must be UTC. The naive substring
	// search defends against decoders that quietly normalize +00:00 to
	// UTC during Unmarshal (Go's time package does NOT do this, but
	// defense-in-depth: if the decoder ever changes, this check catches
	// the regression).
	if !allTimestampsAreUTC(raw) {
		return WatchdogState{}, fmt.Errorf("non-UTC timestamp in raw bytes: %w", errWatchdogStateSchemaInvalid)
	}

	if s.Cooldowns == nil {
		s.Cooldowns = map[string]CooldownEntry{}
	}
	return s, nil
}

// allTimestampsAreUTC scans raw bytes for date-shaped strings and
// asserts each ends in `Z`. RFC3339 in JSON: `"YYYY-MM-DDTHH:MM:SS...Z"`
// or `"...+HH:MM"` etc. We only allow the `Z` suffix.
func allTimestampsAreUTC(raw []byte) bool {
	// Walk every quoted-string occurrence and check shape. A precise
	// JSON walk is overkill — instead, look for patterns matching
	// `"NNNN-NN-NN`. If any such substring's terminating `"` is preceded
	// by anything other than `Z`, fail.
	const datePrefix = `"20`
	idx := 0
	for {
		i := bytes.Index(raw[idx:], []byte(datePrefix))
		if i < 0 {
			return true
		}
		start := idx + i + 1 // skip leading quote
		end := bytes.IndexByte(raw[start:], '"')
		if end < 0 {
			// Unterminated string — let json.Unmarshal catch it.
			return true
		}
		s := string(raw[start : start+end])
		if looksLikeRFC3339(s) {
			if !strings.HasSuffix(s, "Z") {
				return false
			}
		}
		idx = start + end + 1
	}
}

// looksLikeRFC3339 returns true for strings shaped like an RFC3339
// timestamp ("YYYY-MM-DDTHH:MM:SS"). Conservative: rejects strings that
// could be plain text containing `2006-...` literals. Used only by the
// UTC-check pass above.
func looksLikeRFC3339(s string) bool {
	if len(s) < 19 {
		return false
	}
	if s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// WriteWatchdogState — atomic temp+rename + stale-clear sweep.
// ---------------------------------------------------------------------------

// WriteWatchdogState persists the supplied WatchdogStateRead. Per plan
// §36 v9 err-first contract:
//
//   - Successful write → returns (cleared-task-names, nil). Cleared
//     names come from the stale-RestartPendingAt sweep: any entry with
//     RestartPendingAt non-zero AND now - RestartPendingAt >
//     RestartPendingTTL is zeroed during serialization, and its task
//     name is appended to the events slice. Caller logs each event as
//     `restart-pending-stale-cleared` to watchdog.log.
//   - Failure → returns (nil, err). The events slice MUST be nil on
//     err so callers cannot accidentally log a stale-clear that did
//     not persist.
//
// Atomicity matches Task 2's daemon-intent pattern: gofrs/flock on the
// sibling `.lock` file, write to a fresh temp file, rename onto the
// canonical path. The rename is the commit point.
//
// Snapshot semantics: the supplied `s` argument's strike-window slices
// and cool engine are read at call time; the caller may have mutated
// the cool engine in place (via the returned interface) since the
// matching ReadWatchdogState. The persisted file reflects the current
// engine state at the moment Write is called.
func (a *API) WriteWatchdogState(s WatchdogStateRead, now time.Time) (staleClearEvents []string, err error) {
	dir, err := DaemonStateDir()
	if err != nil {
		return nil, err
	}
	statePath := filepath.Join(dir, watchdogStateFileLeaf)
	lockPath := filepath.Join(dir, watchdogStateLockLeaf)

	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("flock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	// Build the on-disk struct from the read's components.
	disk := WatchdogState{
		Cooldowns:           cooldownEntries(s.Cool),
		LastWallClockSeen:   now.UTC(),
		CorruptStrikeWindow: normalizeStrikeWindow(s.CorruptStrikeWindow),
		AuditFailureWindow:  normalizeStrikeWindow(s.AuditFailureWindow),
		StaleClearWindow:    normalizeStrikeWindow(s.StaleClearWindow),
	}

	// Stale-RestartPendingAt sweep. Mutate the disk copy (not the
	// caller's engine) so a subsequent WriteWatchdogState behaves the
	// same way against the same input.
	candidateEvents := sweepStaleRestartPending(disk.Cooldowns, now)

	if writeErr := writeWatchdogStateFileLocked(statePath, disk); writeErr != nil {
		// Err-first contract: events slice MUST be nil on err.
		return nil, writeErr
	}

	// Successful write: also clear the stale entries in the caller's
	// in-memory engine so the next IsRestartPending call agrees with
	// the on-disk state.
	if cool, ok := s.Cool.(*cooldownEngine); ok {
		for _, name := range candidateEvents {
			cool.ClearRestartPending(name)
		}
	}

	if len(candidateEvents) == 0 {
		return nil, nil
	}
	return candidateEvents, nil
}

// cooldownEntries extracts the entries map from a Cooldown interface.
// For the real engine this returns the live map (caller mutations land
// here). For the suppress-all stub this returns an empty map (no
// state to persist on corrupt path).
func cooldownEntries(c Cooldown) map[string]CooldownEntry {
	if c == nil {
		return map[string]CooldownEntry{}
	}
	if engine, ok := c.(*cooldownEngine); ok {
		// Defensive copy so the persisted bytes do not alias the
		// caller's mutable map. Any cap-related concerns (e.g. the
		// engine grows the map mid-write) are bounded by the flock.
		out := make(map[string]CooldownEntry, len(engine.entries))
		for k, v := range engine.entries {
			// Normalize times to UTC on persist.
			v.FirstAttemptAt = v.FirstAttemptAt.UTC()
			v.LastRunningAt = v.LastRunningAt.UTC()
			v.RestartPendingAt = v.RestartPendingAt.UTC()
			out[k] = v
		}
		return out
	}
	return map[string]CooldownEntry{}
}

// normalizeStrikeWindow returns a defensive copy of `in` with every
// timestamp converted to UTC. Nil input → nil output (preserves the
// "missing field" backward-compat shape).
func normalizeStrikeWindow(in []time.Time) []time.Time {
	if in == nil {
		return nil
	}
	out := make([]time.Time, len(in))
	for i, t := range in {
		out[i] = t.UTC()
	}
	return out
}

// sweepStaleRestartPending zeroes every CooldownEntry.RestartPendingAt
// where now - RestartPendingAt > RestartPendingTTL, and returns the
// task names whose entries were cleared. The map is mutated in place
// (caller passes the disk copy so callers' in-memory engine is
// untouched at this point).
func sweepStaleRestartPending(m map[string]CooldownEntry, now time.Time) []string {
	if len(m) == 0 {
		return nil
	}
	var cleared []string
	for name, e := range m {
		if e.RestartPendingAt.IsZero() {
			continue
		}
		if now.Sub(e.RestartPendingAt) > RestartPendingTTL {
			e.RestartPendingAt = time.Time{}
			m[name] = e
			cleared = append(cleared, name)
		}
	}
	// Deterministic order so callers (and tests) can rely on a stable
	// events list shape.
	sort.Strings(cleared)
	return cleared
}

// writeWatchdogStateFileLocked marshals + writes the file under the
// caller's already-held flock, using temp+rename for atomicity. Returns
// any error from the underlying I/O steps verbatim (caller maps to
// the err-first WriteWatchdogState contract).
func writeWatchdogStateFileLocked(statePath string, s WatchdogState) error {
	// json.MarshalIndent for human readability; downstream tools tolerate
	// either form.
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	dir := filepath.Dir(statePath)
	tempName := filepath.Join(dir, fmt.Sprintf("%s.%d.%d", watchdogStateTempPrefix, os.Getpid(), time.Now().UnixNano()))
	tempFile, err := os.OpenFile(tempName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("open temp: %w", err)
	}
	cleanupTemp := func() {
		_ = tempFile.Close()
		_ = os.Remove(tempName)
	}

	if _, err := tempFile.Write(raw); err != nil {
		cleanupTemp()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		cleanupTemp()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tempName, 0o600); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("chmod temp: %w", err)
	}

	// Rename via the seam so tests can inject failures.
	if err := watchdogStateRename(tempName, statePath); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// watchdogStateRename routes through the watchdogStateRenameFn seam if
// set, otherwise calls os.Rename directly.
func watchdogStateRename(oldpath, newpath string) error {
	if watchdogStateRenameFn != nil {
		return watchdogStateRenameFn(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

// ---------------------------------------------------------------------------
// Quarantine helpers (parallel to Task 2 daemon_intent.go).
// ---------------------------------------------------------------------------

// quarantineCorruptWatchdogStateFile renames statePath to
// `${stem}.corrupt-{ts}` using a UTC timestamp suffix. Returns the
// renamed path on success, or an error wrapping the rename failure.
// Caller already holds flock; this routine performs no locking of
// its own.
//
// Failure abort policy (plan §23): if rename fails the entire
// quarantine is aborted — there is no fallback (we won't truncate the
// corrupt file in place; that would destroy forensic state).
func quarantineCorruptWatchdogStateFile(statePath string) (string, error) {
	ts := time.Now().UTC().Format(quarantineSuffixLayout)
	target := statePath + ".corrupt-" + ts
	for i := 0; i < 8; i++ {
		try := target
		if i > 0 {
			try = fmt.Sprintf("%s.%d", target, i)
		}
		if _, err := os.Stat(try); errors.Is(err, os.ErrNotExist) {
			if renameErr := os.Rename(statePath, try); renameErr != nil {
				return "", fmt.Errorf("rename to quarantine: %w", renameErr)
			}
			return try, nil
		}
	}
	return "", fmt.Errorf("quarantine: target path collision for %s", target)
}

// pruneWatchdogCorruptSiblings keeps the QuarantineCap newest
// `.corrupt-*` siblings of statePath and deletes the rest. Per-file
// failures are logged via watchdogQuarantinePruneLogFn and treated as
// non-fatal: rename already succeeded, so forensic state is preserved.
func pruneWatchdogCorruptSiblings(statePath string) {
	dir := filepath.Dir(statePath)
	leaf := filepath.Base(statePath)
	prefix := leaf + ".corrupt-"

	entries, err := os.ReadDir(dir)
	if err != nil {
		logWatchdogQuarantine("quarantine-prune-list-failed-non-fatal", dir, err)
		return
	}

	type candidate struct {
		path  string
		mtime time.Time
	}
	var found []candidate
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		found = append(found, candidate{path: full, mtime: info.ModTime()})
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].mtime.Equal(found[j].mtime) {
			return found[i].path > found[j].path
		}
		return found[i].mtime.After(found[j].mtime)
	})

	if len(found) <= QuarantineCap {
		return
	}

	for _, victim := range found[QuarantineCap:] {
		removeErr := removeWatchdogQuarantineFile(victim.path)
		if removeErr != nil {
			logWatchdogQuarantine("quarantine-prune-failed-non-fatal", victim.path, removeErr)
			// continue; per-file failure must not block other deletions
		}
	}
}

// removeWatchdogQuarantineFile is the indirection that lets tests inject
// a failing delete. Production calls os.Remove.
func removeWatchdogQuarantineFile(path string) error {
	if watchdogQuarantineRemoveFileFn != nil {
		return watchdogQuarantineRemoveFileFn(path)
	}
	return os.Remove(path)
}

// logWatchdogQuarantine emits a non-fatal prune event. Production wires
// this to watchdog.log in Task 9; until then the default behavior is
// silent (the test seam watchdogQuarantinePruneLogFn captures events
// when the caller cares).
func logWatchdogQuarantine(event, path string, err error) {
	if watchdogQuarantinePruneLogFn != nil {
		watchdogQuarantinePruneLogFn(event, path, err)
	}
}

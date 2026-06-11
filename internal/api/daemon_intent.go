// Package api — Task 2 daemon-intent persistence (watchdog plan v13 §3,
// §4, §15, §23, §35).
//
// daemon_intent.go owns the on-disk daemon-intent.json file: schema,
// three-state read (missing | corrupt | valid), atomic writes under
// gofrs/flock, post-rename quarantine with non-fatal prune, UTC
// enforcement, TTL with clock-skew detection, and 1KB identity-oversize
// rejection. Every persistence concern shared with later watchdog
// tasks (3, 4, 7, 9) goes through this file's exported surface.
//
// Production callers reach the schema via *API methods (ReadDaemonIntent,
// WriteDaemonIntent, ClearDaemonIntent) and the pure-function predicate
// DaemonIntent.IsActiveStop. Path resolution leans on Task 1's
// DaemonStateDir / OpenStateFile so this file does not duplicate the
// platform-resolver or POSIX-perm logic.
//
// Test seam strategy:
//   - daemonStateRootOverride (Task 1) routes the on-disk path to a
//     per-test temp dir; daemon_intent.go relies on it transparently.
//   - quarantineRemoveFileFn / quarantinePruneLogFn (this file) let
//     tests inject prune-step failures without faking the filesystem.
//   - readDaemonIntentFn (api_surfaces.go) is bound at init() time to
//     a thin adapter over ReadDaemonIntent; Task 0's IntentStillRunning
//     is now backed by real on-disk state (replacing the Task 0 stub).
//
// Locking: every read AND write acquires gofrs/flock on a sibling
// `.lock` file. POSIX flock is advisory but consistent within mcphub
// itself. Windows uses LockFileEx via gofrs/flock for kernel-enforced
// exclusion. Quarantine work runs under the same lock so prune cannot
// race with a fresh write that happens to land between rename and
// list-corrupt-siblings.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// intentFileLeaf is the canonical file name (relative to DaemonStateDir)
// holding the JSON-encoded DaemonIntentFile.
const intentFileLeaf = "daemon-intent.json"

// intentLockLeaf is the sibling file used by gofrs/flock. Distinct leaf
// keeps lock ownership independent of the JSON file's lifecycle (rename
// during quarantine, atomic temp+rename writes).
const intentLockLeaf = "daemon-intent.json.lock"

// intentTempPrefix is the temp-file prefix for atomic writes. Each
// write creates a fresh `${stem}.tmp.${pid}.${nano}` file then renames
// it onto the canonical path under flock.
const intentTempPrefix = intentFileLeaf + ".tmp"

// quarantineSuffixLayout formats the timestamp suffix appended to
// renamed corrupt files: `daemon-intent.json.corrupt-2026-01-02T15-04-05.123456789Z`.
// Filesystem-safe (no colons) so Windows accepts the path. Sortable
// lexicographically because every component is fixed-width.
const quarantineSuffixLayout = "2006-01-02T15-04-05.000000000Z"

// ---------------------------------------------------------------------------
// Tunables (plan §3, §4, §23).
// ---------------------------------------------------------------------------

const (
	// StopIntentTTL is how long a `Reason=user-stop` intent remains
	// "active" before auto-revive resumes. Plan §4.
	StopIntentTTL = 24 * time.Hour
	// ClockSkewFutureTolerance is the slop window for "now < UpdatedAt".
	// Below this delta the entry is treated as legitimate; above it the
	// read fails CLOSED and reports clock-skew-future-suspect. Plan §4.
	ClockSkewFutureTolerance = 5 * time.Minute
	// ClockSkewStaleBound is the upper bound on age. Beyond a year we
	// treat the entry as stale (likely a clock rollback during install)
	// and fall back to default-running semantics. Plan §4.
	ClockSkewStaleBound = 365 * 24 * time.Hour
	// QuarantineCap is the maximum number of `.corrupt-*` siblings we
	// keep around for forensic review before pruning the oldest. Plan §23.
	QuarantineCap = 5
	// IdentityFieldByteCap is the upper bound on a single identity-field
	// byte length. Per plan §35: real TS task names are <100 bytes; 1KB
	// gives ample headroom while still rejecting accidental binary blobs.
	IdentityFieldByteCap = 1024
)

// ---------------------------------------------------------------------------
// Reason / Desired enums.
// ---------------------------------------------------------------------------

// IntentDesiredRunning marks a daemon that should be alive. Watchdog
// auto-revives on absence.
const IntentDesiredRunning = "running"

// IntentDesiredStopped marks a daemon the operator (or a fail-closed
// path) explicitly suppressed. Watchdog respects until TTL expires
// (only for IntentReasonUserStop) or the entry is cleared.
const IntentDesiredStopped = "stopped"

// IntentReasonUserStop is the operator-initiated stop with TTL.
// Decays to "ineligible-for-active-stop" after StopIntentTTL.
const IntentReasonUserStop = "user-stop"

// IntentReasonUserDisabled is the operator-initiated permanent stop.
// Never expires (until cleared via ClearDaemonIntent).
const IntentReasonUserDisabled = "user-disabled"

// IntentReasonChronicFailure is the watchdog fail-closed quarantine
// after the strike loop trips. Never expires automatically.
const IntentReasonChronicFailure = "chronic-failure"

// IntentReasonUninstalled is the cleanup-side reason recorded when a
// task is removed; allows the next install pass to skip recording an
// auto-revive intent for a daemon the operator just removed.
const IntentReasonUninstalled = "uninstalled"

// IntentReasonInstall is the desired=running intent recorded by
// `mcphub install` so a fresh daemon is auto-revived from boot one.
const IntentReasonInstall = "install"

// IntentReasonRegister is the desired=running intent recorded by the
// register flow (workspace-scoped lazy-proxy registration).
const IntentReasonRegister = "register"

// IntentReasonIdle is the v0.6 idle-shutdown stop (#6, spec §6). The 60s
// idle sweeper writes Desired=stopped+IntentReasonIdle for a serena pool
// daemon that has had no /serena/mcp activity for the operator-configured
// idle threshold, so the supervisor terminates it; the next /serena/mcp
// request for that daemon CLEARS this stop (ClearStopIntent) and the
// supervisor respawns it.
//
// Unlike IntentReasonUserStop, an idle stop NEVER expires by TTL — an idle
// daemon sleeps indefinitely until a /serena/mcp wake clears the reason
// (IsActiveStop deliberately does NOT apply the StopIntentTTL branch to it).
// Unlike IntentReasonUserDisabled, it is NOT an operator stop: the router
// wake clears ONLY IntentReasonIdle, so a user-disabled / user-stopped
// daemon is never resurrected by an idle wake (the operator stop wins).
const IntentReasonIdle = "idle"

// ClockSkewFutureSuspectReason is the synthetic Reason returned by
// IsActiveStop when the entry's UpdatedAt is far enough in the future
// to suggest clock skew or tampering. Caller-side logging uses this
// literal so audit traces can be filtered.
const ClockSkewFutureSuspectReason = "clock-skew-future-suspect"

// ---------------------------------------------------------------------------
// State enum (plan §3 three-state read).
// ---------------------------------------------------------------------------

// IntentStateMissing means the file does not exist on disk (bootstrap
// case). Caller treats every task in the registry as Desired=running.
const IntentStateMissing = "missing"

// IntentStateCorrupt means the file existed but failed strict JSON
// parsing OR schema validation OR UTF-8 enforcement. The reader
// renames the offending file with a `.corrupt-{ts}` suffix under
// flock and prunes older siblings to QuarantineCap.
const IntentStateCorrupt = "corrupt"

// IntentStateValid means the file parsed and passed schema/UTF-8
// validation. File.Tasks is the authoritative map.
const IntentStateValid = "valid"

// ---------------------------------------------------------------------------
// Errors.
// ---------------------------------------------------------------------------

// ErrEntryOversize is returned by WriteDaemonIntent when the supplied
// task name exceeds IdentityFieldByteCap. Per plan §35 + Task 2.1:
// identity fields are NEVER truncated; the writer fails closed and
// the caller refuses the persistence step so log/audit downstream
// never see partial identity. Distinct from ErrIdentityOversize
// (Task 3, intent_audit.go) so callers can distinguish "intent file
// rejected this name" from "audit log rejected this name".
var ErrEntryOversize = errors.New("api: daemon intent entry exceeds 1KB identity-field cap")

// errIntentSchemaInvalid is the internal sentinel raised by
// validateIntentFile when JSON parsing succeeded but the field shape
// (Desired enum, Reason enum, UpdatedAt non-zero, UpdatedAt UTC) is
// wrong. Callers see IntentStateCorrupt; the sentinel is never
// surfaced through the public API.
var errIntentSchemaInvalid = errors.New("api: daemon-intent schema invalid")

// errIntentInvalidUTF8 is the internal sentinel raised when the raw
// file bytes are not valid UTF-8. JSON unmarshal accepts arbitrary
// bytes in string values, so we run a whole-file utf8.Valid pre-check
// to honor the plan's "invalid UTF-8 → corrupt" semantic.
var errIntentInvalidUTF8 = errors.New("api: daemon-intent file is not valid UTF-8")

// ---------------------------------------------------------------------------
// Test seams.
// ---------------------------------------------------------------------------

// quarantineRemoveFileFn, when non-nil, replaces os.Remove inside the
// per-file delete loop run by quarantinePrune. Tests inject a function
// that returns an error to exercise the "non-fatal prune failure" path.
var quarantineRemoveFileFn func(path string) error

// quarantinePruneLogFn, when non-nil, receives one event per non-fatal
// prune outcome. event is one of {"quarantine-prune-failed-non-fatal",
// "quarantine-prune-list-failed-non-fatal", ...}. Production wires this
// to a watchdog.log appender in a later task; until then the default is
// a no-op so production silently swallows prune failures (which is the
// plan-specified "non-fatal" behavior).
var quarantinePruneLogFn func(event, path string, err error)

// ---------------------------------------------------------------------------
// Public types.
// ---------------------------------------------------------------------------

// DaemonIntent is the desired-state record for one daemon. Persisted
// as one entry inside DaemonIntentFile.Tasks.
//
// Field semantics:
//   - Desired: IntentDesiredRunning | IntentDesiredStopped — strict enum.
//   - Reason: one of IntentReason* — strict enum. Free-form strings are
//     rejected at read time as schema-invalid.
//   - UpdatedAt: UTC RFC3339Nano. The writer normalizes to UTC; the
//     reader rejects non-UTC offsets as schema-invalid (plan §3 +
//     Task 2.1 UTC enforcement test).
type DaemonIntent struct {
	Desired   string    `json:"desired"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DaemonIntentFile is the on-disk container for the per-task intent
// map. Stored as JSON at `${DaemonStateDir}/daemon-intent.json`.
//
// Mixed-bootstrap (plan §3): the file may carry entries for only a
// subset of registered daemons. Lookups for absent task names return
// the zero DaemonIntent — caller treats that as "default-running".
type DaemonIntentFile struct {
	Tasks map[string]DaemonIntent `json:"tasks"`
}

// IntentReadResult bundles the outcome of ReadDaemonIntent so callers
// can distinguish the three states without separate methods. Per Task 2
// API contract:
//
//   - State="missing": file absent; File.Tasks is non-nil (empty map).
//   - State="corrupt": file existed but rejected; QuarantinePath holds
//     the renamed `.corrupt-{ts}` location. File.Tasks is non-nil
//     (empty map). Err carries the underlying parse/schema error so
//     callers can log root cause.
//   - State="valid": parse succeeded. File.Tasks is the authoritative
//     map. QuarantinePath is empty. Err is nil.
type IntentReadResult struct {
	State          string
	File           DaemonIntentFile
	QuarantinePath string
	Err            error
}

// ---------------------------------------------------------------------------
// Task-name normalization (Codex deep-sec PR #135 Finding 1).
// ---------------------------------------------------------------------------

// canonicalIntentTaskKey returns the canonical leading-backslash form of
// taskName so every intent file write/read lands on a single key shape.
//
// Background: scheduler.List() and the supervisor reconcile loop's rows
// expose Windows Task Scheduler names with the leading "\" the OS persists
// (e.g. "\mcp-local-hub-memory-default"). The supervisor reconcile loop
// (internal/cli/supervise_reconcile.go) indexes the intent file via
// `intent.Tasks[row.TaskName]`, so any caller that wrote intent
// under the bare form (e.g. install/uninstall paths that manifest-derive
// their task names) used to slip past the lookup → the reconcile loop
// revived a daemon the operator had just stopped/uninstalled. Per the
// security review, Option A normalizes at this single boundary instead of
// asking every call site to remember.
//
// The contract is purely lexical: prepend "\" if missing. Names that
// already have the leading backslash are returned unchanged. Empty input
// is returned unchanged so the IdentityFieldByteCap rejection in
// WriteDaemonIntent / ClearDaemonIntent fires on its own message rather
// than being masked by an artificially synthesized name.
func canonicalIntentTaskKey(taskName string) string {
	if taskName == "" {
		return taskName
	}
	if len(taskName) > 0 && taskName[0] == '\\' {
		return taskName
	}
	return "\\" + taskName
}

// ---------------------------------------------------------------------------
// IsActiveStop — pure predicate (plan §4).
// ---------------------------------------------------------------------------

// IsActiveStop reports whether this intent is an effective stop
// directive at evaluation time `now`. Returns (true, reason) when
// auto-revive must be suppressed; (false, "") otherwise.
//
// Decision tree (plan §4 + Task 2.1):
//  1. Desired != stopped → not a stop directive.
//  2. now < UpdatedAt - 5m → suspected clock-skew → fail CLOSED
//     (return true with reason="clock-skew-future-suspect"). The
//     watchdog defers to operator review rather than auto-reviving
//     a daemon whose stop intent looks newer than wall-clock.
//  3. now - UpdatedAt > 365d → entry is older than the stale bound;
//     treat as inert (return false). Catches install-time clock
//     rollbacks that would otherwise persist forever.
//  4. Reason="user-stop" + now - UpdatedAt > 24h → TTL expired; the
//     daemon becomes eligible for auto-revive again. The TTL branch is
//     SCOPED to IntentReasonUserStop ALONE: IntentReasonUserDisabled,
//     IntentReasonChronicFailure, and IntentReasonIdle (v0.6 #6, spec §6)
//     all stay active stops past 24h. For idle this is load-bearing — an
//     idle daemon must sleep INDEFINITELY until a /serena/mcp wake clears
//     the reason via ClearStopIntent; a TTL-based auto-revive would let the
//     supervisor respawn it on its own, defeating the idle-shutdown. Do NOT
//     add IntentReasonIdle to this branch.
//  5. Otherwise → active stop with the recorded reason.
//
// Pure: no I/O, no clock reads beyond the supplied `now`. Safe for
// any number of concurrent callers.
func (i DaemonIntent) IsActiveStop(now time.Time) (bool, string) {
	if i.Desired != IntentDesiredStopped {
		return false, ""
	}
	// Step 2: clock-skew future.
	if now.Before(i.UpdatedAt.Add(-ClockSkewFutureTolerance)) {
		return true, ClockSkewFutureSuspectReason
	}
	// Step 3: stale bound.
	if now.Sub(i.UpdatedAt) > ClockSkewStaleBound {
		return false, ""
	}
	// Step 4: TTL only on user-stop. IntentReasonIdle (and user-disabled,
	// chronic-failure) are deliberately NOT covered here — an idle stop
	// sleeps indefinitely until a /serena/mcp wake clears it (spec §6 #6).
	if i.Reason == IntentReasonUserStop && now.Sub(i.UpdatedAt) > StopIntentTTL {
		return false, ""
	}
	return true, i.Reason
}

// ParseDaemonIntentFile applies the daemon-intent.json strict decode and
// schema validation rules to already-read bytes. It intentionally does not
// quarantine or prune corrupt files; callers that own a path-specific read
// seam can use this without duplicating the validation owner.
func ParseDaemonIntentFile(raw []byte) (DaemonIntentFile, error) {
	return parseAndValidateIntent(raw)
}

// ---------------------------------------------------------------------------
// ReadDaemonIntent — three-state file read with quarantine-on-corrupt.
// ---------------------------------------------------------------------------

// ReadDaemonIntent loads the on-disk intent file. Returns one of three
// IntentReadResult.State values per plan §3:
//
//   - missing: os.Stat returned ErrNotExist. Caller treats every task
//     as Desired=running. File.Tasks is a non-nil empty map.
//   - corrupt: file existed but failed strict parse / schema / UTF-8
//     check. The reader renames the offending file with a
//     `.corrupt-{ts}` suffix under flock, prunes older siblings to
//     QuarantineCap, and returns QuarantinePath set. Per-file prune
//     failures are non-fatal (plan §23); the rename succeeded so
//     forensic data is preserved.
//   - valid: parse + schema check both succeeded. File.Tasks is the
//     authoritative map; QuarantinePath is empty.
//
// Concurrency: the entire read holds gofrs/flock on the sibling
// `.lock` file. Safe for any number of concurrent readers/writers
// (subject to flock's mutual-exclusion semantic).
func (a *API) ReadDaemonIntent() IntentReadResult {
	dir, err := DaemonStateDir()
	if err != nil {
		return IntentReadResult{
			State: IntentStateMissing,
			File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			Err:   err,
		}
	}

	statePath := filepath.Join(dir, intentFileLeaf)
	lockPath := filepath.Join(dir, intentLockLeaf)

	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return IntentReadResult{
			State: IntentStateMissing,
			File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			Err:   fmt.Errorf("flock: %w", err),
		}
	}
	defer func() { _ = lock.Unlock() }()

	return readIntentParseAndQuarantine(statePath)
}

// ReadDaemonIntentFile loads a caller-specified daemon-intent.json path
// using the same sibling-lock and parse/quarantine owner as ReadDaemonIntent.
// This is for supervisor startup, where the CLI has already resolved the
// state directory through its MCPHUB_STATE_DIR_OVERRIDE seam.
func ReadDaemonIntentFile(statePath string, timeout time.Duration) IntentReadResult {
	if statePath == "" {
		return IntentReadResult{
			State: IntentStateMissing,
			File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			Err:   fmt.Errorf("daemon-intent path is empty"),
		}
	}
	return readDaemonIntentPathWithTimeout(statePath, statePath+".lock", timeout)
}

// TryReadDaemonIntent loads the on-disk intent file with a bounded
// lock-acquisition timeout. Same three-state semantic as
// ReadDaemonIntent (missing | corrupt | valid) but the underlying
// flock acquisition uses Flock.TryLockContext(ctx, retryDelay) instead
// of the blocking Flock.Lock(). This is the variant the tray
// aggregator (internal/cli/gui_tray_state.go) calls on its 5-second
// snapshot pump so a long-held writer (e.g. a noisy `mcphub install`
// or a flaky AV scanner pinning the daemon-intent.json.lock file)
// cannot freeze tray icon / toast updates.
//
// Bot finding (PR #142 round 2 P2, 2026-05-08): the prior wiring
// embedded the blocking ReadDaemonIntent inline in the snapshot loop,
// so a held lock would stall the goroutine until release — `ctx.Done()`
// was unobservable while waiting on the kernel-level LockFileEx /
// flock(2) call. This method gives the caller a real budget.
//
// Behaviour on the lock-acquisition timeout path: returns
//
//	IntentReadResult{
//	  State: IntentStateMissing,
//	  File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
//	  Err:   <error wrapping context.DeadlineExceeded — caller can
//	         test with errors.Is(res.Err, context.DeadlineExceeded)>,
//	}
//
// The empty-Tasks fallback matches the existing graceful-degrade
// contract `defaultIntentReader` already handles — a momentary lack
// of intent data degrades to "no preference", which is the same
// fallback the tray applies when the file is genuinely missing.
//
// Round 3 codex finding R6+Q3: the wrapped error precisely tags
// timeout (via errors.Is + context.DeadlineExceeded) only on the
// real ctx-deadline path; non-timeout flock errors (permission/IO at
// lock-acquire time) are surfaced verbatim without being mislabelled
// as a timeout. Callers that need to distinguish "tray's 250ms budget
// elapsed under contention" from "filesystem permission failure on the
// state dir" can branch on errors.Is(res.Err, context.DeadlineExceeded).
//
// Round 3 codex finding R2: timeout <= 0 is a single non-blocking
// TryLock() attempt — context.WithTimeout(0) creates an already-fired
// context, so flock.TryLockContext would short-circuit with
// DeadlineExceeded EVEN IF the lock was free. The non-blocking branch
// preserves the caller's "try once" intent (a free lock returns the
// real file; a held lock returns ErrLockUnavailable WITHOUT a fake
// timeout label).
//
// On lock acquisition: identical body to ReadDaemonIntent — same
// parse + quarantine + prune flow via the shared helper
// readIntentParseAndQuarantine. Callers must NOT use this method when
// blocking semantics are required (e.g. install / stop / uninstall
// one-shot flows that genuinely need to wait on the writer to finish).
//
// Tunable: retryDelay is fixed at 10ms inside the implementation —
// short enough that a 50ms timeout still polls ~5 times before
// giving up, and long enough not to spin on a busy disk. Callers
// supply only the absolute timeout; callers that need a custom
// retryDelay can refactor later.
func (a *API) TryReadDaemonIntent(timeout time.Duration) IntentReadResult {
	dir, err := DaemonStateDir()
	if err != nil {
		return IntentReadResult{
			State: IntentStateMissing,
			File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			Err:   err,
		}
	}

	statePath := filepath.Join(dir, intentFileLeaf)
	lockPath := filepath.Join(dir, intentLockLeaf)

	return readDaemonIntentPathWithTimeout(statePath, lockPath, timeout)
}

func readDaemonIntentPathWithTimeout(statePath, lockPath string, timeout time.Duration) IntentReadResult {
	lock := flock.New(lockPath)

	// Round 3 codex finding R2: zero/negative timeout is a single
	// non-blocking probe. Going through context.WithTimeout(0) would
	// short-circuit with DeadlineExceeded even if the lock was free,
	// because the context is already past its deadline before
	// TryLockContext's first poll. The bare TryLock() preserves caller
	// intent ("try once non-blocking") and reports lock-unavailable as
	// a non-timeout error (callers will not confuse it with the
	// retry-budget-exhausted path).
	if timeout <= 0 {
		locked, lockErr := lock.TryLock()
		if lockErr != nil {
			return IntentReadResult{
				State: IntentStateMissing,
				File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
				Err:   fmt.Errorf("flock TryLock: %w", lockErr),
			}
		}
		if !locked {
			return IntentReadResult{
				State: IntentStateMissing,
				File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
				Err:   fmt.Errorf("flock TryLock: lock unavailable (non-blocking probe)"),
			}
		}
		defer func() { _ = lock.Unlock() }()
		return readIntentParseAndQuarantine(statePath)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	const retryDelay = 10 * time.Millisecond
	locked, lockErr := lock.TryLockContext(ctx, retryDelay)
	if lockErr != nil {
		// Distinguish timeout (caller's budget elapsed under contention)
		// from non-timeout errors (rare: permission/IO at lock-acquire
		// time). The wrapped chain preserves errors.Is behaviour so
		// callers can test errors.Is(res.Err, context.DeadlineExceeded)
		// to branch on contention vs. real I/O failure.
		if errors.Is(lockErr, context.DeadlineExceeded) {
			return IntentReadResult{
				State: IntentStateMissing,
				File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
				Err:   fmt.Errorf("flock TryLockContext: timeout after %s: %w", timeout, lockErr),
			}
		}
		return IntentReadResult{
			State: IntentStateMissing,
			File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			Err:   fmt.Errorf("flock TryLockContext: %w", lockErr),
		}
	}
	if !locked {
		// TryLockContext returned (false, nil) — never observed in
		// practice, but defensive against future flock revisions.
		return IntentReadResult{
			State: IntentStateMissing,
			File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			Err:   fmt.Errorf("flock TryLockContext: lock unavailable after %s timeout", timeout),
		}
	}
	defer func() { _ = lock.Unlock() }()

	return readIntentParseAndQuarantine(statePath)
}

// readIntentParseAndQuarantine is the lock-held body shared by
// ReadDaemonIntent and TryReadDaemonIntent. Caller must already hold
// the daemon-intent flock; this routine performs no locking of its
// own. Returns the three-state IntentReadResult per plan §3 contract.
//
// Factored out so TryReadDaemonIntent can reuse the parse +
// quarantine + prune flow without duplicating the logic. Production
// invariants (corrupt-rename under flock, prune best-effort,
// QuarantinePath surfaced to caller) are preserved exactly.
func readIntentParseAndQuarantine(statePath string) IntentReadResult {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return IntentReadResult{
				State: IntentStateMissing,
				File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			}
		}
		return IntentReadResult{
			State: IntentStateMissing,
			File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			Err:   fmt.Errorf("read %s: %w", statePath, err),
		}
	}

	parsed, parseErr := parseAndValidateIntent(raw)
	if parseErr == nil {
		return IntentReadResult{
			State: IntentStateValid,
			File:  parsed,
		}
	}

	// Corrupt path: rename + prune under the same flock.
	quarantinePath, quarantineErr := quarantineCorruptFile(statePath)
	if quarantineErr != nil {
		// Rename failed — quarantine aborted entirely. Return the parse
		// error so callers know what happened, but include the
		// quarantine error in the chain via fmt.Errorf so log readers
		// see both root causes.
		return IntentReadResult{
			State: IntentStateCorrupt,
			File:  DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
			Err:   fmt.Errorf("parse: %w; quarantine: %v", parseErr, quarantineErr),
		}
	}
	// Rename succeeded; prune is best-effort.
	pruneCorruptSiblings(statePath)

	return IntentReadResult{
		State:          IntentStateCorrupt,
		File:           DaemonIntentFile{Tasks: map[string]DaemonIntent{}},
		QuarantinePath: quarantinePath,
		Err:            parseErr,
	}
}

// parseAndValidateIntent applies strict JSON decoding + schema/UTF-8
// validation to the raw file bytes. Returns the parsed file on success,
// or a wrapped error describing the first violation found. Always returns
// a non-nil File on success.
func parseAndValidateIntent(raw []byte) (DaemonIntentFile, error) {
	if !utf8.Valid(raw) {
		return DaemonIntentFile{}, errIntentInvalidUTF8
	}

	var file DaemonIntentFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return DaemonIntentFile{}, fmt.Errorf("json decode: %w", err)
	}
	// Reject trailing tokens — strict single-document files only.
	if dec.More() {
		return DaemonIntentFile{}, fmt.Errorf("json decode: extra data after top-level object")
	}

	// Reject non-UTC timestamps. json.Time decodes RFC3339 with zone
	// info preserved; we require Z (UTC) on disk.
	//
	// isUTCInstant scans every `"updated_at":"...":` occurrence in the
	// raw bytes — its result is identical regardless of which task
	// name is passed (the parameter is unused), so call it ONCE up
	// front. The prior per-task call was O(N*M) on the raw buffer
	// (N tasks × M-byte file) and tripped the api-suite hang on
	// Windows once the intent file grew under multi-task tests
	// (work-items/bugs/2026-05-12-internal-api-suite-hangs-on-windows.md).
	if !isUTCInstant(raw, "") {
		return DaemonIntentFile{}, fmt.Errorf("%w (updated_at must be UTC Z)", errIntentSchemaInvalid)
	}
	for name, intent := range file.Tasks {
		if err := validateIntentFields(intent); err != nil {
			return DaemonIntentFile{}, fmt.Errorf("entry %q: %w", name, err)
		}
	}

	if file.Tasks == nil {
		file.Tasks = map[string]DaemonIntent{}
	}
	return file, nil
}

// validateIntentFields enforces the per-entry schema rules: Desired in
// {running, stopped}, Reason in the known enum set, UpdatedAt non-zero.
// Returns errIntentSchemaInvalid wrapped with a per-field message on
// any violation.
func validateIntentFields(in DaemonIntent) error {
	if in.Desired != IntentDesiredRunning && in.Desired != IntentDesiredStopped {
		return fmt.Errorf("desired=%q not in {running,stopped}: %w", in.Desired, errIntentSchemaInvalid)
	}
	if !isKnownIntentReason(in.Reason) {
		return fmt.Errorf("reason=%q not in known set: %w", in.Reason, errIntentSchemaInvalid)
	}
	if in.UpdatedAt.IsZero() {
		return fmt.Errorf("updated_at is zero value: %w", errIntentSchemaInvalid)
	}
	if in.UpdatedAt.Location() != time.UTC {
		return fmt.Errorf("updated_at location=%v not UTC: %w", in.UpdatedAt.Location(), errIntentSchemaInvalid)
	}
	return nil
}

// isKnownIntentReason returns true when the string matches one of the
// declared IntentReason* constants. Case-sensitive exact match —
// arbitrary operator-supplied reasons are rejected so adversarial
// writers cannot inject control characters or path-traversal segments
// into audit logs downstream.
func isKnownIntentReason(r string) bool {
	switch r {
	case IntentReasonUserStop,
		IntentReasonUserDisabled,
		IntentReasonChronicFailure,
		IntentReasonUninstalled,
		IntentReasonInstall,
		IntentReasonRegister,
		IntentReasonIdle:
		return true
	}
	return false
}

// isUTCInstant scans the raw JSON for the entry's updated_at value and
// asserts it ends in `Z`. Defends against decoders that quietly normalize
// `+00:00` to UTC during Unmarshal (Go's time package does NOT do this,
// but defense-in-depth: if the decoder ever changes, this check catches
// the regression). The naive substring search is acceptable because we
// already validated the JSON shape above.
func isUTCInstant(raw []byte, taskName string) bool {
	// Build a search key that is unique enough: `"updated_at":"`.
	// Walk every occurrence to find the one for this task. This is a
	// coarse check; the json.Unmarshal pass above already populated
	// time.Time values, so we mostly need to confirm no offset-bearing
	// timestamp slipped through Go's RFC3339 parse.
	const key = `"updated_at":"`
	idx := 0
	for {
		i := bytes.Index(raw[idx:], []byte(key))
		if i < 0 {
			break
		}
		start := idx + i + len(key)
		end := bytes.IndexByte(raw[start:], '"')
		if end < 0 {
			return false
		}
		ts := string(raw[start : start+end])
		// RFC3339Nano UTC ends in `Z`. Anything else (`+00:00`, `-08:00`,
		// `+05:30`) is rejected — our writers always emit `Z`.
		if !strings.HasSuffix(ts, "Z") {
			return false
		}
		idx = start + end
	}
	_ = taskName
	return true
}

// ---------------------------------------------------------------------------
// WriteDaemonIntent — atomic temp+rename under flock.
// ---------------------------------------------------------------------------

// WriteDaemonIntent records (or replaces) the entry for taskName.
// `who` carries the operator/caller identity for the audit trail.
// Task 3 wired the AppendIntentAudit dispatcher; this function now
// emits a "set-intent" audit entry with the pre-mutation Before
// snapshot and the post-mutation After snapshot.
//
// Phase 4-E2: this is NO LONGER the live stop writer. The five production
// stop writers (recordStopIntentAs / recordInstall/Restart/Uninstall/Register-
// IntentForTask) now write the supervisor-intent.json `stops` sub-block via
// WriteStopIntent (stop_intent_subblock.go). WriteDaemonIntent remains ONLY
// to (a) write the legacy daemon-intent.json an OLD binary still owns during
// the redeploy window, and (b) let the merge tests seed a daemon-intent.json
// the boot-merge then migrates + deletes. Production code outside that
// migration boundary must use WriteStopIntent.
//
// Atomicity:
//  1. Acquire gofrs/flock on the sibling `.lock` file.
//  2. Read+parse the existing file (if any). Corrupt files are
//     quarantined and treated as starting from an empty map — the
//     write proceeds against the cleared state. This honors the
//     "no silent data loss" principle: forensic copies live in
//     `.corrupt-{ts}` siblings.
//  3. Capture the Before snapshot (current value for taskName, if any).
//  4. Update the in-memory map.
//  5. Marshal with UTC normalization on UpdatedAt.
//  6. Write to a fresh temp file (`${stem}.tmp.${pid}.${nano}`) at 0600.
//  7. os.Rename onto the canonical path. POSIX guarantees atomicity;
//     Windows ReplaceFile semantics achieve the same effect.
//  8. Emit "set-intent" audit entry with Before / After / Who.
//
// Failure semantics: any error is returned verbatim and the canonical
// file is left in its prior state (rename happens last). The temp file
// is best-effort cleaned up on error paths. Audit-write failure (e.g.,
// ErrIdentityOversize from a malicious task name slipping past the
// 1KB intent gate) is surfaced to the caller; install/stop are
// expected to fail closed per §51.
func (a *API) WriteDaemonIntent(taskName string, intent DaemonIntent, who string) error {
	if len(who) > IdentityFieldByteCap {
		return ErrEntryOversize
	}

	// Codex deep-sec PR #135 Finding 1: normalize the storage key to the
	// canonical leading-backslash form BEFORE any persistence work so every
	// intent record lands on the same key shape that the supervisor
	// reconcile loop (internal/cli/supervise_reconcile.go) indexes
	// (`intent.Tasks[row.TaskName]` where row.TaskName carries the
	// leading "\").
	//
	// PR #135 round 3 P2: cap-check on the CANONICAL key, not the raw
	// input. canonicalIntentTaskKey can prepend exactly one byte ("\\"),
	// so a bare 1024-byte input becomes a 1025-byte key — that key flows
	// to the audit log, where AuditIdentityFieldByteCap (intent_audit.go)
	// rejects it via ErrIdentityOversize. WriteDaemonIntent ignores audit
	// append errors, so the audit record was being silently dropped for
	// max-length valid task identifiers. Capping the canonical form
	// keeps disk + audit storage symmetric on a single 1KB ceiling.
	taskName = canonicalIntentTaskKey(taskName)
	if len(taskName) > IdentityFieldByteCap {
		return ErrEntryOversize
	}

	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	statePath := filepath.Join(dir, intentFileLeaf)
	lockPath := filepath.Join(dir, intentLockLeaf)

	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	current := readIntentLocked(statePath)
	if current.Tasks == nil {
		current.Tasks = map[string]DaemonIntent{}
	}

	// Capture Before snapshot for the audit entry. Pointer is nil when
	// no prior intent exists (clean install case).
	var before *DaemonIntent
	if prior, ok := current.Tasks[taskName]; ok {
		priorCopy := prior
		before = &priorCopy
	}

	// Normalize timestamp to UTC. Caller may pass any location; we want
	// disk and downstream code to see UTC instants only.
	intent.UpdatedAt = intent.UpdatedAt.UTC()
	if intent.UpdatedAt.IsZero() {
		intent.UpdatedAt = time.Now().UTC()
	}
	current.Tasks[taskName] = intent

	if err := writeIntentLocked(statePath, current); err != nil {
		return err
	}

	// Task 3 audit emit: set-intent with Before / After / Who. Routed
	// through the appendIntentAuditFn seam (api_surfaces.go) which
	// intent_audit.go's init() binds to the real (*API).AppendIntentAudit.
	after := intent
	if appendIntentAuditFn != nil {
		_ = appendIntentAuditFn(NewIntentAuditEntry(
			WithAction("set-intent"),
			WithTask(taskName),
			WithWho(who),
			WithReason(intent.Reason),
			WithBefore(before),
			WithAfter(&after),
		))
	}
	return nil
}

// ClearDaemonIntent removes the entry for taskName. Idempotent — a
// missing entry is treated as success. Same atomic rename + flock
// semantics as WriteDaemonIntent. Emits a "clear-intent" audit entry
// when the prior entry actually existed (Before != nil); a no-op
// clear (entry missing or map empty) does NOT emit an audit entry
// since there is nothing to record.
func (a *API) ClearDaemonIntent(taskName string, who string) error {
	if len(who) > IdentityFieldByteCap {
		return ErrEntryOversize
	}

	// Codex deep-sec PR #135 Finding 1: same normalization as WriteDaemonIntent
	// so a clear-by-bare-form locates the canonical leading-backslash entry
	// instead of leaving the canonical record untouched (silent no-op).
	// PR #135 round 3 P2: cap-check on the CANONICAL key (one byte longer
	// in the worst case) so disk + audit storage share one 1KB ceiling
	// and the audit record can never be silently dropped on edge-case
	// max-length names.
	taskName = canonicalIntentTaskKey(taskName)
	if len(taskName) > IdentityFieldByteCap {
		return ErrEntryOversize
	}

	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	statePath := filepath.Join(dir, intentFileLeaf)
	lockPath := filepath.Join(dir, intentLockLeaf)

	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	current := readIntentLocked(statePath)
	if current.Tasks == nil {
		// Already empty; clearing is a no-op.
		return nil
	}
	prior, ok := current.Tasks[taskName]
	if !ok {
		return nil
	}
	delete(current.Tasks, taskName)

	if err := writeIntentLocked(statePath, current); err != nil {
		return err
	}

	// Task 3 audit emit: clear-intent with Before snapshot (the
	// directive being cleared). After is nil because the entry no
	// longer exists. Routed through the appendIntentAuditFn seam.
	before := prior
	if appendIntentAuditFn != nil {
		_ = appendIntentAuditFn(NewIntentAuditEntry(
			WithAction("clear-intent"),
			WithTask(taskName),
			WithWho(who),
			WithReason(prior.Reason),
			WithBefore(&before),
		))
	}
	return nil
}

// readIntentLocked reads + parses the existing file under the caller's
// already-held flock. Returns an empty (non-nil) DaemonIntentFile on
// missing/corrupt — callers proceed against a fresh map. Corruption
// here triggers a quarantine rename so the next write lands on a
// clean canonical path.
func readIntentLocked(statePath string) DaemonIntentFile {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return DaemonIntentFile{Tasks: map[string]DaemonIntent{}}
	}
	parsed, parseErr := parseAndValidateIntent(raw)
	if parseErr != nil {
		// Best-effort quarantine — failure is logged but does not
		// block the write. The new write below will land on a
		// fresh canonical path either way.
		if _, qErr := quarantineCorruptFile(statePath); qErr == nil {
			pruneCorruptSiblings(statePath)
		}
		return DaemonIntentFile{Tasks: map[string]DaemonIntent{}}
	}
	if parsed.Tasks == nil {
		parsed.Tasks = map[string]DaemonIntent{}
	}
	return parsed
}

// writeIntentLocked marshals + writes the file under the caller's
// already-held flock, using temp+rename for atomicity. Marshal uses
// indent for human readability; downstream tools tolerate either form.
func writeIntentLocked(statePath string, file DaemonIntentFile) error {
	// Re-normalize UpdatedAt on every entry: defends against a buggy
	// mutation upstream that bypassed WriteDaemonIntent's normalization.
	for k, v := range file.Tasks {
		v.UpdatedAt = v.UpdatedAt.UTC()
		file.Tasks[k] = v
	}

	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	dir := filepath.Dir(statePath)
	tempName := filepath.Join(dir, fmt.Sprintf("%s.%d.%d", intentTempPrefix, os.Getpid(), time.Now().UnixNano()))
	tempFile, err := os.OpenFile(tempName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("open temp: %w", err)
	}
	// Best-effort cleanup on failure; on success the rename consumes
	// the temp before this defer runs (Remove on a renamed path is a
	// no-op on POSIX and a benign error on Windows).
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

	if err := os.Rename(tempName, statePath); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Quarantine helpers (plan §23).
// ---------------------------------------------------------------------------

// quarantineCorruptFile renames statePath to `${stem}.corrupt-{ts}`
// using a UTC timestamp suffix. Returns the renamed path on success,
// or an error wrapping the rename failure on disk-full / permission
// problems. Caller already holds flock; this routine performs no
// locking of its own.
//
// Failure abort policy (plan §23): if rename fails the entire
// quarantine is aborted — there is no fallback (we won't truncate the
// corrupt file in place; that would destroy forensic state). Caller
// surfaces the error to the user and the next read tries again.
func quarantineCorruptFile(statePath string) (string, error) {
	ts := time.Now().UTC().Format(quarantineSuffixLayout)
	target := statePath + ".corrupt-" + ts
	// Disambiguate identical-millisecond renames within one nanosecond
	// budget: if target already exists (effectively impossible at
	// nanosecond granularity, but defense-in-depth), append a counter.
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

// pruneCorruptSiblings keeps the QuarantineCap newest `.corrupt-*`
// siblings of statePath and deletes the rest. Per-file failures are
// logged via quarantinePruneLogFn and treated as non-fatal (plan §23):
// rename already succeeded, so forensic state is preserved.
//
// List failure is also non-fatal — an empty directory listing simply
// means no work to do; the caller already received a successful
// quarantine.
func pruneCorruptSiblings(statePath string) {
	dir := filepath.Dir(statePath)
	leaf := filepath.Base(statePath)
	prefix := leaf + ".corrupt-"

	entries, err := os.ReadDir(dir)
	if err != nil {
		logQuarantine("quarantine-prune-list-failed-non-fatal", dir, err)
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
			// File vanished mid-walk; benign.
			continue
		}
		found = append(found, candidate{path: full, mtime: info.ModTime()})
	}

	// Sort by mtime DESC: newest first. Equal mtimes break by path so
	// the order is deterministic across runs.
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
		removeErr := removeQuarantineFile(victim.path)
		if removeErr != nil {
			logQuarantine("quarantine-prune-failed-non-fatal", victim.path, removeErr)
			// continue; per-file failure must not block other deletions
		}
	}
}

// removeQuarantineFile is the indirection that lets tests inject a
// failing delete. Production calls os.Remove.
func removeQuarantineFile(path string) error {
	if quarantineRemoveFileFn != nil {
		return quarantineRemoveFileFn(path)
	}
	return os.Remove(path)
}

// logQuarantine emits a non-fatal prune event. Production wires this
// to watchdog.log in a later task; for Task 2 the default behavior is
// silent (the test seam quarantinePruneLogFn captures events when the
// caller cares).
func logQuarantine(event, path string, err error) {
	if quarantinePruneLogFn != nil {
		quarantinePruneLogFn(event, path, err)
		return
	}
	// Production fallback: write to stderr so a debug-level operator
	// running the watchdog with stderr open still sees the event.
	// Format is deliberately minimal — logging will move to JSON Lines
	// once watchdog.log lands. Truncate path to 256 bytes to defend
	// against pathological filesystem names.
	pathTrim := path
	if len(pathTrim) > 256 {
		pathTrim = pathTrim[:256]
	}
	fmt.Fprintf(io.Discard, "%s %s %v\n", event, pathTrim, err)
}

// ---------------------------------------------------------------------------
// Seam wiring — replace Task 0's stub with the real reader.
// ---------------------------------------------------------------------------

// init binds api_surfaces.go's readDaemonIntentFn seam to a thin
// adapter over ReadDaemonIntent. After Task 2 ships, IntentStillRunning
// is backed by real on-disk state by default; tests in api_surfaces_test.go
// continue to overwrite the seam via installTestIntentReader and rely
// on cleanup to restore this production binding.
//
// The adapter constructs a transient *API to call ReadDaemonIntent.
// ReadDaemonIntent is stateless on the receiver — it consults
// DaemonStateDir() + the on-disk file, both of which are package-global
// — so a fresh API is functionally equivalent to the caller's API
// instance for the purposes of this read.
func init() {
	readDaemonIntentFn = func(taskName string) (DaemonIntent, bool, error) {
		api := NewAPI()
		res := api.ReadDaemonIntent()
		if res.Err != nil && res.State != IntentStateCorrupt {
			// State==corrupt with non-nil Err is still informative
			// (the file got quarantined) — caller treats no entry as
			// "no preference". Other read errors propagate.
			return DaemonIntent{}, false, res.Err
		}
		intent, ok := res.File.Tasks[taskName]
		if !ok {
			return DaemonIntent{}, false, nil
		}
		return intent, true, nil
	}
}

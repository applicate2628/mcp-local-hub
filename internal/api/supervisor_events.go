// Package api — Task 2.3 supervisor-events.log JSONL helper (v0.5.0
// supervisor architecture, plan §"supervisor-events.log (NEW)").
//
// supervisor-events.log is the FOURTH JSON-Lines log file in the
// state-dir family (alongside watchdog.log, intent-audit.log, and
// gui-events.log). Owned by the long-lived `mcphub supervise`
// process, it records every supervisor-side decision so operators
// can reason about lifecycle (start/stop/respawn), restart-policy
// transitions, IPC commands, migration progress, and reconcile-loop
// activity without scraping the per-child daemon stdout.
//
// Envelope shape (plan v4 §"supervisor-events.log (NEW)") — mirrors
// gui_event_log.go:19-25 with two supervisor-specific additions:
//
//	{
//	  "schema_version": "1",
//	  "ts": "RFC3339Nano",
//	  "severity": "debug|info|warn|error",
//	  "source": "ipc|lifecycle|restart-policy|migration|autostart|reconcile",
//	  "event": "<canonical event vocabulary>",
//	  "task_name": "\\mcp-local-hub-...",
//	  "body": { "..." }
//	}
//
// Differences vs gui_event_log: `event` discriminator instead of
// `type`, `task_name` added field. Same schema_version + body keys.
// Same 16 KB per-entry cap, 10 MB rotation, and gofrs/flock
// serialization discipline.
//
// Identity preservation (mirrors intent_audit.go:87-98 §35):
// `event`, `source`, and `task_name` are identity fields and NEVER
// truncated. When the marshaled entry exceeds the 16 KB cap the body
// payload is replaced with a sentinel `_truncated_note` map; the
// entry gains `_truncated:true` so consumers know not to trust the
// body content for analysis.
//
// Constructor / Emit pattern: the supervisor binary launches before
// any *API instance exists, so this helper exposes a path-injected
// constructor (`OpenSupervisorEventLog(path)`) returning a
// `*SupervisorEventLog` with an `Emit(...)` method — DIFFERENT from
// the `(*API).AppendGUIEventLog` precedent which carries an *API
// receiver and resolves DaemonStateDir internally. The supervisor
// task plan explicitly specifies this
// constructor-based shape (Task 2.3 step 4); see spec §"Package
// ownership" for rationale.
package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// ---------------------------------------------------------------------------
// File layout constants.
// ---------------------------------------------------------------------------

// SupervisorEventLogFileLeaf is the canonical file name (relative to
// DaemonStateDir) for supervisor-events.log. Exposed so callers in
// other packages (e.g. `internal/cli/supervise.go`) can construct
// the full path without re-declaring the literal.
const SupervisorEventLogFileLeaf = "supervisor-events.log"

// supervisorEventLogLockSuffix is appended to the active log path to
// form the gofrs/flock lock path. Mirrors `watchdog.log.lock` and
// `gui-events.log.lock` naming so operators see one consistent
// convention across all four JSONL logs.
const supervisorEventLogLockSuffix = ".lock"

// supervisorEventLogRotatedSuffix is appended to the rotated file
// name. Mirrors gui_event_log.go / intent_audit.go — only .1 is
// retained; older rotations are overwritten on next rotation.
const supervisorEventLogRotatedSuffix = ".1"

// supervisorEventLogRotateSize is the 10 MB rotation threshold per
// plan §"supervisor-events.log (NEW)". Kept in sync with
// GUIEventLogRotateSizeBytes / WatchdogLogRotateSizeBytes so all
// four log families share one ceiling.
const supervisorEventLogRotateSize int64 = 10 * 1024 * 1024

// supervisorEventMaxBytes is the per-entry size cap per plan
// §"supervisor-events.log (NEW)" (mirrors AuditEntryMaxBytes,
// intent_audit.go:87-91 §35). Marshaled JSON Lines exceeding this
// ceiling are truncated with identity-field protection.
const supervisorEventMaxBytes = 16 * 1024

const (
	supervisorEventPendingDirSuffix   = ".pending"
	supervisorEventPendingFileSuffix  = ".jsonl"
	supervisorEventPendingReplayLimit = 64
	// The valid-carrier quota and raw traversal budget are deliberately separate:
	// temporary/unrecognized names must not consume all replay slots, but a
	// damaged directory must not make one replay unbounded.
	supervisorEventPendingRawScanBudget = supervisorEventPendingReplayLimit * 64
	supervisorEventPendingScanPageSize  = 64
)

// supervisorEventIdentityCap is the per-identity-field byte ceiling
// per plan §51. Identity fields (Event, Source, TaskName) are NEVER
// truncated; if any exceeds the cap, Emit fails closed with
// ErrSupervisorEventIdentityOversize. Mirrors
// AuditIdentityFieldByteCap (intent_audit.go:98) so the supervisor
// envelope shares one identity-oversize discipline with the other
// JSONL log families.
const supervisorEventIdentityCap = 1024

// SupervisorEventSchemaVersion is the current envelope schema
// version. Bumped when any field is added/removed or semantics
// change. Tools reading the log should branch on this value.
const SupervisorEventSchemaVersion = "1"

// Severity values — canonical, mirror plan envelope.
const (
	SupervisorEventSeverityDebug = "debug"
	SupervisorEventSeverityInfo  = "info"
	SupervisorEventSeverityWarn  = "warn"
	SupervisorEventSeverityError = "error"
)

// Source values — canonical, mirror plan envelope.
const (
	SupervisorEventSourceIPC           = "ipc"
	SupervisorEventSourceLifecycle     = "lifecycle"
	SupervisorEventSourceRestartPolicy = "restart-policy"
	SupervisorEventSourceMigration     = "migration"
	SupervisorEventSourceAutostart     = "autostart"
	SupervisorEventSourceReconcile     = "reconcile"
)

// ---------------------------------------------------------------------------
// Schema.
// ---------------------------------------------------------------------------

// SupervisorEvent is the in-memory shape of one envelope. Auto-fill
// rules (mirroring gui_event_log.go:137-151):
//   - SchemaVersion: defaults to SupervisorEventSchemaVersion ("1")
//     when empty.
//   - TS: defaults to time.Now().UTC().Format(RFC3339Nano) when empty;
//     callers may pre-set when replaying tests with deterministic
//     timestamps.
//   - Severity: defaults to "info" when empty.
//   - Event: REQUIRED. Empty Event returns ErrSupervisorEventMissingEvent.
//   - Source: REQUIRED. Empty Source returns ErrSupervisorEventMissingSource.
//
// Identity fields (NEVER truncated per §35): Event, Source, TaskName.
// Body is the only field truncated when the entry exceeds the 16 KB
// cap.
type SupervisorEvent struct {
	SchemaVersion string         `json:"schema_version"`
	TS            string         `json:"ts"`
	Severity      string         `json:"severity"`
	Source        string         `json:"source"`
	Event         string         `json:"event"`
	TaskName      string         `json:"task_name,omitempty"`
	Body          map[string]any `json:"body,omitempty"`
	Truncated     bool           `json:"_truncated,omitempty"`
}

// PreparedSupervisorEvent is the immutable, normalized byte representation of
// one supervisor event. Its fields are deliberately unexported: callers can
// create a value only through PrepareSupervisorEvent, which applies the
// canonical defaults, truncation, and terminal-newline rules exactly once.
type PreparedSupervisorEvent struct {
	raw    []byte
	digest [sha256.Size]byte
}

// PrepareSupervisorEvent normalizes one event into its exact JSONL bytes and
// binds those bytes to their SHA-256 content identity. The returned value can
// be emitted and persisted without regenerating its timestamp or remarshal.
func PrepareSupervisorEvent(evt SupervisorEvent) (PreparedSupervisorEvent, error) {
	raw, err := marshalSupervisorEventLine(evt)
	if err != nil {
		return PreparedSupervisorEvent{}, err
	}
	return preparedSupervisorEventFromRaw(raw), nil
}

func preparedSupervisorEventFromRaw(raw []byte) PreparedSupervisorEvent {
	owned := bytes.Clone(raw)
	return PreparedSupervisorEvent{
		raw:    owned,
		digest: sha256.Sum256(owned),
	}
}

// ---------------------------------------------------------------------------
// Errors.
// ---------------------------------------------------------------------------

// ErrSupervisorEventMissingEvent is returned by Emit when the entry
// has no Event discriminator. Callers must supply a non-empty Event
// so log consumers can categorize the row.
var ErrSupervisorEventMissingEvent = errors.New("supervisor event log: missing event discriminator")

// ErrSupervisorEventMissingSource is returned by Emit when the
// entry has no Source. Callers must supply a non-empty Source so log
// consumers can categorize the emit-site.
var ErrSupervisorEventMissingSource = errors.New("supervisor event log: missing source")

// ErrSupervisorEventIdentityOversize is returned by Emit when any
// identity field (Event, Source, TaskName) exceeds
// supervisorEventIdentityCap (1024 bytes). Identity fields are never
// truncated per §35; Emit fails closed so a malicious or
// programmer-error oversize identity cannot land in the log under
// the truncation rules used for Body. Mirrors ErrIdentityOversize
// (intent_audit.go:118 §51).
var ErrSupervisorEventIdentityOversize = errors.New("supervisor event log: identity field (event/source/task_name) exceeds 1024-byte cap")

// ErrSupervisorEventEmitTimeout reports that EmitWithTimeout could not acquire
// the event-log mutex or flock inside its bounded wait. Callers that need a
// durable fallback can distinguish this best-effort skip from a successful
// append.
var ErrSupervisorEventEmitTimeout = errors.New("supervisor event log: bounded emit timed out")

// ErrSupervisorEventReleaseFailed classifies an emit whose WRITE phase may
// well have succeeded but whose cross-process flock could NOT be released
// (review finding 2). It is the one emit outcome that says nothing about the
// row and everything about the LOCK: this process may still hold the flock on
// supervisor-events.log, so every other process that emits here — the
// supervisor, the install CLI — is blocked until this one exits.
//
// Callers distinguish it with errors.Is. It is joined with, never substituted
// for, a write error: an emit that failed BOTH ways reports both.
var ErrSupervisorEventReleaseFailed = errors.New("supervisor event log: flock release failed; the cross-process lock may still be held")

// PendingSupervisorEventEmit exposes the EVENTUAL completion of an
// EmitWithTimeoutTracked call that timed out (residual 2 review fix). A
// timeout does NOT mean the write was lost: writeEventLine's rotation/open/
// write/close have no cancellable syscall surface, so the worker goroutine
// that already holds both the in-process mutex and the cross-process flock
// keeps running and (usually) finishes shortly after its caller gives up
// (see emit's emitTimeout branch). A caller whose fallback path would
// otherwise enqueue an INDEPENDENT duplicate write (identical Event/Source/
// Body) MUST instead await this handle first via Wait, so a late-but-
// successful original write is never followed by a second, redundant row.
type PendingSupervisorEventEmit struct {
	done <-chan error
}

// NewPendingSupervisorEventEmitForTest builds a handle whose first Wait
// immediately yields the given outcome. It exists so a CONSUMER of this handle
// (e.g. daemonrecovery's queueIdempotentAuditFallback, whose branch logic
// decides whether a duplicate audit row gets written) can be tested
// deterministically, without staging a real stalled worker and then racing its
// release. Only tests may call it.
func NewPendingSupervisorEventEmitForTest(outcome error) *PendingSupervisorEventEmit {
	done := make(chan error, 1)
	done <- outcome
	return &PendingSupervisorEventEmit{done: done}
}

// Wait blocks up to timeout for the abandoned worker to finish its write AND
// release both locks — since review finding 2 the worker signals completion
// only once its release outcome is known, so a result observed here can never
// precede the release it reports.
//
// Returns the worker's own completion error: nil on a successful append that
// also released cleanly, an error matching ErrSupervisorEventReleaseFailed when
// the write landed but the cross-process flock could not be released, or
// ErrSupervisorEventEmitTimeout again if it still has not finished within
// the bound — the caller remains free to treat that as "still unknown" and
// fall back to an independent write as a last resort. A non-positive timeout
// performs a single non-blocking check. Safe to call on a nil receiver
// (returns ErrSupervisorEventEmitTimeout immediately). Call AT MOST ONCE per
// handle: the underlying channel is drained by the first receive, so a
// second call would simply wait out its own timeout and report
// ErrSupervisorEventEmitTimeout regardless of the first call's real result.
func (p *PendingSupervisorEventEmit) Wait(timeout time.Duration) error {
	if p == nil || p.done == nil {
		return ErrSupervisorEventEmitTimeout
	}
	if timeout <= 0 {
		select {
		case err := <-p.done:
			return err
		default:
			return ErrSupervisorEventEmitTimeout
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-p.done:
		return err
	case <-timer.C:
		return ErrSupervisorEventEmitTimeout
	}
}

// ---------------------------------------------------------------------------
// SupervisorEventLog — constructor + Emit.
// ---------------------------------------------------------------------------

// SupervisorEventLog is the open file handle abstraction. Holds the
// active log path + the gofrs/flock primitive + an in-process mutex
// so concurrent goroutines in the same supervisor binary serialize
// before contending for the file lock.
//
// The `flock.Flock` instance is re-used across calls (the gofrs/flock
// API is safe for repeated Lock/Unlock cycles on the same handle).
// In-process serialization via `mu` is additive defense — flock
// alone is advisory and a single Go process holding the OS-level
// flock can still racily interleave file writes from two goroutines
// without the mutex.
type SupervisorEventLog struct {
	path      string
	lock      *flock.Flock
	mu        sync.Mutex
	pendingIO supervisorEventPendingIO
}

type supervisorEventFile interface {
	Name() string
	Write([]byte) (int, error)
	ReadAt([]byte, int64) (int, error)
	Stat() (fs.FileInfo, error)
	Sync() error
	Close() error
}

// supervisorEventPendingIO is installed per log handle so durability failures
// can be exercised without mutable package-global seams or cross-test races.
// File method calls remain injectable through the returned supervisorEventFile.
type supervisorEventPendingIO struct {
	mkdirAll       func(string, fs.FileMode) error
	chmod          func(string, fs.FileMode) error
	createTemp     func(string, string) (supervisorEventFile, error)
	readBounded    func(string, int64) ([]byte, error)
	openDir        func(string) (supervisorEventPendingDir, error)
	link           func(string, string) error
	rotateIfNeeded func(string) error
	openAppend     func(string) (supervisorEventFile, error)
	containsRecord func(string, []byte) (bool, error)
	remove         func(string) error
}

// supervisorEventPendingDir is the one-handle paged pending scan seam. The
// scanner owns Close on every normal and error return.
type supervisorEventPendingDir interface {
	Readdirnames(int) ([]string, error)
	Close() error
}

// OpenSupervisorEventLog constructs a SupervisorEventLog rooted at
// the given absolute file path. The lock file is `path + ".lock"`.
// No I/O occurs here — the active log file is created lazily by
// the first Emit call (mirrors gui_event_log.go's O_CREATE-on-write
// pattern).
//
// Path injection makes the helper usable from `mcphub supervise`
// without resolving DaemonStateDir() inside the helper (the
// supervisor CLI body owns path resolution; this helper stays a
// thin durable-write primitive).
func OpenSupervisorEventLog(path string) (*SupervisorEventLog, error) {
	return &SupervisorEventLog{
		path:      path,
		lock:      flock.New(path + supervisorEventLogLockSuffix),
		pendingIO: defaultSupervisorEventPendingIO(),
	}, nil
}

// Close releases any resources held by the log handle. Currently a
// no-op because gofrs/flock does not require an explicit Close (the
// lock is held only for the duration of each Emit call). Kept on
// the API surface so future implementations that hold a long-lived
// file descriptor can add cleanup without breaking callers.
func (l *SupervisorEventLog) Close() error { return nil }

// Emit serializes the entry as a JSON Line and appends it to the
// active log under flock. Auto-fills SchemaVersion, TS, and
// Severity when blank. Returns:
//   - ErrSupervisorEventMissingEvent if Event is empty.
//   - ErrSupervisorEventMissingSource if Source is empty.
//   - wrapped I/O / marshal errors on disk-level failures.
//   - an error matching ErrSupervisorEventReleaseFailed when the append itself
//     succeeded but the cross-process flock could not be released. Every emit
//     mode can return this; it says the row is fine and the LOCK is not.
//
// Rotation: at the start of each Emit, if the existing log is
// >= supervisorEventLogRotateSize, os.Rename(*.log, *.log.1). No
// self-event is emitted (rotation for supervisor-events.log is
// implicit; the next successful append goes through normally —
// matches gui_event_log.go).
//
// Per-entry cap: if the marshaled bytes exceed
// supervisorEventMaxBytes, the body is replaced with a sentinel
// `{"_truncated_note": "..."}` map and `_truncated:true` is set.
// Identity fields (Event, Source, TaskName) are NEVER touched.
// eventLogEmitMode selects how emit acquires the cross-process flock on
// supervisor-events.log.
type eventLogEmitMode int

const (
	emitBlocking eventLogEmitMode = iota // block until the flock is acquired (durable; may wait indefinitely)
	emitTry                              // single non-blocking attempt; skip on contention
	emitTimeout                          // bounded wait via TryLockContext; skip on timeout
)

// eventLogEmitRetryDelay is the flock re-poll cadence for emitTimeout, matching
// readDaemonIntentPathWithTimeout's TryLockContext usage (daemon_intent.go).
const eventLogEmitRetryDelay = 10 * time.Millisecond

func (l *SupervisorEventLog) Emit(evt SupervisorEvent) error {
	prepared, err := PrepareSupervisorEvent(evt)
	if err != nil {
		return err
	}
	_, err = l.emitPrepared(prepared, emitBlocking, 0)
	return err
}

// EmitWithTimeout serializes the entry and appends it, waiting up to timeout
// for both its in-process mutex and the cross-process flock via
// TryLockContext. On timeout (or a defensive not-locked) it returns
// ErrSupervisorEventEmitTimeout so callers can arrange a durable fallback; it
// does NOT block indefinitely.
//
// Use this for an audit row that is the ONLY record of a state mutation AND is
// emitted while the caller still holds another lock (e.g. the strict-mode CLI
// under migration.lock + --once.lock). The bounded wait keeps the common-case
// durability of a blocking Emit — momentary contention with the supervisor's
// own event stream is ridden out rather than silently dropped, so the row is
// NOT lossy under normal contention (unlike TryEmit) — while capping how long
// the caller's outer lock can be held if the event-log flock is wedged, so a
// stalled writer can never make the caller hang forever holding its lock.
//
// This signature is unchanged by residual 2's fix and stays the right choice
// for every caller that treats the timeout as purely observability (ignores
// the error, or has its own independent outer bound) — see
// EmitWithTimeoutTracked's doc for the one case that needs more.
func (l *SupervisorEventLog) EmitWithTimeout(evt SupervisorEvent, timeout time.Duration) error {
	prepared, err := PrepareSupervisorEvent(evt)
	if err != nil {
		return err
	}
	_, err = l.emitPrepared(prepared, emitTimeout, timeout)
	return err
}

// EmitWithTimeoutTracked behaves exactly like EmitWithTimeout, but on a
// timeout ALSO returns a *PendingSupervisorEventEmit exposing the abandoned
// worker's eventual completion (residual 2 review fix). A timeout does NOT
// mean the write was lost — writeEventLine has no cancellable syscall
// surface, so the worker keeps running in the background holding both
// locks. A caller whose fallback path would otherwise enqueue an
// INDEPENDENT duplicate write (identical Event/Source/Body) MUST await the
// returned handle first (see PendingSupervisorEventEmit.Wait), so a
// late-but-successful original write is never followed by a second,
// redundant row.
//
// The returned handle is non-nil ONLY when err is
// ErrSupervisorEventEmitTimeout AND a worker was actually spawned (i.e. both
// locks were acquired before the deadline). It is nil on immediate success
// (nothing to wait for), nil on every other error (marshal/validation
// failures and lock-acquisition timeouts never spawn a worker), and nil on
// emitBlocking/emitTry (which never spawn one either).
func (l *SupervisorEventLog) EmitWithTimeoutTracked(evt SupervisorEvent, timeout time.Duration) (*PendingSupervisorEventEmit, error) {
	prepared, err := PrepareSupervisorEvent(evt)
	if err != nil {
		return nil, err
	}
	return l.EmitPreparedWithTimeoutTracked(prepared, timeout)
}

// EmitPreparedWithTimeoutTracked emits an already-normalized event without
// regenerating its timestamp or marshaling it again.
func (l *SupervisorEventLog) EmitPreparedWithTimeoutTracked(prepared PreparedSupervisorEvent, timeout time.Duration) (*PendingSupervisorEventEmit, error) {
	return l.emitPrepared(prepared, emitTimeout, timeout)
}

// TryEmit serializes the entry as a JSON Line and appends it only if the
// in-process mutex and OS flock are immediately available. Lock contention is
// treated as a best-effort skip and returns nil; validation and I/O failures are
// reported like Emit. Use this only on non-critical observability paths where a
// blocking event-log lock must not stall the caller.
//
// NOTE — deliberate divergence: this is the ONLY best-effort (lossy-on-
// contention) emit path among the JSONL log families. intent_audit.go and
// gui_event_log.go both use a blocking Lock(); TryEmit is the sole exception.
// The tradeoff is scoped on purpose: a TryEmit caller may silently drop its
// audit row under event-log lock contention, so it must be used only where the
// underlying state change is independently durable. The serena intent repair is
// the motivating caller — its WRITE to supervisor-intent.json commits under the
// held intent flock BEFORE these events emit (serena_intent_repair.go), so
// losing the observability row never loses the repair itself. Do not adopt
// TryEmit for an event that is the only record of a state mutation.
func (l *SupervisorEventLog) TryEmit(evt SupervisorEvent) error {
	prepared, err := PrepareSupervisorEvent(evt)
	if err != nil {
		return err
	}
	_, err = l.emitPrepared(prepared, emitTry, 0)
	return err
}

// supervisorEventWriteFn is the injectable file-write SEAM (P1-4 review
// fix). Production always resolves to (*SupervisorEventLog).writeEventLine;
// a test substitutes a fake that blocks past a caller's timeout budget to
// prove EmitWithTimeout's outer bound covers the write phase itself, not
// just lock acquisition. Reassigning this directly from production code is
// forbidden — SetSupervisorEventWriteFnForTest is the only allowed write
// path, and callers MUST restore it before the next test runs.
var supervisorEventWriteFn = func(l *SupervisorEventLog, raw []byte) error {
	return l.writeEventLine(raw)
}

// SetSupervisorEventWriteFnForTest installs a test file-write function.
// Returns an "uninstall" function tests defer to restore production wiring.
// Only supervisor_events_test.go invokes this.
func SetSupervisorEventWriteFnForTest(fn func(l *SupervisorEventLog, raw []byte) error) func() {
	prev := supervisorEventWriteFn
	supervisorEventWriteFn = fn
	return func() { supervisorEventWriteFn = prev }
}

// supervisorEventUnlockFn is the injectable cross-process-flock RELEASE seam,
// the release-side sibling of supervisorEventWriteFn. Production always
// resolves to (*flock.Flock).Unlock; a test substitutes a fake that fails
// persistently to prove that a failed release is PROPAGATED to the caller
// rather than discarded. gofrs/flock exposes no injection point of its own and
// l.lock is a concrete *flock.Flock, so a seam here is the only way to
// exercise the failure without a real locked-down filesystem.
//
// Reassigning this directly from production code is forbidden —
// SetSupervisorEventUnlockFnForTest is the only allowed write path, and
// callers MUST restore it before the next test runs.
var supervisorEventUnlockFn = func(l *SupervisorEventLog) error {
	return l.lock.Unlock()
}

// SetSupervisorEventUnlockFnForTest installs a test flock-release function.
// Returns an "uninstall" function tests defer to restore production wiring.
// Only supervisor_events_test.go invokes this.
func SetSupervisorEventUnlockFnForTest(fn func(l *SupervisorEventLog) error) func() {
	prev := supervisorEventUnlockFn
	supervisorEventUnlockFn = fn
	return func() { supervisorEventUnlockFn = prev }
}

// ReleaseSupervisorEventFlockForTest performs the REAL cross-process flock
// release for a log handle.
//
// It exists for tests in OTHER packages that install a failing
// SetSupervisorEventUnlockFnForTest. Such a fake models "the release syscall
// reported an error", but if it merely returns an error without releasing, the
// OS handle leaks — and on Windows that blocks the test's own t.TempDir()
// cleanup with a sharing violation. Calling this first and then returning the
// error reproduces exactly what a caller observes while keeping the handle
// reclaimable. Only tests may call it.
func ReleaseSupervisorEventFlockForTest(l *SupervisorEventLog) error {
	return l.lock.Unlock()
}

// joinSupervisorEventReleaseErr folds a cross-process-flock RELEASE failure
// into the emit result (review finding 2).
//
// Before this, `_ = l.lock.Unlock()` discarded the outcome, so a SUCCESSFUL
// write followed by a failed UnlockFileEx returned nil — a success verdict —
// while the flock stayed held. That is not a cosmetic loss: the supervisor and
// the install CLI both emit into this same log across processes, so a silently
// retained flock blocks every OTHER process's event-log write until this one
// exits, and the caller that caused it is told nothing.
//
// The release failure is wrapped in ErrSupervisorEventReleaseFailed so a caller
// can tell it apart from a write failure with errors.Is, and joined with (not
// substituted for) any write error so neither cause is lost.
func joinSupervisorEventReleaseErr(writeErr, releaseErr error) error {
	if releaseErr == nil {
		return writeErr
	}
	return errors.Join(writeErr, fmt.Errorf("%w: %w", ErrSupervisorEventReleaseFailed, releaseErr))
}

// emit's results are NAMED so the single deferred releaser below can fold a
// cross-process-flock RELEASE failure into the error every return path already
// produces (review finding 2). Every `return` here still states both results
// explicitly; the releaser only ever ADDS a release failure on top.
func (l *SupervisorEventLog) emitPrepared(prepared PreparedSupervisorEvent, mode eventLogEmitMode, timeout time.Duration) (pending *PendingSupervisorEventEmit, err error) {
	if err := validatePreparedSupervisorEvent(prepared); err != nil {
		return nil, err
	}
	raw := bytes.Clone(prepared.raw)
	writeFn := supervisorEventWriteFn

	// In-process serialization first — guards against two goroutines in the
	// same supervisor binary racing past the flock acquire.
	var ctx context.Context
	var cancel context.CancelFunc
	if mode == emitTimeout {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// A blocking Emit can hold mu while it waits indefinitely for the
		// cross-process flock. Poll rather than using Lock so this mode's
		// timeout covers that contention too.
		retry := time.NewTicker(eventLogEmitRetryDelay)
		defer retry.Stop()
		for !l.mu.TryLock() {
			select {
			case <-ctx.Done():
				return nil, ErrSupervisorEventEmitTimeout
			case <-retry.C:
			}
		}
	} else if mode == emitTry {
		if !l.mu.TryLock() {
			return nil, nil
		}
	} else {
		l.mu.Lock()
	}
	// l.mu is now held by this goroutine.
	//
	// SINGLE-OWNER RELEASE DISCIPLINE (P1 review fix). Both locks are owned
	// by the releaser below on EVERY exit path out of this function —
	// success, bounded timeout, a genuine non-timeout flock error, and a
	// panic — with EXACTLY ONE exception: the emitTimeout hand-off path,
	// which transfers ownership of both locks to the spawned worker
	// goroutine and records that transfer in `handedOff`.
	//
	// Before this fix each branch unlocked l.mu itself, and the emitTimeout
	// branch's `!locked` arm unlocked and then FELL THROUGH into the shared
	// `lockErr != nil` arm, which unlocked a second time. On a genuine
	// non-timeout flock error — e.g. gofrs/flock's setFh failing because the
	// state directory disappeared or is inaccessible — that second unlock hit
	// an already-unlocked sync.Mutex, which is an UNRECOVERABLE runtime fatal
	// ("sync: unlock of unlocked mutex"), not a catchable panic. Because
	// daemon recovery emits its audit row AFTER terminating a daemon but
	// BEFORE respawning it, that fatal left the daemon stopped.
	//
	// `flockHeld` (rather than a re-read of l.lock's internal state) is the
	// authoritative record of whether THIS call owns the cross-process flock,
	// so the theoretical (true, err) return that gofrs/flock does not
	// currently produce still releases the flock instead of leaking it.
	//
	// The releaser also PROPAGATES a flock-release failure into the returned
	// error (review finding 2). `_ = l.lock.Unlock()` used to discard it, so a
	// successful write followed by a failed UnlockFileEx returned SUCCESS while
	// the cross-process flock stayed held — blocking event-log writes in every
	// other process that emits here. The mutex unlock stays UNCONDITIONAL and
	// outside that branch: skipping it is the double-unlock class's mirror image
	// and would wedge this log for the rest of the process's life.
	//
	// Capture the release seam ONCE, for the same reason writeFn is captured
	// below: the emitTimeout worker is deliberately abandoned, so a package
	// global it read at release time could be read arbitrarily late and race a
	// test's Cleanup restore under -race.
	unlockFn := supervisorEventUnlockFn
	flockHeld := false
	handedOff := false
	defer func() {
		if handedOff {
			return
		}
		if flockHeld {
			if unlockErr := unlockFn(l); unlockErr != nil {
				// Record with the single process-scoped owner BEFORE folding
				// into err: every caller of this surface is free to discard
				// err (131 of them do), so the owner — not the return value —
				// is what makes a stranded flock observable.
				noteSupervisorEventLockReleaseFailed(l.path)
				err = joinSupervisorEventReleaseErr(err, unlockErr)
			}
		}
		l.mu.Unlock()
	}()

	// OS-level serialization across processes (e.g. supervisor +
	// install CLI emitting migration events concurrently). Each arm either
	// returns (releaser runs) or leaves BOTH locks held with flockHeld true.
	switch mode {
	case emitBlocking:
		if err := l.lock.Lock(); err != nil {
			return nil, fmt.Errorf("supervisor event log flock: %w", err)
		}
		flockHeld = true
	case emitTry:
		locked, err := l.lock.TryLock()
		flockHeld = locked
		if err != nil {
			return nil, fmt.Errorf("supervisor event log flock: %w", err)
		}
		if !locked {
			return nil, nil
		}
	case emitTimeout:
		locked, err := l.lock.TryLockContext(ctx, eventLogEmitRetryDelay)
		flockHeld = locked
		if !locked {
			// Timed out under contention (DeadlineExceeded) or a defensive
			// (false, nil): report the bounded skip so a mutation-audit caller
			// can fall back after leaving its critical path. A genuine
			// non-timeout lock error is reported as itself — and, unlike
			// before, returns from THIS arm instead of falling through into a
			// second unlock.
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				return nil, ErrSupervisorEventEmitTimeout
			}
			return nil, fmt.Errorf("supervisor event log flock: %w", err)
		}
		if err != nil {
			return nil, fmt.Errorf("supervisor event log flock: %w", err)
		}
	}

	// Both locks are held by this goroutine. The remaining work — rotation
	// check, file open, append write, close — is ordinary filesystem I/O
	// with no cancellable syscall surface (os.OpenFile, os.Rename,
	// (*os.File).Write/Close accept no context). A filesystem or antivirus
	// stall here previously blocked the caller indefinitely regardless of
	// mode, defeating EmitWithTimeout's documented "never hang forever"
	// contract (P1-4 review finding: the timeout only ever bounded lock
	// ACQUISITION, never the write itself). emitBlocking/emitTry already
	// accept an unbounded wait by contract (see their own doc comments), so
	// only emitTimeout gets the bounded wrapper below.
	// Both locks stay owned by the deferred releaser above, which frees them
	// after the write returns — including when the write panics.
	if mode != emitTimeout {
		return nil, l.writeEventBatch(raw, writeFn)
	}

	// Hand off both locks to a worker goroutine so this call can give up
	// waiting without unlocking out from under a write that may still be in
	// flight. The worker releases both locks itself when writeEventLine
	// returns, whether or not anyone is still waiting on it — mirroring the
	// owned-probe release discipline in
	// runEnsureAliveGUIRecoveryFree (internal/cli/supervise_ensure_alive.go):
	// the locks either return to this call while it is still waiting, or
	// this call gives up first and the abandoned goroutine finishes the
	// write and releases them whenever the stall clears. Either way a
	// successor Emit can never acquire either lock before the abandoned
	// write actually completes, so log-line interleaving is still
	// impossible.
	//
	// residual 2 review fix: `done` is also handed to the caller (wrapped in
	// a *PendingSupervisorEventEmit) on the timeout path below, so a caller
	// that needs idempotency against a late-but-successful write can await
	// this SAME operation instead of racing an independent duplicate.
	done := make(chan error, 1)
	// Capture the write function BEFORE spawning, and never re-read the
	// package-level variable from inside the worker.
	//
	// On the timeout path this goroutine is deliberately ABANDONED — it keeps
	// running and commits the row whenever the stall clears. That means its
	// lifetime is unbounded from the caller's point of view, so any global it
	// reads at write time it may read arbitrarily late. The only writer of
	// supervisorEventWriteFn is SetSupervisorEventWriteFnForTest, whose
	// returned uninstall runs in a test's Cleanup — so an abandoned worker
	// racing that restore is a real data race, and `-race` reports it in both
	// internal/api and internal/daemonrecovery.
	//
	// Production never rewrites the variable after init, so this was not a
	// production correctness bug — but it made the whole package unrunnable
	// under `-race`, and `-race` is exactly what proved the resolver
	// generation-ordering defect on this branch. Losing that tool is not an
	// acceptable trade.
	// Ownership of both locks transfers to the worker HERE. Setting the flag
	// before the `go` statement (both are touched only by this goroutine, so
	// there is no race with the worker) is what keeps the deferred releaser
	// from unlocking out from under a write that is still in flight.
	handedOff = true
	// The worker now owns the flock. Until it reports a release, this process
	// may be holding it, and that fact must be observable to a reader that has
	// no access to this call's return value.
	noteSupervisorEventLockHandoff(l.path)
	go func() {
		var (
			writeErr  error
			unlockErr error
		)
		// Both locks are released BEFORE the send on `done` (review finding 2).
		// The previous shape sent from the goroutine body, so its two deferred
		// releases ran AFTER the send — a caller awaiting this handle could
		// observe SUCCESS strictly before the release outcome even existed, and
		// a failed flock release was discarded on top of that. The inner
		// function keeps the releases DEFERRED (so a panicking write still frees
		// both locks) while moving the send after them.
		//
		// Release ORDER is deliberately unchanged: LIFO runs the flock unlock
		// first, then the mutex, matching the synchronous releaser above. The
		// mutex unlock stays unconditional.
		func() {
			defer l.mu.Unlock()
			defer func() { unlockErr = unlockFn(l) }()
			writeErr = l.writeEventBatch(raw, writeFn)
		}()
		// Close out the handoff with the worker's own release outcome. This
		// runs BEFORE the send on `done`, so a reader that observes the send
		// can never see a stale "outstanding" for this worker.
		noteSupervisorEventLockHandoffDone(l.path, unlockErr)
		done <- joinSupervisorEventReleaseErr(writeErr, unlockErr)
	}()
	select {
	case err := <-done:
		return nil, err
	case <-ctx.Done():
		return &PendingSupervisorEventEmit{done: done}, ErrSupervisorEventEmitTimeout
	}
}

// writeEventLine performs the rotation check, file open, and append write
// for an emit call that already holds both the in-process mutex and the
// cross-process flock. Callers MUST hold both locks before calling this and
// release them only after it returns (directly, or — for the bounded
// emitTimeout path in emit — via the worker goroutine that calls it).
// PersistPending atomically establishes a process-exit-safe handoff for the
// prepared row without acquiring the event-log flock. Publication uses a
// same-directory hard link so an existing digest carrier is never overwritten.
func (l *SupervisorEventLog) PersistPending(prepared PreparedSupervisorEvent) error {
	if err := validatePreparedSupervisorEvent(prepared); err != nil {
		return err
	}

	dir := l.path + supervisorEventPendingDirSuffix
	if err := l.pendingIO.mkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create supervisor event pending directory %s: %w", dir, err)
	}
	if err := l.pendingIO.chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure supervisor event pending directory %s: %w", dir, err)
	}

	finalPath := filepath.Join(dir, hex.EncodeToString(prepared.digest[:])+supervisorEventPendingFileSuffix)
	if existing, err := l.pendingIO.readBounded(finalPath, supervisorEventMaxBytes+1); err == nil {
		if bytes.Equal(existing, prepared.raw) {
			return nil
		}
		return fmt.Errorf("supervisor event pending digest/content collision at %s", finalPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing supervisor event pending carrier %s: %w", finalPath, err)
	}

	temp, err := l.pendingIO.createTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create supervisor event pending temp in %s: %w", dir, err)
	}
	tempPath := temp.Name()
	tempOpen := true
	defer func() {
		if tempOpen {
			_ = temp.Close()
		}
		_ = l.pendingIO.remove(tempPath)
	}()

	n, writeErr := temp.Write(prepared.raw)
	if writeErr == nil && n != len(prepared.raw) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return fmt.Errorf("write supervisor event pending temp %s: %w", tempPath, writeErr)
	}
	if syncErr := temp.Sync(); syncErr != nil {
		return fmt.Errorf("sync supervisor event pending temp %s: %w", tempPath, syncErr)
	}
	if closeErr := temp.Close(); closeErr != nil {
		tempOpen = false
		return fmt.Errorf("close supervisor event pending temp %s: %w", tempPath, closeErr)
	}
	tempOpen = false

	if linkErr := l.pendingIO.link(tempPath, finalPath); linkErr != nil {
		if errors.Is(linkErr, os.ErrExist) {
			existing, readErr := l.pendingIO.readBounded(finalPath, supervisorEventMaxBytes+1)
			if readErr != nil {
				return fmt.Errorf("verify raced supervisor event pending carrier %s: %w", finalPath, readErr)
			}
			if !bytes.Equal(existing, prepared.raw) {
				return fmt.Errorf("supervisor event pending digest/content collision at %s", finalPath)
			}
		} else {
			return fmt.Errorf("publish supervisor event pending carrier %s: %w", finalPath, linkErr)
		}
	}
	if removeErr := l.pendingIO.remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove supervisor event pending temp %s: %w", tempPath, removeErr)
	}
	return nil
}

// TryReplayPending opportunistically drains pending carriers. Lock contention
// is a successful no-op; every acquired resource is released on all exits.
func (l *SupervisorEventLog) TryReplayPending() (err error) {
	if !l.mu.TryLock() {
		return nil
	}
	defer l.mu.Unlock()

	locked, lockErr := l.lock.TryLock()
	if lockErr != nil {
		return fmt.Errorf("supervisor event log flock: %w", lockErr)
	}
	if !locked {
		return nil
	}
	defer func() {
		if unlockErr := supervisorEventUnlockFn(l); unlockErr != nil {
			// TryReplayPending's own release can strand the flock too, and its
			// error is routinely discarded (replay is opportunistic by
			// contract). Same owner, same reason.
			noteSupervisorEventLockReleaseFailed(l.path)
			err = joinSupervisorEventReleaseErr(err, unlockErr)
		}
	}()
	return l.replayPendingLocked()
}

func (l *SupervisorEventLog) writeEventBatch(raw []byte, writeFn func(*SupervisorEventLog, []byte) error) error {
	if err := l.replayPendingLocked(); err != nil {
		return err
	}
	return writeFn(l, raw)
}

func (l *SupervisorEventLog) replayPendingLocked() error {
	dir := l.path + supervisorEventPendingDirSuffix
	names, rawScanned, coverage, err := l.scanPendingCarrierNames(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		if coverage == supervisorEventPendingCoverageRawTraversalExhausted && len(names) == 0 {
			return errors.Join(
				&supervisorEventPendingCoverageError{directory: dir, rawScanned: rawScanned, rawBudget: supervisorEventPendingRawScanBudget},
				fmt.Errorf("scan supervisor event pending directory %s: %w", dir, err),
			)
		}
		return fmt.Errorf("scan supervisor event pending directory %s: %w", dir, err)
	}

	for _, name := range names {
		pendingPath := filepath.Join(dir, name)
		raw, readErr := l.pendingIO.readBounded(pendingPath, supervisorEventMaxBytes+1)
		if readErr != nil {
			return fmt.Errorf("read supervisor event pending carrier %s: %w", pendingPath, readErr)
		}
		digestBytes, decodeErr := hex.DecodeString(strings.TrimSuffix(name, supervisorEventPendingFileSuffix))
		if decodeErr != nil || len(digestBytes) != sha256.Size {
			return fmt.Errorf("invalid supervisor event pending carrier name %s", name)
		}
		var digest [sha256.Size]byte
		copy(digest[:], digestBytes)
		prepared := PreparedSupervisorEvent{raw: raw, digest: digest}
		if validErr := validatePendingSupervisorEventCarrier(prepared); validErr != nil {
			return fmt.Errorf("validate supervisor event pending carrier %s: %w", pendingPath, validErr)
		}

		activeMatch, activeErr := l.pendingIO.containsRecord(l.path, raw)
		if activeErr != nil {
			return fmt.Errorf("scan active supervisor event log %s: %w", l.path, activeErr)
		}
		backupMatch, backupErr := l.pendingIO.containsRecord(l.path+supervisorEventLogRotatedSuffix, raw)
		if backupErr != nil {
			return fmt.Errorf("scan rotated supervisor event log %s: %w", l.path+supervisorEventLogRotatedSuffix, backupErr)
		}
		if !activeMatch && !backupMatch {
			if appendErr := l.appendSupervisorEventLine(raw, true); appendErr != nil {
				return appendErr
			}
		}
		if removeErr := l.pendingIO.remove(pendingPath); removeErr != nil {
			return fmt.Errorf("retire supervisor event pending carrier %s: %w", pendingPath, removeErr)
		}
	}
	if coverage == supervisorEventPendingCoverageRawTraversalExhausted && len(names) == 0 {
		return &supervisorEventPendingCoverageError{directory: dir, rawScanned: rawScanned, rawBudget: supervisorEventPendingRawScanBudget}
	}
	return nil
}

type supervisorEventPendingCoverage uint8

const (
	supervisorEventPendingCoverageValidQuota supervisorEventPendingCoverage = iota
	supervisorEventPendingCoverageDirectoryComplete
	supervisorEventPendingCoverageRawTraversalExhausted
)

// supervisorEventPendingCoverageError makes a bounded zero-progress scan
// observable instead of silently allowing a stale raw-name prefix to starve a
// valid carrier forever.
type supervisorEventPendingCoverageError struct {
	directory  string
	rawScanned int
	rawBudget  int
}

func (e *supervisorEventPendingCoverageError) Error() string {
	return fmt.Sprintf("SUPERVISOR_EVENT_PENDING_SCAN_INCOMPLETE: pending replay coverage incomplete for %s after %d raw names (budget %d)", e.directory, e.rawScanned, e.rawBudget)
}

func (l *SupervisorEventLog) scanPendingCarrierNames(dir string) (finals []string, rawScanned int, coverage supervisorEventPendingCoverage, err error) {
	handle, openErr := l.pendingIO.openDir(dir)
	if errors.Is(openErr, os.ErrNotExist) {
		return nil, 0, supervisorEventPendingCoverageDirectoryComplete, nil
	}
	if openErr != nil {
		return nil, 0, supervisorEventPendingCoverageDirectoryComplete, fmt.Errorf("open supervisor event pending directory %s: %w", dir, openErr)
	}
	defer func() {
		if closeErr := handle.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close supervisor event pending directory %s: %w", dir, closeErr))
		}
	}()

	finals = make([]string, 0, supervisorEventPendingReplayLimit)
	for rawScanned < supervisorEventPendingRawScanBudget {
		pageLimit := supervisorEventPendingScanPageSize
		if remaining := supervisorEventPendingRawScanBudget - rawScanned; pageLimit > remaining {
			pageLimit = remaining
		}
		page, pageErr := handle.Readdirnames(pageLimit)
		rawScanned += len(page)
		for _, name := range page {
			if !isSupervisorEventPendingFilename(name) {
				continue
			}
			finals = append(finals, name)
			if len(finals) == supervisorEventPendingReplayLimit {
				sort.Strings(finals)
				return finals, rawScanned, supervisorEventPendingCoverageValidQuota, nil
			}
		}
		if errors.Is(pageErr, io.EOF) {
			sort.Strings(finals)
			return finals, rawScanned, supervisorEventPendingCoverageDirectoryComplete, nil
		}
		if pageErr != nil {
			return nil, rawScanned, supervisorEventPendingCoverageDirectoryComplete, fmt.Errorf("scan supervisor event pending directory %s: %w", dir, pageErr)
		}
		if len(page) == 0 {
			return nil, rawScanned, supervisorEventPendingCoverageDirectoryComplete, fmt.Errorf("scan supervisor event pending directory %s: empty page without EOF", dir)
		}
	}

	// Exact budget needs one bounded probe to distinguish completion from a
	// prefix that could still hide carriers later in the directory.
	probe, probeErr := handle.Readdirnames(1)
	rawScanned += len(probe)
	if probeErr != nil {
		if errors.Is(probeErr, io.EOF) && len(probe) == 0 {
			sort.Strings(finals)
			return finals, rawScanned, supervisorEventPendingCoverageDirectoryComplete, nil
		}
		if errors.Is(probeErr, io.EOF) && len(probe) == 1 {
			sort.Strings(finals)
			return finals, rawScanned, supervisorEventPendingCoverageRawTraversalExhausted, nil
		}
		return nil, rawScanned, supervisorEventPendingCoverageDirectoryComplete, fmt.Errorf("probe supervisor event pending directory %s: %w", dir, probeErr)
	}
	if len(probe) != 1 {
		return nil, rawScanned, supervisorEventPendingCoverageDirectoryComplete, fmt.Errorf("probe supervisor event pending directory %s: empty page without EOF", dir)
	}
	sort.Strings(finals)
	return finals, rawScanned, supervisorEventPendingCoverageRawTraversalExhausted, nil
}

// writeEventLine performs the ordinary unsynced current-row append. The caller
// owns both locks; replay uses appendSupervisorEventLine directly with sync.
func (l *SupervisorEventLog) writeEventLine(raw []byte) error {
	return l.appendSupervisorEventLine(raw, false)
}

func (l *SupervisorEventLog) appendSupervisorEventLine(raw []byte, syncWrite bool) error {
	if err := l.pendingIO.rotateIfNeeded(l.path); err != nil {
		return err
	}
	f, err := l.pendingIO.openAppend(l.path)
	if err != nil {
		return fmt.Errorf("open supervisor event log %s: %w", l.path, err)
	}
	open := true
	defer func() {
		if open {
			_ = f.Close()
		}
	}()

	stat, statErr := f.Stat()
	if statErr != nil {
		return fmt.Errorf("stat supervisor event log %s before append: %w", l.path, statErr)
	}
	appendCurrent := true
	if stat.Size() > 0 {
		var last [1]byte
		n, readErr := f.ReadAt(last[:], stat.Size()-1)
		if readErr != nil {
			return fmt.Errorf("read supervisor event log tail %s: %w", l.path, readErr)
		}
		if n != 1 {
			return fmt.Errorf("read supervisor event log tail %s: %w", l.path, io.ErrUnexpectedEOF)
		}
		if last[0] != '\n' {
			fragmentLen := len(raw) - 1
			if fragmentLen > 0 && stat.Size() >= int64(fragmentLen) {
				offset := stat.Size() - int64(fragmentLen)
				fragment := make([]byte, fragmentLen)
				n, readErr := f.ReadAt(fragment, offset)
				if readErr != nil {
					return fmt.Errorf("read supervisor event log fragment %s: %w", l.path, readErr)
				}
				if n != fragmentLen {
					return fmt.Errorf("read supervisor event log fragment %s: %w", l.path, io.ErrUnexpectedEOF)
				}
				atRecordBoundary := offset == 0
				if !atRecordBoundary {
					var preceding [1]byte
					n, readErr := f.ReadAt(preceding[:], offset-1)
					if readErr != nil {
						return fmt.Errorf("read supervisor event log fragment boundary %s: %w", l.path, readErr)
					}
					if n != 1 {
						return fmt.Errorf("read supervisor event log fragment boundary %s: %w", l.path, io.ErrUnexpectedEOF)
					}
					atRecordBoundary = preceding[0] == '\n'
				}
				appendCurrent = !(atRecordBoundary && bytes.Equal(fragment, raw[:fragmentLen]))
			}
			n, writeErr := f.Write([]byte{'\n'})
			if writeErr == nil && n != 1 {
				writeErr = io.ErrShortWrite
			}
			if writeErr != nil {
				return fmt.Errorf("separate incomplete supervisor event log tail %s: %w", l.path, writeErr)
			}
		}
	}

	if appendCurrent {
		n, writeErr := f.Write(raw)
		if writeErr == nil && n != len(raw) {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			return fmt.Errorf("write supervisor event log %s: %w", l.path, writeErr)
		}
	}
	if syncWrite {
		if syncErr := f.Sync(); syncErr != nil {
			return fmt.Errorf("sync supervisor event log %s: %w", l.path, syncErr)
		}
	}
	if closeErr := f.Close(); closeErr != nil {
		open = false
		return fmt.Errorf("close supervisor event log %s: %w", l.path, closeErr)
	}
	open = false
	return nil
}

func marshalSupervisorEventLine(evt SupervisorEvent) ([]byte, error) {
	if evt.Event == "" {
		return nil, ErrSupervisorEventMissingEvent
	}
	if evt.Source == "" {
		return nil, ErrSupervisorEventMissingSource
	}

	// Identity-oversize gate. Identity fields (Event/Source/TaskName)
	// are never truncated per §35; if any exceeds the 1024-byte cap,
	// fail closed so the post-truncation re-marshal cannot produce a
	// silent oversize entry. Mirrors AuditIdentityFieldByteCap
	// discipline in intent_audit.go (§51).
	if len(evt.Event) > supervisorEventIdentityCap ||
		len(evt.Source) > supervisorEventIdentityCap ||
		len(evt.TaskName) > supervisorEventIdentityCap {
		return nil, ErrSupervisorEventIdentityOversize
	}

	// Auto-fill envelope defaults.
	if evt.SchemaVersion == "" {
		evt.SchemaVersion = SupervisorEventSchemaVersion
	}
	if evt.TS == "" {
		evt.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if evt.Severity == "" {
		evt.Severity = SupervisorEventSeverityInfo
	}

	// Marshal once to measure; if oversize, truncate body and
	// re-marshal. Identity fields are off-limits per §35.
	raw, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("supervisor event log marshal: %w", err)
	}
	if len(raw) > supervisorEventMaxBytes {
		evt.Body = map[string]any{
			"_truncated_note": fmt.Sprintf("body fields exceeded %dKB cap", supervisorEventMaxBytes/1024),
		}
		evt.Truncated = true
		raw, err = json.Marshal(evt)
		if err != nil {
			return nil, fmt.Errorf("supervisor event log re-marshal after truncation: %w", err)
		}
		// Post-truncation re-check. With the identity gate above the
		// envelope (sans Body) is bounded well below 16 KB so this
		// branch should be unreachable; we keep it as a defense in
		// depth so a future schema growth (new envelope fields)
		// cannot silently leak an oversize line into the log. Drop
		// the entry with a sentinel placeholder.
		if len(raw) > supervisorEventMaxBytes {
			placeholder := SupervisorEvent{
				SchemaVersion: SupervisorEventSchemaVersion,
				TS:            evt.TS,
				Severity:      SupervisorEventSeverityWarn,
				Source:        evt.Source,
				Event:         "log-entry-dropped-oversize",
				TaskName:      evt.TaskName,
				Truncated:     true,
			}
			raw, _ = json.Marshal(placeholder)
		}
	}
	return append(raw, '\n'), nil
}

func validatePreparedSupervisorEvent(prepared PreparedSupervisorEvent) error {
	raw := prepared.raw
	if len(raw) == 0 {
		return errors.New("supervisor event pending: empty prepared record")
	}
	if len(raw) > supervisorEventMaxBytes+1 {
		return fmt.Errorf("supervisor event pending: prepared record is %d bytes, maximum is %d", len(raw), supervisorEventMaxBytes+1)
	}
	if raw[len(raw)-1] != '\n' || bytes.Count(raw, []byte{'\n'}) != 1 {
		return errors.New("supervisor event pending: prepared record must contain exactly one terminal newline")
	}
	if sha256.Sum256(raw) != prepared.digest {
		return errors.New("supervisor event pending: prepared record digest mismatch")
	}
	return nil
}

// validatePendingSupervisorEventCarrier adds structural envelope validation at
// the corruptible on-disk boundary. Body stays a RawMessage: decoding and
// remarshal here would narrow SupervisorEvent.Body's admitted JSON domain
// through float64 conversion or custom-Marshaler loss.
func validatePendingSupervisorEventCarrier(prepared PreparedSupervisorEvent) error {
	if err := validatePreparedSupervisorEvent(prepared); err != nil {
		return err
	}
	var envelope struct {
		SchemaVersion string          `json:"schema_version"`
		TS            string          `json:"ts"`
		Severity      string          `json:"severity"`
		Source        string          `json:"source"`
		Event         string          `json:"event"`
		TaskName      string          `json:"task_name,omitempty"`
		Body          json.RawMessage `json:"body,omitempty"`
		Truncated     bool            `json:"_truncated,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(prepared.raw[:len(prepared.raw)-1]))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("supervisor event pending: invalid JSONL record: %w", err)
	}
	if err := ensureSupervisorEventJSONEOF(decoder); err != nil {
		return fmt.Errorf("supervisor event pending: invalid JSONL record: %w", err)
	}
	if envelope.SchemaVersion == "" || envelope.TS == "" || envelope.Severity == "" {
		return errors.New("supervisor event pending: missing normalized envelope defaults")
	}
	if envelope.Event == "" {
		return ErrSupervisorEventMissingEvent
	}
	if envelope.Source == "" {
		return ErrSupervisorEventMissingSource
	}
	if len(envelope.Event) > supervisorEventIdentityCap ||
		len(envelope.Source) > supervisorEventIdentityCap ||
		len(envelope.TaskName) > supervisorEventIdentityCap {
		return ErrSupervisorEventIdentityOversize
	}
	if body := bytes.TrimSpace(envelope.Body); len(body) > 0 && body[0] != '{' {
		return errors.New("supervisor event pending: body must be a JSON object when present")
	}
	return nil
}

func ensureSupervisorEventJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values in one supervisor event record")
}

func isSupervisorEventPendingFilename(name string) bool {
	if !strings.HasSuffix(name, supervisorEventPendingFileSuffix) {
		return false
	}
	digest := strings.TrimSuffix(name, supervisorEventPendingFileSuffix)
	if len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size
}

func defaultSupervisorEventPendingIO() supervisorEventPendingIO {
	return supervisorEventPendingIO{
		mkdirAll:    os.MkdirAll,
		chmod:       os.Chmod,
		createTemp:  defaultCreateSupervisorEventPendingTemp,
		readBounded: readSupervisorEventFileBounded,
		openDir:     func(path string) (supervisorEventPendingDir, error) { return os.Open(path) },
		link:        os.Link,
		rotateIfNeeded: func(path string) error {
			if size, ok := supervisorEventLogFileSize(path); ok && size >= supervisorEventLogRotateSize {
				return rotateSupervisorEventLogFile(path)
			}
			return nil
		},
		openAppend: func(path string) (supervisorEventFile, error) {
			return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
		},
		containsRecord: retainedSupervisorEventLogContainsRecord,
		remove:         os.Remove,
	}
}

func defaultCreateSupervisorEventPendingTemp(dir, pattern string) (supervisorEventFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

func readSupervisorEventFileBounded(path string, maxBytes int64) (raw []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	raw, err = io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("file exceeds %d-byte bound", maxBytes)
	}
	return raw, nil
}

func retainedSupervisorEventLogContainsRecord(path string, want []byte) (found bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	reader := bufio.NewReaderSize(f, supervisorEventMaxBytes+2)
	for {
		line, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) {
			// The cap applies to complete retained records. Drain this
			// over-cap candidate without retaining it in memory so EOF can
			// distinguish a harmless incomplete tail from a complete oversize
			// record.
			for errors.Is(readErr, bufio.ErrBufferFull) {
				_, readErr = reader.ReadSlice('\n')
			}
			if errors.Is(readErr, io.EOF) {
				return false, nil
			}
			if readErr == nil {
				return false, fmt.Errorf("retained supervisor event record exceeds %d-byte cap", supervisorEventMaxBytes+1)
			}
			return false, readErr
		}
		if errors.Is(readErr, io.EOF) {
			// A final incomplete fragment is intentionally not a retained row,
			// regardless of its length.
			return false, nil
		}
		if len(line) > supervisorEventMaxBytes+1 {
			return false, fmt.Errorf("retained supervisor event record exceeds %d-byte cap", supervisorEventMaxBytes+1)
		}
		if readErr == nil {
			if bytes.Equal(line, want) {
				return true, nil
			}
			continue
		}
		return false, readErr
	}
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// supervisorEventLogFileSize returns (size, true) when path exists,
// (0, false) otherwise. Mirrors guiEventLogFileSize /
// watchdogLogFileSize so a fresh log file is treated as below the
// rotation threshold.
func supervisorEventLogFileSize(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}

// rotateSupervisorEventLogFile renames the active log to ${path}.1,
// replacing any existing .1 backup. Mirrors
// rotateGUIEventLogFile / rotateWatchdogLogFile. Per-file delete
// failures on the existing .1 are propagated so the caller can
// surface them (rather than silently overwriting the rename).
func rotateSupervisorEventLogFile(path string) error {
	return rotateLogFileToBackup(path)
}

// rotateLogFileToBackup is the single owner of the "rename active ->
// ${path}.1, replacing any existing backup" mechanic shared by the
// supervisor event log and the supervisor stderr sink. Kept as one owner
// so the two cannot drift on the .1-retention or vanished-file semantics.
// Per-file delete failures on the existing .1 are propagated so the caller
// can surface them (rather than silently overwriting the rename).
func rotateLogFileToBackup(path string) error {
	target := path + supervisorEventLogRotatedSuffix
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove existing rotated %s: %w", target, err)
	}
	if err := os.Rename(path, target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // race: file vanished between Stat and Rename, no-op
		}
		return fmt.Errorf("rotate log file %s: %w", path, err)
	}
	return nil
}

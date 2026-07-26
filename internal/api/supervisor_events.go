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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

// Wait blocks up to timeout for the abandoned worker to finish its write.
// Returns the worker's own completion error (nil on a successful append), or
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
	path string
	lock *flock.Flock
	mu   sync.Mutex
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
		path: path,
		lock: flock.New(path + supervisorEventLogLockSuffix),
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
	_, err := l.emit(evt, emitBlocking, 0)
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
	_, err := l.emit(evt, emitTimeout, timeout)
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
	return l.emit(evt, emitTimeout, timeout)
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
	_, err := l.emit(evt, emitTry, 0)
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

func (l *SupervisorEventLog) emit(evt SupervisorEvent, mode eventLogEmitMode, timeout time.Duration) (*PendingSupervisorEventEmit, error) {
	raw, err := marshalSupervisorEventLine(evt)
	if err != nil {
		return nil, err
	}

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
	flockHeld := false
	handedOff := false
	defer func() {
		if handedOff {
			return
		}
		if flockHeld {
			_ = l.lock.Unlock()
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
		return nil, supervisorEventWriteFn(l, raw)
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
	writeFn := supervisorEventWriteFn
	// Ownership of both locks transfers to the worker HERE. Setting the flag
	// before the `go` statement (both are touched only by this goroutine, so
	// there is no race with the worker) is what keeps the deferred releaser
	// from unlocking out from under a write that is still in flight.
	handedOff = true
	go func() {
		defer l.mu.Unlock()
		defer func() { _ = l.lock.Unlock() }()
		done <- writeFn(l, raw)
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
func (l *SupervisorEventLog) writeEventLine(raw []byte) error {
	// Rotation check.
	if size, ok := supervisorEventLogFileSize(l.path); ok && size >= supervisorEventLogRotateSize {
		if rotErr := rotateSupervisorEventLogFile(l.path); rotErr != nil {
			return rotErr
		}
	}

	// Append line. O_APPEND + O_CREATE + O_WRONLY 0o600 mirrors
	// gui_event_log.go's defaultGUIEventLogAppend.
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open supervisor event log %s: %w", l.path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("write supervisor event log %s: %w", l.path, err)
	}
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

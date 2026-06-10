// Package api — Task 3 intent-audit append-only JSON Lines log
// (watchdog plan v13 §9, §20, §25, §26, §35, §37, §38, §48, §52, §55,
// §61, §62).
//
// intent_audit.go owns:
//
//   - The IntentAuditEntry schema (full v9 spec) including caller fields,
//     unexported sealed `systemEntry` flag, and Priority field.
//   - The MarshalJSON / UnmarshalJSON pair that projects systemEntry onto
//     a lowercased "system_entry" wire field on output but DISCARDS that
//     field on input (sealed pattern — system_entry can never be set
//     from external JSON, only via newSystemAuditEntry()).
//   - The Options-pattern constructors NewIntentAuditEntry (public) +
//     newSystemAuditEntry (package-private; sets systemEntry=true).
//   - AppendIntentAudit on *API: fail-open auto-fill of caller fields,
//     identity-oversize rejection (>1KB Task or CallerUser → return
//     ErrIdentityOversize so callers fail closed per §51), 16KB cap with
//     identity preservation + truncation marker + _task_hash, 10MB JSON-
//     Lines rotation with idempotent retry on audit-rotated self-event
//     write failure (per §26).
//   - RedactIntentAuditEntryForNonOwner display helper — exempts system
//     entries (§37, §48).
//   - currentOSUser helper consumed by both auto-fill and redaction.
//   - The CallerStartTime() helper bound to per-OS implementations in
//     intent_audit_caller_*.go (Windows GetProcessTimes, Linux
//     /proc/self/stat, macOS sysctl).
//
// Wiring:
//
//   - init() binds appendIntentAuditFn (api_surfaces.go seam) to a thin
//     adapter over (*API).AppendIntentAudit so daemon_intent.go's
//     WriteDaemonIntent / ClearDaemonIntent reach real disk.
//   - Task 2 audit-write TODOs (set-intent / clear-intent) are now
//     fulfilled by daemon_intent.go calling (*API).AppendIntentAudit.
//
// Test seams:
//
//   - auditAppendWriteFn replaces the disk-append step. Tests inject
//     targeted failures (e.g. fail only the audit-rotated self-event)
//     by inspecting the bytes argument.
//   - auditRotatedFailureLogFn receives the error from a failed
//     audit-rotated self-event append. The default is silent (the
//     v0.4.x watchdog.log sink was deleted in v0.6 Phase D).
//
// Concurrency: AppendIntentAudit takes the audit-log flock before any
// disk read (size check, rotation, append). Concurrent callers are
// serialized; partial writes cannot interleave.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// ---------------------------------------------------------------------------
// File layout constants (§26).
// ---------------------------------------------------------------------------

// intentAuditFileLeaf is the canonical file name (relative to
// DaemonStateDir) holding the JSON Lines audit log.
const intentAuditFileLeaf = "intent-audit.log"

// intentAuditLockLeaf is the sibling file used by gofrs/flock so
// concurrent AppendIntentAudit callers serialize rather than interleave.
const intentAuditLockLeaf = "intent-audit.log.lock"

// intentAuditRotatedSuffix is appended to the rotated file name.
// Plan §26 specifies a single .1 backup target; older rotations are
// not retained (the prior .1 is overwritten on rotation).
const intentAuditRotatedSuffix = ".1"

// AuditRotateSizeBytes is the rotation threshold per §26: when the
// active log reaches this size or larger, the next AppendIntentAudit
// call rotates it to ${leaf}.1 and emits an audit-rotated self-event.
const AuditRotateSizeBytes int64 = 10 * 1024 * 1024

// AuditEntryMaxBytes is the per-line size cap per §35: marshaled lines
// must not exceed this. Identity fields (Task, CallerUser) are never
// truncated; non-identity fields are truncated to fit. Multi-field
// oversize falls back to a placeholder line.
const AuditEntryMaxBytes = 16 * 1024

// AuditIdentityFieldByteCap is the per-identity-field byte ceiling per
// §35: real Task Scheduler task names are <100 bytes; 1KB gives ample
// headroom while still rejecting oversize identifiers (which themselves
// are a red flag — a 32KB task name is almost certainly malicious or
// corrupted manifest).
const AuditIdentityFieldByteCap = 1024

// auditFixedSchemaOverhead is a conservative budget reserved for the
// fixed JSON schema scaffolding (field names, separators, brackets,
// fixed-shape values like ts and caller_pid). Empirically ~256 bytes;
// rounding up to 512 covers worst-case CallerExe paths.
const auditFixedSchemaOverhead = 512

// ---------------------------------------------------------------------------
// Errors (§35, §51).
// ---------------------------------------------------------------------------

// ErrIdentityOversize is returned by AppendIntentAudit when the entry
// has Task or CallerUser longer than AuditIdentityFieldByteCap. Per
// plan §35 + §51: identity fields are NEVER truncated; the caller is
// expected to fail closed (mcphub stop without --force, mcphub install,
// watchdog driver) so a malicious oversized identifier never reaches
// downstream sinks. Distinct from ErrEntryOversize (daemon_intent.go)
// so callers can distinguish "intent file rejected this name" from
// "audit log rejected this name."
var ErrIdentityOversize = errors.New("api: intent audit entry exceeds identity-field byte cap (Task or CallerUser >1KB)")

// ---------------------------------------------------------------------------
// Test seams.
// ---------------------------------------------------------------------------

// auditAppendWriteFn, when non-nil, replaces the disk-append path inside
// AppendIntentAudit. Tests inject targeted failures (e.g., fail only
// the audit-rotated self-event by inspecting the bytes argument).
var auditAppendWriteFn func(path string, line []byte) error

// auditRotatedFailureLogFn, when non-nil, receives the error from a
// failed audit-rotated self-event append. Production wires this to
// watchdog.log in Task 9 — for Task 3 the default is silent.
var auditRotatedFailureLogFn func(err error)

// ---------------------------------------------------------------------------
// Schema (plan v13 Task 3 v9-spec).
// ---------------------------------------------------------------------------

// IntentAuditEntry is one JSON Lines row in intent-audit.log. The
// schema (plan §35 v9-spec) is identity-preserving: Task and CallerUser
// are NEVER truncated; non-identity fields (Reason, Note, CallerExe,
// Who) are truncated as needed to fit AuditEntryMaxBytes.
//
// Sealed systemEntry pattern (§48): the systemEntry flag is unexported
// and can only be set via newSystemAuditEntry. JSON unmarshal IGNORES
// any "system_entry" field on input — external JSON cannot forge the
// system-entry exemption used by RedactIntentAuditEntryForNonOwner.
//
// Field semantics:
//
//   - TS / CallerStartTime: UTC RFC3339Nano. AppendIntentAudit
//     normalizes to UTC + auto-populates if zero.
//   - Task / CallerUser: identity fields. >1KB → ErrIdentityOversize.
//   - Before / After: optional snapshots of the DaemonIntent before
//     and after a set-intent / clear-intent mutation.
//   - CallerPID / CallerExe / CallerUser: caller fingerprint
//     auto-populated by AppendIntentAudit when zero.
//   - Reason / Priority: free-form (subject to truncation) and "high"
//     vs "" enum, respectively. Priority empty is omitted on the wire.
//   - PrevSizeBytes / PrevPath / Note: populated on audit-rotated
//     self-events only; omitempty on the wire.
type IntentAuditEntry struct {
	TS              time.Time     // UTC RFC3339Nano
	Who             string        // operator/caller identity (truncatable)
	Action          string        // canonical action label
	Task            string        // identity field (>1KB → reject)
	Before          *DaemonIntent // pre-mutation snapshot (omitempty)
	After           *DaemonIntent // post-mutation snapshot (omitempty)
	CallerPID       int           // os.Getpid() at write time
	CallerExe       string        // resolved executable path
	CallerStartTime time.Time     // UTC RFC3339Nano per-OS conversion
	CallerUser      string        // identity field (>1KB → reject)
	Reason          string        // free-form (truncatable; omitempty)
	Priority        string        // "high" or "" (omitempty)

	// Rotation-only fields. Populated by newSystemAuditEntry with
	// WithRotationSize / WithRotationPrevPath / WithNote during
	// audit-rotated self-event minting.
	PrevSizeBytes int64  // bytes in the rotated file (omitempty)
	PrevPath      string // path of the rotated file (omitempty)
	Note          string // free-form note on rotation (omitempty)

	// systemEntry is set true ONLY by newSystemAuditEntry. JSON
	// marshal projects this to "system_entry" on the wire; JSON
	// unmarshal DISCARDS the field on input (sealed pattern, §48).
	systemEntry bool
}

// auditWire is the JSON-side projection of IntentAuditEntry. It exists
// to expose a "system_entry" wire field while keeping the systemEntry
// flag unexported on the public struct. UnmarshalJSON populates this
// shape but discards the SystemEntry field (sealed pattern, §48).
type auditWire struct {
	TS              time.Time     `json:"ts"`
	Who             string        `json:"who"`
	Action          string        `json:"action"`
	Task            string        `json:"task"`
	Before          *DaemonIntent `json:"before,omitempty"`
	After           *DaemonIntent `json:"after,omitempty"`
	CallerPID       int           `json:"caller_pid"`
	CallerExe       string        `json:"caller_exe"`
	CallerStartTime time.Time     `json:"caller_start_time"`
	CallerUser      string        `json:"caller_user"`
	Reason          string        `json:"reason,omitempty"`
	Priority        string        `json:"priority,omitempty"`
	PrevSizeBytes   int64         `json:"prev_size_bytes,omitempty"`
	PrevPath        string        `json:"prev_path,omitempty"`
	Note            string        `json:"note,omitempty"`
	SystemEntry     bool          `json:"system_entry,omitempty"`
}

// MarshalJSON projects systemEntry into the lowercased "system_entry"
// wire field per §48 forensic-visibility contract. Time fields are
// already serialized as RFC3339Nano by encoding/json; the TS / start-
// time helpers normalize to UTC at construction so the on-disk
// representation is stable across operating systems.
func (e IntentAuditEntry) MarshalJSON() ([]byte, error) {
	w := auditWire{
		TS:              e.TS,
		Who:             e.Who,
		Action:          e.Action,
		Task:            e.Task,
		Before:          e.Before,
		After:           e.After,
		CallerPID:       e.CallerPID,
		CallerExe:       e.CallerExe,
		CallerStartTime: e.CallerStartTime,
		CallerUser:      e.CallerUser,
		Reason:          e.Reason,
		Priority:        e.Priority,
		PrevSizeBytes:   e.PrevSizeBytes,
		PrevPath:        e.PrevPath,
		Note:            e.Note,
		SystemEntry:     e.systemEntry,
	}
	return json.Marshal(w)
}

// UnmarshalJSON populates the entry from JSON but DISCARDS any
// "system_entry" field. Per §48 sealed pattern: external JSON cannot
// forge the system flag — only newSystemAuditEntry (in-process) can.
// rehydrateSystemEntryFromTrustedSource is the package-private
// rehydrator used by status-command audit-tail readers (same-package
// trust; never reaches external untrusted JSON).
func (e *IntentAuditEntry) UnmarshalJSON(data []byte) error {
	var w auditWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	e.TS = w.TS
	e.Who = w.Who
	e.Action = w.Action
	e.Task = w.Task
	e.Before = w.Before
	e.After = w.After
	e.CallerPID = w.CallerPID
	e.CallerExe = w.CallerExe
	e.CallerStartTime = w.CallerStartTime
	e.CallerUser = w.CallerUser
	e.Reason = w.Reason
	e.Priority = w.Priority
	e.PrevSizeBytes = w.PrevSizeBytes
	e.PrevPath = w.PrevPath
	e.Note = w.Note
	// systemEntry intentionally NOT set from w.SystemEntry — sealed.
	return nil
}

// IsSystemEntry reports whether the entry was minted by
// newSystemAuditEntry (rotation, watchdog-self-quarantine, ...).
// Used by RedactIntentAuditEntryForNonOwner to skip caller_user
// redaction on system entries (§37, §48).
func (e IntentAuditEntry) IsSystemEntry() bool { return e.systemEntry }

// rehydrateSystemEntryFromTrustedSource is the package-private setter
// used by audit-tail readers in the same package when reading entries
// that were emitted as system events (e.g., the status command needs
// to render the audit-rotated self-event without redacting its
// "<rotation-system>" sentinel CallerUser). Never called from external
// untrusted JSON paths.
//
//nolint:unused // referenced by future Task 9 status command.
func rehydrateSystemEntryFromTrustedSource(e *IntentAuditEntry, raw bool) {
	e.systemEntry = raw
}

// ---------------------------------------------------------------------------
// Options pattern (§52, §61).
// ---------------------------------------------------------------------------

// IntentAuditEntryOption configures an IntentAuditEntry being built
// via NewIntentAuditEntry / newSystemAuditEntry. The plan §61 example
// uses WithAction, WithPriority, WithReason, WithTask, WithBefore,
// WithAfter, WithWho — Task 3 implements all of those plus the
// rotation-specific helpers (WithNote, WithRotationSize,
// WithRotationPrevPath).
type IntentAuditEntryOption func(*IntentAuditEntry)

// WithAction sets the Action field.
func WithAction(action string) IntentAuditEntryOption {
	return func(e *IntentAuditEntry) { e.Action = action }
}

// WithTask sets the Task identity field. Per §35: NEVER truncated;
// AppendIntentAudit returns ErrIdentityOversize if >1KB.
func WithTask(task string) IntentAuditEntryOption {
	return func(e *IntentAuditEntry) { e.Task = task }
}

// WithWho sets the Who field (operator/caller identity).
func WithWho(who string) IntentAuditEntryOption {
	return func(e *IntentAuditEntry) { e.Who = who }
}

// WithReason sets the Reason free-form field.
func WithReason(reason string) IntentAuditEntryOption {
	return func(e *IntentAuditEntry) { e.Reason = reason }
}

// WithPriority sets the Priority enum-like field. Plan §55 v9 specifies
// "high" or "" (default low). Empty is omitted on the wire.
func WithPriority(p string) IntentAuditEntryOption {
	return func(e *IntentAuditEntry) { e.Priority = p }
}

// WithBefore captures the pre-mutation DaemonIntent snapshot.
func WithBefore(intent *DaemonIntent) IntentAuditEntryOption {
	return func(e *IntentAuditEntry) { e.Before = intent }
}

// WithAfter captures the post-mutation DaemonIntent snapshot.
func WithAfter(intent *DaemonIntent) IntentAuditEntryOption {
	return func(e *IntentAuditEntry) { e.After = intent }
}

// WithNote sets the Note free-form field. Used by audit-rotated
// self-events (§26) to carry "rotation triggered at 10MB".
func WithNote(note string) IntentAuditEntryOption {
	return func(e *IntentAuditEntry) { e.Note = note }
}

// WithRotationSize sets PrevSizeBytes for audit-rotated self-events.
func WithRotationSize(n int64) IntentAuditEntryOption {
	return func(e *IntentAuditEntry) { e.PrevSizeBytes = n }
}

// WithRotationPrevPath sets PrevPath for audit-rotated self-events.
func WithRotationPrevPath(p string) IntentAuditEntryOption {
	return func(e *IntentAuditEntry) { e.PrevPath = p }
}

// ---------------------------------------------------------------------------
// Sealed constructors (§48).
// ---------------------------------------------------------------------------

// NewIntentAuditEntry constructs a non-system audit entry. systemEntry
// is left false; the entry is subject to caller_user redaction by
// RedactIntentAuditEntryForNonOwner if the running OS user does not
// match the entry's CallerUser at display time.
//
// Caller-fingerprint fields (TS, CallerPID, CallerExe, CallerStartTime,
// CallerUser) are auto-populated by AppendIntentAudit when zero —
// callers may pre-fill any of them if a different value is needed
// (e.g., tests forcing an oversized CallerUser).
func NewIntentAuditEntry(opts ...IntentAuditEntryOption) IntentAuditEntry {
	e := IntentAuditEntry{}
	for _, opt := range opts {
		opt(&e)
	}
	return e
}

// newSystemAuditEntry constructs a system audit entry with
// systemEntry=true. Package-private; only the rotation path inside
// this file may call this. External callers cannot forge the system flag.
func newSystemAuditEntry(action string, opts ...IntentAuditEntryOption) IntentAuditEntry {
	e := IntentAuditEntry{Action: action}
	for _, opt := range opts {
		opt(&e)
	}
	e.systemEntry = true
	return e
}

// ---------------------------------------------------------------------------
// Display-only redaction (§37, §48, §52).
// ---------------------------------------------------------------------------

// RedactIntentAuditEntryForNonOwner returns a display-safe copy of e.
// System entries (e.IsSystemEntry()=true via newSystemAuditEntry) are
// returned unchanged — their CallerUser sentinel ("<rotation-system>")
// is meaningful as-is and must not be replaced with
// "<redacted-non-owner>".
//
// Non-system entries with CallerUser != currentOSUser() get their
// CallerUser replaced with "<redacted-non-owner>". This guards against
// a non-owner reading another user's audit tail via the watchdog
// status command.
//
// Pure: no I/O beyond the os/user lookup the redaction cares about
// (which is itself cheap and idempotent).
func RedactIntentAuditEntryForNonOwner(e IntentAuditEntry) IntentAuditEntry {
	if e.IsSystemEntry() {
		return e // §48 sealed-constructor exemption — never redact.
	}
	if e.CallerUser != currentOSUser() {
		e.CallerUser = "<redacted-non-owner>"
	}
	return e
}

// currentOSUser returns the running process's username (best-effort).
// Returns "<unknown>" on lookup failure so downstream code can still
// match the constant rather than crashing on nil.
func currentOSUser() string {
	u, err := user.Current()
	if err != nil || u == nil {
		return "<unknown>"
	}
	name := u.Username
	// Strip Windows DOMAIN\ prefix to match scheduler.windowsScheduler's
	// principal handling (scheduler_windows.go).
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// ---------------------------------------------------------------------------
// AppendIntentAudit — production write path (§9, §20, §25, §26, §35).
// ---------------------------------------------------------------------------

// AppendIntentAudit serializes the entry as a JSON Line and appends it
// to the per-user intent-audit.log under flock. Returns ErrIdentityOversize
// when Task or CallerUser exceed AuditIdentityFieldByteCap (>1KB) so
// callers can fail closed per §51 (mcphub stop, mcphub install,
// watchdog driver).
//
// 16KB cap (§35): identity fields are never truncated. If the marshaled
// entry exceeds AuditEntryMaxBytes, the longest non-identity string
// field (typically Reason, then Note, then CallerExe, then Who) is
// truncated and the entry gains _truncated:true + _truncated_field +
// _task_hash markers. If truncation alone cannot fit, the entry is
// replaced by a placeholder line:
//
//	{"ts":"...","action":"log-entry-dropped-oversize","reason":"...","_truncated":true,"_task_hash":"..."}
//
// Rotation (§26): at the start of each call, if the existing log is
// >= AuditRotateSizeBytes, os.Rename(*.log, *.log.1) and emit an
// audit-rotated self-event via newSystemAuditEntry. Self-event write
// failures are logged via auditRotatedFailureLogFn (Task 9 wiring) and
// do NOT propagate — idempotent retry: the next successful append
// goes through normally; this rotation's self-event is permanently
// lost but the rotation itself is recorded only in watchdog.log.
//
// Concurrency: holds gofrs/flock on intent-audit.log.lock for the
// duration of the call. Concurrent callers serialize.
func (a *API) AppendIntentAudit(entry IntentAuditEntry) error {
	// 1. Auto-populate caller fields for any zero-valued slot.
	fillCallerFields(&entry)

	// 2. Identity oversize gate — applied AFTER auto-fill so a caller
	//    that pre-set a >1KB CallerUser still fails closed.
	if len(entry.Task) > AuditIdentityFieldByteCap {
		return ErrIdentityOversize
	}
	if len(entry.CallerUser) > AuditIdentityFieldByteCap {
		return ErrIdentityOversize
	}

	// 3. Resolve paths.
	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, intentAuditFileLeaf)
	lockPath := filepath.Join(dir, intentAuditLockLeaf)

	// 4. Acquire flock so concurrent appenders + the rotation step do
	//    not interleave.
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("audit flock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	// 5. Rotation check + audit-rotated self-event (§26).
	if size, ok := auditFileSize(path); ok && size >= AuditRotateSizeBytes {
		if rotErr := rotateAuditFile(path); rotErr != nil {
			return rotErr
		}
		// Self-event: best-effort. Failure logged via seam, no retry.
		writeRotatedSelfEvent(path, size)
	}

	// 6. Marshal entry within the 16KB budget (§35).
	line, marshalErr := marshalAuditLineWithCap(entry)
	if marshalErr != nil {
		return marshalErr
	}

	// 7. Append.
	return appendAuditLine(path, line)
}

// fillCallerFields auto-populates the caller-identity slots when zero.
// Tests pre-fill specific fields (e.g., a >1KB CallerUser) to bypass
// the auto-fill and exercise rejection paths.
func fillCallerFields(e *IntentAuditEntry) {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	} else {
		e.TS = e.TS.UTC()
	}
	if e.CallerPID == 0 {
		e.CallerPID = os.Getpid()
	}
	if e.CallerExe == "" {
		exe, err := os.Executable()
		if err != nil {
			exe = "<unknown>"
		}
		e.CallerExe = exe
	}
	if e.CallerStartTime.IsZero() {
		e.CallerStartTime = CallerStartTime()
	} else {
		e.CallerStartTime = e.CallerStartTime.UTC()
	}
	if e.CallerUser == "" {
		e.CallerUser = currentOSUser()
	}
}

// auditFileSize returns the size of `path` in bytes plus a "found"
// flag. ErrNotExist returns (0, false) so the caller treats a fresh
// file as below the rotation threshold.
func auditFileSize(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}

// rotateAuditFile renames the active audit log to ${path}.1, replacing
// any existing .1 backup. Returns the rename error verbatim on
// failure; the caller propagates so the user sees a disk-level
// problem rather than silently losing an audit trail.
func rotateAuditFile(path string) error {
	target := path + intentAuditRotatedSuffix
	// On Windows os.Rename refuses to overwrite an existing target.
	// Remove any prior .1 first; ignore ErrNotExist.
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove existing rotated %s: %w", target, err)
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("rotate %s -> %s: %w", path, target, err)
	}
	return nil
}

// writeRotatedSelfEvent emits the audit-rotated self-event into the
// fresh log. Per §26: self-event write failures are non-fatal — log
// via the auditRotatedFailureLogFn seam (Task 9 wires this to
// watchdog.log as audit-rotated-event-write-failed-non-fatal) and
// proceed. NO double-rotation, NO infinite retry.
func writeRotatedSelfEvent(path string, prevSize int64) {
	prev := filepath.Base(path) + intentAuditRotatedSuffix
	entry := newSystemAuditEntry(
		"audit-rotated",
		WithTask("<rotation-system>"),
		WithRotationSize(prevSize),
		WithRotationPrevPath(prev),
		WithNote("rotation triggered at 10MB"),
	)
	entry.Who = "intent-audit-rotator"
	entry.CallerUser = "<rotation-system>"
	// Auto-fill the remaining caller fingerprint fields so the entry
	// keeps a stable shape on the wire (CallerPID/CallerExe/...).
	fillCallerFields(&entry)

	line, err := marshalAuditLineWithCap(entry)
	if err != nil {
		notifyRotationFailure(err)
		return
	}
	if err := appendAuditLine(path, line); err != nil {
		notifyRotationFailure(err)
	}
}

// notifyRotationFailure routes the failure through the configured
// seam, falling back to silence in production until Task 9 wires the
// real watchdog.log appender.
func notifyRotationFailure(err error) {
	if auditRotatedFailureLogFn != nil {
		auditRotatedFailureLogFn(err)
	}
}

// appendAuditLine writes line + newline to path under the caller's
// already-held flock. Routes through auditAppendWriteFn when the seam
// is set; otherwise calls defaultAppendAuditLine.
func appendAuditLine(path string, line []byte) error {
	if auditAppendWriteFn != nil {
		return auditAppendWriteFn(path, line)
	}
	return defaultAppendAuditLine(path, line)
}

// defaultAppendAuditLine is the production disk write — opens the
// file with O_APPEND|O_CREATE|O_WRONLY 0o600, writes line + "\n", and
// performs a post-open Chmod to 0600 so umask drift cannot widen
// permissions on POSIX. Callers MUST already hold the audit flock.
func defaultAppendAuditLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open audit %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// Defense vs umask drift on POSIX. No-op on Windows (NTFS ignores).
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod audit %s: %w", path, err)
	}

	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write audit line: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync audit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 16KB cap with identity preservation (§35).
// ---------------------------------------------------------------------------

// marshalAuditLineWithCap marshals e into JSON Lines bytes, applying
// the 16KB cap with identity preservation per §35:
//
//   - Identity fields (Task, CallerUser) intact.
//   - Longest non-identity STRING field truncated if marshal exceeds
//     AuditEntryMaxBytes; entry gains _truncated:true,
//     _truncated_field, _task_hash markers.
//   - If post-truncation the entry STILL exceeds the cap, replace
//     with a placeholder line carrying _task_hash for forensic
//     correlation.
func marshalAuditLineWithCap(e IntentAuditEntry) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("marshal audit entry: %w", err)
	}
	if len(raw) <= AuditEntryMaxBytes {
		return raw, nil
	}

	// Oversize path: identify the longest non-identity string field
	// and truncate so the marshaled entry fits.
	field, truncated, ok := truncateLongestNonIdentity(&e, len(raw))
	if ok {
		raw2, err2 := json.Marshal(e)
		if err2 != nil {
			return nil, fmt.Errorf("re-marshal truncated audit entry: %w", err2)
		}
		// Inject markers (_truncated, _truncated_field, _task_hash) by
		// surgical JSON append since they are not part of the struct.
		injected := injectTruncationMarkers(raw2, field, taskHash12(e.Task))
		_ = truncated
		if len(injected) <= AuditEntryMaxBytes {
			return injected, nil
		}
	}

	// Drop with placeholder.
	placeholder := buildDropPlaceholder(e)
	return placeholder, nil
}

// truncateLongestNonIdentity finds the longest non-identity string
// field on e and shortens it so the next marshal lands within budget.
// Returns the field name, the truncated text it replaced, and an "ok"
// flag indicating whether truncation was meaningful (false → the
// caller must fall through to the placeholder branch).
//
// The non-identity field set, in priority order: Reason, Note,
// CallerExe, Who. Identity fields (Task, CallerUser) are excluded.
func truncateLongestNonIdentity(e *IntentAuditEntry, currentSize int) (string, string, bool) {
	type cand struct {
		name string
		ptr  *string
	}
	cands := []cand{
		{"reason", &e.Reason},
		{"note", &e.Note},
		{"caller_exe", &e.CallerExe},
		{"who", &e.Who},
	}
	// Find the longest.
	var longest *cand
	for i := range cands {
		c := &cands[i]
		if longest == nil || len(*c.ptr) > len(*longest.ptr) {
			longest = c
		}
	}
	if longest == nil || len(*longest.ptr) == 0 {
		return "", "", false
	}

	// Compute target byte budget for the truncatable field. We start
	// from the over-budget marshaled size and subtract the slack we
	// need to recover (currentSize - target). The truncated field
	// should be at most (current_field_len - over_by) - safety_margin.
	overBy := currentSize - AuditEntryMaxBytes + auditFixedSchemaOverhead
	if overBy < 0 {
		overBy = 0
	}
	curLen := len(*longest.ptr)
	if overBy >= curLen {
		// Truncating this field alone won't fit — caller falls through.
		return "", "", false
	}
	newLen := curLen - overBy
	if newLen < 0 {
		newLen = 0
	}
	original := *longest.ptr
	*longest.ptr = original[:newLen]
	return longest.name, original, true
}

// injectTruncationMarkers appends _truncated, _truncated_field, and
// _task_hash markers to a marshaled JSON object. The input must be a
// JSON object ending in '}'. The output preserves the original field
// order plus the markers.
func injectTruncationMarkers(raw []byte, field string, taskHash string) []byte {
	if len(raw) == 0 || raw[len(raw)-1] != '}' {
		return raw // defensive: not a JSON object ending in '}'
	}
	// Build the suffix without the trailing '}'.
	body := raw[:len(raw)-1]
	// Detect whether the existing body already has a key=value (so we
	// know whether to prepend a comma).
	suffix := ""
	if hasFieldsBefore(body) {
		suffix = ","
	}
	suffix += fmt.Sprintf(`"_truncated":true,"_truncated_field":%q,"_task_hash":%q}`, field, taskHash)
	out := make([]byte, 0, len(body)+len(suffix))
	out = append(out, body...)
	out = append(out, []byte(suffix)...)
	return out
}

// hasFieldsBefore reports whether `body` (the bytes inside a JSON
// object minus its closing '}') already contains at least one key.
// Used by injectTruncationMarkers to decide whether to prepend a
// comma. Strict: walks past the leading '{' and any leading
// whitespace; presence of any non-whitespace content means at least
// one field exists.
func hasFieldsBefore(body []byte) bool {
	// Skip a leading '{' if present.
	i := 0
	for i < len(body) && (body[i] == '{' || body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
		i++
	}
	return i < len(body)
}

// buildDropPlaceholder constructs the placeholder line emitted when
// even truncation cannot fit the entry within AuditEntryMaxBytes.
// Per §35: the placeholder MUST include _task_hash for forensic
// correlation with downstream entries.
func buildDropPlaceholder(e IntentAuditEntry) []byte {
	hash := taskHash12(e.Task)
	ts := e.TS.Format(time.RFC3339Nano)
	body := fmt.Sprintf(
		`{"ts":%q,"action":"log-entry-dropped-oversize","reason":"original entry exceeded 16KB after truncation","_truncated":true,"_task_hash":%q}`,
		ts, hash,
	)
	return []byte(body)
}

// taskHash12 returns the first 12 hex characters of the SHA-256 sum of
// task. Used as the forensic identifier in truncated and dropped
// entries (§35). 48 bits is documented as collision-acceptable for
// single-user forensic correlation per §54.
func taskHash12(task string) string {
	sum := sha256.Sum256([]byte(task))
	return hex.EncodeToString(sum[:])[:12]
}

// ---------------------------------------------------------------------------
// Wiring (Task 0 seam + Task 2 audit-write TODOs).
// ---------------------------------------------------------------------------

// init binds the appendIntentAuditFn seam (api_surfaces.go) to the
// production AppendIntentAudit so daemon_intent.go's WriteDaemonIntent /
// ClearDaemonIntent reach the real disk path. Tests in api_surfaces_test.go
// continue to override the seam via installTestAuditFn for deterministic
// capture.
func init() {
	appendIntentAuditFn = func(e IntentAuditEntry) error {
		api := NewAPI()
		return api.AppendIntentAudit(e)
	}
}

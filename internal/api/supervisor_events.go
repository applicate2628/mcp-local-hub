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
// Identity preservation (mirrors watchdog_log.go:25-36 §35):
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
// the `(*API).AppendGUIEventLog` / `(*API).AppendWatchdogLog`
// precedents which carry an *API receiver and resolve DaemonStateDir
// internally. The supervisor task plan explicitly specifies this
// constructor-based shape (Task 2.3 step 4); see spec §"Package
// ownership" for rationale.
package api

import (
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
// name. Mirrors gui_event_log.go / watchdog_log.go — only .1 is
// retained; older rotations are overwritten on next rotation.
const supervisorEventLogRotatedSuffix = ".1"

// supervisorEventLogRotateSize is the 10 MB rotation threshold per
// plan §"supervisor-events.log (NEW)". Kept in sync with
// GUIEventLogRotateSizeBytes / WatchdogLogRotateSizeBytes so all
// four log families share one ceiling.
const supervisorEventLogRotateSize int64 = 10 * 1024 * 1024

// supervisorEventMaxBytes is the per-entry size cap per plan
// §"supervisor-events.log (NEW)" (mirrors AuditEntryMaxBytes /
// watchdog_log.go:25-36 §35). Marshaled JSON Lines exceeding this
// ceiling are truncated with identity-field protection.
const supervisorEventMaxBytes = 16 * 1024

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
func (l *SupervisorEventLog) Emit(evt SupervisorEvent) error {
	if evt.Event == "" {
		return ErrSupervisorEventMissingEvent
	}
	if evt.Source == "" {
		return ErrSupervisorEventMissingSource
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
		return fmt.Errorf("supervisor event log marshal: %w", err)
	}
	if len(raw) > supervisorEventMaxBytes {
		evt.Body = map[string]any{
			"_truncated_note": fmt.Sprintf("body fields exceeded %dKB cap", supervisorEventMaxBytes/1024),
		}
		evt.Truncated = true
		raw, err = json.Marshal(evt)
		if err != nil {
			return fmt.Errorf("supervisor event log re-marshal after truncation: %w", err)
		}
	}

	// In-process serialization first — guards against two goroutines
	// in the same supervisor binary racing past the flock acquire.
	l.mu.Lock()
	defer l.mu.Unlock()

	// OS-level serialization across processes (e.g. supervisor +
	// install CLI emitting migration events concurrently).
	if err := l.lock.Lock(); err != nil {
		return fmt.Errorf("supervisor event log flock: %w", err)
	}
	defer func() { _ = l.lock.Unlock() }()

	// Rotation check.
	if size, ok := supervisorEventLogFileSize(l.path); ok && size >= supervisorEventLogRotateSize {
		if rotErr := rotateSupervisorEventLogFile(l.path); rotErr != nil {
			return rotErr
		}
	}

	// Append line + newline. O_APPEND + O_CREATE + O_WRONLY 0o600
	// mirrors gui_event_log.go's defaultGUIEventLogAppend.
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open supervisor event log %s: %w", l.path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write supervisor event log %s: %w", l.path, err)
	}
	return nil
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
	target := path + supervisorEventLogRotatedSuffix
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove existing rotated %s: %w", target, err)
	}
	if err := os.Rename(path, target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // race: file vanished between Stat and Rename, no-op
		}
		return fmt.Errorf("rotate supervisor event log %s: %w", path, err)
	}
	return nil
}
